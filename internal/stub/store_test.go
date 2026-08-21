package stub

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

const method = "/shop.v1.OrderService/GetOrder"

func compiled(t *testing.T, reg *schema.Registry, s Stub) *Compiled {
	t.Helper()
	c, err := Compile(reg, s, "test")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func request(t *testing.T, reg *schema.Registry, jsonBody string) *dynamicpb.Message {
	t.Helper()
	m, err := reg.LookupMethod(method)
	if err != nil {
		t.Fatal(err)
	}
	msg := dynamicpb.NewMessage(m.Input())
	if err := protojson.Unmarshal([]byte(jsonBody), msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestSelectPriorityAndTimes(t *testing.T) {
	reg := testRegistry(t)
	specific := compiled(t, reg, Stub{
		Method:   "shop.v1.OrderService/GetOrder",
		Match:    &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "o-123"}}},
		Priority: 10,
		Times:    1,
		Respond:  Respond{Message: map[string]any{"note": "specific"}},
	})
	fallback := compiled(t, reg, Stub{
		Method:  "shop.v1.OrderService/GetOrder",
		Respond: Respond{Message: map[string]any{"note": "fallback"}},
	})
	store := storeWith(t, fallback, specific) // load order: fallback first

	req := request(t, reg, `{"order_id":"o-123"}`)

	// Higher priority wins even though it was loaded second.
	if got := store.Select(method, match.Input{Message: req.ProtoReflect()}); got != specific {
		t.Fatalf("first select = %v, want the priority-10 stub", got)
	}
	// times: 1 is now exhausted; fallback matches next.
	if got := store.Select(method, match.Input{Message: req.ProtoReflect()}); got != fallback {
		t.Fatalf("second select = %v, want the fallback stub", got)
	}
	// No stubs for other methods.
	if got := store.Select("/shop.v1.OrderService/Other", match.Input{Message: req.ProtoReflect()}); got != nil {
		t.Fatalf("select for unknown method = %v, want nil", got)
	}
	if n := store.CountFor(method); n != 2 {
		t.Errorf("CountFor = %d, want 2", n)
	}
}

func TestSelectNoMatch(t *testing.T) {
	reg := testRegistry(t)
	only := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Match:  &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "o-999"}}},
	})
	store := storeWith(t, only)
	req := request(t, reg, `{"order_id":"o-123"}`)
	if got := store.Select(method, match.Input{Message: req.ProtoReflect()}); got != nil {
		t.Fatalf("Select = %v, want nil for non-matching request", got)
	}
}

func TestReplaceAtomicallyResetsTimesBudget(t *testing.T) {
	reg := testRegistry(t)
	old := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Times:  1,
	})
	fresh := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Times:  1,
	})
	store := storeWith(t, old)

	if got := store.Select(method, match.Input{}); got != old {
		t.Fatalf("first Select = %v, want old stub", got)
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("second Select = %v, want exhausted budget", got)
	}

	if _, err := store.ReplaceOrigin(OriginFile, []*Compiled{fresh}); err != nil {
		t.Fatal(err)
	}
	if got := store.Select(method, match.Input{}); got != fresh {
		t.Fatalf("Select after ReplaceOrigin = %v, want fresh stub", got)
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("second Select after ReplaceOrigin = %v, want fresh budget exhausted", got)
	}
}

func TestLenCountsEveryMethodAndFollowsReplace(t *testing.T) {
	reg := testRegistry(t)
	order := compiled(t, reg, Stub{Method: "shop.v1.OrderService/GetOrder", Times: 1})
	list := compiled(t, reg, Stub{Method: "shop.v1.OrderService/UploadOrders"})
	store := storeWith(t, order, list)

	if got := store.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2 across both methods", got)
	}
	// A spent times budget still counts as registered.
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("Select = nil, want the limited stub")
	}
	if got := store.Len(); got != 2 {
		t.Fatalf("Len after consuming a budget = %d, want 2", got)
	}

	if _, err := store.ReplaceOrigin(OriginFile, []*Compiled{list}); err != nil {
		t.Fatal(err)
	}
	if got := store.Len(); got != 1 {
		t.Fatalf("Len after ReplaceOrigin = %d, want 1", got)
	}
	if got := NewStore().Len(); got != 0 {
		t.Fatalf("Len of empty store = %d, want 0", got)
	}
}

