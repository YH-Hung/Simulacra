# Simulacra M3 Phase 4a — Admin Transport, Lifecycle, and ControlService: Design

**Goal (M3 design §13.4, first half):** stand up the control plane — a ConnectRPC handler served
over h2c with `/healthz`, wired into the server facade's lifecycle and reachable via
`serve --admin` — and prove it end to end with `ControlService`, whose three RPCs need only
surfaces Phase 3 already shipped.

**Reference:** `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §4 (architecture), §5
(admin API and server wiring), §7 (CLI, schema-less startup), §11 (error handling), §14 (risks);
`docs/superpowers/specs/2026-08-21-m3-p3-core-extensions-design.md` (the core surfaces consumed
here); `PROPOSAL.md` §4.

Phase 3 is complete at `e263e03`. The `simulacra.admin.v1` contract was frozen in Phase 2 and is
not modified here.

---

## 1. Scope

### Why Phase 4 is split

M3 §13 lists Phase 4 as one phase: handlers, h2c server, `/healthz`, and `serve --admin`. That
bundles 14 RPCs across five services with a new transport, a CLI flag, a startup-rule change, and
a self-terminating RPC. Splitting puts every novel risk in the smaller phase:

- **4a (this design)** — the control plane exists, serves, and shuts down correctly. One service,
  `ControlService`, proves it end to end.
- **4b** — the four data services (`Schema`, `Stub`, `Journal`, `Verify`) plus the shared
  `DecodedMessage` / `MetadataEntry` rendering, as translation against a server that already
  works, reviewable service by service.

### In scope

1. `internal/admin`: `NewHandler(Deps) (http.Handler, error)` — ConnectRPC routes for
   `ControlService`, `GET /healthz`, h2c wrapping.
2. `ControlService` handlers: `GetServerInfo`, `Reset`, `Shutdown`.
3. `server`: `Options.AdminAddr`, `Server.AdminAddr()`, admin listener and `http.Server` folded
   into `Start` / `Shutdown` / `Wait`, and the bounded shutdown sequence.
4. `server.Version` — a package var (default `"dev"`) backing `GetServerInfoResponse.version`.
5. CLI: `serve --admin <addr>|off`, defaulting to `:6566`.
6. The schema-less startup relaxation, gated on the admin plane being enabled.

### Out of scope

- `SchemaService`, `StubService`, `JournalService`, `VerifyService`, and the `DecodedMessage` /
  `MetadataEntry` rendering they share — **Phase 4b**.
- The core-error → connect-code mapping table (M3 §11). 4a's three RPCs have no failure modes
  that reach it; building it before a caller exists would be speculative. 4b introduces it with
  the handlers that need it.
- CLI client commands — Phase 5. Reflection import — Phase 6. SDKs — Phases 7/9.
  Dockerfile — Phase 8.
- Admin-plane authentication and TLS — M3 non-goals (§2).
- Changes to `api/`, `gen/`, or any Phase 3 core package.

### Dependency note

`golang.org/x/net` is already in `go.mod` at v0.53.0 as an **indirect** dependency; using
`x/net/http2/h2c` promotes it to direct. No new module is downloaded. `connectrpc.com/connect`
landed in Phase 2.

---

## 2. Package boundary

`internal/admin` owns **what is served**; `server` owns **when it runs**. The split follows the two
independent questions, and keeps lifecycle in the one place that already handles it carefully (the
data-plane serve goroutine, the watcher join, the `ErrServerStopped` normalization).

```go
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

