package clink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/keygen"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// fileRequestConcurrency caps how many client-side file transfers can run at
// once per Connect. Excess requests are rejected with ResourceShortage rather
// than spawning unbounded goroutines / file descriptors.
const fileRequestConcurrency = 32

// ConnectOption configures Connect. See WithPTY, AutoPTY, WithLocalCommand.
type ConnectOption func(*connectOpts)

type connectOpts struct {
	pty       bool
	locals    map[string]func(context.Context, []string) error
	fallbacks map[string]func(context.Context, []string) error
}

// WithPTY makes Connect allocate a PTY for the command session, enabling
// the server-side Handler to launch an Interactive (Bubble Tea) TUI for
// that command. Without it, Interactive returns from Handler will fail
// with an exit-1 message on the server.
func WithPTY() ConnectOption {
	return func(o *connectOpts) {
		o.pty = true
	}
}

// AutoPTY makes Connect allocate a PTY when os.Stdin is a terminal, and
// skip PTY allocation when stdin is piped/redirected. This mirrors what
// ssh does by default and lets a single Connect call handle both TUI
// subcommands (tty stdin) and pipe-friendly non-TUI subcommands.
func AutoPTY() ConnectOption {
	return func(o *connectOpts) {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return
		}

		o.pty = true
	}
}

// WithLocalCommand registers a command that must not be forwarded to the
// daemon. If args[0] matches name (or name == "" and args is empty), Connect
// calls fn instead of dialing. Connect returns an error if a daemon is already
// reachable, to prevent a double-run — use WithLocalFallback for
// forward-if-up / run-if-down semantics.
func WithLocalCommand(name string, fn func(context.Context, []string) error) ConnectOption {
	return func(o *connectOpts) {
		if o.locals == nil {
			o.locals = make(map[string]func(context.Context, []string) error)
		}

		o.locals[name] = fn
	}
}

// WithLocalFallback registers a command that runs locally only when no daemon
// is reachable. If args[0] matches name (or name == "" and args is empty) and
// the daemon is up, Connect forwards as usual; if the daemon is down, fn runs
// instead. Typical use: no-args entry that opens the TUI when the daemon is
// running and starts the daemon otherwise.
func WithLocalFallback(name string, fn func(context.Context, []string) error) ConnectOption {
	return func(o *connectOpts) {
		if o.fallbacks == nil {
			o.fallbacks = make(map[string]func(context.Context, []string) error)
		}

		o.fallbacks[name] = fn
	}
}

// Connect sends args to the running daemon.
// If args is empty, it opens an interactive TUI session.
// If args[0] matches a WithLocalCommand name, Connect first checks that no
// daemon is already reachable on the configured host/port; if one is, Connect
// returns an error to prevent a double-run. Otherwise it invokes the local
// handler instead of dialing.
// ctx is used for the local-command handler and for cancelling the
// connection setup (dial + handshake); it does not itself cancel an in-flight
// remote command. To cancel a running non-PTY command, deliver SIGINT/SIGTERM
// to the client process — Connect forwards it to the daemon, which cancels the
// handler's context. PTY sessions deliver Ctrl-C in-band and forward terminal
// resizes (SIGWINCH) to the server-side TUI.
func Connect(ctx context.Context, conf Config, args []string, opts ...ConnectOption) error {
	var co connectOpts
	for _, o := range opts {
		o(&co)
	}

	key := ""
	if len(args) > 0 {
		key = args[0]
	}

	if fn, ok := co.locals[key]; ok {
		if daemonReachable(conf) {
			host, err := normalizeHost(conf.Host)
			if err != nil {
				host = conf.Host
			}

			return fmt.Errorf("clink: something is already listening on %s:%d; refusing to run local command %q", host, conf.Port, key)
		}

		return fn(ctx, args)
	}

	if fn, ok := co.fallbacks[key]; ok && !daemonReachable(conf) {
		return fn(ctx, args)
	}

	host, err := normalizeHost(conf.Host)
	if err != nil {
		return err
	}

	if len(conf.HostPublicKey) == 0 && !isLoopback(host) {
		return fmt.Errorf("refusing to connect to non-loopback host %q without HostPublicKey; pin the daemon's host key or use a literal loopback IP (e.g. 127.0.0.1)", host)
	}

	auth, err := clientAuth(conf)
	if err != nil {
		return err
	}

	hostKeyCallback, err := hostKeyCallback(conf)
	if err != nil {
		return err
	}

	config := &ssh.ClientConfig{
		User:            "user",
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
	}

	addr := net.JoinHostPort(host, strconv.Itoa(conf.Port))

	dialer := net.Dialer{Timeout: 10 * time.Second}
	tcpConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Unblocks NewClientConn below; the handshake error is what we report.
			_ = tcpConn.Close()
		case <-handshakeDone:
		}
	}()

	sshConn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
	close(handshakeDone)
	if err != nil {
		// Handshake failed; the conn is unusable and close errors add nothing.
		_ = tcpConn.Close()
		return fmt.Errorf("failed to connect: %w", err)
	}

	allow := newAllowlist(args)
	filteredChans := interceptFileChannels(chans, allow)

	conn := ssh.NewClient(sshConn, filteredChans, reqs)
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	interactive := len(args) == 0 || co.pty
	if interactive {
		restore, err := setupClientPTY(session)
		if err != nil {
			return err
		}
		defer restore()

		stopWinch := forwardWinch(session)
		defer stopWinch()
	} else {
		stopSignals := forwardSignals(session)
		defer stopSignals()
	}

	if len(args) == 0 {
		if err := session.Shell(); err != nil {
			return fmt.Errorf("failed to start shell: %w", err)
		}

		return session.Wait()
	}

	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}

	return session.Run(strings.Join(quoted, " "))
}

