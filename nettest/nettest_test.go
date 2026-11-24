package nettest

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A connection to the closed port is refused at once, not left waiting.
func TestClosedPortRefusesConnections(t *testing.T) {
	_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ClosedPort(t))), 5*time.Second)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("dial the closed port = %v, want connection refused", err)
	}
}
