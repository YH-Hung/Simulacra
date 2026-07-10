// Package dataplane serves the mock gRPC endpoint: a single grpc-go server
// whose UnknownServiceHandler dispatches every method of every registered
// schema dynamically — no code generation, no per-service registration.
package dataplane

import (
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

type Server struct {
	reg   *schema.Registry
	store *stub.Store
	grpc  *grpc.Server
}

func New(reg *schema.Registry, store *stub.Store) (*Server, error) {
	s := &Server{reg: reg, store: store}
	s.grpc = grpc.NewServer(grpc.UnknownServiceHandler(s.handleUnknown))

	// Health: standard grpc.health.v1 protocol, always SERVING in M1.
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s.grpc, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	// Make the health service resolvable through our reflection resolver
	// so `grpcurl ... grpc.health.v1.Health/Check` works against the mock.
	if err := reg.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		return nil, err
	}

	// Reflection over the dynamic registry: v1 and v1alpha (grpcurl uses both).
	opts := reflection.ServerOptions{
		Services:           s,
		DescriptorResolver: reg.Files(),
	}
	v1reflectionpb.RegisterServerReflectionServer(s.grpc, reflection.NewServerV1(opts))
	v1alphareflectionpb.RegisterServerReflectionServer(s.grpc, reflection.NewServer(opts))
	return s, nil
}

// GetServiceInfo implements reflection.ServiceInfoProvider by listing every
// service in the schema registry (mocked services plus health).
func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	out := make(map[string]grpc.ServiceInfo)
	for _, svc := range s.reg.Services() {
		out[string(svc.FullName())] = grpc.ServiceInfo{Metadata: svc.ParentFile().Path()}
	}
	return out
}

func (s *Server) Serve(lis net.Listener) error { return s.grpc.Serve(lis) }

// GracefulStop waits for in-flight RPCs — including open streams — so it
// can block indefinitely. Callers that need a bound must fall back to Stop.
func (s *Server) GracefulStop() { s.grpc.GracefulStop() }

// Stop aborts all connections immediately.
func (s *Server) Stop() { s.grpc.Stop() }

func (s *Server) handleUnknown(_ any, stream grpc.ServerStream) error {
	full, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "simulacra: no method name on stream")
	}
	m, err := s.reg.LookupMethod(full)
	if err != nil {
		return status.Errorf(codes.Unimplemented, "simulacra: %v", err)
	}
	if m.IsStreamingClient() || m.IsStreamingServer() {
		return status.Errorf(codes.Unimplemented,
			"simulacra: %s is a streaming method; this build supports unary methods only (streaming lands in M2)", full)
	}

	req := dynamicpb.NewMessage(m.Input())
	if err := stream.RecvMsg(req); err != nil {
		return status.Errorf(codes.Internal, "simulacra: receiving request: %v", err)
	}
	md, _ := metadata.FromIncomingContext(stream.Context())

	in := match.Input{Method: strings.TrimPrefix(full, "/"), Metadata: md, Message: req.ProtoReflect()}
	selected, misses := s.store.SelectOrExplain(full, in)
	if selected == nil {
		return s.noMatch(full, misses)
	}
	return stream.SendMsg(selected.Response())
}

func (s *Server) noMatch(full string, misses []stub.Miss) error {
	message := fmt.Sprintf(
		"simulacra: no stub matched %s (%d stub(s) registered for this method)",
		full, s.store.CountFor(full),
	)
	limit := len(misses)
	if limit > 3 {
		limit = 3
	}
	for _, miss := range misses[:limit] {
		message += fmt.Sprintf("; %s (priority %d): %s", miss.Source, miss.Priority, strings.Join(miss.Reasons, "; "))
	}
	if len(misses) > limit {
		message += "; …"
	}
	return status.Error(codes.NotFound, message)
}
