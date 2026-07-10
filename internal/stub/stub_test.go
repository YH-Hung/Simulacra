package stub

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

var matchBlockWithBadField = match.Block{
	Message: map[string]match.Rules{"no_such": {"eq": "x"}},
}

func testRegistry(t *testing.T) *schema.Registry {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	return reg
}

func TestCompileBuildsResponse(t *testing.T) {
	reg := testRegistry(t)
	s := Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Respond: Respond{Message: map[string]any{
			"order_id": "o-123",
			"status":   "ORDER_STATUS_SHIPPED",
			"note":     "on its way",
			"eta":      "2026-08-01T12:00:00Z", // Timestamp in natural RFC 3339 form
		}},
	}
	c, err := Compile(reg, s, "test.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Method != "/shop.v1.OrderService/GetOrder" {
		t.Errorf("Method = %q, want normalized /shop.v1.OrderService/GetOrder", c.Method)
	}
	out, err := protojson.Marshal(c.Response())
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	for _, want := range []string{`"o-123"`, `ORDER_STATUS_SHIPPED`, `2026-08-01T12:00:00Z`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("response %s missing %s", out, want)
		}
	}
}

func TestCompileEmptyResponseIsValid(t *testing.T) {
	// A stub may legitimately respond with an empty/default message.
	reg := testRegistry(t)
	if _, err := Compile(reg, Stub{Method: "shop.v1.OrderService/GetOrder"}, "t#0"); err != nil {
		t.Fatalf("Compile with empty respond: %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	reg := testRegistry(t)
	cases := []struct {
		name string
		s    Stub
		want string // substring of the error
	}{
		{"unknown method", Stub{Method: "shop.v1.OrderService/Nope"}, "Nope"},
		{"streaming method", Stub{Method: "shop.v1.OrderService/WatchOrder"}, "unary"},
		{"response field not in schema", Stub{
			Method:  "shop.v1.OrderService/GetOrder",
			Respond: Respond{Message: map[string]any{"no_such_field": 1}},
		}, "no_such_field"},
		{"response enum typo", Stub{
			Method:  "shop.v1.OrderService/GetOrder",
			Respond: Respond{Message: map[string]any{"status": "SHIPPED_TYPO"}},
		}, "SHIPPED_TYPO"},
		{"bad match block", Stub{
			Method: "shop.v1.OrderService/GetOrder",
			Match:  &matchBlockWithBadField,
		}, "no_such"},
		{"negative times", Stub{
			Method: "shop.v1.OrderService/GetOrder",
			Times:  -1,
		}, "times"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(reg, tc.s, "t#0")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestBuildMessageResolvesAnyFromRegistry(t *testing.T) {
	reg := testRegistry(t)
	m, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := BuildMessage(reg.Types(), m.Output(), map[string]any{
		"extra": map[string]any{
			"@type":  "type.googleapis.com/shop.v1.Customer",
			"id":     "c-1",
			"region": "EU",
		},
	})
	if err != nil {
		t.Fatalf("BuildMessage with Any: %v", err)
	}
	out, err := (protojson.MarshalOptions{Resolver: reg.Types()}).Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type.googleapis.com/shop.v1.Customer", `"c-1"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("marshaled %s missing %s", out, want)
		}
	}
}

func TestBuildMessageUnknownAnyTypeFails(t *testing.T) {
	reg := testRegistry(t)
	m, _ := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	_, err := BuildMessage(reg.Types(), m.Output(), map[string]any{
		"extra": map[string]any{"@type": "type.googleapis.com/no.such.Type"},
	})
	if err == nil || !strings.Contains(err.Error(), "no.such.Type") {
		t.Errorf("err = %v, want mention of no.such.Type", err)
	}
}
