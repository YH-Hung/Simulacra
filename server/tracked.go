package server

import (
	"net"
	"sync"
)

// trackedListener wraps a listener so teardown knows which connections are
// still open.
//
// http.Server.Shutdown cannot supply that. For the hijacked connections a
// native gRPC client creates over h2c it returns immediately and waits for
// nothing; for plain HTTP/1.1 it blocks until the request finishes. Neither
// gives a drain signal and neither closes anything, so the server tracks
// connections itself and force-closes what is left when the budget runs out.
type trackedListener struct {
	net.Listener

	mu       sync.Mutex
	conns    map[*trackedConn]struct{}
	draining bool
	idle     chan struct{}
}

func newTrackedListener(inner net.Listener) *trackedListener {
	return &trackedListener{
		Listener: inner,
		conns:    make(map[*trackedConn]struct{}),
		idle:     make(chan struct{}),
	}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, lis: l}
	l.mu.Lock()
	l.conns[tracked] = struct{}{}
	l.mu.Unlock()
	return tracked, nil
}

// drain marks the listener as draining and returns a channel closed once no
// tracked connection remains — immediately, if none remain already. Calling it
// more than once returns the same channel.
func (l *trackedListener) drain() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.draining = true
	if len(l.conns) == 0 {
		l.closeIdleLocked()
	}
	return l.idle
}

// closeAll force-closes every tracked connection. It closes the wrapper, not
// the connection underneath it, because only the wrapper's Close untracks:
// closing the inner net.Conn directly leaves the drain waiting forever on an
// entry that will never be removed.
func (l *trackedListener) closeAll() {
	l.mu.Lock()
	conns := make([]*trackedConn, 0, len(l.conns))
	for conn := range l.conns {
		conns = append(conns, conn)
	}
	l.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (l *trackedListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

func (l *trackedListener) remove(conn *trackedConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, conn)
	if l.draining && len(l.conns) == 0 {
		l.closeIdleLocked()
	}
}

// closeIdleLocked closes idle at most once. The caller holds mu, which is what
// makes the once-ness safe without a sync.Once.
func (l *trackedListener) closeIdleLocked() {
	select {
	case <-l.idle:
	default:
		close(l.idle)
	}
}

type trackedConn struct {
	net.Conn

	lis  *trackedListener
	once sync.Once
}

// Close untracks the connection exactly once, however many times it is called
// and whoever calls it — the server's own close, or teardown's force-close.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.lis.remove(c) })
	return err
}
