package stub

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yinghanhung/simulacra/internal/match"
)

// Store holds compiled stubs grouped by method and selects the stub for a
// request: highest priority first; at equal priority API-origin stubs beat
// file-origin stubs (a test overrides a sandbox default without priority
// arithmetic), and within an origin ingest order breaks ties. Stubs whose
// `times` budget is spent are skipped. Selection consumes one use atomically.
//
// The store owns stub identity (design §3.4): Add and ReplaceOrigin stamp
// Origin — and, for API stubs, ID and Source — on the values they install,
// so no ingest path can create a stub with wrong ownership.
type Store struct {
	mu       sync.Mutex
	byMethod map[string][]*entry
	nextID   uint64 // "api-<n>" assignment; monotonic, never reused
	nextSeq  uint64 // ingest order for same-origin tie-breaking
}

type entry struct {
	stub *Compiled
	used int
	seq  uint64
}

// ErrStubNotFound reports a Remove of an id the store does not hold.
var ErrStubNotFound = errors.New("no stub with this id")

// FileOwnedError reports a Remove of a file-origin stub, which only its
// file can remove (FAILED_PRECONDITION at the admin surface).
type FileOwnedError struct {
	ID     string
	Source string
}

func (e *FileOwnedError) Error() string {
	return fmt.Sprintf("stub %s is owned by %s; edit or remove the file", e.ID, e.Source)
}

// Info is the read-only envelope for one registered stub (admin Stub).
// Hits is the stub's consumed times budget: it resets when the stub's own
// origin is replaced or reset — it is not a lifetime total.
type Info struct {
	ID       string
	Method   string
	Shape    match.Shape
	Priority int
	Times    int
	Origin   Origin
	Source   string
	Hits     int
	Document string
}

// ListFilter narrows List output; the zero value lists everything.
type ListFilter struct {
	Method string  // normalized "/pkg.Service/Method"; "" means all
	Origin *Origin // nil means all origins
}

// Miss describes why one registered stub did not select for a request.
type Miss struct {
	Source   string
	Priority int
	Reasons  []string
}

// Selection is the result of a selection attempt. On a failed selection,
// Misses and RegisteredCount are captured from the same locked store state.
type Selection struct {
	Selected        *Compiled
	Misses          []Miss
	RegisteredCount int
}

type candidateSnapshot struct {
	stub *Compiled
	used int
}

// NewStore returns an empty store. Population goes through Add and
// ReplaceOrigin only, so every ingest is validated and stamped.
func NewStore() *Store {
	return &Store{byMethod: make(map[string][]*entry)}
}

func originRank(o Origin) int {
	if o == OriginAPI {
		return 0
	}
	return 1
}

func sortEntries(entries []*entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.stub.Priority != b.stub.Priority {
			return a.stub.Priority > b.stub.Priority
		}
		if ra, rb := originRank(a.stub.Origin), originRank(b.stub.Origin); ra != rb {
			return ra < rb
		}
		return a.seq < b.seq
	})
}

// Add installs one API-origin stub, stamping Origin, Source, and a
// store-assigned id on c, and returns the id.
func (s *Store) Add(c *Compiled) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	c.ID = fmt.Sprintf("api-%d", s.nextID)
	c.Origin = OriginAPI
	c.Source = "api"
	s.nextSeq++
	entries := append(s.byMethod[c.Method], &entry{stub: c, seq: s.nextSeq})
	sortEntries(entries)
	s.byMethod[c.Method] = entries
	return c.ID
}

// Remove deletes an API-origin stub by id.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, entries := range s.byMethod {
		for i, e := range entries {
			if e.stub.ID != id {
				continue
			}
			if e.stub.Origin != OriginAPI {
				return &FileOwnedError{ID: id, Source: e.stub.Source}
			}
			entries = append(entries[:i], entries[i+1:]...)
			if len(entries) == 0 {
				delete(s.byMethod, m)
			} else {
				s.byMethod[m] = entries
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrStubNotFound, id)
}

