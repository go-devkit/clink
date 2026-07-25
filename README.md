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

    ShutdownGrace time.Duration // server: how long Listen waits for in-flight handlers after ctx cancel before force-closing. Zero = 5s.
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
    // "--file=/tmp/x" or "-f=/tmp/x") are served. Transfers are confirmed
    // end-to-end: a truncated read or a failed write surfaces as an error
    // (from Close, for the streaming forms) rather than silent partial data.
    ReadFile(path string) ([]byte, error)
    OpenFile(path string) (io.ReadCloser, error)
    WriteFile(path string, data []byte) error
    CreateFile(path string) (io.WriteCloser, error)
}

type Handler func(ctx context.Context, s Session, args []string) error

// Handler is the single dispatch point. args is empty for interactive
// (no-args) clients — return *Interactive there to launch the main TUI.
// Return ErrNotHandled if the command is unknown; the session exits 127.
// Return *ExitError to set a custom remote exit code:
//   return &clink.ExitError{Code: 2, Err: err}
// Return *Interactive to launch a Bubble Tea TUI for this command:
//   return &clink.Interactive{Model: dashboard.New()}
//   (subcommand TUIs require the client to call Connect with clink.WithPTY)
// ctx is cancelled when the client disconnects.
// Handler runs on one goroutine per session, concurrently with other sessions —
// it and everything it closes over must be goroutine-safe. See "Concurrency".

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
        // Build a fresh command tree per session: handlers run concurrently and
        // a shared *cli.Command carries per-run state (I/O writers, parsed
        // flags) that two sessions would race on.
        cmd := app.NewRoot() // *cli.Command with wire-injected subcommands
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

## Concurrency

`Handler` runs on its own goroutine per session, and clink serializes nothing:
several clients — or several sessions from one client — can be inside `Handler`
simultaneously. `Handler` and everything it closes over (CLI command tree,
wire-built services, caches, loggers) must be safe for concurrent use.

CLI frameworks are usually *not*. A single `*cli.Command` (urfave) or
`*cobra.Command` holds per-run state — the I/O writers you assign and the parsed
flag values — so reusing one instance across sessions races and can leak one
client's output into another's. Build or clone the command tree inside `Handler`,
as the example above does.

A single `Session` is per-connection and not safe for concurrent use by multiple
goroutines within one `Handler` call.

## Security model

clink assumes daemon and clients share one trust domain, typically a
loopback-bound daemon serving processes of the same user:

- **Empty `Password` means no authentication.** `Listen` then accepts *any*
  public key, and `Connect` authenticates with a throwaway in-memory key. Any
  local user or process able to reach the port gets a session — and thus
  arbitrary command execution plus file read/write on connecting clients'
  behalf. `Listen` refuses to start in this mode on a non-loopback host, but on
  a multi-user machine loopback is not a boundary: set `Password`.
- **No rate limiting, no connection or session cap.** Password auth compares a
  SHA-256 digest in constant time, but nothing throttles guesses or bounds the
  number of open connections. Don't expose the port; front it with a real
  gateway if you must.
- **Host key pinning is opt-in.** Without `HostKeyPEM` the daemon's key is
  ephemeral per start, so clients cannot pin it across restarts. For any
  non-loopback `Host`, `Connect` requires `HostPublicKey`.
- **File forwarding is allowlisted, not sandboxed.** The client only serves
  paths that appeared verbatim in the args it passed to `Connect`, so the daemon
  cannot read arbitrary client files — but it can read or overwrite anything the
  user named on the command line.

## Versioning

There is no version or protocol negotiation between `Connect` and `Listen`. The
wire contract — argv forwarding, the `file-request` channel payload, exit-code
signalling — is assumed identical on both ends because **both ends are the same
binary**. A client built against a different clink version than the running
daemon may fail in unhelpful ways instead of reporting a mismatch. Restart the
daemon after upgrading the binary.
