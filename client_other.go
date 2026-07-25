//go:build !unix

package clink

import "golang.org/x/crypto/ssh"

// forwardWinch is a no-op on platforms without SIGWINCH. Terminal-resize
// forwarding is only wired up on unix (see client_unix.go).
func forwardWinch(_ *ssh.Session) func() {
	return func() {}
}
