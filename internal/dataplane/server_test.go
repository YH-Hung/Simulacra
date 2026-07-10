package dataplane

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

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
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return reg, conn
}

func invoke(t *testing.T, reg *schema.Registry, conn *grpc.ClientConn, ctx context.Context, method, reqJSON string) (string, error) {
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
	if err := conn.Invoke(ctx, method, req, resp); err != nil {
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
	if !strings.Contains(st.Message(), "1 stub") {
		t.Errorf("message %q should mention how many stubs exist for the method", st.Message())
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
