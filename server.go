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
}

// Session provides I/O for command handlers.
//
// ReadFile/OpenFile/WriteFile/CreateFile request files on the client's local
// filesystem. The client enforces an exact-string allowlist derived from the
// args it passed to Connect: only paths matching one of those argv strings
// (or the RHS of any "-"-prefixed "key=value" arg, e.g. "--file=/tmp/x" or
// "-f=/tmp/x") are served. Anything else is rejected with "path not in
// allowlist".
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
// args contains the parsed command arguments (without leading "--" separators);
// args is empty for interactive (no-args) clients.
// Return ErrNotHandled if the command is not recognized — the session closes.
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

// ErrNotHandled is returned by a Handler to indicate the command was not recognized.
var ErrNotHandled = errors.New("command not handled")

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
		shutdownErr := s.Shutdown(context.Background())
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
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}

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

func (w *sessionWrapper) openFileChannel(path, mode string) (gossh.Channel, error) {
	if w.conn == nil {
		return nil, errors.New("clink: no client connection available for file transfer")
	}

	payload, err := json.Marshal(fileRequest{Path: path, Mode: mode})
	if err != nil {
		return nil, fmt.Errorf("marshal file request: %w", err)
	}

	ch, reqs, err := w.conn.OpenChannel(fileRequestChannel, payload)
	if err != nil {
		var openErr *gossh.OpenChannelError
		if errors.As(err, &openErr) {
			return nil, fmt.Errorf("client refused file %s %q: %s", mode, path, openErr.Message)
		}

		return nil, fmt.Errorf("open file channel for %s %q: %w", mode, path, err)
	}

	go gossh.DiscardRequests(reqs)

	return ch, nil
}

func (w *sessionWrapper) ReadFile(path string) ([]byte, error) {
	rc, err := w.OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
}

func (w *sessionWrapper) OpenFile(path string) (io.ReadCloser, error) {
	ch, err := w.openFileChannel(path, "read")
	if err != nil {
		return nil, err
	}

	return ch, nil
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
	ch, err := w.openFileChannel(path, "write")
	if err != nil {
		return nil, err
	}

	return &writeChannel{ch: ch}, nil
}

// writeChannel wraps a gossh.Channel so Close sends EOF (signals end-of-write
// to the client) before closing the full channel.
type writeChannel struct {
	ch gossh.Channel
}

func (w *writeChannel) Write(p []byte) (int, error) {
	return w.ch.Write(p)
}

func (w *writeChannel) Close() error {
	_ = w.ch.CloseWrite()
	return w.ch.Close()
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
		_ = s.Exit(1)
		return
	}

	pty, winch, ok := s.Pty()
	if !ok {
		fmt.Fprintln(s.Stderr(), "clink: interactive command requires a PTY (use clink.WithPTY)")
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
		_ = s.Exit(1)
		return
	}

	p.Kill()
}
