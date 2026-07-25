package clink

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/wish/v2"
	"charm.land/wish/v2/bubbletea"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Config holds the connection settings for Listen and Connect.
//
// Host defaults to "127.0.0.1" on both server (Listen) and client (Connect).
// Setting Host to "" keeps Listen bound to loopback only, which is the safe
// default for a local daemon.
//
// Password is optional. When empty, Listen accepts any public key and Connect
// authenticates with an ephemeral in-memory key — effectively no auth. Listen
// returns an error if Password is empty and Host is not a literal loopback IP
// (e.g. 127.0.0.1, ::1); hostnames are rejected to avoid DNS-dependent safety
// checks. On shared hosts any local user or process that can reach the loopback
// port can connect; set Password when running on multi-user machines.
//
// clink does no rate limiting and caps neither concurrent connections nor
// concurrent sessions. Password auth compares a SHA-256 digest in constant
// time, but an attacker that can reach the port may guess passwords as fast as
// it can open connections. The intended deployment is a loopback-bound daemon
// serving the same trust domain as its clients; put a real gateway in front of
// it before exposing the port.
type Config struct {
	Host     string
	Port     int
	Password string

	// HostKeyPEM (server) is an optional PEM-encoded private key used as the
	// daemon's SSH host key. When empty, Listen generates an ephemeral key on
	// each start; clients then cannot pin the host key across restarts.
	HostKeyPEM []byte

	// HostPublicKey (client) is an SSH public key in authorized_keys format
	// that Connect uses to verify the daemon's host key, preventing MITM.
	// Required when Host is not a literal loopback IP; empty disables
	// verification (loopback only).
	HostPublicKey []byte

	// ShutdownGrace (server) bounds how long Listen waits for in-flight handlers
	// to finish after its context is cancelled before force-closing the listener.
	// Cancelling the context already cancels each handler's ctx, so this only
	// bounds handlers that ignore cancellation. Zero uses a 5s default.
	ShutdownGrace time.Duration
}

// Session provides I/O for command handlers.
//
// ReadFile/OpenFile/WriteFile/CreateFile request files on the client's local
// filesystem. The client enforces an exact-string allowlist derived from the
// args it passed to Connect: only paths matching one of those argv strings
// (or the RHS of any "-"-prefixed "key=value" arg, e.g. "--file=/tmp/x" or
// "-f=/tmp/x") are served. Anything else is rejected with "path not in
// allowlist".
//
// Each transfer is confirmed end-to-end: the client reports whether it read or
// wrote every byte. ReadFile and WriteFile return that status directly. For the
// streaming forms, the status is reported by Close — always check the error
// from the io.ReadCloser / io.WriteCloser's Close, as a truncated transfer
// surfaces there rather than as a short read or a silent success.
type Session interface {
	io.Reader
	io.Writer
	Stderr() io.Writer

	ReadFile(path string) ([]byte, error)
	OpenFile(path string) (io.ReadCloser, error)
	WriteFile(path string, data []byte) error
	CreateFile(path string) (io.WriteCloser, error)
}

// fileRequest is the channel-open payload for file-request channels.
type fileRequest struct {
	Path string `json:"path"`
	Mode string `json:"mode"` // "read" or "write"
}

const fileRequestChannel = "file-request"

// defaultShutdownGrace is the shutdown grace period used when Config.ShutdownGrace
// is zero.
const defaultShutdownGrace = 5 * time.Second

// fileStatusRequest is the channel-request name the producing side sends once,
// after the last byte, to report whether the transfer completed. It turns a
// silent truncation (a channel that just closes) into a surfaced error.
const fileStatusRequest = "clink-file-status"

// fileStatus is the fileStatusRequest payload. Empty Err means success.
type fileStatus struct {
	Err string
}

// encodeFileStatus renders a transfer error (nil for success) as a
// fileStatusRequest payload.
func encodeFileStatus(err error) []byte {
	var s fileStatus
	if err != nil {
		s.Err = err.Error()
	}

	return gossh.Marshal(s)
}

// decodeFileStatus parses a fileStatusRequest payload into a transfer error,
// or nil on success.
func decodeFileStatus(payload []byte) error {
	var s fileStatus
	if err := gossh.Unmarshal(payload, &s); err != nil {
		return fmt.Errorf("clink: malformed file transfer status: %w", err)
	}
	if s.Err != "" {
		return errors.New(s.Err)
	}

	return nil
}

