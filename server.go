package cligate

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
)

// Handler processes a CLI command received over SSH.
// args contains the parsed command arguments (without leading "--" separators).
// Return ErrNotHandled to signal that the command is not recognized,
// which causes the server to fall through to the TUI middleware.
type Handler func(ctx context.Context, s ssh.Session, args []string) error

// ErrNotHandled is returned by a Handler to indicate the command was not recognized.
var ErrNotHandled = errors.New("command not handled")

func Serve(
	ctx context.Context, port, password string,
	handler Handler, newTUI func(ssh.Session) (tea.Model, []tea.ProgramOption),
) error {
	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		return fmt.Errorf("failed to generate host key: %w", err)
	}

	s, err := wish.NewServer(
		wish.WithAddress(":"+port),
		wish.WithHostKeyPEM(k.RawPrivateKey()),
		wish.WithPasswordAuth(func(ctx ssh.Context, pass string) bool {
			return pass == password
		}),
		wish.WithMiddleware(
			bubbletea.Middleware(newTUI),
			handleCLI(ctx, handler),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create wish server: %w", err)
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

func QuitTea() tea.Model {
	return quitTea{}
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

			if err := handler(ctx, s, args); err != nil {
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

// quitTea is a bubbletea model that immediately quits.
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
