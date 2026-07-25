# Simulacra M3 Phase 1 — Server Facade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract `simulacra serve`'s hand-wired registry/store/journal/data-plane/watcher/shutdown plumbing into a new public `server/` package (`server.Start` → `*server.Server`), and refactor `serve` onto it with **no behavior change** — establishing the single wiring path that the admin plane (Phase 4), the Go SDK's in-process mode (Phase 7), and tests will all reuse.

**Architecture:** A new root-level `server` package owns the instance lifecycle. `Start(ctx, Options)` builds the schema registry from proto dirs / descriptor sets, loads and compiles stubs, constructs the `dataplane` gRPC server, binds the data listener, serves in a background goroutine, and (when `Watch`) runs the existing hot-reload watcher. `Server` exposes `DataAddr()`, `ServiceCount()`, `StubCount()`, `Wait()`, `GracefulStop()`, `Stop()`, and `Shutdown(ctx)`. `internal/cli/serve.go` maps flags → `server.Options`, prints the startup banner via accessors, and keeps its signal handling (`shutdown.go` unchanged) using `srv.GracefulStop`/`srv.Stop` as the graceful/force closures. The watcher plumbing and its tests move out of `internal/cli` into `server`.

**Tech Stack:** Go 1.25, module `github.com/yinghanhung/simulacra`. **No new dependencies** (ConnectRPC and `golang.org/x/net` arrive in Phase 4). Tests use the standard library + existing `google.golang.org/grpc` health client + the `testdata/protos` fixture.

**Reference:** M3 design `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §4 (facade), §13 phase 1. This is the first of the per-phase M3 plans; each phase lands green on `main` independently.

## Global Constraints

- Module path `github.com/yinghanhung/simulacra`; Go 1.25. The new package is **public**: import path `github.com/yinghanhung/simulacra/server` (not under `internal/`).
- No new third-party dependencies in this phase.
- **No behavior change.** Every existing `serve` and `check` behavior — flags, defaults, startup banner text, hot-reload, graceful/force shutdown, exit codes — is preserved. All existing tests pass; watcher tests **move** (not rewrite) into `server`.
- The facade's `Start` **requires at least one schema source** (proto dir or descriptor set), mirroring today's `serve`. The design's "schema sources optional when the admin plane is enabled" relaxation is **out of scope** here — it arrives in Phase 4 together with `Options.AdminAddr` / `Server.AdminAddr()`, which this phase deliberately does **not** add.
- One intentional, test-free deviation: `serve`'s "no schema source" error text becomes flag-agnostic (the embeddable facade must not name CLI flags like `--proto`). `check` keeps its flag-named message unchanged. No test asserts the `serve` text (verified: the string lives only in `internal/cli/load.go`, used by `check`).

---

## File structure

```
simulacra/
├── server/                                  # NEW public package
│   ├── server.go                            # Options, Server, Start, lifecycle methods, registry/stub helpers
│   ├── server_test.go                       # facade lifecycle tests (health, counts, errors, shutdown)
│   ├── watcher.go                            # MOVED from internal/cli: watcherRun, startStubWatcher, reconcileStubDirs
│   └── watcher_test.go                       # MOVED watcher tests, adjusted to package server
├── internal/cli/
│   ├── serve.go                             # refactored onto server.Start; watcher plumbing removed
│   └── serve_test.go                        # watcher tests removed; signal/flag tests kept & adjusted
```

Dependency direction: `internal/cli → server → {dataplane, stub, schema, journal}`. `server` never imports `internal/cli`. `internal/cli/load.go` (`sources.buildRegistry`/`loadStubs`) is untouched and still serves `check`.

**Package boundary map** (what moves where, decided during exploration):

| Symbol (today, `internal/cli/serve.go`) | Fate |
|---|---|
| `watcherRun` (+ `cancelNow`, `stop`), `watchDirsFunc` | **Move** to `server/watcher.go` |
| `startStubWatcher`, `startReadyWatcher` | **Move** to `server/watcher.go` |
| `reconcileStubDirsContext` | **Move + rename** to `reconcileStubDirs` in `server/watcher.go` (`reloadOutput` param → `server.Reporter`) |
| `reloadOutput` interface | **Replace** with exported `server.Reporter` |
| free `startWatcher`, `reloadStubDirs`, `reloadStubDirsContext` | **Drop** (test-only convenience wrappers; no non-test callers — verified) |
| `commandOutput`, `serveWithRuntime`, `newServeCmd`, `newServeCmdWithListen` | **Stay** in `internal/cli` (`serveWithRuntime` loses its `watcher` param) |
| `shutdown.go` (`waitAndShutdown*`) | **Stay**, unchanged |

---

### Task 1: `server` package — core lifecycle (no watcher yet)

Create the facade that does everything `serve --watch=false` does today: build registry, load stubs, construct the data-plane server, bind, serve in the background, and stop. The hot-reload watcher is added in Task 2; `Options.Watch` is defined here but not yet honored.

**Files:**
- Create: `server/server.go`
- Test: `server/server_test.go`

**Interfaces:**
- Consumes: `schema.NewRegistry`, `(*schema.Registry).AddProtoDir(ctx, dir)`, `.AddDescriptorSetFile(path)`, `.Services()`; `stub.LoadDirs(reg, dirs) ([]*stub.Compiled, []error)`, `stub.NewStore(stubs)`; `journal.New(size)`; `dataplane.New(reg, store, calls) (*dataplane.Server, error)` with `.Serve(lis) error`, `.GracefulStop()`, `.Stop()`.
- Produces (relied on by Task 2 and `serve`):
  - `type Reporter interface { Printf(string, ...any); PrintErrln(...any) }`
  - `type Options struct { ProtoDirs, DescriptorSetPaths, StubDirs []string; DataAddr string; JournalSize int; Watch bool; Listen func(string, string) (net.Listener, error); Reporter Reporter }`
  - `func Start(ctx context.Context, opts Options) (*Server, error)`
  - `func (s *Server) DataAddr() net.Addr`
  - `func (s *Server) ServiceCount() int`
  - `func (s *Server) StubCount() int`
  - `func (s *Server) Wait() error`
  - `func (s *Server) GracefulStop()`
  - `func (s *Server) Stop()`
  - `func (s *Server) Shutdown(ctx context.Context) error`
  - unexported helpers `buildRegistry(ctx, protoDirs, descriptorSets)` and `loadStubs(reg, dirs, reporter)` (reused by Task 2's watcher), and `normalizeServeErr(err)` (maps `grpc.ErrServerStopped` → nil)

- [ ] **Step 1: Write the failing lifecycle test**

Create `server/server_test.go`:

```go
package server

