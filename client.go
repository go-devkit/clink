package cligate

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func Forward(opts ...Option) func(context.Context, *cli.Command) (context.Context, error) {
	options := toOpts(opts)

	return func(ctx context.Context, cmd *cli.Command) (_ context.Context, err error) {
		if options.Before != nil {
			ctx, err = options.Before(ctx, cmd)
			if err != nil {
				return ctx, fmt.Errorf("before failed: %w", err)
			}
		}

		defer func() {
			if err == nil && options.After != nil {
				ctx, err = options.After(ctx, cmd)
			}
		}()

		cmdName := cmd.Args().First()

		if slices.Contains(options.IgnoreCommands, cmdName) {
			return ctx, nil // do not forward
		}

		// Try to execute via SSH
		host := cmd.String(options.FlagHost)
		port := strconv.Itoa(cmd.Int(options.FlagPort))
		password := cmd.String(options.FlagPassword)

		if err := forwardViaSSH(os.Args, host, port, password); err != nil {
			return ctx, fmt.Errorf("server connection failed: %w", err)
		}

		return ctx, cli.Exit("", 0)
	}
}

// forwardViaSSH executes commands on SSH server using native Go SSH client
func forwardViaSSH(args []string, host, port, password string) error {
	config := &ssh.ClientConfig{
		User: "user",
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	conn, err := ssh.Dial("tcp", net.JoinHostPort(host, port), config)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Execute command - skip program name but include the actual command
	if len(args) > 1 {
		// Connect session I/O to client
		session.Stdin = os.Stdin
		session.Stdout = os.Stdout
		session.Stderr = os.Stderr

		quotedArgs := make([]string, len(args)-1)
		for i, arg := range args[1:] {
			if strings.ContainsAny(arg, " \t\n") {
				quotedArgs[i] = strconv.Quote(arg)
			} else {
				quotedArgs[i] = arg
			}
		}
		remoteCmd := strings.Join(quotedArgs, " ")
		return session.Run(remoteCmd)
	}

	// For interactive/TUI mode, set up terminal and PTY
	// Put local terminal in raw mode for proper TUI interaction
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Get actual terminal size
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width, height = 80, 24 // fallback
	}

	// Get terminal type from environment
	termType := os.Getenv("TERM")
	if termType == "" {
		termType = "xterm-256color" // fallback to reasonable default
	}

	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	// Request PTY with proper terminal modes for TUI
	modes := ssh.TerminalModes{
		ssh.ECHO:          0, // Don't echo
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
		ssh.ICRNL:         1, // Translate CR to NL
		ssh.IXON:          0, // Disable XON/XOFF
		ssh.IXANY:         0,
		ssh.IMAXBEL:       0,
		ssh.IUTF8:         1, // UTF-8
	}
	if err := session.RequestPty(termType, height, width, modes); err != nil {
		return fmt.Errorf("failed to request PTY: %w", err)
	}

	// If no command provided, default to TUI
	return session.Run("tui")
}
