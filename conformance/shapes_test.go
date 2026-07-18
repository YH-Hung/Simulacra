package conformance_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func TestShapes_CorpusDescriptorContractAndDynamicAny(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	h := &harness{reg: reg}
	for method, shape := range map[string]match.Shape{
		"/conformance.v1.CorpusService/Echo":     match.Unary,
		"/conformance.v1.CorpusService/Pull":     match.ServerStream,
		"/conformance.v1.CorpusService/Push":     match.ClientStream,
		"/conformance.v1.CorpusService/Converse": match.Bidi,
	} {
		desc := h.method(t, method, shape)
		if got := desc.Output().FullName(); got != "conformance.v1.Everything" {
			t.Errorf("%s output = %s, want conformance.v1.Everything", method, got)
		}
	}

	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	request := richRequest(t, h, desc.Input(), "any-round-trip")
	data, err := (protojson.MarshalOptions{Resolver: h.reg.Types()}).Marshal(request)
	if err != nil {
		t.Fatalf("marshal dynamic Any request: %v", err)
	}
	for _, want := range []string{"type.googleapis.com/conformance.v1.Inner", "inside-any"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled request %s missing %q", data, want)
		}
	}
}

const shapeStubs = `
- method: conformance.v1.CorpusService/Echo
  match:
    metadata:
      x-tenant: { eq: acme }
    message:
      text: { eq: hi }
  respond:
    metadata: { x-mock: simulacra }
    trailers: { x-stub: echo-1 }
    message: { text: 'hello {{ message.text }}' }
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      text: { eq: boom }
  respond:
    status:
      code: FAILED_PRECONDITION
      message: rejected by conformance stub
      details:
        - type: google.rpc.PreconditionFailure
          value:
            violations:
              - { type: CORPUS, subject: everything/boom, description: rejected }
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      text: { eq: slow }
  respond:
    delay: 2s
    message: { text: eventually }
- method: conformance.v1.CorpusService/Pull
  match:
    message:
      text: { eq: watch }
  respond:
    stream:
      - message: { text: first }
      - delay: 20ms
        message: { text: 'second {{ message.text }}' }
      - status: { code: UNAVAILABLE, message: source exhausted }
- method: conformance.v1.CorpusService/Push
  match:
    expr: 'size(messages) == 3 && messages.all(m, m.text != "")'
  respond:
    message: { text: 'got {{ size(messages) }}' }
- method: conformance.v1.CorpusService/Converse
  respond:
    on_open:
      - message: { text: welcome }
    rules:
      - match:
          message:
            text: { matches: "^ping" }
        send:
          - message: { text: pong }
    on_close:
      status: { code: OK }
`

func TestShapes_UnaryFixtureTemplateContract(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stubs.yaml"), []byte(shapeStubs), 0o600); err != nil {
		t.Fatalf("write stubs: %v", err)
	}
	compiled, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatalf("LoadDirs: %v", errs)
	}
	if len(compiled) == 0 || compiled[0].Plan().Message == nil {
		t.Fatal("unary fixture compiled without a response message")
	}
	method, err := reg.LookupMethod("conformance.v1.CorpusService/Echo")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	request := dynamicpb.NewMessage(method.Input())
	request.Set(method.Input().Fields().ByName("text"), protoreflect.ValueOfString("hi"))
	response, err := compiled[0].Plan().Message.Render(match.Input{Message: request.ProtoReflect()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := fieldString(t, response, "text"); got != "hello hi" {
		t.Fatalf("response text = %q, want hello hi", got)
	}
}

func TestShapes_UnaryEchoMetadataMessageHeadersTrailersAndVerification(t *testing.T) {
	h := newHarness(t, shapeStubs)
	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-tenant", "acme")
	var header, trailer metadata.MD

	response, err := h.invoke(t, ctx, "/conformance.v1.CorpusService/Echo", h.jsonMessage(t, desc.Input(), `{"text":"hi"}`),
		grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := fieldString(t, response, "text"); got != "hello hi" {
		t.Fatalf("response text = %q, want hello hi", got)
	}
	if got := header.Get("x-mock"); len(got) != 1 || got[0] != "simulacra" {
		t.Fatalf("x-mock header = %v, want [simulacra]", got)
	}
	if got := trailer.Get("x-stub"); len(got) != 1 || got[0] != "echo-1" {
		t.Fatalf("x-stub trailer = %v, want [echo-1]", got)
	}

	h.verify(t, `
method: conformance.v1.CorpusService/Echo
match:
  message:
    text: { eq: hi }
times: { exactly: 1 }
`, match.Unary)
	h.verify(t, `
method: conformance.v1.CorpusService/Echo
match:
  message:
    text: { eq: zzz }
times: { never: true }
`, match.Unary)
}

func TestShapes_UnaryEchoStatusDetails(t *testing.T) {
	h := newHarness(t, shapeStubs)
	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	_, err := h.invoke(t, ctx, "/conformance.v1.CorpusService/Echo", h.jsonMessage(t, desc.Input(), `{"text":"boom"}`))
	st := status.Convert(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("Echo error = %v, want FailedPrecondition", err)
	}
	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("status details = %v, want one PreconditionFailure", details)
	}
	failure, ok := details[0].(*errdetails.PreconditionFailure)
	if !ok || len(failure.Violations) != 1 || failure.Violations[0].Subject != "everything/boom" {
		t.Fatalf("status detail = %#v, want PreconditionFailure subject everything/boom", details[0])
	}
}

func TestShapes_UnaryEchoDelayHonorsDeadline(t *testing.T) {
	h := newHarness(t, shapeStubs)
	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()

	_, err := h.invoke(t, ctx, "/conformance.v1.CorpusService/Echo", h.jsonMessage(t, desc.Input(), `{"text":"slow"}`))
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Echo error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("deadline returned after %s, want under 1s", elapsed)
	}
	call, err := waitForJournalCall(h.journal, "/conformance.v1.CorpusService/Echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(call.StubSource, "stubs.yaml#2") {
		t.Errorf("Echo stub source = %q, want suffix stubs.yaml#2", call.StubSource)
	}
	if len(call.Requests) != 1 || fieldString(t, call.Requests[0], "text") != "slow" {
		t.Fatalf("Echo journal requests = %v, want one request with text slow", call.Requests)
	}
	if call.Err == nil || call.Err.Code() != codes.DeadlineExceeded {
		t.Errorf("Echo journal error = %v, want DeadlineExceeded", call.Err)
	}
}

