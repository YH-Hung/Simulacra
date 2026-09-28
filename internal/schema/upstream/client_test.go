package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	v1pb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fakeStream is a grpc.ClientStream whose send and receive outcomes are
// dictated by the test. It exists because the ordering that produces a
// send-side io.EOF is a race on a real connection (F2); here it is a
// parameter.
type fakeStream struct {
	grpc.ClientStream
	sendErr error
	recv    *v1pb.ServerReflectionResponse
	recvErr error
	sends   int
}

func (f *fakeStream) SendMsg(any) error { f.sends++; return f.sendErr }

func (f *fakeStream) RecvMsg(m any) error {
	if f.recvErr != nil {
		return f.recvErr
	}
	proto.Merge(m.(*v1pb.ServerReflectionResponse), f.recv)
	return nil
}

func (f *fakeStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeStream) Context() context.Context     { return context.Background() }
func (f *fakeStream) CloseSend() error             { return nil }

func listServicesResponse(names ...string) *v1pb.ServerReflectionResponse {
	var svcs []*v1pb.ServiceResponse
	for _, n := range names {
		svcs = append(svcs, &v1pb.ServiceResponse{Name: n})
	}
	return &v1pb.ServerReflectionResponse{
		MessageResponse: &v1pb.ServerReflectionResponse_ListServicesResponse{
			ListServicesResponse: &v1pb.ListServiceResponse{Service: svcs},
		},
	}
}

// The core regression: a send-side io.EOF means the stream is already done
// and the real status comes from RecvMsg. Treating it as fatal would surface
// a bare io.EOF and skip the fallback against exactly the servers the
// fallback exists for (F2).
func TestFallbackSurvivesSendSideEOF(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sendErr error
	}{
		{"send returns io.EOF (the racy ordering)", io.EOF},
		{"send returns nil (the fast ordering)", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened []string
			streams := map[string]*fakeStream{}
			open := func(_ context.Context, path string) (grpc.ClientStream, error) {
				opened = append(opened, path)
				var s *fakeStream
				if path == PathV1 {
					s = &fakeStream{
						sendErr: tc.sendErr,
						recvErr: status.Error(codes.Unimplemented, "unknown service"),
					}
				} else {
					s = &fakeStream{recv: listServicesResponse("shop.v1.OrderService")}
				}
				streams[path] = s
				return s, nil
			}
			f, release := NewFetcher(open)
			defer release()
			got, err := f.ListServices(context.Background())
			if err != nil {
				t.Fatalf("ListServices: %v", err)
			}
			if len(got) != 1 || got[0] != "shop.v1.OrderService" {
				t.Fatalf("services = %v, want [shop.v1.OrderService]", got)
			}
			want := []string{PathV1, PathV1Alpha}
			if len(opened) != len(want) || opened[0] != want[0] || opened[1] != want[1] {
				t.Fatalf("opened %v, want %v", opened, want)
			}
			// Exactly one send per stream proves the request was genuinely
			// replayed on the v1alpha stream after the v1 rejection, rather
			// than a buffered send being reused or a send being skipped.
			for _, path := range want {
				if s := streams[path]; s.sends != 1 {
					t.Fatalf("stream for %s sent %d time(s), want 1", path, s.sends)
				}
			}
		})
	}
}