// ReplaceOrigin atomically replaces every stub of one origin, stamping
// origin on each installed stub and assigning ids to stubs that carry none
// (API documents; file stubs arrive with ID = Source). Entries of other
// origins — and their consumed times budgets — are untouched, so a file
// reload cannot replenish API budgets or vice versa. On a duplicate id the
// store is left unchanged and the error names the id. Returns installed ids
// in input order.
func (s *Store) ReplaceOrigin(origin Origin, stubs []*Compiled) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := make(map[string]string)
	for _, entries := range s.byMethod {
		for _, e := range entries {
			if e.stub.Origin != origin {
				taken[e.stub.ID] = e.stub.Source
			}
		}
	}
	ids := make([]string, 0, len(stubs))
	for _, c := range stubs {
		c.Origin = origin
		if c.ID == "" {
			s.nextID++
			c.ID = fmt.Sprintf("api-%d", s.nextID)
			if origin == OriginAPI {
				c.Source = "api"
			}
		}
		if owner, dup := taken[c.ID]; dup {
			return nil, fmt.Errorf("duplicate stub id %q (already used by %s)", c.ID, owner)
		}
		taken[c.ID] = c.Source
		ids = append(ids, c.ID)
	}
	byMethod := make(map[string][]*entry)
	for m, entries := range s.byMethod {
		for _, e := range entries {
			if e.stub.Origin != origin {
				byMethod[m] = append(byMethod[m], e)
			}
		}
	}
	for _, c := range stubs {
		s.nextSeq++
		byMethod[c.Method] = append(byMethod[c.Method], &entry{stub: c, seq: s.nextSeq})
	}
	for m := range byMethod {
		sortEntries(byMethod[m])
	}
	s.byMethod = byMethod
	return ids, nil
}

// List returns envelopes in deterministic order: methods sorted, entries in
// selection order within a method.
func (s *Store) List(f ListFilter) []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	methods := make([]string, 0, len(s.byMethod))
	for m := range s.byMethod {
		if f.Method == "" || m == f.Method {
			methods = append(methods, m)
		}
	}
	sort.Strings(methods)
	var out []Info
	for _, m := range methods {
		for _, e := range s.byMethod[m] {
			if f.Origin != nil && e.stub.Origin != *f.Origin {
				continue
			}
			out = append(out, Info{
				ID:       e.stub.ID,
				Method:   e.stub.Method,
				Shape:    e.stub.Shape,
				Priority: e.stub.Priority,
				Times:    e.stub.Times,
				Origin:   e.stub.Origin,
				Source:   e.stub.Source,
				Hits:     e.used,
				Document: e.stub.Document,
			})
		}
	}
	return out
}

// ResetStubs drops every API-origin stub and restores file-origin times
// budgets in one atomic step (ControlService.Reset stub semantics).
func (s *Store) ResetStubs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, entries := range s.byMethod {
		kept := entries[:0]
		for _, e := range entries {
			if e.stub.Origin == OriginAPI {
				continue
			}
			e.used = 0
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(s.byMethod, m)
		} else {
			s.byMethod[m] = kept
		}
	}
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
func (s *Store) SelectOrExplain(method string, in match.Input) Selection {
	in = inputWithTime(in)
	s.mu.Lock()
	for _, e := range s.byMethod[method] {
		if e.stub.Times > 0 && e.used >= e.stub.Times {
			continue
		}
		if e.stub.Matches(in) {
			e.used++
			s.mu.Unlock()
			return Selection{Selected: e.stub}
		}
	}
	snapshot := s.snapshotLocked(method)
	s.mu.Unlock()
	return Selection{
		Misses:          explainSnapshot(snapshot, in),
		RegisteredCount: len(snapshot),
	}
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

// Len reports how many stubs are registered across all methods, regardless of
// times budget. It reads the same synchronized state Select does, so it never
// disagrees with the generation of stubs currently serving.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, entries := range s.byMethod {
		total += len(entries)
	}
	return total
}

// CountFor reports how many stubs are registered for a method, regardless of
// times budget.
func (s *Store) CountFor(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byMethod[method])
}
