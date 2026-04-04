package cligate

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
)

func Serve(
	ctx context.Context, port, password string,
	newCLI func() *cli.Command, newTUI func(ssh.Session) (tea.Model, []tea.ProgramOption),
) error {
	s, err := wish.NewServer(
		wish.WithAddress(":"+port),
		wish.WithPasswordAuth(func(ctx ssh.Context, pass string) bool {
			return pass == password
		}),
		wish.WithMiddleware(
			bubbletea.Middleware(newTUI),
			tryCLI(ctx, newCLI),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create wish server: %w", err)
	}

	return s.ListenAndServe()
}

func QuitTea() tea.Model {
	return quitTea{}
}

func tryCLI(ctx context.Context, newCLI func() *cli.Command) func(next ssh.Handler) ssh.Handler {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			command := s.Command()

			if len(command) > 0 && command[0] == "--" {
				command = command[1:]
			}

			if len(command) < 1 {
				next(s)
				return
			}

			cmd := newCLI()
			propagateWriter(s, cmd)

			if sub := cmd.Command(command[0]); sub != nil {
				command = append([]string{cmd.Name}, command...)
				if err := cmd.Run(ctx, command); err != nil {
					fmt.Fprintf(s.Stderr(), "%v\n", err)

					if err := s.Exit(1); err != nil {
						fmt.Fprintf(s.Stderr(), "%v\n", err)
					}
				}

				return
			}

			// Call the next handler
			next(s)
		}
	}
}

//
// -- HELPER
//

func propagateWriter(s ssh.Session, cmd *cli.Command) {
	cmd.Reader = s
	cmd.Writer = s
	cmd.ErrWriter = s.Stderr()

	if len(cmd.Commands) < 1 {
		return
	}

	for _, sub := range cmd.Commands {
		propagateWriter(s, sub)
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
