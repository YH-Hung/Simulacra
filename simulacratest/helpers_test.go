package simulacratest_test

import (
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// recordingTB stands in for the *testing.T an SDK helper is given, so a test
// can assert on what the helper reported instead of failing itself. It
// overrides exactly the three methods the SDK reports through. Fatalf must
// stop the goroutine like the real one, or a helper would run on past its own
// failure.
type recordingTB struct {
	testing.TB

	mu     sync.Mutex
	errors []string
	fatal  string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fatal = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

// Errors returns every Errorf message, in order.
func (r *recordingTB) Errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

// FatalMsg returns the Fatalf message, or "" if the helper never failed
// fatally. It cannot be named Fatal: testing.TB already has Fatal(...any), and
// a different signature would stop recordingTB from being a testing.TB.
func (r *recordingTB) FatalMsg() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatal
}

// runRecorded runs f against a recordingTB on its own goroutine, because
// Fatalf ends the goroutine it is called on. The deferred close runs on
// Goexit too, so the join sees a fatal exit as well as a normal return.
func runRecorded(t *testing.T, f func(tb testing.TB)) *recordingTB {
	t.Helper()
	rec := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(rec)
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatalf("runRecorded: f did not return within 10s")
	}
	return rec
}

// hangingListener returns the address of a listener that accepts connections
// and never responds, so an RPC stays in flight until its deadline.
func hangingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	return ln.Addr().String()
}

// closeCountingListener tracks how many accepted connections are still open
// and signals on each close, so a test can wait for the client side to let go
// without sleeping.
type closeCountingListener struct {
	net.Listener
	closed chan struct{}

	mu   sync.Mutex
	open int
}

func newCloseCountingListener(ln net.Listener) *closeCountingListener {
	// One slot suffices: a signal that finds the slot full leaves one pending,
	// and waitAllClosed re-reads the count after every wakeup.
	return &closeCountingListener{Listener: ln, closed: make(chan struct{}, 1)}
}

func (l *closeCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.open++
	l.mu.Unlock()
	return &countedConn{Conn: c, lis: l}, nil
}

type countedConn struct {
	net.Conn
	lis  *closeCountingListener
	once sync.Once
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.lis.mu.Lock()
		c.lis.open--
		c.lis.mu.Unlock()
		select {
		case c.lis.closed <- struct{}{}:
		default:
		}
	})
	return err
}

// waitAllClosed fails the test if an accepted connection is still open after
// within.
func (l *closeCountingListener) waitAllClosed(t *testing.T, within time.Duration) {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		l.mu.Lock()
		n := l.open
		l.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-l.closed:
		case <-timer.C:
			t.Fatalf("%d accepted connection(s) still open after %v", n, within)
		}
	}
}