func TestExplainRanksNearestMiss(t *testing.T) {
	reg := testRegistry(t)
	twoWrong := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Match: &match.Block{
			Metadata: map[string]match.Rules{"x-tenant": {"eq": "acme"}},
			Message:  map[string]match.Rules{"order_id": {"eq": "o-999"}},
		},
		Priority: 20,
	})
	twoWrong.Source = "two-wrong.yaml#0"
	oneWrong := compiled(t, reg, Stub{
		Method:   "shop.v1.OrderService/GetOrder",
		Match:    &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "o-777"}}},
		Priority: 10,
	})
	oneWrong.Source = "one-wrong.yaml#0"
	spent := compiled(t, reg, Stub{
		Method:   "shop.v1.OrderService/GetOrder",
		Priority: 30,
		Times:    1,
	})
	spent.Source = "spent.yaml#0"

	store := storeWith(t, twoWrong, oneWrong, spent)
	in := match.Input{Message: request(t, reg, `{"order_id":"o-123"}`).ProtoReflect()}
	if got := store.Select(method, in); got != spent {
		t.Fatalf("Select = %v, want catch-all stub", got)
	}

	misses := store.Explain(method, in)
	if len(misses) != 3 {
		t.Fatalf("Explain returned %d misses, want 3: %#v", len(misses), misses)
	}
	for i := 1; i < len(misses); i++ {
		if len(misses[i-1].Reasons) > len(misses[i].Reasons) {
			t.Errorf("misses are not sorted by reason count: %#v", misses)
		}
	}
	if misses[2].Source != twoWrong.Source || len(misses[2].Reasons) != 2 {
		t.Errorf("last miss = %#v, want the two-reason candidate", misses[2])
	}
	var sawSpent, sawOneWrong bool
	for _, miss := range misses {
		switch miss.Source {
		case spent.Source:
			sawSpent = len(miss.Reasons) == 1 && strings.Contains(miss.Reasons[0], "times budget exhausted")
		case oneWrong.Source:
			sawOneWrong = len(miss.Reasons) == 1 && strings.Contains(miss.Reasons[0], "order_id")
		}
	}
	if !sawSpent || !sawOneWrong {
		t.Errorf("Explain misses = %#v, want exhausted and one-rule misses", misses)
	}
}

func TestExplainStableTiesAndDoesNotConsumeBudget(t *testing.T) {
	reg := testRegistry(t)
	makeMiss := func(source, orderID string, priority int) *Compiled {
		stub := compiled(t, reg, Stub{
			Method:   "shop.v1.OrderService/GetOrder",
			Match:    &match.Block{Message: map[string]match.Rules{"order_id": {"eq": orderID}}},
			Priority: priority,
		})
		stub.Source = source
		return stub
	}
	first := makeMiss("first", "o-first", 10)
	high := makeMiss("high", "o-high", 20)
	second := makeMiss("second", "o-second", 10)
	store := storeWith(t, first, high, second)
	in := match.Input{Message: request(t, reg, `{"order_id":"actual"}`).ProtoReflect()}

	misses := store.Explain(method, in)
	gotSources := make([]string, len(misses))
	for i, miss := range misses {
		gotSources[i] = miss.Source
	}
	if want := []string{"high", "first", "second"}; !reflect.DeepEqual(gotSources, want) {
		t.Fatalf("equal-distance miss order = %v, want %v", gotSources, want)
	}

	limited := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Match:  &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "actual"}}},
		Times:  1,
	})
	budgetStore := storeWith(t, limited)
	if reasons := budgetStore.Explain(method, in); reasons != nil {
		t.Fatalf("Explain for a live match = %#v, want nil", reasons)
	}
	if got := budgetStore.Select(method, in); got != limited {
		t.Fatalf("Select after Explain = %v, want the unconsumed limited stub", got)
	}
	if got := budgetStore.Select(method, in); got != nil {
		t.Fatalf("second Select = %v, want exhausted budget", got)
	}
}

