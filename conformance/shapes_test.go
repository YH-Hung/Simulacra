package conformance_test

import (
	"context"
	"io"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
)

const shapesYAML = `
- method: conformance.v1.CorpusService/Echo
  match:
    metadata:
      x-conformance: { eq: echo }
    message:
      text: { eq: echo }
  respond:
    metadata: { x-shape: unary }
    trailers: { x-finished: echo }
    message: { text: 'echo {{ message.text }}', extra: '{{ message.word }}' }
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
      text: { eq: pull }
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
            text: { eq: ping }
        send:
          - message: { text: pong }
    on_close:
      status: { code: OK }
`

func TestShapes_UnaryEchoMetadataMessageHeadersTrailersAndVerification(t *testing.T) {
	h := newHarness(t, shapesYAML)
	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-conformance", "echo")
	var header, trailer metadata.MD

	response, err := h.invoke(t, ctx, "/conformance.v1.CorpusService/Echo", richRequest(t, h, desc.Input(), "echo"),
		grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := fieldString(t, response, "text"); got != "echo echo" {
		t.Fatalf("response text = %q, want echo echo", got)
	}
	if got := fieldString(t, response, "extra"); got != "chosen" {
		t.Fatalf("response extra = %q, want chosen", got)
	}
	if got := header.Get("x-shape"); len(got) != 1 || got[0] != "unary" {
		t.Fatalf("x-shape header = %v, want [unary]", got)
	}
	if got := trailer.Get("x-finished"); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("x-finished trailer = %v, want [echo]", got)
	}

	h.verify(t, `
method: conformance.v1.CorpusService/Echo
match:
  metadata:
    x-conformance: { eq: echo }
  message:
    text: { eq: echo }
times: { exactly: 1 }
`, match.Unary)
	h.verify(t, `
method: conformance.v1.CorpusService/Echo
match:
  message:
    text: { eq: absent }
times: { never: true }
`, match.Unary)
}

func TestShapes_UnaryEchoStatusDetails(t *testing.T) {
	h := newHarness(t, shapesYAML)
	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	_, err := h.invoke(t, ctx, "/conformance.v1.CorpusService/Echo", richRequest(t, h, desc.Input(), "boom"))
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
	h := newHarness(t, shapesYAML)
	desc := h.method(t, "/conformance.v1.CorpusService/Echo", match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()

	_, err := h.invoke(t, ctx, "/conformance.v1.CorpusService/Echo", richRequest(t, h, desc.Input(), "slow"))
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Echo error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("deadline returned after %s, want under 1s", elapsed)
	}
}

func TestShapes_ServerStreamingPullScriptThenStatus(t *testing.T) {
	h := newHarness(t, shapesYAML)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	stream, desc := h.openStream(t, ctx, "/conformance.v1.CorpusService/Pull", match.ServerStream)
	if err := stream.SendMsg(richRequest(t, h, desc.Input(), "pull")); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	for i, want := range []string{"first", "second pull"} {
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
match:
  message:
    text: { eq: pull }
times: { exactly: 1 }
`, match.ServerStream)
}

func TestShapes_ClientStreamingPushMatchesMessages(t *testing.T) {
	h := newHarness(t, shapesYAML)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	stream, desc := h.openStream(t, ctx, "/conformance.v1.CorpusService/Push", match.ClientStream)
	for _, text := range []string{"one", "two", "three"} {
		if err := stream.SendMsg(richRequest(t, h, desc.Input(), text)); err != nil {
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
  expr: 'size(messages) == 3 && messages.all(m, m.text != "")'
times: { exactly: 1 }
`, match.ClientStream)
}

func TestShapes_BidirectionalConverseOpenRuleAndClose(t *testing.T) {
	h := newHarness(t, shapesYAML)
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
	if err := stream.SendMsg(richRequest(t, h, desc.Input(), "ping")); err != nil {
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