// Once a version answers, later calls must not re-probe v1.
func TestNegotiatedVersionIsReused(t *testing.T) {
	var opened []string
	open := func(_ context.Context, path string) (grpc.ClientStream, error) {
		opened = append(opened, path)
		if path == PathV1 {
			return &fakeStream{recvErr: status.Error(codes.Unimplemented, "unknown service")}, nil
		}
		return &fakeStream{recv: listServicesResponse("a.A")}, nil
	}
	f, release := NewFetcher(open)
	defer release()
	for i := 0; i < 3; i++ {
		if _, err := f.ListServices(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	var v1Probes int
	for _, p := range opened {
		if p == PathV1 {
			v1Probes++
		}
	}
	if v1Probes != 1 {
		t.Fatalf("probed v1 %d times, want 1; opened %v", v1Probes, opened)
	}
}

// Reflection reports a missing file in-band as an ErrorResponse, not as a
// stream error. It must not be mistaken for a successful empty result.
func TestInBandErrorResponseIsAnError(t *testing.T) {
	open := func(context.Context, string) (grpc.ClientStream, error) {
		return &fakeStream{recv: &v1pb.ServerReflectionResponse{
			MessageResponse: &v1pb.ServerReflectionResponse_ErrorResponse{
				ErrorResponse: &v1pb.ErrorResponse{
					ErrorCode:    int32(codes.NotFound),
					ErrorMessage: "file not found",
				},
			},
		}}, nil
	}
	f, release := NewFetcher(open)
	defer release()
	_, err := f.FileByFilename(context.Background(), "gone.proto")
	if err == nil {
		t.Fatal("FileByFilename succeeded on an ErrorResponse, want an error")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", status.Code(err))
	}
}

// A non-EOF send failure is a real transport failure and must not be
// swallowed into a fallback attempt.
func TestNonEOFSendErrorIsFatal(t *testing.T) {
	boom := errors.New("connection reset")
	open := func(context.Context, string) (grpc.ClientStream, error) {
		return &fakeStream{sendErr: boom}, nil
	}
	f, release := NewFetcher(open)
	defer release()
	_, err := f.ListServices(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the transport failure", err)
	}
}

func TestFileContainingSymbolReturnsDescriptorBytes(t *testing.T) {
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("a.proto")}
	raw, err := proto.Marshal(fd)
	if err != nil {
		t.Fatal(err)
	}
	open := func(context.Context, string) (grpc.ClientStream, error) {
		return &fakeStream{recv: &v1pb.ServerReflectionResponse{
			MessageResponse: &v1pb.ServerReflectionResponse_FileDescriptorResponse{
				FileDescriptorResponse: &v1pb.FileDescriptorResponse{
					FileDescriptorProto: [][]byte{raw},
				},
			},
		}}, nil
	}
	f, release := NewFetcher(open)
	defer release()
	got, err := f.FileContainingSymbol(context.Background(), "a.A")
	if err != nil {
		t.Fatalf("FileContainingSymbol: %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(raw) {
		t.Fatalf("got %d descriptor(s), want the one served", len(got))
	}
}

// fileDescriptorResponse wraps raw descriptor bytes the way a real
// FileContainingSymbol/FileByFilename response does.
func fileDescriptorResponse(raw ...[]byte) *v1pb.ServerReflectionResponse {
	return &v1pb.ServerReflectionResponse{
		MessageResponse: &v1pb.ServerReflectionResponse_FileDescriptorResponse{
			FileDescriptorResponse: &v1pb.FileDescriptorResponse{
				FileDescriptorProto: raw,
			},
		},
	}
}

// scriptedStream answers a fixed sequence of RecvMsg outcomes in order, one
// per call, so a test can simulate several round trips over one retained
// stream. fakeStream cannot do this: every RecvMsg on it returns the same
// canned outcome, which is right for one round trip but not for proving a
// stream survives several.
type scriptedStream struct {
	grpc.ClientStream
	responses []*v1pb.ServerReflectionResponse
	sends     int
	recvs     int
}

func (s *scriptedStream) SendMsg(any) error { s.sends++; return nil }

func (s *scriptedStream) RecvMsg(m any) error {
	if s.recvs >= len(s.responses) {
		return fmt.Errorf("scriptedStream: RecvMsg called an unscripted %d(th) time", s.recvs+1)
	}
	proto.Merge(m.(*v1pb.ServerReflectionResponse), s.responses[s.recvs])
	s.recvs++
	return nil
}

func (s *scriptedStream) Header() (metadata.MD, error) { return nil, nil }
func (s *scriptedStream) Context() context.Context     { return context.Background() }
func (s *scriptedStream) CloseSend() error             { return nil }

// The core correctness property of item 1 (design §5): the whole walk must
// go over one negotiated stream, not a new one per request. Behind an L7
// proxy, a new stream per request can be routed to a different backend
// instance; mixing descriptors from two backends is what this retains a
// single stream to prevent.
//
// Mutation check: reverting roundTrip to open a fresh stream per call (the
// pre-fix behavior) makes this fail with opens == 3, one per request below.
func TestClosureUsesOneStreamForTheWholeWalk(t *testing.T) {
	orderRaw, err := proto.Marshal(file("order.proto", "ts.proto"))
	if err != nil {
		t.Fatal(err)
	}
	tsRaw, err := proto.Marshal(file("ts.proto"))
	if err != nil {
		t.Fatal(err)
	}
	strm := &scriptedStream{responses: []*v1pb.ServerReflectionResponse{
		listServicesResponse("shop.v1.OrderService"), // ListServices
		fileDescriptorResponse(orderRaw),             // FileContainingSymbol
		fileDescriptorResponse(tsRaw),                // FileByFilename(ts.proto)
	}}
	var opens int
	open := func(_ context.Context, path string) (grpc.ClientStream, error) {
		opens++
		if path != PathV1 {
			t.Fatalf("opened %s, want the v1 path negotiated on the first call", path)
		}
		return strm, nil
	}
	f, release := NewFetcher(open)
	defer release()

	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	if got := names(set); len(got) != 2 {
		t.Fatalf("got %v, want both order.proto and ts.proto", got)
	}
	if opens != 1 {
		t.Fatalf("opened %d stream(s), want exactly 1: the whole walk must reuse the negotiated stream", opens)
	}
	if strm.sends != 3 {
		t.Fatalf("sent %d request(s) on the retained stream, want 3 (ListServices, FileContainingSymbol, FileByFilename)", strm.sends)
	}
}

// The same property, but through the v1alpha fallback: the rejected v1
// probe is one legitimate extra open, and the retained v1alpha stream must
// still absorb every later request with no further opens.
//
// Mutation check: reverting roundTrip to open a fresh stream per call makes
// this fail with opens == 3 (the v1 probe, plus a fresh open for each of the
// two v1alpha round trips below) instead of 2.
func TestClosureUsesOneStreamAfterFallback(t *testing.T) {
	orderRaw, err := proto.Marshal(file("order.proto"))
	if err != nil {
		t.Fatal(err)
	}
	strm := &scriptedStream{responses: []*v1pb.ServerReflectionResponse{
		listServicesResponse("shop.v1.OrderService"), // ListServices
		fileDescriptorResponse(orderRaw),             // FileContainingSymbol
	}}
	var opens int
	open := func(_ context.Context, path string) (grpc.ClientStream, error) {
		opens++
		if path == PathV1 {
			return &fakeStream{recvErr: status.Error(codes.Unimplemented, "unknown service")}, nil
		}
		return strm, nil
	}
	f, release := NewFetcher(open)
	defer release()

	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	if got := names(set); len(got) != 1 || got[0] != "order.proto" {
		t.Fatalf("got %v, want [order.proto]", got)
	}
	if opens != 2 {
		t.Fatalf("opened %d stream(s), want 2 (the rejected v1 probe, then the retained v1alpha stream)", opens)
	}
	if strm.sends != 2 {
		t.Fatalf("sent %d request(s) on the retained stream, want 2", strm.sends)
	}
}

// A failure on the retained stream, once negotiated, must surface directly
// rather than trigger a silent re-open -- re-opening would reintroduce the
// exact routing hazard retaining one stream removes.
//
// Mutation check: making roundTrip re-open and retry on any post-negotiation
// error makes this fail (opens > 1, or the error swallowed).
func TestMidWalkStreamFailureIsNotRetried(t *testing.T) {
	strm := &scriptedStream{responses: []*v1pb.ServerReflectionResponse{
		listServicesResponse("a.A"),
	}}
	var opens int
	open := func(context.Context, string) (grpc.ClientStream, error) {
		opens++
		return strm, nil
	}
	f, release := NewFetcher(open)
	defer release()

	if _, err := f.ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	// The stream has no more scripted responses, so its next RecvMsg errors
	// -- standing in for a real mid-walk transport failure.
	_, err := f.FileContainingSymbol(context.Background(), "a.A")
	if err == nil {
		t.Fatal("FileContainingSymbol succeeded after the stream ran out, want its error surfaced")
	}
	if opens != 1 {
		t.Fatalf("opened %d stream(s) after a mid-walk failure, want 1: a failure must not trigger a silent re-open", opens)
	}
}

// release must actually free the retained stream's resources -- cancel its
// context -- deterministically, rather than leaving that to process exit.
//
// Mutation check: making release a no-op makes this fail, since the
// captured stream context would never report itself done.
func TestReleaseCancelsTheRetainedStream(t *testing.T) {
	var streamCtx context.Context
	open := func(ctx context.Context, _ string) (grpc.ClientStream, error) {
		streamCtx = ctx
		return &fakeStream{recv: listServicesResponse("a.A")}, nil
	}
	f, release := NewFetcher(open)
	if _, err := f.ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if err := streamCtx.Err(); err != nil {
		t.Fatalf("the stream's context is already done before release: %v", err)
	}
	release()
	if err := streamCtx.Err(); err == nil {
		t.Fatal("release did not cancel the retained stream's context")
	}
}
