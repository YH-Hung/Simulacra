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

1. `internal/admin`: `Install(*http.Server, Deps) error` — ConnectRPC routes for
   `ControlService`, `GET /healthz`, h2c wrapping and `http2.ConfigureServer`.
2. `ControlService` handlers: `GetServerInfo`, `Reset`, `Shutdown`.
3. `server`: `Options.AdminAddr`, `Server.AdminAddr()`, a connection-tracking admin listener, the
   bounded teardown sequence, and cross-plane supervision. `GracefulStop`, `Stop`, `Shutdown`, and
   `Wait` become **whole-server** operations covering both planes.
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

func Install(srv *http.Server, deps Deps) error
```

`Install` validates that every field is non-nil, registers
`adminv1connect.NewControlServiceHandler` on a `*http.ServeMux` at the path the generated
constructor returns, adds `GET /healthz` → `200 ok`, sets `srv.Handler` to the mux wrapped in
`h2c.NewHandler(mux, h2s)`, and calls **`http2.ConfigureServer(srv, h2s)`**.

It takes the `*http.Server` rather than returning a handler because the h2c handler and the
`*http2.Server` driving it must be paired with that specific server for shutdown to work at all
(§3.3). Handing back a bare `http.Handler` would let a caller wire it up in a way that silently
leaks connections. `admin` still owns its whole contract surface — routes, health endpoint, and
protocol configuration — and Phase 4b extends it by registering four more services on the same
mux, touching nothing in `server`.

**Why funcs for addresses.** Passing bound address strings would force `Install` to run after
both listeners bind, which inverts the natural construction order and makes the handler's
dependencies order-sensitive. Accessors resolve per request and remove the constraint entirely.

---

## 3. Lifecycle in `server`

### 3.1 Options and accessors

`Options.AdminAddr string` — empty disables the admin plane. `Server.AdminAddr() net.Addr` returns
the bound address, or nil when disabled.

### 3.2 Start order

1. registry → store → journal → `dataplane.New` (unchanged)
2. stub watcher (unchanged)
3. bind the data listener
4. **bind the admin listener, if `AdminAddr != ""`**
5. serve both under the supervisor (§3.4)

An admin bind failure is **fatal**: it tears down the watcher and closes the data listener before
returning, matching the data plane's existing rule (M3 §11). A server asked for a control plane
that cannot provide one must not start half-working.

`http.Server.Serve` returns `http.ErrServerClosed` after a clean stop; that is normalized to nil
exactly as `grpc.ErrServerStopped` already is.

### 3.3 Shutting down an h2c server — what actually works

**The obvious mechanism does not work, and this was verified rather than assumed.** A native gRPC
client speaks cleartext HTTP/2 with prior knowledge, so `h2c.NewHandler` **hijacks** the
connection and hands it to `http2.ServeConn`. `net/http` documents that `Server.Shutdown` neither
closes nor waits for hijacked connections. Measured against a real prior-knowledge h2c client:

| Probe | Result |
|---|---|
| `srv.Shutdown(ctx)` with a live h2c connection | returned in **0s**, `err=nil` |
| same client connection, after `Shutdown` returned | **still served an RPC** |
| with `http2.ConfigureServer(srv, h2s)` added | client had to re-dial — connection was terminated |
| `Shutdown` with a long-lived streaming request open | still returned in **0s**; connection stayed open |
| tracked-listener force-close after a bound | connection closed, `Serve` returned |
| `Shutdown` with an in-flight **HTTP/1.1** request | **blocked** — still blocked after 500ms |
| same, with its context canceled | returned in **0s**, `err=context canceled` (request itself not terminated) |

**`Shutdown` is asymmetric, and both halves bite.** For hijacked h2c connections it returns
instantly and waits for nothing. For plain HTTP/1.1 — the Connect protocol path the CLI and Go SDK
use — it blocks until the request finishes, exactly as documented. So it can neither be trusted as
a barrier nor treated as non-blocking: a teardown that simply calls it can hang there
indefinitely, never reaching the code that would time out or force. Canceling its context releases
it promptly, but does **not** terminate the request; only closing the connection does that.

Two conclusions drive the design. First, **`http2.ConfigureServer(srv, h2s)` is mandatory**, not
optional tuning: without it the graceful-shutdown hook never reaches h2c connections and the admin
plane keeps serving after the server claims to have stopped. Second, **`Shutdown`'s context bound
is meaningless here** — it returns immediately because it is not tracking the hijacked connection
at all, so it provides neither a drain nor an ordering guarantee.

The server therefore tracks connections itself. The admin listener is wrapped so every accepted
connection is recorded and removed on close, which gives both the drain signal and the ability to
force the issue.

### 3.4 The teardown sequence

One sequence, reached identically by the `Shutdown` RPC, `Server.Shutdown`, `GracefulStop`,
`Stop`, and the supervisor. A single deadline — `shutdownGrace = 5 * time.Second` — bounds the
whole thing, so teardown is predictable end to end rather than the sum of independent timeouts:

1. `adminSrv.Shutdown(ctx)` — closes the admin listener and starts the h2 graceful shutdown
   (GOAWAY) that `ConfigureServer` registered. **`ctx` is derived from the budget deadline and is
   also canceled by `force`**, because this call blocks on in-flight HTTP/1.1 requests (§3.3);
   without that, an escalation could not interrupt it and teardown would sit here until the
   deadline. Its return means "stop accepting and start draining", never "everything is finished".
2. **Wait for tracked admin connections to reach zero, bounded by the remaining budget.** This is
   the real drain: it is what lets the in-flight `ShutdownResponse` flush and lets streaming RPCs
   end. Phase 4b's `WatchCalls` makes this load-bearing — a client tailing calls holds a
   connection open indefinitely, and only a bound stops it from holding teardown open too.
3. **Force-close every remaining tracked connection.** A context timeout alone is not enough:
   `http.Server.Shutdown` returns without terminating anything, so something must actually close
   the sockets.
4. Stop the data plane: `GracefulStop`, force-stopped via `Stop` if the budget is exhausted.
5. Join both serve goroutines and the watcher.

**Bounded, not unbounded.** `ShutdownRequest` is empty and frozen, so the policy is a decision, not
a parameter. Unbounded graceful waits on open bidi streams — exactly what a failed test leaves
behind — after the client has already been told `OK`, so it cannot learn it is hung. Adding a
timeout field to the proto later is non-breaking.

The `Shutdown` RPC's response flushes because step 2 waits for its connection to drain, not
because step 1 blocks. That distinction is the whole reason this section exists.

### 3.5 Starting teardown and escalating are separate signals

Wrapping the whole sequence in one `sync.Once` would break the graceful-to-force escalation the
CLI depends on. `sync.Once.Do` blocks concurrent callers until the first invocation *returns*, so
if `GracefulStop` entered first, a concurrent `Stop` — the CLI's second-signal path — would block
inside `Do` until the graceful sequence finished on its own. The escalation would be silently
inert, and worse, `Stop` itself would hang for up to `shutdownGrace`: the user's second Ctrl-C
would appear to do nothing.

Starting and escalating are therefore two independent idempotent signals:

```go
stopOnce  sync.Once     // starts the teardown coordinator exactly once
forceOnce sync.Once     // escalates to force exactly once
force     chan struct{} // closed by forceOnce
teardown  chan struct{} // closed once teardown has fully completed
```

```go
func (s *Server) begin()    { s.stopOnce.Do(func() { go s.runTeardown() }) }
func (s *Server) escalate() { s.forceOnce.Do(func() { close(s.force) }) }

