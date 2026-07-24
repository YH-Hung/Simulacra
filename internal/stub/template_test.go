package stub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

func templateFor(t *testing.T, fields map[string]any) (*Template, *schema.Registry) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	env, err := match.NewCompiler(reg.Files()).Env(method.Input(), match.Unary)
	if err != nil {
		t.Fatalf("CEL Env: %v", err)
	}
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), fields, "orders.yaml#2")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	return tmpl, reg
}

func templateParts(t *testing.T) (*schema.Registry, protoreflect.MethodDescriptor, *cel.Env) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	env, err := match.NewCompiler(reg.Files()).Env(method.Input(), match.Unary)
	if err != nil {
		t.Fatalf("CEL Env: %v", err)
	}
	return reg, method, env
}

func conformanceTemplateParts(t *testing.T) (*schema.Registry, protoreflect.MethodDescriptor, *cel.Env) {
	t.Helper()
	return conformanceMethodParts(t, "conformance.v1.CorpusService/Echo")
}

func conformanceMethodParts(t *testing.T, methodName string) (*schema.Registry, protoreflect.MethodDescriptor, *cel.Env) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod(methodName)
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	env, err := match.NewCompiler(reg.Files()).Env(method.Input(), match.Unary)
	if err != nil {
		t.Fatalf("CEL Env: %v", err)
	}
	return reg, method, env
}

func msgFor(t *testing.T, desc protoreflect.MessageDescriptor, body string) *dynamicpb.Message {
	t.Helper()
	msg := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(body), msg); err != nil {
		t.Fatalf("protojson.Unmarshal: %v", err)
	}
	return msg
}

func msgForResolver(t *testing.T, desc protoreflect.MessageDescriptor, body string, resolver *schema.Types) *dynamicpb.Message {
	t.Helper()
	msg := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{Resolver: resolver}).Unmarshal([]byte(body), msg); err != nil {
		t.Fatalf("protojson.Unmarshal: %v", err)
	}
	return msg
}

func messageJSON(t *testing.T, msg *dynamicpb.Message, resolver *schema.Types) map[string]any {
	t.Helper()
	data, err := (protojson.MarshalOptions{Resolver: resolver}).Marshal(msg)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return out
}

func TestTemplateStaticResponseIsPrebuiltAndShared(t *testing.T) {
	tmpl, _ := templateFor(t, map[string]any{
		"order_id": "o-static",
		"status":   "ORDER_STATUS_SHIPPED",
	})

	first, err := tmpl.Render(match.Input{})
	if err != nil {
		t.Fatalf("Render first: %v", err)
	}
	second, err := tmpl.Render(match.Input{})
	if err != nil {
		t.Fatalf("Render second: %v", err)
	}
	if first != second {
		t.Fatalf("static Render pointers differ: %p != %p", first, second)
	}
	if first != tmpl.static {
		t.Fatalf("Render = %p, want prebuilt static %p", first, tmpl.static)
	}
}

func TestTemplateRendersTypedWholeSiteAndInterpolatedStrings(t *testing.T) {
	tmpl, reg := templateFor(t, map[string]any{
		"order_id": "{{ message.order_id }}",
		"note":     "owner={{ metadata['x-owner'][0] }} order={{message.order_id}}",
	})
	method, _ := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	in := match.Input{
		Method:   "/shop.v1.OrderService/GetOrder",
		Metadata: metadata.Pairs("x-owner", "alice"),
		Message:  msgFor(t, method.Input(), `{"orderId":"o-42"}`).ProtoReflect(),
	}

	got, err := tmpl.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	jsonMsg := messageJSON(t, got, reg.Types())
	if jsonMsg["orderId"] != "o-42" {
		t.Errorf("orderId = %#v, want o-42", jsonMsg["orderId"])
	}
	if jsonMsg["note"] != "owner=alice order=o-42" {
		t.Errorf("note = %#v", jsonMsg["note"])
	}
}

