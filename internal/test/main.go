package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/go-devkit/cligate"
	"github.com/urfave/cli/v3"
)

var defaultCfg = cligate.Config{
	Port:     2222,
	Password: "test",
}

func main() {
	cmd := &cli.Command{
		Name: "testapp",
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.Args().First() == "serve" {
				return ctx, nil
			}

			cfg := cligate.Config{
				Host:     cmd.String("host"),
				Port:     int(cmd.Int("port")),
				Password: cmd.String("password"),
			}

			if err := cligate.Connect(cfg, cmd.Args().Slice()); err != nil {
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
			return cligate.Listen(ctx, defaultCfg, newHandler(), newTUI)
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newHandler() cligate.Handler {
	return func(ctx context.Context, s cligate.Session, args []string) error {
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

func propagateWriter(s cligate.Session, cmd *cli.Command) {
	cmd.Reader = s
	cmd.Writer = s
	cmd.ErrWriter = s.Stderr()

	for _, sub := range cmd.Commands {
		propagateWriter(s, sub)
	}
}

func newTUI() (tea.Model, []tea.ProgramOption) {
	return tuiModel{}, []tea.ProgramOption{tea.WithAltScreen()}
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