func (s *Server) GracefulStop() { s.begin(); <-s.teardown }
func (s *Server) Stop()         { s.begin(); s.escalate(); <-s.teardown }
func (s *Server) Wait() error   { <-s.teardown; return s.waitErr }
```

`Shutdown(ctx)` begins teardown, then waits on `teardown` and `ctx.Done()` together, escalating if
the context expires first.

**Every blocking step inside `runTeardown` is force-responsive**, so an escalation short-circuits
whichever one is in flight: `adminSrv.Shutdown` (step 1) runs under a context that `force` cancels,
the connection drain (step 2) selects on `force` alongside its timer, and the data plane's graceful
phase (step 4) is abandoned for `Stop`. Step 1 matters as much as the others — it is the one that
blocks on HTTP/1.1 traffic, and a `select` that is never reached is no better than no `select`. That is what makes `Stop` return promptly while `GracefulStop` is mid-flight,
which is precisely the CLI's contract.

### 3.6 Cross-plane supervision

Joining both serve goroutines is not sufficient. If either `Serve` loop exits unexpectedly — a
broken listener, an unrecoverable accept error — the sibling plane keeps running, and `Wait`
blocks forever on the goroutine that never exits, leaving a half-working server that reports
neither failure nor shutdown.

A supervisor goroutine watches both results. The **first** exit that is not part of an intentional
teardown (tracked by the same `stopOnce` that guards §3.5) triggers the full sequence against
the other plane and the watcher. `Wait` then returns the **originating** error once every join
completes, so the caller learns why the server died rather than seeing a nil from the plane that
was merely told to stop.

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

**`GracefulStop` and `Stop` become whole-server operations.** The existing `serve` command hands
`srv.GracefulStop` and `srv.Stop` to its signal handler
(`internal/cli/serve.go:65`) and then blocks on `srv.Wait`. Those methods stop only the data plane
today. Left alone, with the admin plane on by default, Ctrl-C would stop the data plane while the
admin listener kept running and `Wait` would hang forever on a goroutine that never exits — the
default path, broken for every user.

Redefining both to cover both planes fixes it without touching the CLI: `GracefulStop` starts the
§3.4 sequence and waits for it, `Stop` starts it and escalates to force, and `Shutdown(ctx)`
remains graceful-bounded-by-ctx then force. The CLI's existing 10s-then-force wrapper
(`internal/cli/shutdown.go`, which runs `graceful()` in a goroutine and calls `force()` on a
second signal, a timeout, or context cancellation) keeps working — **but only because starting and
escalating are separate signals (§3.5)**. That is a load-bearing dependency, not an incidental
one: with a single `Once` around the sequence, `force()` would block behind the in-flight
`graceful()` and the escalation would do nothing.

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
- `Install` rejects incomplete `Deps`.

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

Three tests exist specifically because the mechanism they cover was wrong in the first draft of
this design, and a plausible-looking implementation would be wrong the same way:

- **The admin plane is actually dead after `Wait` returns.** Hold an h2c client connection open
  across shutdown, then attempt another RPC **on that same connection**; it must fail. Asserting
  only that `Shutdown` returned, or that a fresh dial is refused, passes against the broken
  mechanism (§3.3) — the surviving connection is invisible to both.
- **Failure injection in both directions**: kill the data listener out from under `Serve` and
  assert the admin plane is torn down and `Wait` returns the originating error; then the same with
  the admin listener. Without the supervisor (§3.5) one of these hangs forever.
- **A long-lived admin request does not hold teardown open**: issue a streaming/blocking admin
  request, then shut down, and assert completion within the grace bound. This is the Phase 4b
  `WatchCalls` scenario, tested before the RPC that needs it exists.
- **`Stop` escalates a `GracefulStop` already in flight**: block teardown with an **active,
  blocking HTTP/1.1 request** — not a merely held connection and not an h2c one — then call
  `GracefulStop` from one goroutine and `Stop` from another, asserting both return **well inside
  `shutdownGrace`**. The assertion must be against a fraction of the budget, since waiting the full
  budget is exactly the bug. The HTTP/1.1 detail is what makes this test cover step 1: an h2c
  connection leaves `adminSrv.Shutdown` returning instantly, so the blocking path goes untested and
  the bug survives a green suite.

**CLI tests**, following the existing `serve`/`check` pattern: `--admin off` opens no second port;
an explicit `--admin` address is honored; `check` still requires schema sources; and — covering
the redefinition in §5 — a signal-driven shutdown **with the admin plane enabled** completes and
`Wait` returns, plus a context-cancellation equivalent.

---

## 7. Risks

| Risk | Mitigation |
|---|---|
| h2c interop with native gRPC clients fails or needs configuration | Proven in 4a by a grpc-go client test rather than deferred to Phase 9's grpc-java leg |
| h2c connections survive shutdown (verified: they do, without `ConfigureServer`) | `http2.ConfigureServer` plus connection tracking and force-close (§3.3–3.4); the same-connection test above is what proves it, since every weaker assertion passes against the broken version |
| A future refactor drops `http2.ConfigureServer` and silently reintroduces surviving connections | It is not tuning and is not optional; §3.3 records the measurements, and the same-connection test fails without it |
| `adminSrv.Shutdown` is called with a plain deadline context and blocks on HTTP/1.1 traffic past any escalation | Its context is canceled by `force` (§3.4 step 1); the escalation test drives an active HTTP/1.1 request specifically to exercise it |
| Teardown is collapsed back into a single `sync.Once`, making the CLI's second Ctrl-C inert | §3.5 states why the two signals are separate; the escalation test bounds itself well under `shutdownGrace`, so the regression fails loudly rather than merely running slowly |
| Two shutdown entry points (RPC, `Server.Shutdown`) race | Both funnel through `begin()`, whose `stopOnce` starts the coordinator exactly once (§3.5); a double-`Shutdown` test covers it |
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
7. **`GracefulStop` and `Stop` change meaning** from data-plane-only to whole-server. This is a
   behavior change to the `server` package's public API, taken because the alternative — a CLI
   that hangs on Ctrl-C once the admin plane defaults on — is worse, and because the existing
   names already read as whole-server operations.
8. **The admin drain is bounded and ends in a force-close**, sharing one `shutdownGrace` budget
   with the data-plane phase so total teardown time is predictable rather than additive.
9. **Starting teardown and escalating to force are separate idempotent signals**, not one
   `sync.Once`, so `Stop` can cut short a `GracefulStop` that is already running.
