package dataplane

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

const stubsYAML = `
- method: shop.v1.OrderService/GetOrder
  match:
    metadata:
      x-tenant: { eq: "acme" }
    message:
      order_id: { eq: "o-123" }
  respond:
    message: { order_id: "o-123", status: ORDER_STATUS_SHIPPED, note: "hello" }
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: "echo" }
  respond:
    metadata: { x-mock: "echo" }
    trailers: { x-served-by: "simulacra" }
    message: { order_id: "echo", note: 'tenant {{ metadata["x-tenant"][0] }}' }
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: "fail" }
  respond:
    status:
      code: FAILED_PRECONDITION
      message: order cannot be fulfilled
      details:
        - type: google.rpc.PreconditionFailure
          value:
            violations:
              - { type: ORDER_STATE, subject: "o-123", description: order is not ready }
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: "slow" }
  respond:
    delay: 2s
    message: { order_id: "slow", note: "eventually" }
- method: shop.v1.OrderService/WatchOrder
  match:
    message:
      order_id: { eq: "o-123" }
  respond:
    metadata: { x-mock: "watch" }
    trailers: { x-served-by: "stream-script" }
    stream:
      - message: { order_id: "o-123", status: ORDER_STATUS_PENDING }
      - delay: 20ms
        message: { order_id: "o-123", status: ORDER_STATUS_SHIPPED, note: "for {{ message.order_id }}" }
      - status: { code: UNAVAILABLE, message: "backend hiccup" }
`

func writeStubFile(dir, content string) error {
	return os.WriteFile(filepath.Join(dir, "stubs.yaml"), []byte(content), 0o644)
}

func startServer(t *testing.T) (*schema.Registry, *grpc.ClientConn) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeStubFile(dir, stubsYAML); err != nil {
		t.Fatal(err)
	}
	stubs, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("stub load errors: %v", errs)
	}
	srv, err := New(reg, stub.NewStore(stubs))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return reg, conn
}

func invoke(t *testing.T, reg *schema.Registry, conn *grpc.ClientConn, ctx context.Context, method, reqJSON string) (string, error) {
	return invokeWithOptions(t, reg, conn, ctx, method, reqJSON)
}

func invokeWithOptions(t *testing.T, reg *schema.Registry, conn *grpc.ClientConn, ctx context.Context, method, reqJSON string, opts ...grpc.CallOption) (string, error) {
	t.Helper()
	m, err := reg.LookupMethod(method)
	if err != nil {
		t.Fatal(err)
	}
	req := dynamicpb.NewMessage(m.Input())
	if err := protojson.Unmarshal([]byte(reqJSON), req); err != nil {
		t.Fatal(err)
	}
	resp := dynamicpb.NewMessage(m.Output())
	if err := conn.Invoke(ctx, method, req, resp, opts...); err != nil {
		return "", err
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), nil
}

func openStream(t *testing.T, reg *schema.Registry, conn *grpc.ClientConn, ctx context.Context, method string) (grpc.ClientStream, protoreflect.MethodDescriptor) {
	t.Helper()
	desc, err := reg.LookupMethod(method)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		ClientStreams: desc.IsStreamingClient(),
		ServerStreams: desc.IsStreamingServer(),
	}, method)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	return stream, desc
}

func sendJSON(t *testing.T, stream grpc.ClientStream, desc protoreflect.MessageDescriptor, body string) {
	t.Helper()
	message := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(body), message); err != nil {
		t.Fatalf("protojson.Unmarshal: %v", err)
	}
	if err := stream.SendMsg(message); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
}

func TestUnaryMatchedCall(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-tenant", "acme")

	out, err := invoke(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"o-123"}`)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for _, want := range []string{"o-123", "ORDER_STATUS_SHIPPED", "hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("response %s missing %q", out, want)
		}
	}
}

func TestUnaryNoStubMatched(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Right method, but metadata is missing → no stub matches.
	_, err := invoke(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"o-123"}`)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if !strings.Contains(st.Message(), "4 stub") {
		t.Errorf("message %q should mention how many stubs exist for the method", st.Message())
	}
	if !strings.Contains(st.Message(), "x-tenant") {
		t.Errorf("message %q should include the nearest-miss metadata detail", st.Message())
	}
}

