package clink

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/keygen"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Connect sends args to the running daemon.
// If args is empty, it opens an interactive TUI session.
func Connect(conf Config, args []string) error {
	host := conf.Host
	if host == "" {
		host = "127.0.0.1"
	}

	if len(conf.HostPublicKey) == 0 && !isLoopback(host) {
		return fmt.Errorf("refusing to connect to non-loopback host %q without HostPublicKey; pin the daemon's host key or use a literal loopback IP (e.g. 127.0.0.1)", host)
	}

	auth, err := clientAuth(conf)
	if err != nil {
		return err
	}

	hostKeyCallback, err := hostKeyCallback(conf)
	if err != nil {
		return err
	}

	config := &ssh.ClientConfig{
		User:            "user",
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
	}

	conn, err := ssh.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(conf.Port)), config)
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

// clientAuth returns the SSH auth methods for Connect.
//
// If a password is set, password auth is used. Otherwise an ephemeral ed25519
// key is generated in memory — the server accepts any public key in this mode.
func clientAuth(conf Config) ([]ssh.AuthMethod, error) {
	if conf.Password != "" {
		return []ssh.AuthMethod{ssh.Password(conf.Password)}, nil
	}

	k, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}

	return []ssh.AuthMethod{ssh.PublicKeys(k.Signer())}, nil
}

// hostKeyCallback returns the SSH host key verification callback for Connect.
//
// If conf.HostPublicKey is set, the daemon's host key must match. Otherwise
// host key verification is disabled — only safe on loopback.
func hostKeyCallback(conf Config) (ssh.HostKeyCallback, error) {
	if len(conf.HostPublicKey) == 0 {
		return ssh.InsecureIgnoreHostKey(), nil
	}

	pub, _, _, _, err := ssh.ParseAuthorizedKey(conf.HostPublicKey)
	if err != nil {
		return nil, fmt.Errorf("parse HostPublicKey: %w", err)
	}
	return ssh.FixedHostKey(pub), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
