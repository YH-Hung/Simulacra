package stub

import (
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
	store := NewStore([]*Compiled{fallback, specific}) // load order: fallback first

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
	store := NewStore([]*Compiled{only})
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
	store := NewStore([]*Compiled{old})

	if got := store.Select(method, match.Input{}); got != old {
		t.Fatalf("first Select = %v, want old stub", got)
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("second Select = %v, want exhausted budget", got)
	}

	store.Replace([]*Compiled{fresh})
	if got := store.Select(method, match.Input{}); got != fresh {
		t.Fatalf("Select after Replace = %v, want fresh stub", got)
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("second Select after Replace = %v, want fresh budget exhausted", got)
	}
}

func TestLenCountsEveryMethodAndFollowsReplace(t *testing.T) {
	reg := testRegistry(t)
	order := compiled(t, reg, Stub{Method: "shop.v1.OrderService/GetOrder", Times: 1})
	list := compiled(t, reg, Stub{Method: "shop.v1.OrderService/UploadOrders"})
	store := NewStore([]*Compiled{order, list})

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

	store.Replace([]*Compiled{list})
	if got := store.Len(); got != 1 {
		t.Fatalf("Len after Replace = %d, want 1", got)
	}
	if got := NewStore(nil).Len(); got != 0 {
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

	store := NewStore([]*Compiled{twoWrong, oneWrong, spent})
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
	store := NewStore([]*Compiled{first, high, second})
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
	budgetStore := NewStore([]*Compiled{limited})
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
	store := NewStore([]*Compiled{limited})

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
	store := NewStore([]*Compiled{first, second})
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
