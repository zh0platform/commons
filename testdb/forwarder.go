package testdb

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"git.kloudbox.de/commons/bff/internal/db"
)

// Forwarder is a PostgreSQL protocol relay on 127.0.0.1 between bff and a plaintext test database. When armed with a
// command tag, it withholds the answer to the next statement the server completes with that tag (an explicit COMMIT,
// or a single statement whose implicit transaction commits at its Sync) until the server is ready for the next
// query, then closes the client's connection: the work committed, and the client never hears so.
type Forwarder struct {
	db         *Database
	listener   net.Listener
	dropTag    atomic.Pointer[string]
	stallArmed atomic.Bool
	commits    atomic.Int32
	wg         sync.WaitGroup
}

// NewForwarder listens until the test ends. The database must come from NewPlaintext.
func NewForwarder(t testing.TB, db *Database) *Forwarder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &Forwarder{db: db, listener: listener}
	f.wg.Go(f.accept)
	t.Cleanup(func() {
		_ = listener.Close()
		f.wg.Wait()
	})
	return f
}

// Settings connect a login through the forwarder in plaintext.
func (f *Forwarder) Settings(login string) db.Config {
	settings := f.db.Settings(login)
	addr, _ := f.listener.Addr().(*net.TCPAddr)
	settings.Host, settings.Port, settings.TLSMode, settings.CA = "127.0.0.1", addr.Port, db.Disable, nil
	return settings
}

// Environ is the bff environment that reaches this database through the forwarder.
func (f *Forwarder) Environ() []string {
	s := f.Settings(RuntimeLogin)
	return environOf(s.Host, s.Port, s.Name, "", s.Password)
}

// DropNextAnswer arms the forwarder for the next statement any of its connections completes with tag, such as
// "COMMIT" or "INSERT".
func (f *Forwarder) DropNextAnswer(tag string) {
	f.dropTag.Store(&tag)
}

// StallAfterNextQuery arms the forwarder to withhold every answer after the next simple query on any of its
// connections, without closing it: a server that stops answering.
func (f *Forwarder) StallAfterNextQuery() {
	f.stallArmed.Store(true)
}

// Commits counts the COMMIT messages clients sent through the forwarder.
func (f *Forwarder) Commits() int {
	return int(f.commits.Load())
}

func (f *Forwarder) accept() {
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.wg.Go(func() { f.relay(client) })
	}
}

func (f *Forwarder) relay(client net.Conn) {
	defer func() { _ = client.Close() }()
	settings := f.db.Settings(RuntimeLogin)
	server, err := net.Dial("tcp", net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)))
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	var stalled atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.toServer(client, server, &stalled)
	}()
	f.toClient(server, client, &stalled)
	_ = client.Close()
	_ = server.Close()
	<-done
}

// toServer copies frontend messages; the first is the untyped startup message. An armed fault is set before the
// message that triggers it is forwarded, so its answer cannot pass first.
func (f *Forwarder) toServer(client, server net.Conn, stalled *atomic.Bool) {
	if _, err := copyMessage(client, server, false); err != nil {
		return
	}
	for {
		message, err := readMessage(client, true)
		if err != nil {
			return
		}
		if message[0] == 'Q' && strings.EqualFold(strings.TrimRight(string(message[5:]), "\x00 ;"), "commit") {
			f.commits.Add(1)
		}
		if message[0] == 'Q' && f.stallArmed.CompareAndSwap(true, false) {
			stalled.Store(true)
		}
		if _, err := server.Write(message); err != nil {
			return
		}
	}
}

// toClient copies backend messages. From an armed statement's CommandComplete on it withholds everything and closes
// at the server's ReadyForQuery, when the work has committed; while stalled it reads and withholds everything, so the
// client waits on an open connection.
func (f *Forwarder) toClient(server, client net.Conn, stalled *atomic.Bool) {
	dropping := false
	for {
		message, err := readMessage(server, true)
		if err != nil {
			return
		}
		if stalled.Load() {
			continue
		}
		if tag := f.dropTag.Load(); !dropping && tag != nil && message[0] == 'C' && strings.HasPrefix(string(message[5:]), *tag) {
			dropping = f.dropTag.CompareAndSwap(tag, nil)
		}
		if dropping {
			if message[0] == 'Z' {
				return
			}
			continue
		}
		if _, err := client.Write(message); err != nil {
			return
		}
	}
}

func copyMessage(from, to net.Conn, typed bool) ([]byte, error) {
	message, err := readMessage(from, typed)
	if err != nil {
		return nil, err
	}
	_, err = to.Write(message)
	return message, err
}

// readMessage reads one protocol message: an optional type byte, a length that counts itself, and the body.
func readMessage(conn net.Conn, typed bool) ([]byte, error) {
	headerSize := 4
	if typed {
		headerSize = 5
	}
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[headerSize-4:])
	if length < 4 || length > 1<<30 {
		return nil, errors.New("malformed message")
	}
	message := make([]byte, headerSize+int(length)-4)
	copy(message, header)
	if _, err := io.ReadFull(conn, message[headerSize:]); err != nil {
		return nil, err
	}
	return message, nil
}