import (
	"context"
	"os"
	"path/filepath"
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./server/ -v`
Expected: FAIL — `server/server.go` does not exist, so the package won't compile (`undefined: Start`, `undefined: Options`).

- [ ] **Step 3: Write `server/server.go`**

```go
// Package server is the public wiring facade for a Simulacra instance. It
// builds the schema registry, stub store, journal, and data-plane gRPC server
// from a single Options value, binds the data listener, and serves in the
// background. The CLI, the Go SDK's in-process mode, and tests all start a
// server the same way.
//
// The admin control plane (Options.AdminAddr / Server.AdminAddr) is added in a
// later milestone phase; Options and Server are designed to grow it without
// breaking callers.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// Reporter receives asynchronous progress messages — currently stub hot-reload
// results and watch errors. The CLI supplies an implementation backed by the
// command's output streams; embedders that want silence leave it nil.
type Reporter interface {
	Printf(format string, args ...any)
	PrintErrln(args ...any)
}

// Options configures a server. At least one schema source (ProtoDirs or
// DescriptorSetPaths) is required: a server with no schema could answer
// nothing. JournalSize must be greater than zero.
type Options struct {
	ProtoDirs          []string // directories of .proto files (each is an import root)
	DescriptorSetPaths []string // serialized FileDescriptorSet files
	StubDirs           []string // directories of stub YAML files
	DataAddr           string   // data-plane listen address, e.g. ":6565"
	JournalSize        int      // recent-call ring capacity (must be > 0)
	Watch              bool      // watch StubDirs and hot-reload (honored from Task 2 on)

	// Listen overrides the listener factory so tests can inject fakes.
	// Nil uses net.Listen.
	Listen func(network, address string) (net.Listener, error)
	// Reporter receives hot-reload progress. Nil discards it.
	Reporter Reporter
}

// Server is a running Simulacra instance.
type Server struct {
	reg     *schema.Registry
	store   *stub.Store
	journal *journal.Journal
	data    *dataplane.Server
	lis     net.Listener

	serveResult chan error
	waitOnce    sync.Once
	waitErr     error

	stubCount atomic.Int64
}

// Start builds and starts a server, returning once the data listener is bound.
// The returned Server serves in the background until GracefulStop, Stop, or
// Shutdown is called.
func Start(ctx context.Context, opts Options) (*Server, error) {
	if opts.JournalSize <= 0 {
		return nil, fmt.Errorf("journal size must be greater than zero (got %d)", opts.JournalSize)
	}
	listen := opts.Listen
	if listen == nil {
		listen = net.Listen
	}
	reporter := opts.Reporter
	if reporter == nil {
		reporter = discardReporter{}
	}

	reg, err := buildRegistry(ctx, opts.ProtoDirs, opts.DescriptorSetPaths)
	if err != nil {
		return nil, err
	}
	stubs, err := loadStubs(reg, opts.StubDirs, reporter)
	if err != nil {
		return nil, err
	}
	store := stub.NewStore(stubs)
	jrnl := journal.New(opts.JournalSize)
	data, err := dataplane.New(reg, store, jrnl)
	if err != nil {
		return nil, err
	}

	s := &Server{
		reg:         reg,
		store:       store,
		journal:     jrnl,
		data:        data,
		serveResult: make(chan error, 1),
	}
	s.stubCount.Store(int64(len(stubs)))

	// Task 2 launches the stub watcher here (before binding the listener).

	lis, err := listen("tcp", opts.DataAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", opts.DataAddr, err)
	}
	s.lis = lis

	go func() { s.serveResult <- normalizeServeErr(s.data.Serve(lis)) }()
	return s, nil
}

// normalizeServeErr maps grpc's "server has been stopped" sentinel to nil.
// Serve returns it when Stop or GracefulStop wins the race against the serve
// goroutine's first scheduling (grpc-go returns ErrServerStopped immediately
// when its listener set was already torn down). That is a clean stop, not a
// failure, so Wait must not surface it. The conformance harness already
// applies the same normalization (conformance/harness_test.go, waitForServe).
func normalizeServeErr(err error) error {
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// DataAddr is the bound data-plane listener address.
func (s *Server) DataAddr() net.Addr { return s.lis.Addr() }

// ServiceCount is the number of registered services (mocked services + health).
func (s *Server) ServiceCount() int { return len(s.reg.Services()) }

// StubCount is the number of currently loaded stubs.
func (s *Server) StubCount() int { return int(s.stubCount.Load()) }

// Wait blocks until the data-plane server stops, returning Serve's error
// (nil after a clean GracefulStop or Stop). Safe to call from one goroutine.
func (s *Server) Wait() error {
	s.waitOnce.Do(func() { s.waitErr = <-s.serveResult })
	return s.waitErr
}

// GracefulStop stops the data plane, waiting for in-flight RPCs (including open
// streams); it can block indefinitely. Callers that need a bound fall back to
// Stop.
func (s *Server) GracefulStop() { s.data.GracefulStop() }

// Stop aborts the data plane immediately.
func (s *Server) Stop() { s.data.Stop() }

// Shutdown gracefully stops the server, forcing a hard stop if ctx is done
// before the graceful stop completes, and returns once fully stopped.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.Stop()
		<-done
	}
	return s.Wait()
}