// interceptFileChannels pulls file-request channels off the incoming stream
// and dispatches each in its own goroutine, capped by a semaphore so a rogue
// server can't exhaust FDs. Non-file channels pass through untouched to the
// returned channel.
func interceptFileChannels(chans <-chan ssh.NewChannel, allow *allowlist) <-chan ssh.NewChannel {
	out := make(chan ssh.NewChannel)
	sem := make(chan struct{}, fileRequestConcurrency)

	go func() {
		defer close(out)

		for newCh := range chans {
			if newCh.ChannelType() != fileRequestChannel {
				out <- newCh
				continue
			}

			select {
			case sem <- struct{}{}:
				go func(nc ssh.NewChannel) {
					defer func() { <-sem }()
					handleFileRequest(nc, allow)
				}(newCh)
			default:
				// At the concurrency cap; if the reject itself fails the peer is gone anyway.
				_ = newCh.Reject(ssh.ResourceShortage, "too many concurrent file requests")
			}
		}
	}()

	return out
}

// setupClientPTY puts os.Stdin in raw mode and requests a PTY on the SSH
// session. The returned func restores the terminal state.
func setupClientPTY(session *ssh.Session) (func(), error) {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("failed to set raw mode: %w", err)
	}

	restore := func() {
		// Best-effort teardown; if restore fails the terminal is already broken.
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}

	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width, height = 80, 24
	}

	termType := os.Getenv("TERM")
	if termType == "" {
		termType = "xterm-256color"
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
		ssh.ICRNL:         1,
		ssh.IXON:          0,
		ssh.IXANY:         0,
		ssh.IMAXBEL:       0,
		ssh.IUTF8:         1,
	}

	if err := session.RequestPty(termType, height, width, modes); err != nil {
		restore()
		return nil, fmt.Errorf("failed to request PTY: %w", err)
	}

	return restore, nil
}

// forwardSignals relays client-side SIGINT/SIGTERM to the remote session so
// the server can cancel the per-session handler context. Used for non-PTY
// command sessions; PTY sessions deliver Ctrl-C in-band as a byte instead.
// The returned func stops forwarding.
func forwardSignals(session *ssh.Session) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				// Best-effort relay: if the send fails the remote ctx just
				// won't cancel, and there is nothing the client can do about it.
				switch s {
				case os.Interrupt:
					_ = session.Signal(ssh.SIGINT)
				case syscall.SIGTERM:
					_ = session.Signal(ssh.SIGTERM)
				}
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// clientAuth returns the SSH auth methods for Connect.
//
// If a password is set, password auth is used. Otherwise an ephemeral ed25519
// key is generated in memory — the server accepts any public key in this mode.
func clientAuth(conf Config) ([]ssh.AuthMethod, error) {
	if conf.Password != "" {
		return []ssh.AuthMethod{ssh.Password(conf.Password)}, nil
	}

	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}

	return []ssh.AuthMethod{ssh.PublicKeys(k.Signer())}, nil
}