func TestTemplateRendersTimestampFromTypedExpression(t *testing.T) {
	tmpl, reg := templateFor(t, map[string]any{
		"eta": "{{ now + duration('72h') }}",
	})
	now := time.Date(2026, 7, 11, 3, 4, 5, 600, time.FixedZone("test", 8*60*60))

	got, err := tmpl.Render(match.Input{Now: now})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	jsonMsg := messageJSON(t, got, reg.Types())
	etaText, ok := jsonMsg["eta"].(string)
	if !ok {
		t.Fatalf("eta = %#v, want JSON timestamp string", jsonMsg["eta"])
	}
	eta, err := time.Parse(time.RFC3339Nano, etaText)
	if err != nil {
		t.Fatalf("parsing eta %q: %v", etaText, err)
	}
	if delta := eta.Sub(now); delta < 72*time.Hour-time.Millisecond || delta > 72*time.Hour+time.Millisecond {
		t.Errorf("eta-now = %v, want approximately 72h", delta)
	}
}

func TestTemplateRejectsStaticallyIncompatibleWholeSiteTypes(t *testing.T) {
	reg, method, env := templateParts(t)
	tests := []struct {
		name   string
		fields map[string]any
		want   []string
	}{
		{
			name:   "bool into string",
			fields: map[string]any{"note": "{{ true }}"},
			want:   []string{"bool", "string"},
		},
		{
			name:   "int into timestamp",
			fields: map[string]any{"eta": "{{ 42 }}"},
			want:   []string{"int", "google.protobuf.Timestamp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTemplate(env, reg.Types(), method.Output(), tt.fields, "orders.yaml#2")
			if err == nil {
				t.Fatal("newTemplate succeeded, want incompatible CEL result error")
			}
			for _, want := range append([]string{"orders.yaml#2"}, tt.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestTemplateRejectsStaticallyTypedCompositeCELResults(t *testing.T) {
	reg, method, env := templateParts(t)
	tests := []struct {
		name string
		expr string
		want string
	}{
		{name: "list", expr: "['one', 'two']", want: "list"},
		{name: "map", expr: "{'one': 1}", want: "map"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
				"note": "{{ " + tt.expr + " }}",
			}, "orders.yaml#2")
			if err == nil {
				t.Fatal("newTemplate succeeded, want unsupported composite CEL result error")
			}
			for _, want := range []string{"orders.yaml#2", "unsupported CEL result type", tt.want, "lists or maps"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestTemplateDynamicRepeatedItemDoesNotHideInvalidStaticSibling(t *testing.T) {
	reg, method, env := templateParts(t)
	_, err := newTemplate(env, reg.Types(), method.Input(), map[string]any{
		"tags": []any{"{{ message.order_id }}", true},
	}, "orders.yaml#2")
	if err == nil {
		t.Fatal("newTemplate succeeded, want invalid static repeated element error")
	}
	for _, want := range []string{"orders.yaml#2", "tags", "true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateAcceptsCompatibleTypedAndDynamicWholeSites(t *testing.T) {
	reg, method, env := templateParts(t)

	_, err := newTemplate(env, reg.Types(), method.Input(), map[string]any{
		"order_id": "{{ message.order_id }}",
		"big":      "{{ message.big }}",
		"small":    "{{ message.small }}",
		"ratio":    "{{ message.ratio }}",
		"customer": "{{ message.customer }}",
		"tags":     []any{"{{ message.order_id }}"},
	}, "orders.yaml#2")
	if err != nil {
		t.Fatalf("newTemplate compatible typed sites: %v", err)
	}
	_, err = newTemplate(env, reg.Types(), method.Input(), map[string]any{
		"small": "{{ '42' }}",
	}, "orders.yaml#2")
	if err != nil {
		t.Fatalf("newTemplate numeric string site: %v", err)
	}
	for _, tt := range []struct {
		name   string
		fields map[string]any
	}{
		{name: "bytes rendered as string", fields: map[string]any{"note": "{{ b'abc' }}"}},
		{name: "duration rendered as string", fields: map[string]any{"note": "{{ duration('1s') }}"}},
		{name: "enum from numeric CEL type", fields: map[string]any{"status": "{{ message.customer.region }}"}},
		{name: "null", fields: map[string]any{"eta": "{{ null }}"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newTemplate(env, reg.Types(), method.Output(), tt.fields, "orders.yaml#2"); err != nil {
				t.Fatalf("newTemplate compatible site: %v", err)
			}
		})
	}

	_, err = newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"note": "{{ message.payload }}",
	}, "orders.yaml#2")
	if err != nil {
		t.Fatalf("newTemplate dynamic site: %v", err)
	}
}

func TestTemplateAcceptsScalarEncodedWellKnownTypes(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	tests := []struct {
		name   string
		fields map[string]any
	}{
		{name: "string wrapper", fields: map[string]any{"wrapped": "{{ 'wrapped value' }}"}},
		{name: "Value bool", fields: map[string]any{"val": "{{ true }}"}},
		{name: "Value number", fields: map[string]any{"val": "{{ 1.5 }}"}},
		{name: "Value string", fields: map[string]any{"val": "{{ 'text' }}"}},
		{name: "Value null", fields: map[string]any{"val": "{{ null }}"}},
		{name: "FieldMask string", fields: map[string]any{"mask": "{{ 'foo.bar,baz' }}"}},
		{name: "FieldMask rendered as string", fields: map[string]any{"text": "{{ message.mask }}"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newTemplate(env, reg.Types(), method.Output(), tt.fields, "corpus.yaml#1"); err != nil {
				t.Fatalf("newTemplate compatible WKT site: %v", err)
			}
		})
	}
}

func TestTemplateRendersNaturalProtobufJSONSpecialForms(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"when":    "{{ message.when }}",
		"span":    "{{ message.span }}",
		"wrapped": "{{ message.text }}",
		"attrs": map[string]any{
			"label":  "{{ message.text }}",
			"nested": map[string]any{"enabled": "{{ true }}"},
			"items":  []any{"static", "{{ message.text }}"},
		},
		"val":        map[string]any{"label": "{{ message.text }}"},
		"mask":       "{{ message.mask }}",
		"list_value": []any{"{{ message.text }}", map[string]any{"enabled": "{{ true }}"}},
	}, "corpus.yaml#special")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	request := msgFor(t, method.Input(), `{
		"when":"2026-07-18T01:02:03Z",
		"span":"1.500s",
		"mask":"text,optNote",
		"text":"dynamic"
	}`)
	got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	jsonMsg := messageJSON(t, got, reg.Types())
	if jsonMsg["when"] != "2026-07-18T01:02:03Z" || jsonMsg["span"] != "1.500s" || jsonMsg["wrapped"] != "dynamic" || jsonMsg["mask"] != "text,optNote" {
		t.Fatalf("scalar protobuf JSON forms = %#v", jsonMsg)
	}
	attrs, ok := jsonMsg["attrs"].(map[string]any)
	if !ok || attrs["label"] != "dynamic" {
		t.Fatalf("attrs = %#v, want dynamic natural Struct", jsonMsg["attrs"])
	}
	val, ok := jsonMsg["val"].(map[string]any)
	if !ok || val["label"] != "dynamic" {
		t.Fatalf("val = %#v, want dynamic natural Value object", jsonMsg["val"])
	}
	list, ok := jsonMsg["listValue"].([]any)
	if !ok || len(list) != 2 || list[0] != "dynamic" {
		t.Fatalf("listValue = %#v, want dynamic natural ListValue", jsonMsg["listValue"])
	}
}

func TestTemplateRendersTopLevelSpecialProtobufJSONRoots(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	tests := []struct {
		name   string
		method string
		fields map[string]any
		want   string
	}{
		{
			name:   "Struct natural object",
			method: "conformance.v1.CorpusService/StructRoot",
			fields: map[string]any{"echo": "{{ message.text }}", "nested": map[string]any{"ok": "{{ true }}"}},
			want:   `{"echo":"root-struct","nested":{"ok":true}}`,
		},
		{
			name:   "Value natural object",
			method: "conformance.v1.CorpusService/ValueRoot",
			fields: map[string]any{"echo": "{{ message.text }}"},
			want:   `{"echo":"root-value"}`,
		},
		{
			name:   "Any registered envelope",
			method: "conformance.v1.CorpusService/AnyRoot",
			fields: map[string]any{"@type": "type.googleapis.com/conformance.v1.Inner", "id": "{{ message.text }}"},
			want:   `{"@type":"type.googleapis.com/conformance.v1.Inner","id":"root-any"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, err := reg.LookupMethod(tt.method)
			if err != nil {
				t.Fatalf("LookupMethod: %v", err)
			}
			compiled, err := Compile(reg, Stub{
				Method:  tt.method,
				Respond: Respond{Message: tt.fields},
			}, "roots.yaml#0")
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			request := msgFor(t, method.Input(), `{"text":"root-`+strings.ToLower(strings.Fields(tt.name)[0])+`"}`)
			got, err := compiled.Plan().Message.Render(match.Input{Message: request.ProtoReflect()})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			want := msgForResolver(t, method.Output(), tt.want, reg.Types())
			if !proto.Equal(got, want) {
				t.Fatalf("response = %v, want %v", got, want)
			}
		})
	}
}

func TestTemplateRendersRegisteredAnyForms(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	request := msgForResolver(t, method.Input(), `{
		"text":"rendered",
		"payload":{"@type":"type.googleapis.com/conformance.v1.Inner","id":"from-any"},
		"tree":{"label":"from-message"}
	}`, reg.Types())

	tests := []struct {
		name   string
		value  any
		wantID string
		wantTy string
	}{
		{
			name: "natural envelope with templated payload",
			value: map[string]any{
				"@type": "type.googleapis.com/conformance.v1.Inner",
				"id":    "{{ message.text }}",
			},
			wantID: "rendered",
			wantTy: "type.googleapis.com/conformance.v1.Inner",
		},
		{
			name:   "direct registered Any whole site",
			value:  "{{ message.payload }}",
			wantID: "from-any",
			wantTy: "type.googleapis.com/conformance.v1.Inner",
		},
		{
			name:   "registered payload whole site",
			value:  "{{ message.tree }}",
			wantID: "from-message",
			wantTy: "type.googleapis.com/conformance.v1.Node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{"payload": tt.value}, "corpus.yaml#any")
			if err != nil {
				t.Fatalf("newTemplate: %v", err)
			}
			got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			payload, ok := messageJSON(t, got, reg.Types())["payload"].(map[string]any)
			if !ok || payload["@type"] != tt.wantTy {
				t.Fatalf("payload = %#v, want type %q", payload, tt.wantTy)
			}
			valueField := "id"
			if tt.wantTy == "type.googleapis.com/conformance.v1.Node" {
				valueField = "label"
			}
			if gotValue := payload[valueField]; gotValue != tt.wantID {
				t.Fatalf("payload %s = %#v, want %q", valueField, gotValue, tt.wantID)
			}
		})
	}
}

func TestTemplateRendersProto2ExtensionJSONName(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("conformance.v1.LegacyService/Fetch")
	if err != nil {
		t.Fatal(err)
	}
	env, err := match.NewCompiler(reg.Files()).Env(method.Input(), match.Unary)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"note":                     "static",
		"[conformance.v1.ext_tag]": "{{ message.name }}",
	}, "legacy.yaml#extension")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	request := msgFor(t, method.Input(), `{"name":"templated-extension"}`)
	got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if value := messageJSON(t, got, reg.Types())["[conformance.v1.ext_tag]"]; value != "templated-extension" {
		t.Fatalf("extension = %#v, want templated-extension", value)
	}
}

func TestTemplateNaturalAnyRequiresRegisteredStaticType(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"payload": map[string]any{
			"@type": "type.googleapis.com/acme.Unknown",
			"value": "{{ message.text }}",
		},
	}, "corpus.yaml#unknown-any")
	if err == nil || !strings.Contains(err.Error(), "acme.Unknown") || !strings.Contains(err.Error(), "registered") {
		t.Fatalf("newTemplate error = %v, want clear unregistered Any type error", err)
	}
}

func TestTemplateDynamicProto2RequiredFieldLoadsAndRenders(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("conformance.v1.LegacyService/Make")
	if err != nil {
		t.Fatal(err)
	}
	env, err := match.NewCompiler(reg.Files()).Env(method.Input(), match.Unary)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"name": "{{ message.note }}",
	}, "legacy.yaml#required")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	request := msgFor(t, method.Input(), `{"note":"dynamic-required"}`)
	got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if name := messageJSON(t, got, reg.Types())["name"]; name != "dynamic-required" {
		t.Fatalf("name = %#v, want dynamic-required", name)
	}
}

func TestTemplateStaticMissingProto2RequiredFieldStillFailsAtLoad(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("conformance.v1.LegacyService/Make")
	if err != nil {
		t.Fatal(err)
	}
	_, err = newTemplate(nil, reg.Types(), method.Output(), map[string]any{"level": 8}, "legacy.yaml#missing")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "required") || !strings.Contains(err.Error(), "name") {
		t.Fatalf("newTemplate error = %v, want missing required name", err)
	}
}

func TestTemplateDynamicProto2RequiredFieldStillValidatedAtRender(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("conformance.v1.LegacyService/Make")
	if err != nil {
		t.Fatal(err)
	}
	env, err := match.NewCompiler(reg.Files()).Env(method.Input(), match.Unary)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"name": "{{ message.note == 'omit' ? dyn(null) : dyn(message.note) }}",
	}, "legacy.yaml#required-runtime")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	request := msgFor(t, method.Input(), `{"note":"omit"}`)
	_, err = tmpl.Render(match.Input{Message: request.ProtoReflect()})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "required") || !strings.Contains(err.Error(), "name") {
		t.Fatalf("Render error = %v, want final missing required name validation", err)
	}
}

func TestTemplateDynamicRequiredFieldsInNonStringMapsAndExtensions(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.LegacyService/Container")
	tests := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{
			name: "int map key",
			fields: map[string]any{
				"by_number": map[string]any{"7": map[string]any{"name": "{{ message.note }}"}},
			},
			want: `{"byNumber":{"7":{"name":"dynamic-required"}}}`,
		},
		{
			name: "bool map key",
			fields: map[string]any{
				"by_flag": map[string]any{"true": map[string]any{"name": "{{ message.note }}"}},
			},
			want: `{"byFlag":{"true":{"name":"dynamic-required"}}}`,
		},
		{
			name: "message extension",
			fields: map[string]any{
				"[conformance.v1.ext_payload]": map[string]any{"name": "{{ message.note }}"},
			},
			want: `{"[conformance.v1.ext_payload]":{"name":"dynamic-required"}}`,
		},
	}
	request := msgForResolver(t, method.Input(), `{"note":"dynamic-required"}`, reg.Types())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := newTemplate(env, reg.Types(), method.Output(), tt.fields, "required-container.yaml#dynamic")
			if err != nil {
				t.Fatalf("newTemplate: %v", err)
			}
			got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			want := msgForResolver(t, method.Output(), tt.want, reg.Types())
			if !proto.Equal(got, want) {
				t.Fatalf("response = %v, want %v", got, want)
			}
		})
	}
}

func TestTemplateDynamicRequiredFieldsWithNoncanonicalMapKeys(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.LegacyService/Container")
	tests := []struct {
		name  string
		field string
		key   string
		want  string
	}{
		{name: "signed leading zero", field: "by_number", key: "07", want: `{"byNumber":{"7":{"name":"dynamic-required"}}}`},
		{name: "signed leading plus", field: "by_number", key: "+7", want: `{"byNumber":{"7":{"name":"dynamic-required"}}}`},
		{name: "signed negative control", field: "by_number", key: "-7", want: `{"byNumber":{"-7":{"name":"dynamic-required"}}}`},
		{name: "unsigned leading zero", field: "by_unsigned", key: "07", want: `{"byUnsigned":{"7":{"name":"dynamic-required"}}}`},
		{name: "unsigned canonical control", field: "by_unsigned", key: "7", want: `{"byUnsigned":{"7":{"name":"dynamic-required"}}}`},
		{name: "bool true control", field: "by_flag", key: "true", want: `{"byFlag":{"true":{"name":"dynamic-required"}}}`},
		{name: "bool false control", field: "by_flag", key: "false", want: `{"byFlag":{"false":{"name":"dynamic-required"}}}`},
	}
	request := msgForResolver(t, method.Input(), `{"note":"dynamic-required"}`, reg.Types())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
				tt.field: map[string]any{tt.key: map[string]any{"name": "{{ message.note }}"}},
			}, "required-container.yaml#noncanonical")
			if err != nil {
				t.Fatalf("newTemplate: %v", err)
			}
			got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			want := msgForResolver(t, method.Output(), tt.want, reg.Types())
			if !proto.Equal(got, want) {
				t.Fatalf("response = %v, want %v", got, want)
			}
		})
	}
}

func TestTemplateRejectsInvalidUnsignedMapKey(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.LegacyService/Container")
	_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"by_unsigned": map[string]any{"+7": map[string]any{"name": "{{ message.note }}"}},
	}, "required-container.yaml#invalid-unsigned")
	if err == nil {
		t.Fatal("newTemplate accepted an invalid unsigned protobuf JSON map key")
	}
	for _, want := range []string{"required-container.yaml#invalid-unsigned", "uint32", "+7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateStaticMissingRequiredFieldsInMapsAndExtensionsStillFail(t *testing.T) {
	reg, method, _ := conformanceMethodParts(t, "conformance.v1.LegacyService/Container")
	tests := []struct {
		name   string
		fields map[string]any
	}{
		{name: "int map key", fields: map[string]any{"by_number": map[string]any{"7": map[string]any{}}}},
		{name: "bool map key", fields: map[string]any{"by_flag": map[string]any{"true": map[string]any{}}}},
		{name: "message extension", fields: map[string]any{"[conformance.v1.ext_payload]": map[string]any{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTemplate(nil, reg.Types(), method.Output(), tt.fields, "required-container.yaml#static")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "required") || !strings.Contains(err.Error(), "name") {
				t.Fatalf("newTemplate error = %v, want missing required name", err)
			}
		})
	}
}

func TestTemplateWholeSiteMapValuesUseDestinationCompatibleProjection(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	request := msgForResolver(t, method.Input(), `{"text":"mapped","count":7,"child":{"id":"child-7"}}`, reg.Types())
	tests := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{
			name:   "scalar map value",
			fields: map[string]any{"flags": map[string]any{"true": "{{ message.count }}"}},
			want:   `{"flags":{"true":7}}`,
		},
		{
			name:   "ordinary message map value",
			fields: map[string]any{"numbered_items": map[string]any{"7": "{{ message.child }}"}},
			want:   `{"numberedItems":{"7":{"id":"child-7"}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := newTemplate(env, reg.Types(), method.Output(), tt.fields, "map-values.yaml#0")
			if err != nil {
				t.Fatalf("newTemplate: %v", err)
			}
			got, err := tmpl.Render(match.Input{Message: request.ProtoReflect()})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			want := msgForResolver(t, method.Output(), tt.want, reg.Types())
			if !proto.Equal(got, want) {
				t.Fatalf("response = %v, want %v", got, want)
			}
		})
	}
}

func TestTemplateRejectsStaticallyNullRequiredFieldAtLoad(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.LegacyService/Make")
	_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"name": "{{ null }}",
	}, "required-null.yaml#0")
	if err == nil {
		t.Fatal("newTemplate accepted statically null required field")
	}
	for _, want := range []string{"required-null.yaml#0", "{{ null }}", "required", "name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateAllowsNullForRequiredValueAndNullValueDestinations(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.NullService/Nulls")
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"value":     "{{ null }}",
		"null_enum": "{{ null }}",
	}, "required-null.yaml#semantics")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	got, err := tmpl.Render(match.Input{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := msgForResolver(t, method.Output(), `{"value":null,"nullEnum":null}`, reg.Types())
	if !proto.Equal(got, want) {
		t.Fatalf("response = %v, want %v", got, want)
	}
}

func TestTemplateRejectsStaticallyNullRepeatedAndMapElementsAtLoad(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.NullService/Nulls")
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "repeated scalar", field: "numbers", value: []any{"{{ null }}"}},
		{name: "repeated message", field: "payloads", value: []any{"{{ null }}"}},
		{name: "scalar map value", field: "number_map", value: map[string]any{"key": "{{ null }}"}},
		{name: "message map value", field: "payload_map", value: map[string]any{"key": "{{ null }}"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
				"value":     "{{ null }}",
				"null_enum": "{{ null }}",
				tt.field:    tt.value,
			}, "container-null.yaml#0")
			if err == nil {
				t.Fatal("newTemplate accepted statically null repeated element or map value")
			}
			for _, want := range []string{"container-null.yaml#0", "{{ null }}", "null", tt.field} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestTemplateAllowsNullForOptionalAndContainerValueDestinations(t *testing.T) {
	reg, method, env := conformanceMethodParts(t, "conformance.v1.NullService/Nulls")
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"value":            "{{ null }}",
		"null_enum":        "{{ null }}",
		"optional_payload": "{{ null }}",
		"values":           []any{"{{ null }}"},
		"null_enums":       []any{"{{ null }}"},
		"value_map":        map[string]any{"value": "{{ null }}"},
		"null_enum_map":    map[string]any{"enum": "{{ null }}"},
	}, "container-null.yaml#semantics")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	got, err := tmpl.Render(match.Input{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := msgForResolver(t, method.Output(), `{
		"value": null,
		"nullEnum": null,
		"values": [null],
		"nullEnums": [null],
		"valueMap": {"value": null},
		"nullEnumMap": {"enum": null}
	}`, reg.Types())
	if !proto.Equal(got, want) {
		t.Fatalf("response = %v, want %v", got, want)
	}
}

func TestTemplateRejectsUnknownResponseFieldAtCompileTime(t *testing.T) {
	reg, method, env := templateParts(t)
	_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"no_such_field": "{{ message.order_id }}",
	}, "orders.yaml#2")
	if err == nil || !strings.Contains(err.Error(), "no_such_field") {
		t.Fatalf("newTemplate error = %v, want unknown field", err)
	}
}

func TestTemplateRejectsUnknownCELSymbolAtCompileTime(t *testing.T) {
	reg, method, env := templateParts(t)
	_, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"note": "{{ missing_symbol }}",
	}, "orders.yaml#2")
	if err == nil {
		t.Fatal("newTemplate succeeded, want CEL compile error")
	}
	for _, want := range []string{"orders.yaml#2", "missing_symbol"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateCompileReportsFirstFieldDeterministically(t *testing.T) {
	reg, method, env := templateParts(t)
	fields := map[string]any{
		"order_id": "{{ missing_order_id }}",
		"note":     "{{ missing_note }}",
	}

	for i := 0; i < 100; i++ {
		_, err := newTemplate(env, reg.Types(), method.Output(), fields, "orders.yaml#2")
		if err == nil {
			t.Fatal("newTemplate succeeded, want CEL compile error")
		}
		if got := err.Error(); !strings.Contains(got, `field "note"`) || !strings.Contains(got, "missing_note") {
			t.Fatalf("iteration %d: error = %q, want deterministic diagnostic for field note", i, got)
		}
	}
}

func TestTemplateMissingMetadataReportsSiteAndSource(t *testing.T) {
	tmpl, _ := templateFor(t, map[string]any{
		"note": "owner={{ metadata['missing'][0] }}",
	})

	_, err := tmpl.Render(match.Input{})
	if err == nil {
		t.Fatal("Render succeeded, want missing metadata error")
	}
	for _, want := range []string{"orders.yaml#2", "metadata['missing'][0]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateRenderReportsFirstFieldDeterministically(t *testing.T) {
	tmpl, _ := templateFor(t, map[string]any{
		"order_id": "{{ metadata['missing-order'][0] }}",
		"note":     "{{ metadata['missing-note'][0] }}",
	})

	for i := 0; i < 100; i++ {
		_, err := tmpl.Render(match.Input{})
		if err == nil {
			t.Fatal("Render succeeded, want missing metadata error")
		}
		if got := err.Error(); !strings.Contains(got, `field "note"`) || !strings.Contains(got, "missing-note") {
			t.Fatalf("iteration %d: error = %q, want deterministic diagnostic for field note", i, got)
		}
	}
}

func TestTemplateEmbeddedEnumErrorPreservesExpression(t *testing.T) {
	tmpl, _ := templateFor(t, map[string]any{
		"status": "ORDER_STATUS_{{ metadata['status-suffix'][0] }}",
	})

	_, err := tmpl.Render(match.Input{Metadata: metadata.Pairs("status-suffix", "NOT_REAL")})
	if err == nil {
		t.Fatal("Render succeeded with invalid interpolated enum")
	}
	for _, want := range []string{"orders.yaml#2", "{{ metadata['status-suffix'][0] }}", "ORDER_STATUS_NOT_REAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateEmbeddedRangeAndWKTErrorPreserveExpression(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	tests := []struct {
		name   string
		fields map[string]any
		body   string
		site   string
		value  string
	}{
		{
			name:   "int32 range",
			fields: map[string]any{"count": "214748364{{ message.count }}"},
			body:   `{"count":8}`,
			site:   "{{ message.count }}",
			value:  "2147483648",
		},
		{
			name:   "timestamp text",
			fields: map[string]any{"when": "2026-{{ message.count }}-01T00:00:00Z"},
			body:   `{"count":13}`,
			site:   "{{ message.count }}",
			value:  "2026-13-01T00:00:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := newTemplate(env, reg.Types(), method.Output(), tt.fields, "embedded.yaml#0")
			if err != nil {
				t.Fatalf("newTemplate: %v", err)
			}
			request := msgForResolver(t, method.Input(), tt.body, reg.Types())
			_, err = tmpl.Render(match.Input{Message: request.ProtoReflect()})
			if err == nil {
				t.Fatal("Render succeeded with invalid interpolated value")
			}
			for _, want := range []string{"embedded.yaml#0", tt.site, tt.value} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestTemplateFinalCrossFieldErrorNamesContributingSites(t *testing.T) {
	reg, method, env := conformanceTemplateParts(t)
	tmpl, err := newTemplate(env, reg.Types(), method.Output(), map[string]any{
		"word":   "word-{{ message.text }}",
		"number": "{{ message.count }}",
	}, "cross-field.yaml#0")
	if err != nil {
		t.Fatalf("newTemplate: %v", err)
	}
	request := msgForResolver(t, method.Input(), `{"text":"set","count":7}`, reg.Types())
	_, err = tmpl.Render(match.Input{Message: request.ProtoReflect()})
	if err == nil {
		t.Fatal("Render succeeded with two oneof alternatives")
	}
	for _, want := range []string{"cross-field.yaml#0", "{{ message.text }}", "{{ message.count }}", "oneof"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTemplateRenderedEnumStringMustFitSchema(t *testing.T) {
	tmpl, _ := templateFor(t, map[string]any{
		"status": "{{ metadata['status'][0] }}",
	})

	_, err := tmpl.Render(match.Input{Metadata: metadata.Pairs("status", "NOT_AN_ORDER_STATUS")})
	if err == nil {
		t.Fatal("Render succeeded with invalid enum")
	}
	for _, want := range []string{"orders.yaml#2", "{{ metadata['status'][0] }}", "NOT_AN_ORDER_STATUS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCELToJSONUsesCanonicalProtobufDuration(t *testing.T) {
	got, err := celToJSON(nil, -1500*time.Millisecond)
	if err != nil {
		t.Fatalf("celToJSON: %v", err)
	}
	if got != "-1.500s" {
		t.Errorf("celToJSON(-1.5s) = %#v, want canonical protobuf duration", got)
	}
}

func TestTemplateLeavesMalformedSiteDelimiterStatic(t *testing.T) {
	const literal = "literal {{ message.order_id }"
	tmpl, reg := templateFor(t, map[string]any{"note": literal})
	if tmpl.static == nil {
		t.Fatal("malformed delimiter unexpectedly made the template dynamic")
	}
	got, err := tmpl.Render(match.Input{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if note := messageJSON(t, got, reg.Types())["note"]; note != literal {
		t.Errorf("note = %#v, want literal malformed delimiter", note)
	}
}

func TestCELToJSONUsesSchemaResolverForDynamicAny(t *testing.T) {
	reg, method, _ := templateParts(t)
	request := dynamicpb.NewMessage(method.Output())
	requestJSON := []byte(`{
		"extra": {
			"@type": "type.googleapis.com/shop.v1.Customer",
			"id": "customer-7",
			"region": "EU"
		}
	}`)
	if err := (protojson.UnmarshalOptions{Resolver: reg.Types()}).Unmarshal(requestJSON, request); err != nil {
		t.Fatalf("unmarshaling dynamic Any request: %v", err)
	}

	got, err := celToJSON(reg.Types(), request)
	if err != nil {
		t.Fatalf("celToJSON dynamic Any: %v", err)
	}
	data, ok := got.(json.RawMessage)
	if !ok {
		t.Fatalf("celToJSON result = %T, want json.RawMessage", got)
	}
	var rendered map[string]any
	if err := json.Unmarshal(data, &rendered); err != nil {
		t.Fatalf("unmarshaling rendered message: %v", err)
	}
	extra, ok := rendered["extra"].(map[string]any)
	if !ok || extra["@type"] != "type.googleapis.com/shop.v1.Customer" || extra["id"] != "customer-7" {
		t.Errorf("rendered extra = %#v, want dynamically resolved Customer", rendered["extra"])
	}
}
