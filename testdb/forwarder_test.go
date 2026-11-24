package testdb

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// A relay blocked writing to a server that stopped reading still ends with its test: the cleanup closes the server
// side too. The peer here accepts and never reads, as a stalled server would. A relay that kept waiting would hold the
// subtest's cleanup, and the test fails at its timeout.
func TestRelayToAServerThatStoppedReadingEndsWithItsTest(t *testing.T) {
	server, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = server.Close()
	})
	go func() {
		conn, err := server.Accept()
		if err != nil {
			return
		}
		<-done // holds the connection open without reading until the test ends
		_ = conn.Close()
	}()
	_, port, _ := net.SplitHostPort(server.Addr().String())

	t.Run("owner", func(owner *testing.T) {
		relay, err := NewForwarder(owner, &Database{Name: PlaintextPrefix + "stalled", server: Server{Host: "127.0.0.1", Port: port}})
		if err != nil {
			owner.Fatal(err)
		}
		client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.Settings("x").Port)))
		if err != nil {
			owner.Fatal(err)
		}
		defer func() { _ = client.Close() }()
		startup := binary.BigEndian.AppendUint32(nil, 8)
		startup = binary.BigEndian.AppendUint32(startup, 80877103)
		query := append([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, maxMessage-1)...)
		query = append(query, make([]byte, maxMessage-5)...)
		if _, err := client.Write(startup); err != nil {
			owner.Fatal(err)
		}
		// Writes until one cannot complete: the relay then sits blocked writing to the server that does not read.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			_ = client.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := client.Write(query); errors.Is(err, os.ErrDeadlineExceeded) {
				return
			} else if err != nil {
				owner.Fatal(err)
			}
		}
		owner.Fatal("the relay never blocked: the server kept reading")
	})
}
