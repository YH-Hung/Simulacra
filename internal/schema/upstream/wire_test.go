package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	v1pb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphapb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

// reflectionVersions selects which reflection services a test server serves.
type reflectionVersions int

const (
	bothVersions reflectionVersions = iota
	v1AlphaOnly
)

// startReflectingServer starts a stock grpc-go server. Simulacra registers
// both versions (F3), so only a server we build ourselves can exercise the
// v1alpha fallback.
func startReflectingServer(t *testing.T, versions reflectionVersions) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	healthpb.RegisterHealthServer(s, health.NewServer())
	opts := reflection.ServerOptions{Services: s}
	// NewServer returns the v1alpha implementation; NewServerV1 the v1 one.
	v1alphapb.RegisterServerReflectionServer(s, reflection.NewServer(opts))
	if versions == bothVersions {
		v1pb.RegisterServerReflectionServer(s, reflection.NewServerV1(opts))
	}
	go func() { _ = s.Serve(lis) }()

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close(); s.Stop() })
	return cc
}

func TestWireListServicesOverV1(t *testing.T) {
	cc := startReflectingServer(t, bothVersions)
	f, release := NewFetcher(OpenerFor(cc))
	defer release()
	got, err := f.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !slices.Contains(got, "grpc.health.v1.Health") {
		t.Fatalf("services = %v, want health among them", got)
	}
}

// The fallback, against a server that speaks only v1alpha.
func TestWireFallsBackToV1Alpha(t *testing.T) {
	cc := startReflectingServer(t, v1AlphaOnly)
	f, release := NewFetcher(OpenerFor(cc))
	defer release()
	got, err := f.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !slices.Contains(got, "grpc.health.v1.Health") {
		t.Fatalf("services = %v, want health among them", got)
	}
	if p := f.(*reflectFetcher).path; p != PathV1Alpha {
		t.Fatalf("negotiated %q, want the v1alpha path", p)
	}
}

// The same fallback, with the racy ordering forced. Header() waits for the
// terminal status and then deliberately returns (nil, nil) -- grpc-go calls
// cs.finish(err) and comments "Do not return the error. The user should get
// it by calling Recv()" (stream.go:875-878). So after Header() returns, the
// stream is done and the next SendMsg is io.EOF, with no sleep anywhere (F7).
//
// This asserts only that the fallback SUCCEEDS. It deliberately does not
// assert which send outcome occurred: Header()-finishes-before-returning is a
// grpc-go implementation detail, not a documented contract, and pinning it
// here would turn a library change into a false alarm. The send outcomes
// themselves are covered at the seam in client_test.go.
func TestWireFallsBackWhenTheRejectionArrivesFirst(t *testing.T) {
	cc := startReflectingServer(t, v1AlphaOnly)

	var sawEOF bool
	open := func(ctx context.Context, path string) (grpc.ClientStream, error) {
		strm, err := cc.NewStream(ctx, streamDesc, path)
		if err != nil {
			return nil, err
		}
		if path == PathV1 {
			// THE RENDEZVOUS: block until the rejection has arrived.
			_, _ = strm.Header()
		}
		return &recordingStream{ClientStream: strm, sawEOF: &sawEOF}, nil
	}

	f, release := NewFetcher(open)
	defer release()
	got, err := f.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !slices.Contains(got, "grpc.health.v1.Health") {
		t.Fatalf("services = %v, want health among them", got)
	}
	// Informational: records that the ordering really was the racy one, without
	// failing the test if grpc-go ever changes when Header() returns.
	t.Logf("send-side io.EOF observed on the rejected v1 stream: %v", sawEOF)
}

// recordingStream notes whether SendMsg returned io.EOF.
type recordingStream struct {
	grpc.ClientStream
	sawEOF *bool
}

func (r *recordingStream) SendMsg(m any) error {
	err := r.ClientStream.SendMsg(m)
	if errors.Is(err, io.EOF) {
		*r.sawEOF = true
	}
	return err
}

// The skip rule against a real ListServices response (F5), not a
// self-containment test: a stock grpc-go server advertises its own two
// reflection services, which Simulacra's data plane does not (F5), so only a
// genuine upstream -- not a fake Fetcher -- can exercise this half of the
// skip list. Nothing here asserts anything about dependency closure; the set
// is empty because everything this server advertises (health plus its own
// reflection) is skipped.
func TestWireSkipListDropsEveryServiceARealServerAdvertises(t *testing.T) {
	cc := startReflectingServer(t, bothVersions)
	f, release := NewFetcher(OpenerFor(cc))
	defer release()
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	if n := len(set.GetFile()); n != 0 {
		t.Fatalf("got %d file(s) %v, want none: every service this server "+
			"exposes is one the data plane implements itself", n, set.GetFile())
	}
}
