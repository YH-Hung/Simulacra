# Simulacra M3 Phase 4a — Admin Transport, Lifecycle, and ControlService Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the control plane — a ConnectRPC handler served over h2c with `/healthz`, wired into the server facade's lifecycle and reachable via `serve --admin` — and prove it end to end with `ControlService`'s three RPCs.

**Architecture:** `internal/admin` owns *what is served* (routes, `/healthz`, h2c wrapping, `http2.ConfigureServer`); `server` owns *when it runs*. `http.Server.Shutdown` is unusable as a teardown barrier here — it returns instantly for hijacked h2c connections and blocks indefinitely for HTTP/1.1 ones — so `server` tracks admin connections itself, drains them under a single `shutdownGrace` budget, and force-closes the stragglers. Starting teardown and escalating to force are two independent idempotent signals, never one `sync.Once`, so the CLI's second Ctrl-C can cut short a graceful stop already in flight. A supervisor goroutine watches both serve loops so a plane that dies on its own takes the other down instead of leaving `Wait` blocked forever.

**Tech Stack:** Go 1.25, module `github.com/yinghanhung/simulacra`. `connectrpc.com/connect` v1.20.0 and `google.golang.org/grpc` v1.82.0 are already direct dependencies. `golang.org/x/net` v0.53.0 is already in `go.mod` as **indirect**; this phase promotes it to direct for `http2` and `http2/h2c`. No new module is downloaded.

**Reference:** Design `docs/superpowers/specs/2026-08-22-m3-p4a-admin-transport-design.md` (cited as "design §N" throughout). M3 design `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §4, §5, §7, §11, §14. Phase 3 is complete at `e263e03`.

## Global Constraints

- **No changes to `api/` or `gen/`.** The `simulacra.admin.v1` contract was frozen in Phase 2.
- **No changes to any Phase 3 core package** (`internal/schema`, `internal/stub`, `internal/journal`, `internal/match`). Every surface this phase needs already exists.
- **No new module downloads.** The only `go.mod` change is promoting `golang.org/x/net` from indirect to direct.
- **Out of scope, do not build:** `SchemaService`, `StubService`, `JournalService`, `VerifyService`, the shared `DecodedMessage`/`MetadataEntry` rendering, and the core-error → connect-code mapping table — all Phase 4b. Admin auth and TLS are M3 non-goals.
- **`http2.ConfigureServer(srv, h2s)` is mandatory, not tuning.** Without it, h2c connections keep serving after the server says it stopped. It is never to be removed as a "simplification".
- **`shutdownGrace = 5 * time.Second`**, an unexported constant in `server`, bounds `runTeardown` — within it, the admin drain and the data plane's graceful phase share one budget, and the admin drain (which runs first) has first claim on it. **Post-review:** the data plane is additionally guaranteed `dataGraceFloor = time.Second` of its own graceful window regardless of how much of `shutdownGrace` the admin drain consumed, so worst-case teardown is `shutdownGrace + dataGraceFloor`, not `shutdownGrace`. See Post-review amendments. The final joins in `supervise` (both serve goroutines and the watcher) happen after `runTeardown` returns and are not bounded by either budget; `watcher.stop()` in particular is documented as blocking for as long as an in-flight reload takes.
- **Every test binds `:0`.** The default admin port (`127.0.0.1:6566` as of the post-review `--admin` default change; see Post-review amendments) must never be the address a test *binds* — it may still appear as the flag default in `serve.go` and in the `DefValue` assertion in `serve_test.go`.
- Every task lands green: `go build ./... && go vet ./... && go test ./...` at every commit.
- Race-sensitive changes are verified with `-race -count=1`; the test cache will happily replay a stale pass.

## Pre-verified facts

Every claim below was executed against this repo before the plan was written. The probe programs were deleted; the numbers are what they printed.

1. **A native grpc-go client talks to a connect h2c handler.** `grpc.NewClient(addr, insecure)` + `conn.Invoke("/simulacra.admin.v1.ControlService/GetServerInfo", ...)` against `h2c.NewHandler(mux, h2s)` with the generated `NewControlServiceHandler` succeeded and returned the response body. M3 §14's top deferred risk does not bite.
2. **Without `http2.ConfigureServer`, an h2c connection survives `Shutdown`.** `srv.Shutdown(ctx)` returned in ~59µs with `err=nil`, and the *same* client connection then served another RPC successfully. **With** `ConfigureServer`, the same sequence terminated the connection: the client had to re-dial and got `connection refused`. This is the discriminator every weaker assertion misses.
3. **`Shutdown` blocks on an in-flight HTTP/1.1 request.** With a connect (HTTP/1.1) request parked in a blocking handler, `srv.Shutdown(ctx)` with a 500ms-deadline context returned only at 500.6ms with `context deadline exceeded`. With its context canceled at 200ms it returned at 202ms with `context canceled`. So the call must run under a context that `force` cancels, or an escalation cannot interrupt it.
4. **An idle h2c connection drains to zero.** With `ConfigureServer`, tracked-connection count went 1 → 0 within milliseconds of `Shutdown`. The bounded drain is not a permanent 5s tax on the common case.
5. **Force-close must go through the tracking wrapper.** Closing the *inner* `net.Conn` left the tracked count at 1 forever — the wrapper's `Close` is the only thing that untracks. Closing the wrapper dropped it to 0, and `Serve` then returned `http.ErrServerClosed`. This is the single easiest way to write a drain that never completes.
6. **A half-sent HTTP/1.1 request is a usable "active request" fixture.** Writing a complete request head with `Content-Length: 64` and never sending the body kept the connection tracked, made `Shutdown` block to its deadline (`context deadline exceeded` at 700ms), and `closeAll` then dropped the count to 0 and `Serve` returned `http: Server closed`.
7. **`/healthz` works over plain HTTP/1.1 through the h2c wrapper**: status 200, body `ok`. `mux.HandleFunc("GET /healthz", ...)` (Go 1.22 method patterns) compiles and routes.
8. **`http2.ConfigureServer` sets `srv.TLSNextProto` keys `[h2 unencrypted_http2]`** — an observable, cheap unit-level guard that the call happened.
9. **`golang.org/x/net@v0.53.0/http2/h2c` is in the module cache** and imports without a download.
10. **`internal/dataplane/server.go:111` already returns `codes.Unimplemented`** when `LookupMethod` fails, so schema-less startup needs no data-plane change.
11. **`dataplane.New` works with an empty registry** — it registers health itself via `reg.AddFile(healthpb.File_grpc_health_v1_health_proto)` (`internal/dataplane/server.go:52`), so a server built with no schema source still starts and serves health.
12. **Counts for test assertions:** `schema.NewRegistry()` + `AddProtoDir(ctx, "testdata/protos")` yields exactly **1** service (`shop.v1.OrderService`); `journal.New(4)` after two `Record` calls reports `Len()=2, Cap()=4`.
13. **`shop.v1.OrderService/Chat` is bidi** (`testdata/protos/shop/v1/order.proto:63`), and a stub with only `respond.on_open` is valid (`internal/stub/plan_test.go:120-122`) — after sending the open message the handler parks in `RecvMsg`, holding a stream open, which is what blocks grpc's `GracefulStop`.
14. **Only one production caller depends on the facade's stop methods:** `internal/cli/serve.go:65` hands `srv.GracefulStop`/`srv.Stop` to the signal waiter and `serve.go:67` blocks on `srv.Wait`. `internal/dataplane/server_test.go:142` calls the *data-plane* `GracefulStop`, not the facade's.
15. **`serve` does not validate schema sources itself.** `sources.buildRegistry` (`internal/cli/load.go:28`) is called only by `check`; `serve` passes `ProtoDirs`/`DescriptorSetPaths` straight to `server.Start`. Relaxing the rule in `server` therefore needs no `serve` change and leaves `check` untouched.

## File structure

```
simulacra/
├── go.mod                        # EDIT  Task 1: golang.org/x/net promoted to direct
├── internal/admin/
│   ├── doc.go                    # EDIT  Task 1: package doc describes the served plane
│   ├── admin.go                  # NEW   Task 1: Deps, Install, /healthz, h2c, ConfigureServer
│   ├── admin_test.go             # NEW   Task 1: Deps validation, healthz, route mounting, h2 config
│   ├── control.go                # NEW   Task 1 (unimplemented shell) → 2 GetServerInfo
│   │                             #       → 3 Reset → 4 Shutdown
│   ├── control_test.go           # NEW   Tasks 2–4: handler tests over httptest + HTTP/1.1
│   └── contract_test.go          # UNCHANGED (Phase 2 contract guard)
├── server/
│   ├── tracked.go                # NEW   Task 5: trackedListener / trackedConn
│   ├── tracked_test.go           # NEW   Task 5
│   ├── server.go                 # EDIT  Task 6 coordinator, Task 7 admin plane,
│   │                             #       Task 8 supervisor, Task 10 schema-less startup
│   ├── admin.go                  # NEW   Task 7: admin listener + http.Server construction,
│   │                             #       admin teardown steps
│   ├── server_test.go            # EXTEND Task 6 (escalation), Task 10 (schema-less)
│   └── admin_test.go             # NEW   Tasks 7–9: h2c interop, drain, escalation,
│                                 #       supervision, Shutdown RPC end to end
└── internal/cli/
    ├── serve.go                  # EDIT  Task 11: --admin flag and startup output
    └── serve_test.go             # EXTEND Task 11
```

Dependency order: Tasks 1 → 2, 3, 4 (`internal/admin`, independent of `server`). Task 5 → 6 → 7 → 8 → 9 (`server` lifecycle, strictly sequential). Task 10 after 7. Task 11 after 7. Tasks 1–4 may run in parallel with 5–6.

---

### Task 1: Admin transport — `Deps`, `Install`, `/healthz`, h2c

**Files:**
- Create: `internal/admin/admin.go`
- Create: `internal/admin/control.go`
- Create: `internal/admin/admin_test.go`
- Modify: `internal/admin/doc.go`
- Modify: `go.mod` (`golang.org/x/net` moves from the indirect block to the direct block)

**Interfaces:**
- Consumes: `schema.NewRegistry()`, `(*schema.Registry).AddProtoDir(context.Context, string) error`, `(*schema.Registry).Services() []protoreflect.ServiceDescriptor`, `stub.NewStore() *stub.Store`, `journal.New(int) *journal.Journal` — all shipped in Phase 3.
- Produces:
  - `admin.Deps` — struct with fields `Registry *schema.Registry`, `Store *stub.Store`, `Journal *journal.Journal`, `Version string`, `DataAddr func() string`, `AdminAddr func() string`, `Shutdown func()`.
  - `admin.Install(srv *http.Server, deps Deps) error` — sets `srv.Handler` and calls `http2.ConfigureServer`. Task 7 is its only production caller.
  - Unexported `controlService` struct with field `deps Deps`; Tasks 2–4 add its three methods.

**Why `Install` takes the `*http.Server`** rather than returning a handler: the h2c handler and the `*http2.Server` driving it must be paired with that specific server via `ConfigureServer` or shutdown does not reach h2c connections at all (pre-verified fact 2). Handing back a bare `http.Handler` would let a caller wire it up in a way that silently leaks connections.

**Why the addresses are funcs:** binding happens after the handler is built — a `:0` port is not known until `Listen` returns — and `GetServerInfo` must report the real ones. Accessors resolve per request and remove the ordering constraint.

**Note on `Version` validation:** the design says `Install` validates that every field is non-nil. `Version` is a `string`, and the non-nil analogue for a string is non-empty — an empty `Version` would silently publish `version: ""` on every `GetServerInfo`. It is validated the same way as the rest.

- [ ] **Step 1: Write the failing tests**

Create `internal/admin/admin_test.go`:

```go
package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// testDeps builds a complete Deps over real core objects. Tests that assert on
// specific counts overwrite only the fields they care about.
func testDeps(t *testing.T) admin.Deps {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	return admin.Deps{
		Registry:  reg,
		Store:     stub.NewStore(),
		Journal:   journal.New(4),
		Version:   "test-version",
		DataAddr:  func() string { return "127.0.0.1:6565" },
		AdminAddr: func() string { return "127.0.0.1:6566" },
		Shutdown:  func() {},
	}
}

