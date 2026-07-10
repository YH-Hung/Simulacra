package match

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

func TestExplain(t *testing.T) {
	desc := requestDesc(t)
	request := msg(t, desc, `{
		"order_id": "o-999",
		"customer": {"region": "EU"},
		"tags": ["prio", "gift"]
	}`)

	tests := []struct {
		name     string
		block    *Block
		metadata metadata.MD
		wantNil  bool
		want     []string
		contains []string
	}{
		{
			name:    "matching eq returns nil",
			block:   &Block{Message: map[string]Rules{"order_id": {"eq": "o-999"}}},
			wantNil: true,
		},
		{
			name:  "eq mismatch names field",
			block: &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}}},
			want:  []string{`message order_id: expected to equal "o-123"; actual "o-999"`},
		},
		{
			name:  "enum mismatch shows enum name",
			block: &Block{Message: map[string]Rules{"customer.region": {"eq": "UK"}}},
			want:  []string{"message customer.region: expected to equal UK; actual EU"},
		},
		{
			name:  "absent metadata eq identifies absence",
			block: &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}},
			want:  []string{`metadata x-tenant: expected to contain "acme"; key absent`},
		},
		{
			name:     "wrong metadata value shows actual",
			block:    &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}},
			metadata: metadata.Pairs("x-tenant", "other"),
			want:     []string{`metadata x-tenant: expected to contain "acme"; actual ["other"]`},
		},
		{
			name:  "present true on unset field says unset",
			block: &Block{Message: map[string]Rules{"customer.id": {"present": true}}},
			want:  []string{"message customer.id: expected to be set; actual unset"},
		},
		{
			name:  "failed contains shows actual item",
			block: &Block{Message: map[string]Rules{"tags": {"contains": "bulk"}}},
			want:  []string{`message tags: expected to contain "bulk"; actual ["prio", "gift"]`},
		},
		{
			name:  "false CEL expression shows source",
			block: &Block{Expr: `message.order_id == "nope"`},
			want:  []string{`expr is false: message.order_id == "nope"`},
		},
		{
			name:     "CEL runtime error says expression errored",
			block:    &Block{Expr: `metadata['absent-key'][0] == "x"`},
			contains: []string{"expr errored:", `metadata['absent-key'][0] == "x"`},
		},
		{
			name: "metadata and message reasons are ordered",
			block: &Block{
				Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}},
				Message:  map[string]Rules{"order_id": {"eq": "o-123"}},
			},
			want: []string{
				`metadata x-tenant: expected to contain "acme"; key absent`,
				`message order_id: expected to equal "o-123"; actual "o-999"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiled, err := NewCompiler(testFiles(t)).Compile(desc, tt.block, Unary)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			reasons := compiled.Explain(Input{
				Method:   "shop.v1.OrderService/GetOrder",
				Metadata: tt.metadata,
				Message:  request,
			})
			if tt.wantNil {
				if reasons != nil {
					t.Fatalf("Explain = %q, want nil", reasons)
				}
				return
			}
			if len(tt.want) > 0 && !reflect.DeepEqual(reasons, tt.want) {
				t.Errorf("Explain = %#v, want %#v", reasons, tt.want)
			}
			for _, want := range tt.contains {
				if len(reasons) != 1 || !strings.Contains(reasons[0], want) {
					t.Errorf("Explain = %q, want one reason containing %q", reasons, want)
				}
			}
		})
	}
}

func TestExplainNilMessage(t *testing.T) {
	desc := requestDesc(t)
	compiled, err := NewCompiler(testFiles(t)).Compile(desc, &Block{
		Message: map[string]Rules{"order_id": {"eq": "o-123"}},
	}, Unary)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	reasons := compiled.Explain(Input{Method: "shop.v1.OrderService/GetOrder"})
	want := []string{"message order_id: no request message"}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("Explain = %q, want %q", reasons, want)
	}
}

func TestCompileRuleOrderAndLiteralSources(t *testing.T) {
	desc := requestDesc(t)
	block := &Block{
		Metadata: map[string]Rules{
			"z-key": {"present": true, "eq": "z"},
			"a-key": {"ne": "nope", "matches": "^a"},
		},
		Message: map[string]Rules{
			"tags":     {"present": true, "contains": "prio"},
			"small":    {"eq": -42},
			"order_id": {"present": true, "matches": "^o", "ne": "nope", "in": []any{"a", "b"}, "eq": "o-123"},
		},
	}
	wantOrder := []string{
		"metadata:a-key:matches", "metadata:a-key:ne",
		"metadata:z-key:eq", "metadata:z-key:present",
		"message:order_id:eq", "message:order_id:in", "message:order_id:matches", "message:order_id:ne", "message:order_id:present",
		"message:small:eq",
		"message:tags:contains", "message:tags:present",
	}

	for attempt := 0; attempt < 50; attempt++ {
		compiled, err := NewCompiler(testFiles(t)).Compile(desc, block, Unary)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		var gotOrder []string
		for _, rule := range compiled.metadata {
			gotOrder = append(gotOrder, "metadata:"+rule.key+":"+rule.op)
		}
		literalSources := map[string]string{}
		for _, rule := range compiled.message {
			path := joinPath(rule.path)
			gotOrder = append(gotOrder, "message:"+path+":"+rule.op)
			switch rule.op {
			case "eq", "contains":
				literalSources[path+":"+rule.op] = rule.lit.src
			}
		}
		if !reflect.DeepEqual(gotOrder, wantOrder) {
			t.Fatalf("compiled rule order = %v, want %v", gotOrder, wantOrder)
		}
		wantSources := map[string]string{"order_id:eq": "o-123", "small:eq": "-42", "tags:contains": "prio"}
		if !reflect.DeepEqual(literalSources, wantSources) {
			t.Fatalf("literal sources = %v, want %v", literalSources, wantSources)
		}
	}
}

func TestExplainUsesInputTime(t *testing.T) {
	desc := requestDesc(t)
	compiled, err := NewCompiler(testFiles(t)).Compile(desc, &Block{
		Expr: `now == timestamp("2026-07-10T00:00:00Z")`,
	}, Unary)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	reasons := compiled.Explain(Input{
		Method:  "shop.v1.OrderService/GetOrder",
		Message: msg(t, desc, `{"order_id":"o-123"}`),
		Now:     time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
	})
	if reasons != nil {
		t.Fatalf("Explain = %q, want nil when input time satisfies CEL", reasons)
	}
}