func buildRegistry(ctx context.Context, protoDirs, descriptorSets []string) (*schema.Registry, error) {
	if len(protoDirs) == 0 && len(descriptorSets) == 0 {
		return nil, errors.New("at least one schema source is required (a proto directory or a descriptor set)")
	}
	reg := schema.NewRegistry()
	for _, d := range protoDirs {
		if err := reg.AddProtoDir(ctx, d); err != nil {
			return nil, err
		}
	}
	for _, f := range descriptorSets {
		if err := reg.AddDescriptorSetFile(f); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func loadStubs(reg *schema.Registry, dirs []string, reporter Reporter) ([]*stub.Compiled, error) {
	stubs, errs := stub.LoadDirs(reg, dirs)
	for _, e := range errs {
		reporter.PrintErrln("stub error:", e)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%d invalid stub(s)", len(errs))
	}
	return stubs, nil
}

type discardReporter struct{}

func (discardReporter) Printf(string, ...any) {}
func (discardReporter) PrintErrln(...any)     {}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./server/ -v`
Expected: PASS — all four tests green.

- [ ] **Step 5: Vet and commit**

```bash
go vet ./server/
git add server/server.go server/server_test.go
git commit -m "feat(server): add public lifecycle facade (data plane, no watcher yet)"
```

---

### Task 2: Move the watcher into the facade and refactor `serve` onto it

This is the cutover. It (a) relocates the hot-reload watcher plumbing from `internal/cli` into `server`, (b) wires `Start`'s `Watch` branch and augments the lifecycle methods to cancel/join the watcher, (c) rewrites `serve.go` to call `server.Start`, and (d) moves the watcher tests. `main` must remain green: the watcher symbols leave `internal/cli` and land in `server` in the same commit as the `serve` rewrite, so there is no broken intermediate and no duplication.

**Files:**
- Create: `server/watcher.go`, `server/watcher_test.go`
- Modify: `server/server.go` (add watcher field + `launchStubWatcher`; wire `Start`; augment `GracefulStop`/`Stop`/`Wait`)
- Modify: `internal/cli/serve.go` (rewrite `RunE`; trim `serveWithRuntime`; delete moved symbols; fix imports)
- Modify: `internal/cli/serve_test.go` (delete moved tests; adjust two survivors)

**Interfaces:**
- Consumes: everything from Task 1, plus `stub.WatchOptions{Debounce, OnChange, OnError, Ready}` and `stub.WatchWithOptions(ctx, dirs, opts) error`.
- Produces (in `server`, unexported): `type watcherRun struct{ cancel context.CancelFunc; done <-chan struct{} }` with `cancelNow()` and `stop()`; `startStubWatcher(parent, dirs, reporter, onChange, reconcile, watch) (watcherRun, error)`; `reconcileStubDirs(ctx, reporter, reg, store, dirs, announce) (int, error)`; `func (s *Server) launchStubWatcher(...)`.

**Test disposition** (each existing `internal/cli/serve_test.go` test's fate):

| Test | Fate |
|---|---|
| `TestServeJournalSizeFlagDefaultsTo1024`, `TestServeWatchFlagDefaultsTrue`, `TestServeRejectsNonPositiveJournalSize` | keep (CLI flags) |
| `TestServeExecuteContextCancellationReturnsWithinBound` (+ `blockingListener`, `notifyWriter`, `fixedAddr`) | keep (CLI e2e) |
| `TestCommandOutputSerializesConcurrentWrites` | keep (`commandOutput` stays) |
| `TestServeWithRuntimeCancelsAndJoinsSignalWaiter` | keep, **edit** (drop `watcherRun{}` arg) |
| `TestWatcherCanceledDuringSignalShutdown` | **replace** with `TestWaitAndShutdownRunsGracefulOnSignal` (no moved types) |
| `TestReloadStubDirsKeepsInvalidStoreAndResetsValidBudget`, `TestReloadStubDirsContextDoesNothingAfterCancellation` | **move** to `server` (call `reconcileStubDirs` directly) |
| `TestStartStubWatcherReconcilesMutationBeforeReady`, `…ReturnsStartupReconciliationError`, `…ReturnsInitialAttachError`, `…CancellationDuringStartupJoinsWatcher`, `…SerializesStartupReconcileBeforeCallbacks` | **move** to `server` (recipe below) |
| `TestServeWithRuntimeStopsAndJoinsWatcherOnServeReturn` | **replace** with `server`'s `TestWatcherRunStopJoinsGoroutine` |
| `discardReloadOutput` helper | delete (replaced by `server`'s `discardReporter`) |

- [ ] **Step 1: Write the failing watcher-move tests in `server/watcher_test.go`**

Create `server/watcher_test.go` with the helper, the two reconcile tests (rewritten to call `reconcileStubDirs`), and the join test:

```go
package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)
```

> **Imports are exactly what Step 1's tests use.** `schema` is required by `testRegistry`'s return type. Step 5 adds `errors`, `sync/atomic`, and `time` when the moved tests that use them arrive — adding them now breaks the build, because Go rejects unused imports.

```go

// captureReporter records reload progress for assertions (replaces the
// cobra-backed commandOutput used by these tests when they lived in cli).
type captureReporter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *captureReporter) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(&r.buf, format, args...)
}