// installed installs the control plane on a throwaway http.Server and serves
// its handler over plain HTTP/1.1. Every handler test runs this way: no ports,
// no h2c, no server facade. The h2c and lifecycle behavior is proven in
// server/admin_test.go, where it actually lives.
func installed(t *testing.T, deps admin.Deps) *httptest.Server {
	t.Helper()
	srv := &http.Server{}
	if err := admin.Install(srv, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestInstallServesHealthz(t *testing.T) {
	ts := installed(t, testDeps(t))
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /healthz body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("/healthz body = %q, want %q", body, "ok")
	}
}

// The route assertion is "not 404", not "returns Unimplemented", so it keeps
// passing unchanged as Tasks 2–4 fill the handlers in.
func TestInstallMountsControlServiceRoute(t *testing.T) {
	ts := installed(t, testDeps(t))
	url := ts.URL + adminv1connect.ControlServiceGetServerInfoProcedure
	resp, err := ts.Client().Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("control service route is not mounted: %s returned 404",
			adminv1connect.ControlServiceGetServerInfoProcedure)
	}
}

// http2.ConfigureServer registers the graceful-shutdown hook that reaches
// hijacked h2c connections. Without it the admin plane keeps serving after the
// server reports it stopped. The behavioral proof is
// TestAdminPlaneStopsServingAfterWait in server/; this one fails fast, in the
// package that owns the call, so a refactor that drops it is caught here first.
func TestInstallConfiguresHTTP2(t *testing.T) {
	srv := &http.Server{}
	if err := admin.Install(srv, testDeps(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := srv.TLSNextProto["h2"]; !ok {
		t.Fatal(`srv.TLSNextProto has no "h2" entry; http2.ConfigureServer was not called`)
	}
}

func TestInstallRejectsIncompleteDeps(t *testing.T) {
	unset := map[string]func(*admin.Deps){
		"Registry":  func(d *admin.Deps) { d.Registry = nil },
		"Store":     func(d *admin.Deps) { d.Store = nil },
		"Journal":   func(d *admin.Deps) { d.Journal = nil },
		"Version":   func(d *admin.Deps) { d.Version = "" },
		"DataAddr":  func(d *admin.Deps) { d.DataAddr = nil },
		"AdminAddr": func(d *admin.Deps) { d.AdminAddr = nil },
		"Shutdown":  func(d *admin.Deps) { d.Shutdown = nil },
	}
	for field, clearField := range unset {
		t.Run(field, func(t *testing.T) {
			deps := testDeps(t)
			clearField(&deps)
			err := admin.Install(&http.Server{}, deps)
			if err == nil {
				t.Fatalf("Install with %s unset returned a nil error", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("Install error = %q, want it to name the missing field %q", err, field)
			}
		})
	}
}

func TestInstallRejectsNilServer(t *testing.T) {
	if err := admin.Install(nil, testDeps(t)); err == nil {
		t.Fatal("Install(nil, deps) returned a nil error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestInstall' -count=1`
Expected: FAIL — `undefined: admin.Deps`, `undefined: admin.Install`.

- [ ] **Step 3: Write the implementation**

Create `internal/admin/admin.go`:

```go
package admin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// Deps is everything the control plane needs from the server that hosts it.
type Deps struct {
	Registry *schema.Registry
	Store    *stub.Store
	Journal  *journal.Journal
	Version  string

	// DataAddr and AdminAddr report the bound listener addresses. They are
	// funcs, not strings, because binding happens after the handler is built
	// — a ":0" port is not known until Listen returns — and GetServerInfo
	// must report the real ones.
	DataAddr  func() string
	AdminAddr func() string

	// Shutdown starts server teardown and returns immediately. The server
	// supplies it; the handler must not block on teardown, because the
	// response it is about to write is itself what teardown waits to drain.
	Shutdown func()
}

func (d Deps) validate() error {
	var missing []string
	if d.Registry == nil {
		missing = append(missing, "Registry")
	}
	if d.Store == nil {
		missing = append(missing, "Store")
	}
	if d.Journal == nil {
		missing = append(missing, "Journal")
	}
	if d.Version == "" {
		missing = append(missing, "Version")
	}
	if d.DataAddr == nil {
		missing = append(missing, "DataAddr")
	}
	if d.AdminAddr == nil {
		missing = append(missing, "AdminAddr")
	}
	if d.Shutdown == nil {
		missing = append(missing, "Shutdown")
	}
	if len(missing) > 0 {
		return fmt.Errorf("admin: incomplete Deps, missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Install mounts the control plane on srv: the simulacra.admin.v1 ConnectRPC
// routes, GET /healthz, and the h2c wrapping that lets a native gRPC client
// reach them over cleartext HTTP/2.
//
// It takes the *http.Server rather than returning a handler because the h2c
// handler and the *http2.Server driving it must be paired with that specific
// server: http2.ConfigureServer is what registers the graceful-shutdown hook
// that reaches hijacked h2c connections, and without it the admin plane keeps
// serving after the server reports it has stopped. Handing back a bare
// http.Handler would let a caller wire it up in a way that silently leaks
// connections.
func Install(srv *http.Server, deps Deps) error {
	if srv == nil {
		return errors.New("admin: Install requires a non-nil *http.Server")
	}
	if err := deps.validate(); err != nil {
		return err
	}

	mux := http.NewServeMux()
	path, handler := adminv1connect.NewControlServiceHandler(&controlService{deps: deps})
	mux.Handle(path, handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})

	h2s := &http2.Server{}
	srv.Handler = h2c.NewHandler(mux, h2s)
	// Mandatory, not tuning. See the doc comment above.
	if err := http2.ConfigureServer(srv, h2s); err != nil {
		return fmt.Errorf("admin: configuring http/2: %w", err)
	}
	return nil
}
```

Create `internal/admin/control.go`:

```go
package admin

import (
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

// controlService implements simulacra.admin.v1.ControlService.
//
// The embedded Unimplemented handler is scaffolding: it satisfies the
// generated interface while the three RPCs are filled in one at a time. It is
// removed once Shutdown lands, so a future RPC added to the contract fails the
// build here rather than returning Unimplemented at runtime.
type controlService struct {
	adminv1connect.UnimplementedControlServiceHandler

	deps Deps
}
```

Replace `internal/admin/doc.go` with:

```go
// Package admin implements the Simulacra control plane: ConnectRPC handlers for
// the simulacra.admin.v1 API, served over h2c alongside a /healthz endpoint.
//
// The package owns what is served — routes, health endpoint, and protocol
// configuration. When it runs belongs to the server package, which supplies
// Deps and drives the lifecycle.
package admin
```

- [ ] **Step 4: Promote `golang.org/x/net` and run the tests**

Run: `go mod tidy && go build ./... && go test ./internal/admin/ -count=1`
Expected: `go.mod` now lists `golang.org/x/net v0.53.0` in the direct require block (no longer marked `// indirect`); no new modules downloaded; all `TestInstall*` PASS along with the existing `contract_test.go` tests.

Confirm the dependency graph did not grow:

Run: `git diff --stat go.mod go.sum`
Expected: `go.mod` changed (one line moves between require blocks); `go.sum` unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/admin/admin.go internal/admin/control.go internal/admin/admin_test.go internal/admin/doc.go go.mod
git commit -m "feat(admin): h2c control-plane transport with /healthz and Deps validation"
```

---

### Task 2: `ControlService.GetServerInfo`

**Files:**
- Modify: `internal/admin/control.go`
- Create: `internal/admin/control_test.go`

**Interfaces:**
- Consumes: `admin.Deps` and the `controlService` shell from Task 1; `(*schema.Registry).Services()`, `(*stub.Store).Len() int`, `(*journal.Journal).Len() int`, `(*journal.Journal).Cap() int`, `(*journal.Journal).Record(*journal.Call)` from Phase 3.
- Produces: `(*controlService).GetServerInfo(context.Context, *connect.Request[adminv1.GetServerInfoRequest]) (*connect.Response[adminv1.GetServerInfoResponse], error)`.

A pure read. Every accessor it needs exists; this RPC adds no core surface.

- [ ] **Step 1: Write the failing test**

Create `internal/admin/control_test.go`:

```go
package admin_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestGetServerInfo' -count=1 -v`
Expected: FAIL — both tests report `unimplemented: simulacra.admin.v1.ControlService.GetServerInfo is not implemented`, from the embedded `UnimplementedControlServiceHandler`.

- [ ] **Step 3: Write the implementation**

Append to `internal/admin/control.go` (and add the imports `context`, `connectrpc.com/connect`, and `adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"`):

```go
// GetServerInfo is a pure read of state the server already owns.
func (c *controlService) GetServerInfo(
	_ context.Context,
	_ *connect.Request[adminv1.GetServerInfoRequest],
) (*connect.Response[adminv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&adminv1.GetServerInfoResponse{
		Version:         c.deps.Version,
		DataAddr:        c.deps.DataAddr(),
		AdminAddr:       c.deps.AdminAddr(),
		ServiceCount:    int32(len(c.deps.Registry.Services())),
		StubCount:       int32(c.deps.Store.Len()),
		JournalCount:    int32(c.deps.Journal.Len()),
		JournalCapacity: int32(c.deps.Journal.Cap()),
	}), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS (all of Task 1's tests plus both `TestGetServerInfo*`).

- [ ] **Step 5: Commit**

```bash
git add internal/admin/control.go internal/admin/control_test.go
git commit -m "feat(admin): ControlService.GetServerInfo"
```

---

### Task 3: `ControlService.Reset` and its presence rule

**Files:**
- Modify: `internal/admin/control.go`
- Modify: `internal/admin/control_test.go`

**Interfaces:**
- Consumes: `(*stub.Store).ResetStubs()`, `(*journal.Journal).Reset()` — both Phase 3, both already atomic. `ResetStubs` performs both halves (drop API-origin stubs, restore file-origin `times` budgets) under one lock, so no composition is needed here.
- Produces: `(*controlService).Reset(context.Context, *connect.Request[adminv1.ResetRequest]) (*connect.Response[adminv1.ResetResponse], error)`.

**The one way to get this wrong:** the contract's rule is *an omitted field means true* (reset it); explicit `false` skips. The generated `GetStubs()` flattens a nil pointer to `false`, which inverts the documented default. The handler must read the pointer.

- [ ] **Step 1: Write the failing tests**

Append to `internal/admin/control_test.go`:

```go
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
```

`compileAPIStub` builds a second stub to be added under `stub.OriginAPI`. Add it to the same file, following the loader path so nothing bypasses validation:

```go
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
```

The Phase 3 signatures these tests rely on, verified against the current tree: `func (s *Store) Add(c *Compiled) string` (`internal/stub/store.go:142` — always API-origin, stamping `Origin`, `Source` and the id), `func (s *Store) ReplaceOrigin(origin Origin, stubs []*Compiled) ([]string, error)` (`store.go:187`), and `func (s *Store) ResetStubs()` (`store.go:299`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestResetHonorsPresenceRule' -count=1 -v`
Expected: FAIL — every subtest reports `unimplemented: simulacra.admin.v1.ControlService.Reset is not implemented`.

- [ ] **Step 3: Write the implementation**

Append to `internal/admin/control.go`:

```go
// Reset implements the contract's presence rule: an omitted field means true
// (reset it), an explicit false skips.
//
// Written against the pointer rather than the generated GetStubs()/GetJournal(),
// which flatten nil to false and would invert the documented default — the
// single easiest way to get this RPC wrong.
func (c *controlService) Reset(
	_ context.Context,
	req *connect.Request[adminv1.ResetRequest],
) (*connect.Response[adminv1.ResetResponse], error) {
	if req.Msg.Stubs == nil || *req.Msg.Stubs {
		c.deps.Store.ResetStubs()
	}
	if req.Msg.Journal == nil || *req.Msg.Journal {
		c.deps.Journal.Reset()
	}
	return connect.NewResponse(&adminv1.ResetResponse{}), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS, all five `TestResetHonorsPresenceRule` subtests included.

- [ ] **Step 5: Commit**

```bash
git add internal/admin/control.go internal/admin/control_test.go
git commit -m "feat(admin): ControlService.Reset with contract presence semantics"
```

---

### Task 4: `ControlService.Shutdown`

**Files:**
- Modify: `internal/admin/control.go` (add the handler, remove the embedded `UnimplementedControlServiceHandler`)
- Modify: `internal/admin/control_test.go`

**Interfaces:**
- Consumes: `Deps.Shutdown func()`.
- Produces: `(*controlService).Shutdown(context.Context, *connect.Request[adminv1.ShutdownRequest]) (*connect.Response[adminv1.ShutdownResponse], error)`. After this task `controlService` implements `adminv1connect.ControlServiceHandler` outright, with no embedded fallback.

**The handler stays dumb.** It must not block on teardown: the response it is about to write is what teardown waits to drain (design §3.4 step 2).

- [ ] **Step 1: Write the failing tests**

Append to `internal/admin/control_test.go`:

```go
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
```

Add `"time"` to the imports of `internal/admin/control_test.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestShutdown' -count=1 -v`
Expected: FAIL — `unimplemented: simulacra.admin.v1.ControlService.Shutdown is not implemented`.

- [ ] **Step 3: Write the implementation**

Append to `internal/admin/control.go`:

```go
// Shutdown starts server teardown and returns. The handler stays dumb on
// purpose: it must not block, because the teardown it triggers waits on this
// very response to flush.
func (c *controlService) Shutdown(
	_ context.Context,
	_ *connect.Request[adminv1.ShutdownRequest],
) (*connect.Response[adminv1.ShutdownResponse], error) {
	c.deps.Shutdown()
	return connect.NewResponse(&adminv1.ShutdownResponse{}), nil
}
```

Then remove the scaffolding from the struct — all three RPCs now exist, so the fallback would only hide a future contract addition:

```go
// controlService implements simulacra.admin.v1.ControlService.
//
// It deliberately does not embed UnimplementedControlServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type controlService struct {
	deps Deps
}
```

Drop the now-unused `adminv1connect` import from `control.go` if nothing else in the file uses it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1 && go vet ./internal/admin/`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/admin/control.go internal/admin/control_test.go
git commit -m "feat(admin): ControlService.Shutdown and drop the unimplemented fallback"
```

---

### Task 5: Connection-tracking admin listener

**Files:**
- Create: `server/tracked.go`
- Create: `server/tracked_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces (all unexported, `package server`):
  - `newTrackedListener(inner net.Listener) *trackedListener`
  - `(*trackedListener).Accept() (net.Conn, error)` — satisfies `net.Listener` via the embedded inner listener
  - `(*trackedListener).drain() <-chan struct{}` — marks draining, returns a channel closed when no tracked connection remains
  - `(*trackedListener).closeAll()` — force-closes every tracked connection
  - `(*trackedListener).count() int` — test/diagnostic accessor
  - `trackedConn` wrapper type with `Close() error`

**Why this exists at all:** `http.Server.Shutdown` cannot supply the drain. For the hijacked h2c connections a native gRPC client creates it returns instantly and waits for nothing; for HTTP/1.1 it blocks until the request finishes. Neither gives a drain signal, and neither closes anything. So the server tracks connections itself.

**The trap this task exists to encode:** force-closing must go through the *wrapper's* `Close`, because only the wrapper untracks. Closing the inner `net.Conn` directly leaves the drain waiting on an entry that will never be removed — measured, not hypothesized (pre-verified fact 5).

- [ ] **Step 1: Write the failing tests**

Create `server/tracked_test.go`:

```go
package server

import (
	"net"
	"testing"
	"time"
)

// acceptedConn dials lis and returns both ends once the listener has wrapped
// the server side, so a test never races the accept.
func acceptedConn(t *testing.T, lis *trackedListener) (client net.Conn, server net.Conn) {
	t.Helper()
	type accepted struct {
		conn net.Conn
		err  error
	}
	done := make(chan accepted, 1)
	go func() {
		c, err := lis.Accept()
		done <- accepted{c, err}
	}()
	client, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatalf("Accept: %v", a.err)
		}
		return client, a.conn
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not return")
		return nil, nil
	}
}

func newTestTrackedListener(t *testing.T) *trackedListener {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	lis := newTrackedListener(inner)
	t.Cleanup(func() { _ = lis.Close() })
	return lis
}

func TestTrackedListenerCountsAcceptedConnections(t *testing.T) {
	lis := newTestTrackedListener(t)
	if got := lis.count(); got != 0 {
		t.Fatalf("count before accept = %d, want 0", got)
	}
	_, server := acceptedConn(t, lis)
	if got := lis.count(); got != 1 {
		t.Fatalf("count after accept = %d, want 1", got)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := lis.count(); got != 0 {
		t.Fatalf("count after close = %d, want 0", got)
	}
}

func TestTrackedListenerDrainClosesWhenLastConnectionCloses(t *testing.T) {
	lis := newTestTrackedListener(t)
	_, server := acceptedConn(t, lis)
	idle := lis.drain()
	select {
	case <-idle:
		t.Fatal("drain reported idle while a connection was still open")
	case <-time.After(50 * time.Millisecond):
	}
	_ = server.Close()
	select {
	case <-idle:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not report idle after the last connection closed")
	}
}

func TestTrackedListenerDrainClosesImmediatelyWhenAlreadyIdle(t *testing.T) {
	lis := newTestTrackedListener(t)
	select {
	case <-lis.drain():
	case <-time.After(time.Second):
		t.Fatal("drain on an idle listener did not report idle")
	}
}

// closeAll must untrack what it closes. Closing the inner net.Conn instead of
// the wrapper leaves the entry in the map forever, and every drain after that
// waits out the full budget for a connection that is already gone.
func TestTrackedListenerCloseAllUntracksConnections(t *testing.T) {
	lis := newTestTrackedListener(t)
	acceptedConn(t, lis)
	acceptedConn(t, lis)
	if got := lis.count(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	idle := lis.drain()
	lis.closeAll()
	if got := lis.count(); got != 0 {
		t.Fatalf("count after closeAll = %d, want 0", got)
	}
	select {
	case <-idle:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not report idle after closeAll")
	}
}

// A connection closed twice — once by the server, once by closeAll — must not
// double-count its removal or panic.
func TestTrackedConnCloseIsIdempotent(t *testing.T) {
	lis := newTestTrackedListener(t)
	_, server := acceptedConn(t, lis)
	_ = server.Close()
	_ = server.Close()
	lis.closeAll()
	if got := lis.count(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./server/ -run 'TestTracked' -count=1`
Expected: FAIL — `undefined: newTrackedListener`, `undefined: trackedListener`.

- [ ] **Step 3: Write the implementation**

Create `server/tracked.go`:

```go
package server

import (
	"net"
	"sync"
)

// trackedListener wraps a listener so teardown knows which connections are
// still open.
//
// http.Server.Shutdown cannot supply that. For the hijacked connections a
// native gRPC client creates over h2c it returns immediately and waits for
// nothing; for plain HTTP/1.1 it blocks until the request finishes. Neither
// gives a drain signal and neither closes anything, so the server tracks
// connections itself and force-closes what is left when the budget runs out.
type trackedListener struct {
	net.Listener

	mu       sync.Mutex
	conns    map[*trackedConn]struct{}
	draining bool
	idle     chan struct{}
}

func newTrackedListener(inner net.Listener) *trackedListener {
	return &trackedListener{
		Listener: inner,
		conns:    make(map[*trackedConn]struct{}),
		idle:     make(chan struct{}),
	}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, lis: l}
	l.mu.Lock()
	l.conns[tracked] = struct{}{}
	l.mu.Unlock()
	return tracked, nil
}

// drain marks the listener as draining and returns a channel closed once no
// tracked connection remains — immediately, if none remain already. Calling it
// more than once returns the same channel.
func (l *trackedListener) drain() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.draining = true
	if len(l.conns) == 0 {
		l.closeIdleLocked()
	}
	return l.idle
}

// closeAll force-closes every tracked connection. It closes the wrapper, not
// the connection underneath it, because only the wrapper's Close untracks:
// closing the inner net.Conn directly leaves the drain waiting forever on an
// entry that will never be removed.
func (l *trackedListener) closeAll() {
	l.mu.Lock()
	conns := make([]*trackedConn, 0, len(l.conns))
	for conn := range l.conns {
		conns = append(conns, conn)
	}
	l.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (l *trackedListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

func (l *trackedListener) remove(conn *trackedConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, conn)
	if l.draining && len(l.conns) == 0 {
		l.closeIdleLocked()
	}
}

// closeIdleLocked closes idle at most once. The caller holds mu, which is what
// makes the once-ness safe without a sync.Once.
func (l *trackedListener) closeIdleLocked() {
	select {
	case <-l.idle:
	default:
		close(l.idle)
	}
}

type trackedConn struct {
	net.Conn

	lis  *trackedListener
	once sync.Once
}

// Close untracks the connection exactly once, however many times it is called
// and whoever calls it — the server's own close, or teardown's force-close.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.lis.remove(c) })
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./server/ -run 'TestTracked' -race -count=1 -v`
Expected: PASS, all five tests, no race reports. `-count=1` is not optional: the test cache will replay a stale pass over a real race.

- [ ] **Step 5: Commit**

```bash
git add server/tracked.go server/tracked_test.go
git commit -m "feat(server): connection-tracking listener with bounded drain and force-close"
```

---

### Task 6: Teardown coordinator — two independent signals

**Files:**
- Modify: `server/server.go` (`Server` struct, `Start`, `normalizeServeErr`, `Wait`, `GracefulStop`, `Stop`, `Shutdown`; new `begin`, `escalate`, `supervise`, `runTeardown`, `stopDataPlane`, `firstErr`, `shutdownGrace`)
- Modify: `server/server_test.go` (new helpers and two new tests)

**Interfaces:**
- Consumes: `trackedListener` is *not* used yet — Task 7 wires it in. This task touches only the data plane and the watcher.
- Produces:
  - `const shutdownGrace = 5 * time.Second`
  - `(*Server).begin()`, `(*Server).escalate()`, `(*Server).supervise()`, `(*Server).runTeardown()`, `(*Server).stopDataPlane(deadline time.Time)`
  - `firstErr(errs ...error) error`
  - Server fields `dataServe chan error`, `adminServe chan error`, `stopping`, `force`, `teardown chan struct{}`, `stopOnce`, `forceOnce sync.Once`, `waitErr error`. `serveResult` and `waitOnce` are removed.
  - Redefined semantics for `GracefulStop`, `Stop`, `Shutdown`, `Wait`. They stay data-plane-only in behavior until Task 7; the *shape* changes here.
- Test helpers produced for later tasks: `startWithStub(t, stubYAML) *Server`, `openChatStream(t, srv) (grpc.ClientStream, protoreflect.MethodDescriptor)`, `recvChatText(t, stream, desc) string`, `waitFor(t, timeout, what string, cond func() bool)`.

**Why two signals and not one `sync.Once`:** `sync.Once.Do` blocks concurrent callers until the first invocation *returns*. With one `Once` around the whole sequence, a `Stop` racing an in-flight `GracefulStop` would block inside `Do` until the graceful sequence finished on its own — the escalation would be silently inert and the user's second Ctrl-C would appear to do nothing. This is a load-bearing dependency of `internal/cli/shutdown.go`, not an incidental one.

- [ ] **Step 1: Write the failing tests**

Append to `server/server_test.go`. Its current import block has `context`, `os`, `path/filepath`, `strings`, `sync`, `testing`, `time`, `grpc`, `insecure`, `healthpb`, `protojson`, and `dynamicpb`; this task also needs `errors`, `net`, and `google.golang.org/protobuf/reflect/protoreflect`.

```go
// blockingBidiStub parks the data plane inside a bidi handler: after sending
// the on_open message it waits in RecvMsg, so grpc's GracefulStop has an open
// stream to wait for and will not return on its own.
const blockingBidiStub = `
- method: shop.v1.OrderService/Chat
  respond:
    on_open:
      - message: { text: open }
`

// startWithStub starts a server with the given stub YAML and no admin plane.
func startWithStub(t *testing.T, stubYAML string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(stubYAML), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// openChatStream opens the bidi shop.v1.OrderService/Chat stream against the
// data plane and returns it with its method descriptor.
func openChatStream(t *testing.T, srv *Server) (grpc.ClientStream, protoreflect.MethodDescriptor) {
	t.Helper()
	conn, err := grpc.NewClient(srv.DataAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	const method = "/shop.v1.OrderService/Chat"
	desc, err := srv.reg.LookupMethod(method)
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	stream, err := conn.NewStream(ctx,
		&grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	return stream, desc
}

func recvChatText(t *testing.T, stream grpc.ClientStream, desc protoreflect.MethodDescriptor) string {
	t.Helper()
	msg := dynamicpb.NewMessage(desc.Output())
	if err := stream.RecvMsg(msg); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	return msg.Get(desc.Output().Fields().ByName("text")).String()
}

// waitFor polls cond until it holds, failing the test if the timeout expires.
// cond is evaluated once before the first sleep and once more after the
// deadline, so a condition that becomes true during the final sleep is not
// reported as a timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stop must cut short a GracefulStop that is already running. With a single
// sync.Once around the teardown sequence it could not: Once.Do blocks
// concurrent callers until the first invocation returns, so Stop would wait
// out the very graceful phase it is meant to abandon.
func TestStopEscalatesGracefulStopAlreadyInFlight(t *testing.T) {
	srv := startWithStub(t, blockingBidiStub)
	stream, desc := openChatStream(t, srv)
	// Receiving on_open proves the handler is inside its receive loop, so
	// GracefulStop genuinely has an open stream to wait on. Without this
	// synchronization the test could race ahead and pass vacuously.
	if got := recvChatText(t, stream, desc); got != "open" {
		t.Fatalf("on_open text = %q, want %q", got, "open")
	}

	gracefulDone := make(chan struct{})
	go func() { srv.GracefulStop(); close(gracefulDone) }()
	select {
	case <-gracefulDone:
		t.Fatal("GracefulStop returned with an open stream; the stream is not blocking it")
	case <-time.After(200 * time.Millisecond):
	}

	stopDone := make(chan struct{})
	go func() { srv.Stop(); close(stopDone) }()

	// The assertion is against a fraction of the budget on purpose: waiting
	// out the whole shutdownGrace is exactly the regression this catches, and
	// it would otherwise show up only as a slow suite.
	bound := time.After(shutdownGrace / 2)
	for _, w := range []struct {
		name string
		done <-chan struct{}
	}{{"Stop", stopDone}, {"GracefulStop", gracefulDone}} {
		select {
		case <-w.done:
		case <-bound:
			t.Fatalf("%s did not return within %v; the escalation is inert",
				w.name, shutdownGrace/2)
		}
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil after a forced stop", err)
	}
}

// A Serve loop that exits on its own must start teardown rather than leaving
// the server half-alive with Wait blocked on a goroutine that never exits.
func TestDataServeFailureStopsTheServer(t *testing.T) {
	broken := newBreakableListener(t)
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 16,
		Listen: func(network, address string) (net.Listener, error) {
			return broken, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	broken.breakNow()

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, errListenerBroken) {
			t.Fatalf("Wait = %v, want %v", err, errListenerBroken)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("Wait did not return after the data listener broke")
	}
	// A stop issued after the fact must return immediately, not hang.
	srv.GracefulStop()
}
```

Add the breakable listener to `server/server_test.go` (Task 8 reuses it):

```go
var errListenerBroken = errors.New("listener broken for test")

// breakableListener never yields a connection. It exists to fail on demand,
// the way a listener breaking underneath Serve looks to the serve loop: a
// plain (non-net.Error) error, which both grpc.Server.Serve and
// http.Server.Serve treat as fatal rather than retrying.
type breakableListener struct {
	net.Listener
	broken    chan struct{}
	closed    chan struct{}
	breakOnce sync.Once
	closeOnce sync.Once
}

func newBreakableListener(t *testing.T) *breakableListener {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	l := &breakableListener{
		Listener: inner,
		broken:   make(chan struct{}),
		closed:   make(chan struct{}),
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// Accept must unblock on Close as well as on breakNow. A fixture whose Accept
// only ever unblocks on breakNow deadlocks teardown: stopDataPlane joins the
// serve goroutine after forcing, and grpc's Serve cannot return while Accept
// is parked.
func (l *breakableListener) Accept() (net.Conn, error) {
	select {
	case <-l.broken:
		return nil, errListenerBroken
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *breakableListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

func (l *breakableListener) breakNow() { l.breakOnce.Do(func() { close(l.broken) }) }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./server/ -run 'TestStopEscalates|TestDataServeFailure' -count=1`
Expected: FAIL — `undefined: shutdownGrace`, and `TestDataServeFailureStopsTheServer` times out or reports a nil error, because today's `Wait` never stops the watcher path on a spontaneous exit and `GracefulStop` afterwards would re-enter the data plane's stop.

- [ ] **Step 3: Write the implementation**

In `server/server.go`, replace the `Server` struct, `Wait`, `GracefulStop`, `Stop`, and `Shutdown`, and extend `normalizeServeErr`:

```go
// shutdownGrace bounds runTeardown: the admin drain and the data plane's
// graceful phase share one budget, so that portion of teardown time is
// predictable rather than the sum of independent timeouts. The final joins in
// supervise — the serve goroutines and the watcher — happen after runTeardown
// returns and are not bounded by it; watcher.stop() in particular is
// documented as blocking for as long as an in-flight reload takes.
//
// It is a constant rather than an option because ShutdownRequest is empty and
// frozen: the policy is a decision, not a parameter. Unbounded graceful waits
// on open bidi streams — exactly what a failed test leaves behind — after the
// client has already been told OK, so it cannot learn it is hung. Adding a
// timeout field to the proto later is non-breaking.
const shutdownGrace = 5 * time.Second

// Server is a running Simulacra instance.
type Server struct {
	reg     *schema.Registry
	store   *stub.Store
	journal *journal.Journal
	data    *dataplane.Server
	lis     net.Listener

	dataServe  chan error
	adminServe chan error // nil while the admin plane is disabled

	// Starting teardown and escalating to force are two independent
	// idempotent signals, never one sync.Once: Once.Do blocks concurrent
	// callers until the first invocation returns, so a single Once would make
	// Stop wait out the GracefulStop it is meant to cut short.
	stopOnce  sync.Once
	stopping  chan struct{}
	forceOnce sync.Once
	force     chan struct{}

	// teardown is closed by supervise once everything has stopped. waitErr is
	// written before that close and read only after it, so every waiter sees
	// the final value.
	teardown chan struct{}
	waitErr  error

	watcher watcherRun
}

// normalizeServeErr maps the clean-stop sentinels of both planes to nil.
// grpc's Serve returns ErrServerStopped when a stop wins the race against the
// serve goroutine's first scheduling, and http.Server.Serve returns
// ErrServerClosed after Shutdown. Neither is a failure, so Wait must not
// surface them. The conformance harness already applies the grpc half
// (conformance/harness_test.go, waitForServe).
func normalizeServeErr(err error) error {
	if errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// begin starts teardown exactly once and returns immediately: the sequence
// runs on the supervisor's goroutine. That is what lets the Shutdown RPC
// trigger a teardown which is itself waiting for that RPC's response to flush.
func (s *Server) begin() { s.stopOnce.Do(func() { close(s.stopping) }) }

// escalate abandons the graceful phase exactly once.
func (s *Server) escalate() { s.forceOnce.Do(func() { close(s.force) }) }

// GracefulStop stops the whole server — every plane, plus the stub watcher —
// waiting for in-flight work up to shutdownGrace and then forcing.
func (s *Server) GracefulStop() { s.begin(); <-s.teardown }

// Stop stops the whole server, abandoning the graceful phase immediately. It
// returns promptly even when a GracefulStop is already in flight, which is
// what the CLI's second-signal path depends on.
func (s *Server) Stop() { s.begin(); s.escalate(); <-s.teardown }

// Wait blocks until the server has fully stopped, returning the error that
// stopped it (nil after a clean GracefulStop, Stop, or Shutdown). Safe to call
// from any number of goroutines.
//
// Before returning it joins both serve goroutines and the stub watcher — plus
// any reload already in flight — so once Wait returns nothing the server
// started is still touching the store or the Reporter. A Reporter that blocks
// delays that join (see Reporter).
func (s *Server) Wait() error { <-s.teardown; return s.waitErr }

// Shutdown stops the whole server, forcing a hard stop if ctx is done before
// the graceful phase completes, and returns once fully stopped.
func (s *Server) Shutdown(ctx context.Context) error {
	s.begin()
	select {
	case <-s.teardown:
	case <-ctx.Done():
		s.escalate()
		<-s.teardown
	}
	return s.waitErr
}

// supervise owns teardown. It waits for either an explicit stop or a Serve
// loop exiting on its own, runs the teardown sequence, joins everything the
// server started, and publishes the error Wait reports.
//
// Watching the serve loop is what keeps a half-dead server from going
// unnoticed: without it, a Serve that exited on a broken listener would leave
// the watcher running and Wait blocked on a goroutine that never exits.
func (s *Server) supervise() {
	var dataErr error
	dataDone := false

	select {
	case dataErr = <-s.dataServe:
		dataDone = true
	case <-s.stopping:
	}
	s.begin()
	s.runTeardown()
	if !dataDone {
		dataErr = <-s.dataServe
	}
	s.watcher.stop()

	s.waitErr = firstErr(dataErr)
	close(s.teardown)
}

// runTeardown is the one bounded sequence every stop path reaches. Every
// blocking step inside it is force-responsive, so an escalation short-circuits
// whichever one is in flight.
func (s *Server) runTeardown() {
	deadline := time.Now().Add(shutdownGrace)
	s.watcher.cancelNow()
	s.stopDataPlane(deadline)
}

// stopDataPlane runs grpc's graceful stop, abandoning it for Stop when the
// budget runs out or an escalation arrives. grpc-go's Stop is what unblocks a
// GracefulStop already in flight; the CLI has driven the pair this way since
// M1 (internal/cli/shutdown.go).
func (s *Server) stopDataPlane(deadline time.Time) {
	done := make(chan struct{})
	go func() {
		s.data.GracefulStop()
		close(done)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-s.force:
	case <-timer.C:
	}
	s.data.Stop()
	<-done
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
```

In `Start`, replace the `Server` literal and the tail:

```go
	s := &Server{
		reg:       reg,
		store:     store,
		journal:   jrnl,
		data:      data,
		dataServe: make(chan error, 1),
		stopping:  make(chan struct{}),
		force:     make(chan struct{}),
		teardown:  make(chan struct{}),
	}
```

```go
	go func() { s.dataServe <- normalizeServeErr(s.data.Serve(lis)) }()
	go s.supervise()
	return s, nil
```

Add `"net/http"` to the imports of `server/server.go`.

- [ ] **Step 4: Run the whole server suite to verify it passes**

Run: `go test ./server/ -race -count=1`
Expected: PASS — the two new tests plus every pre-existing test, including `TestStopImmediatelyAfterStartReportsCleanStop` (50 iterations of the stop-wins-the-race path) and `TestShutdownWithCanceledContextReturnsPromptly`.

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS. The CLI is untouched and still compiles: `GracefulStop`, `Stop`, and `Wait` keep their signatures.

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/server_test.go
git commit -m "refactor(server): bounded teardown coordinator with independent stop and force signals"
```

---

### Task 7: The admin plane — bind, serve, stop

**Files:**
- Create: `server/admin.go`
- Create: `server/admin_test.go`
- Modify: `server/server.go` (`Options.AdminAddr`, `Server.adminSrv`/`adminLis` fields, `Start` bind order, `supervise` joins `adminServe`, `runTeardown` calls `stopAdminPlane`)

**Interfaces:**
- Consumes: `admin.Install`, `admin.Deps` (Task 1); `newTrackedListener`, `(*trackedListener).drain/closeAll/count` (Task 5); `begin`, `escalate`, `force`, `runTeardown`, `shutdownGrace`, `firstErr` (Task 6).
- Produces:
  - `Options.AdminAddr string` — empty disables the admin plane.
  - `(*Server).AdminAddr() net.Addr` — bound address, nil when disabled.
  - `var server.Version = "dev"` — a package var; `GetServerInfoResponse.version` reads it.
  - `(*Server).startAdminPlane(addr string, listen func(string, string) (net.Listener, error)) error`
  - `(*Server).stopAdminPlane(deadline time.Time)` — steps 1–3 of design §3.4
  - `addrString(lis net.Listener) string`
  - Server fields `adminSrv *http.Server`, `adminLis *trackedListener`.
  - Test helpers for Tasks 8–9: `startWithAdmin`, `adminClient`, `adminGRPCConn`, `getServerInfoOverGRPC`, `holdActiveHTTP1Request`, `recordingListener`, `recordingReporter`.

**Start order** (design §3.2): registry/store/journal/dataplane → watcher → bind data listener → bind admin listener → install → start both serve goroutines and the supervisor. Binding admin *before* any serve goroutine starts is what makes a fatal bind failure cheap to unwind: there is nothing running to join.

- [ ] **Step 1: Write the failing tests**

Create `server/admin_test.go`:

```go
package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

// startWithAdmin starts a server with both planes on ephemeral ports. Every
// test binds :0 — the default admin port :6566 must never appear in a test, or
// parallel runs collide.
func startWithAdmin(t *testing.T, opts Options) *Server {
	t.Helper()
	if len(opts.ProtoDirs) == 0 && len(opts.DescriptorSetPaths) == 0 {
		opts.ProtoDirs = []string{"../testdata/protos"}
	}
	if opts.DataAddr == "" {
		opts.DataAddr = "127.0.0.1:0"
	}
	if opts.AdminAddr == "" {
		opts.AdminAddr = "127.0.0.1:0"
	}
	if opts.JournalSize == 0 {
		opts.JournalSize = 16
	}
	srv, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// adminClient speaks the Connect protocol over HTTP/1.1 — the path the CLI and
// Go SDK use. Its own transport keeps idle connections from leaking between
// tests.
func adminClient(t *testing.T, srv *Server) adminv1connect.ControlServiceClient {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	return adminv1connect.NewControlServiceClient(client, "http://"+srv.AdminAddr().String())
}

// adminGRPCConn dials the admin plane the way a native gRPC client does:
// cleartext HTTP/2 with prior knowledge, which h2c serves by hijacking the
// connection.
func adminGRPCConn(t *testing.T, srv *Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(srv.AdminAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func getServerInfoOverGRPC(ctx context.Context, conn *grpc.ClientConn) (*adminv1.GetServerInfoResponse, error) {
	out := &adminv1.GetServerInfoResponse{}
	err := conn.Invoke(ctx, adminv1connect.ControlServiceGetServerInfoProcedure,
		&adminv1.GetServerInfoRequest{}, out)
	return out, err
}

// holdActiveHTTP1Request opens a connection to the admin plane and writes a
// complete request head whose Content-Length body never arrives. net/http
// treats a connection as active from the first byte read, so
// http.Server.Shutdown blocks on it — the case that makes step 1 of teardown
// need a context that force cancels.
//
// The settle sleep is the one soft spot: the connection is tracked at accept,
// which is observable, but "active" happens when the server's read loop
// consumes the head, which is not. Too short a settle only makes a test weaker
// (teardown finishes early and the assertion passes vacuously); it cannot
// produce a false failure.
func holdActiveHTTP1Request(t *testing.T, srv *Server) {
	t.Helper()
	addr := srv.AdminAddr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial admin plane: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	head := "POST " + adminv1connect.ControlServiceResetProcedure + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Type: application/proto\r\n" +
		"Content-Length: 64\r\n" +
		"\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatalf("write request head: %v", err)
	}
	waitFor(t, 2*time.Second, "the admin listener to track the connection",
		func() bool { return srv.adminLis.count() > 0 })
	time.Sleep(200 * time.Millisecond)
}

// recordingListener notes whether it was closed, so a failed Start can be
// checked for a leaked listener.
type recordingListener struct {
	net.Listener
	mu     sync.Mutex
	closed bool
}

func (l *recordingListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.Listener.Close()
}

func (l *recordingListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// recordingReporter collects every message the server reports.
type recordingReporter struct{ messages chan string }

func newRecordingReporter() *recordingReporter {
	return &recordingReporter{messages: make(chan string, 64)}
}

func (r *recordingReporter) Printf(format string, args ...any) {
	select {
	case r.messages <- format:
	default:
	}
}

func (r *recordingReporter) PrintErrln(args ...any) {
	select {
	case r.messages <- "err":
	default:
	}
}

// M3 §14 lists grpc-java <-> connect-go h2c as a top risk deferred to Phase 9.
// A native grpc-go client proves the transport now, when a fix is cheap,
// rather than discovering it inside the milestone's exit criterion.
//
// Post-review: this test originally asserted only Version, AdminAddr and
// DataAddr — three of GetServerInfo's seven fields — leaving the server →
// admin wiring seam (startAdminPlane passing s.store/s.journal, not a fresh
// instance) untested by anything outside internal/admin's hand-built Deps.
// See Post-review amendments.
func TestAdminPlaneServesNativeGRPCClientOverH2C(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
`), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	const journalSize = 16
	srv := startWithAdmin(t, Options{StubDirs: []string{dir}, JournalSize: journalSize})
	conn := adminGRPCConn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := getServerInfoOverGRPC(ctx, conn)
	if err != nil {
		t.Fatalf("GetServerInfo over h2c: %v", err)
	}
	if info.Version != Version {
		t.Errorf("version = %q, want %q", info.Version, Version)
	}
	if info.AdminAddr != srv.AdminAddr().String() {
		t.Errorf("admin_addr = %q, want %q", info.AdminAddr, srv.AdminAddr().String())
	}
	if info.DataAddr != srv.DataAddr().String() {
		t.Errorf("data_addr = %q, want %q", info.DataAddr, srv.DataAddr().String())
	}
	// Pin GetServerInfo to the running server's own registry, store and
	// journal rather than to some other instance. StubDirs above makes
	// StubCount non-zero so this assertion actually discriminates a
	// mis-wired store instead of passing vacuously as 0 == 0.
	if info.ServiceCount != int32(srv.ServiceCount()) {
		t.Errorf("service_count = %d, want %d (srv.ServiceCount())", info.ServiceCount, srv.ServiceCount())
	}
	if info.StubCount != int32(srv.StubCount()) {
		t.Errorf("stub_count = %d, want %d (srv.StubCount())", info.StubCount, srv.StubCount())
	}
	if info.JournalCapacity != int32(journalSize) {
		t.Errorf("journal_capacity = %d, want %d (the JournalSize the server was started with)",
			info.JournalCapacity, journalSize)
	}
}

func TestAdminPlaneServesHealthz(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	resp, err := http.Get("http://" + srv.AdminAddr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("/healthz body = %q, want %q", body, "ok")
	}
}

func TestAdminAddrIsNilWhenDisabled(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	if got := srv.AdminAddr(); got != nil {
		t.Fatalf("AdminAddr with the admin plane disabled = %v, want nil", got)
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want %q — M4 sets a real one via ldflags", Version, "dev")
	}
}

// A server asked for a control plane that cannot provide one must not start
// half-working.
func TestAdminBindFailureIsFatalAndLeavesNothingRunning(t *testing.T) {
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "stub.yaml")
	if err := os.WriteFile(stubPath, []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: one }
`), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	bindErr := errors.New("admin bind refused for test")
	reporter := newRecordingReporter()
	var dataLis *recordingListener
	calls := 0
	_, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		Watch:       true,
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
		Reporter:    reporter,
		// Start binds the data listener first and the admin listener second
		// (design §3.2), so call 2 is the admin bind.
		Listen: func(network, address string) (net.Listener, error) {
			calls++
			if calls == 1 {
				inner, lerr := net.Listen(network, address)
				if lerr != nil {
					return nil, lerr
				}
				dataLis = &recordingListener{Listener: inner}
				return dataLis, nil
			}
			return nil, bindErr
		},
	})
	if err == nil {
		t.Fatal("Start with a failing admin bind succeeded; it must be fatal")
	}
	if !errors.Is(err, bindErr) {
		t.Fatalf("Start error = %v, want it to wrap %v", err, bindErr)
	}
	if dataLis == nil {
		t.Fatal("the data listener was never bound; check the Listen call order assumption")
	}
	if !dataLis.isClosed() {
		t.Fatal("the data listener was left open after a fatal admin bind failure")
	}

	// A watcher left running would reload on the next change and report it.
	drainMessages(reporter)
	if err := os.WriteFile(stubPath, []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: two }
`), 0o644); err != nil {
		t.Fatalf("rewrite stub: %v", err)
	}
	select {
	case msg := <-reporter.messages:
		t.Fatalf("the stub watcher is still running after a fatal admin bind failure: %q", msg)
	case <-time.After(time.Second):
	}
}

func drainMessages(r *recordingReporter) {
	for {
		select {
		case <-r.messages:
		default:
			return
		}
	}
}

// The discriminating test. Asserting only that Shutdown returned, or that a
// fresh dial is refused, passes against the broken mechanism: the surviving
// h2c connection is invisible to both. This holds one open across shutdown and
// tries again on that same connection.
//
// The elapsed-time bound on GracefulStop+Wait is what makes the retried-RPC
// assertion below actually load-bearing. Without it, a missing
// http2.ConfigureServer is invisible: http.Server.Shutdown still returns
// immediately (it closes listeners unconditionally), the warmed connection
// then sits undrained until the force-close backstop in step 3 kills it
// after the full shutdownGrace, and the retried RPC still fails — just
// roughly 5s slower. The force-close would silently absorb the defect into
// latency instead of a failure. With ConfigureServer wired correctly, an
// idle h2c connection drains to zero in milliseconds, so teardown finishes
// far inside the budget.
func TestAdminPlaneStopsServingAfterWait(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	conn := adminGRPCConn(t, srv)
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer warmCancel()
	if _, err := getServerInfoOverGRPC(warmCtx, conn); err != nil {
		t.Fatalf("warm-up GetServerInfo: %v", err)
	}

	start := time.Now()
	srv.GracefulStop()
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	// Against a fraction of the budget: taking the whole shutdownGrace is
	// exactly the bug (the force-close backstop masking a missing
	// http2.ConfigureServer), and it would otherwise pass as a merely slow
	// test.
	if elapsed := time.Since(start); elapsed > shutdownGrace/2 {
		t.Fatalf("GracefulStop+Wait took %v, want well under %v; the admin drain "+
			"likely fell through to the force-close backstop instead of draining "+
			"the warmed h2c connection on its own", elapsed, shutdownGrace/2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := getServerInfoOverGRPC(ctx, conn); err == nil {
		t.Fatal("the admin plane served an RPC on a pre-existing connection after Wait returned")
	}
}

// Phase 4b's WatchCalls makes this load-bearing: a client tailing calls holds a
// connection open indefinitely, and only the bound stops it holding teardown
// open too. Tested before the RPC that needs it exists.
func TestLongLivedAdminRequestDoesNotHoldTeardownOpen(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	holdActiveHTTP1Request(t, srv)

	start := time.Now()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("teardown was held open by a long-lived admin request")
	}
	if elapsed := time.Since(start); elapsed > shutdownGrace+2*time.Second {
		t.Fatalf("teardown took %v, want no more than about %v", elapsed, shutdownGrace)
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// The HTTP/1.1 detail is what makes this cover step 1 of the sequence: an h2c
// connection leaves adminSrv.Shutdown returning instantly, so the blocking
// path would go untested and the bug would survive a green suite.
func TestStopEscalatesAdminShutdownBlockedOnHTTP1(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	holdActiveHTTP1Request(t, srv)

	gracefulDone := make(chan struct{})
	go func() { srv.GracefulStop(); close(gracefulDone) }()
	select {
	case <-gracefulDone:
		t.Fatal("GracefulStop returned instantly; the held request is not blocking teardown")
	case <-time.After(200 * time.Millisecond):
	}

	stopDone := make(chan struct{})
	go func() { srv.Stop(); close(stopDone) }()

	// Against a fraction of the budget: waiting the whole shutdownGrace is
	// exactly the bug, and it would otherwise pass as a merely slow test.
	bound := time.After(shutdownGrace / 2)
	for _, w := range []struct {
		name string
		done <-chan struct{}
	}{{"Stop", stopDone}, {"GracefulStop", gracefulDone}} {
		select {
		case <-w.done:
		case <-bound:
			t.Fatalf("%s did not return within %v; adminSrv.Shutdown is not force-responsive",
				w.name, shutdownGrace/2)
		}
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./server/ -run 'TestAdmin|TestVersion|TestLongLived|TestStopEscalatesAdmin' -count=1`
Expected: FAIL to compile — `opts.AdminAddr undefined`, `srv.AdminAddr undefined`, `Version undefined`, `srv.adminLis undefined`.

- [ ] **Step 3: Write the implementation**

Create `server/admin.go`:

**Post-review:** the block below is `server/admin.go` as it stands after the final review's Fix A and Fix E, not as this task originally wrote it. Fix A added `ReadHeaderTimeout`/`IdleTimeout` to the `http.Server` literal — an unauthenticated, on-by-default plane must not let a slow-header peer pin a connection and its goroutine indefinitely — while deliberately leaving `ReadTimeout`/`WriteTimeout` unset, since either would cap a whole request/response and break Phase 4b's `WatchCalls` server-streaming RPC. Fix E removed the `addrString` helper: `s.adminLis` is a `*trackedListener`, and a nil `*trackedListener` passed through `addrString`'s `net.Listener` parameter became a non-nil interface holding a nil pointer, so its `lis == nil` guard never caught it — dead protection, not real protection (unreachable in practice, since the closure only runs once `adminLis` is set, but misleading to a reader). `AdminAddr`'s closure now checks the concrete `*trackedListener` directly; `DataAddr`'s inlines `s.lis.Addr().String()` since `s.lis`'s static type is already `net.Listener` and is always bound by the time the closure could run. See Post-review amendments.

```go
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/yinghanhung/simulacra/internal/admin"
)

// Version is the version string GetServerInfo reports. Nothing in the tree
// carries a version yet and the frozen contract has the field, so it defaults
// to "dev"; M4's release work sets it via ldflags.
var Version = "dev"

// startAdminPlane binds the admin listener and builds the http.Server that
// serves the control plane on it. It runs before either serve goroutine
// starts, so a failure leaves nothing running for the caller to join.
func (s *Server) startAdminPlane(addr string, listen func(network, address string) (net.Listener, error)) error {
	lis, err := listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	s.adminLis = newTrackedListener(lis)

	srv := &http.Server{
		// A slow-header peer must not be able to pin a connection — and its
		// server goroutine — for the life of the process: this plane is on by
		// default and unauthenticated. ReadHeaderTimeout bounds only the time
		// to read the request head; IdleTimeout bounds only time between
		// requests on a keep-alive connection. Deliberately absent:
		// ReadTimeout and WriteTimeout, which would each cap the duration of
		// a whole request/response and so would break Phase 4b's WatchCalls
		// server-streaming RPC. Do not add them to "complete" this set.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	deps := admin.Deps{
		Registry: s.reg,
		Store:    s.store,
		Journal:  s.journal,
		Version:  Version,
		// s.lis is always bound by the time this closure could run — Start
		// binds the data listener before calling startAdminPlane — so it
		// needs no nil guard. It stays a func because binding happens before
		// GetServerInfo is ever called, not before this struct literal is
		// built: a ":0" port is not known until Listen returns.
		DataAddr: func() string { return s.lis.Addr().String() },
		// s.adminLis is a *trackedListener, not a plain net.Listener: a nil
		// *trackedListener boxed into a net.Listener interface is a non-nil
		// interface holding a nil pointer, so an `iface == nil` check would
		// not catch it. Checking the concrete pointer here does.
		AdminAddr: func() string {
			if s.adminLis == nil {
				return ""
			}
			return s.adminLis.Addr().String()
		},
		// begin, not a blocking stop: the handler must return so its response
		// can flush, which is exactly what the drain in stopAdminPlane waits
		// for.
		Shutdown: s.begin,
	}
	if err := admin.Install(srv, deps); err != nil {
		_ = s.adminLis.Close()
		s.adminLis = nil
		return err
	}
	s.adminSrv = srv
	s.adminServe = make(chan error, 1)
	return nil
}

// stopAdminPlane is steps 1–3 of the teardown sequence.
//
// http.Server.Shutdown is asymmetric and both halves bite. For the hijacked
// connections a native gRPC client creates it returns instantly and waits for
// nothing; for plain HTTP/1.1 — the Connect path the CLI and Go SDK use — it
// blocks until the request finishes. So it can neither be trusted as a barrier
// nor treated as non-blocking, and the server drains and force-closes itself.
func (s *Server) stopAdminPlane(deadline time.Time) {
	if s.adminSrv == nil {
		return
	}

	// Step 1: stop accepting and start the h2 graceful shutdown (GOAWAY) that
	// http2.ConfigureServer registered. The context is bounded by the budget
	// and canceled by force, because this call blocks on in-flight HTTP/1.1
	// requests; without that, an escalation could not interrupt it and
	// teardown would sit here until the deadline. Its return means "stop
	// accepting and start draining", never "everything is finished".
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	unwatch := make(chan struct{})
	go func() {
		select {
		case <-s.force:
			cancel()
		case <-unwatch:
		}
	}()
	_ = s.adminSrv.Shutdown(ctx)
	close(unwatch)
	cancel()

	// Step 2: the real drain. This is what lets an in-flight ShutdownResponse
	// flush and lets long-lived requests end. Phase 4b's WatchCalls makes it
	// load-bearing: a client tailing calls holds a connection open
	// indefinitely, and only this bound stops it holding teardown open too.
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.adminLis.drain():
	case <-s.force:
	case <-timer.C:
	}

	// Step 3: force-close whatever is left. A timeout alone is not enough:
	// http.Server.Shutdown returns without terminating anything, so something
	// must actually close the sockets.
	s.adminLis.closeAll()
}
```

In `server/server.go`:

Add to `Options` (and extend its doc comment to mention the new field):

```go
	DataAddr           string   // data-plane listen address, e.g. ":6565"
	AdminAddr          string   // admin-plane listen address, e.g. ":6566"; empty disables it
```

Add to `Server`:

```go
	adminSrv *http.Server
	adminLis *trackedListener
```

Add the accessor next to `DataAddr`:

```go
// AdminAddr is the bound admin-plane listener address, or nil when the admin
// plane is disabled.
func (s *Server) AdminAddr() net.Addr {
	if s.adminLis == nil {
		return nil
	}
	return s.adminLis.Addr()
}
```

Replace the tail of `Start` (from the data `listen` call onward):

```go
	lis, err := listen("tcp", opts.DataAddr)
	if err != nil {
		s.watcher.stop()
		return nil, fmt.Errorf("listening on %s: %w", opts.DataAddr, err)
	}
	s.lis = lis

	if opts.AdminAddr != "" {
		// Fatal, matching the data plane's rule: a server asked for a control
		// plane that cannot provide one must not start half-working. Nothing
		// is serving yet, so the listener and the watcher are all there is to
		// undo.
		if err := s.startAdminPlane(opts.AdminAddr, listen); err != nil {
			_ = lis.Close()
			s.watcher.stop()
			return nil, err
		}
		go func() { s.adminServe <- normalizeServeErr(s.adminSrv.Serve(s.adminLis)) }()
	}

	go func() { s.dataServe <- normalizeServeErr(s.data.Serve(lis)) }()
	go s.supervise()
	return s, nil
```

Extend `runTeardown` — admin first, so the drain that flushes a `ShutdownResponse` happens before the data plane goes away:

```go
func (s *Server) runTeardown() {
	deadline := time.Now().Add(shutdownGrace)
	s.watcher.cancelNow()
	s.stopAdminPlane(deadline)
	s.stopDataPlane(deadline)
}
```

Extend `supervise` to join the admin serve goroutine (cross-plane *origination* is Task 8; this is only the join):

```go
	if !dataDone {
		dataErr = <-s.dataServe
	}
	var adminErr error
	if s.adminServe != nil {
		adminErr = <-s.adminServe
	}
	s.watcher.stop()

	s.waitErr = firstErr(dataErr, adminErr)
	close(s.teardown)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./server/ -race -count=1 -v -run 'TestAdmin|TestVersion|TestLongLived|TestStopEscalates'`
Expected: PASS. In particular `TestAdminPlaneStopsServingAfterWait` must pass — if it fails with the pre-existing connection still serving, `http2.ConfigureServer` is missing or is being called with a different `*http2.Server` than the one `h2c.NewHandler` wraps.

Run: `go test ./... -race -count=1`
Expected: PASS across the tree.

- [ ] **Step 5: Commit**

```bash
git add server/admin.go server/admin_test.go server/server.go
git commit -m "feat(server): serve the admin control plane over h2c with a bounded drain"
```

---

### Task 8: Cross-plane supervision

**Files:**
- Modify: `server/server.go` (`supervise`)
- Modify: `server/admin_test.go`

**Interfaces:**
- Consumes: `breakableListener`/`errListenerBroken` (Task 6 test helpers), `startWithAdmin`, `recordingListener` (Task 7).
- Produces: no new exported surface. `supervise`'s initial `select` gains the `adminServe` case and an `originErr` that `Wait` reports in preference to the plane that was merely told to stop.

**Why:** joining both serve goroutines is not sufficient. If either `Serve` loop exits unexpectedly, the sibling plane keeps running and `Wait` blocks forever on the goroutine that never exits — a half-working server that reports neither failure nor shutdown. After Task 7, an admin `Serve` that dies on its own does exactly that: `supervise` is parked on `dataServe`/`stopping` and never learns.

- [ ] **Step 1: Write the failing tests**

Append to `server/admin_test.go`:

```go
// startWithBreakableListeners starts both planes on listeners the test can
// break on demand. Start binds the data listener first and the admin listener
// second (design §3.2), which is how the two are told apart here.
//
// It also starts with a real stub directory and Watch: true so a listener
// failure injected here exercises supervise's watcher join
// (s.watcher.stop()), not just the cross-plane teardown: a server started
// with no watcher would leave that path uncovered by both failure-injection
// tests below.
func startWithBreakableListeners(t *testing.T) (srv *Server, data, adminL *breakableListener) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
`), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	data = newBreakableListener(t)
	adminL = newBreakableListener(t)
	calls := 0
	var err error
	srv, err = Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		Watch:       true,
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
		Listen: func(network, address string) (net.Listener, error) {
			calls++
			if calls == 1 {
				return data, nil
			}
			return adminL, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, data, adminL
}

func TestDataPlaneFailureTearsDownAdminPlane(t *testing.T) {
	srv, data, _ := startWithBreakableListeners(t)
	data.breakNow()

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, errListenerBroken) {
			t.Fatalf("Wait = %v, want the originating error %v", err, errListenerBroken)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("Wait never returned after the data listener broke; the admin plane is holding it open")
	}
	// The admin plane must be gone too, not merely un-joined.
	if _, err := net.DialTimeout("tcp", srv.AdminAddr().String(), time.Second); err == nil {
		t.Fatal("the admin plane still accepts connections after the data plane died")
	}
}

func TestAdminPlaneFailureTearsDownDataPlane(t *testing.T) {
	srv, _, adminL := startWithBreakableListeners(t)
	adminL.breakNow()

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, errListenerBroken) {
			t.Fatalf("Wait = %v, want the originating error %v", err, errListenerBroken)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("Wait never returned after the admin listener broke; supervise is not watching it")
	}
	// The data plane must be gone too, not merely un-joined.
	if _, err := net.DialTimeout("tcp", srv.DataAddr().String(), time.Second); err == nil {
		t.Fatal("the data plane still accepts connections after the admin plane died")
	}
}

// An intentional stop must report nil, not a plane's incidental serve error.
func TestIntentionalStopReportsNoError(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	srv.GracefulStop()
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait after GracefulStop = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./server/ -run 'TestAdminPlaneFailureTearsDownDataPlane' -count=1 -timeout 60s`
Expected: FAIL — the test times out at `shutdownGrace + 10s` with "supervise is not watching it", because `supervise`'s initial select has no `adminServe` case.

Run: `go test ./server/ -run 'TestDataPlaneFailureTearsDownAdminPlane' -count=1 -timeout 60s`
Expected: PASS already (the data case was wired in Task 6). Keep it: it is the other half of the pair, and it fails loudly if a later refactor drops the `dataServe` case.

- [ ] **Step 3: Write the implementation**

In `server/server.go`, replace `supervise`:

```go
// supervise owns teardown. It waits for either an explicit stop or the first
// Serve loop to exit on its own, runs the teardown sequence, joins everything
// the server started, and publishes the error Wait reports.
//
// Watching both serve loops is what keeps a half-dead server from going
// unnoticed: if one plane's Serve exits unexpectedly — a broken listener, an
// unrecoverable accept error — the sibling keeps running and Wait would
// otherwise block forever on a goroutine that never exits. Wait returns the
// originating error, so the caller learns why the server died rather than
// seeing a nil from the plane that was merely told to stop.
func (s *Server) supervise() {
	var originErr, dataErr, adminErr error
	dataDone := false
	adminDone := s.adminServe == nil

	select {
	case dataErr = <-s.dataServe:
		dataDone = true
		originErr = dataErr
	case adminErr = <-s.adminServe: // a nil channel blocks forever, as intended
		adminDone = true
		originErr = adminErr
	case <-s.stopping:
	}
	s.begin()
	s.runTeardown()
	if !dataDone {
		dataErr = <-s.dataServe
	}
	if !adminDone {
		adminErr = <-s.adminServe
	}
	s.watcher.stop()

	s.waitErr = firstErr(originErr, dataErr, adminErr)
	close(s.teardown)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./server/ -race -count=1 -timeout 120s`
Expected: PASS, all three new tests plus everything from Tasks 5–7.

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/admin_test.go
git commit -m "feat(server): supervise both planes so one failing takes the other down"
```

---

### Task 9: The `Shutdown` RPC, end to end

**Files:**
- Modify: `server/admin_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–8. This task adds no production code — the wiring landed in Task 7 (`Deps.Shutdown: s.begin`). It exists because the self-terminating RPC is the phase's riskiest behavior and its ordering guarantee is not implied by any test written so far.
- Produces: no new surface.

If any test here fails, the fix is in `stopAdminPlane` or `supervise`, not in a new abstraction. The most likely cause of a failing response is teardown reaching `closeAll` before the response flushed, i.e. step 2's drain not being consulted.

- [ ] **Step 1: Write the failing tests**

Append to `server/admin_test.go` (add `"connectrpc.com/connect"` to the imports):

```go
// The response flushes because the drain waits for its connection, not because
// adminSrv.Shutdown blocks. This asserts both halves: the client receives the
// response *and* the server subsequently stops.
func TestShutdownRPCRespondsThenTheServerStops(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	client := adminClient(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown RPC: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait after the Shutdown RPC = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server did not stop after the Shutdown RPC")
	}
}

// The same over h2c, where the connection is hijacked and the response path is
// the one http.Server.Shutdown cannot see at all.
func TestShutdownRPCOverH2CRespondsThenTheServerStops(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	conn := adminGRPCConn(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, adminv1connect.ControlServiceShutdownProcedure,
		&adminv1.ShutdownRequest{}, &adminv1.ShutdownResponse{}); err != nil {
		t.Fatalf("Shutdown over h2c: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait after the h2c Shutdown RPC = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server did not stop after the h2c Shutdown RPC")
	}
}

// Two entry points can start teardown — the RPC and Server.Shutdown — and both
// funnel through begin(), whose stopOnce starts the coordinator exactly once.
func TestShutdownIsSafeToCallTwice(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	ctx1, cancel1 := context.WithTimeout(context.Background(), shutdownGrace+5*time.Second)
	defer cancel1()
	if err := srv.Shutdown(ctx1); err != nil {
		t.Fatalf("first Shutdown = %v, want nil", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := srv.Shutdown(ctx2); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}

func TestShutdownRPCConcurrentWithServerShutdown(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	client := adminClient(t, srv)

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace+5*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()
	// The RPC may or may not land before the admin plane stops accepting;
	// either outcome is fine. What must hold is that nothing deadlocks and
	// Wait reports a clean stop.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{}))

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Server.Shutdown = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("Server.Shutdown raced with the Shutdown RPC and deadlocked")
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// The test that fails if the policy ever regresses to unbounded graceful. An
// open bidi stream is exactly what a failed test leaves behind, and grpc's
// GracefulStop waits for it forever.
func TestShutdownWithOpenBidiStreamCompletesWithinTheGraceBound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(blockingBidiStub), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	srv := startWithAdmin(t, Options{StubDirs: []string{dir}})
	stream, desc := openChatStream(t, srv)
	if got := recvChatText(t, stream, desc); got != "open" {
		t.Fatalf("on_open text = %q, want %q", got, "open")
	}

	client := adminClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown RPC: %v", err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server never stopped with an open bidi stream; the graceful wait is unbounded")
	}
	if elapsed := time.Since(start); elapsed > shutdownGrace+2*time.Second {
		t.Fatalf("teardown took %v, want no more than about %v", elapsed, shutdownGrace)
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./server/ -run 'TestShutdown' -race -count=1 -v -timeout 180s`
Expected: PASS. These are the acceptance tests for Tasks 6–8; they should pass without new production code.

If `TestShutdownRPCRespondsThenTheServerStops` fails with a transport error on the *response*, teardown is closing the connection before the response flushed — check that `stopAdminPlane` step 2 actually waits on `s.adminLis.drain()` and that `closeAll` runs only after it.

If `TestShutdownWithOpenBidiStreamCompletesWithinTheGraceBound` hangs, `stopDataPlane` is not falling back to `Stop` on the timer.

- [ ] **Step 3: Fix any failure in `server/admin.go` or `server/server.go`**

No new files. If everything passed in Step 2, this step is a no-op — record that and move on.

- [ ] **Step 4: Re-run the full suite**

Run: `go test ./... -race -count=1 -timeout 300s`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/admin_test.go
git commit -m "test(server): Shutdown RPC ordering, idempotence, and the grace bound"
```

---

### Task 10: Schema-less startup, gated on the admin plane

**Files:**
- Modify: `server/server.go` (`Start`, `buildRegistry`, `Options` doc comment)
- Modify: `server/admin_test.go`
- Modify: `server/server_test.go` (comment on `TestStartRequiresSchemaSource`)

**Interfaces:**
- Consumes: `Options.AdminAddr` (Task 7).
- Produces: no new surface. `buildRegistry` loses its schema-source check; `Start` gains it, gated on the admin plane.

**The rule:** admin on + no schema source → **starts**; every data-plane call answers `Unimplemented` until schemas arrive (Phase 4b's `RegisterSchemas`). This is the container/SDK boot path: the server must become healthy before any schema exists, because the SDKs register only after connecting. Admin off + no schema source → still fails; that server could never answer anything. `check` is untouched and still requires schema sources.

No data-plane change is needed: `handleUnknown` already returns `codes.Unimplemented` when `LookupMethod` fails (`internal/dataplane/server.go:111`), and `dataplane.New` registers health itself, so a schema-less server still serves `grpc.health.v1.Health`.

- [ ] **Step 1: Write the failing tests**

Append to `server/admin_test.go` (add `"google.golang.org/grpc/codes"` and `"google.golang.org/grpc/status"` to the imports):

```go
// The container/SDK boot path: healthy first, schemas later.
func TestStartWithoutSchemaSourceSucceedsWhenAdminIsOn(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start with no schema source and the admin plane on: %v", err)
	}
	t.Cleanup(srv.Stop)

	resp, err := http.Get("http://" + srv.AdminAddr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// Every data-plane call answers Unimplemented until schemas arrive.
	conn, err := grpc.NewClient(srv.DataAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The request and response types are irrelevant: the server rejects the
	// call at method lookup, before it reads a message.
	err = conn.Invoke(ctx, "/shop.v1.OrderService/GetOrder",
		&adminv1.GetServerInfoRequest{}, &adminv1.GetServerInfoResponse{})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("data-plane call on a schema-less server = %v (code %v), want Unimplemented", err, got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./server/ -run 'TestStartWithoutSchemaSourceSucceedsWhenAdminIsOn' -count=1`
Expected: `TestStartWithoutSchemaSourceSucceedsWhenAdminIsOn` FAILS with `at least one schema source is required...`.

- [ ] **Step 3: Write the implementation**

In `server/server.go`, move the check out of `buildRegistry` and into `Start`, just after the journal-size validation:

```go
	// A schema source is required only when the admin plane is off. With it
	// on, the server may boot empty and take schemas over the control plane
	// (Phase 4b's RegisterSchemas) — the container/SDK path, where the server
	// must be healthy before any schema exists. With it off, that server could
	// never answer anything, so it still refuses to start.
	if opts.AdminAddr == "" && len(opts.ProtoDirs) == 0 && len(opts.DescriptorSetPaths) == 0 {
		return nil, errors.New("at least one schema source is required (a proto directory or a descriptor set) unless the admin plane is enabled")
	}
```

And delete these three lines from `buildRegistry`:

```go
	if len(protoDirs) == 0 && len(descriptorSets) == 0 {
		return nil, errors.New("at least one schema source is required (a proto directory or a descriptor set)")
	}
```

Update the `Options` doc comment:

```go
// Options configures a server. At least one schema source (ProtoDirs or
// DescriptorSetPaths) is required unless AdminAddr is set: a server with no
// schema and no control plane could answer nothing. JournalSize must be
// greater than zero.
```

Add a line to `TestStartRequiresSchemaSource` in `server/server_test.go` making the gate explicit:

```go
// The requirement is gated on the admin plane being off, which is the case
// here (Options.AdminAddr is empty). The admin-on counterpart is
// TestStartWithoutSchemaSourceSucceedsWhenAdminIsOn in admin_test.go.
func TestStartRequiresSchemaSource(t *testing.T) {
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./server/ -race -count=1 -timeout 300s`
Expected: PASS, including the unchanged `TestStartRequiresSchemaSource`.

Run: `go test ./internal/cli/ -count=1`
Expected: PASS — `check` is untouched and still requires schema sources through `sources.buildRegistry` (`internal/cli/load.go:28`), which is a different code path from the one just relaxed.

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/server_test.go server/admin_test.go
git commit -m "feat(server): allow schema-less startup when the admin plane is enabled"
```

---

### Task 11: `serve --admin`

**Files:**
- Modify: `internal/cli/serve.go`
- Modify: `internal/cli/serve_test.go`

**Interfaces:**
- Consumes: `Options.AdminAddr`, `(*Server).AdminAddr()` (Task 7); the redefined `GracefulStop`/`Stop`/`Wait` (Task 6).
- Produces: the `--admin` flag on `serve`, defaulting to `:6566` (post-review: `127.0.0.1:6566` — see Post-review amendments); an `adminAddrOption(flag string) string` helper mapping `off` to the empty string.

**This is a user-visible change to every existing `serve` invocation:** a second port opens by default, per M3 §7. A bind failure names the address; `--admin off` is the documented opt-out.

**Why the CLI needs no other change:** `serve` hands `srv.GracefulStop` and `srv.Stop` to its signal waiter (`internal/cli/serve.go:65`) and blocks on `srv.Wait` (`serve.go:67`). Task 6 redefined all three as whole-server operations, so the existing wiring covers both planes. That works **only** because starting and escalating are separate signals: with a single `Once` around the sequence, the waiter's `force()` would block behind its own in-flight `graceful()` and the second Ctrl-C would do nothing.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/serve_test.go` (add imports `sync/atomic`, `io`, and `github.com/yinghanhung/simulacra/server`):

**Post-review:** the test below is as this task originally wrote it, asserting the pre-review default `:6566`. Fix C changed the default to `127.0.0.1:6566` (loopback only — the plane is unauthenticated) and the `DefValue` assertion in `internal/cli/serve_test.go` was updated to match; see Post-review amendments.

```go
func TestServeAdminFlagDefaultsTo6566(t *testing.T) {
	cmd := newServeCmd()
	flag := cmd.Flags().Lookup("admin")
	if flag == nil {
		t.Fatal("--admin flag is missing")
	}
	if flag.DefValue != "127.0.0.1:6566" {
		t.Fatalf("--admin default = %q, want %q", flag.DefValue, "127.0.0.1:6566")
	}
}

// runServe starts `serve` with args, waits for the readiness line, then cancels
// and returns the command's error and everything it printed.
func runServe(t *testing.T, listen func(string, string) (net.Listener, error), needle string, args ...string) (error, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newServeCmdWithListen(listen)
	ready := make(chan struct{})
	writer := &notifyWriter{needle: needle, seen: ready}
	cmd.SetOut(writer)
	cmd.SetErr(writer)
	cmd.SetArgs(args)
	result := make(chan error, 1)
	go func() { result <- cmd.ExecuteContext(ctx) }()
	select {
	case <-ready:
	case err := <-result:
		cancel()
		t.Fatalf("serve returned before printing %q: %v", needle, err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("serve never printed %q", needle)
	}
	cancel()
	select {
	case err := <-result:
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return err, writer.buf.String()
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not return within 20s of cancellation")
		return nil, ""
	}
}

func TestServeAdminOffBindsOnlyTheDataPlane(t *testing.T) {
	var binds atomic.Int32
	listen := func(network, address string) (net.Listener, error) {
		binds.Add(1)
		return net.Listen(network, address)
	}
	err, out := runServe(t, listen, "data plane listening",
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0",
		"--admin", "off",
		"--watch=false",
	)
	if err != nil {
		t.Fatalf("ExecuteContext = %v", err)
	}
	if got := binds.Load(); got != 1 {
		t.Fatalf("bound %d listener(s) with --admin off, want 1", got)
	}
	if strings.Contains(out, "admin plane") {
		t.Fatalf("output mentions the admin plane with --admin off:\n%s", out)
	}
}

func TestServeAdminAddressIsHonoredAndAnnounced(t *testing.T) {
	var binds atomic.Int32
	listen := func(network, address string) (net.Listener, error) {
		binds.Add(1)
		return net.Listen(network, address)
	}
	err, out := runServe(t, listen, "admin plane listening",
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0",
		"--admin", "127.0.0.1:0",
		"--watch=false",
	)
	if err != nil {
		t.Fatalf("ExecuteContext = %v", err)
	}
	if got := binds.Load(); got != 2 {
		t.Fatalf("bound %d listener(s) with --admin set, want 2", got)
	}
	if !strings.Contains(out, "admin plane listening on 127.0.0.1:") {
		t.Fatalf("output does not announce the bound admin address:\n%s", out)
	}
}

// The §5 redefinition, end to end: the CLI's signal waiter drives the
// whole-server GracefulStop/Stop pair and Wait returns.
func TestSignalShutdownStopsBothPlanes(t *testing.T) {
	srv, err := server.Start(context.Background(), server.Options{
		ProtoDirs:   []string{"../../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	if srv.AdminAddr() == nil {
		t.Fatal("AdminAddr = nil, want a bound admin listener")
	}

	sig := make(chan os.Signal, 2)
	done := make(chan struct{})
	go func() {
		waitAndShutdown(sig, 10*time.Second, srv.GracefulStop, srv.Stop, func(...any) {})
		close(done)
	}()
	sig <- os.Interrupt
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("signal shutdown did not complete with the admin plane enabled")
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if _, err := net.DialTimeout("tcp", srv.AdminAddr().String(), time.Second); err == nil {
		t.Fatal("the admin plane still accepts connections after the signal shutdown")
	}
}

func TestCheckStillRequiresSchemaSources(t *testing.T) {
	cmd := newCheckCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--stubs", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "at least one schema source is required") {
		t.Fatalf("check error = %v, want a schema-source-required error", err)
	}
}
```

Also update the existing `TestServeExecuteContextCancellationReturnsWithinBound`: it injects one shared `blockingListener` for every `Listen` call, which with the admin plane now on by default would hand the same listener to both planes. Add `"--admin", "off"` to its `cmd.SetArgs` list so it keeps testing exactly what it tested before — the data plane's cancellation path.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL — `TestServeAdminFlagDefaultsTo6566` reports "--admin flag is missing", and the two bind-count tests never see their readiness needles.

- [ ] **Step 3: Write the implementation**

In `internal/cli/serve.go`, add the flag variable and its registration:

```go
	var listen string
	var admin string
	var journalSize int
	var watchStubs bool
```

Register the flag next to the existing ones, keeping every current line intact:

**Post-review:** the block below is as this task originally wrote it, with default `:6566`. Fix C (a human decision taken during the final review) changed the default to `127.0.0.1:6566` and reworded the help text to say so — the admin plane is unauthenticated and serves `Shutdown` (terminates the process) and `Reset` (drops stubs and the journal), so it must not be reachable from other hosts unless asked for. `server/admin.go` and `internal/cli/serve.go` reflect the post-review version; see Post-review amendments.

```go
	src.register(cmd)
	cmd.Flags().StringVar(&listen, "listen", ":6565", "data-plane listen address")
	cmd.Flags().StringVar(&admin, "admin", "127.0.0.1:6566",
		`admin-plane listen address, or "off" to disable the control plane. `+
			`Loopback only by default — unauthenticated, it must not be reachable `+
			`from other hosts unless asked for; pass an explicit address such as `+
			`"0.0.0.0:6566" to allow that.`)
	cmd.Flags().IntVar(&journalSize, "journal-size", 1024, "number of recent data-plane calls to retain")
	cmd.Flags().BoolVar(&watchStubs, "watch", true, "watch stub directories and reload changes")
	return cmd
```

Pass it through to `server.Options`:

```go
			srv, err := server.Start(cmd.Context(), server.Options{
				ProtoDirs:          src.protoDirs,
				DescriptorSetPaths: src.descriptorSets,
				StubDirs:           src.stubDirs,
				DataAddr:           listen,
				AdminAddr:          adminAddrOption(admin),
				JournalSize:        journalSize,
				Watch:              watchStubs,
				Listen:             serveListen,
				Reporter:           output,
			})
```

Announce the admin address after the data-plane lines:

```go
			output.Printf("simulacra: data plane listening on %s\n", srv.DataAddr())
			output.Printf("  %d service(s) registered, %d stub(s) loaded — reflection and health enabled\n",
				srv.ServiceCount(), srv.StubCount())
			if addr := srv.AdminAddr(); addr != nil {
				output.Printf("simulacra: admin plane listening on %s\n", addr)
			}
```

Add the helper at the bottom of the file:

```go
// adminAddrOption maps the --admin flag onto server.Options.AdminAddr. One
// string carries both the address and the on/off decision, reading the way
// --listen already does; "off" is the documented opt-out from the default
// admin plane.
func adminAddrOption(flag string) string {
	if flag == "off" {
		return ""
	}
	return flag
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -race -count=1 -v`
Expected: PASS, including the amended `TestServeExecuteContextCancellationReturnsWithinBound`.

Run: `go build ./... && go vet ./... && go test ./... -race -count=1 -timeout 300s`
Expected: PASS across the tree.

Sanity-check the flag help by hand:

```bash
go run ./cmd/simulacra serve --help
```
Expected (pre-review): the output lists `--admin string   admin-plane listen address, or "off" to disable the control plane (default ":6566")`.

**Post-review:** the flag now defaults to `127.0.0.1:6566` with reworded help text; see Post-review amendments for the actual `--admin` help line as it stands now.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/serve.go internal/cli/serve_test.go
git commit -m "feat(cli): serve --admin, defaulting the control plane on at :6566"
```

---

## Verification checklist

Run before declaring the phase done, from the repository root:

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 -timeout 600s
```

Then confirm the phase's own guard rails:

- [ ] `git diff --stat e263e03..HEAD -- api/ gen/` is empty — the contract was not touched.
- [ ] `git diff --stat e263e03..HEAD -- internal/schema internal/stub internal/journal internal/match` is empty — no Phase 3 core package changed.
- [ ] `git diff e263e03..HEAD -- go.sum` is empty — no module was added.
- [ ] `grep -rn ':6566' --include='*_test.go' .` returns nothing (pre-review) — every test binds `:0`. **Post-review:** Fix C's human decision permits the literal in exactly one place — the `DefValue` assertion in `internal/cli/serve_test.go`, now `"127.0.0.1:6566"` — as long as no test *binds* it; see Post-review amendments.
- [ ] `grep -rn 'ConfigureServer' internal/admin/` returns the call in `admin.go` — the mandatory one is still there.
- [ ] `make lint-api` passes (unchanged protos, but the target is the repo default goal).

---

## Self-review notes

**Spec coverage.** Every section of the design maps to a task: §2 package boundary → Task 1; §3.1 options/accessors → Task 7; §3.2 start order → Task 7; §3.3 h2c shutdown mechanics → Tasks 1, 5, 7; §3.4 teardown sequence → Tasks 6 (steps 4–5) and 7 (steps 1–3); §3.5 two signals → Task 6; §3.6 supervision → Task 8; §4 ControlService → Tasks 2–4, with `server.Version` in Task 7; §5 CLI → Task 11 and the schema-less relaxation in Task 10; §6 testing → distributed across the tasks that own each behavior; §7 risks → each row has a named test. §8 decisions 1–9 are all implemented; decision 6 (error table deferred) is a non-action recorded under Global Constraints.

**Deliberate deviations from the design's sketch.** The design writes `begin()` as `stopOnce.Do(func() { go s.runTeardown() })`. This plan instead has `begin()` close a `stopping` channel that a single supervisor goroutine — started in `Start` — selects on alongside both serve results. The observable contract is identical (one teardown, `Stop` cutting short an in-flight `GracefulStop`, `Wait` returning the originating error), and it gives §3.6's supervision the same entry point rather than a second coordinator racing the first. `waitErr` needs no mutex because it is written by that one goroutine before `close(s.teardown)` and read only after a receive on that channel, inside `Wait`/`Stop`/`GracefulStop`/`Shutdown` — the ordering is enforced by the code, not by a contract imposed on callers.

**Known soft spot.** `holdActiveHTTP1Request` settles for 200ms after the connection is tracked, because "the server's read loop consumed the request head" is not observable from the client. The final review corrected an earlier claim here that too short a settle "can never produce a false failure": that holds for `TestLongLivedAdminRequestDoesNotHoldTeardownOpen`, which merely goes vacuous, but **not** for `TestStopEscalatesAdminShutdownBlockedOnHTTP1`, which fails if `GracefulStop` returns within 200ms — a connection `net/http` has not yet marked active is closed as idle, and that fatal fires. One caller degrades, the other can flake. If either needs to be stronger, Phase 4b's `WatchCalls` supplies a genuinely long-lived request with an observable start, and both tests should be rewritten against it.

---

## Post-review amendments

The final whole-branch review of this phase (conducted after all 11 tasks above had landed) found four Important findings that changed code the plan quoted verbatim, plus several cheap documentation corrections. This section records the four that touch the plan's own text; the fixes themselves landed as two follow-up commits on top of Task 11's. Two of the four were human decisions taken during the review, not autonomous findings — noted below.

1. **The admin `http.Server` had no request timeouts** (Task 7's `startAdminPlane`). An unauthenticated, on-by-default plane let a slow-header peer pin a connection and its server goroutine for the life of the process — the branch's own `holdActiveHTTP1Request` fixture demonstrated the hole. Fixed by setting `ReadHeaderTimeout: 10 * time.Second` and `IdleTimeout: 120 * time.Second` on the `http.Server` literal, with `ReadTimeout`/`WriteTimeout` deliberately left unset so a whole request/response is never capped — that would break Phase 4b's `WatchCalls` server-streaming RPC. Verified not to affect `holdActiveHTTP1Request`'s two callers: the fixture writes the full request head immediately, so `ReadHeaderTimeout` never has cause to fire, and the held connection is mid-request rather than idle, so `IdleTimeout` does not apply either.

2. **The data plane had no guaranteed graceful floor** (Task 6/7's `runTeardown`) — **human decision.** If the admin drain consumes the whole `shutdownGrace` budget — already possible with one stuck HTTP/1.1 request, and the normal case once Phase 4b's `WatchCalls` lets a client hold a connection open indefinitely — `stopDataPlane` received an already-expired deadline and hard-killed in-flight data-plane RPCs with zero graceful window. The human decided to floor the data phase: a new `dataGraceFloor = time.Second` constant, and `runTeardown` now extends the data-plane deadline to at least `time.Now().Add(dataGraceFloor)` after `stopAdminPlane` returns, never shortening it. Worst-case teardown is therefore `shutdownGrace + dataGraceFloor`, not `shutdownGrace` — both the `shutdownGrace` and `dataGraceFloor` doc comments in `server/server.go` say so. Pinned by a new discriminating test, `TestDataPlaneGetsGuaranteedGraceFloorWhenAdminDrainConsumesTheBudget` in `server/admin_test.go`, whose lower-bound assertion was proven to fail when `runTeardown` is reverted to pass `deadline` straight through.

3. **`--admin` defaulted to `:6566`, binding all interfaces** (Task 11) — **human decision.** The control plane is unauthenticated and serves `Shutdown` (terminates the process) and `Reset` (drops stubs and the journal), with Phase 4b adding schema and stub mutation to it. The human decided to default to loopback: `--admin`'s default is now `127.0.0.1:6566`, with help text saying so and naming `--admin 0.0.0.0:6566` as the way to opt into other hosts. `--admin off` is unaffected. The `DefValue` assertion in `internal/cli/serve_test.go` now expects `"127.0.0.1:6566"`; this is the one place besides `serve.go`'s flag default where the literal `:6566` is permitted to appear in the tree — no test may *bind* it.

4. **`TestAdminPlaneServesNativeGRPCClientOverH2C` left the server→admin wiring seam untested** (Task 7). It asserted only `Version`, `AdminAddr` and `DataAddr` — three of `GetServerInfo`'s seven fields — so `startAdminPlane` handing `admin.Deps` a fresh `stub.NewStore()`/`journal.New(n)` instead of the server's own `s.store`/`s.journal` would still have passed the whole suite; `ServiceCount`/`StubCount`/`JournalCount`/`JournalCapacity` were exercised only against hand-built `Deps` in `internal/admin/control_test.go`. Fixed by adding `ServiceCount`, `StubCount`, and `JournalCapacity` assertions against the running server's own accessors, and by starting the server with a stub directory (rather than `Options{}`) so `StubCount` is non-zero and the assertion can actually discriminate a mis-wired store instead of passing vacuously as `0 == 0`.

A fifth, unrelated seam — `addrString`'s nil guard in `server/admin.go`, which read as protection against a nil `s.adminLis` but could not provide it (a nil `*trackedListener` boxed into its `net.Listener` parameter is a non-nil interface holding a nil pointer) — was also fixed by removing the helper and inlining both accessors, one of which now checks the concrete `*trackedListener` directly. It is not one of the four Important findings above (it was unreachable in practice) but is included here because it touches the same `startAdminPlane` code block this section already amends.

---

**Plan complete.**

### Follow-up: transport hardening (post-merge)

A later review of the merged phase found two gaps in the planned transport protection — not
missing tasks, but holes the plan never contemplated. Both were reproduced before fixing and
mutation-tested after.

1. **Truncated h2c handshakes leaked connections.** `golang.org/x/net@v0.53.0`'s
   `h2c.initH2CWithPriorKnowledge` returns its preface read-error without closing the connection
   it hijacked, and its caller returns before installing `defer conn.Close()`. Hijacking also
   clears the deadline `http.Server` derived from `ReadHeaderTimeout`, so nothing bounded the
   read. Measured: a client that sent `PRI * HTTP/2.0\r\n\r\n` and disconnected left the socket
   and its tracking entry open past 11s. `internal/admin` now performs the prior-knowledge
   handshake itself, with an unconditional close and a `handshakeTimeout` bound, and delegates
   only the upgrade and HTTP/1.1 paths to `h2c`.
2. **Admin request bodies were unbounded.** `connect` defaults to unlimited, and neither the
   handlers nor the outer h2c handler capped the stream; an 8 MiB `GetServerInfo` request
   returned 200. Now capped at `maxRequestBytes` (4 MiB) by `connect.WithReadMaxBytes` for a
   single message plus `http.MaxBytesHandler` on both the outer path (which covers `h2cUpgrade`'s
   `io.ReadAll(r.Body)`) and the per-stream handler.

**Phase 4b must revisit `maxRequestBytes`:** `RegisterSchemas` carries descriptor sets and
`ReplaceAllStubs` carries stub documents. Raise it deliberately if either needs more; do not
remove the cap.
