package cobra_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-devkit/clink"
	clinkcobra "github.com/go-devkit/clink/cobra"
	"github.com/spf13/cobra"
)

func TestTreeName(t *testing.T) {
	cmd := &cobra.Command{Use: "run [flags]"}
	if got := clinkcobra.Tree(cmd).Name(); got != "run" {
		t.Fatalf("Name() = %q, want run", got)
	}
}

func TestTreeRunDropsArgv0(t *testing.T) {
	var gotFlag string
	cmd := &cobra.Command{
		Use: "run",
		RunE: func(c *cobra.Command, _ []string) error {
			gotFlag, _ = c.Flags().GetString("addr")
			return nil
		},
	}
	cmd.Flags().String("addr", "", "listen address")

	err := clinkcobra.Tree(cmd).Run(context.Background(), []string{"run", "--addr", ":2222"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotFlag != ":2222" {
		t.Fatalf("addr = %q, want :2222", gotFlag)
	}
}

func TestTreeRunHelpPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{
		Use:   "run",
		Short: "start the daemon",
		RunE: func(*cobra.Command, []string) error {
			t.Fatal("RunE ran for --help")
			return nil
		},
	}
	cmd.SetOut(&out)

	if err := clinkcobra.Tree(cmd).Run(context.Background(), []string{"run", "--help"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "start the daemon") {
		t.Fatalf("help output = %q", out.String())
	}
}

func TestTreeRunWithEmptyArgs(t *testing.T) {
	called := false
	cmd := &cobra.Command{
		Use: "",
		RunE: func(*cobra.Command, []string) error {
			called = true
			return nil
		},
	}

	if err := clinkcobra.Tree(cmd).Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("RunE not invoked for empty args")
	}
}

func TestTreeRoutesThroughConnect(t *testing.T) {
	called := false
	cmd := &cobra.Command{
		Use: "run",
		RunE: func(*cobra.Command, []string) error {
			called = true
			return nil
		},
	}

	// Nothing is listening on the configured port, so Connect dispatches locally.
	conf := clink.Config{Host: "127.0.0.1", Port: freePort(t), Password: "pw"}
	err := clink.Connect(context.Background(), conf, []string{"run"},
		clink.WithLocalCommandTree(clinkcobra.Tree(cmd)))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !called {
		t.Fatal("tree RunE not invoked")
	}
}
