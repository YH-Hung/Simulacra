package stub

import (
	"sort"
	"sync"

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

// CountFor reports how many stubs are registered for a method (regardless
// of times budget) — used in "no stub matched" error messages.
func (s *Store) CountFor(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byMethod[method])
}
