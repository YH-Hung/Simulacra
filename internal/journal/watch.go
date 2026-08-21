package journal

import (
	"context"
	"errors"
)

// watchBuffer is each subscriber's channel depth. A subscriber that falls
// this far behind is dropped (design §4).
const watchBuffer = 64

// ErrSlowConsumer reports a subscriber evicted for falling watchBuffer calls
// behind (RESOURCE_EXHAUSTED at the admin surface: reconnect).
var ErrSlowConsumer = errors.New("journal: subscriber fell behind and was dropped")

// Subscription is one live tail of the journal. Calls returns the stream
// channel; once it closes, Err reports why: nil for a clean Close or context
// cancellation, ErrSlowConsumer for an eviction. Delivered calls are the
// journal's own retained snapshots and must be treated as read-only.
type Subscription struct {
	journal *Journal
	method  string
	calls   chan *Call
	done    chan struct{}
	err     error // written under journal.mu before calls closes
}

// Watch subscribes to calls recorded from now on, optionally filtered to one
// method ("" means all; the filter runs before buffering, so unrelated
// traffic cannot evict a filtered subscriber). Canceling ctx closes the
// subscription; so does Close.
func (j *Journal) Watch(ctx context.Context, method string) *Subscription {
	s := &Subscription{
		journal: j,
		method:  normalizeMethod(method),
		calls:   make(chan *Call, watchBuffer),
		done:    make(chan struct{}),
	}
	j.mu.Lock()
	if j.subs == nil {
		j.subs = make(map[*Subscription]struct{})
	}
	j.subs[s] = struct{}{}
	j.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.done:
		}
	}()
	return s
}

// Calls is the stream channel; it closes when the subscription ends.
func (s *Subscription) Calls() <-chan *Call { return s.calls }

// Err reports why Calls closed. Read it only after Calls is closed.
func (s *Subscription) Err() error { return s.err }

// Close ends the subscription. Idempotent; Err stays nil on this path.
func (s *Subscription) Close() {
	s.journal.mu.Lock()
	s.closeLocked(nil)
	s.journal.mu.Unlock()
}

// closeLocked finalizes the subscription; the journal's write lock must be
// held. Membership in journal.subs is the "still open" flag, which makes
// close idempotent and — because every send in Record holds the same lock —
// makes send-on-closed-channel impossible by construction (design §4).
// Closing done here is what terminates the context-bridge goroutine on
// every path, not just cancellation.
func (s *Subscription) closeLocked(err error) {
	if _, open := s.journal.subs[s]; !open {
		return
	}
	delete(s.journal.subs, s)
	s.err = err
	close(s.calls)
	close(s.done)
}

// bridgeDone exposes the internal done channel so tests can assert the
// context-bridge goroutine terminates on Close under a live context.
func (s *Subscription) bridgeDone() <-chan struct{} { return s.done }

// broadcastLocked delivers one retained call to every live subscriber; the
// journal's write lock must be held. Sends are non-blocking: a full buffer
// evicts the subscriber with ErrSlowConsumer. Deleting from the map during
// its own range is allowed in Go.
func (j *Journal) broadcastLocked(retained *Call) {
	for s := range j.subs {
		if s.method != "" && retained.Method != s.method {
			continue
		}
		select {
		case s.calls <- retained:
		default:
			s.closeLocked(ErrSlowConsumer)
		}
	}
}
