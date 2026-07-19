# clink

Connect CLI invocations to a long-running daemon instance of the same binary.

One binary, two modes: the daemon runs endlessly (`serve`), and subsequent invocations connect to it to execute CLI commands or open a TUI.

## Core API

```go
type Config struct {
    Host     string // defaults to "127.0.0.1" on both server and client
    Port     int
    Password string // optional; empty disables auth (loopback only; not safe on multi-user hosts)

    HostKeyPEM    []byte // server: optional persisted SSH host private key (PEM). Empty = ephemeral per Listen.
    HostPublicKey []byte // client: pinned host public key (authorized_keys format). Required for non-loopback Host; empty = no verification (loopback only, MITM risk on remote).
}
```

> **Note:** Host is checked as a literal IP. Hostnames (e.g. `"localhost"`) are
> treated as non-loopback — Listen rejects them when Password is empty, and
> Connect rejects them when HostPublicKey is empty. Use a literal loopback IP
> (e.g. `127.0.0.1`) or set Password / HostPublicKey explicitly.

```go

type Session interface {
    io.Reader
    io.Writer
    Stderr() io.Writer

    // File forwarding: server reads/writes files on the client's local
    // filesystem. The client enforces an exact-string allowlist derived from
    // the args it passed to Connect — only paths matching one of those argv
    // strings (or the RHS of any "-"-prefixed "key=value" arg, e.g.
    // "--file=/tmp/x" or "-f=/tmp/x") are served.
    ReadFile(path string) ([]byte, error)
    OpenFile(path string) (io.ReadCloser, error)
    WriteFile(path string, data []byte) error
    CreateFile(path string) (io.WriteCloser, error)
}

type Handler func(ctx context.Context, s Session, args []string) error

// Handler is the single dispatch point. args is empty for interactive
// (no-args) clients — return *Interactive there to launch the main TUI.
// Return ErrNotHandled if the command is unknown; the session closes.
// Return *ExitError to set a custom remote exit code:
//   return &clink.ExitError{Code: 2, Err: err}
// Return *Interactive to launch a Bubble Tea TUI for this command:
//   return &clink.Interactive{Model: dashboard.New()}
//   (subcommand TUIs require the client to call Connect with clink.WithPTY)
// ctx is cancelled when the client disconnects.

// Daemon side: listen for incoming commands and TUI sessions.
clink.Listen(ctx, conf, handler)

// Client side. Connect forwards args to the daemon.
// AutoPTY allocates a PTY iff os.Stdin is a terminal (mirrors ssh's default),
// so the same call handles TUI subcommands and pipe-friendly plain commands.
// WithLocalCommand names a subcommand that must run locally instead of being
// forwarded (typically the one that starts the daemon). Connect refuses to
// invoke a local command when a daemon is already reachable, preventing a
// double-run.
// WithLocalFallback names a command that runs locally ONLY when no daemon is
// reachable; if the daemon is up, Connect forwards as usual. Use name "" to
// match the no-args invocation — handy for an entry that opens the dashboard
// when the daemon runs and starts the daemon otherwise.
clink.Connect(ctx, conf, args,
    clink.AutoPTY(),
    clink.WithLocalCommand("run", runLocally),
    clink.WithLocalFallback("", runLocally),
)
```

## Usage

clink is framework-agnostic: the client only forwards, so it doesn't need any
CLI framework. Only the `run` command (which starts the daemon) uses one on
its way to `clink.Listen`. Below uses urfave/cli v3; cobra works identically.

```go
package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"

    "github.com/go-devkit/clink"
    "github.com/urfave/cli/v3"
)

var conf = clink.Config{Port: 2222, Password: "secret"}

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    err := clink.Connect(ctx, conf, os.Args[1:],
        clink.AutoPTY(),
        clink.WithLocalCommand("run", runLocally),
        // Optional: `myapp` (no subcommand) starts the daemon if none is up,
        // else forwards and opens the main TUI.
        clink.WithLocalFallback("", runLocally),
    )
    if err != nil {
        os.Exit(1)
    }
}

// runLocally is invoked instead of Connect when args[0] == "run".
// This is where wire/DI builds the full application and hands the CLI
// tree to clink.Listen.
func runLocally(ctx context.Context, _ []string) error {
    // e.g. wire.BuildApp() — full DI happens ONLY here, on the server env.
    app, cleanup, err := buildApp(ctx)
    if err != nil {
        return err
    }
    defer cleanup()

    handler := func(hctx context.Context, s clink.Session, args []string) error {
        cmd := app.Root() // *cli.Command with wire-injected subcommands
        // urfave/cli v3 inherits Reader/Writer/ErrWriter from parent, so only
        // the root needs wiring.
        cmd.Reader, cmd.Writer, cmd.ErrWriter = s, s, s.Stderr()
        return cmd.Run(clink.WithSession(hctx, s), append([]string{cmd.Name}, args...))
    }
    return clink.Listen(ctx, conf, handler)
}
```

- `myapp run` — starts the daemon; refuses if one is already up.
- `myapp anything else …` — forwards to the daemon. PTY allocated when stdin is a tty (TUI subcommands work); no PTY when piped (`myapp report | jq` works).
- `myapp` (no subcommand) — opens the daemon's shell-mode session; Handler receives empty args and can return `*Interactive` for the main TUI.

The client binary never touches wire, DB, or any server-only service. Only
`runLocally` does. Same binary on both sides.
