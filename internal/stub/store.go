package stub

import (
	"fmt"
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

// Miss describes why one registered stub did not select for a request.
type Miss struct {
	Source   string
	Priority int
	Reasons  []string
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

// Explain ranks the registered stubs that miss an input by ascending number
// of failed clauses. It is diagnostic only and never consumes a times budget.
func (s *Store) Explain(method string, in match.Input) []Miss {
	s.mu.Lock()
	defer s.mu.Unlock()

	var misses []Miss
	for _, e := range s.byMethod[method] {
		reasons := e.stub.Explain(in)
		if e.stub.Times > 0 && e.used >= e.stub.Times {
			reasons = append(reasons, fmt.Sprintf("times budget exhausted (%d/%d used)", e.used, e.stub.Times))
		}
		if len(reasons) == 0 {
			continue
		}
		misses = append(misses, Miss{
			Source:   e.stub.Source,
			Priority: e.stub.Priority,
			Reasons:  reasons,
		})
	}
	sort.SliceStable(misses, func(i, j int) bool {
		return len(misses[i].Reasons) < len(misses[j].Reasons)
	})
	return misses
}

// CountFor reports how many stubs are registered for a method (regardless
// of times budget) — used in "no stub matched" error messages.
func (s *Store) CountFor(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byMethod[method])
}
