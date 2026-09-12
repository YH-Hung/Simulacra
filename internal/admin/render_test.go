package admin_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// getOrderMessages builds a GetOrder request with order_id and a GetOrder
// response with note, against reg.
func getOrderMessages(t *testing.T, reg *schema.Registry, orderID, note string) (*dynamicpb.Message, *dynamicpb.Message) {
	t.Helper()
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	req := dynamicpb.NewMessage(method.Input())
	req.Set(method.Input().Fields().ByName("order_id"), protoreflect.ValueOfString(orderID))
	resp := dynamicpb.NewMessage(method.Output())
	resp.Set(method.Output().Fields().ByName("note"), protoreflect.ValueOfString(note))
	return req, resp
}

// jsonFields decodes a DecodedMessage's json. protojson output is not
// byte-stable, so tests compare decoded fields and never the string.
func jsonFields(t *testing.T, text string) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatalf("decoding json %q: %v", text, err)
	}
	return fields
}

func TestRenderCallRendersMessagesMetadataAndStatus(t *testing.T) {
	reg := testDeps(t).Registry
	req, resp := getOrderMessages(t, reg, "o-1", "shipped")
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	call := &journal.Call{
		Seq:    7,
		Method: "/shop.v1.OrderService/GetOrder",
		Metadata: metadata.MD{
			"x-tenant":  {"acme", "beta"},
			"trace-bin": {"\x00\xff"},
			"a-first":   {"1"},
		},
		Requests:  []*dynamicpb.Message{req},
		Responses: []*dynamicpb.Message{resp},
		StubID:    "api-3",
		Start:     start,
		Duration:  1500 * time.Millisecond,
	}

	got := admin.RenderCall(call, reg.Types())

	if got.Seq != 7 || got.Method != "/shop.v1.OrderService/GetOrder" || got.MatchedStubId != "api-3" {
		t.Errorf("seq/method/matched_stub_id = %d/%q/%q", got.Seq, got.Method, got.MatchedStubId)
	}
	if !got.Start.AsTime().Equal(start) || got.Duration.AsDuration() != 1500*time.Millisecond {
		t.Errorf("start/duration = %v/%v", got.Start.AsTime(), got.Duration.AsDuration())
	}

	var keys []string
	for _, entry := range got.RequestMetadata {
		keys = append(keys, entry.Key)
	}
	if want := []string{"a-first", "trace-bin", "x-tenant"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("metadata keys = %q, want %q (sorted)", keys, want)
	}
	binary, text := got.RequestMetadata[1], got.RequestMetadata[2]
	if len(binary.Values) != 0 || len(binary.BinaryValues) != 1 || string(binary.BinaryValues[0]) != "\x00\xff" {
		t.Errorf("trace-bin entry = %v, want its bytes in binary_values only", binary)
	}
	if len(text.BinaryValues) != 0 || !reflect.DeepEqual(text.Values, []string{"acme", "beta"}) {
		t.Errorf("x-tenant entry = %v, want [acme beta] in values only", text)
	}

	if len(got.Requests) != 1 || len(got.Responses) != 1 {
		t.Fatalf("requests/responses = %d/%d, want 1/1", len(got.Requests), len(got.Responses))
	}
	request := got.Requests[0]
	if request.TypeName != "shop.v1.GetOrderRequest" {
		t.Errorf("request type_name = %q, want shop.v1.GetOrderRequest", request.TypeName)
	}
	fields := jsonFields(t, request.Json)
	if fields["order_id"] != "o-1" {
		t.Errorf("request json = %s, want order_id o-1 under its proto field name", request.Json)
	}
	if _, camel := fields["orderId"]; camel {
		t.Errorf("request json = %s uses the JSON name; matchers use proto names", request.Json)
	}
	decoded := dynamicpb.NewMessage(req.Descriptor())
	if err := proto.Unmarshal(request.WireBytes, decoded); err != nil || !proto.Equal(decoded, req) {
		t.Errorf("request wire_bytes do not decode back to the request (err %v)", err)
	}
	if note := jsonFields(t, got.Responses[0].Json)["note"]; note != "shipped" {
		t.Errorf("response json = %s, want note shipped", got.Responses[0].Json)
	}

	if got.Status == nil || got.Status.Code != 0 || got.Status.Message != "" || len(got.Status.Details) != 0 {
		t.Errorf("status of a call with no Err = %v, want a present status with code 0", got.Status)
	}
}

