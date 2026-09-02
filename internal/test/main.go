package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/go-devkit/clink"
	"github.com/urfave/cli/v3"
)

var conf = clink.Config{
	Port:     2222,
	Password: "test",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := clink.Connect(ctx, conf, os.Args[1:],
		clink.AutoPTY(),
		clink.WithLocalCommand(clink.LocalFunc("serve", runServer)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runServer(ctx context.Context, _ []string) error {
	fmt.Println("starting server on :2222")
	return clink.Listen(ctx, conf, newHandler())
}

func newHandler() clink.Handler {
	return func(ctx context.Context, s clink.Session, args []string) error {
		if len(args) == 0 {
			return &clink.Interactive{Model: tuiModel{}}
		}

		cmd := &cli.Command{
			Name:     "testapp",
			Commands: commands(),
		}
		cmd.Reader, cmd.Writer, cmd.ErrWriter = s, s, s.Stderr()

		if cmd.Command(args[0]) == nil {
			return clink.ErrNotHandled
		}

		return cmd.Run(clink.WithSession(ctx, s), append([]string{cmd.Name}, args...))
	}
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
		{
			Name:      "cp",
			Usage:     "Server-initiated copy of a client-side file: cp <src> <dst>",
			ArgsUsage: "<src> <dst>",
			Action: func(ctx context.Context, cmd *cli.Command) error {
				s := clink.SessionFrom(ctx)
				if s == nil {
					return fmt.Errorf("no clink session on ctx")
				}

				if cmd.Args().Len() != 2 {
					return fmt.Errorf("usage: cp <src> <dst>")
				}

				src := cmd.Args().Get(0)
				dst := cmd.Args().Get(1)

				data, err := s.ReadFile(src)
				if err != nil {
					return fmt.Errorf("read %s: %w", src, err)
				}

				if err := s.WriteFile(dst, data); err != nil {
					return fmt.Errorf("write %s: %w", dst, err)
				}

				fmt.Fprintf(cmd.Writer, "copied %d bytes: %s -> %s\n", len(data), src, dst)

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
	case tea.KeyPressMsg:
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

func (m tuiModel) View() tea.View {
	v := tea.NewView(fmt.Sprintf("Test TUI (%dx%d) — keypresses: %d\n\nPress 'q' to quit.", m.width, m.height, m.keys))
	v.AltScreen = true
	return v
}
