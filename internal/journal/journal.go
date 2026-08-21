// Package journal records recent data-plane calls in a bounded in-memory ring.
package journal

import (
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Call is one completed data-plane RPC.
type Call struct {
	Seq        uint64
	Method     string
	Metadata   metadata.MD
	Requests   []*dynamicpb.Message
	Responses  []*dynamicpb.Message
	Err        *status.Status
	StubSource string
	StubID     string
	Start      time.Time
	Duration   time.Duration
}

// Filter selects calls by method and limits results to the newest matches.
type Filter struct {
	Method string
	Limit  int
}

// Journal is a mutex-protected bounded journal.
type Journal struct {
	mu      sync.RWMutex
	entries []*Call
	cap     int
	start   int
	count   int
	total   uint64
	subs    map[*Subscription]struct{} // live Watch subscribers; guarded by mu
}

func New(capacity int) *Journal {
	if capacity < 1 {
		capacity = 1
	}
	return &Journal{entries: make([]*Call, capacity), cap: capacity}
}

// Record assigns the next sequence number and retains an independent snapshot.
// Capacity bounds calls, not decoded message bytes: every request and rendered
// response is deliberately retained in full.
func (j *Journal) Record(call *Call) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.total++
	call.Seq = j.total
	call.Method = normalizeMethod(call.Method)
	retained := cloneCall(call)
	var index int
	if j.count < j.cap {
		index = (j.start + j.count) % j.cap
		j.count++
	} else {
		index = j.start
		j.start = (j.start + 1) % j.cap
	}
	j.entries[index] = retained
	j.broadcastLocked(retained)
}

// List returns independent call snapshots oldest first.
func (j *Journal) List() []*Call {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]*Call, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = cloneCall(j.entries[(j.start+i)%j.cap])
	}
	return out
}

// Filter returns matching calls oldest first, restricted to the newest Limit.
func (j *Journal) Filter(filter Filter) []*Call {
	j.mu.RLock()
	defer j.mu.RUnlock()
	method := normalizeMethod(filter.Method)
	matched := make([]*Call, 0, j.count)
	for i := 0; i < j.count; i++ {
		call := j.entries[(j.start+i)%j.cap]
		if method == "" || call.Method == method {
			matched = append(matched, call)
		}
	}
	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[len(matched)-filter.Limit:]
	}
	out := make([]*Call, len(matched))
	for i, call := range matched {
		out[i] = cloneCall(call)
	}
	return out
}

// Reset clears retained entries without resetting sequence numbers or total.
func (j *Journal) Reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := range j.entries {
		j.entries[i] = nil
	}
	j.start = 0
	j.count = 0
}

// Total returns the number of calls ever recorded.
func (j *Journal) Total() uint64 {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.total
}

func cloneCall(call *Call) *Call {
	cloned := *call
	cloned.Metadata = call.Metadata.Copy()
	cloned.Requests = cloneMessages(call.Requests)
	cloned.Responses = cloneMessages(call.Responses)
	if call.Err != nil {
		cloned.Err = status.FromProto(call.Err.Proto())
	}
	return &cloned
}

func cloneMessages(messages []*dynamicpb.Message) []*dynamicpb.Message {
	if messages == nil {
		return nil
	}
	cloned := make([]*dynamicpb.Message, len(messages))
	for i, message := range messages {
		if message != nil {
			cloned[i] = proto.Clone(message).(*dynamicpb.Message)
		}
	}
	return cloned
}

func normalizeMethod(method string) string {
	if method == "" {
		return ""
	}
	return "/" + strings.TrimPrefix(method, "/")
}

// Len reports how many calls the ring currently retains.
func (j *Journal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.count
}

// Cap reports the ring capacity, fixed at construction.
func (j *Journal) Cap() int { return j.cap }
