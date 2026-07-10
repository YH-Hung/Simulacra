package match

import (
	"strings"
	"testing"

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
		name      string
		block     *Block
		metadata  metadata.MD
		wantNil   bool
		want      []string
		wantCount int
	}{
		{
			name:    "matching eq returns nil",
			block:   &Block{Message: map[string]Rules{"order_id": {"eq": "o-999"}}},
			wantNil: true,
		},
		{
			name:  "eq mismatch names field",
			block: &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}}},
			want:  []string{"order_id", "o-123", "o-999"},
		},
		{
			name:  "enum mismatch shows enum name",
			block: &Block{Message: map[string]Rules{"customer.region": {"eq": "UK"}}},
			want:  []string{"customer.region", "UK", "EU"},
		},
		{
			name:  "absent metadata eq identifies absence",
			block: &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}},
			want:  []string{"x-tenant", "absent"},
		},
		{
			name:     "wrong metadata value shows actual",
			block:    &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}},
			metadata: metadata.Pairs("x-tenant", "other"),
			want:     []string{"x-tenant", "acme", "other"},
		},
		{
			name:  "present true on unset field says unset",
			block: &Block{Message: map[string]Rules{"customer.id": {"present": true}}},
			want:  []string{"customer.id", "unset"},
		},
		{
			name:  "failed contains shows actual item",
			block: &Block{Message: map[string]Rules{"tags": {"contains": "bulk"}}},
			want:  []string{"tags", "bulk", "prio"},
		},
		{
			name:  "false CEL expression shows source",
			block: &Block{Expr: `message.order_id == "nope"`},
			want:  []string{`expr is false: message.order_id == "nope"`},
		},
		{
			name:  "CEL runtime error says expression errored",
			block: &Block{Expr: `metadata['absent-key'][0] == "x"`},
			want:  []string{"expr errored:", `metadata['absent-key'][0] == "x"`},
		},
		{
			name: "metadata and message reasons are ordered",
			block: &Block{
				Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}},
				Message:  map[string]Rules{"order_id": {"eq": "o-123"}},
			},
			want:      []string{"x-tenant", "order_id"},
			wantCount: 2,
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
			if tt.wantCount != 0 && len(reasons) != tt.wantCount {
				t.Fatalf("Explain returned %d reasons (%q), want %d", len(reasons), reasons, tt.wantCount)
			}
			for i, want := range tt.want {
				if tt.wantCount > 0 {
					if !strings.Contains(reasons[i], want) {
						t.Errorf("reason[%d] = %q, want it to contain %q", i, reasons[i], want)
					}
					continue
				}
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
	if len(reasons) != 1 || !strings.Contains(reasons[0], "no request message") {
		t.Fatalf("Explain = %q, want one no-request-message reason", reasons)
	}
}