// awaitFileStatus drains a file channel's requests, delivering the first
// fileStatusRequest as a transfer error (nil on success) to the returned
// channel. If the channel closes before a status arrives — an aborted transfer
// — it reports that as an error rather than a silent success. Draining also
// keeps the request from blocking the channel's mux.
func awaitFileStatus(reqs <-chan *gossh.Request) <-chan error {
	out := make(chan error, 1)

	go func() {
		var done bool
		for req := range reqs {
			if req.Type == fileStatusRequest && !done {
				out <- decodeFileStatus(req.Payload)
				done = true
			}
			if req.WantReply {
				// We only consume the status frame; reply failure means the
				// channel is already closing, which the loop exit handles.
				_ = req.Reply(false, nil)
			}
		}
		if !done {
			out <- errors.New("clink: file transfer channel closed before completion status")
		}
	}()

	return out
}

type sessionCtxKey struct{}

// SessionFrom returns the clink.Session attached to ctx by the daemon, or nil
// if ctx was not produced by a clink handler. Use inside CLI actions to
// reach Session.ReadFile/WriteFile and friends.
func SessionFrom(ctx context.Context) Session {
	s, _ := ctx.Value(sessionCtxKey{}).(Session)
	return s
}

// WithSession attaches s to ctx so handlers reachable via SessionFrom can use
// it. The daemon-side dispatcher (whatever runs the CLI framework's command
// tree) is responsible for calling this before invoking actions.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// Handler processes a CLI command received from a connected client.
// args contains the command arguments as sent by the client;
// args is empty for interactive (no-args) clients.
// Return ErrNotHandled if the command is not recognized — the session exits 127.
// Return *ExitError to set a custom remote exit code.
// Return *Interactive to launch a Bubble Tea TUI for this command.
//
// ctx is the command's lifetime: it is cancelled when the client disconnects
// and when the client forwards SIGINT/SIGTERM (Ctrl-C on a non-PTY command).
// Work that must outlive the request — e.g. a background task the client only
// kicks off — must not use ctx; spawn it with the daemon's own root context
// instead.
//
// Handler runs on its own goroutine per session and clink serializes nothing:
// several clients (or several sessions from one client) can be inside Handler
// at the same time. Handler and everything it closes over — the CLI command
// tree, wire-built services, caches — must be safe for concurrent use. Note
// that CLI frameworks generally are not: mutating a shared *cli.Command (its
// Reader/Writer/ErrWriter, or urfave's parsed flag state) from two sessions
// races. Build or clone the command tree inside Handler rather than reusing one
// instance across sessions.
//
// Session itself is per-connection and not safe for concurrent use by multiple
// goroutines within one Handler call.
type Handler func(ctx context.Context, s Session, args []string) error

// ErrNotHandled is returned by a Handler to indicate the command was not
// recognized. The session then exits with code 127 (the shell "command not
// found" convention), so a client can distinguish an unhandled command from one
// that ran and succeeded.
var ErrNotHandled = errors.New("command not handled")

// exitNotHandled is the remote exit code for an ErrNotHandled command, matching
// the shell convention for "command not found".
const exitNotHandled = 127

// ExitError lets a Handler set a custom remote exit code. Wrap or return
// directly; if Err is non-nil it is written to the session's stderr.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitError) Unwrap() error {
	return e.Err
}

// Interactive is returned by a Handler to launch a Bubble Tea TUI.
// Empty-args sessions: the client already allocates a PTY before opening
// the shell. Non-empty subcommands: the client must opt in via
// clink.WithPTY(); without it the session exits 1 with a stderr message.
type Interactive struct {
	Model tea.Model
	Opts  []tea.ProgramOption
}

func (*Interactive) Error() string {
	return "clink: interactive session"
}

