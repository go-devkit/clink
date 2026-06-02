// Package urfave wraps clink for use with urfave/cli v3.
//
// The same *cli.Command tree is used on both the client side (forwards the
// invocation to the daemon) and the server side (executes the action). Wrap
// individual actions with Wrap or WrapTUI, attach a serve subcommand via
// WithServe, set the connection config with SetDefault, then call cmd.Run as
// usual.
package urfave

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sync"

	"github.com/go-devkit/clink"
	"github.com/urfave/cli/v3"
)

var wrappedSet sync.Map

type serverMarker struct{}

func withServerMarker(ctx context.Context) context.Context {
	return context.WithValue(ctx, serverMarker{}, true)
}

func isServerSide(ctx context.Context) bool {
	v, _ := ctx.Value(serverMarker{}).(bool)
	return v
}

var (
	defaultMu   sync.RWMutex
	defaultConf clink.Config
)

// SetDefault sets the Config that Wrap/WrapTUI use to reach the daemon. Call
// once during program init before the cli root runs.
func SetDefault(c clink.Config) {
	defaultMu.Lock()
	defaultConf = c
	defaultMu.Unlock()
}

func conf() clink.Config {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultConf
}

// Wrap returns an Action that forwards to the daemon when invoked client-side
// and runs fn when invoked server-side (from inside the Serve dispatch).
func Wrap(fn cli.ActionFunc) cli.ActionFunc {
	return wrap(fn, false)
}

// WrapTUI is like Wrap, but requests a PTY on the client so fn can return a
// *clink.Interactive to launch a Bubble Tea TUI.
func WrapTUI(fn cli.ActionFunc) cli.ActionFunc {
	return wrap(fn, true)
}

func wrap(fn cli.ActionFunc, tui bool) cli.ActionFunc {
	wrapped := func(ctx context.Context, cmd *cli.Command) error {
		if isServerSide(ctx) {
			return fn(ctx, cmd)
		}
		var opts []clink.ConnectOption
		if tui {
			opts = append(opts, clink.WithPTY())
		}
		if err := clink.Connect(conf(), os.Args[1:], opts...); err != nil {
			return fmt.Errorf("daemon connection failed: %w", err)
		}
		return cli.Exit("", 0)
	}
	wrappedSet.Store(reflect.ValueOf(wrapped).Pointer(), true)
	return wrapped
}

func isWrapped(fn cli.ActionFunc) bool {
	if fn == nil {
		return true
	}
	_, ok := wrappedSet.Load(reflect.ValueOf(fn).Pointer())
	return ok
}

// WrapAll returns copies of the given commands with all unwrapped actions
// recursively wrapped via Wrap. Actions already wrapped with Wrap, WrapTUI,
// or Serve are left as-is.
func WrapAll(commands ...*cli.Command) []*cli.Command {
	out := make([]*cli.Command, len(commands))

	for idx, cmd := range commands {
		clone := *cmd

		if !isWrapped(clone.Action) {
			clone.Action = Wrap(clone.Action)
		}

		clone.Commands = WrapAll(clone.Commands...)
		out[idx] = &clone
	}

	return out
}

// Serve wraps an Action with the clink daemon. It starts clink.Listen in the
// background, then calls fn. When fn returns (success or error), the daemon
// shuts down and Serve returns fn's error. This means fn controls the lifetime
// of the entire process — use it for the main application loop.
func Serve(fn cli.ActionFunc) cli.ActionFunc {
	wrapped := func(ctx context.Context, cmd *cli.Command) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		root := cmd.Root()

		handler := func(hctx context.Context, s clink.Session, args []string) error {
			fresh := cloneTree(root)
			wireSession(s, fresh)
			runArgs := append([]string{fresh.Name}, args...)
			return fresh.Run(withServerMarker(hctx), runArgs)
		}

		go func() {
			if err := clink.Listen(ctx, conf(), handler); err != nil && ctx.Err() == nil {
				slog.Error("clink listener error", "error", err)
			}
		}()

		return fn(ctx, cmd)
	}
	wrappedSet.Store(reflect.ValueOf(wrapped).Pointer(), true)
	return wrapped
}

func cloneTree(src *cli.Command) *cli.Command {
	dst := *src
	dst.Commands = make([]*cli.Command, len(src.Commands))
	for i, sub := range src.Commands {
		dst.Commands[i] = cloneTree(sub)
	}
	return &dst
}

func wireSession(s clink.Session, cmd *cli.Command) {
	cmd.Reader = s
	cmd.Writer = s
	cmd.ErrWriter = s.Stderr()
	for _, sub := range cmd.Commands {
		wireSession(s, sub)
	}
}
