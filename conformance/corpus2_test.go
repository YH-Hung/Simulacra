package conformance_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

var corpusCasesII = []corpusCase{
	{
		feature: "any.registered",
		stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.payload.id == "in-1"
  respond:
    message:
      payload:
        "@type": type.googleapis.com/conformance.v1.Inner
        id: out-1
`,
		req: `{
  "payload": {
    "@type": "type.googleapis.com/conformance.v1.Inner",
    "id": "in-1"
  }
}`,
		want: []string{"out-1", "type.googleapis.com/conformance.v1.Inner"},
		expect: `{
  "payload": {
    "@type": "type.googleapis.com/conformance.v1.Inner",
    "id": "out-1"
  }
}`,
	},
	{
		feature: "proto2.required_and_defaults",
		stub: `
- method: conformance.v1.LegacyService/Fetch
  match:
    message:
      name: { eq: thing }
    expr: message.level == 7
  respond:
    message: { note: fetched }
`,
		req:    `{"name":"thing"}`,
		want:   []string{"fetched"},
		expect: `{"note":"fetched"}`,
	},
	{
		feature: "proto2.extensions",
		stub: `
- method: conformance.v1.LegacyService/Fetch
  respond:
    message:
      note: x
      "[conformance.v1.ext_tag]": tagged
`,
		req:    `{"name":"thing"}`,
		want:   []string{"[conformance.v1.ext_tag]", "tagged"},
		expect: `{"note":"x","[conformance.v1.ext_tag]":"tagged"}`,
	},
}

func TestCorpusHardCasesII(t *testing.T) {
	for _, tc := range corpusCasesII {
		t.Run(tc.feature, func(t *testing.T) {
			h := start(t, tc.stub)
			methodName := corpusMethod
			if strings.Contains(tc.stub, "LegacyService") {
				methodName = "/conformance.v1.LegacyService/Fetch"
			}
			desc := h.method(t, methodName, match.Unary)
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			response, err := h.invoke(t, ctx, methodName, h.jsonMessage(t, desc.Input(), tc.req))
			if err != nil {
				t.Fatalf("invoke %s: %v", tc.feature, err)
			}
			decoded := dynamicpb.NewMessage(desc.Output())
			h.unmarshalWire(t, h.marshalWire(t, response), decoded)
			data, err := (protojson.MarshalOptions{Resolver: h.reg.Types()}).Marshal(decoded)
			if err != nil {
				t.Fatalf("marshal %s response: %v", tc.feature, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(data), want) {
					t.Errorf("response %s missing %q", data, want)
				}
			}
			assertCorpusResponse(t, h, desc.Output(), decoded, tc.expect)
		})
	}
}

func TestCorpusRegisteredAnyResponseUnpacksSemantically(t *testing.T) {
	h := newHarness(t, `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.payload.id == "in-1"
  respond:
    message:
      payload:
        "@type": type.googleapis.com/conformance.v1.Inner
        id: out-1
`)
	method := h.method(t, corpusMethod, match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	response, err := h.invoke(t, ctx, corpusMethod, h.jsonMessage(t, method.Input(), `{
  "payload": {"@type":"type.googleapis.com/conformance.v1.Inner", "id":"in-1"}
}`))
	if err != nil {
		t.Fatalf("invoke registered Any: %v", err)
	}
	payloadField := response.Descriptor().Fields().ByName("payload")
	var envelope anypb.Any
	h.unmarshalWire(t, h.marshalWire(t, response.Get(payloadField).Message().Interface()), &envelope)
	unpacked, err := anypb.UnmarshalNew(&envelope, proto.UnmarshalOptions{Resolver: h.reg.Types()})
	if err != nil {
		t.Fatalf("unpack registered Any: %v", err)
	}
	inner, ok := unpacked.(*dynamicpb.Message)
	if !ok {
		t.Fatalf("unpacked Any = %T, want *dynamicpb.Message", unpacked)
	}
	if got := fieldString(t, inner, "id"); got != "out-1" {
		t.Fatalf("unpacked Any id = %q, want out-1", got)
	}
}

func TestCorpusProto2DefaultPresenceSemantics(t *testing.T) {
	h := newHarness(t, `
- method: conformance.v1.LegacyService/Fetch
  match:
    message:
      name: { eq: thing }
    expr: message.level == 7
  respond:
    message: { note: fetched }
`)
	method := h.method(t, "/conformance.v1.LegacyService/Fetch", match.Unary)
	request := h.jsonMessage(t, method.Input(), `{"name":"thing"}`)
	level := request.Descriptor().Fields().ByName("level")
	if request.Has(level) {
		t.Fatal("proto2 level is set, want unset presence")
	}
	if got := request.Get(level).Int(); got != 7 {
		t.Fatalf("proto2 level default = %d, want 7", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	response, err := h.invoke(t, ctx, "/conformance.v1.LegacyService/Fetch", request)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := fieldString(t, response, "note"); got != "fetched" {
		t.Fatalf("Fetch note = %q, want fetched", got)
	}
	if want := h.jsonMessage(t, method.Output(), `{"note":"fetched"}`); !proto.Equal(response, want) {
		t.Fatalf("Fetch response = %v, want %v", response, want)
	}
}

func TestCorpusProto2ExtensionDescriptorSemantics(t *testing.T) {
	h := newHarness(t, `
- method: conformance.v1.LegacyService/Fetch
  respond:
    message:
      note: x
      "[conformance.v1.ext_tag]": tagged
`)
	method := h.method(t, "/conformance.v1.LegacyService/Fetch", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	response, err := h.invoke(t, ctx, "/conformance.v1.LegacyService/Fetch", h.jsonMessage(t, method.Input(), `{"name":"thing"}`))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := fieldString(t, response, "note"); got != "x" {
		t.Fatalf("Fetch note = %q, want x", got)
	}
	extensionType, err := h.reg.Types().FindExtensionByName("conformance.v1.ext_tag")
	if err != nil {
		t.Fatalf("find ext_tag type: %v", err)
	}
	extension := extensionType.TypeDescriptor()
	decoded := dynamicpb.NewMessage(method.Output())
	h.unmarshalWire(t, h.marshalWire(t, response), decoded)
	if !decoded.Has(extension) || decoded.Get(extension).String() != "tagged" {
		t.Fatalf("ext_tag = %v (present %v), want tagged", decoded.Get(extension), decoded.Has(extension))
	}
}

func TestCorpusUnregisteredAny(t *testing.T) {
	t.Run("any.unregistered", testCorpusUnregisteredAny)
}

func testCorpusUnregisteredAny(t *testing.T) {
	h := newHarness(t, `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      payload.type_url: { eq: type.googleapis.com/acme.Opaque }
  respond:
    message: { text: matched-opaque }
`)
	method := h.method(t, corpusMethod, match.Unary)
	request := dynamicpb.NewMessage(method.Input())
	payloadField := request.Descriptor().Fields().ByName("payload")
	payload := request.Mutable(payloadField).Message()
	payload.Set(payload.Descriptor().Fields().ByName("type_url"), protoreflect.ValueOfString("type.googleapis.com/acme.Opaque"))
	payload.Set(payload.Descriptor().Fields().ByName("value"), protoreflect.ValueOfBytes([]byte{1, 2, 3}))

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	response, err := h.invoke(t, ctx, corpusMethod, request)
	if err != nil {
		t.Fatalf("Echo opaque Any: %v", err)
	}
	if got := fieldString(t, response, "text"); got != "matched-opaque" {
		t.Fatalf("Echo text = %q, want matched-opaque", got)
	}
	if want := h.jsonMessage(t, method.Output(), `{"text":"matched-opaque"}`); !proto.Equal(response, want) {
		t.Fatalf("Echo response = %v, want %v", response, want)
	}
	call, err := waitForJournalCall(h.journal, corpusMethod, rpcTimeout)
	if err != nil {
		t.Fatal(err)
	}
	journalPayload := call.Requests[0].Get(payloadField).Message()
	got := journalPayload.Get(journalPayload.Descriptor().Fields().ByName("value")).Bytes()
	if !proto.Equal(call.Requests[0], request) || string(got) != string([]byte{1, 2, 3}) {
		t.Fatalf("journal request did not preserve opaque Any bytes: %v", got)
	}
}

func TestCorpusUnregisteredAnyBuildFailsAtLoad(t *testing.T) {
	errs := loadCorpusStubs(t, `
- method: conformance.v1.CorpusService/Echo
  respond:
    message:
      payload:
        "@type": type.googleapis.com/acme.Opaque
        value: AQID
`)
	if len(errs) == 0 {
		t.Fatal("building unregistered Any unexpectedly loaded")
	}
	if got := errs[0].Error(); !strings.Contains(got, "acme.Opaque") {
		t.Fatalf("load error = %q, want unregistered type name acme.Opaque", got)
	}
}

func TestCorpusMissingRequiredProto2ResponseFailsAtLoad(t *testing.T) {
	errs := loadCorpusStubs(t, `
- method: conformance.v1.LegacyService/Make
  respond:
    message: { level: 8 }
`)
	if len(errs) == 0 {
		t.Fatal("LegacyThing response missing required name unexpectedly loaded")
	}
	if got := strings.ToLower(errs[0].Error()); !strings.Contains(got, "name") || !strings.Contains(got, "required") {
		t.Fatalf("load error = %q, want missing required name", got)
	}
}

func TestCorpusUnknownFieldsPreserved(t *testing.T) {
	t.Run("unknown_fields.preserved", testCorpusUnknownFieldsPreserved)
}

func testCorpusUnknownFieldsPreserved(t *testing.T) {
	h := newHarness(t, `
- method: conformance.v1.CorpusService/Echo
  respond:
    message: { text: unknown-preserved }
`)
	echo := h.method(t, corpusMethod, match.Unary)
	wideDesc, err := h.reg.LookupMessage("conformance.v1.WideEverything")
	if err != nil {
		t.Fatalf("LookupMessage WideEverything: %v", err)
	}
	wide := h.jsonMessage(t, wideDesc, `{"extra":"extra99"}`)
	request := dynamicpb.NewMessage(echo.Input())
	h.unmarshalWire(t, h.marshalWire(t, wide), request)
	if len(request.GetUnknown()) == 0 {
		t.Fatal("wire conversion did not retain field 99 as unknown")
	}

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	response, err := h.invoke(t, ctx, corpusMethod, request)
	if err != nil {
		t.Fatalf("Echo unknown fields: %v", err)
	}
	if want := h.jsonMessage(t, echo.Output(), `{"text":"unknown-preserved"}`); !proto.Equal(response, want) {
		t.Fatalf("Echo response = %v, want %v", response, want)
	}
	call, err := waitForJournalCall(h.journal, corpusMethod, rpcTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if len(call.Requests) != 1 || len(call.Requests[0].GetUnknown()) == 0 {
		t.Fatal("journal request lost unknown field 99")
	}
}

func TestCorpusEditionsBasic(t *testing.T) {
	t.Run("editions.basic", testCorpusEditionsBasic)
}

func testCorpusEditionsBasic(t *testing.T) {
	h := newHarness(t, `
- method: conformance.v1e.EditionService/Get
  match:
    message:
      name: { eq: edition-in }
  respond:
    message: { name: edition-out }
`)
	method := h.method(t, "/conformance.v1e.EditionService/Get", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	response, err := h.invoke(t, ctx, "/conformance.v1e.EditionService/Get", h.jsonMessage(t, method.Input(), `{"name":"edition-in"}`))
	if err != nil {
		t.Fatalf("EditionService.Get: %v", err)
	}
	want := h.jsonMessage(t, method.Output(), `{"name":"edition-out"}`)
	if !proto.Equal(response, want) {
		t.Fatalf("EditionService.Get response = %v, want %v", response, want)
	}
}

func loadCorpusStubs(t *testing.T, body string) []error {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stubs.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write stubs: %v", err)
	}
	_, errs := stub.LoadDirs(reg, []string{dir})
	return errs
}
