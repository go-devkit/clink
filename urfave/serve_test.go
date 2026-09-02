package urfave_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-devkit/clink"
	"github.com/go-devkit/clink/urfave"
	"github.com/urfave/cli/v3"
	gossh "golang.org/x/crypto/ssh"
)

// freePort grabs an OS-assigned loopback port and releases it. Small race vs
// Serve rebinding it, but acceptable for tests.
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

// startServe launches Serve in a goroutine and waits for it to accept on the
// chosen port. Returns conf and a stop func.
func startServe(t *testing.T, newRoot func(clink.Session) *cli.Command, opts ...urfave.Option) (clink.Config, func()) {
	t.Helper()
	port := freePort(t)
	conf := clink.Config{Host: "127.0.0.1", Port: port, Password: "pw"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- urfave.Serve(ctx, conf, newRoot, opts...) }()

	deadline := time.Now().Add(3 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			c.Close()
			ready = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Serve exited early: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatal("server did not become ready within deadline")
	}

	return conf, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("Serve returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("Serve did not shut down in time")
		}
	}
}

func dial(t *testing.T, conf clink.Config) *gossh.Client {
	t.Helper()
	c, err := gossh.Dial("tcp", net.JoinHostPort(conf.Host, strconv.Itoa(conf.Port)), &gossh.ClientConfig{
		User:            "user",
		Auth:            []gossh.AuthMethod{gossh.Password(conf.Password)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh.Dial: %v", err)
	}
	return c
}

// run executes cmdline against the daemon and returns stdout, stderr and the
// remote exit status (0 when the command succeeded).
func run(t *testing.T, conf clink.Config, cmdline string) (string, string, int) {
	t.Helper()
	c := dial(t, conf)
	defer c.Close()

	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	err = sess.Run(cmdline)
	code := 0
	if err != nil {
		var exitErr *gossh.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %q: %v", cmdline, err)
		}
		code = exitErr.ExitStatus()
	}
	return stdout.String(), stderr.String(), code
}

// rootWith builds a fresh tree per session — the contract Serve documents.
func rootWith(commands ...*cli.Command) func(clink.Session) *cli.Command {
	return func(clink.Session) *cli.Command {
		return &cli.Command{Name: "testapp", Commands: commands}
	}
}

func TestExitCoderPropagatesCode(t *testing.T) {
	conf, stop := startServe(t, rootWith(&cli.Command{
		Name:   "boom",
		Action: func(context.Context, *cli.Command) error { return cli.Exit("bad input", 3) },
	}))
	defer stop()

	_, stderr, code := run(t, conf, "boom")
	if code != 3 {
		t.Errorf("exit status = %d, want 3", code)
	}
	if !strings.Contains(stderr, "bad input") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "bad input")
	}
}

func TestPlainErrorExitsOne(t *testing.T) {
	conf, stop := startServe(t, rootWith(&cli.Command{
		Name:   "fail",
		Action: func(context.Context, *cli.Command) error { return errors.New("nope") },
	}))
	defer stop()

	_, stderr, code := run(t, conf, "fail")
	if code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if !strings.Contains(stderr, "nope") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "nope")
	}
}

