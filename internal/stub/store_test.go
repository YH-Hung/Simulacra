package stub

import (
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
