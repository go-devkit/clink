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

type Session interface {
    io.Reader
    io.Writer
    Stderr() io.Writer
}

type Handler func(ctx context.Context, s Session, args []string) error

// Daemon side: listen for incoming commands and TUI sessions.
// Pass nil for newTUI if no TUI is needed.
clink.Listen(ctx, conf, handler, newTUI)

// Client side: connect to the daemon and send a command.
// If args is empty, opens an interactive TUI session.
clink.Connect(conf, args)
```

## Usage with urfave/cli v3

### Server

```go
func startServer(ctx context.Context) error {
    handler := func(ctx context.Context, s clink.Session, args []string) error {
        cmd := &cli.Command{
            Name:     "myapp",
            Commands: commands(), // your subcommands
        }

        // Wire session I/O into the CLI command tree
        propagateWriter(s, cmd)

        if cmd.Command(args[0]) == nil {
            return clink.ErrNotHandled
        }

        return cmd.Run(ctx, append([]string{cmd.Name}, args...))
    }

    conf := clink.Config{Port: 2222, Password: "secret"}
    return clink.Listen(ctx, conf, handler, newTUI)
}

func propagateWriter(s clink.Session, cmd *cli.Command) {
    cmd.Reader = s
    cmd.Writer = s
    cmd.ErrWriter = s.Stderr()

    for _, sub := range cmd.Commands {
        propagateWriter(s, sub)
    }
}
```

### Client

```go
cmd := &cli.Command{
    Name: "myapp",
    Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
        if cmd.Args().First() == "serve" {
            return ctx, nil // don't forward the serve command itself
        }

        conf := clink.Config{
            Host:     cmd.String("host"),
            Port:     int(cmd.Int("port")),
            Password: cmd.String("password"),
        }

        // cmd.Args().Slice() contains only the subcommand and its args,
        // excluding root-level flags like --host/--port/--password.
        if err := clink.Connect(conf, cmd.Args().Slice()); err != nil {
            return ctx, fmt.Errorf("connection failed: %w", err)
        }

        return ctx, cli.Exit("", 0) // prevent local execution
    },
    Flags: []cli.Flag{
        &cli.StringFlag{Name: "host", Value: "127.0.0.1"},
        &cli.IntFlag{Name: "port", Value: 2222},
        &cli.StringFlag{Name: "password"},
    },
    Commands: commands(),
}
```

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
    return clink.Listen(ctx, conf, handler, nil)
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
