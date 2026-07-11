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

func msgFor(t *testing.T, desc protoreflect.MessageDescriptor, body string) *dynamicpb.Message {
	t.Helper()
	msg := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(body), msg); err != nil {
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

func TestTemplateRenderedEnumStringMustFitSchema(t *testing.T) {
	tmpl, _ := templateFor(t, map[string]any{
		"status": "{{ metadata['status'][0] }}",
	})

	_, err := tmpl.Render(match.Input{Metadata: metadata.Pairs("status", "NOT_AN_ORDER_STATUS")})
	if err == nil {
		t.Fatal("Render succeeded with invalid enum")
	}
	for _, want := range []string{"orders.yaml#2", "NOT_AN_ORDER_STATUS"} {
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