func (r *captureReporter) PrintErrln(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(&r.buf, args...)
}

func (r *captureReporter) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func testRegistry(t *testing.T) *schema.Registry {
	t.Helper()
	reg, err := buildRegistry(context.Background(), []string{"../testdata/protos"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestReconcileStubDirsKeepsInvalidStoreAndResetsValidBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	valid := `
- method: shop.v1.OrderService/GetOrder
  times: 1
  respond: { message: { note: fresh } }
`
	write(valid)
	reg := testRegistry(t)
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("initial Select = nil, want limited stub")
	}

	reporter := &captureReporter{}
	write("{{{ invalid yaml")
	if _, err := reconcileStubDirs(context.Background(), reporter, reg, store, []string{dir}, true); err == nil {
		t.Fatal("reconcile error = nil, want invalid-stub error")
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after invalid reload = %v, want old exhausted store", got)
	}
	if !strings.Contains(reporter.String(), "stub error:") {
		t.Fatalf("invalid reload output = %q, want stub error", reporter.String())
	}

	reporter = &captureReporter{}
	write(valid)
	if _, err := reconcileStubDirs(context.Background(), reporter, reg, store, []string{dir}, true); err != nil {
		t.Fatalf("reconcile valid = %v", err)
	}
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("Select after valid reload = nil, want reset times budget")
	}
	if !strings.Contains(reporter.String(), "1 stub(s) reloaded") {
		t.Fatalf("valid reload output = %q, want count", reporter.String())
	}
}

func TestReconcileStubDirsDoesNothingAfterCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  times: 1
  respond: { message: {} }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := testRegistry(t)
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("initial limited stub did not select")
	}

	reporter := &captureReporter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reconcileStubDirs(ctx, reporter, reg, store, []string{dir}, true); err == nil {
		t.Fatal("reconcile error = nil, want context cancellation")
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after canceled reload = %v, want exhausted original store", got)
	}
	if reporter.String() != "" {
		t.Fatalf("canceled reload output = %q, want none", reporter.String())
	}
}

