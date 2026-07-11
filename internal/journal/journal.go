// Package journal records recent data-plane calls in a bounded in-memory ring.
package journal

import (
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
	mu      sync.Mutex
	entries []*Call
	cap     int
	total   uint64
}

func New(capacity int) *Journal {
	if capacity < 1 {
		capacity = 1
	}
	return &Journal{cap: capacity}
}

// Record assigns the next sequence number and retains the call.
func (j *Journal) Record(call *Call) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.total++
	call.Seq = j.total
	call.Method = normalizeMethod(call.Method)
	if len(j.entries) == j.cap {
		copy(j.entries, j.entries[1:])
		j.entries[len(j.entries)-1] = call
		return
	}
	j.entries = append(j.entries, call)
}

// List returns retained calls oldest first.
func (j *Journal) List() []*Call {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]*Call(nil), j.entries...)
}

// Filter returns matching calls oldest first, restricted to the newest Limit.
func (j *Journal) Filter(filter Filter) []*Call {
	j.mu.Lock()
	defer j.mu.Unlock()
	method := normalizeMethod(filter.Method)
	matched := make([]*Call, 0, len(j.entries))
	for _, call := range j.entries {
		if method == "" || call.Method == method {
			matched = append(matched, call)
		}
	}
	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[len(matched)-filter.Limit:]
	}
	return append([]*Call(nil), matched...)
}

// Reset clears retained entries without resetting sequence numbers or total.
func (j *Journal) Reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = nil
}

// Total returns the number of calls ever recorded.
func (j *Journal) Total() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.total
}

func normalizeMethod(method string) string {
	if method == "" {
		return ""
	}
	return "/" + strings.TrimPrefix(method, "/")
}
