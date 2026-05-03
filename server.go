package clink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
)

// Config holds the connection settings for Listen and Connect.
//
// Host defaults to "127.0.0.1" on the server (Listen) and "localhost" on the
// client (Connect). Setting Host to "" keeps Listen bound to loopback only,
// which is the safe default for a local daemon.
//
// Password is optional. When empty, Listen accepts any public key and Connect
// authenticates with an ephemeral in-memory key — effectively no auth. Listen
// returns an error if Password is empty and Host is not a literal loopback IP
// (e.g. 127.0.0.1, ::1); hostnames are rejected to avoid DNS-dependent safety
// checks. On shared hosts any local user or process that can reach the loopback
// port can connect; set Password when running on multi-user machines.
type Config struct {
	Host     string
	Port     int
	Password string

	// HostKeyPEM (server) is an optional PEM-encoded private key used as the
	// daemon's SSH host key. When empty, Listen generates an ephemeral key on
	// each start; clients then cannot pin the host key across restarts.
	HostKeyPEM []byte

	// HostPublicKey (client) is an optional SSH public key in authorized_keys
	// format. When set, Connect verifies the daemon presents this host key,
	// preventing MITM. When empty, host key verification is disabled.
	HostPublicKey []byte
}

// Session provides I/O for command handlers.
type Session interface {
	io.Reader
	io.Writer
	Stderr() io.Writer
}

// Handler processes a CLI command received from a connected client.
// args contains the parsed command arguments (without leading "--" separators).
// Return ErrNotHandled to signal that the command is not recognized,
// which causes the server to fall through to the TUI.
type Handler func(ctx context.Context, s Session, args []string) error

// ErrNotHandled is returned by a Handler to indicate the command was not recognized.
var ErrNotHandled = errors.New("command not handled")

// Listen starts the daemon and handles incoming CLI commands and TUI sessions.
// If newTUI is nil, connections without a command are closed.
func Listen(
	ctx context.Context, conf Config,
	handler Handler, newTUI func() (tea.Model, []tea.ProgramOption),
) error {
	hostKeyPEM := conf.HostKeyPEM
	if len(hostKeyPEM) == 0 {
		k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
		if err != nil {
			return fmt.Errorf("failed to generate host key: %w", err)
		}
		hostKeyPEM = k.RawPrivateKey()
	}

	tuiHandler := func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
		if newTUI == nil {
			return quitTea{}, nil
		}
		return newTUI()
	}

	host := conf.Host
	if host == "" {
		host = "127.0.0.1"
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}

	if _, _, err := net.SplitHostPort(host); err == nil {
		return fmt.Errorf("Host must not include a port: %q", host)
	}

	if conf.Password == "" && !isLoopback(host) {
		return fmt.Errorf("refusing to start with empty Password on non-loopback host %q; set Password or bind to a literal loopback IP (e.g. 127.0.0.1)", host)
	}

	opts := []ssh.Option{
		wish.WithAddress(net.JoinHostPort(host, strconv.Itoa(conf.Port))),
		wish.WithHostKeyPEM(hostKeyPEM),
		wish.WithMiddleware(
			bubbletea.Middleware(tuiHandler),
			handleCLI(ctx, handler),
		),
	}

	if conf.Password != "" {
		opts = append(opts, wish.WithPasswordAuth(func(_ ssh.Context, pass string) bool {
			return pass == conf.Password
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

func handleCLI(ctx context.Context, handler Handler) func(next ssh.Handler) ssh.Handler {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			args := s.Command()

			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}

			if len(args) < 1 {
				next(s)
				return
			}

			if err := handler(ctx, &sessionWrapper{s}, args); err != nil {
				if errors.Is(err, ErrNotHandled) {
					next(s)
					return
				}

				fmt.Fprintf(s.Stderr(), "%v\n", err)
				if err := s.Exit(1); err != nil {
					fmt.Fprintf(s.Stderr(), "%v\n", err)
				}
				return
			}
		}
	}
}

type sessionWrapper struct {
	s ssh.Session
}

func (w *sessionWrapper) Read(p []byte) (int, error)  { return w.s.Read(p) }
func (w *sessionWrapper) Write(p []byte) (int, error) { return w.s.Write(p) }
func (w *sessionWrapper) Stderr() io.Writer            { return w.s.Stderr() }

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type quitTea struct{}

func (quitTea) Init() tea.Cmd {
	return nil
}

func (qt quitTea) Update(_ tea.Msg) (tea.Model, tea.Cmd) {
	return qt, tea.Quit
}

func (quitTea) View() string {
	return ""
}
