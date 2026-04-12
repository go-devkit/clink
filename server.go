package cligate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
)

// Config holds the connection settings for Listen and Connect.
type Config struct {
	Host     string // used by Connect only; defaults to "localhost"
	Port     int
	Password string
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
// If newTUI is nil, connections without a command are closed immediately.
func Listen(
	ctx context.Context, conf Config,
	handler Handler, newTUI func() (tea.Model, []tea.ProgramOption),
) error {
	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		return fmt.Errorf("failed to generate host key: %w", err)
	}

	tuiHandler := func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
		if newTUI == nil {
			return quitTea{}, nil
		}
		return newTUI()
	}

	s, err := wish.NewServer(
		wish.WithAddress(":"+strconv.Itoa(conf.Port)),
		wish.WithHostKeyPEM(k.RawPrivateKey()),
		wish.WithPasswordAuth(func(ctx ssh.Context, pass string) bool {
			return pass == conf.Password
		}),
		wish.WithMiddleware(
			bubbletea.Middleware(tuiHandler),
			handleCLI(ctx, handler),
		),
	)
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
