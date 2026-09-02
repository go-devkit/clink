package urfave_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-devkit/clink"
	"github.com/go-devkit/clink/urfave"
	"github.com/urfave/cli/v3"
)

func TestTreeName(t *testing.T) {
	cmd := &cli.Command{Name: "run"}
	if got := urfave.Tree(cmd).Name(); got != "run" {
		t.Fatalf("Name() = %q, want %q", got, cmd.Name)
	}
}

func TestTreeRunPassesArgsVerbatim(t *testing.T) {
	var gotFlag string
	cmd := &cli.Command{
		Name:  "run",
		Flags: []cli.Flag{&cli.StringFlag{Name: "addr"}},
		Action: func(_ context.Context, c *cli.Command) error {
			gotFlag = c.String("addr")
			return nil
		},
	}

	err := urfave.Tree(cmd).Run(context.Background(), []string{"run", "--addr", ":2222"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotFlag != ":2222" {
		t.Fatalf("addr = %q, want :2222", gotFlag)
	}
}

func TestTreeRunHelpPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	cmd := &cli.Command{
		Name:   "run",
		Usage:  "start the daemon",
		Writer: &out,
		Action: func(context.Context, *cli.Command) error {
			t.Fatal("action ran for --help")
			return nil
		},
	}

	if err := urfave.Tree(cmd).Run(context.Background(), []string{"run", "--help"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "start the daemon") {
		t.Fatalf("help output = %q", out.String())
	}
}

func TestTreeRoutesThroughConnect(t *testing.T) {
	called := false
	cmd := &cli.Command{
		Name: "run",
		Action: func(context.Context, *cli.Command) error {
			called = true
			return nil
		},
	}

	// Nothing is listening on the configured port, so Connect dispatches locally.
	conf := clink.Config{Host: "127.0.0.1", Port: freePort(t), Password: "pw"}
	err := clink.Connect(context.Background(), conf, []string{"run"},
		clink.WithLocalCommandTree(urfave.Tree(cmd)))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !called {
		t.Fatal("tree action not invoked")
	}
}
