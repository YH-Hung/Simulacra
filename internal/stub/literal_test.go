package stub

import (
	"testing"

	"github.com/yinghanhung/simulacra/internal/match"
)

func TestLiteralRespondSkipsTemplateSites(t *testing.T) {
	// Each site is valid CEL for its method, so a build that scanned it would
	// render it (to "" against the empty request) rather than fail to compile.
	// on_open runs before any request message exists, so its CEL environment has
	// no `message`; `method` is the valid site there.
	const (
		shopSite   = "{{ message.order_id }}"
		confSite   = "{{ message.text }}"
		onOpenSite = "{{ method }}"
	)
	shop := testRegistry(t)
	conf, _, _ := conformanceMethodParts(t, "conformance.v1.CorpusService/Echo")
	anyBody := map[string]any{"@type": "type.googleapis.com/conformance.v1.Inner", "id": confSite}
	msg := func(fields map[string]any) []StepSpec { return []StepSpec{{Message: fields}} }

	tests := []struct {
		name   string
		reg    string // "shop" or "conf"
		stub   Stub
		render func(*Plan) *Template
		path   []any // JSON path of the field that must hold the site
		want   string
	}{
		{
			name:   "top level",
			reg:    "shop",
			stub:   Stub{Method: "shop.v1.OrderService/GetOrder", Respond: Respond{Message: map[string]any{"note": shopSite}}},
			render: func(p *Plan) *Template { return p.Message },
			path:   []any{"note"},
			want:   shopSite,
		},
		{
			name:   "nested message",
			reg:    "conf",
			stub:   Stub{Method: "conformance.v1.CorpusService/Echo", Respond: Respond{Message: map[string]any{"child": map[string]any{"id": confSite}}}},
			render: func(p *Plan) *Template { return p.Message },
			path:   []any{"child", "id"},
			want:   confSite,
		},
		{
			name: "map value",
			reg:  "conf",
			stub: Stub{Method: "conformance.v1.CorpusService/Echo", Respond: Respond{Message: map[string]any{
				"items": map[string]any{"k": map[string]any{"id": confSite}}}}},
			render: func(p *Plan) *Template { return p.Message },
			path:   []any{"items", "k", "id"},
			want:   confSite,
		},
		{
			name: "repeated element",
			reg:  "conf",
			stub: Stub{Method: "conformance.v1.CorpusService/Echo", Respond: Respond{Message: map[string]any{
				"list_value": []any{confSite}}}},
			render: func(p *Plan) *Template { return p.Message },
			path:   []any{"listValue", 0},
			want:   confSite,
		},
		{
			name:   "Any payload",
			reg:    "conf",
			stub:   Stub{Method: "conformance.v1.CorpusService/Echo", Respond: Respond{Message: map[string]any{"payload": anyBody}}},
			render: func(p *Plan) *Template { return p.Message },
			path:   []any{"payload", "id"},
			want:   confSite,
		},
		{
			name:   "server-stream step",
			reg:    "conf",
			stub:   Stub{Method: "conformance.v1.CorpusService/Pull", Respond: Respond{Stream: msg(map[string]any{"text": confSite})}},
			render: func(p *Plan) *Template { return p.Stream[0].Message },
			path:   []any{"text"},
			want:   confSite,
		},
		{
			name: "bidi send",
			reg:  "conf",
			stub: Stub{Method: "conformance.v1.CorpusService/Converse", Respond: Respond{
				Rules: []RuleSpec{{Send: msg(map[string]any{"text": confSite})}}}},
			render: func(p *Plan) *Template { return p.Rules[0].Send[0].Message },
			path:   []any{"text"},
			want:   confSite,
		},
		{
			name:   "bidi on_open",
			reg:    "conf",
			stub:   Stub{Method: "conformance.v1.CorpusService/Converse", Respond: Respond{OnOpen: msg(map[string]any{"text": onOpenSite})}},
			render: func(p *Plan) *Template { return p.OnOpen[0].Message },
			path:   []any{"text"},
			want:   onOpenSite,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := shop
			if tt.reg == "conf" {
				reg = conf
			}
			tt.stub.Respond.Literal = true
			compiled, err := Compile(reg, tt.stub, "literal.yaml#0")
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got, err := tt.render(compiled.Plan()).Render(match.Input{})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var cur any = messageJSON(t, got, reg.Types())
			for _, step := range tt.path {
				switch step := step.(type) {
				case string:
					cur = cur.(map[string]any)[step]
				case int:
					cur = cur.([]any)[step]
				}
			}
			if cur != tt.want {
				t.Errorf("field %v = %#v, want %q verbatim", tt.path, cur, tt.want)
			}
		})
	}
}

func TestNonLiteralRespondStillRendersTemplates(t *testing.T) {
	reg := testRegistry(t)
	compiled, err := Compile(reg, Stub{
		Method:  "shop.v1.OrderService/GetOrder",
		Respond: Respond{Message: map[string]any{"note": "{{ message.order_id }}"}},
	}, "literal.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	got, err := compiled.Plan().Message.Render(match.Input{
		Message: msgFor(t, method.Input(), `{"orderId":"o-9"}`).ProtoReflect(),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if note := messageJSON(t, got, reg.Types())["note"]; note != "o-9" {
		t.Errorf("note = %#v, want o-9", note)
	}
}
