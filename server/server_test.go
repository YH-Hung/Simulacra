package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestStartServesHealthAndReportsCounts(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 1024,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	if srv.ServiceCount() == 0 {
		t.Fatal("ServiceCount = 0, want registered services (proto schema + health)")
	}
	if got := srv.DataAddr(); got == nil {
		t.Fatal("DataAddr = nil, want a bound listener address")
	}

	conn, err := grpc.NewClient(srv.DataAddr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health status = %v, want SERVING", resp.Status)
	}
}

// TestStartServesStubbedUnaryCall is the behavioral gate for the whole
// refactor: it drives a real gRPC call through a facade-built server and
// asserts the stubbed response and the journal entry. Registry, stub store,
// journal, and data-plane wiring must all be correct for it to pass — a
// health check alone would not catch a mis-wired store or journal.
func TestStartServesStubbedUnaryCall(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: o-123 }
  respond:
    message:
      order_id: o-123
      note: from-facade
`), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 1024,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if got := srv.StubCount(); got != 1 {
		t.Fatalf("StubCount = %d, want 1", got)
	}

	conn, err := grpc.NewClient(srv.DataAddr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	const method = "/shop.v1.OrderService/GetOrder"
	desc, err := srv.reg.LookupMethod(method)
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	req := dynamicpb.NewMessage(desc.Input())
	if err := (protojson.UnmarshalOptions{Resolver: srv.reg.Types()}).Unmarshal([]byte(`{"order_id":"o-123"}`), req); err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp := dynamicpb.NewMessage(desc.Output())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, method, req, resp); err != nil {
		t.Fatalf("Invoke %s: %v", method, err)
	}
	if got := resp.Get(desc.Output().Fields().ByName("note")).String(); got != "from-facade" {
		t.Fatalf("response note = %q, want %q", got, "from-facade")
	}
	if got := srv.journal.Total(); got != 1 {
		t.Fatalf("journal Total = %d, want 1 (journal not wired into the data plane)", got)
	}
}

// TestStopImmediatelyAfterStartReportsCleanStop guards the Start/Stop race:
// Stop can beat the background serve goroutine's first scheduling, in which
// case grpc's Serve returns ErrServerStopped. Wait promises nil after a clean
// stop, so that sentinel must be normalized. The loop makes the losing
// interleaving likely to occur at least once.
func TestStopImmediatelyAfterStartReportsCleanStop(t *testing.T) {
	for i := range 50 {
		srv, err := Start(context.Background(), Options{
			ProtoDirs:   []string{"../testdata/protos"},
			DataAddr:    "127.0.0.1:0",
			JournalSize: 1024,
		})
		if err != nil {
			t.Fatalf("Start #%d: %v", i, err)
		}
		srv.Stop()
		if err := srv.Wait(); err != nil {
			t.Fatalf("Wait after immediate Stop #%d = %v, want nil", i, err)
		}
	}
}

func TestStartRequiresSchemaSource(t *testing.T) {
	_, err := Start(context.Background(), Options{DataAddr: "127.0.0.1:0", JournalSize: 1024})
	if err == nil {
		t.Fatal("Start error = nil, want a schema-source-required error")
	}
}

func TestStartRejectsNonPositiveJournalSize(t *testing.T) {
	_, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 0,
	})
	if err == nil {
		t.Fatal("Start error = nil, want a journal-size error")
	}
}

// TestStartWithWatchHotReloadsOnChange proves the Watch branch end to end:
// watcher launch → filesystem event → debounce → reconcile → store swap →
// StubCount update. Asserting only the initial count would pass even if the
// Watch branch were never wired, so the assertion is on a change made *after*
// the server is running.
func TestStartWithWatchHotReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	const stubBody = `
- method: shop.v1.OrderService/GetOrder
  respond: { message: {} }
`
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(stubBody), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 1024,
		Watch:       true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if got := srv.StubCount(); got != 1 {
		t.Fatalf("StubCount after start = %d, want 1", got)
	}

	// A second stub file must be picked up by the running watcher.
	if err := os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte(stubBody), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := srv.StubCount(); got == 2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("StubCount = %d after 10s, want 2 — hot reload did not run", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartContextCancellationKeepsHotReloadRunning pins the lifetime rule for
// the ctx passed to Start: it scopes startup only. An embedder that starts the
// server under a startup timeout (the SDK's in-process mode does) must not end
// up with a running data plane whose Watch: true watcher has silently died.
func TestStartContextCancellationKeepsHotReloadRunning(t *testing.T) {
	dir := t.TempDir()
	const stubBody = `
- method: shop.v1.OrderService/GetOrder
  respond: { message: {} }
`
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(stubBody), 0o644); err != nil {
		t.Fatal(err)
	}
	startCtx, cancelStart := context.WithCancel(context.Background())
	srv, err := Start(startCtx, Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 1024,
		Watch:       true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	cancelStart() // startup is over; this must not disarm the watcher

	if err := os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte(stubBody), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := srv.StubCount(); got == 2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("StubCount = %d after 10s, want 2 — canceling the Start context stopped hot reload", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStubCountTracksStoreWhileReporterBlocks keeps StubCount consistent with
// the stubs that are actually serving. The reload swaps the store and then
// announces it; a Reporter that is slow to return must not leave StubCount
// reporting the previous generation.
func TestStubCountTracksStoreWhileReporterBlocks(t *testing.T) {
	dir := t.TempDir()
	const stubBody = `
- method: shop.v1.OrderService/GetOrder
  respond: { message: {} }
`
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(stubBody), 0o644); err != nil {
		t.Fatal(err)
	}
	reporter := &blockingReporter{released: make(chan struct{})}
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 1024,
		Watch:       true,
		Reporter:    reporter,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	// Release the reload announcement only once the assertion is done, so
	// Shutdown (which joins in-flight reloads) is never the thing under test.
	defer reporter.release()

	if err := os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte(stubBody), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := srv.StubCount(); got == 2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("StubCount = %d after 10s, want 2 — count trails the store while the reporter blocks", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingReporter holds every Printf until release is called.
type blockingReporter struct {
	released  chan struct{}
	closeOnce sync.Once
}

func (r *blockingReporter) Printf(string, ...any) { <-r.released }
func (r *blockingReporter) PrintErrln(...any)     {}
func (r *blockingReporter) release()              { r.closeOnce.Do(func() { close(r.released) }) }

func TestShutdownWithCanceledContextReturnsPromptly(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 1024,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force path: ctx already done
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within 2s")
	}
}

// Passing the same stub root twice must fail at startup with the
// duplicate-id error — the path that bypassed validation when NewStore took
// stubs directly (design §3.4).
func TestStartRejectsDuplicateStubRoots(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir, dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 16,
	})
	if err == nil {
		t.Fatal("Start with a duplicated stub root succeeded, want duplicate-id error")
	}
	if !strings.Contains(err.Error(), "duplicate stub id") {
		t.Fatalf("err = %v, want a duplicate-stub-id failure", err)
	}
}
