# cligate

A framework-agnostic remote CLI/TUI gateway over SSH.

Forward CLI commands and [BubbleTea](https://github.com/charmbracelet/bubbletea) TUIs to a remote server via SSH — works with any CLI framework or none at all.

## Core API

```go
// Client: forward command-line args to a remote SSH server.
cligate.Dial(host, port, password string, args []string) error

// Server: handle incoming SSH commands and TUI sessions.
cligate.Serve(ctx, port, password string, handler Handler, newTUI func(ssh.Session) (tea.Model, []tea.ProgramOption)) error

// Handler processes a CLI command received over SSH.
// Return ErrNotHandled to fall through to the TUI.
type Handler func(ctx context.Context, s ssh.Session, args []string) error

// QuitTea returns a no-op BubbleTea model for when no TUI is needed.
cligate.QuitTea() tea.Model
```

## Usage with urfave/cli v3

### Server

```go
func startServer(ctx context.Context) error {
    handler := func(ctx context.Context, s ssh.Session, args []string) error {
        cmd := &cli.Command{
            Name:     "myapp",
            Commands: commands(), // your subcommands
        }

        // Wire SSH session I/O into the CLI command tree
        propagateWriter(s, cmd)

        if cmd.Command(args[0]) == nil {
            return cligate.ErrNotHandled
        }

        return cmd.Run(ctx, append([]string{cmd.Name}, args...))
    }

    return cligate.Serve(ctx, "2222", "secret", handler, newTUI)
}

func propagateWriter(s ssh.Session, cmd *cli.Command) {
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

        host := cmd.String("host")
        port := strconv.Itoa(cmd.Int("port"))
        password := cmd.String("password")

        if err := cligate.Dial(host, port, password, os.Args); err != nil {
            return ctx, fmt.Errorf("server connection failed: %w", err)
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
    handler := func(ctx context.Context, s ssh.Session, args []string) error {
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

    return cligate.Serve(ctx, "2222", "secret", handler, newTUI)
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

        host, _ := cmd.Flags().GetString("host")
        port, _ := cmd.Flags().GetInt("port")
        password, _ := cmd.Flags().GetString("password")

        if err := cligate.Dial(host, strconv.Itoa(port), password, os.Args); err != nil {
            return fmt.Errorf("server connection failed: %w", err)
        }

        os.Exit(0) // prevent local execution
        return nil
    },
}

func init() {
    rootCmd.PersistentFlags().String("host", "localhost", "remote server host")
    rootCmd.PersistentFlags().Int("port", 2222, "remote server port")
    rootCmd.PersistentFlags().String("password", "", "remote server password")
}
```
