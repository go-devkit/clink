package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/go-devkit/cligate"
	"github.com/urfave/cli/v3"
)

func main() {
	cmd := &cli.Command{
		Name: "testapp",
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			cmdName := cmd.Args().First()
			if cmdName == "serve" {
				return ctx, nil
			}

			host := cmd.String("host")
			port := strconv.Itoa(cmd.Int("port"))
			password := cmd.String("password")

			if err := cligate.Dial(host, port, password, cmd.Args().Slice()); err != nil {
				return ctx, fmt.Errorf("server connection failed: %w", err)
			}

			return ctx, cli.Exit("", 0)
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "host", Value: "localhost"},
			&cli.IntFlag{Name: "port", Value: 2222},
			&cli.StringFlag{Name: "password", Value: "test"},
		},
		Commands: commands(),
	}

	cmd.Commands = append(cmd.Commands, &cli.Command{
		Name:  "serve",
		Usage: "Start the SSH server",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			fmt.Println("starting server on :2222")
			return cligate.Serve(ctx, "2222", "test", newHandler(), newTUI)
		},
	})

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newHandler() cligate.Handler {
	return func(ctx context.Context, s ssh.Session, args []string) error {
		cmd := &cli.Command{
			Name:     "testapp",
			Commands: commands(),
		}

		propagateWriter(s, cmd)

		if cmd.Command(args[0]) == nil {
			return cligate.ErrNotHandled
		}

		return cmd.Run(ctx, append([]string{cmd.Name}, args...))
	}
}

func propagateWriter(s ssh.Session, cmd *cli.Command) {
	cmd.Reader = s
	cmd.Writer = s
	cmd.ErrWriter = s.Stderr()

	for _, sub := range cmd.Commands {
		propagateWriter(s, sub)
	}
}

func newTUI(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	pty, _, _ := s.Pty()
	m := tuiModel{
		width:  pty.Window.Width,
		height: pty.Window.Height,
	}
	return m, []tea.ProgramOption{tea.WithAltScreen()}
}

func commands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "greet",
			Usage: "Greet someone",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Value: "World"},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				fmt.Fprintf(cmd.Writer, "Hello, %s!\n", cmd.String("name"))
				return nil
			},
		},
		{
			Name:  "echo",
			Usage: "Echo back arguments",
			Action: func(ctx context.Context, cmd *cli.Command) error {
				fmt.Fprintln(cmd.Writer, cmd.Args().Slice())
				return nil
			},
		},
		{
			Name:  "version",
			Usage: "Print version",
			Action: func(ctx context.Context, cmd *cli.Command) error {
				fmt.Fprintln(cmd.Writer, "testapp v0.1.0")
				return nil
			},
		},
	}
}

// -- TUI

type tuiModel struct {
	width  int
	height int
	keys   int
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		m.keys++
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

func (m tuiModel) View() string {
	return fmt.Sprintf("Test TUI (%dx%d) — keypresses: %d\n\nPress 'q' to quit.", m.width, m.height, m.keys)
}
