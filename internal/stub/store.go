package stub

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yinghanhung/simulacra/internal/match"
)

// Store holds compiled stubs grouped by method and selects the stub for a
// request: highest priority first (load order breaks ties), skipping stubs
// whose `times` budget is spent. Selection consumes one use atomically.
type Store struct {
	mu       sync.Mutex
	byMethod map[string][]*entry
}

type entry struct {
	stub *Compiled
	used int
}

// Miss describes why one registered stub did not select for a request.
type Miss struct {
	Source   string
	Priority int
	Reasons  []string
}

type candidateSnapshot struct {
	stub *Compiled
	used int
}

func NewStore(stubs []*Compiled) *Store {
	s := &Store{byMethod: make(map[string][]*entry)}
	for _, c := range stubs {
		s.byMethod[c.Method] = append(s.byMethod[c.Method], &entry{stub: c})
	}
	for _, entries := range s.byMethod {
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].stub.Priority > entries[j].stub.Priority
		})
	}
	return s
}

// Select returns the first live matching stub for the method, or nil.
func (s *Store) Select(method string, in match.Input) *Compiled {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.byMethod[method] {
		if e.stub.Times > 0 && e.used >= e.stub.Times {
			continue
		}
		if e.stub.Matches(in) {
			e.used++
			return e.stub
		}
	}
	return nil
}

// SelectOrExplain atomically selects a live stub or snapshots the failed
// selection for diagnostics. Matcher explanations run after releasing the
// store lock and use the same timestamp as the selection attempt.
func (s *Store) SelectOrExplain(method string, in match.Input) (*Compiled, []Miss) {
	in = inputWithTime(in)
	s.mu.Lock()
	for _, e := range s.byMethod[method] {
		if e.stub.Times > 0 && e.used >= e.stub.Times {
			continue
		}
		if e.stub.Matches(in) {
			e.used++
			s.mu.Unlock()
			return e.stub, nil
		}
	}
	snapshot := s.snapshotLocked(method)
	s.mu.Unlock()
	return nil, explainSnapshot(snapshot, in)
}

// Explain ranks the registered stubs that miss an input by ascending number
// of failed clauses. It is diagnostic only and never consumes a times budget.
func (s *Store) Explain(method string, in match.Input) []Miss {
	in = inputWithTime(in)
	s.mu.Lock()
	snapshot := s.snapshotLocked(method)
	s.mu.Unlock()
	return explainSnapshot(snapshot, in)
}

func (s *Store) snapshotLocked(method string) []candidateSnapshot {
	entries := s.byMethod[method]
	snapshot := make([]candidateSnapshot, len(entries))
	for i, e := range entries {
		snapshot[i] = candidateSnapshot{stub: e.stub, used: e.used}
	}
	return snapshot
}

func explainSnapshot(snapshot []candidateSnapshot, in match.Input) []Miss {
	var misses []Miss
	for _, candidate := range snapshot {
		reasons := candidate.stub.Explain(in)
		if candidate.stub.Times > 0 && candidate.used >= candidate.stub.Times {
			reasons = append(reasons, fmt.Sprintf("times budget exhausted (%d/%d used)", candidate.used, candidate.stub.Times))
		}
		if len(reasons) == 0 {
			continue
		}
		misses = append(misses, Miss{
			Source:   candidate.stub.Source,
			Priority: candidate.stub.Priority,
			Reasons:  reasons,
		})
	}
	sort.SliceStable(misses, func(i, j int) bool {
		return len(misses[i].Reasons) < len(misses[j].Reasons)
	})
	return misses
}

func inputWithTime(in match.Input) match.Input {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	return in
}

// CountFor reports how many stubs are registered for a method (regardless
// of times budget) — used in "no stub matched" error messages.
func (s *Store) CountFor(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byMethod[method])
}
