package clink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/keygen"
	gossh "golang.org/x/crypto/ssh"
)

// --- pure helpers ---

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "127.0.0.1", false},
		{"127.0.0.1", "127.0.0.1", false},
		{"::1", "::1", false},
		{"[::1]", "::1", false},
		{"localhost", "localhost", false},
		{"127.0.0.1:80", "", true},
		{"[::1]:80", "", true},
	}
	for _, c := range cases {
		got, err := normalizeHost(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeHost(%q) expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeHost(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.5.6.7", true},
		{"::1", true},
		{"8.8.8.8", false},
		{"localhost", false}, // hostname, not IP
		{"", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.in); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"foo", "'foo'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"a'b'c", `'a'\''b'\''c'`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- config validation ---

func TestListenRejectsEmptyPasswordOnNonLoopback(t *testing.T) {
	err := Listen(context.Background(), Config{Host: "8.8.8.8", Port: 0}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("expected non-loopback rejection, got %v", err)
	}
}

func TestListenRejectsHostnameWithEmptyPassword(t *testing.T) {
	// hostnames are treated as non-loopback
	err := Listen(context.Background(), Config{Host: "localhost", Port: 0}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("expected hostname rejection, got %v", err)
	}
}

func TestConnectRejectsNonLoopbackWithoutHostKey(t *testing.T) {
	err := Connect(Config{Host: "8.8.8.8", Port: 22}, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("expected non-loopback rejection, got %v", err)
	}
}

// --- integration ---

// freePort grabs an OS-assigned loopback port and releases it. Small race vs
// Listen rebinding it, but acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// startServer launches Listen in a goroutine and waits for it to accept on the
// chosen port. Returns conf and a stop func.
func startServer(t *testing.T, password string, hostKeyPEM []byte, handler Handler) (Config, func()) {
	t.Helper()
	port := freePort(t)
	conf := Config{Host: "127.0.0.1", Port: port, Password: password, HostKeyPEM: hostKeyPEM}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Listen(ctx, conf, handler, nil) }()

	deadline := time.Now().Add(3 * time.Second)
	var ready bool
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			c.Close()
			ready = true
			break
		}
		lastErr = err
		select {
		case err := <-done:
			t.Fatalf("Listen exited early: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatalf("server did not become ready within deadline: %v", lastErr)
	}

	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("Listen returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("Listen did not shut down in time")
		}
	}
	return conf, stop
}

func dialSSH(t *testing.T, conf Config, auth gossh.AuthMethod) *gossh.Client {
	t.Helper()
	cfg := &gossh.ClientConfig{
		User:            "user",
		Auth:            []gossh.AuthMethod{auth},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	c, err := gossh.Dial("tcp", net.JoinHostPort(conf.Host, strconv.Itoa(conf.Port)), cfg)
	if err != nil {
		t.Fatalf("ssh.Dial: %v", err)
	}
	return c
}

func TestHandlerExecutesCommand(t *testing.T) {
	handler := func(_ context.Context, s Session, args []string) error {
		fmt.Fprintf(s, "got:%s", strings.Join(args, ","))
		return nil
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("greet world")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != "got:greet,world" {
		t.Fatalf("got %q", out)
	}
}

func TestHandlerErrorExitsNonZero(t *testing.T) {
	handler := func(_ context.Context, s Session, args []string) error {
		return errors.New("boom")
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("anything")
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	if exitErr.ExitStatus() != 1 {
		t.Errorf("exit status = %d, want 1", exitErr.ExitStatus())
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr missing error: %q", stderr.String())
	}
}

func TestPasswordAuthRejectsWrongPassword(t *testing.T) {
	handler := func(_ context.Context, _ Session, _ []string) error { return nil }
	conf, stop := startServer(t, "right", nil, handler)
	defer stop()

	cfg := &gossh.ClientConfig{
		User:            "user",
		Auth:            []gossh.AuthMethod{gossh.Password("wrong")},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	_, err := gossh.Dial("tcp", net.JoinHostPort(conf.Host, strconv.Itoa(conf.Port)), cfg)
	if err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestEmptyPasswordAcceptsAnyPubkey(t *testing.T) {
	handler := func(_ context.Context, s Session, _ []string) error {
		fmt.Fprint(s, "ok")
		return nil
	}
	conf, stop := startServer(t, "", nil, handler)
	defer stop()

	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	c := dialSSH(t, conf, gossh.PublicKeys(k.Signer()))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("x")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "ok" {
		t.Fatalf("got %q", out)
	}
}

func TestHostKeyPersistence(t *testing.T) {
	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pem := k.RawPrivateKey()

	capture := func(t *testing.T) string {
		handler := func(_ context.Context, s Session, _ []string) error {
			fmt.Fprint(s, "ok")
			return nil
		}
		conf, stop := startServer(t, "pw", pem, handler)
		defer stop()

		var got gossh.PublicKey
		cfg := &gossh.ClientConfig{
			User: "user",
			Auth: []gossh.AuthMethod{gossh.Password("pw")},
			HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
				got = key
				return nil
			},
			Timeout: 3 * time.Second,
		}
		c, err := gossh.Dial("tcp", net.JoinHostPort(conf.Host, strconv.Itoa(conf.Port)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return string(got.Marshal())
	}

	first := capture(t)
	second := capture(t)
	if first != second {
		t.Fatal("host key changed across Listens with same HostKeyPEM")
	}
}

func TestStdinPipingViaSSH(t *testing.T) {
	handler := func(_ context.Context, s Session, _ []string) error {
		b, err := io.ReadAll(s)
		if err != nil {
			return err
		}
		_, err = s.Write([]byte(strings.ToUpper(string(b))))
		return err
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("upper"); err != nil {
		t.Fatal(err)
	}
	go func() {
		io.WriteString(stdin, "hello world")
		stdin.Close()
	}()
	out, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil {
		t.Fatal(err)
	}
	if string(out) != "HELLO WORLD" {
		t.Fatalf("got %q", out)
	}
}

func TestHandleCLIStripsLeadingDoubleDash(t *testing.T) {
	var got []string
	handler := func(_ context.Context, s Session, args []string) error {
		got = args
		fmt.Fprint(s, "ok")
		return nil
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Output("-- greet world"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "greet" || got[1] != "world" {
		t.Fatalf("args = %q, want [greet world]", got)
	}
}

func TestConnectHostPublicKeyAccepts(t *testing.T) {
	osStdMu.Lock()
	defer osStdMu.Unlock()

	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	handler := func(_ context.Context, s Session, _ []string) error {
		fmt.Fprint(s, "verified")
		return nil
	}
	conf, stop := startServer(t, "pw", k.RawPrivateKey(), handler)
	defer stop()

	clientConf := conf
	clientConf.HostPublicKey = k.RawAuthorizedKey()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdoutR.Close(); stdoutW.Close() })
	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr })

	if err := Connect(clientConf, []string{"x"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	stdoutW.Close()
	out, _ := io.ReadAll(stdoutR)
	if string(out) != "verified" {
		t.Fatalf("got %q", out)
	}
}

func TestConnectHostPublicKeyMismatchRejects(t *testing.T) {
	handler := func(_ context.Context, _ Session, _ []string) error { return nil }
	conf, stop := startServer(t, "pw", nil, handler) // ephemeral host key
	defer stop()

	other, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	clientConf := conf
	clientConf.HostPublicKey = other.RawAuthorizedKey()

	err = Connect(clientConf, []string{"x"})
	if err == nil {
		t.Fatal("expected host key mismatch error")
	}
}

func TestConnectHostPublicKeyParseError(t *testing.T) {
	err := Connect(Config{Host: "127.0.0.1", Port: 1, HostPublicKey: []byte("not a key")}, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "parse HostPublicKey") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestListenBadHostKeyPEM(t *testing.T) {
	err := Listen(context.Background(), Config{
		Host:       "127.0.0.1",
		Port:       freePort(t),
		Password:   "pw",
		HostKeyPEM: []byte("not a pem key"),
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error from bad HostKeyPEM")
	}
}

func TestConcurrentSessions(t *testing.T) {
	var n int
	var mu sync.Mutex
	handler := func(_ context.Context, s Session, args []string) error {
		mu.Lock()
		n++
		mu.Unlock()
		fmt.Fprint(s, args[0])
		return nil
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			sess, err := c.NewSession()
			if err != nil {
				errs <- err
				return
			}
			defer sess.Close()
			out, err := sess.Output(fmt.Sprintf("cmd%d", i))
			if err != nil {
				errs <- err
				return
			}
			if string(out) != fmt.Sprintf("cmd%d", i) {
				errs <- fmt.Errorf("got %q", out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n != N {
		t.Errorf("handler invoked %d times, want %d", n, N)
	}
}

// TestConnectStdinPiping covers the same behavior through the public Connect
// API, swapping os.Stdin/Stdout/Stderr with pipes. Not parallel-safe.
var osStdMu sync.Mutex

func TestConnectStdinPiping(t *testing.T) {
	osStdMu.Lock()
	defer osStdMu.Unlock()

	handler := func(_ context.Context, s Session, args []string) error {
		if len(args) == 0 || args[0] != "upper" {
			return ErrNotHandled
		}
		b, err := io.ReadAll(s)
		if err != nil {
			return err
		}
		_, err = s.Write([]byte(strings.ToUpper(string(b))))
		return err
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdinR.Close(); stdinW.Close() })
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdoutR.Close(); stdoutW.Close() })
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderrR.Close(); stderrW.Close() })

	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdinR, stdoutW, stderrW
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr })

	go func() {
		io.WriteString(stdinW, "piped input")
		stdinW.Close()
	}()

	// Drain stderr so a hypothetical write doesn't block.
	go io.Copy(io.Discard, stderrR)

	outCh := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(stdoutR)
		outCh <- b
	}()

	if err := Connect(conf, []string{"upper"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	stdoutW.Close()
	stderrW.Close()

	select {
	case out := <-outCh:
		if string(out) != "PIPED INPUT" {
			t.Fatalf("got %q", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout reading stdout")
	}
}

func TestExitErrorPropagatesCustomCode(t *testing.T) {
	handler := func(_ context.Context, _ Session, _ []string) error {
		return &ExitError{Code: 42, Err: errors.New("nope")}
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("x")
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	if exitErr.ExitStatus() != 42 {
		t.Errorf("exit status = %d, want 42", exitErr.ExitStatus())
	}
	if !strings.Contains(stderr.String(), "nope") {
		t.Errorf("stderr missing message: %q", stderr.String())
	}
}

func TestExitErrorSilentNoMessage(t *testing.T) {
	handler := func(_ context.Context, _ Session, _ []string) error {
		return &ExitError{Code: 7}
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("x")
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	if exitErr.ExitStatus() != 7 {
		t.Errorf("exit status = %d, want 7", exitErr.ExitStatus())
	}
	if stderr.String() != "" {
		t.Errorf("stderr should be empty, got %q", stderr.String())
	}
}

func TestSessionContextCancelsOnDisconnect(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	handler := func(ctx context.Context, _ Session, _ []string) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	conf, stop := startServer(t, "pw", nil, handler)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("x"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}
	c.Close() // abrupt disconnect
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler ctx not cancelled after disconnect")
	}
}

func TestNoTUINoCommandClosesSession(t *testing.T) {
	handler := func(_ context.Context, _ Session, _ []string) error { return nil }
	conf, stop := startServer(t, "pw", nil, handler) // newTUI nil (startServer passes nil)
	defer stop()

	c := dialSSH(t, conf, gossh.Password("pw"))
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	done := make(chan error, 1)
	go func() { done <- sess.Run("") }() // empty command
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session with no command and no TUI did not close")
	}
}
