package cligate

import (
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Dial connects to a remote SSH server and forwards the given args as a command.
// If args is empty, it requests a PTY and launches the TUI.
func Dial(host, port, password string, args []string) error {
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

	if len(args) > 0 {
		session.Stdin = os.Stdin
		session.Stdout = os.Stdout
		session.Stderr = os.Stderr

		quoted := make([]string, len(args))
		for i, arg := range args {
			quoted[i] = shellQuote(arg)
		}
		return session.Run(strings.Join(quoted, " "))
	}

	// For interactive/TUI mode, set up terminal and PTY
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width, height = 80, 24
	}

	termType := os.Getenv("TERM")
	if termType == "" {
		termType = "xterm-256color"
	}

	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
		ssh.ICRNL:         1,
		ssh.IXON:          0,
		ssh.IXANY:         0,
		ssh.IMAXBEL:       0,
		ssh.IUTF8:         1,
	}
	if err := session.RequestPty(termType, height, width, modes); err != nil {
		return fmt.Errorf("failed to request PTY: %w", err)
	}

	return session.Run("tui")
}

// shellQuote quotes a string using POSIX single-quote escaping,
// safe for parsing by go-shlex on the server side.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
