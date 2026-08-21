package journal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func recordCall(j *Journal, method string) {
	j.Record(&Call{Method: method})
}

func TestWatchDeliversInSeqOrder(t *testing.T) {
	j := New(16)
	sub := j.Watch(context.Background(), "")
	defer sub.Close()
	for i := 0; i < 5; i++ {
		recordCall(j, "/a.B/C")
	}
	var last uint64
	for i := 0; i < 5; i++ {
		select {
		case call := <-sub.Calls():
			if call.Seq <= last {
				t.Fatalf("out of order: %d after %d", call.Seq, last)
			}
			last = call.Seq
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for call")
		}
	}
}

func TestWatchFiltersByMethodBeforeBuffering(t *testing.T) {
	j := New(16)
	sub := j.Watch(context.Background(), "a.B/Wanted") // unnormalized on purpose
	defer sub.Close()
	// Flood with unrelated traffic far past the buffer size: the filter runs
	// before the buffered send, so this must not evict the subscriber.
	for i := 0; i < watchBuffer*3; i++ {
		recordCall(j, "/a.B/Noise")
	}
	recordCall(j, "/a.B/Wanted")
	select {
	case call := <-sub.Calls():
		if call.Method != "/a.B/Wanted" {
			t.Fatalf("method = %q", call.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber was starved or dropped by unrelated traffic")
	}
	if err := sub.Err(); err != nil {
		t.Fatalf("Err = %v, want nil (never fell behind on its own method)", err)
	}
}

func TestSlowConsumerIsDroppedWithErrSlowConsumer(t *testing.T) {
	j := New(4)
	sub := j.Watch(context.Background(), "")
	for i := 0; i < watchBuffer+1; i++ { // one past the buffer with no reader
		recordCall(j, "/a.B/C")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-sub.Calls():
			if !ok {
				if !errors.Is(sub.Err(), ErrSlowConsumer) {
					t.Fatalf("Err = %v, want ErrSlowConsumer", sub.Err())
				}
				return
			}
		case <-deadline:
			t.Fatal("channel never closed after overflow")
		}
	}
}

func TestCloseEndsStreamWithNilErr(t *testing.T) {
	j := New(4)
	sub := j.Watch(context.Background(), "")
	sub.Close()
	if _, ok := <-sub.Calls(); ok {
		t.Fatal("channel still open after Close")
	}
	if sub.Err() != nil {
		t.Fatalf("Err = %v, want nil after clean Close", sub.Err())
	}
	sub.Close()             // idempotent
	recordCall(j, "/a.B/C") // must not panic on a closed subscription
}

func TestContextCancelEndsStreamWithNilErr(t *testing.T) {
	j := New(4)
	ctx, cancel := context.WithCancel(context.Background())
	sub := j.Watch(ctx, "")
	cancel()
	select {
	case _, ok := <-sub.Calls():
		if ok {
			t.Fatal("got a call, want close")
		}
	case <-time.After(time.Second):
		t.Fatal("channel never closed after ctx cancel")
	}
	if sub.Err() != nil {
		t.Fatalf("Err = %v, want nil", sub.Err())
	}
}

// The context bridge must terminate on explicit Close even when the context
// lives on (design §4: a Background-context tail must not leak a goroutine).
func TestCloseTerminatesContextBridge(t *testing.T) {
	j := New(4)
	sub := j.Watch(context.Background(), "")
	done := make(chan struct{})
	go func() {
		<-sub.bridgeDone() // test-only accessor, see watch.go
		close(done)
	}()
	sub.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge goroutine did not exit after Close under a live context")
	}
}

// Race pair 1: consumer Close against writer-side eviction.
func TestCloseRacesEviction(t *testing.T) {
	for i := 0; i < 100; i++ {
		j := New(4)
		sub := j.Watch(context.Background(), "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 0; k < watchBuffer+8; k++ {
				recordCall(j, "/a.B/C")
			}
		}()
		go func() {
			defer wg.Done()
			sub.Close()
		}()
		wg.Wait()
	}
}

// Race pair 2: context cancellation against a concurrent broadcast — the
// send-versus-close hazard (design §4).
func TestCancelRacesBroadcast(t *testing.T) {
	for i := 0; i < 100; i++ {
		j := New(4)
		ctx, cancel := context.WithCancel(context.Background())
		sub := j.Watch(ctx, "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 0; k < 16; k++ {
				recordCall(j, fmt.Sprintf("/a.B/C%d", k))
			}
		}()
		go func() {
			defer wg.Done()
			cancel()
		}()
		wg.Wait()
		// Drain to close like a real consumer: the stream must terminate,
		// and cancellation must never be reported as a slow-consumer drop.
		for range sub.Calls() {
		}
		if err := sub.Err(); err != nil {
			t.Fatalf("Err = %v, want nil after cancellation", err)
		}
	}
}

func TestLenAndCap(t *testing.T) {
	j := New(3)
	if j.Cap() != 3 || j.Len() != 0 {
		t.Fatalf("Cap=%d Len=%d, want 3, 0", j.Cap(), j.Len())
	}
	for i := 0; i < 5; i++ {
		recordCall(j, "/a.B/C")
	}
	if j.Len() != 3 {
		t.Fatalf("Len = %d after overflow, want 3", j.Len())
	}
	j.Reset()
	if j.Len() != 0 || j.Cap() != 3 {
		t.Fatalf("after Reset: Len=%d Cap=%d, want 0, 3", j.Len(), j.Cap())
	}
}
