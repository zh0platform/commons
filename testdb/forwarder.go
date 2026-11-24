package testdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// maxMessage bounds the length a protocol message may declare, so a peer on the loopback port cannot make the relay
// allocate up to the protocol's 1 GiB. The largest message the lost-commit tests send is a migration of under 5 KiB
// (measured 2026-11-23 on the products' migrations); 1 MiB leaves room for any test's statements and rows.
const maxMessage = 1 << 20

// Forwarder is a PostgreSQL protocol relay on 127.0.0.1 between a client and a plaintext test database. When armed with a
// command tag, it withholds the answer to the next statement the server completes with that tag (an explicit COMMIT,
// or a single statement whose implicit transaction commits at its Sync) until the server is ready for the next
// query, then closes the client's connection: the work committed, and the client never hears so.
type Forwarder struct {
	db         *Database
	listener   net.Listener
	dropMu     sync.Mutex
	drop       dropArm // guarded by dropMu
	stallArmed atomic.Bool
	commits    atomic.Int32
	wg         sync.WaitGroup
	mu         sync.Mutex
	open       map[net.Conn]struct{} // both sockets of every relayed connection, closed by the test's cleanup
	closed     bool                  // the test's cleanup began: a connection accepted now is closed at once
}

// NewForwarder listens until the test ends, when it closes every connection it accepted and waits for its relays to
// end. The relay speaks plaintext, so it refuses a database that did not come from NewPlaintext.
func NewForwarder(t testing.TB, db *Database) (*Forwarder, error) {
	t.Helper()
	if !strings.HasPrefix(db.Name, PlaintextPrefix) {
		return nil, fmt.Errorf("relay to %s refused: only a database from NewPlaintext is reachable without TLS", db.Name)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Forwarder{db: db, listener: listener, open: map[net.Conn]struct{}{}}
	f.wg.Go(f.accept)
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		f.closed = true
		for conn := range f.open {
			_ = conn.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	return f, nil
}

// Settings connect a login through the forwarder in plaintext.
func (f *Forwarder) Settings(login string) Settings {
	settings := f.db.Settings(login)
	addr, _ := f.listener.Addr().(*net.TCPAddr)
	settings.Host, settings.Port, settings.CA = "127.0.0.1", addr.Port, ""
	return settings
}

// dropArm is what the forwarder is armed to withhold: the completion after skip more with a tag starting with tag.
type dropArm struct {
	armed bool
	tag   string
	skip  int
}

// DropNextAnswer arms the forwarder for the next statement any of its connections completes with tag, such as
// "COMMIT" or "INSERT".
func (f *Forwarder) DropNextAnswer(tag string) {
	f.DropAnswerAfter(tag, 0)
}

// DropAnswerAfter arms the forwarder like DropNextAnswer, but lets the next skip such completions pass first, counted
// over all of its connections. Arming again replaces an earlier arm.
func (f *Forwarder) DropAnswerAfter(tag string, skip int) {
	f.dropMu.Lock()
	defer f.dropMu.Unlock()
	f.drop = dropArm{armed: true, tag: tag, skip: skip}
}

// takeDrop reports whether a completion with this command tag is the armed one, which disarms the forwarder; a
// skipped completion counts down the arm.
func (f *Forwarder) takeDrop(completed string) bool {
	f.dropMu.Lock()
	defer f.dropMu.Unlock()
	if !f.drop.armed || !strings.HasPrefix(completed, f.drop.tag) {
		return false
	}
	if f.drop.skip > 0 {
		f.drop.skip--
		return false
	}
	f.drop.armed = false
	return true
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
		f.mu.Lock()
		if f.closed {
			_ = client.Close()
		} else {
			f.open[client] = struct{}{}
			f.wg.Go(func() { f.relay(client) })
		}
		f.mu.Unlock()
	}
}

// relay copies one client's connection both ways; when either direction ends, for any reason, it closes both sockets
// so the other direction ends too.
func (f *Forwarder) relay(client net.Conn) {
	defer func() {
		_ = client.Close()
		f.mu.Lock()
		delete(f.open, client)
		f.mu.Unlock()
	}()
	server, err := net.Dial("tcp", net.JoinHostPort(f.db.server.Host, f.db.server.Port))
	if err != nil {
		return
	}
	// The server socket is closed by the cleanup too: a relay blocked writing to a server that stopped reading ends
	// only when that socket closes.
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = server.Close()
		return
	}
	f.open[server] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.open, server)
		f.mu.Unlock()
	}()
	closeBoth := func() {
		_ = client.Close()
		_ = server.Close()
	}
	var stalled atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.toServer(client, server, &stalled)
		closeBoth()
	}()
	f.toClient(server, client, &stalled)
	closeBoth()
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
		if !dropping && message[0] == 'C' && f.takeDrop(string(message[5:])) {
			dropping = true
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
	if length < 4 || length > maxMessage {
		return nil, errors.New("malformed message")
	}
	message := make([]byte, headerSize+int(length)-4)
	copy(message, header)
	if _, err := io.ReadFull(conn, message[headerSize:]); err != nil {
		return nil, err
	}
	return message, nil
}
