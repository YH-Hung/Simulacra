# Simulacra M3 — The Test Story: Design

**Goal (roadmap §12):** Admin API (ConnectRPC), reflection schema import, JVM SDK + JUnit5 + Testcontainers, Go SDK, `calls tail`, journal/stub CLI.

**Exit criterion:** a grpc-java integration test using the JVM SDK passes in CI via Testcontainers.

**Reference:** `PROPOSAL.md` §4 (control plane), §5 (stub model), §6 (schema management), §8 (verification), §9 (CLI + SDKs), §12 (roadmap M3 row).

---

## 1. Where M2 left the codebase

Everything below builds on interfaces that exist today:

| Component | State after M2 | M3 change |
|---|---|---|
| `internal/schema.Registry` | Built once at startup (`AddProtoDir`, `AddDescriptorSetFile`, `AddFile`); read concurrently by the data plane; reflection serves from the live registry via `ServiceInfoProvider` | Becomes safely mutable at runtime (`RegisterSchemas` over the admin API) |
| `internal/stub.Store` | Priority/times selection; `Replace` swaps **all** stubs (used by the hot-reload watcher) | Origin tracking (file vs. API), stub IDs, `Add`/`Remove`/`ReplaceOrigin`, listing with hit counts |
| `internal/journal.Journal` | Ring buffer, `Filter`, `Reset`, `Verify` (exactly/at_least/at_most/never) with nearest-miss reports | `Watch` subscription for live tail; `StubID` on `Call` |
| `internal/dataplane` | All four shapes, reflection + health, nearest-miss diagnostics | Behaviorally unchanged; resolver wiring updated (`DescriptorResolver` takes the registry, not the raw `Files` — §6) |
| `internal/cli` | `serve`, `check` | `stub`, `calls`, `verify`, `schema` command groups; `--admin` flag on serve |
| `conformance/` | Protobuf corpus + shape suite (Go client) | Admin-driven leg (same scenarios, stubs created via API) |

Not in scope before M3 and still absent: `api/` protos, admin plane, SDKs, Dockerfile.

## 2. Scope

### In scope

1. **Admin API** — `api/simulacra/admin/v1/*.proto` (buf module), served with ConnectRPC on `:6566` (gRPC + JSON/HTTP + gRPC-Web from one handler).
2. **Core extensions** the admin API needs: mutable registry, stub origins/IDs, journal watch.
3. **CLI client commands** — `stub list|add|rm|export`, `calls list|tail`, `verify`, `schema register|list|import`.
4. **Reflection schema import** — `simulacra schema import --reflect host:port -o schema.binpb` (CLI-side).
5. **Go SDK** — `sdks/go`: `simulacratest` (in-process, `testing.T`-integrated) + testcontainers-go module.
6. **JVM SDK** — `sdks/java`: Kotlin-authored admin client + stubbing DSL, JUnit 5 extension, Testcontainers module, and the grpc-java integration test that is the milestone exit criterion.
7. **Dockerfile + CI** — minimal image for Testcontainers; buf lint/breaking; Java CI leg.

### Non-goals (M3)

- **Dashboard** (M4) — but the admin API is designed as its data source (gRPC-Web comes free with ConnectRPC).
- **Publishing** — no Maven Central, no GoReleaser, no published Docker image (all M4). CI builds a local dev image only.
- **TLS/mTLS on either plane** (M4, with the release hardening pass).
- **`VerifySequence`** (order-sensitive verification, proposal §8) — adding an RPC later is non-breaking in proto; `VerifyCalls` covers the exit criterion. First candidate for M3.5.
- **Server-side upstream import** (a `SchemaService.ImportUpstream` RPC where the *server* dials staging) — CLI-side import covers §6; the RPC can be added non-breaking later.
- **Admin-plane authentication** — same posture as WireMock/gripmock: an unauthenticated dev/test tool. Documented, revisited post-v1.
- **Rust/C++ SDKs** (post-v1; generated admin clients already work from day one once `api/` exists).

