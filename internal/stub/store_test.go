package stub

import (
	"strings"
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