func TestUnaryStatusIncludesConcretePreconditionFailure(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := invoke(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"fail"}`)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("details = %v, want one PreconditionFailure", details)
	}
	precondition, ok := details[0].(*errdetails.PreconditionFailure)
	if !ok {
		t.Fatalf("details[0] = %T, want *errdetails.PreconditionFailure", details[0])
	}
	violations := precondition.GetViolations()
	if len(violations) != 1 || violations[0].GetSubject() != "o-123" {
		t.Fatalf("violations = %v, want subject o-123", violations)
	}
}

func TestUnaryTemplateAndResponseMetadata(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-tenant", "acme")
	var header, trailer metadata.MD

	out, err := invokeWithOptions(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"echo"}`,
		grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(out, "tenant acme") {
		t.Errorf("response %s missing rendered tenant", out)
	}
	if got := header.Get("x-mock"); len(got) != 1 || got[0] != "echo" {
		t.Errorf("x-mock header = %v, want [echo]", got)
	}
	if got := trailer.Get("x-served-by"); len(got) != 1 || got[0] != "simulacra" {
		t.Errorf("x-served-by trailer = %v, want [simulacra]", got)
	}
}

func TestUnaryDelayHonorsDeadline(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()

	_, err := invoke(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"slow"}`)
	if st, ok := status.FromError(err); !ok || st.Code() != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("deadline returned after %v, want under 1s", elapsed)
	}
}

func TestUnaryRejectsMissingRequestFrame(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true},
		"/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	method, err := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	err = stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.Internal {
		t.Fatalf("RecvMsg error = %v, want Internal", err)
	}
	if !strings.Contains(st.Message(), "missing request") {
		t.Fatalf("message = %q, want missing request context", st.Message())
	}
}

func TestUnaryRejectsSecondRequestFrame(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true},
		"/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	method, err := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := dynamicpb.NewMessage(method.Input())
		if err := protojson.Unmarshal([]byte(`{"order_id":"slow"}`), req); err != nil {
			t.Fatal(err)
		}
		if err := stream.SendMsg(req); err != nil {
			t.Fatalf("SendMsg(%d): %v", i+1, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	err = stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.Internal {
		t.Fatalf("RecvMsg error = %v, want Internal", err)
	}
	if !strings.Contains(st.Message(), "cardinality") {
		t.Fatalf("message = %q, want cardinality context", st.Message())
	}
}

type receiveErrorStream struct {
	grpc.ServerStream
	err error
}

func (s receiveErrorStream) Context() context.Context { return context.Background() }
func (s receiveErrorStream) RecvMsg(any) error        { return s.err }

func TestUnaryPreservesReceiveStatusCode(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	method, err := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Canceled, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			receiveErr := status.Error(code, "transport receive failed")
			err := (&Server{}).unary(receiveErrorStream{err: receiveErr},
				"/shop.v1.OrderService/GetOrder", method, match.Input{})
			st := status.Convert(err)
			if st.Code() != code {
				t.Fatalf("code = %s, want %s (err %v)", st.Code(), code, err)
			}
			if !strings.Contains(st.Message(), "receiving request") || !strings.Contains(st.Message(), "transport receive failed") {
				t.Fatalf("message = %q, want receive context and cause", st.Message())
			}
		})
	}
}

func TestNoMatchFormatsTopThreeAndEllipsis(t *testing.T) {
	const full = "/shop.v1.OrderService/GetOrder"
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	var stubs []*stub.Compiled
	for i, tc := range []struct {
		source   string
		priority int
		orderID  string
	}{
		{"first", 40, "o-1"},
		{"second", 30, "o-2"},
		{"third", 20, "o-3"},
		{"fourth", 10, "o-4"},
	} {
		compiled, err := stub.Compile(reg, stub.Stub{
			Method:   "shop.v1.OrderService/GetOrder",
			Priority: tc.priority,
			Match:    &match.Block{Message: map[string]match.Rules{"order_id": {"eq": tc.orderID}}},
		}, tc.source)
		if err != nil {
			t.Fatalf("Compile stub %d: %v", i, err)
		}
		stubs = append(stubs, compiled)
	}
	store := stub.NewStore(stubs)
	methodDesc, err := reg.LookupMethod(full)
	if err != nil {
		t.Fatal(err)
	}
	request := dynamicpb.NewMessage(methodDesc.Input())
	if err := protojson.Unmarshal([]byte(`{"order_id":"actual"}`), request); err != nil {
		t.Fatal(err)
	}
	in := match.Input{Method: strings.TrimPrefix(full, "/"), Message: request.ProtoReflect()}
	server := &Server{store: store}

	selected, misses := store.SelectOrExplain(full, in)
	if selected != nil {
		t.Fatalf("SelectOrExplain selected %v, want nearest misses", selected)
	}
	message := status.Convert(server.noMatch(full, misses)).Message()
	want := `simulacra: no stub matched /shop.v1.OrderService/GetOrder (4 stub(s) registered for this method); first (priority 40): message order_id: expected to equal "o-1"; actual "actual"; second (priority 30): message order_id: expected to equal "o-2"; actual "actual"; third (priority 20): message order_id: expected to equal "o-3"; actual "actual"; …`
	if message != want {
		t.Fatalf("no-match message:\n got: %s\nwant: %s", message, want)
	}
}

func TestUnknownMethodIsUnimplemented(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Any proto message works as a payload; the server must reject the
	// method name before it ever decodes the body.
	m, _ := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	req := dynamicpb.NewMessage(m.Input())
	resp := dynamicpb.NewMessage(m.Output())
	err := conn.Invoke(ctx, "/no.such.Service/Nope", req, resp)
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented", err)
	}
}

func TestReflectionListsAndResolvesServices(t *testing.T) {
	_, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc := v1reflectionpb.NewServerReflectionClient(conn)
	strm, err := rc.ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// ListServices must include the mocked service and health.
	if err := strm.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := strm.Recv()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range resp.GetListServicesResponse().GetService() {
		names[s.GetName()] = true
	}
	for _, want := range []string{"shop.v1.OrderService", "grpc.health.v1.Health"} {
		if !names[want] {
			t.Errorf("ListServices missing %s (got %v)", want, names)
		}
	}

	// FileContainingSymbol must return descriptor bytes for the mocked service.
	if err := strm.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: "shop.v1.OrderService",
		},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err = strm.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetFileDescriptorResponse().GetFileDescriptorProto()) == 0 {
		t.Error("FileContainingSymbol returned no descriptors for shop.v1.OrderService")
	}
}

func TestHealthCheck(t *testing.T) {
	_, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.GetStatus())
	}
}

func TestServerStreamingScript(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/WatchOrder")
	sendJSON(t, stream, method.Input(), `{"order_id":"o-123"}`)
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	wants := []struct {
		status string
		note   string
	}{
		{status: "ORDER_STATUS_PENDING"},
		{status: "ORDER_STATUS_SHIPPED", note: "for o-123"},
	}
	for i, want := range wants {
		response := dynamicpb.NewMessage(method.Output())
		if err := stream.RecvMsg(response); err != nil {
			t.Fatalf("RecvMsg(%d): %v", i+1, err)
		}
		body, err := protojson.Marshal(response)
		if err != nil {
			t.Fatalf("Marshal response %d: %v", i+1, err)
		}
		if !strings.Contains(string(body), want.status) {
			t.Errorf("response %d = %s, want status %s", i+1, body, want.status)
		}
		if want.note != "" && !strings.Contains(string(body), want.note) {
			t.Errorf("response %d = %s, want note %q", i+1, body, want.note)
		}
		if i == 0 {
			header, err := stream.Header()
			if err != nil {
				t.Fatalf("Header: %v", err)
			}
			if got := header.Get("x-mock"); len(got) != 1 || got[0] != "watch" {
				t.Errorf("x-mock header = %v, want [watch]", got)
			}
		}
	}

	err := stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.Unavailable || st.Message() != "backend hiccup" {
		t.Fatalf("terminal error = %v, want Unavailable backend hiccup", err)
	}
	if got := stream.Trailer().Get("x-served-by"); len(got) != 1 || got[0] != "stream-script" {
		t.Errorf("x-served-by trailer = %v, want [stream-script]", got)
	}
}

func TestServerStreamingRejectsMissingRequestFrame(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/WatchOrder")
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	err := stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.Internal || !strings.Contains(st.Message(), "missing request") {
		t.Fatalf("RecvMsg error = %v, want Internal missing request", err)
	}
}

func TestServerStreamingRejectsSecondRequestFrame(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	method, err := reg.LookupMethod("/shop.v1.OrderService/WatchOrder")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true},
		"/shop.v1.OrderService/WatchOrder")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	sendJSON(t, stream, method.Input(), `{"order_id":"o-123"}`)
	sendJSON(t, stream, method.Input(), `{"order_id":"o-123"}`)
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	err = stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.Internal || !strings.Contains(st.Message(), "cardinality") {
		t.Fatalf("RecvMsg error = %v, want Internal cardinality error", err)
	}
}

type recordingSender struct {
	messages []any
	err      error
}

func (s *recordingSender) SendMsg(message any) error {
	s.messages = append(s.messages, message)
	return s.err
}

func compileServerPlan(t *testing.T, reg *schema.Registry, respond stub.Respond) *stub.Plan {
	t.Helper()
	compiled, err := stub.Compile(reg, stub.Stub{
		Method:  "shop.v1.OrderService/WatchOrder",
		Respond: respond,
	}, "runner.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return compiled.Plan()
}

func TestRunStepsCompletesEmptyScript(t *testing.T) {
	sender := &recordingSender{}
	if err := runSteps(context.Background(), sender, nil, match.Input{}); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent %d messages, want none", len(sender.messages))
	}
}

func TestRunStepsPreservesSendError(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	plan := compileServerPlan(t, reg, stub.Respond{Stream: []stub.StepSpec{{
		Message: map[string]any{"order_id": "o-123"},
	}}})
	sendErr := errors.New("transport send failed")
	err := runSteps(context.Background(), &recordingSender{err: sendErr}, plan.Stream, match.Input{})
	if err != sendErr {
		t.Fatalf("runSteps error = %v, want exact send error %v", err, sendErr)
	}
}

func TestRunStepsHonorsCanceledDelay(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	plan := compileServerPlan(t, reg, stub.Respond{Stream: []stub.StepSpec{{
		Delay: "2s", Message: map[string]any{"order_id": "o-123"},
	}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runSteps(ctx, &recordingSender{}, plan.Stream, match.Input{})
	if got := status.Code(err); got != codes.Canceled {
		t.Fatalf("runSteps code = %s, want Canceled (err %v)", got, err)
	}
}

func TestRunStepsReturnsTerminalStatus(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	plan := compileServerPlan(t, reg, stub.Respond{Stream: []stub.StepSpec{{
		Status: &stub.StatusSpec{Code: "UNAVAILABLE", Message: "planned outage"},
	}}})
	err := runSteps(context.Background(), &recordingSender{}, plan.Stream, match.Input{})
	st := status.Convert(err)
	if st.Code() != codes.Unavailable || st.Message() != "planned outage" {
		t.Fatalf("runSteps error = %v, want Unavailable planned outage", err)
	}
}

func TestRunStepsMapsTemplateFailureToInternal(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	plan := compileServerPlan(t, reg, stub.Respond{Stream: []stub.StepSpec{{
		Message: map[string]any{"note": "{{ metadata['missing'][0] }}"},
	}}})
	err := runSteps(context.Background(), &recordingSender{}, plan.Stream, match.Input{})
	st := status.Convert(err)
	if st.Code() != codes.Internal || !strings.Contains(st.Message(), "rendering response") {
		t.Fatalf("runSteps error = %v, want Internal rendering response", err)
	}
}
