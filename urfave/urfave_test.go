package urfave

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/go-devkit/clink"
	"github.com/urfave/cli/v3"
	gossh "golang.org/x/crypto/ssh"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// startDaemon spawns the daemon via a serve subcommand using Serve and waits
// until it accepts connections.
func startDaemon(t *testing.T, factory func() *cli.Command) (clink.Config, func()) {
	t.Helper()
	port := freePort(t)
	cfg := clink.Config{Host: "127.0.0.1", Port: port, Password: "pw"}
	SetDefault(cfg)

	root := factory()
	root.Commands = append(root.Commands, &cli.Command{
		Name: "serve",
		Action: Serve(func(ctx context.Context, _ *cli.Command) error {
			<-ctx.Done()
			return nil
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.Run(ctx, []string{root.Name, "serve"}) }()

	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			c.Close()
			ready = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("daemon exited early: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatal("daemon did not become ready")
	}
	return cfg, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	}
}

func dial(t *testing.T, cfg clink.Config) *gossh.Client {
	t.Helper()
	c, err := gossh.Dial("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), &gossh.ClientConfig{
		User:            "user",
		Auth:            []gossh.AuthMethod{gossh.Password(cfg.Password)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWrapServerSideDispatches(t *testing.T) {
	var gotName string
	var mu sync.Mutex
	factory := func() *cli.Command {
		return &cli.Command{
			Name: "myapp",
			Commands: []*cli.Command{{
				Name:  "greet",
				Flags: []cli.Flag{&cli.StringFlag{Name: "name", Value: "world"}},
				Action: Wrap(func(_ context.Context, cmd *cli.Command) error {
					mu.Lock()
					gotName = cmd.String("name")
					mu.Unlock()
					fmt.Fprintf(cmd.Writer, "hello %s", cmd.String("name"))
					return nil
				}),
			}},
		}
	}
	cfg, stop := startDaemon(t, factory)
	defer stop()

	c := dial(t, cfg)
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("greet --name alice")
	if err != nil {
		t.Fatalf("run: %v (out=%q)", err, out)
	}
	if string(out) != "hello alice" {
		t.Fatalf("stdout: %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotName != "alice" {
		t.Fatalf("server saw name=%q", gotName)
	}
}

type recordingTUI struct{ ran chan struct{} }

func (m recordingTUI) Init() tea.Cmd { return nil }
func (m recordingTUI) Update(_ tea.Msg) (tea.Model, tea.Cmd) {
	select {
	case <-m.ran:
	default:
		close(m.ran)
	}
	return m, tea.Quit
}
func (m recordingTUI) View() tea.View { return tea.NewView("") }

func TestWrapTUIServerSideLaunchesInteractive(t *testing.T) {
	ran := make(chan struct{})
	factory := func() *cli.Command {
		return &cli.Command{
			Name: "myapp",
			Commands: []*cli.Command{{
				Name: "dashboard",
				Action: WrapTUI(func(_ context.Context, _ *cli.Command) error {
					return &clink.Interactive{Model: recordingTUI{ran: ran}}
				}),
			}},
		}
	}
	cfg, stop := startDaemon(t, factory)
	defer stop()

	c := dial(t, cfg)
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Run("dashboard") }()

	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("TUI never ran")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not close after TUI quit")
	}
}

func TestRootActionRunsAsMainTUI(t *testing.T) {
	ran := make(chan struct{})
	factory := func() *cli.Command {
		return &cli.Command{
			Name: "myapp",
			Action: WrapTUI(func(_ context.Context, _ *cli.Command) error {
				return &clink.Interactive{Model: recordingTUI{ran: ran}}
			}),
		}
	}
	cfg, stop := startDaemon(t, factory)
	defer stop()

	c := dial(t, cfg)
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("main TUI never ran")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not close")
	}
}

func TestServerMarkerNotLeakedToClientSide(t *testing.T) {
	// Sanity: bare Wrap-wrapped action invoked locally (no server marker)
	// should attempt a Connect — verified by pointing at an unreachable port.
	SetDefault(clink.Config{Host: "127.0.0.1", Port: 1, Password: "pw"})
	defer SetDefault(clink.Config{})

	called := false
	fn := Wrap(func(_ context.Context, _ *cli.Command) error {
		called = true
		return nil
	})
	err := fn(context.Background(), &cli.Command{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "daemon connection failed") {
		t.Fatalf("expected connection failure, got %v", err)
	}
	if called {
		t.Fatal("server-side fn ran on client path")
	}
}
