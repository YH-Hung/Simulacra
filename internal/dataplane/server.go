// Package dataplane serves the mock gRPC endpoint: a single grpc-go server
// whose UnknownServiceHandler dispatches every method of every registered
// schema dynamically — no code generation, no per-service registration.
package dataplane

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

type Server struct {
	reg   *schema.Registry
	store *stub.Store
	calls *journal.Journal
	grpc  *grpc.Server
}

func New(reg *schema.Registry, store *stub.Store, calls *journal.Journal) (*Server, error) {
	s := &Server{reg: reg, store: store, calls: calls}
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

func (s *Server) handleUnknown(_ any, stream grpc.ServerStream) (err error) {
	call := &journal.Call{Start: time.Now()}
	defer func() {
		panicked := recover()
		call.Duration = time.Since(call.Start)
		if panicked != nil {
			call.Err = status.New(codes.Internal, fmt.Sprintf("simulacra: panic serving call: %v", panicked))
		} else if err != nil {
			call.Err = status.Convert(err)
		}
		if s.calls != nil {
			s.calls.Record(call)
		}
		if panicked != nil {
			panic(panicked)
		}
	}()
	md, _ := metadata.FromIncomingContext(stream.Context())
	call.Metadata = md.Copy()

	full, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "simulacra: no method name on stream")
	}
	call.Method = full
	m, err := s.reg.LookupMethod(full)
	if err != nil {
		return status.Errorf(codes.Unimplemented, "simulacra: %v", err)
	}
	in := match.Input{Method: strings.TrimPrefix(full, "/"), Metadata: md}

	switch match.ShapeOf(m) {
	case match.Unary:
		return s.unary(stream, full, m, in, call)
	case match.ServerStream:
		return s.serverStream(stream, full, m, in, call)
	case match.ClientStream:
		return s.clientStream(stream, full, m, in, call)
	case match.Bidi:
		return s.bidi(stream, full, m, in, call)
	default:
		return status.Errorf(codes.Internal, "simulacra: unsupported method shape for %s", full)
	}
}

func (s *Server) bidi(stream grpc.ServerStream, full string, method protoreflect.MethodDescriptor, in match.Input, call *journal.Call) error {
	if in.Messages == nil {
		in.Messages = []protoreflect.Message{}
	}
	selected, misses := s.store.SelectOrExplain(full, in)
	if selected == nil {
		return s.noMatch(full, misses)
	}
	call.StubSource = selected.Source
	plan := selected.Plan()
	if len(plan.Trailer) > 0 {
		stream.SetTrailer(plan.Trailer)
	}
	if len(plan.Header) > 0 {
		if err := stream.SendHeader(plan.Header); err != nil {
			return err
		}
	}
	if err := runSteps(stream.Context(), stream, plan.OnOpen, in, call); err != nil {
		return err
	}

	for {
		message := dynamicpb.NewMessage(method.Input())
		err := stream.RecvMsg(message)
		if err == io.EOF {
			if plan.OnClose == nil {
				return nil
			}
			return plan.OnClose.Err()
		}
		if err != nil {
			return receiveError(err)
		}
		call.Requests = append(call.Requests, message)
		in.Message = message.ProtoReflect()
		in.Messages = append(in.Messages, in.Message)
		for _, rule := range plan.Rules {
			if !rule.Matcher.Eval(in) {
				continue
			}
			if err := runSteps(stream.Context(), stream, rule.Send, in, call); err != nil {
				return err
			}
			break
		}
	}
}

func (s *Server) clientStream(stream grpc.ServerStream, full string, method protoreflect.MethodDescriptor, in match.Input, call *journal.Call) error {
	if in.Messages == nil {
		in.Messages = []protoreflect.Message{}
	}
	for {
		message := dynamicpb.NewMessage(method.Input())
		err := stream.RecvMsg(message)
		if err == io.EOF {
			break
		}
		if err != nil {
			return receiveError(err)
		}
		call.Requests = append(call.Requests, message)
		in.Messages = append(in.Messages, message.ProtoReflect())
	}

	selected, misses := s.store.SelectOrExplain(full, in)
	if selected == nil {
		return s.noMatch(full, misses)
	}
	call.StubSource = selected.Source
	if err := applyMetadata(stream, selected.Plan()); err != nil {
		return err
	}
	return sendSingle(stream, selected.Plan(), in, call)
}