func NewHandler(deps Deps) (http.Handler, error)
```

`NewHandler` validates that every field is non-nil, registers
`adminv1connect.NewControlServiceHandler` on a `*http.ServeMux` at the path the generated
constructor returns, adds `GET /healthz` → `200 ok`, and returns the mux wrapped in
`h2c.NewHandler(mux, &http2.Server{})`.

h2c wrapping belongs here rather than in `server` because it is a property of *the handler*, not
of the listener: `h2c.NewHandler` is a decorator. Keeping it in `admin` means the package owns its
whole contract surface — routes, health endpoint, and protocol support — and Phase 4b extends it
by registering four more services on the same mux, touching nothing in `server`.

**Why funcs for addresses.** Passing bound address strings would force `NewHandler` to run after
both listeners bind, which inverts the natural construction order and makes the handler's
dependencies order-sensitive. Accessors resolve per request and remove the constraint entirely.

---

## 3. Lifecycle in `server`

### Options and accessors

`Options.AdminAddr string` — empty disables the admin plane. `Server.AdminAddr() net.Addr` returns
the bound address, or nil when disabled.

### Start order

1. registry → store → journal → `dataplane.New` (unchanged)
2. stub watcher (unchanged)
3. bind the data listener
4. **bind the admin listener, if `AdminAddr != ""`**
5. serve both in the background

An admin bind failure is **fatal**: it tears down the watcher and closes the data listener before
returning, matching the data plane's existing rule (M3 §11, "Admin listen failure at startup is
fatal"). A server that was asked for a control plane and cannot provide one must not start
half-working.

`http.Server.Serve` returns `http.ErrServerClosed` after a clean stop; that is normalized to nil
exactly as `grpc.ErrServerStopped` already is, so `Wait` does not report a clean shutdown as a
failure.

### Shutdown order: admin first, then data plane

This ordering is what makes the `Shutdown` RPC work, and it is the same for every caller:

1. `adminSrv.Shutdown(ctx)` — by contract this drains in-flight HTTP requests, **including the
   request still writing the `ShutdownResponse`**, before returning.
2. The data plane stops under a bounded grace: graceful first, force-stopped after
   `shutdownGrace`.
3. `Wait` joins both serve goroutines and the watcher.

**Bounded grace, not unbounded.** `ShutdownRequest` is empty and frozen, so the policy is a
decision, not a parameter. A pure graceful stop waits for in-flight data-plane RPCs including open
bidirectional streams, which can block forever — and an abandoned stream is exactly what a failed
test leaves behind. The caller has already received `OK` by then, so it has no way to learn it is
hung. `shutdownGrace` is an unexported 5s constant; adding a timeout field to the proto later is
non-breaking if anyone needs one.

The RPC and the embedder path (`Server.Shutdown`) reach the same sequence, guarded by a
`sync.Once`, so a client calling `Shutdown` twice — or an SDK racing its own `t.Cleanup` — runs
teardown once.

---

## 4. ControlService

**`GetServerInfo`** is a pure read: `Version`, `DataAddr()`, `AdminAddr()`,
`len(Registry.Services())`, `Store.Len()`, `Journal.Len()`, `Journal.Cap()`. Every accessor exists
after Phase 3; this RPC adds no core surface.

**`Reset`** implements the contract's presence rule — an omitted field means *true* (reset it),
explicit `false` skips:

```go
if req.Msg.Stubs == nil || *req.Msg.Stubs     { deps.Store.ResetStubs() }
if req.Msg.Journal == nil || *req.Msg.Journal { deps.Journal.Reset() }
```

Written against the pointer rather than the generated `GetStubs()`, which flattens nil to `false`
and would invert the documented default — the single easiest way to get this RPC wrong.
`Store.ResetStubs` already performs both halves (drop API-origin stubs, restore file-origin
budgets) under one lock, so no composition is needed here.

**`Shutdown`** calls `deps.Shutdown()` and returns the response. The handler stays dumb: it must
not block, because the teardown it triggers waits on this very response to flush.

**`server.Version`** is a new package var defaulting to `"dev"`. Nothing in the tree carries a
version today and the contract has the field; M4's release work sets it via ldflags.

---

## 5. CLI

```
simulacra serve ... [--admin :6566 | --admin off]
```

One string flag carries both the address and the on/off decision, reading the way `--listen`
already does. Default `:6566` — **the admin plane is on by default**, per M3 §7. This is a
user-visible change to every existing `serve` invocation: a second port opens. `--admin off` is
the documented opt-out.

**Schema-less startup.** Today `server.Start` refuses to start without `ProtoDirs` or
`DescriptorSetPaths`. That check is relaxed to apply only when the admin plane is disabled:

- admin on, no schema source → **starts**; every data-plane call answers `Unimplemented` until
  schemas arrive (Phase 4b's `RegisterSchemas`). This is the container/SDK boot path: the server
  must become healthy before any schema exists, because the SDKs register only after connecting.
- admin off, no schema source → still fails. That server could never answer anything.

`check` is untouched and still requires schema sources.

The `Unimplemented` behavior needs no data-plane change: `handleUnknown` already returns
`codes.Unimplemented` when `LookupMethod` fails (`internal/dataplane/server.go:111`), verified
against the current tree.

---

## 6. Testing

**Handler tests** run against `httptest.Server` with the generated connect client over HTTP/1.1 —
no ports, no h2c, no facade:

- `GetServerInfo` reports version, both addresses, and all four counts.
- `Reset` in all three presence cases: omitted (resets), explicit `true` (resets), explicit
  `false` (skips) — asserted independently for `stubs` and `journal`.
- `Shutdown` invokes its callback exactly once and still returns a response.
- `GET /healthz` returns 200 with body `ok`.
- `NewHandler` rejects incomplete `Deps`.

**Integration tests** through `server.Start`, where the real risk lives:

- **h2c interop**: a native **grpc-go** client dialing the admin port with insecure credentials
  and calling `GetServerInfo` over cleartext HTTP/2. M3 §14 lists grpc-java ↔ connect-go h2c as a
  top risk deferred to Phase 9; a native gRPC client proves the transport now, when a fix is
  cheap, rather than discovering it inside the milestone's exit criterion.
- **Shutdown with an open bidi data-plane stream**, asserting the call returns and the server
  stops within the grace bound. This is the test that fails if the policy ever regresses to
  unbounded graceful.
- `Shutdown` RPC: the response is received *and* `Wait` subsequently returns — proving the
  admin-drains-first ordering rather than assuming it.
- `Shutdown` called twice is safe.
- Admin bind failure (an already-bound address) is fatal, and leaves no listener or watcher
  running.
- Schema-less boot: `Start` with no schema source and admin on succeeds, `/healthz` returns 200,
  and a data-plane call returns `Unimplemented`.
- No schema source with admin off still fails.

**CLI tests**, following the existing `serve`/`check` pattern: `--admin off` opens no second port;
an explicit `--admin` address is honored; `check` still requires schema sources.

---

## 7. Risks

| Risk | Mitigation |
|---|---|
| h2c interop with native gRPC clients fails or needs configuration | Proven in 4a by a grpc-go client test rather than deferred to Phase 9's grpc-java leg |
| `http.Server.Shutdown` does not drain the in-flight `Shutdown` response, so the client sees a transport error | The integration test asserts the response is actually received before `Wait` returns; if it proves unreliable, the fallback is a short flush delay before draining, isolated to the server's shutdown sequence |
| Two shutdown entry points (RPC, `Server.Shutdown`) race | Both funnel through one `sync.Once`-guarded sequence; a double-`Shutdown` test covers it |
| Admin on by default breaks an existing user's `serve` (port already in use) | Fatal bind failure names the address; `--admin off` is documented in the flag help and the design |
| Default `:6566` collides during parallel test runs | Every test binds `:0` |

---

## 8. Decisions flagged for review

1. **Phase 4 split 4a/4b** — transport and lifecycle first, data services second.
2. **Bounded shutdown grace** (5s, unexported constant) rather than unbounded graceful, chosen
   because the frozen `ShutdownRequest` carries no timeout and a hung teardown is worse than a
   truncated call.
3. **Admin on by default** in `serve` — a user-visible change, per M3 §7.
4. **Schema-less boot gated on the admin plane**, so a server that could never answer still
   refuses to start.
5. **`server.Version` defaults to `"dev"`** — the version field needs a source before M4 supplies
   a real one.
6. **The error-mapping table is deferred to 4b**, where the first handlers that can fail arrive.