// Everything rendering cannot represent faithfully degrades instead of
// failing: an Any holding an unregistered type, an unresolvable status detail,
// and strings holding invalid UTF-8. The final marshals are the discriminator:
// a response carrying one invalid UTF-8 string does not serialize at all.
func TestRenderCallDegradesWhatItCannotRepresent(t *testing.T) {
	reg := testDeps(t).Registry
	req, _ := getOrderMessages(t, reg, "o-1", "")
	payload := req.Descriptor().Fields().ByName("payload")
	unknown := dynamicpb.NewMessage(payload.Message())
	unknown.Set(payload.Message().Fields().ByName("type_url"),
		protoreflect.ValueOfString("type.googleapis.com/unknown.v1.Thing"))
	unknown.Set(payload.Message().Fields().ByName("value"), protoreflect.ValueOfBytes([]byte{0x0a, 0x01, 'x'}))
	req.Set(payload, protoreflect.ValueOfMessage(unknown))

	customerType, err := reg.Types().FindMessageByName("shop.v1.Customer")
	if err != nil {
		t.Fatalf("FindMessageByName: %v", err)
	}
	customer := customerType.New()
	customer.Set(customer.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString("c-1"))
	known, err := anypb.New(customer.Interface())
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	unresolvable := &anypb.Any{TypeUrl: "type.googleapis.com/unknown.v1.Thing", Value: []byte{0x0a, 0x01, 'x'}}

	call := &journal.Call{
		Seq:      1,
		Method:   "/shop.v1.OrderService/Get\xffOrder",
		Metadata: metadata.MD{"x-probe": {"ok\xffbad"}},
		Requests: []*dynamicpb.Message{req},
		Err: status.FromProto(&spb.Status{
			Code:    int32(codes.NotFound),
			Message: "bad\xffmessage",
			Details: []*anypb.Any{known, unresolvable},
		}),
		StubID: "stubs/\xff.yaml#0",
	}

	got := admin.RenderCall(call, reg.Types())

	if got.Requests[0].Json != "" || len(got.Requests[0].WireBytes) == 0 {
		t.Errorf("request with an unregistered Any: json %q, %d wire bytes; want empty json and the wire bytes kept",
			got.Requests[0].Json, len(got.Requests[0].WireBytes))
	}
	if got.Status.Code != int32(codes.NotFound) || got.Status.Message != "bad�message" {
		t.Errorf("status = %d %q, want NotFound with U+FFFD in the message", got.Status.Code, got.Status.Message)
	}
	if len(got.Status.Details) != 2 {
		t.Fatalf("details = %d, want 2", len(got.Status.Details))
	}
	if d := got.Status.Details[0]; d.TypeName != "shop.v1.Customer" || jsonFields(t, d.Json)["id"] != "c-1" {
		t.Errorf("resolvable detail = %v, want shop.v1.Customer rendered to json", d)
	}
	if d := got.Status.Details[1]; d.TypeName != "unknown.v1.Thing" || d.Json != "" || string(d.WireBytes) != "\x0a\x01x" {
		t.Errorf("unresolvable detail = %v, want the URL's type name, the raw bytes, and empty json", d)
	}
	if got.Method != "/shop.v1.OrderService/Get�Order" {
		t.Errorf("method = %q, want U+FFFD in place of the invalid byte", got.Method)
	}
	if values := got.RequestMetadata[0].Values; len(values) != 1 || values[0] != "ok�bad" {
		t.Errorf("x-probe values = %q, want [ok�bad]", values)
	}
	if got.MatchedStubId != "stubs/�.yaml#0" {
		t.Errorf("matched_stub_id = %q, want U+FFFD in place of the invalid byte", got.MatchedStubId)
	}
	if _, err := proto.Marshal(got); err != nil {
		t.Fatalf("the rendered call does not marshal: %v", err)
	}
	if _, err := protojson.Marshal(got); err != nil {
		t.Fatalf("the rendered call does not marshal to JSON: %v", err)
	}
}

func TestValidUTF8(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"plain":      "plain",
		"café":       "café",
		"ok\xffbad":  "ok�bad",
		"a\xff\xfeb": "a�b",
	}
	for in, want := range cases {
		if got := admin.ValidUTF8(in); got != want {
			t.Errorf("ValidUTF8(%q) = %q, want %q", in, got, want)
		}
	}
}