func TestUnknownCommandReportsToSession(t *testing.T) {
	conf, stop := startServe(t, rootWith(&cli.Command{
		Name:   "known",
		Action: func(context.Context, *cli.Command) error { return nil },
	}))
	defer stop()

	stdout, stderr, code := run(t, conf, "nope")
	if code == 0 {
		t.Errorf("exit status = 0, want non-zero for an unknown command")
	}
	if !strings.Contains(stdout+stderr, "nope") {
		t.Errorf("neither stream mentions the unknown command: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestStdoutAndStderrLandOnSession(t *testing.T) {
	conf, stop := startServe(t, rootWith(&cli.Command{
		Name: "say",
		Action: func(_ context.Context, cmd *cli.Command) error {
			fmt.Fprint(cmd.Root().Writer, "to-stdout")
			fmt.Fprint(cmd.Root().ErrWriter, "to-stderr")
			return nil
		},
	}))
	defer stop()

	stdout, stderr, code := run(t, conf, "say")
	if code != 0 {
		t.Fatalf("exit status = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "to-stdout" {
		t.Errorf("stdout = %q, want %q", stdout, "to-stdout")
	}
	if stderr != "to-stderr" {
		t.Errorf("stderr = %q, want %q", stderr, "to-stderr")
	}
}

func TestHelpGoesToSession(t *testing.T) {
	conf, stop := startServe(t, rootWith(&cli.Command{
		Name:  "greet",
		Usage: "say hello to someone",
		Action: func(context.Context, *cli.Command) error {
			t.Error("action ran for --help")
			return nil
		},
	}))
	defer stop()

	stdout, stderr, code := run(t, conf, "greet --help")
	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr: %q)", code, stderr)
	}
	if !strings.Contains(stdout, "say hello to someone") {
		t.Errorf("help not on the session's stdout: %q", stdout)
	}
}

func TestArgvZeroIsRootName(t *testing.T) {
	var got []string
	newRoot := func(clink.Session) *cli.Command {
		return &cli.Command{
			Name: "testapp",
			Commands: []*cli.Command{{
				Name: "echo",
				Action: func(_ context.Context, cmd *cli.Command) error {
					got = append([]string{cmd.Root().Name}, cmd.Args().Slice()...)
					return nil
				},
			}},
		}
	}
	conf, stop := startServe(t, newRoot)
	defer stop()

	if _, stderr, code := run(t, conf, "echo one two"); code != 0 {
		t.Fatalf("exit status = %d, want 0 (stderr: %q)", code, stderr)
	}
	want := []string{"testapp", "one", "two"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

// TestOsExiterDoesNotKillDaemon covers the failure mode the adapter exists to
// prevent: urfave's global OsExiter would os.Exit the whole daemon.
func TestOsExiterDoesNotKillDaemon(t *testing.T) {
	conf, stop := startServe(t, rootWith(&cli.Command{
		Name: "quit",
		Action: func(context.Context, *cli.Command) error {
			cli.HandleExitCoder(cli.Exit("dying", 7))
			return nil
		},
	}))
	defer stop()

	if _, stderr, code := run(t, conf, "quit"); code != 0 {
		t.Fatalf("exit status = %d, want 0 (stderr: %q)", code, stderr)
	}
	if _, _, code := run(t, conf, "quit"); code != 0 {
		t.Fatalf("daemon did not survive OsExiter: exit status = %d", code)
	}
}

func TestPanicReturnsErrorAndDaemonSurvives(t *testing.T) {
	var (
		mu     sync.Mutex
		reason any
		stack  []byte
	)
	onPanic := func(_ context.Context, r any, s []byte) {
		mu.Lock()
		defer mu.Unlock()
		reason, stack = r, s
	}

	conf, stop := startServe(t, rootWith(
		&cli.Command{
			Name:   "panic",
			Action: func(context.Context, *cli.Command) error { panic("kaboom") },
		},
		&cli.Command{
			Name: "ok",
			Action: func(_ context.Context, cmd *cli.Command) error {
				fmt.Fprint(cmd.Root().Writer, "alive")
				return nil
			},
		},
	), urfave.WithPanicHandler(onPanic))
	defer stop()

	_, stderr, code := run(t, conf, "panic")
	if code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if !strings.Contains(stderr, urfave.ErrPanic.Error()) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, urfave.ErrPanic.Error())
	}
	if strings.Contains(stderr, "kaboom") {
		t.Errorf("panic value leaked to the client: %q", stderr)
	}

	mu.Lock()
	gotReason, gotStack := reason, stack
	mu.Unlock()
	if gotReason != "kaboom" {
		t.Errorf("panic handler reason = %v, want %q", gotReason, "kaboom")
	}
	if len(gotStack) == 0 {
		t.Error("panic handler got an empty stack")
	}

	stdout, _, code := run(t, conf, "ok")
	if code != 0 || stdout != "alive" {
		t.Errorf("daemon did not survive the panic: stdout = %q, exit = %d", stdout, code)
	}
}

// TestConcurrentSessionsAreIsolated is what the fresh-tree rule protects: two
// sessions must not see each other's output or flag values.
func TestConcurrentSessionsAreIsolated(t *testing.T) {
	release := make(chan struct{})
	newRoot := func(clink.Session) *cli.Command {
		return &cli.Command{
			Name: "testapp",
			Commands: []*cli.Command{{
				Name:  "echo",
				Flags: []cli.Flag{&cli.StringFlag{Name: "msg"}},
				Action: func(_ context.Context, cmd *cli.Command) error {
					msg := cmd.String("msg")
					// Both sessions park here, so each tree holds its flags and
					// writers while the other one runs.
					<-release
					fmt.Fprint(cmd.Root().Writer, msg)
					return nil
				},
			}},
		}
	}
	conf, stop := startServe(t, newRoot)
	defer stop()

	const n = 4
	var wg sync.WaitGroup
	out := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("session-%d", i)
			stdout, stderr, code := run(t, conf, "echo --msg "+want)
			if code != 0 {
				t.Errorf("session %d: exit status = %d (stderr: %q)", i, code, stderr)
			}
			out[i] = stdout
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range n {
		if want := fmt.Sprintf("session-%d", i); out[i] != want {
			t.Errorf("session %d got %q, want %q", i, out[i], want)
		}
	}
}

func TestNilRootIsAnError(t *testing.T) {
	conf, stop := startServe(t, func(clink.Session) *cli.Command { return nil })
	defer stop()

	_, stderr, code := run(t, conf, "anything")
	if code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if !strings.Contains(stderr, urfave.ErrNilRoot.Error()) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, urfave.ErrNilRoot.Error())
	}
}

func TestHandlerIsUsableStandalone(t *testing.T) {
	h := urfave.Handler(rootWith(&cli.Command{
		Name:   "x",
		Action: func(context.Context, *cli.Command) error { return nil },
	}))
	if h == nil {
		t.Fatal("Handler returned nil")
	}
}
