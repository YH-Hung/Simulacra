package match

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/schema"
)

func requestDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	m, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	return m.Input()
}

func msg(t *testing.T, desc protoreflect.MessageDescriptor, jsonBody string) protoreflect.Message {
	t.Helper()
	m := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(jsonBody), m); err != nil {
		t.Fatalf("building test message from %s: %v", jsonBody, err)
	}
	return m
}

func TestEval(t *testing.T) {
	desc := requestDesc(t)
	req := `{
		"order_id": "o-123",
		"customer": {"id": "c-9", "region": "EU"},
		"items": [{"sku": "A1", "qty": "3"}, {"sku": "B2", "qty": "1"}],
		"tags": ["prio", "gift"]
	}`

	cases := []struct {
		name  string
		block *Block
		body  string
		md    metadata.MD
		want  bool
	}{
		{"nil block matches all", nil, req, nil, true},
		{"eq string", &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}}}, req, nil, true},
		{"eq string miss", &Block{Message: map[string]Rules{"order_id": {"eq": "other"}}}, req, nil, false},
		{"ne", &Block{Message: map[string]Rules{"order_id": {"ne": "other"}}}, req, nil, true},
		{"nested path", &Block{Message: map[string]Rules{"customer.id": {"eq": "c-9"}}}, req, nil, true},
		{"enum by name", &Block{Message: map[string]Rules{"customer.region": {"eq": "EU"}}}, req, nil, true},
		{"enum in list", &Block{Message: map[string]Rules{"customer.region": {"in": []any{"EU", "UK"}}}}, req, nil, true},
		{"enum in list miss", &Block{Message: map[string]Rules{"customer.region": {"in": []any{"UK", "US"}}}}, req, nil, false},
		{"regex", &Block{Message: map[string]Rules{"order_id": {"matches": "^o-\\d+$"}}}, req, nil, true},
		{"present true", &Block{Message: map[string]Rules{"customer": {"present": true}}}, req, nil, true},
		{"present false on unset", &Block{Message: map[string]Rules{"customer": {"present": true}}}, `{"order_id":"x"}`, nil, false},
		{"present on repeated means non-empty", &Block{Message: map[string]Rules{"items": {"present": true}}}, `{"order_id":"x"}`, nil, false},
		{"contains on repeated string", &Block{Message: map[string]Rules{"tags": {"contains": "gift"}}}, req, nil, true},
		{"contains miss", &Block{Message: map[string]Rules{"tags": {"contains": "bulk"}}}, req, nil, false},
		{"unset leaf reads as default", &Block{Message: map[string]Rules{"customer.id": {"eq": ""}}}, `{"order_id":"x"}`, nil, true},
		{"two rules AND", &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}, "customer.id": {"eq": "nope"}}}, req, nil, false},
		{"metadata eq", &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}, req, metadata.Pairs("x-tenant", "acme"), true},
		{"metadata eq missing key", &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}, req, nil, false},
		{"metadata present", &Block{Metadata: map[string]Rules{"authorization": {"present": true}}}, req, metadata.Pairs("authorization", "Bearer x"), true},
		{"metadata any-value semantics", &Block{Metadata: map[string]Rules{"x-tag": {"eq": "b"}}}, req, metadata.Pairs("x-tag", "a", "x-tag", "b"), true},
		{"metadata regex", &Block{Metadata: map[string]Rules{"x-tenant": {"matches": "^ac"}}}, req, metadata.Pairs("x-tenant", "acme"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compile(desc, tc.block)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := c.Eval(msg(t, desc, tc.body), tc.md); got != tc.want {
				t.Errorf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	desc := requestDesc(t)
	cases := []struct {
		name  string
		block *Block
	}{
		{"unknown field", &Block{Message: map[string]Rules{"no_such_field": {"eq": "x"}}}},
		{"unknown nested field", &Block{Message: map[string]Rules{"customer.nope": {"eq": "x"}}}},
		{"descend through scalar", &Block{Message: map[string]Rules{"order_id.sub": {"eq": "x"}}}},
		{"descend through repeated", &Block{Message: map[string]Rules{"items.sku": {"eq": "x"}}}},
		{"unknown op", &Block{Message: map[string]Rules{"order_id": {"equalz": "x"}}}},
		{"regex on non-string", &Block{Message: map[string]Rules{"customer.region": {"matches": "E.*"}}}},
		{"bad regex", &Block{Message: map[string]Rules{"order_id": {"matches": "("}}}},
		{"contains on singular", &Block{Message: map[string]Rules{"order_id": {"contains": "x"}}}},
		{"eq on repeated", &Block{Message: map[string]Rules{"tags": {"eq": "x"}}}},
		{"unknown enum name", &Block{Message: map[string]Rules{"customer.region": {"eq": "MARS"}}}},
		{"type mismatch", &Block{Message: map[string]Rules{"order_id": {"eq": 42}}}},
		{"metadata unknown op", &Block{Metadata: map[string]Rules{"k": {"has": true}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(desc, tc.block); err == nil {
				t.Error("expected compile error, got nil")
			}
		})
	}
}
