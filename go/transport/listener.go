package transport

// tailcat's Server hands each accepted tunnel connection to a callback;
// net/http wants to pull connections from a Listener. memListener is the
// adapter: deliver feeds the callback side, Accept feeds http.Server.Serve.
//
// Ownership: netstack calls the OnTCP handler and never touches the conn
// again, so once deliver enqueues a conn it belongs to http.Server, which
// closes it. deliver must therefore return immediately: parking its
// goroutine would leak one per connection for the life of the server.

import (
	"net"
	"sync"
)

type memListener struct {
	mu     sync.Mutex // guards closed and every send into conns
	closed bool
	conns  chan net.Conn
	done   chan struct{}
	port   uint16
}

func newMemListener(port uint16) *memListener {
	return &memListener{
		conns: make(chan net.Conn, 16),
		done:  make(chan struct{}),
		port:  port,
	}
}

// deliver is the OnTCP handler. The enqueue happens under the mutex so a
// concurrent Close can never strand a conn in the buffer unclosed. If the
// buffer is full — 16 dials between two Accepts of a live http.Server —
// the conn is closed rather than blocking tailcat's handler goroutine;
// backpressure by refusal, not by tying up the tunnel's stack.
func (l *memListener) deliver(c net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		c.Close()
		return
	}
	select {
	case l.conns <- c:
	default:
		c.Close()
	}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.done)
	// Drain whatever was queued but never accepted; new arrivals see closed
	// above. Both paths hold the mutex, so this drain is exhaustive.
	for {
		select {
		case c := <-l.conns:
			c.Close()
		default:
			return nil
		}
	}
}

func (l *memListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv6unspecified, Port: int(l.port)}
}
