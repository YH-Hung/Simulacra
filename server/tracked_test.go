package server

import (
	"net"
	"testing"
	"time"
)

// acceptedConn dials lis and returns both ends once the listener has wrapped
// the server side, so a test never races the accept.
func acceptedConn(t *testing.T, lis *trackedListener) (client net.Conn, server net.Conn) {
	t.Helper()
	type accepted struct {
		conn net.Conn
		err  error
	}
	done := make(chan accepted, 1)
	go func() {
		c, err := lis.Accept()
		done <- accepted{c, err}
	}()
	client, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatalf("Accept: %v", a.err)
		}
		return client, a.conn
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not return")
		return nil, nil
	}
}

func newTestTrackedListener(t *testing.T) *trackedListener {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	lis := newTrackedListener(inner)
	t.Cleanup(func() { _ = lis.Close() })
	return lis
}

func TestTrackedListenerCountsAcceptedConnections(t *testing.T) {
	lis := newTestTrackedListener(t)
	if got := lis.count(); got != 0 {
		t.Fatalf("count before accept = %d, want 0", got)
	}
	_, server := acceptedConn(t, lis)
	if got := lis.count(); got != 1 {
		t.Fatalf("count after accept = %d, want 1", got)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := lis.count(); got != 0 {
		t.Fatalf("count after close = %d, want 0", got)
	}
}

func TestTrackedListenerDrainClosesWhenLastConnectionCloses(t *testing.T) {
	lis := newTestTrackedListener(t)
	_, server := acceptedConn(t, lis)
	idle := lis.drain()
	select {
	case <-idle:
		t.Fatal("drain reported idle while a connection was still open")
	case <-time.After(50 * time.Millisecond):
	}
	_ = server.Close()
	select {
	case <-idle:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not report idle after the last connection closed")
	}
}

func TestTrackedListenerDrainClosesImmediatelyWhenAlreadyIdle(t *testing.T) {
	lis := newTestTrackedListener(t)
	select {
	case <-lis.drain():
	case <-time.After(time.Second):
		t.Fatal("drain on an idle listener did not report idle")
	}
}

// closeAll must untrack what it closes. Closing the inner net.Conn instead of
// the wrapper leaves the entry in the map forever, and every drain after that
// waits out the full budget for a connection that is already gone.
func TestTrackedListenerCloseAllUntracksConnections(t *testing.T) {
	lis := newTestTrackedListener(t)
	acceptedConn(t, lis)
	acceptedConn(t, lis)
	if got := lis.count(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	idle := lis.drain()
	lis.closeAll()
	if got := lis.count(); got != 0 {
		t.Fatalf("count after closeAll = %d, want 0", got)
	}
	select {
	case <-idle:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not report idle after closeAll")
	}
}

// A connection closed twice — once by the server, once by closeAll — must not
// double-count its removal or panic.
func TestTrackedConnCloseIsIdempotent(t *testing.T) {
	lis := newTestTrackedListener(t)
	_, server := acceptedConn(t, lis)
	_ = server.Close()
	_ = server.Close()
	lis.closeAll()
	if got := lis.count(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}
