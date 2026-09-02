// Package cobra adapts a spf13/cobra command tree to [clink.LocalCommand], so a
// locally-dispatched command is named once — by the tree itself — instead of
// once for clink's routing and once in the command definition.
//
// It lives in its own module so that consumers of clink who do not use cobra
// never take on the dependency.
package cobra

import (
	"context"

	"github.com/go-devkit/clink"
	"github.com/spf13/cobra"
)

// Tree wraps cmd so it can be passed to [clink.WithLocalCommand] or
// [clink.WithLocalFallback]. The routing key is cmd.Name(), i.e. the first
// word of cmd.Use.
//
// cmd is built before Connect decides whether the command is selected, so its
// constructor must stay cheap — assemble the tree, but do the server-only wiring
// inside the RunE functions.
func Tree(cmd *cobra.Command) clink.LocalCommand {
	return tree{cmd: cmd}
}

type tree struct {
	cmd *cobra.Command
}

func (t tree) Name() string {
	return t.cmd.Name()
}

func (t tree) Run(ctx context.Context, args []string) error {
	// clink passes argv with args[0] being the command name; cobra's SetArgs
	// expects the arguments *after* the program name, so drop it here rather
	// than letting cobra parse the name as a subcommand.
	if len(args) > 0 {
		args = args[1:]
	}

	t.cmd.SetArgs(args)

	return t.cmd.ExecuteContext(ctx)
}
