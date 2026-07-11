package dataplane

import (
	"context"
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

func TestStreamingMethodRejected(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, _ := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	req := dynamicpb.NewMessage(m.Input())
	resp := dynamicpb.NewMessage(m.Output())
	err := conn.Invoke(ctx, "/shop.v1.OrderService/WatchOrder", req, resp)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented for streaming method in M1", err)
	}
	if !strings.Contains(st.Message(), "streaming") {
		t.Errorf("message %q should explain streaming is not supported yet", st.Message())
	}
}
