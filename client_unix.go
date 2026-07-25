//go:build unix

package clink

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// forwardWinch relays client-side SIGWINCH to the remote PTY session so the
// server-side TUI resizes, sending the current terminal size on each change.
// The returned func stops forwarding. SIGWINCH is unix-only; see the no-op
// stub in client_other.go for other platforms.
func forwardWinch(session *ssh.Session) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				width, height, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					continue
				}
				// Cosmetic resize; a failed send leaves a stale size that the
				// next SIGWINCH corrects, so nothing to handle.
				_ = session.WindowChange(height, width)
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(ch)
		close(done)
	}
}
