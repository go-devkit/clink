package cobra_test

import (
	"net"
	"testing"
)

// freePort returns a port with nothing listening on it, so Connect falls
// through to local dispatch.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // port is only needed as a number; a close error cannot affect that

	return port
}