func TestSelectOrExplainSnapshotsConcurrentLastBudget(t *testing.T) {
	reg := testRegistry(t)
	limited := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Times:  1,
	})
	limited.Source = "limited"
	store := storeWith(t, limited)

	type result struct {
		selection Selection
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- result{selection: store.SelectOrExplain(method, match.Input{})}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	selectedCount := 0
	missCount := 0
	for got := range results {
		if got.selection.Selected != nil {
			selectedCount++
			if got.selection.Selected != limited || got.selection.Misses != nil {
				t.Errorf("selected result = %#v, want limited stub and no misses", got)
			}
			continue
		}
		missCount++
		want := []Miss{{
			Source:   "limited",
			Priority: 0,
			Reasons:  []string{"times budget exhausted (1/1 used)"},
		}}
		if !reflect.DeepEqual(got.selection.Misses, want) {
			t.Errorf("losing result misses = %#v, want %#v", got.selection.Misses, want)
		}
	}
	if selectedCount != 1 || missCount != 1 {
		t.Fatalf("selected results = %d, miss results = %d; want one of each", selectedCount, missCount)
	}
}

func TestSelectOrExplainSnapshotsRegisteredCount(t *testing.T) {
	reg := testRegistry(t)
	first := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Match:  &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "first"}}},
	})
	second := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Match:  &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "second"}}},
	})
	store := storeWith(t, first, second)
	in := match.Input{Message: request(t, reg, `{"order_id":"actual"}`).ProtoReflect()}

	result := store.SelectOrExplain(method, in)
	if result.Selected != nil {
		t.Fatalf("SelectOrExplain selected %v, want no match", result.Selected)
	}
	if result.RegisteredCount != 2 {
		t.Fatalf("SelectOrExplain registered count = %d, want 2", result.RegisteredCount)
	}
	if len(result.Misses) != 2 {
		t.Fatalf("SelectOrExplain misses = %#v, want two misses", result.Misses)
	}
}

// storeWith builds a store the way server.Start now does: empty, then one
// validated file-origin ingest.
func storeWith(t *testing.T, stubs ...*Compiled) *Store {
	t.Helper()
	s := NewStore()
	if _, err := s.ReplaceOrigin(OriginFile, stubs); err != nil {
		t.Fatal(err)
	}
	return s
}

// plainStub compiles a matcher-less GetOrder stub, which matches any input.
func plainStub(t *testing.T, reg *schema.Registry) *Compiled {
	t.Helper()
	return compiled(t, reg, Stub{Method: "shop.v1.OrderService/GetOrder"})
}

func TestAddStampsAPIOwnership(t *testing.T) {
	reg := testRegistry(t)
	c := plainStub(t, reg)
	c.Origin = OriginFile // a wrong pre-set value must be overwritten
	c.Source = "left-over"
	s := NewStore()
	id := s.Add(c)
	if id != "api-1" {
		t.Fatalf("id = %q, want api-1", id)
	}
	if c.Origin != OriginAPI || c.Source != "api" || c.ID != "api-1" {
		t.Fatalf("Add did not stamp ownership: ID=%q Origin=%v Source=%q", c.ID, c.Origin, c.Source)
	}
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].Origin != OriginAPI || infos[0].ID != "api-1" {
		t.Fatalf("List = %+v", infos)
	}
}

func TestAPIBeatsFileAtEqualPriority(t *testing.T) {
	reg := testRegistry(t)
	file := plainStub(t, reg)
	api := plainStub(t, reg) // same priority
	s := storeWith(t, file)
	s.Add(api)
	if got := s.Select(method, match.Input{}); got != api {
		t.Fatalf("Select = %v, want the API stub at equal priority", got)
	}
}

func TestHigherPriorityFileBeatsLowerPriorityAPI(t *testing.T) {
	reg := testRegistry(t)
	file := plainStub(t, reg)
	file.Priority = 10
	api := plainStub(t, reg)
	s := storeWith(t, file)
	s.Add(api)
	if got := s.Select(method, match.Input{}); got != file {
		t.Fatalf("Select = %v, want the priority-10 file stub", got)
	}
}