func TestWatcherRunStopJoinsGoroutine(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	watcher, _ := startReadyWatcher(context.Background(), func(ctx context.Context, ready func()) error {
		ready()
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil
	}, func(error) {})
	<-started
	watcher.stop() // cancels then joins
	select {
	case <-canceled:
	default:
		t.Fatal("watcherRun.stop returned before the watcher goroutine exited")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./server/ -run 'Reconcile|WatcherRun' -v`
Expected: FAIL — `undefined: reconcileStubDirs`, `undefined: startReadyWatcher` (watcher.go not created yet).

- [ ] **Step 3: Create `server/watcher.go` (move the watcher plumbing)**

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

type watcherRun struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

type watchDirsFunc func(context.Context, []string, stub.WatchOptions) error

// startStubWatcher does not return success until every initial watch is
// attached. Once attached, it reconciles the store while filesystem events
// are already being collected, closing the load-before-watch update window.
func startStubWatcher(parent context.Context, dirs []string, reporter Reporter, onChange func(context.Context), reconcile func(context.Context) error, watch watchDirsFunc) (watcherRun, error) {
	startupComplete := make(chan struct{})
	var completeStartup sync.Once
	finishStartup := func() {
		completeStartup.Do(func() { close(startupComplete) })
	}
	watcher, started := startReadyWatcher(parent, func(ctx context.Context, ready func()) error {
		return watch(ctx, dirs, stub.WatchOptions{
			Debounce: 200 * time.Millisecond,
			OnChange: onChange,
			OnError: func(err error) {
				reporter.PrintErrln("watch error:", err)
			},
			Ready: func() {
				ready()
				<-startupComplete
			},
		})
	}, func(err error) {
		reporter.PrintErrln("watch error:", err)
	})

	select {
	case err := <-started:
		if err != nil {
			finishStartup()
			watcher.stop()
			return watcherRun{}, fmt.Errorf("starting stub watcher: %w", err)
		}
	case <-parent.Done():
		finishStartup()
		watcher.stop()
		return watcherRun{}, parent.Err()
	}
	if err := reconcile(parent); err != nil {
		watcher.cancelNow()
		finishStartup()
		watcher.stop()
		if parentErr := parent.Err(); parentErr != nil {
			return watcherRun{}, parentErr
		}
		return watcherRun{}, fmt.Errorf("reconciling stub watcher: %w", err)
	}
	finishStartup()
	if err := parent.Err(); err != nil {
		watcher.stop()
		return watcherRun{}, err
	}
	return watcher, nil
}

func startReadyWatcher(parent context.Context, run func(context.Context, func()) error, report func(error)) (watcherRun, <-chan error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	started := make(chan error, 1)
	var startup sync.Once
	ready := func() {
		startup.Do(func() { started <- nil })
	}
	go func() {
		defer close(done)
		err := run(ctx, ready)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		sentStartup := false
		startup.Do(func() {
			sentStartup = true
			started <- err
		})
		if !sentStartup && err != nil && !errors.Is(err, context.Canceled) {
			report(err)
		}
	}()
	return watcherRun{cancel: cancel, done: done}, started
}

func (w watcherRun) cancelNow() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w watcherRun) stop() {
	w.cancelNow()
	if w.done != nil {
		<-w.done
	}
}

// reconcileStubDirs reloads and recompiles every stub under dirs and atomically
// swaps them into the store. On any load error the store is left untouched and
// the errors are reported. Returns the number of stubs installed.
func reconcileStubDirs(ctx context.Context, reporter Reporter, reg *schema.Registry, store *stub.Store, dirs []string, announce bool) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	stubs, errs := stub.LoadDirs(reg, dirs)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if len(errs) > 0 {
		for _, err := range errs {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			reporter.PrintErrln("stub error:", err)
		}
		return 0, errors.Join(errs...)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	store.Replace(stubs)
	if announce && ctx.Err() == nil {
		reporter.Printf("simulacra: %d stub(s) reloaded\n", len(stubs))
	}
	return len(stubs), nil
}
```

- [ ] **Step 4: Run the moved tests to verify they pass**

Run: `go test ./server/ -run 'Reconcile|WatcherRun' -v`
Expected: PASS.

- [ ] **Step 5: Move the five `startStubWatcher` concurrency tests into `server/watcher_test.go`**

Copy `TestStartStubWatcherReconcilesMutationBeforeReady`, `TestStartStubWatcherReturnsStartupReconciliationError`, `TestStartStubWatcherReturnsInitialAttachError`, `TestStartStubWatcherCancellationDuringStartupJoinsWatcher`, and `TestStartStubWatcherSerializesStartupReconcileBeforeCallbacks` from `internal/cli/serve_test.go` into `server/watcher_test.go`, applying these **exact substitutions** (they change only fixtures/wiring, never the concurrency logic under test):

1. Replace the registry setup
   ```go
   src := &sources{protoDirs: []string{"../../testdata/protos"}}
   reg, err := src.buildRegistry(context.Background())
   if err != nil {
       t.Fatal(err)
   }
   ```
   with
   ```go
   reg := testRegistry(t)
   ```
2. `discardReloadOutput{}` → `discardReporter{}`
3. `&commandOutput{cmd: cmd}` → `&captureReporter{}` (and delete any now-unused `cmd := &cobra.Command{}` / `cmd.SetOut(...)` / `cmd.SetErr(...)` scaffolding that only fed `commandOutput`)
4. `reconcileStubDirsContext(` → `reconcileStubDirs(`
5. Drop the `err`-from-`buildRegistry` handling folded into substitution 1.

Worked example — `TestStartStubWatcherReconcilesMutationBeforeReady` after substitution:

```go
func TestStartStubWatcherReconcilesMutationBeforeReady(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	write := func(times int) {
		t.Helper()
		body := `
- method: shop.v1.OrderService/GetOrder
  times: ` + fmt.Sprint(times) + `
  respond: { message: {} }
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(1)
	reg := testRegistry(t)
	initial, loadErrs := stub.LoadDirs(reg, []string{dir})
	if len(loadErrs) != 0 {
		t.Fatal(loadErrs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("initial limited stub did not select")
	}

	reporter := &captureReporter{}
	ctx, cancel := context.WithCancel(context.Background())
	watch := func(ctx context.Context, _ []string, opts stub.WatchOptions) error {
		write(2) // mutation after the initial load but before watches report ready
		opts.Ready()
		<-ctx.Done()
		return nil
	}
	reconcile := func(reloadCtx context.Context) error {
		_, err := reconcileStubDirs(reloadCtx, reporter, reg, store, []string{dir}, true)
		return err
	}
	watcher, err := startStubWatcher(ctx, []string{dir}, reporter, func(reloadCtx context.Context) {
		_ = reconcile(reloadCtx)
	}, reconcile, watch)
	if err != nil {
		t.Fatal(err)
	}
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("startup reconciliation did not install mutation made before watcher readiness")
	}
	cancel()
	watcher.stop()
}
```

Apply the same recipe to the other four. (`TestStartStubWatcherReturnsInitialAttachError` and `TestStartStubWatcherCancellationDuringStartupJoinsWatcher` use `discardReloadOutput{}` → `discardReporter{}` only; they need no registry.) Add a `discardReporter` note: it already exists in `server.go` from Task 1 — reuse it, do not redeclare.

- [ ] **Step 6: Run the moved suite**

Run: `go test ./server/ -v`
Expected: PASS — all facade + watcher tests green.

- [ ] **Step 7: Wire the watcher into `Start` and the lifecycle methods (`server/server.go`)**

In `server/server.go`, add the watcher field to `Server`:

```go
	serveResult chan error
	waitOnce    sync.Once
	waitErr     error

	watcher watcherRun

	stubCount atomic.Int64
```

Replace the placeholder comment in `Start` (`// Task 2 launches the stub watcher here ...`) with:

```go
	if opts.Watch && len(opts.StubDirs) > 0 {
		watcher, err := s.launchStubWatcher(ctx, opts.StubDirs, reporter)
		if err != nil {
			return nil, err
		}
		s.watcher = watcher
	}
```

And make listen failure stop the watcher — change the listen error branch to:

```go
	lis, err := listen("tcp", opts.DataAddr)
	if err != nil {
		s.watcher.stop()
		return nil, fmt.Errorf("listening on %s: %w", opts.DataAddr, err)
	}
```

Augment the lifecycle methods so shutdown cancels the watcher and `Wait` joins it:

```go
func (s *Server) Wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = <-s.serveResult
		s.watcher.stop()
	})
	return s.waitErr
}

func (s *Server) GracefulStop() {
	s.watcher.cancelNow()
	s.data.GracefulStop()
}

func (s *Server) Stop() {
	s.watcher.cancelNow()
	s.data.Stop()
}
```

Add the `launchStubWatcher` method (updates `stubCount` on each successful reconcile):

```go
func (s *Server) launchStubWatcher(ctx context.Context, dirs []string, reporter Reporter) (watcherRun, error) {
	onChange := func(ctx context.Context) {
		if count, err := reconcileStubDirs(ctx, reporter, s.reg, s.store, dirs, true); err == nil {
			s.stubCount.Store(int64(count))
		}
	}
	return startStubWatcher(ctx, dirs, reporter, onChange, func(ctx context.Context) error {
		count, err := reconcileStubDirs(ctx, reporter, s.reg, s.store, dirs, false)
		if err == nil {
			s.stubCount.Store(int64(count))
		}
		return err
	}, stub.WatchWithOptions)
}
```

- [ ] **Step 8: Add a facade watcher test and run the `server` suite**

Append to `server/server_test.go`:

```go
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
```

(`os`, `path/filepath`, and `time` are already in `server_test.go`'s import block from Task 1.)

Run: `go test ./server/ -v`
Expected: PASS.

- [ ] **Step 9: Rewrite `internal/cli/serve.go` onto the facade**

Replace the entire `RunE` body (lines 34–101 today) with:

```go
		RunE: func(cmd *cobra.Command, args []string) error {
			if journalSize <= 0 {
				return fmt.Errorf("journal-size must be greater than zero (got %d)", journalSize)
			}
			output := &commandOutput{cmd: cmd}
			srv, err := server.Start(cmd.Context(), server.Options{
				ProtoDirs:          src.protoDirs,
				DescriptorSetPaths: src.descriptorSets,
				StubDirs:           src.stubDirs,
				DataAddr:           listen,
				JournalSize:        journalSize,
				Watch:              watchStubs,
				Listen:             serveListen,
				Reporter:           output,
			})
			if err != nil {
				return err
			}

			output.Printf("simulacra: data plane listening on %s\n", srv.DataAddr())
			output.Printf("  %d service(s) registered, %d stub(s) loaded — reflection and health enabled\n",
				srv.ServiceCount(), srv.StubCount())

			sig := make(chan os.Signal, 2)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			stopShutdownWaiter := make(chan struct{})
			var stopShutdownWaiterOnce sync.Once
			cancelShutdown := func() {
				stopShutdownWaiterOnce.Do(func() { close(stopShutdownWaiter) })
			}
			shutdownDone := make(chan struct{})
			go func() {
				defer close(shutdownDone)
				waitAndShutdownContextStop(cmd.Context(), stopShutdownWaiter, sig, 10*time.Second,
					srv.GracefulStop, srv.Stop, output.Println)
			}()
			return serveWithRuntime(cancelShutdown, shutdownDone, srv.Wait)
		},
```

Delete these now-moved/dead symbols from `serve.go`: `watcherRun` (+ `cancelNow`, `stop`), `watchDirsFunc`, `startStubWatcher`, `startReadyWatcher`, `startWatcher`, `reloadOutput`, `reloadStubDirs`, `reloadStubDirsContext`, `reconcileStubDirsContext`.

Trim `serveWithRuntime` to drop the watcher parameter:

```go
func serveWithRuntime(cancelShutdown context.CancelFunc, shutdownDone <-chan struct{}, serve func() error) error {
	defer func() {
		cancelShutdown()
		<-shutdownDone
	}()
	return serve()
}
```

Keep `commandOutput` (it satisfies `server.Reporter` and provides `Println` for shutdown logging). Fix the import block to:

```go
import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/server"
)
```

(Removed: `errors`, `internal/dataplane`, `internal/journal`, `internal/schema`, `internal/stub`.)

- [ ] **Step 10: Adjust the two CLI test survivors and delete the moved ones (`internal/cli/serve_test.go`)**

Delete from `serve_test.go`: `TestReloadStubDirsKeepsInvalidStoreAndResetsValidBudget`, `TestReloadStubDirsContextDoesNothingAfterCancellation`, all five `TestStartStubWatcher*`, `TestServeWithRuntimeStopsAndJoinsWatcherOnServeReturn`, and the `discardReloadOutput` type.

Edit `TestServeWithRuntimeCancelsAndJoinsSignalWaiter` — drop the leading `watcherRun{}` argument:

```go
	go func() {
		_ = serveWithRuntime(cancel, waiterDone, func() error { return nil })
		close(returned)
	}()
```

Replace `TestWatcherCanceledDuringSignalShutdown` with a shutdown test that no longer references the moved watcher types:

```go
func TestWaitAndShutdownRunsGracefulOnSignal(t *testing.T) {
	canceled := make(chan struct{})
	graceful := func() { close(canceled) }
	sig := make(chan os.Signal, 1)
	shutdownDone := make(chan struct{})
	go func() {
		waitAndShutdown(sig, time.Second, graceful, func() {}, func(...any) {})
		close(shutdownDone)
	}()
	sig <- os.Interrupt
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("graceful shutdown was not invoked on signal")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("signal shutdown did not finish")
	}
}
```

Then fix the `serve_test.go` import block. After the deletions, remove `errors`, `fmt`, `path/filepath`, `sync/atomic`, `github.com/yinghanhung/simulacra/internal/match`, and `github.com/yinghanhung/simulacra/internal/stub`; **keep** `bytes`, `context`, `net`, `os`, `strings`, `sync`, `time`, `testing`, and `github.com/spf13/cobra` (`cobra` is still used by `TestCommandOutputSerializesConcurrentWrites`). Let the compiler in the next step confirm.

- [ ] **Step 11: Build, vet, and run the full CLI + server suites**

Run:
```bash
go build ./... && go vet ./...
go test ./internal/cli/ ./server/ -v
```
Expected: PASS. Fix any "imported and not used" / "declared and not used" errors surfaced by Step 10's pruning until green.

- [ ] **Step 12: Commit**

```bash
git add server/ internal/cli/serve.go internal/cli/serve_test.go
git commit -m "refactor(serve): move wiring behind server.Start facade; relocate watcher"
```

---

### Task 3: Phase exit gate — race, full suite, and a real `serve` smoke test

Prove the refactor preserved behavior end-to-end and under the race detector across every escape path (reflection, CEL, journal), and that the shipped binary still serves.

**Files:** none modified unless a failure is found.

- [ ] **Step 1: Full suite under the race detector**

Run: `go test -race ./...`
Expected: PASS (`ok` for every package, including `conformance` and the new `server`).

Note what actually gates behavior here. `conformance` still builds its own server directly via `dataplane.New` (`conformance/harness_test.go`), so it does **not** exercise the facade — it proves the data plane is unchanged, not that the facade wires it correctly. The facade's behavioral gate is `TestStartServesStubbedUnaryCall` (Task 1): a real gRPC call through a `server.Start` server, asserting both the stubbed response and the journal entry, so a mis-wired registry, store, or journal fails the phase. Migrating the conformance harness onto the facade is deliberately left to a later phase, where the admin-driven conformance leg (design §10) reworks that harness anyway.

- [ ] **Step 2: Vet the whole module**

Run: `go vet ./...`
Expected: no output.

- [ ] **Step 3: Smoke-test the built binary**

Proves the wired-up `cmd/simulacra` binary starts, prints the banner, and shuts down cleanly on SIGINT. (Health-over-the-port is already proven by `TestStartServesHealthAndReportsCounts`, so this uses no external probe tool.)

```bash
go build -o /tmp/simulacra-p1 ./cmd/simulacra
/tmp/simulacra-p1 serve --proto ./testdata/protos --listen 127.0.0.1:6565 >/tmp/sim-p1.log 2>&1 &
SIM_PID=$!
sleep 1
cat /tmp/sim-p1.log
kill -INT $SIM_PID; wait $SIM_PID 2>/dev/null
grep -q "data plane listening on 127.0.0.1:6565" /tmp/sim-p1.log && echo "SMOKE OK"
```
Expected: the log shows `simulacra: data plane listening on 127.0.0.1:6565` followed by `N service(s) registered, 0 stub(s) loaded — reflection and health enabled`, the process exits on SIGINT, and the final line prints `SMOKE OK`. (If `127.0.0.1:6565` is busy, pick another port and adjust the `grep`.)

- [ ] **Step 4: Confirm no behavior drift in the CLI surface**

Run: `go test ./internal/cli/ -run 'TestServe' -v`
Expected: PASS — flag defaults (`--journal-size=1024`, `--watch=true`), non-positive journal-size rejection, and context-cancellation-returns-within-bound all still hold.

- [ ] **Step 5: Final commit only if Steps 1–4 required a fix**

If any step forced a change, stage **only the files that fix changed** — never `git add -A`. The working tree also carries unrelated in-flight work (the M3 design spec and these plan documents), and a blanket stage would sweep it into this commit:

```bash
git status --short                     # confirm what the fix actually touched
git add server/server.go server/watcher.go internal/cli/serve.go   # adjust to the real list
git commit -m "fix: close M3 phase 1 verification gaps"
```
Otherwise Phase 1 is complete: the `server` facade exists, `serve` runs on it with no behavior change, and the wiring is ready for Phase 2 (`api/` protos) and Phase 4 (admin plane) to plug in.

---

## Self-review

**Spec coverage (§4 facade, §13 phase 1):**
- `server/` public package, `server.Options{ProtoDirs, DescriptorSetPaths, StubDirs, DataAddr, JournalSize, Watch}` — Task 1 ✓. `AdminAddr` deliberately deferred to Phase 4 (documented in Global Constraints).
- `server.Start(ctx, opts) (*server.Server, error)`, `Server.DataAddr()`, `Server.Shutdown(ctx)` — Task 1 ✓. `Server.AdminAddr()` deferred to Phase 4.
- "serve refactored onto this facade… wiring moves behind `server.Start` and is reused by SDK, tests, and CLI identically" — Task 2 ✓ (registry/store/journal/**watcher** all moved).
- "no behavior change" — preserved banner text, flags, hot-reload, graceful/force shutdown; enforced by kept CLI tests (Task 2 Step 10) + smoke test (Task 3). ✓

**Review fixes applied (2026-07-26):**
- `server/watcher_test.go`'s Step-1 import block now lists exactly what Step 1 uses — added `internal/schema` (required by `testRegistry`'s return type), removed `errors`, `sync/atomic`, and `time` (unused until Step 5's moved tests, and Go rejects unused imports). Step 5 re-adds them.
- `Start`'s serve goroutine normalizes `grpc.ErrServerStopped` → nil via `normalizeServeErr`, so `Wait` keeps its "nil after a clean stop" contract when `Stop`/`GracefulStop` beats the goroutine's first scheduling. Guarded by `TestStopImmediatelyAfterStartReportsCleanStop`. (Verified against grpc-go 1.82 `server.go:896`, and the conformance harness already normalizes the same sentinel.) Without this, `TestShutdownWithCanceledContextReturnsPromptly` would itself have been flaky.
- The watch test now mutates the stub directory **after** startup and polls `StubCount`, so it fails if the `Watch` branch is never wired; the old version asserted only the initial count.
- Added `TestStartServesStubbedUnaryCall` — a real gRPC call through a facade-built server asserting the stubbed response *and* the journal entry. `conformance` still constructs its server directly via `dataplane.New`, so before this the exit gate could not catch mis-wired registry/store/journal.
- The contingency commit stages explicit paths instead of `git add -A` (the tree carries unrelated spec/plan work).

**Placeholder scan:** No TBD/TODO. Every code step shows complete code; the five relocated concurrency tests carry an exact substitution recipe plus one fully-worked example (a mechanical move of existing, already-passing tests, not new logic).

**Type consistency:** `Reporter` (Printf/PrintErrln) is satisfied by `commandOutput` (used by `serve`) and `captureReporter` (tests); `discardReporter` is declared once in `server.go` (Task 1) and reused in tests. `serveWithRuntime` signature change (drop `watcherRun`) is applied at its definition (Task 2 Step 9) and its one remaining caller/test (Step 10). `reconcileStubDirsContext` → `reconcileStubDirs` rename is applied at the definition and every call site (facade `launchStubWatcher` + moved tests). Lifecycle contract: `GracefulStop`/`Stop` cancel the watcher; `Wait` joins it once; `Shutdown` = graceful-with-ctx-forced-fallback + `Wait`.