## 3. Approaches considered

### A. Stub wire format over the admin API

| Option | Trade-off |
|---|---|
| **A1. Typed proto mirror** of the whole stub grammar (Match/Respond/Stream messages) | Typed SDK builders and buf breaking-detection over stub structure — but two parallel grammars (YAML loader + proto) that must never drift, double validation, and every M2 feature (CEL, stream scripts, typed error details) re-modeled in proto. |
| **A2. Canonical document passthrough** — `CreateStub` carries the stub document as text; the server parses it with the *same strict loader* files use | One grammar, one validator, zero drift; loader diagnostics (already good) become API errors verbatim; `stub export` is trivial. SDKs render the document — no proto typing of stub internals. |
| **A3. Hybrid** — document in; structured *envelope* out (id, method, shape, priority, times, origin, hits + normalized document) | A2's single grammar, plus enough typed metadata for listings, the CLI, and the M4 dashboard. |

**Decision: A3.** The proposal already commits to it implicitly: stubs are "identical in shape whether loaded from files, POSTed to the admin API, or built through an SDK" (§5). The document is the contract; the envelope is derived, read-only metadata.

### B. Admin plane transport

| Option | Trade-off |
|---|---|
| **B1. ConnectRPC on its own port** (`:6566`, h2c) | gRPC + plain JSON + gRPC-Web from one implementation; `curl` is a supported client (§14.3). One extra dependency. |
| B2. Second grpc-go server | No new dependency, but no JSON/HTTP or gRPC-Web — loses two of the three protocols the proposal promises, and the dashboard would need a proxy in M4. |
| B3. Single port, mux data + admin | One port to remember, but the data plane's `UnknownServiceHandler` and reflection make routing ambiguous ("is `simulacra.admin.v1` a mocked service or the real admin?"), and it breaks the clean "mock what you like, even a service named like ours" property. |

**Decision: B1**, as proposed. h2c (HTTP/2 cleartext) on the admin listener so grpc-java's native client works without TLS.

### C. How the JVM SDK provisions schemas

| Option | Trade-off |
|---|---|
| C1. Mount a proto dir into the container (`withProtoDir`) | Matches the sandbox mental model; requires proto files present in the test module. |
| C2. **Classpath descriptor auto-registration** — grpc-java's generated `MethodDescriptor` exposes its `FileDescriptor`; the SDK walks dependencies, builds a `FileDescriptorSet`, and calls `RegisterSchemas` | Zero proto files in test resources; works wherever generated stubs are on the classpath (the common case, §9). Only covers types the test references. |
| C3. Both, C2 as the flagship path | Slightly more SDK surface. |

**Decision: C3.** C2 is the demo-quality DX ("no proto files in your test, ever"); C1 remains for tests exercising services without generated stubs.

## 4. Architecture

```
                        ┌────────────────────────────────────────────────┐
   gRPC clients ───────▶│ DATA PLANE :6565      (unchanged from M2)      │
                        ├────────────────────────────────────────────────┤
                        │ CORE                                           │
                        │  schema.Registry   ← now runtime-mutable       │
                        │  stub.Store        ← origins (file|api), IDs   │
                        │  journal.Journal   ← Watch() broadcast         │
                        ├────────────────────────────────────────────────┤
   CLI / SDKs / curl ──▶│ CONTROL PLANE :6566   internal/admin           │
   (gRPC, JSON, Web)    │  ConnectRPC handlers over net/http + h2c       │
                        │  GET /healthz (container wait strategy)        │
                        └────────────────────────────────────────────────┘

   api/simulacra/admin/v1/*.proto ──buf generate──▶ gen/simulacra/admin/v1 (Go, committed)
                                   └─gradle protobuf─▶ sdks/java (Java, build-time)
```

