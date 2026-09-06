package admin_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// controlClient serves deps over HTTP/1.1 and returns a generated client for
// it, plus the server so a test can inspect state afterwards.
func controlClient(t *testing.T, deps admin.Deps) (adminv1connect.ControlServiceClient, *httptest.Server) {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewControlServiceClient(ts.Client(), ts.URL), ts
}

// loadStubsInto compiles the given stub YAML into deps.Store as file-origin
// stubs, the same way server.Start does.
func loadStubsInto(t *testing.T, deps admin.Deps, yaml string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stubs.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write stubs: %v", err)
	}
	compiled, errs := stub.LoadDirs(deps.Registry, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("LoadDirs: %v", errs)
	}
	if _, err := deps.Store.ReplaceOrigin(stub.OriginFile, compiled); err != nil {
		t.Fatalf("ReplaceOrigin: %v", err)
	}
}

func TestGetServerInfoReportsVersionAddressesAndCounts(t *testing.T) {
	deps := testDeps(t)
	deps.Version = "v-under-test"
	deps.DataAddr = func() string { return "127.0.0.1:11111" }
	deps.AdminAddr = func() string { return "127.0.0.1:22222" }
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: one }
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: two }
`)
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})

	client, _ := controlClient(t, deps)
	resp, err := client.GetServerInfo(context.Background(),
		connect.NewRequest(&adminv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
	got := resp.Msg
	if got.Version != "v-under-test" {
		t.Errorf("version = %q, want %q", got.Version, "v-under-test")
	}
	if got.DataAddr != "127.0.0.1:11111" {
		t.Errorf("data_addr = %q, want %q", got.DataAddr, "127.0.0.1:11111")
	}
	if got.AdminAddr != "127.0.0.1:22222" {
		t.Errorf("admin_addr = %q, want %q", got.AdminAddr, "127.0.0.1:22222")
	}
	// testdata/protos declares exactly one service, shop.v1.OrderService.
	if got.ServiceCount != 1 {
		t.Errorf("service_count = %d, want 1", got.ServiceCount)
	}
	if got.StubCount != 2 {
		t.Errorf("stub_count = %d, want 2", got.StubCount)
	}
	if got.JournalCount != 1 {
		t.Errorf("journal_count = %d, want 1", got.JournalCount)
	}
	// testDeps builds the journal with journal.New(4).
	if got.JournalCapacity != 4 {
		t.Errorf("journal_capacity = %d, want 4", got.JournalCapacity)
	}
}

// The addresses are read per request, so a listener bound after the handler was
// built is still reported correctly — the whole reason they are funcs.
func TestGetServerInfoResolvesAddressesPerRequest(t *testing.T) {
	deps := testDeps(t)
	addr := "unbound"
	deps.AdminAddr = func() string { return addr }
	client, _ := controlClient(t, deps)

	first, err := client.GetServerInfo(context.Background(),
		connect.NewRequest(&adminv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
	if first.Msg.AdminAddr != "unbound" {
		t.Fatalf("first admin_addr = %q, want %q", first.Msg.AdminAddr, "unbound")
	}

	addr = "127.0.0.1:33333"
	second, err := client.GetServerInfo(context.Background(),
		connect.NewRequest(&adminv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
	if second.Msg.AdminAddr != "127.0.0.1:33333" {
		t.Fatalf("second admin_addr = %q, want the rebound address", second.Msg.AdminAddr)
	}
}

// ptr is a local helper for the presence-tracked bools in ResetRequest.
func ptr[T any](v T) *T { return &v }

// resetStubYAML is the file-origin half of the fixture; compileAPIStub supplies
// the API-origin half, so ResetStubs has something to drop and something to
// keep.
const resetStubYAML = `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: file-origin }
`

func TestResetHonorsPresenceRule(t *testing.T) {
	cases := []struct {
		name        string
		req         *adminv1.ResetRequest
		wantStubs   int // expected Store.Len() after Reset
		wantJournal int // expected Journal.Len() after Reset
	}{
		{
			// Omitted means reset, for both fields.
			name:        "both omitted",
			req:         &adminv1.ResetRequest{},
			wantStubs:   1, // the file-origin stub survives; the API one is dropped
			wantJournal: 0,
		},
		{
			name:        "both explicitly true",
			req:         &adminv1.ResetRequest{Stubs: ptr(true), Journal: ptr(true)},
			wantStubs:   1,
			wantJournal: 0,
		},
		{
			name:        "both explicitly false",
			req:         &adminv1.ResetRequest{Stubs: ptr(false), Journal: ptr(false)},
			wantStubs:   2, // nothing dropped
			wantJournal: 1,
		},
		{
			name:        "stubs false, journal omitted",
			req:         &adminv1.ResetRequest{Stubs: ptr(false)},
			wantStubs:   2,
			wantJournal: 0,
		},
		{
			name:        "journal false, stubs omitted",
			req:         &adminv1.ResetRequest{Journal: ptr(false)},
			wantStubs:   1,
			wantJournal: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			loadStubsInto(t, deps, resetStubYAML)
			// Store.Add always installs an API-origin stub — it stamps
			// Origin, Source and the id itself — which is exactly the
			// entry ResetStubs must drop.
			deps.Store.Add(compileAPIStub(t, deps))
			deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
			if got := deps.Store.Len(); got != 2 {
				t.Fatalf("precondition: Store.Len() = %d, want 2", got)
			}
			if got := deps.Journal.Len(); got != 1 {
				t.Fatalf("precondition: Journal.Len() = %d, want 1", got)
			}

			client, _ := controlClient(t, deps)
			if _, err := client.Reset(context.Background(), connect.NewRequest(tc.req)); err != nil {
				t.Fatalf("Reset: %v", err)
			}
			if got := deps.Store.Len(); got != tc.wantStubs {
				t.Errorf("Store.Len() = %d, want %d", got, tc.wantStubs)
			}
			if got := deps.Journal.Len(); got != tc.wantJournal {
				t.Errorf("Journal.Len() = %d, want %d", got, tc.wantJournal)
			}
		})
	}
}

// compileAPIStub compiles a single stub the way an API caller's stub would be
// compiled, so ResetStubs has an API-origin entry to drop.
func compileAPIStub(t *testing.T, deps admin.Deps) *stub.Compiled {
	t.Helper()
	dir := t.TempDir()
	const body = `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: api-origin }