func TestRemoveRefusesFileOriginAndUnknownIDs(t *testing.T) {
	reg := testRegistry(t)
	file := plainStub(t, reg)
	file.ID, file.Source = "stubs/a.yaml#0", "stubs/a.yaml#0"
	s := storeWith(t, file)
	err := s.Remove("stubs/a.yaml#0")
	var owned *FileOwnedError
	if !errors.As(err, &owned) {
		t.Fatalf("Remove(file-origin) = %v, want *FileOwnedError", err)
	}
	if owned.Source != "stubs/a.yaml#0" {
		t.Fatalf("owned.Source = %q", owned.Source)
	}
	if err := s.Remove("nope"); !errors.Is(err, ErrStubNotFound) {
		t.Fatalf("Remove(unknown) = %v, want ErrStubNotFound", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d after refused removes, want 1", s.Len())
	}
}

func TestRemoveDeletesAPIStub(t *testing.T) {
	reg := testRegistry(t)
	s := NewStore()
	id := s.Add(plainStub(t, reg))
	if err := s.Remove(id); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestReplaceOriginPreservesOtherOriginAndItsCounters(t *testing.T) {
	reg := testRegistry(t)
	limited := plainStub(t, reg)
	limited.Times = 2
	s := NewStore()
	s.Add(limited) // API stub with a budget
	if got := s.Select(method, match.Input{}); got != limited {
		t.Fatal("setup select failed")
	}
	// A file hot-reload must not replenish the API stub's budget.
	fresh := plainStub(t, reg)
	fresh.ID, fresh.Source = "f#0", "f#0"
	fresh.Priority = -1 // below the API stub, so selection still hits the API stub
	if _, err := s.ReplaceOrigin(OriginFile, []*Compiled{fresh}); err != nil {
		t.Fatal(err)
	}
	if got := s.Select(method, match.Input{}); got != limited {
		t.Fatalf("Select = %v, want the API stub's second use", got)
	}
	if got := s.Select(method, match.Input{}); got != fresh {
		t.Fatalf("Select = %v, want fallthrough to file stub: API budget must be spent (2 uses), not replenished by the reload", got)
	}
}

func TestReplaceOriginRejectsDuplicateIDsUntouched(t *testing.T) {
	reg := testRegistry(t)
	a := plainStub(t, reg)
	a.ID, a.Source = "dup#0", "dup#0"
	s := storeWith(t, a)
	b := plainStub(t, reg)
	b.ID, b.Source = "same#0", "same#0"
	c := plainStub(t, reg)
	c.ID, c.Source = "same#0", "same#0"
	if _, err := s.ReplaceOrigin(OriginFile, []*Compiled{b, c}); err == nil {
		t.Fatal("duplicate ids accepted, want error")
	} else if !strings.Contains(err.Error(), "same#0") {
		t.Fatalf("error %q does not name the duplicate id", err)
	}
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].ID != "dup#0" {
		t.Fatalf("failed ReplaceOrigin changed the store: %+v", infos)
	}
}

func TestReplaceOriginAssignsIDsToAPIDocuments(t *testing.T) {
	reg := testRegistry(t)
	s := NewStore()
	ids, err := s.ReplaceOrigin(OriginAPI, []*Compiled{plainStub(t, reg), plainStub(t, reg)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == ids[1] || ids[0] == "" {
		t.Fatalf("ids = %v, want two distinct assigned ids", ids)
	}
}

func TestResetStubsDropsAPIAndRestoresFileBudgets(t *testing.T) {
	reg := testRegistry(t)
	limited := plainStub(t, reg)
	limited.Times = 1
	limited.ID, limited.Source = "f#0", "f#0"
	s := storeWith(t, limited)
	s.Add(plainStub(t, reg))
	if got := s.Select(method, match.Input{}); got == nil {
		t.Fatal("setup select failed")
	}
	s.ResetStubs()
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].Origin != OriginFile {
		t.Fatalf("after ResetStubs: %+v, want only the file stub", infos)
	}
	if infos[0].Hits != 0 {
		t.Fatalf("Hits = %d after ResetStubs, want 0 (budget restored)", infos[0].Hits)
	}
}

func TestListFiltersByMethodAndOrigin(t *testing.T) {
	reg := testRegistry(t)
	f := plainStub(t, reg)
	f.ID, f.Source = "f#0", "f#0"
	s := storeWith(t, f)
	s.Add(plainStub(t, reg))
	api := OriginAPI
	if got := s.List(ListFilter{Origin: &api}); len(got) != 1 || got[0].Origin != OriginAPI {
		t.Fatalf("List(api) = %+v", got)
	}
	if got := s.List(ListFilter{Method: "/no.Such/Method"}); len(got) != 0 {
		t.Fatalf("List(unknown method) = %+v", got)
	}
	if got := s.List(ListFilter{}); len(got) != 2 {
		t.Fatalf("List(all) = %+v", got)
	}
}

func TestListReportsHits(t *testing.T) {
	reg := testRegistry(t)
	c := plainStub(t, reg)
	s := NewStore()
	s.Add(c)
	s.Select(method, match.Input{})
	s.Select(method, match.Input{})
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].Hits != 2 {
		t.Fatalf("Hits = %+v, want 2", infos)
	}
}
