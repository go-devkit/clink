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

// Client side: connect to the daemon and send a command.
// If args is empty, opens an interactive TUI session.
clink.Connect(conf, args)

// For subcommands whose Handler returns *Interactive, opt the client
// into PTY allocation so the TUI can render:
clink.Connect(conf, args, clink.WithPTY())
```

## Usage with urfave/cli v3

The `github.com/go-devkit/clink/urfave` subpackage removes the boilerplate.
Wrap each Action with `Wrap` (or `WrapTUI` for Bubble Tea commands), attach
`Serve(factory)` to any subcommand you want, and run as a normal CLI. Same
binary, same tree, both modes.

```go
import (
    "context"
    "fmt"
    "os"

    "github.com/go-devkit/clink"
    "github.com/go-devkit/clink/urfave"
    "github.com/urfave/cli/v3"
)

func newRoot() *cli.Command {
    return &cli.Command{
        Name: "myapp",
        // Optional: main TUI when invoked with no subcommand.
        Action: urfave.WrapTUI(func(_ context.Context, _ *cli.Command) error {
            return &clink.Interactive{Model: newMainTUI()}
        }),
        Commands: []*cli.Command{
            {
                Name:  "greet",
                Flags: []cli.Flag{&cli.StringFlag{Name: "name", Value: "world"}},
                Action: urfave.Wrap(func(_ context.Context, cmd *cli.Command) error {
                    fmt.Fprintf(cmd.Writer, "hello %s\n", cmd.String("name"))
                    return nil
                }),
            },
            {
                Name: "dashboard",
                Action: urfave.WrapTUI(func(_ context.Context, _ *cli.Command) error {
                    return &clink.Interactive{Model: newDashboard()}
                }),
            },
            {
                Name:   "daemon",
                Usage:  "Run the clink server",
                Action: urfave.Serve(newRoot),
            },
        },
    }
}

func main() {
    urfave.SetDefault(clink.Config{Port: 2222, Password: "secret"})

    if err := newRoot().Run(context.Background(), os.Args); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

- `myapp daemon` — runs the server.
- `myapp greet --name alice` — forwards to the daemon and prints `hello alice`.
- `myapp dashboard` — forwards with a PTY, server runs the Bubble Tea model.
- `myapp` (no subcommand) — opens the main TUI via SSH `shell`.

`Serve` calls the factory once per incoming session, so the command tree is
isolated between concurrent invocations.

## Usage with cobra

### Server

```go
func startServer(ctx context.Context) error {
    handler := func(ctx context.Context, s clink.Session, args []string) error {
        cmd := newRootCmd() // your cobra root command

        cmd.SetIn(s)
        cmd.SetOut(s)
        cmd.SetErr(s.Stderr())
        cmd.SetArgs(args)

        err := cmd.ExecuteContext(ctx)
        if err != nil && strings.Contains(err.Error(), "unknown command") {
            return clink.ErrNotHandled
        }

        return err
    }

    conf := clink.Config{Port: 2222, Password: "secret"}
    return clink.Listen(ctx, conf, handler)
}
```

### Client

```go
var rootCmd = &cobra.Command{
    Use: "myapp",
    PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
        if cmd.Name() == "serve" {
            return nil // don't forward the serve command itself
        }

        host, _ := cmd.Root().PersistentFlags().GetString("host")
        port, _ := cmd.Root().PersistentFlags().GetInt("port")
        password, _ := cmd.Root().PersistentFlags().GetString("password")

        conf := clink.Config{Host: host, Port: port, Password: password}

        // Build forwarded args: subcommand name + its own flags + positional args.
        // This excludes persistent connection flags (--host/--port/--password).
        fwdArgs := []string{cmd.Name()}
        cmd.NonInheritedFlags().Visit(func(f *pflag.Flag) {
            fwdArgs = append(fwdArgs, "--"+f.Name, f.Value.String())
        })
        fwdArgs = append(fwdArgs, args...)

        if err := clink.Connect(conf, fwdArgs); err != nil {
            return fmt.Errorf("connection failed: %w", err)
        }

        os.Exit(0) // prevent local execution
        return nil
    },
}

func init() {
    rootCmd.PersistentFlags().String("host", "127.0.0.1", "daemon host")
    rootCmd.PersistentFlags().Int("port", 2222, "daemon port")
    rootCmd.PersistentFlags().String("password", "", "daemon password")
}
```