`
	if err := os.WriteFile(filepath.Join(dir, "api.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write api stub: %v", err)
	}
	compiled, errs := stub.LoadDirs(deps.Registry, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("LoadDirs: %v", errs)
	}
	if len(compiled) != 1 {
		t.Fatalf("compiled %d stubs, want 1", len(compiled))
	}
	return compiled[0]
}

func TestShutdownInvokesCallbackOnceAndResponds(t *testing.T) {
	deps := testDeps(t)
	calls := make(chan struct{}, 8)
	deps.Shutdown = func() { calls <- struct{}{} }

	client, _ := controlClient(t, deps)
	if _, err := client.Shutdown(context.Background(),
		connect.NewRequest(&adminv1.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := len(calls); got != 1 {
		t.Fatalf("Deps.Shutdown invoked %d times, want exactly 1", got)
	}
}

// The handler must return while the teardown it triggered is still running:
// server teardown waits for this very response to flush, so a handler that
// waited for teardown to finish would deadlock. Deps.Shutdown stands in for the
// real server.begin — it starts the coordinator on its own goroutine and
// returns immediately — with that coordinator parked, so the response can only
// arrive if the handler did not wait for it.
func TestShutdownRespondsWhileTeardownIsStillRunning(t *testing.T) {
	deps := testDeps(t)
	teardownRunning := make(chan struct{})
	releaseTeardown := make(chan struct{})
	deps.Shutdown = func() {
		go func() {
			close(teardownRunning)
			<-releaseTeardown
		}()
	}
	t.Cleanup(func() { close(releaseTeardown) })

	client, _ := controlClient(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown RPC: %v", err)
	}

	select {
	case <-teardownRunning:
	case <-time.After(3 * time.Second):
		t.Fatal("Deps.Shutdown never started teardown")
	}
	select {
	case <-releaseTeardown:
		t.Fatal("teardown finished before the assertion; the test proves nothing")
	default:
	}
}