A new public facade package makes the server embeddable (needed by the Go SDK's in-process mode) and becomes the single wiring path:

- **`server/` (public)** — `server.Options{ProtoDirs, DescriptorSetPaths, StubDirs, DataAddr, AdminAddr, JournalSize, Watch}`; `server.Start(ctx, opts) (*server.Server, error)`; `Server.DataAddr()`, `Server.AdminAddr()`, `Server.Shutdown(ctx)`. Schema sources are optional when the admin plane is enabled (empty-registry boot, §7); with admin disabled, at least one source is required — the facade enforces both. `internal/cli/serve.go` refactors onto this facade (targeted cleanup: serve currently hand-wires registry/store/journal/watcher; that wiring moves behind `server.Start` and is reused by SDK, tests, and CLI identically).

## 5. Admin API (`api/simulacra/admin/v1`)

buf module at `api/` (`buf.yaml`, lint + breaking in CI from day one — the admin API is a public contract). Generated Go code is committed under `gen/simulacra/admin/v1` (package `adminv1`) so `go build ./...` works without buf installed; a `make generate` / CI check keeps it in sync.

### Services and RPCs

| Service | RPC | Notes |
|---|---|---|
| `SchemaService` | `RegisterSchemas(descriptor_set bytes)` | Parses a `FileDescriptorSet`. **All-or-nothing**: the set is applied to a candidate registry snapshot and swapped in only if every file registers cleanly (§6); on any conflict the RPC fails and the served registry is byte-for-byte untouched (rollback covered by a dedicated test). Idempotent: re-registering an identical file is a no-op; same path with different content → `INVALID_ARGUMENT` naming the file. |
| | `ListServices()` | Services/methods from the registry (dashboard + CLI `schema list`). |
| `StubService` | `CreateStub(document string)` → `Stub` | Document is **exactly one stub as a YAML/JSON mapping** (a sequence is rejected with an error pointing at the file grammar — files use lists, the API is per-stub). Both grammars share the same decoder boundary: the strict per-stub decode (`KnownFields`) + `Compiler.Compile` from M1/M2; `parseFile` wraps it in the list-of-documents shape, the API calls it directly. Diagnostics returned verbatim as `INVALID_ARGUMENT`. Tested in both YAML and JSON forms. |
| | `ListStubs()` → `[]Stub` | Envelope: `id, method, shape, priority, times, origin (FILE\|API), source, hits, document` (normalized). |
| | `DeleteStub(id)` | API-origin only; deleting a file-origin stub → `FAILED_PRECONDITION` ("owned by <file>; edit or remove the file"). |
| | `ReplaceAllStubs(documents []string)` | Each document is a single stub mapping (same shape as `CreateStub`). Atomically replaces **API-origin** stubs only (see §6). The test-isolation RPC (§8 of the proposal). |
| | `ExportStubs()` | Returns API-origin stubs as YAML documents (CLI writes files). |
| `JournalService` | `ListCalls(method?, limit?)` | Newest-first, fully decoded (below). |
| | `WatchCalls(method?)` (server-streaming) | Live tail; backpressure policy in §7. |
| | `ResetJournal()` | |
| `VerifyService` | `VerifyCalls(method, matcher document, times)` | `times` mirrors `journal.Times` (`oneof exactly/at_least/at_most` + `never`). Returns verdict, matched count, and on failure the actual calls with per-call nearest-miss explanations. |
| `ControlService` | `GetServerInfo()` | Version, both listen addresses, service/stub/journal counts. |
| | `Reset(optional bool stubs, optional bool journal)` | Presence-tracked (`optional`) because plain proto3 bools cannot distinguish omitted from explicit `false`: an **omitted** field means `true` (reset it), explicit `false` skips. Stub reset clears API-origin stubs and restores file-origin `times` budgets. |
| | `Shutdown()` | Graceful stop; lets SDKs tear down non-container servers. |

### Wire form of a journal call

Admin clients generally do not know the data-plane message types, so each decoded message travels as:

```proto
message DecodedMessage {
  string type_name = 1;   // fully-qualified message name
  bytes  wire_bytes = 2;  // canonical protobuf encoding
  string json = 3;        // protojson rendered against the server registry
}
```

`Call` carries `seq, method, metadata, requests[], responses[], status (code, message, details as google.protobuf.Any), matched_stub_id, start, duration`. This is proposal §8's "JSON form + raw bytes + descriptor reference" — argument capture works from any language via `json`, and typed capture works via `wire_bytes` + the client's own generated classes.

### Server wiring (`internal/admin`)

`net/http` server on `AdminAddr` wrapped in `h2c` (`golang.org/x/net/http2/h2c`) so native gRPC (grpc-java, grpc-go) works over cleartext; ConnectRPC handlers give JSON/HTTP and gRPC-Web on the same routes. Plus `GET /healthz` → `200 ok` (Testcontainers wait strategy; cheaper than a gRPC health probe from the JVM). New dependencies: `connectrpc.com/connect`, `golang.org/x/net`.

## 6. Core changes

### `stub.Store` — origins, IDs, counts

- `Compiled` gains `ID string` and `Origin` (`file` | `api`). File stubs reuse the existing `Source` string as the ID (`"<path-as-given>#<index>"` — the walk path includes the `--stubs` root, so two roots containing the same relative path yield distinct IDs). API stubs: server-assigned `"api-<seq>"`. `ReplaceOrigin` enforces ID uniqueness as a load error (catches the same root passed twice); a test covers duplicate relative paths across two roots. `Source` stays as today for files, `"api"` for API stubs.
- Store API: `Add(*Compiled) string`, `Remove(id) error`, `ReplaceOrigin(origin, []*Compiled)`, `List() []Info` (`Info` = envelope fields + hits; the existing per-entry `used` counter doubles as the hit count).
- The hot-reload watcher switches from `Replace` to `ReplaceOrigin(file, …)` — **hot reload can no longer clobber API-created stubs**, and `ReplaceAllStubs` cannot clobber file stubs. `Replace` (all-origin) is removed.
- Selection is unchanged except tie-breaking across origins: highest priority wins; on ties, **API-origin beats file-origin**, and within an origin the existing load-order rule holds. Rationale: a test overrides a sandbox default without priority arithmetic; documented in the stub-model docs.

### `schema.Registry` — runtime mutation (copy-on-write)

Method-level locking is not enough: the raw `*protoregistry.Files` currently escapes the registry — `Files()` is handed to grpc reflection (`dataplane/server.go`) and to the match compiler's CEL environment (`stub.NewCompiler`), and `Types()` binds it at construction. `protoregistry.Files` is not safe for mutation concurrent with lookups, so any of those held references would race with `RegisterSchemas`.

Design: the registry holds an **immutable snapshot** (`atomic.Pointer` to a `protoregistry.Files` + derived types view). Registration builds a *candidate* snapshot (re-registering the existing immutable descriptors plus the new files into a fresh `Files`) and swaps it in atomically — which also makes registration **all-or-nothing**: any conflict discards the candidate and the served snapshot is untouched (see `RegisterSchemas`, §5).

The atomic pointer protects *readers* only; it does not stop two concurrent registrations from both loading S0, building S0+A and S0+B, and silently losing one on the second store. A **writer mutex serializes the whole load→build→swap sequence** (registration is rare and cheap; readers never touch the mutex). A functional test — not just `-race`, which cannot see this lost-update — runs concurrent disjoint registrations and asserts both succeed and the final registry contains their union.

`Registry.Files()` is removed. The registry itself implements the resolver interfaces (`protodesc.Resolver`, plus the dynamic type lookups behind `Types()`), delegating every call to the current snapshot. Consumer changes — small, but they are data-plane changes:

- `dataplane`: `DescriptorResolver: reg.Files()` → `DescriptorResolver: reg`.
- `stub.NewCompiler` / `match.NewCompiler`: accept the registry (resolver interface), not `*protoregistry.Files`. Compilers are already constructed per load operation (`LoadDirs`, watcher reconcile, and now `CreateStub`), so each stub compiles against the snapshot current at that moment — a stub created after `RegisterSchemas` sees the new types.

The race test (`-race`) must exercise all three escape paths concurrently with registration: reflection lookups, CEL compile + eval, and dynamic `Any`/type resolution.

### `journal.Journal` — watch

`Watch(ctx) (<-chan *Call, func())` with a per-subscriber buffered channel (64). `Record` broadcasts non-blocking; a subscriber that falls 64 calls behind is dropped and its stream ends with `RESOURCE_EXHAUSTED` ("client too slow — reconnect"). `Call` gains `StubID` (alongside the existing `StubSource`).

## 7. CLI additions (`internal/cli`)

Client commands talk to a running server via the generated connect-go client (Connect protocol over HTTP/1.1 — no h2c needed client-side). Shared flag `--addr` (default `localhost:6566`, env `SIMULACRA_ADDR`); `--output json` on every read command.

```
simulacra stub list                      # envelopes: id, method, origin, hits, times
simulacra stub add -f stubs.yaml         # file grammar (lists/multi-doc); split
                                         #   syntactically client-side → one
                                         #   CreateStub mapping per stub
simulacra stub rm <id>
simulacra stub export -o ./stubs         # API-origin stubs → YAML files
simulacra calls list [--method M] [--limit N]
simulacra calls tail [--method M]        # WatchCalls; prints decoded protojson; on
                                         #   RESOURCE_EXHAUSTED warns and resumes from now
simulacra verify --method M [--match-file m.yaml] --times exactly=2
                                         # exit 0 pass / 1 fail (CI-scriptable);
                                         #   prints nearest-miss per actual call on failure
simulacra schema register -f api.binpb
simulacra schema list
simulacra schema import --reflect host:443 [--plaintext] -o schema.binpb
simulacra serve ... [--admin :6566 | --admin off]   # admin plane on by default
```

**Schema-less startup:** today `serve` refuses to start without `--proto`/`--descriptors` (`buildRegistry` errors). That rule is relaxed: with the admin plane enabled, `serve` may start with an **empty registry** — every data-plane call answers `UNIMPLEMENTED` until schemas arrive via `RegisterSchemas`. This is the container/SDK boot path (`/simulacra serve` with no flags): the server must become healthy *before* any schema exists, because the JVM/Go SDKs register schemas only after connecting. With `--admin off` and no schema source, startup still fails — that server could never serve anything. `check` keeps requiring schema sources.

**Reflection import** is a thin hand-rolled client over `grpc_reflection_v1` (v1alpha fallback): `ListServices`, then `FileContainingSymbol` recursion with dedupe into a `FileDescriptorSet`. We avoid a `jhump/protoreflect` dependency for ~150 lines we can conformance-test against our own reflection endpoint (Simulacra imports Simulacra — a test and a demo in one).

## 8. Go SDK (`sdks/go`)

Separate Go module (`github.com/yinghanhung/simulacra/sdks/go`) so testcontainers-go and its Docker-client dependency tree never touch the core module. Depends on the core module (facade + `gen/`).

- **`simulacratest`** — the `testing.T` path, in-process via `server.Start` on `:0` ports:

  ```go
  sim := simulacratest.Start(t)                    // t.Cleanup(shutdown); per-test ready in ms
  sim.RegisterFiles(t, orderv1.File_shop_v1_order_proto)  // walks deps → FileDescriptorSet →
                                                          //   RegisterSchemas; or Start with ProtoDirs
  sim.StubYAML(t, `method: shop.v1.OrderService/GetOrder ...`)
  sim.Stub(t, method).MatchField("order_id", "o-123").Respond(t, respMsg)  // thin builder
  conn := sim.Dial(t)                              // client conn to the data plane
  sim.Verify(t, method).MatchFile("m.yaml").Exactly(2)
  sim.Reset(t)                                     // ReplaceAllStubs(nil) + ResetJournal
  ```

  The builder is a deliberately thin renderer of the canonical stub document (typed `proto.Message` responses are rendered via protojson with proto field names); `StubYAML` is always available for full grammar access. Everything drives the admin API over loopback — the SDK exercises the same surface as every other client.

- **`simulacratc`** — testcontainers-go module: container from an image name, `/healthz` wait strategy, mapped-port accessors, the same client helpers.

## 9. JVM SDK (`sdks/java`)

Kotlin-authored, Java-first-class API (proposal §10: Kotlin's strength redirected to the JVM SDK). Gradle (Kotlin DSL) multi-module build, isolated from the Go toolchain; Java 11 bytecode baseline (built with a 17 toolchain). Admin transport is plain grpc-java stubs generated from `api/` by protobuf-gradle-plugin — the ConnectRPC server speaks native gRPC, so no Connect-specific JVM dependency is needed.

| Module | Contents |
|---|---|
| `simulacra-client` | Generated admin stubs + `SimulacraClient` (connect/close, register/stub/verify/journal/reset) + the stubbing DSL |
| `simulacra-junit5` | `SimulacraExtension`: lifecycle (`testcontainers(image)` \| `external(host, port)`), `beforeEach` = `ReplaceAllStubs([]) + ResetJournal` (§14.5: isolation is one line) |
| `simulacra-testcontainers` | `SimulacraContainer` (image name, `/healthz` wait, port accessors) |
| `it` | The exit-criterion integration test (not published) |

**DSL** renders the canonical stub document as JSON (`JsonFormat` with `preservingProtoFieldNames`, enums by name — matching the stub grammar's ergonomics rules) and posts `CreateStub`:

```java
simulacra.stub(OrderServiceGrpc.getGetOrderMethod())     // typed: schema auto-registered (§3-C2)
         .whenMessage(m -> m.field("order_id").eq("o-123").field("customer.region").in("EU","UK"))
         .whenMetadata("x-tenant", "acme")
         .expr("size(message.items) <= 10")               // CEL escape hatch
         .respond(GetOrderResponse.newBuilder()...build())
         .withDelay(Duration.ofMillis(50));

simulacra.verify(OrderServiceGrpc.getGetOrderMethod()).calledExactly(1);
```

`stub(MethodDescriptor)` walks the method's `FileDescriptor` dependency graph into a `FileDescriptorSet` and calls `RegisterSchemas` (idempotent, cached per client) — generated stubs on the test classpath mean **zero proto files in the test module**. A string-method + YAML-document overload covers the untyped case.

**Exit-criterion test** (`it` module, runs in CI): builds the local Docker image, starts `SimulacraContainer` via the JUnit extension, stubs `shop.v1.OrderService/GetOrder` through the DSL with typed messages, calls it with a real grpc-java client, asserts the response and `calledExactly(1)` — plus one negative case asserting the nearest-miss text from a failed `verify`.

## 10. Docker + CI

- **`Dockerfile`** — two-stage: Go build → `FROM scratch`, binary + `EXPOSE 6565 6566`, entrypoint `["/simulacra", "serve"]`. (Published multi-arch images are M4/GoReleaser; this image exists for Testcontainers.)
- **CI (`ci.yml`)** adds: `buf lint` + `buf breaking --against main` on `api/`; a check that `gen/` is in sync; `docker build -t simulacra:dev`; a `java` job (temurin 17 + Gradle cache) running `sdks/java` unit tests and the `it` leg against `simulacra:dev`; `sdks/go` module tests. Existing Go build/vet/race legs unchanged.
- **Conformance** gains an admin-driven leg: a representative slice of the shape suite re-run with stubs created via `CreateStub` instead of files, proving file/API parity of the single grammar.

## 11. Error handling

- Admin errors use canonical codes: `INVALID_ARGUMENT` (document/descriptor parse — loader and CEL diagnostics passed through verbatim, with source positions), `NOT_FOUND` (unknown stub ID/method), `FAILED_PRECONDITION` (deleting file-origin stubs), `RESOURCE_EXHAUSTED` (slow watch consumers).
- `verify` CLI and both SDKs surface the nearest-miss explanations exactly as the data plane produces them — one diagnostic engine, four surfaces (§14.4).
- Admin listen failure at startup is fatal (same as data plane); `--admin off` is the explicit opt-out.

## 12. Testing strategy

| Layer | Tests |
|---|---|
| Core extensions | Store origin/ID/tie-break units, including duplicate relative paths across two `--stubs` roots; registry `-race` suite exercising all three escape paths (reflection lookups, CEL compile/eval, dynamic `Any`/type resolution) concurrently with registration, the all-or-nothing rollback test (conflicting set → registry untouched), and the concurrent-writers union test (disjoint parallel registrations both land — a lost-update check `-race` cannot make); journal watch (broadcast, slow-consumer drop) |
| Admin handlers | In-process `server.Start` + connect-go client: every RPC, error-code contract, document→envelope round-trip; `CreateStub` documents in both YAML and JSON forms, sequence-shaped documents rejected |
| Reflection import | Against Simulacra's own reflection endpoint (registry in → identical descriptor set out) |
| CLI | Command tests against an in-process server (pattern from M1/M2 CLI tests) |
| Go SDK | Its own tests dogfood `simulacratest`; a testcontainers smoke test (CI-only, needs Docker) proving the boot contract: container reports healthy with **zero schemas**, then registers schemas, creates a stub, and completes a data-plane call entirely through the API |
| JVM SDK | DSL unit tests: golden JSON documents (no server needed); `it` integration leg = exit criterion |
| Conformance | Admin-driven parity leg (§10) |

## 13. Phasing

Each phase lands green on `main` independently:

1. **Server facade** — `server/` package; `serve` refactored onto it (no behavior change).
2. **`api/` protos + buf + committed Go codegen** (`gen/`), CI lint/breaking/sync checks.
3. **Core extensions** — store origins/IDs, registry mutability, journal watch.
4. **`internal/admin`** — handlers + h2c server + `/healthz`; `serve --admin`.
5. **CLI client commands** — `stub`, `calls`, `verify`, `schema register|list`.
6. **Reflection import** — `schema import --reflect`.
7. **Go SDK** — `simulacratest` + `simulacratc`.
8. **Dockerfile + CI image build** (front-loaded so JVM work has an image).
9. **JVM SDK** — client + DSL, JUnit5, Testcontainers, `it` leg in CI. **← milestone exit**
10. **Docs + conformance parity leg** — README quickstarts for both SDKs; admin-driven corpus slice.

## 14. Risks

| Risk | Mitigation |
|---|---|
| JVM SDK is the long pole (new toolchain, new ecosystem) | Dockerfile front-loaded (phase 8 before 9); DSL validated by golden-document unit tests before any container work; `external(host, port)` mode decouples SDK dev from Testcontainers plumbing |
| grpc-java ↔ connect-go h2c interop surprises | Well-trodden path (Connect's own conformance covers it); `it` leg exercises it end-to-end from phase 9's first day |
| Two stub surfaces drift (file vs. API) | The outer document shapes differ by design (files: lists; API: one mapping per stub), but there is only one per-stub decoder, compiler, and semantic grammar by construction (decision A3, §5); the conformance parity leg guards it |
| Committed `gen/` drifts from `api/` | CI regenerates and fails on diff |
| Gradle-in-monorepo friction (contributor setup) | `sdks/java` is fully self-contained; Go-only contributors never touch it; CI is the integration point |

## 15. Decisions flagged for review

1. **Stub wire model A3** (document in / envelope out) — locks the admin API away from typed stub-structure messages; revisiting after v1.0 means a v2 API surface.
2. **Tie-break rule** API-origin > file-origin at equal priority — behavior change visible to sandbox users who add API stubs.
3. **Java package / Maven coordinates** — provisionally `io.simulacra.*`; must be confirmed against a domain or moved to a `com.github.…` coordinate **before M4 publishing** (registry sweep is already a §13 risk item).
4. **`ReplaceAllStubs` scope** (API-origin only) — file stubs in a sandbox survive test-style resets; the alternative (nuke everything) was rejected because the watcher would resurrect files on the next change anyway.
