package urfave

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/go-devkit/clink"
	"github.com/urfave/cli/v3"
)

// ErrPanic is returned to the client whose command panicked. The panic value
// and stack go to the panic handler (see [WithPanicHandler]), not to the
// client, so an application's internals do not leak into a user's terminal.
var ErrPanic = errors.New("command panicked")

// ErrNilRoot is returned when newRoot yields a nil command.
var ErrNilRoot = errors.New("urfave: newRoot returned nil")

// Option configures [Serve] and [Handler].
type Option func(*options)

type options struct {
	onPanic     func(ctx context.Context, reason any, stack []byte)
	globalSinks bool
}

// WithPanicHandler replaces the default panic handler, which logs to
// [slog.Default]. Consumers that journal panics elsewhere can hook in here. The
// client still receives [ErrPanic] and exit code 1 regardless.
func WithPanicHandler(fn func(ctx context.Context, reason any, stack []byte)) Option {
	return func(o *options) {
		if fn != nil {
			o.onPanic = fn
		}
	}
}

// WithGlobalSinks keeps urfave's process-global cli.OsExiter and cli.ErrWriter
// untouched. The default (false) neutralises both, which is what a daemon
// wants: cli.OsExiter would otherwise take the whole daemon down on a bad
// command, and cli.ErrWriter writes to the daemon's console instead of the
// client's terminal. Set this only if the application manages those globals
// itself.
func WithGlobalSinks(keep bool) Option {
	return func(o *options) { o.globalSinks = keep }
}

func newOptions(opts []Option) options {
	o := options{onPanic: logPanic}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func logPanic(ctx context.Context, reason any, stack []byte) {
	slog.ErrorContext(ctx, "clink/urfave: command panicked",
		slog.Any("panic", reason),
		slog.String("stack", string(stack)),
	)
}

// neutraliseOnce guards the process-global mutation so repeated Serve calls do
// not race on urfave's package variables.
var neutraliseOnce sync.Once

func neutraliseGlobals() {
	neutraliseOnce.Do(func() {
		cli.OsExiter = func(int) {}
		cli.ErrWriter = io.Discard
	})
}

// Serve runs a clink daemon that dispatches each session through an urfave
// command tree.
//
// newRoot must return a fresh tree per call: urfave stores per-run state on the
// command (I/O writers, parsed flags) and handlers run concurrently, so a
// shared instance races and can leak one client's output into another's
// session.
//
// Serve blocks until ctx is cancelled, like [clink.Listen]. Use [Handler] to
// build the handler yourself when a session needs handling that is not a
// command tree — an empty-args main TUI, say.
func Serve(ctx context.Context, conf clink.Config, newRoot func(clink.Session) *cli.Command, opts ...Option) error {
	return clink.Listen(ctx, conf, Handler(newRoot, opts...))
}

// Handler builds the [clink.Handler] that [Serve] runs. It is exported for
// applications that need to wrap it — dispatching empty args to a TUI before
// falling through to the command tree, for instance.
func Handler(newRoot func(clink.Session) *cli.Command, opts ...Option) clink.Handler {
	o := newOptions(opts)
	if !o.globalSinks {
		neutraliseGlobals()
	}

	return func(ctx context.Context, s clink.Session, args []string) (err error) {
		root := newRoot(s)
		if root == nil {
			return ErrNilRoot
		}

		// urfave/cli v3 inherits Reader/Writer/ErrWriter from parent, so only
		// the root needs wiring.
		root.Reader, root.Writer, root.ErrWriter = s, s, s.Stderr()
		// Without this, urfave prints the error to the now-discarded global
		// ErrWriter and calls the neutralised OsExiter. Reporting is clink's
		// job: it writes the message to this session's stderr.
		root.ExitErrHandler = func(context.Context, *cli.Command, error) {}

		defer func() {
			if reason := recover(); reason != nil {
				o.onPanic(ctx, reason, debug.Stack())
				err = ErrPanic
			}
		}()

		// clink hands the handler args without the program name; urfave wants
		// argv[0] to be it.
		argv := append([]string{root.Name}, args...)

		return exitError(root.Run(clink.WithSession(ctx, s), argv))
	}
}

// exitError translates urfave's exit code into clink's, so cli.Exit("x", 3)
// reaches the client as exit status 3 rather than a flattened 1.
func exitError(err error) error {
	if err == nil {
		return nil
	}

	var coder cli.ExitCoder
	if errors.As(err, &coder) && coder.ExitCode() != 0 {
		return &clink.ExitError{Code: coder.ExitCode(), Err: err}
	}

	return err
}
