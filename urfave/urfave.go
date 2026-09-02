// Package urfave adapts a urfave/cli v3 command tree to [clink.LocalCommand],
// so a locally-dispatched command is named once — by the tree itself — instead
// of once for clink's routing and once in the command definition.
//
// It lives in its own module so that consumers of clink who do not use
// urfave/cli never take on the dependency.
package urfave

import (
	"context"

	"github.com/go-devkit/clink"
	"github.com/urfave/cli/v3"
)

// Tree wraps cmd so it can be passed to [clink.WithLocalCommand] or
// [clink.WithLocalFallback]. The routing key is cmd.Name.
//
// cmd is built before Connect decides whether the command is selected, so its
// constructor must stay cheap — assemble the tree, but do the server-only wiring
// inside the actions.
func Tree(cmd *cli.Command) clink.LocalCommand {
	return tree{cmd: cmd}
}

type tree struct {
	cmd *cli.Command
}

func (t tree) Name() string {
	return t.cmd.Name
}

func (t tree) Run(ctx context.Context, args []string) error {
	// args[0] is already the command name and urfave/cli treats argv[0] as the
	// program name — prepending cmd.Name here would shift every flag by one.
	return t.cmd.Run(ctx, args)
}