// Listen starts the daemon and handles incoming CLI commands and TUI sessions.
// Handler receives empty args for interactive (no-args) clients and can return
// *Interactive to launch a TUI for them — same mechanism as subcommand TUIs.
//
// There is no version or protocol negotiation between Connect and Listen: the
// wire contract (argv forwarding, the "file-request" channel payload, exit-code
// signalling) is assumed identical on both ends because both ends are the same
// binary. A client built against a different clink version than the running
// daemon may fail in unhelpful ways rather than reporting a version mismatch.
// After upgrading the binary, restart the daemon.
//
// When ctx is cancelled, Listen stops accepting connections and cancels every
// in-flight handler's context, then waits up to a short grace period for them
// to return before force-closing. A handler that ignores its context is cut off
// at the deadline rather than blocking shutdown.
func Listen(ctx context.Context, conf Config, handler Handler) error {
	hostKeyPEM := conf.HostKeyPEM
	if len(hostKeyPEM) == 0 {
		k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
		if err != nil {
			return fmt.Errorf("failed to generate host key: %w", err)
		}
		hostKeyPEM = k.RawPrivateKey()
	}

	host, err := normalizeHost(conf.Host)
	if err != nil {
		return err
	}

	if conf.Password == "" && !isLoopback(host) {
		return fmt.Errorf("refusing to start with empty Password on non-loopback host %q (hostnames are treated as non-loopback); set Password or bind to a literal loopback IP (e.g. 127.0.0.1)", host)
	}

	if handler == nil {
		return errors.New("clink: Listen called with nil handler")
	}

	opts := []ssh.Option{
		wish.WithAddress(net.JoinHostPort(host, strconv.Itoa(conf.Port))),
		wish.WithHostKeyPEM(hostKeyPEM),
		wish.WithMiddleware(handleCLI(ctx, handler)),
	}

	if conf.Password != "" {
		expected := sha256.Sum256([]byte(conf.Password))
		opts = append(opts, wish.WithPasswordAuth(func(_ ssh.Context, pass string) bool {
			got := sha256.Sum256([]byte(pass))

			return subtle.ConstantTimeCompare(got[:], expected[:]) == 1
		}))
	} else {
		opts = append(opts, wish.WithPublicKeyAuth(func(_ ssh.Context, _ ssh.PublicKey) bool {
			return true
		}))
	}

	s, err := wish.NewServer(opts...)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err

	case <-ctx.Done():
		// Cancelling ctx already cancelled every per-session handler ctx (they
		// derive from it), so well-behaved handlers are unwinding. Give them a
		// grace period to finish, then force the listener closed so a handler
		// that ignores its ctx can't block shutdown forever.
		grace := conf.ShutdownGrace
		if grace <= 0 {
			grace = defaultShutdownGrace
		}

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
		defer cancel()

		shutdownErr := s.Shutdown(shutdownCtx)
		if errors.Is(shutdownErr, context.DeadlineExceeded) {
			shutdownErr = s.Close()
		}

		if err := <-errCh; err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			return err
		}
		if shutdownErr != nil {
			return shutdownErr
		}

		return nil
	}
}

func handleCLI(parent context.Context, handler Handler) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			args := s.Command()

			ctx, cancel := context.WithCancel(parent)
			defer cancel()

			go func() {
				select {
				case <-s.Context().Done():
					cancel()
				case <-ctx.Done():
				}
			}()

			sigs := make(chan ssh.Signal, 1)
			s.Signals(sigs)
			go func() {
				for {
					select {
					case sig := <-sigs:
						if sig == ssh.SIGINT || sig == ssh.SIGTERM {
							cancel()
						}
					case <-s.Context().Done():
						return
					}
				}
			}()

			conn, _ := s.Context().Value(ssh.ContextKeyConn).(*gossh.ServerConn)
			err := handler(ctx, &sessionWrapper{s: s, conn: conn}, args)
			if err == nil {
				return
			}
			if errors.Is(err, ErrNotHandled) {
				// Exit only errors if the session already ended; nothing to do then.
				_ = s.Exit(exitNotHandled)
				return
			}

			var inter *Interactive
			if errors.As(err, &inter) {
				runInteractive(s, inter)
				return
			}

			code := 1
			msg := err.Error()

			var exit *ExitError
			if errors.As(err, &exit) {
				code = exit.Code
				if direct, ok := err.(*ExitError); ok && direct.Err == nil {
					msg = ""
				}
			}

			if msg != "" {
				fmt.Fprintf(s.Stderr(), "%v\n", msg)
			}

			if err := s.Exit(code); err != nil {
				fmt.Fprintf(s.Stderr(), "%v\n", err)
			}
		}
	}
}

type sessionWrapper struct {
	s    ssh.Session
	conn *gossh.ServerConn
}

func (w *sessionWrapper) Read(p []byte) (int, error) {
	return w.s.Read(p)
}

func (w *sessionWrapper) Write(p []byte) (int, error) {
	return w.s.Write(p)
}

func (w *sessionWrapper) Stderr() io.Writer {
	return w.s.Stderr()
}

func (w *sessionWrapper) openFileChannel(path, mode string) (gossh.Channel, <-chan error, error) {
	if w.conn == nil {
		return nil, nil, errors.New("clink: no client connection available for file transfer")
	}

	payload, err := json.Marshal(fileRequest{Path: path, Mode: mode})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal file request: %w", err)
	}

	ch, reqs, err := w.conn.OpenChannel(fileRequestChannel, payload)
	if err != nil {
		var openErr *gossh.OpenChannelError
		if errors.As(err, &openErr) {
			return nil, nil, fmt.Errorf("client refused file %s %q: %s", mode, path, openErr.Message)
		}

		return nil, nil, fmt.Errorf("open file channel for %s %q: %w", mode, path, err)
	}

	return ch, awaitFileStatus(reqs), nil
}

