# cligate

Connect CLI invocations to a long-running daemon instance of the same binary.

One binary, two modes: the daemon runs endlessly (`serve`), and subsequent invocations connect to it to execute CLI commands or open a TUI.

## Core API

```go
type Config struct {
    Host     string // client only; defaults to "localhost"
    Port     int
    Password string
}

type Session interface {
    io.Reader
    io.Writer
    Stderr() io.Writer
}

type Handler func(ctx context.Context, s Session, args []string) error

// Daemon side: listen for incoming commands and TUI sessions.
// Pass nil for newTUI if no TUI is needed.
cligate.Listen(ctx, conf, handler, newTUI)

// Client side: connect to the daemon and send a command.
// If args is empty, opens an interactive TUI session.
cligate.Connect(conf, args)
```

## Usage with urfave/cli v3

### Server

```go
func startServer(ctx context.Context) error {
    handler := func(ctx context.Context, s cligate.Session, args []string) error {
        cmd := &cli.Command{
            Name:     "myapp",
            Commands: commands(), // your subcommands
        }

        // Wire session I/O into the CLI command tree
        propagateWriter(s, cmd)

        if cmd.Command(args[0]) == nil {
            return cligate.ErrNotHandled
        }

        return cmd.Run(ctx, append([]string{cmd.Name}, args...))
    }

    conf := cligate.Config{Port: 2222, Password: "secret"}
    return cligate.Listen(ctx, conf, handler, newTUI)
}

func propagateWriter(s cligate.Session, cmd *cli.Command) {
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

        conf := cligate.Config{
            Host:     cmd.String("host"),
            Port:     int(cmd.Int("port")),
            Password: cmd.String("password"),
        }

        // cmd.Args().Slice() contains only the subcommand and its args,
        // excluding root-level flags like --host/--port/--password.
        if err := cligate.Connect(conf, cmd.Args().Slice()); err != nil {
            return ctx, fmt.Errorf("connection failed: %w", err)
        }

        return ctx, cli.Exit("", 0) // prevent local execution
    },
    Flags: []cli.Flag{
        &cli.StringFlag{Name: "host", Value: "localhost"},
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
    handler := func(ctx context.Context, s cligate.Session, args []string) error {
        cmd := newRootCmd() // your cobra root command

        cmd.SetIn(s)
        cmd.SetOut(s)
        cmd.SetErr(s.Stderr())
        cmd.SetArgs(args)

        err := cmd.ExecuteContext(ctx)
        if err != nil && strings.Contains(err.Error(), "unknown command") {
            return cligate.ErrNotHandled
        }

        return err
    }

    conf := cligate.Config{Port: 2222, Password: "secret"}
    return cligate.Listen(ctx, conf, handler, nil)
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

        conf := cligate.Config{Host: host, Port: port, Password: password}

        // Build forwarded args: subcommand name + its own flags + positional args.
        // This excludes persistent connection flags (--host/--port/--password).
        fwdArgs := []string{cmd.Name()}
        cmd.NonInheritedFlags().Visit(func(f *pflag.Flag) {
            fwdArgs = append(fwdArgs, "--"+f.Name, f.Value.String())
        })
        fwdArgs = append(fwdArgs, args...)

        if err := cligate.Connect(conf, fwdArgs); err != nil {
            return fmt.Errorf("connection failed: %w", err)
        }

        os.Exit(0) // prevent local execution
        return nil
    },
}

func init() {
    rootCmd.PersistentFlags().String("host", "localhost", "daemon host")
    rootCmd.PersistentFlags().Int("port", 2222, "daemon port")
    rootCmd.PersistentFlags().String("password", "", "daemon password")
}
```
