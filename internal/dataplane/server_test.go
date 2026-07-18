package dataplane

import (
	"context"
	"errors"
	"io"
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

	"github.com/yinghanhung/simulacra/internal/journal"
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
- method: shop.v1.OrderService/UploadOrders
  match:
    expr: 'size(messages) == 2 && messages.exists(m, m.order_id == "o-1")'
  respond:
    message: { note: 'got {{ size(messages) }} orders' }
- method: shop.v1.OrderService/UploadOrders
  match:
    expr: 'size(messages) == 0'
  respond:
    message: { note: 'got {{ size(messages) }} orders' }
- method: shop.v1.OrderService/Chat
  respond:
    on_open:
      - message: { text: welcome }
    rules:
      - match:
          message:
            text: { matches: 'ping.*' }
        send:
          - message: { text: pong }
      - match:
          expr: 'size(messages) >= 3'
        send:
          - message: { text: chatty }
    on_close:
      status: { code: OK }
`

func writeStubFile(dir, content string) error {
	return os.WriteFile(filepath.Join(dir, "stubs.yaml"), []byte(content), 0o644)
}

func startServer(t *testing.T) (*schema.Registry, *grpc.ClientConn, *journal.Journal) {
	return startServerWithStubs(t, stubsYAML)
}

func startServerWithStubs(t *testing.T, contents string) (*schema.Registry, *grpc.ClientConn, *journal.Journal) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeStubFile(dir, contents); err != nil {
		t.Fatal(err)
	}
	stubs, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("stub load errors: %v", errs)
	}
	j := journal.New(100)
	srv, err := New(reg, stub.NewStore(stubs), j)
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
	return reg, conn, j
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

func recvText(t *testing.T, stream grpc.ClientStream, desc protoreflect.MessageDescriptor) string {
	t.Helper()
	message := dynamicpb.NewMessage(desc)
	if err := stream.RecvMsg(message); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	field := desc.Fields().ByName("text")
	if field == nil {
		t.Fatalf("message %s has no text field", desc.FullName())
	}
	return message.Get(field).String()
}

func TestUnaryMatchedCall(t *testing.T) {
	reg, conn, calls := startServer(t)
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
	recorded := calls.List()
	if len(recorded) != 1 {
		t.Fatalf("journal calls = %d, want 1", len(recorded))
	}
	call := recorded[0]
	if call.Method != "/shop.v1.OrderService/GetOrder" || call.StubSource == "" {
		t.Errorf("journal method/source = %q/%q", call.Method, call.StubSource)
	}
	if got := call.Metadata.Get("x-tenant"); len(got) != 1 || got[0] != "acme" {
		t.Errorf("journal metadata x-tenant = %v, want [acme]", got)
	}
	if call.Err != nil || call.Start.IsZero() || call.Duration < 0 {
		t.Errorf("journal status/timing = %v/%v/%v", call.Err, call.Start, call.Duration)
	}
	if len(call.Requests) != 1 || len(call.Responses) != 1 {
		t.Fatalf("journal request/response counts = %d/%d, want 1/1", len(call.Requests), len(call.Responses))
	}
	requestJSON, _ := protojson.Marshal(call.Requests[0])
	responseJSON, _ := protojson.Marshal(call.Responses[0])
	if !strings.Contains(string(requestJSON), "o-123") || !strings.Contains(string(responseJSON), "SHIPPED") {
		t.Errorf("journal request/response = %s/%s", requestJSON, responseJSON)
	}
}

func TestUnaryNoStubMatched(t *testing.T) {
	reg, conn, calls := startServer(t)
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
	recorded := calls.List()
	if len(recorded) != 1 || recorded[0].Err == nil || recorded[0].Err.Code() != codes.NotFound {
		t.Fatalf("journal = %+v, want one NotFound call", recorded)
	}
	if recorded[0].StubSource != "" || len(recorded[0].Requests) != 1 || len(recorded[0].Responses) != 0 {
		t.Errorf("journal unmatched source/requests/responses = %q/%d/%d", recorded[0].StubSource, len(recorded[0].Requests), len(recorded[0].Responses))
	}
}

func TestUnaryStatusIncludesConcretePreconditionFailure(t *testing.T) {
	reg, conn, _ := startServer(t)
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
	reg, conn, _ := startServer(t)
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
	reg, conn, _ := startServer(t)
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

func TestContextErrorPrefersExpiredDeadlineOverTransportCancellation(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := contextError(ctx, context.Canceled)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("contextError = %v, want DeadlineExceeded", err)
	}
}

func TestUnaryRejectsMissingRequestFrame(t *testing.T) {
	reg, conn, _ := startServer(t)
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
	reg, conn, _ := startServer(t)
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

type headerErrorStream struct {
	grpc.ServerStream
	err error
}

func (s headerErrorStream) Context() context.Context     { return context.Background() }
func (s headerErrorStream) SetHeader(metadata.MD) error  { return s.err }
func (s headerErrorStream) SendHeader(metadata.MD) error { return s.err }
func (s headerErrorStream) SetTrailer(metadata.MD)       {}
func (s headerErrorStream) RecvMsg(any) error            { return io.EOF }
func (s headerErrorStream) SendMsg(any) error            { return nil }

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
				"/shop.v1.OrderService/GetOrder", method, match.Input{}, &journal.Call{})
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

func TestNoMatchFormatsSelectionSnapshotTopThreeAndEllipsis(t *testing.T) {
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

	selection := store.SelectOrExplain(full, in)
	if selection.Selected != nil {
		t.Fatalf("SelectOrExplain selected %v, want nearest misses", selection.Selected)
	}
	// A reload between selection and error formatting must not change the
	// registered count attached to this failed-selection diagnostic.
	store.Replace(stubs[:1])
	message := status.Convert(server.noMatch(full, selection.Misses, selection.RegisteredCount)).Message()
	want := `simulacra: no stub matched /shop.v1.OrderService/GetOrder (4 stub(s) registered for this method); first (priority 40): message order_id: expected to equal "o-1"; actual "actual"; second (priority 30): message order_id: expected to equal "o-2"; actual "actual"; third (priority 20): message order_id: expected to equal "o-3"; actual "actual"; …`
	if message != want {
		t.Fatalf("no-match message:\n got: %s\nwant: %s", message, want)
	}
}

func TestUnknownMethodIsUnimplemented(t *testing.T) {
	reg, conn, calls := startServer(t)
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
	recorded := calls.List()
	if len(recorded) != 1 || recorded[0].Method != "/no.such.Service/Nope" || recorded[0].Err == nil || recorded[0].Err.Code() != codes.Unimplemented {
		t.Fatalf("journal = %+v, want one Unimplemented unknown-method call", recorded)
	}
}

type panicTransportStream struct{ method string }

func (s panicTransportStream) Method() string             { return s.method }
func (panicTransportStream) SetHeader(metadata.MD) error  { return nil }
func (panicTransportStream) SendHeader(metadata.MD) error { return nil }
func (panicTransportStream) SetTrailer(metadata.MD) error { return nil }

type contextServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s contextServerStream) Context() context.Context { return s.ctx }

func TestHandleUnknownRecordsInternalStatusAndRepanics(t *testing.T) {
	calls := journal.New(1)
	server := &Server{calls: calls} // nil registry deliberately panics after method extraction.
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), panicTransportStream{
		method: "/shop.v1.OrderService/GetOrder",
	})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = server.handleUnknown(nil, contextServerStream{ctx: ctx})
	}()
	if recovered == nil {
		t.Fatal("handleUnknown did not repanic")
	}
	recorded := calls.List()
	if len(recorded) != 1 || recorded[0].Err == nil || recorded[0].Err.Code() != codes.Internal {
		t.Fatalf("journal = %+v, want one Internal panic call", recorded)
	}
	if recorded[0].Method != "/shop.v1.OrderService/GetOrder" || recorded[0].Duration < 0 {
		t.Errorf("recorded method/duration = %q/%v", recorded[0].Method, recorded[0].Duration)
	}
}

func TestReflectionListsAndResolvesServices(t *testing.T) {
	_, conn, _ := startServer(t)
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
	_, conn, _ := startServer(t)
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
	reg, conn, _ := startServer(t)
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

func TestServerStreamingTopLevelDelayHonorsStreamContext(t *testing.T) {
	const delayedStream = `
- method: shop.v1.OrderService/WatchOrder
  respond:
    delay: 1h
    stream:
      - message: { order_id: "o-123", status: ORDER_STATUS_PENDING }
`

	t.Run("deadline", func(t *testing.T) {
		reg, conn, _ := startServerWithStubs(t, delayedStream)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/WatchOrder")
		sendJSON(t, stream, method.Input(), `{"order_id":"o-123"}`)
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		err := stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
		if got := status.Code(err); got != codes.DeadlineExceeded {
			t.Fatalf("RecvMsg code = %s, want DeadlineExceeded (err %v)", got, err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		reg, conn, _ := startServerWithStubs(t, delayedStream)
		ctx, cancel := context.WithCancel(context.Background())

		stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/WatchOrder")
		sendJSON(t, stream, method.Input(), `{"order_id":"o-123"}`)
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		cancel()
		err := stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
		if got := status.Code(err); got != codes.Canceled {
			t.Fatalf("RecvMsg code = %s, want Canceled (err %v)", got, err)
		}
	})
}

func TestServerStreamingRejectsMissingRequestFrame(t *testing.T) {
	reg, conn, _ := startServer(t)
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
	reg, conn, _ := startServer(t)
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

func TestClientStreamingMatchesAtClose(t *testing.T) {
	reg, conn, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/UploadOrders")
	sendJSON(t, stream, method.Input(), `{"order_id":"o-1"}`)
	sendJSON(t, stream, method.Input(), `{"order_id":"o-2"}`)
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	response := dynamicpb.NewMessage(method.Output())
	if err := stream.RecvMsg(response); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	body, err := protojson.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal response: %v", err)
	}
	if !strings.Contains(string(body), "got 2 orders") {
		t.Fatalf("response = %s, want templated message count", body)
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(method.Output())); err != io.EOF {
		t.Fatalf("second RecvMsg = %v, want EOF", err)
	}
}

func TestClientStreamingMatchesEmptyMessageList(t *testing.T) {
	reg, conn, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/UploadOrders")
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	response := dynamicpb.NewMessage(method.Output())
	if err := stream.RecvMsg(response); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	body, err := protojson.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal response: %v", err)
	}
	if !strings.Contains(string(body), "got 0 orders") {
		t.Fatalf("response = %s, want templated empty message count", body)
	}
}

func TestClientStreamingNoMatchExplainsMessagesExpression(t *testing.T) {
	reg, conn, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/UploadOrders")
	sendJSON(t, stream, method.Input(), `{"order_id":"o-9"}`)
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	err := stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.NotFound {
		t.Fatalf("RecvMsg error = %v, want NotFound", err)
	}
	if !strings.Contains(st.Message(), "size(messages)") {
		t.Fatalf("message = %q, want messages expression nearest miss", st.Message())
	}
}

func TestClientStreamingPreservesReceiveStatusCode(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	method, err := reg.LookupMethod("/shop.v1.OrderService/UploadOrders")
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Canceled, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			receiveErr := status.Error(code, "transport receive failed")
			err := (&Server{}).clientStream(receiveErrorStream{err: receiveErr},
				"/shop.v1.OrderService/UploadOrders", method, match.Input{}, &journal.Call{})
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

func TestBidirectionalStreamingReactiveRules(t *testing.T) {
	reg, conn, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/Chat")
	if got := recvText(t, stream, method.Output()); got != "welcome" {
		t.Fatalf("on_open response = %q, want welcome", got)
	}

	sendJSON(t, stream, method.Input(), `{"text":"ping one"}`)
	if got := recvText(t, stream, method.Output()); got != "pong" {
		t.Fatalf("ping response = %q, want pong", got)
	}
	sendJSON(t, stream, method.Input(), `{"text":"xyz"}`)
	sendJSON(t, stream, method.Input(), `{"text":"abc"}`)
	if got := recvText(t, stream, method.Output()); got != "chatty" {
		t.Fatalf("third-message response = %q, want chatty", got)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(method.Output())); err != io.EOF {
		t.Fatalf("RecvMsg after CloseSend = %v, want EOF", err)
	}
}

func TestBidirectionalStreamingFirstMatchingRuleWins(t *testing.T) {
	reg, conn, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/Chat")
	if got := recvText(t, stream, method.Output()); got != "welcome" {
		t.Fatalf("on_open response = %q, want welcome", got)
	}
	sendJSON(t, stream, method.Input(), `{"text":"one"}`)
	sendJSON(t, stream, method.Input(), `{"text":"two"}`)
	sendJSON(t, stream, method.Input(), `{"text":"ping three"}`)
	if got := recvText(t, stream, method.Output()); got != "pong" {
		t.Fatalf("overlapping-rule response = %q, want first rule's pong", got)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(method.Output())); err != io.EOF {
		t.Fatalf("second overlapping-rule response = %v, want EOF", err)
	}
}

func TestBidirectionalStreamingFlushesHeadersBeforeReceiving(t *testing.T) {
	const rulesOnly = `
- method: shop.v1.OrderService/Chat
  respond:
    metadata: { x-open: ready }
    trailers: { x-final: done }
    rules:
      - match:
          message:
            text: { eq: ping }
        send:
          - message: { text: pong }
`
	reg, conn, _ := startServerWithStubs(t, rulesOnly)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/Chat")
	header, err := stream.Header()
	if err != nil {
		t.Fatalf("Header before sending request: %v", err)
	}
	if got := header.Get("x-open"); len(got) != 1 || got[0] != "ready" {
		t.Fatalf("x-open header = %v, want [ready]", got)
	}
	sendJSON(t, stream, method.Input(), `{"text":"ping"}`)
	if got := recvText(t, stream, method.Output()); got != "pong" {
		t.Fatalf("rule response = %q, want pong", got)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.RecvMsg(dynamicpb.NewMessage(method.Output())); err != io.EOF {
		t.Fatalf("RecvMsg after CloseSend = %v, want EOF", err)
	}
	if got := stream.Trailer().Get("x-final"); len(got) != 1 || got[0] != "done" {
		t.Fatalf("x-final trailer = %v, want [done]", got)
	}
}

func TestBidirectionalStreamingPreservesSendHeaderError(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	method, err := reg.LookupMethod("/shop.v1.OrderService/Chat")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := stub.Compile(reg, stub.Stub{
		Method: "shop.v1.OrderService/Chat",
		Respond: stub.Respond{
			Metadata: map[string]string{"x-open": "ready"},
			Rules: []stub.RuleSpec{{
				Send: []stub.StepSpec{{Message: map[string]any{"text": "unused"}}},
			}},
		},
	}, "chat.yaml#0")
	if err != nil {
		t.Fatal(err)
	}
	headerErr := status.Error(codes.Unavailable, "header transport failed")
	err = (&Server{store: stub.NewStore([]*stub.Compiled{compiled})}).bidi(
		headerErrorStream{err: headerErr}, "/shop.v1.OrderService/Chat", method, match.Input{}, &journal.Call{})
	if err != headerErr {
		t.Fatalf("bidi error = %v, want exact SendHeader error %v", err, headerErr)
	}
}

func TestBidirectionalStreamingSelectsAtOpenAndExplainsMetadataMiss(t *testing.T) {
	const gated = `
- method: shop.v1.OrderService/Chat
  match:
    metadata:
      x-room: { eq: general }
  respond:
    on_open:
      - message: { text: welcome }
`
	reg, conn, _ := startServerWithStubs(t, gated)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, method := openStream(t, reg, conn, ctx, "/shop.v1.OrderService/Chat")
	err := stream.RecvMsg(dynamicpb.NewMessage(method.Output()))
	st := status.Convert(err)
	if st.Code() != codes.NotFound {
		t.Fatalf("RecvMsg error = %v, want NotFound", err)
	}
	if !strings.Contains(st.Message(), "x-room") {
		t.Fatalf("message = %q, want nearest metadata miss", st.Message())
	}
}

func TestBidirectionalStreamingPreservesReceiveStatusCode(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	method, err := reg.LookupMethod("/shop.v1.OrderService/Chat")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := stub.Compile(reg, stub.Stub{
		Method: "shop.v1.OrderService/Chat",
		Respond: stub.Respond{Rules: []stub.RuleSpec{{
			Send: []stub.StepSpec{{Message: map[string]any{"text": "unused"}}},
		}}},
	}, "chat.yaml#0")
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Canceled, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			receiveErr := status.Error(code, "transport receive failed")
			err := (&Server{store: stub.NewStore([]*stub.Compiled{compiled})}).bidi(
				receiveErrorStream{err: receiveErr}, "/shop.v1.OrderService/Chat", method, match.Input{}, &journal.Call{})
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
	if err := runSteps(context.Background(), sender, nil, match.Input{}, &journal.Call{}); err != nil {
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
	err := runSteps(context.Background(), &recordingSender{err: sendErr}, plan.Stream, match.Input{}, &journal.Call{})
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
	err := runSteps(ctx, &recordingSender{}, plan.Stream, match.Input{}, &journal.Call{})
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
	err := runSteps(context.Background(), &recordingSender{}, plan.Stream, match.Input{}, &journal.Call{})
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
	err := runSteps(context.Background(), &recordingSender{}, plan.Stream, match.Input{}, &journal.Call{})
	st := status.Convert(err)
	if st.Code() != codes.Internal || !strings.Contains(st.Message(), "rendering response") {
		t.Fatalf("runSteps error = %v, want Internal rendering response", err)
	}
}
