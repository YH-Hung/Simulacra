package conformance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/yinghanhung/simulacra/internal/journal"
)

type fakeHealthClient struct {
	response *healthpb.HealthCheckResponse
	err      error
	called   bool
}

func (f *fakeHealthClient) Check(context.Context, *healthpb.HealthCheckRequest, ...grpc.CallOption) (*healthpb.HealthCheckResponse, error) {
	f.called = true
	return f.response, f.err
}

func TestAwaitServingChecksStandardHealthStatus(t *testing.T) {
	client := &fakeHealthClient{response: &healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_NOT_SERVING,
	}}
	err := awaitServing(context.Background(), client)
	if !client.called {
		t.Fatal("health Check was not called")
	}
	if err == nil {
		t.Fatal("awaitServing accepted NOT_SERVING status")
	}
}

func TestWaitForServeRejectsUnexpectedError(t *testing.T) {
	want := errors.New("accept failed")
	done := make(chan error, 1)
	done <- want
	if err := waitForServe(done, time.Second); !errors.Is(err, want) {
		t.Fatalf("waitForServe error = %v, want %v", err, want)
	}
}

func TestWaitForServeAcceptsGRPCStop(t *testing.T) {
	done := make(chan error, 1)
	done <- grpc.ErrServerStopped
	if err := waitForServe(done, time.Second); err != nil {
		t.Fatalf("waitForServe error = %v, want nil", err)
	}
}

func TestWaitForJournalCallObservesDeferredRecord(t *testing.T) {
	calls := journal.New(1)
	release := make(chan struct{})
	go func() {
		<-release
		calls.Record(&journal.Call{Method: "/example.Service/Echo"})
	}()
	close(release)

	call, err := waitForJournalCall(calls, "/example.Service/Echo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "/example.Service/Echo" {
		t.Fatalf("journal method = %q", call.Method)
	}
}