// hostKeyCallback returns the SSH host key verification callback for Connect.
//
// If conf.HostPublicKey is set, the daemon's host key must match. Otherwise
// host key verification is disabled — only safe on loopback.
func hostKeyCallback(conf Config) (ssh.HostKeyCallback, error) {
	if len(conf.HostPublicKey) == 0 {
		return ssh.InsecureIgnoreHostKey(), nil
	}

	pub, _, _, _, err := ssh.ParseAuthorizedKey(conf.HostPublicKey)
	if err != nil {
		return nil, fmt.Errorf("parse HostPublicKey: %w", err)
	}

	return ssh.FixedHostKey(pub), nil
}

func shellQuote(s string) string {
	quoted := strings.ReplaceAll(s, "'", "'\\''")

	return "'" + quoted + "'"
}

// daemonReachable does a quick TCP dial to see whether something is already
// listening on the configured host/port. Used to refuse local-command
// dispatch when the daemon is already running.
func daemonReachable(conf Config) bool {
	host, err := normalizeHost(conf.Host)
	if err != nil {
		return false
	}

	addr := net.JoinHostPort(host, strconv.Itoa(conf.Port))
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	// Only the successful dial matters; this is a liveness probe, not a session.
	_ = c.Close()

	return true
}

// allowlist is the set of path strings the server is permitted to ReadFile /
// WriteFile against. Built from the args passed to Connect: every arg is
// added verbatim, and if the arg begins with "-" the RHS of an "=" form is
// added too (so "--file=/tmp/x" allowlists "/tmp/x"). Match is exact.
type allowlist struct {
	paths map[string]struct{}
}

func newAllowlist(args []string) *allowlist {
	a := &allowlist{paths: make(map[string]struct{}, len(args))}
	for _, arg := range args {
		a.paths[arg] = struct{}{}

		if !strings.HasPrefix(arg, "-") {
			continue
		}

		if i := strings.Index(arg, "="); i > 0 {
			a.paths[arg[i+1:]] = struct{}{}
		}
	}

	return a
}

func (a *allowlist) allowed(path string) bool {
	_, ok := a.paths[path]

	return ok
}

func handleFileRequest(newCh ssh.NewChannel, allow *allowlist) {
	var req fileRequest
	if err := json.Unmarshal(newCh.ExtraData(), &req); err != nil {
		// Rejecting is the whole response here; a failed reject means the peer left.
		_ = newCh.Reject(ssh.UnknownChannelType, "invalid file request payload")
		return
	}

	if !allow.allowed(req.Path) {
		// Same: the reject is terminal, nothing to recover if it doesn't land.
		_ = newCh.Reject(ssh.Prohibited, "path not in allowlist: "+req.Path)
		return
	}

	switch req.Mode {
	case "read":
		serveFileRead(newCh, req.Path)

	case "write":
		serveFileWrite(newCh, req.Path)

	default:
		// Terminal reject for an unknown mode; failure here is unrecoverable.
		_ = newCh.Reject(ssh.UnknownChannelType, "unknown file request mode: "+req.Mode)
	}
}

func serveFileRead(newCh ssh.NewChannel, path string) {
	f, err := os.Open(path)
	if err != nil {
		// Open failed; report it via reject, then nothing more to do.
		_ = newCh.Reject(ssh.Prohibited, err.Error())
		return
	}
	defer f.Close()

	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	go ssh.DiscardRequests(reqs)

	_, copyErr := io.Copy(ch, f)
	sendFileStatus(ch, copyErr)
}

func serveFileWrite(newCh ssh.NewChannel, path string) {
	f, err := os.Create(path)
	if err != nil {
		// Create failed; report it via reject, then nothing more to do.
		_ = newCh.Reject(ssh.Prohibited, err.Error())
		return
	}

	ch, reqs, err := newCh.Accept()
	if err != nil {
		f.Close()
		return
	}
	defer ch.Close()

	go ssh.DiscardRequests(reqs)

	_, copyErr := io.Copy(f, ch)
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	sendFileStatus(ch, copyErr)
}

// sendFileStatus reports the outcome of a file transfer back to the server over
// the file channel, so a truncated or failed transfer surfaces there as an
// error instead of a silent EOF. Best-effort: if the channel is already gone
// the server sees the same missing-status error either way.
func sendFileStatus(ch ssh.Channel, transferErr error) {
	// Fire-and-forget: a dropped send is indistinguishable from a broken channel,
	// and the server treats a missing status frame as a transfer error either way.
	_, _ = ch.SendRequest(fileStatusRequest, false, encodeFileStatus(transferErr))
}