func (s *Server) unary(stream grpc.ServerStream, full string, method protoreflect.MethodDescriptor, in match.Input, call *journal.Call) error {
	req, err := receiveSingleRequest(stream, method, "unary", call)
	if err != nil {
		return err
	}
	in.Message = req.ProtoReflect()
	selected, misses := s.store.SelectOrExplain(full, in)
	if selected == nil {
		return s.noMatch(full, misses)
	}
	call.StubSource = selected.Source
	if err := applyMetadata(stream, selected.Plan()); err != nil {
		return err
	}
	return sendSingle(stream, selected.Plan(), in, call)
}

func (s *Server) serverStream(stream grpc.ServerStream, full string, method protoreflect.MethodDescriptor, in match.Input, call *journal.Call) error {
	req, err := receiveSingleRequest(stream, method, "server-streaming", call)
	if err != nil {
		return err
	}
	in.Message = req.ProtoReflect()
	selected, misses := s.store.SelectOrExplain(full, in)
	if selected == nil {
		return s.noMatch(full, misses)
	}
	call.StubSource = selected.Source
	plan := selected.Plan()
	if err := applyMetadata(stream, plan); err != nil {
		return err
	}
	if plan.Delay != nil {
		if err := plan.Delay.Wait(stream.Context()); err != nil {
			return status.FromContextError(err).Err()
		}
	}
	return runSteps(stream.Context(), stream, plan.Stream, in, call)
}

func receiveSingleRequest(stream grpc.ServerStream, method protoreflect.MethodDescriptor, shape string, call *journal.Call) (*dynamicpb.Message, error) {
	req := dynamicpb.NewMessage(method.Input())
	if err := stream.RecvMsg(req); err != nil {
		if err == io.EOF {
			return nil, status.Errorf(codes.Internal, "simulacra: missing request for %s RPC", shape)
		}
		return nil, receiveError(err)
	}
	call.Requests = append(call.Requests, req)
	extra := dynamicpb.NewMessage(method.Input())
	if err := stream.RecvMsg(extra); err != io.EOF {
		if err == nil {
			call.Requests = append(call.Requests, extra)
			return nil, status.Errorf(codes.Internal, "simulacra: %s request cardinality violation: received more than one request", shape)
		}
		return nil, receiveError(err)
	}
	return req, nil
}

func receiveError(err error) error {
	return status.Errorf(status.Convert(err).Code(), "simulacra: receiving request: %v", err)
}

func applyMetadata(stream grpc.ServerStream, plan *stub.Plan) error {
	if len(plan.Header) > 0 {
		if err := stream.SetHeader(plan.Header); err != nil {
			return status.Errorf(codes.Internal, "simulacra: setting response headers: %v", err)
		}
	}
	if len(plan.Trailer) > 0 {
		stream.SetTrailer(plan.Trailer)
	}
	return nil
}

func sendSingle(stream grpc.ServerStream, plan *stub.Plan, in match.Input, call *journal.Call) error {
	if plan.Delay != nil {
		if err := plan.Delay.Wait(stream.Context()); err != nil {
			return status.FromContextError(err).Err()
		}
	}
	if plan.Status != nil {
		return plan.Status.Err()
	}
	response, err := plan.Message.Render(in)
	if err != nil {
		return status.Errorf(codes.Internal, "simulacra: rendering response: %v", err)
	}
	// Responses are rendered attempts, retained even if the transport send fails.
	call.Responses = append(call.Responses, response)
	return stream.SendMsg(response)
}

type messageSender interface {
	SendMsg(any) error
}

func runSteps(ctx context.Context, sender messageSender, steps []stub.Step, in match.Input, call *journal.Call) error {
	for _, step := range steps {
		if step.Delay != nil {
			if err := step.Delay.Wait(ctx); err != nil {
				return status.FromContextError(err).Err()
			}
		}
		if step.Status != nil {
			return step.Status.Err()
		}
		message, err := step.Message.Render(in)
		if err != nil {
			return status.Errorf(codes.Internal, "simulacra: rendering response: %v", err)
		}
		// Responses are rendered attempts, retained even if the transport send fails.
		call.Responses = append(call.Responses, message)
		if err := sender.SendMsg(message); err != nil {
			return err
		}
	}
	return nil
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