func (w *sessionWrapper) ReadFile(path string) ([]byte, error) {
	rc, err := w.OpenFile(path)
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	if readErr != nil {
		return data, readErr
	}

	// closeErr carries the client's transfer status: a truncated read surfaces
	// here rather than returning partial data as success.
	return data, closeErr
}

func (w *sessionWrapper) OpenFile(path string) (io.ReadCloser, error) {
	ch, status, err := w.openFileChannel(path, "read")
	if err != nil {
		return nil, err
	}

	return &readChannel{ch: ch, status: status}, nil
}

// readChannel wraps a file-read channel so Close reports the client's transfer
// status: a read that was truncated on the client surfaces as an error rather
// than passing partial data off as a complete file.
type readChannel struct {
	ch     gossh.Channel
	status <-chan error
}

func (r *readChannel) Read(p []byte) (int, error) {
	return r.ch.Read(p)
}

func (r *readChannel) Close() error {
	// Closing our end first aborts an incomplete transfer; on a completed read
	// it is a benign "peer already closed". Either way the status channel — nil
	// on success, an error on truncation/abort — is the authoritative result.
	_ = r.ch.Close()

	return <-r.status
}

func (w *sessionWrapper) WriteFile(path string, data []byte) error {
	wc, err := w.CreateFile(path)
	if err != nil {
		return err
	}

	if _, err := wc.Write(data); err != nil {
		wc.Close()
		return err
	}

	return wc.Close()
}

func (w *sessionWrapper) CreateFile(path string) (io.WriteCloser, error) {
	ch, status, err := w.openFileChannel(path, "write")
	if err != nil {
		return nil, err
	}

	return &writeChannel{ch: ch, status: status}, nil
}

// writeChannel wraps a gossh.Channel so Close sends EOF (signals end-of-write
// to the client), waits for the client's transfer status, then closes the full
// channel. A write that failed on the client (e.g. disk full) surfaces from
// Close instead of being silently reported as success.
type writeChannel struct {
	ch     gossh.Channel
	status <-chan error
}

func (w *writeChannel) Write(p []byte) (int, error) {
	return w.ch.Write(p)
}

func (w *writeChannel) Close() error {
	// CloseWrite signals EOF so the client flushes to disk and reports status;
	// only then fully close. The status — nil on success, an error if the client
	// failed to persist — is the authoritative result; a teardown-race error
	// from the final Close is noise.
	_ = w.ch.CloseWrite()
	serr := <-w.status
	_ = w.ch.Close()

	return serr
}

// normalizeHost applies the shared Host handling for Listen and Connect:
// defaulting empty to 127.0.0.1, trimming a single pair of IPv6 brackets,
// and rejecting host:port forms.
func normalizeHost(host string) (string, error) {
	if host == "" {
		host = "127.0.0.1"
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}

	if host == "" {
		return "", fmt.Errorf("host must not be empty after normalization")
	}

	if _, _, err := net.SplitHostPort(host); err == nil {
		return "", fmt.Errorf("host must not include a port: %q", host)
	}

	return host, nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// runInteractive runs a tea.Program for an Interactive command. The session
// must already have a PTY allocated (the client used WithPTY).
func runInteractive(s ssh.Session, i *Interactive) {
	if i == nil || i.Model == nil {
		fmt.Fprintln(s.Stderr(), "clink: Handler returned a nil Interactive or Model")
		// Message already sent; Exit only errors on an ended session, nothing to do.
		_ = s.Exit(1)
		return
	}

	pty, winch, ok := s.Pty()
	if !ok {
		fmt.Fprintln(s.Stderr(), "clink: interactive command requires a PTY (use clink.WithPTY)")
		// Same: the diagnostic is out; a failed Exit means the session is already gone.
		_ = s.Exit(1)
		return
	}

	opts := append([]tea.ProgramOption(nil), i.Opts...)
	opts = append(opts, tea.WithWindowSize(pty.Window.Width, pty.Window.Height))
	opts = append(opts, bubbletea.MakeOptions(s)...)

	p := tea.NewProgram(i.Model, opts...)

	ctx, cancel := context.WithCancel(s.Context())
	defer cancel()

	go func() {
		for {
			select {
			case <-ctx.Done():
				p.Quit()
				return

			case w, ok := <-winch:
				if !ok {
					return
				}
				p.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
			}
		}
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(s.Stderr(), "clink: TUI exited with error: %v\n", err)
		p.Kill()
		// Error already reported; a failed Exit means the session already closed.
		_ = s.Exit(1)
		return
	}

	p.Kill()
}