func TestShapes_ServerStreamingPullScriptThenStatus(t *testing.T) {
	h := newHarness(t, shapeStubs)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	stream, desc := h.openStream(t, ctx, "/conformance.v1.CorpusService/Pull", match.ServerStream)
	if err := stream.SendMsg(h.jsonMessage(t, desc.Input(), `{"text":"watch"}`)); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	for i, want := range []string{"first", "second watch"} {
		response := dynamicpb.NewMessage(desc.Output())
		if err := stream.RecvMsg(response); err != nil {
			t.Fatalf("RecvMsg[%d]: %v", i, err)
		}
		if got := fieldString(t, response, "text"); got != want {
			t.Fatalf("response[%d] text = %q, want %q", i, got, want)
		}
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(desc.Output())); status.Code(err) != codes.Unavailable {
		t.Fatalf("terminal RecvMsg = %v, want Unavailable", err)
	}
	h.verify(t, `
method: conformance.v1.CorpusService/Pull
times: { exactly: 1 }
`, match.ServerStream)
}

func TestShapes_ClientStreamingPushMatchesMessages(t *testing.T) {
	h := newHarness(t, shapeStubs)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	stream, desc := h.openStream(t, ctx, "/conformance.v1.CorpusService/Push", match.ClientStream)
	for _, text := range []string{"one", "two", "three"} {
		if err := stream.SendMsg(h.jsonMessage(t, desc.Input(), `{"text":"`+text+`"}`)); err != nil {
			t.Fatalf("SendMsg(%s): %v", text, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	response := dynamicpb.NewMessage(desc.Output())
	if err := stream.RecvMsg(response); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if got := fieldString(t, response, "text"); got != "got 3" {
		t.Fatalf("response text = %q, want got 3", got)
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(desc.Output())); err != io.EOF {
		t.Fatalf("second RecvMsg = %v, want EOF", err)
	}
	h.verify(t, `
method: conformance.v1.CorpusService/Push
match:
  expr: 'size(messages) == 3'
times: { exactly: 1 }
`, match.ClientStream)
}

func TestShapes_BidirectionalConverseOpenRuleAndClose(t *testing.T) {
	h := newHarness(t, shapeStubs)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	stream, desc := h.openStream(t, ctx, "/conformance.v1.CorpusService/Converse", match.Bidi)

	welcome := dynamicpb.NewMessage(desc.Output())
	if err := stream.RecvMsg(welcome); err != nil {
		t.Fatalf("on_open RecvMsg: %v", err)
	}
	if got := fieldString(t, welcome, "text"); got != "welcome" {
		t.Fatalf("on_open text = %q, want welcome", got)
	}
	if err := stream.SendMsg(h.jsonMessage(t, desc.Input(), `{"text":"ping-1"}`)); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	pong := dynamicpb.NewMessage(desc.Output())
	if err := stream.RecvMsg(pong); err != nil {
		t.Fatalf("rule RecvMsg: %v", err)
	}
	if got := fieldString(t, pong, "text"); got != "pong" {
		t.Fatalf("rule text = %q, want pong", got)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(desc.Output())); err != io.EOF {
		t.Fatalf("on_close RecvMsg = %v, want EOF for OK", err)
	}
	h.verify(t, `
method: conformance.v1.CorpusService/Converse
times: { exactly: 1 }
`, match.Bidi)
}
