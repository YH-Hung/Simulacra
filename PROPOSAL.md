# Simulacra — Proposal

**A gRPC-native mock server.** MockServer's capability, protobuf-native ergonomics.

| | |
|---|---|
| Status | Proposal (v1 design) |
| Date | 2026-07-10 |
| License | Apache-2.0 |
| Audience | Public open source |

---

## 1. Summary

Simulacra is a standalone mock server for gRPC. It lets developers and CI pipelines stand in for real gRPC services: register schemas, define stubs (request matching → canned or templated responses), exercise clients against them, and verify what was received — across all four gRPC interaction patterns, from any client language.

The defining design decision: **everything is expressed in gRPC's own vocabulary** — services, methods, protobuf messages, metadata, status codes, deadlines. Existing tools bolt gRPC onto an HTTP mocking model (MockServer, WireMock) or offer gRPC-native serving with shallow matching and no verification (gripmock). Simulacra treats gRPC as the first-class citizen and inherits nothing from HTTP.

Core tech decision: a **single static Go binary** for the server; a **gRPC-defined admin API** so that idiomatic test SDKs for JVM, Go, Rust, and C++ are mostly generated code plus a thin ergonomic layer.

## 2. The gap

"MockServer for gRPC" today means choosing between three compromises:

### MockServer 6.x
MockServer added gRPC support and its feature list is real: descriptor loading, server streaming, record/forward, reflection, gRPC-Web. But architecturally, every gRPC request is **decoded to JSON and matched by the HTTP expectation engine** — synthetic `x-grpc-service` headers, `httpResponse` bodies with `grpc-status` headers, JsonPath matchers over converted payloads. Consequences:

- The mental model is HTTP-shaped. You write "HTTP expectations that happen to carry protobuf," not gRPC stubs. This is the ergonomic failure Simulacra exists to fix.
- Client streaming is aggregated into a single request body; bidirectional streaming is experimental behind a flag.
- Protobuf semantics leak through the JSON conversion: int64 precision, `bytes`, enums, `oneof`, well-known types, and typed error details are all mediated by a JSON representation the user must reason about.
- It's a JVM application — meaningful startup time and memory for a dev-loop tool.

### WireMock + gRPC extension
Same architecture, same consequences, stated openly in its docs: protobuf is converted to JSON to reuse WireMock's HTTP matchers. Unidirectional streams support only a **single** request/response event; **bidirectional streaming is not supported at all**; only a limited range of protobuf features is tested. JVM-only.

### gripmock (bavix fork)
The closest thing to a gRPC-native mock server, and the honest benchmark for Simulacra. Go binary, all four streaming patterns, runtime stub API, a UI, status errors and delays, and recent proxy/record modes. Its gaps are precisely where a *testing* tool needs depth:

- **Matching**: field-level `equals` / `contains` / `matches` (regex) plus header rules. No expression language, no compound predicates across fields, no protobuf-type-aware semantics.
- **Verification**: stub *usage* lists (`used-list` / `unused-list`) — you can see which stubs fired, but there is no "assert this method received a request matching M exactly N times," no request capture API, no ordering assertions. For integration testing this is the difference between a stub server and a mock server.
- **SDKs**: no first-class polyglot test-client story (JUnit extension, Testcontainers modules, typed clients).

### The shared failure mode: undisclosed protobuf gaps

All three tools share a deeper problem than any single feature gap: **none supports the full protobuf feature set, and none tells you where the edges are.** `google.protobuf.Any` fields that won't decode, well-known types mangled by JSON conversion, proto2 extensions, deeply recursive messages, `oneof` presence semantics, 64-bit integers silently losing precision — these failures are discovered mid-test, not in the documentation. WireMock is the only one that even admits it ("only a limited range of standard Protobuf features have been tested"). The cost lands on the user: hours of debugging that end in "the mock is the bug," and eroded trust in the tool for every test after that.

### The opening

A tool that is simultaneously **gRPC-native like gripmock** and **test-capable like MockServer** — deep matching, first-class verification, full streaming choreography, typed error details — with a polyglot SDK story none of them has, and a **published, CI-generated protobuf support matrix** so its limits are documented before you hit them, never discovered after. That is Simulacra.

## 3. Goals and non-goals

### Goals (v1)

1. **Automated integration tests** — spin up in milliseconds (binary or Testcontainers), program stubs and verify calls from test code in JVM/Go (Rust/C++ soon after), tear down. Deterministic, isolated, CI-friendly.
2. **Local dev sandbox** — long-running mock of upstream services from declarative stub files with hot reload, a rich CLI, and a read-only web dashboard for inspection.
3. **Table-stakes fault simulation** — any gRPC status with typed error details, fixed/ranged response delays, mid-stream errors. (Chaos-grade fault injection is post-v1.)
4. **Protobuf fidelity, transparently documented** — the full protobuf feature spectrum exercised by a per-release conformance corpus, with results published as a public support matrix. Anything untested is not claimed. See §7.

### Non-goals (v1)

- **Proxy / record & replay** — deferred to post-v1 (design keeps the door open; see roadmap).
- **HTTP mocking — ever.** Focus is the moat. Simulacra will never grow an HTTP expectation engine.
- **Stub editing in the dashboard** — v1 dashboard is read-only; stubs are managed via files, API, CLI, and SDKs.
- **Kubernetes operator / service-mesh integration.**
- **gRPC-Web on the data plane** (the admin plane speaks gRPC-Web for the dashboard; mocking browser-facing gRPC-Web services is post-v1).

## 4. Architecture

One static binary, three planes:

```
                    ┌──────────────────────────────────────────────┐
                    │                simulacra (Go)                │
   gRPC clients     │  ┌────────────────────────────────────────┐  │
   (any language) ──┼─▶│ DATA PLANE                :6565        │  │
                    │  │  dynamic gRPC server                   │  │
                    │  │  (UnknownServiceHandler dispatch)      │  │
                    │  │  TLS/mTLS · health · server reflection │  │
                    │  └───────────────┬────────────────────────┘  │
                    │                  ▼                           │
                    │  ┌────────────────────────────────────────┐  │
                    │  │ CORE                                   │  │
                    │  │  schema registry (protocompile /       │  │
                    │  │    descriptor sets / upstream          │  │
                    │  │    reflection import)                  │  │
                    │  │  stub store · matcher engine (CEL)     │  │
                    │  │  request journal (ring buffer)         │  │
                    │  └───────────────▲────────────────────────┘  │
   test SDKs,       │                  │                           │
   CLI, curl,     ──┼─▶┌───────────────┴────────────────────────┐  │
   dashboard        │  │ CONTROL PLANE             :6566        │  │
                    │  │  admin API (ConnectRPC: gRPC + JSON    │  │
                    │  │    + gRPC-Web from one implementation) │  │
                    │  │  embedded dashboard (static assets)    │  │
                    │  └────────────────────────────────────────┘  │
                    └──────────────────────────────────────────────┘
```

### Data plane

A dynamic gRPC server built on grpc-go's `UnknownServiceHandler` — the primitive that lets one generic stream handler serve *any* method of *any* registered schema with no code generation. This is the same mechanism production gRPC proxies use; it is boring, battle-tested infrastructure. Requests are decoded with `dynamicpb` against the schema registry, matched, and answered.

Behavior details that define the tool's quality:

- **Unmatched requests** return `UNIMPLEMENTED` for unknown methods and `NOT_FOUND` for known methods with no matching stub, with a message that lists the nearest-matching stubs and *why each one didn't match*. This "nearest miss" explanation — WireMock's most-loved debugging feature — appears in the response, the logs, the CLI, and the dashboard.
- **Server reflection is served for mocked schemas**, so `grpcurl`, `grpcui`, Postman, and buf curl work against Simulacra with zero setup.
- **Standard health protocol** (`grpc.health.v1`) served natively — Testcontainers and orchestrators get readiness for free.
- **TLS/mTLS** on both planes; deadline-aware (a stub delay exceeding the client deadline naturally yields `DEADLINE_EXCEEDED`, which is itself a useful test behavior).

### Control plane

The admin API is defined in `simulacra/admin/v1/*.proto` and served with **ConnectRPC**, which yields three protocols from one implementation:

- **gRPC** — what the SDKs use (generated clients in every language).
- **Plain JSON/HTTP** — `curl -d '{...}' localhost:6566/simulacra.admin.v1.StubService/CreateStub` works. No SDK, no codegen, no excuses.
- **gRPC-Web** — feeds the embedded dashboard directly, no proxy layer.

Admin services (sketch; full proto in the repo under `api/`):

| Service | Responsibility |
|---|---|
| `SchemaService` | Register schemas (proto sources, descriptor sets), import from a live upstream via reflection, list registered services |
| `StubService` | Create / list / delete stubs, replace-all (test isolation), export API-created stubs to files |
| `JournalService` | List calls (fully decoded), live-stream calls (tail), reset |
| `VerifyService` | `VerifyCalls(method, matcher, times)` → pass/fail with nearest-miss diagnostics |
| `ControlService` | Reset (stubs + journal), server info, shutdown |

### Core

- **Schema registry** — see §6.
- **Stub store** — file-sourced stubs (canonical for the sandbox, hot-reloaded) and API-created stubs (ephemeral, for tests) coexist; `stub export` bridges the two (capture a sandbox session into files).
- **Matcher engine** — see §5.
- **Request journal** — bounded ring buffer (configurable size) of fully-decoded calls: method, metadata, message(s), matched stub, response, timings. Powers verification, the CLI tail, and the dashboard.

## 5. The stub model

Stubs are declarative YAML (JSON accepted), identical in shape whether loaded from files, POSTed to the admin API, or built through an SDK. Ergonomics rules: full method names always; protobuf field names as written in the `.proto`; enums by name; durations as Go-style strings (`50ms`, `2s`).

### Matching

Three composable layers, all protobuf-aware (they operate on decoded messages, not JSON conversions):

```yaml
- method: shop.v1.OrderService/GetOrder
  match:
    metadata:                      # gRPC metadata (headers)
      x-tenant: { eq: "acme" }
      authorization: { present: true }
    message:                       # structured field matchers
      order_id: { eq: "o-123" }
      customer.region: { in: [EU, UK] }        # nested paths, enums by name
      note: { matches: "urgent.*" }            # regex on strings
    expr: 'message.items.exists(i, i.sku == "A1") && size(message.items) <= 10'
  priority: 10                     # higher wins on ties
  times: 1                         # consume-once (unlimited if omitted)
```

- **Structured matchers** (`eq`, `ne`, `in`, `matches`, `present`, `contains` for repeated fields) cover the 80% case with no learning curve.
- **`expr`** is a [CEL](https://cel.dev) expression — the same language as Kubernetes admission policies and Envoy RBAC. Variables: `message` (the request), `metadata` (map), `method` (string); for client-streaming/bidi, `messages` (the list so far). CEL gives compound predicates, arithmetic, string/list/map functions, and timestamp/duration handling over true protobuf types — the capability jump over every competitor's equals/contains/regex.
- Matching respects protobuf semantics: field presence vs. defaults, `oneof`, int64 without precision loss, `bytes`, well-known types (`Timestamp`, `Duration`, wrappers) with their natural CEL forms.

### Responses

```yaml
  respond:
    metadata: { x-mock: "simulacra" }        # response headers
    message:
      order_id: "o-123"
      status: ORDER_STATUS_SHIPPED
      eta: "{{ now + duration('72h') }}"     # CEL templating
      echo_note: "{{ message.note }}"        # request-field access
    trailers: { x-served-by: "stub-42" }
    delay: 50ms..200ms                       # fixed or uniform range
```

Error responses are first-class and **typed**:

```yaml
  respond:
    status:
      code: FAILED_PRECONDITION
      message: "order already shipped"
      details:                               # google.rpc.Status details — real ones
        - type: google.rpc.PreconditionFailure
          value:
            violations:
              - { type: STATE, subject: "o-123", description: "already shipped" }
```

Typed error details (`BadRequest`, `RetryInfo`, `PreconditionFailure`, any registered message type) are exactly what JSON-translation tools fumble and what resilient clients need to test against.

### Streaming

All four shapes, with explicit per-shape semantics:

| Shape | Match on | Respond with |
|---|---|---|
| Unary | `message` | one `message` or `status` |
| Server streaming | `message` (the single request) | a `stream` script |
| Client streaming | `messages` via CEL (evaluated at stream close) | one `message` or `status` |
| Bidirectional | per-message `rules` | messages per rule, plus `on_open` / `on_close` |

Server-streaming scripts are choreography, not single events:

```yaml
- method: shop.v1.OrderService/WatchOrder
  match: { message: { order_id: { eq: "o-123" } } }
  respond:
    stream:
      - message: { state: PACKED }
      - delay: 200ms
        message: { state: SHIPPED }
      - delay: 1s
        status: { code: UNAVAILABLE, message: "backend hiccup" }   # mid-stream error
```

Bidirectional stubs are reactive rules — deliberately simple in v1:

```yaml
- method: chat.v1.ChatService/Session
  respond:
    on_open:
      - message: { text: "welcome" }
    rules:
      - match: { message: { text: { matches: "ping.*" } } }
        send:
          - message: { text: "pong" }
    on_close: { status: { code: OK } }
```

### Sandbox lifecycle

- `--stubs ./stubs` loads a directory tree of YAML files, watched for changes (hot reload, atomic swap, parse errors reported without dropping the running set).
- Stub files support a `$schema` reference for editor autocomplete/validation (JSON Schema published with each release).

## 6. Schema management

Simulacra must know message shapes to decode, match, and template. Three sources, mixable:

1. **Proto sources** — `--proto ./protos` compiles `.proto` trees at runtime via `bufbuild/protocompile` (no `protoc` installation, ever). Include paths and well-known types handled.
2. **Descriptor sets / buf images** — `--descriptors ./api.binpb` for teams with buf/Bazel pipelines that already produce images.
3. **Live upstream reflection** — `simulacra schema import --reflect staging.example.com:443` snapshots a real service's schema. Point Simulacra at staging once; mock offline forever after.

Schemas can also be registered at runtime through `SchemaService` (tests ship descriptor bytes; no files needed). Proto3, proto2, and editions are supported to the extent `protocompile`/`protobuf-go` support them — which is first-party and tracks upstream.

## 7. Protobuf fidelity and the public support matrix

The single most frustrating property of existing tools is not any specific missing feature — it's that missing features are *undisclosed*. You find them by watching your test fail, ruling out your own code, and finally suspecting the mock. Simulacra treats this as a product requirement, not a documentation nicety.

### Fidelity by construction

The pipeline never converts protobuf to an intermediate JSON representation. Messages are decoded with first-party `dynamicpb` against real descriptors, matched and templated over protobuf types (CEL operates on protobuf natively), and re-encoded from descriptors. This removes by design the entire class of translation bugs — int64 precision, bytes-vs-base64 confusion, enum name/number drift, well-known-type mangling — that JSON-bridging tools inherit architecturally.

The historically hard cases get first-class, tested treatment:

- **`google.protobuf.Any`** — resolved against a type registry built from the schema registry; matchable and templatable when the payload type is registered, with defined, documented behavior (type-URL matching + opaque bytes) when it isn't.
- **Well-known types** — `Timestamp`, `Duration`, `Struct`, `Value`, `FieldMask`, wrappers: natural forms in stub YAML, matchers, and CEL.
- **Unknown fields** — preserved and round-tripped, never silently dropped.
- **Presence semantics** — proto3 `optional`, `oneof`, message-vs-scalar presence honored in matching (`present: true` means *set*, not *non-default*).
- **proto2 and editions** — extensions, groups, required fields, and editions-syntax files supported to the same level as `protobuf-go` itself, and tested as such.
- **Structural stress** — recursive/self-referential messages, deep nesting, large messages, maps with message values, packed repeated fields.

### The conformance corpus

A dedicated fixture corpus — inspired by the official protobuf conformance suite, adapted to the mock-server pipeline — exercises every feature above end-to-end: schema load → request decode → match → template → response encode → client decode, for each RPC shape where it applies. It runs in CI on every commit and against every release binary, alongside the polyglot client matrix (§10).

### The support matrix

Corpus results are not an internal artifact — they are **generated into a public support matrix**, published with every release on the docs site and as `SUPPORT.md` in the repo. Every protobuf feature is in exactly one state:

| State | Meaning |
|---|---|
| ✅ Supported | in the corpus, passing, covered by semver |
| ❌ Not supported | documented, with the failing behavior described and an issue link |
| ⬜ Untested | **not claimed** — treated as unsupported in public messaging until it enters the corpus |

The policy is one line: **if it isn't tested, it isn't claimed.** A user should never be the first to discover a gap — and where a gap exists, the matrix says so before they write a single stub.

## 8. Verification

The feature that makes Simulacra a *mock* server rather than a stub server:

```
VerifyCalls(
  method:  "shop.v1.OrderService/GetOrder",
  matcher: <same match block as stubs>,
  times:   { exactly: 2 }         # or at_least / at_most / never
) → VERDICT + on failure: the actual calls received, and per-call
    explanations of which matcher clause failed (nearest-miss)
```

- **Journal queries** return fully decoded requests (JSON form + raw bytes + descriptor reference) for argument capture and custom assertions in any language.
- **Order-sensitive verification** (`VerifySequence`) for asserting call ordering across methods.
- Journal `Reset` between tests; `ReplaceAllStubs` for setup — the two RPCs that make test isolation one line in each SDK.

## 9. Developer experience surfaces

### CLI

```
simulacra serve --proto ./protos --stubs ./stubs        # the sandbox, one line
simulacra serve --descriptors api.binpb --port 6565
simulacra schema import --reflect staging:443 -o schema.binpb
simulacra stub list | add -f stub.yaml | export
simulacra calls tail                                    # live decoded request log
simulacra calls tail --method shop.v1.OrderService/GetOrder
simulacra verify --method ... --match-file m.yaml --times 2   # CI-scriptable
simulacra check ./stubs                                 # validate stubs against schemas offline
```

`cobra`-based, shell completions, `--output json` on every read command for scripting.

### SDKs

The admin API being gRPC means every SDK is ~80% generated. The remaining 20% is where ergonomics live:

**JVM (v1)** — authored in Kotlin, Java-first-class API. Fluent stubbing DSL, JUnit 5 extension managing lifecycle + per-test isolation, Testcontainers module:

```java
@RegisterExtension
static SimulacraExtension simulacra = SimulacraExtension.testcontainers()
        .withProtoDir("src/test/protos");

@Test
void shipsOrder() {
    simulacra.stub(OrderServiceGrpc.getGetOrderMethod())
             .whenMessage(m -> m.field("order_id").eq("o-123"))
             .respond(GetOrderResponse.newBuilder().setStatus(SHIPPED).build());

    client.getOrder(...);

    simulacra.verify(OrderServiceGrpc.getGetOrderMethod()).calledExactly(1);
}
```

When generated stubs are on the test classpath (the common case), the SDK accepts real generated message/method objects — full type safety. Without them, the string/DSL form works.

**Go (v1)** — `testing.T`-integrated helpers (`simulacratest.Start(t)` with automatic cleanup), Testcontainers-go module, and an in-process option (importing the server as a library) for the fastest possible tests.

**Rust and C++ (v1.x)** — thin idiomatic wrappers over the generated clients (tonic client for Rust; the C++ helper published as a small CMake `FetchContent`-able library). Until then, both ecosystems use generated admin-API clients directly — which is a working, supported path from day one, not a placeholder.

### Dashboard (read-only, v1)

Embedded static assets, served from the control-plane port, zero configuration:

- **Live request log** — streaming, with fully decoded protobuf payloads, matched-stub links, timing, status.
- **Nearest-miss explanations** for unmatched requests — the "why didn't my stub match?" answer, visually.
- **Registered services browser** — services/methods/message shapes from the registry.
- **Active stubs** — with per-stub hit counts and source (file vs. API).

## 10. Tech stack

### Language assessment

The core must: compile `.proto` at runtime, handle dynamic protobuf messages faithfully, serve arbitrary gRPC methods without codegen, ship as a single fast-starting binary, and be approachable to OSS contributors in this tool category.

| | Go | Rust | Kotlin/Scala (JVM) |
|---|---|---|---|
| Dynamic protobuf | `dynamicpb`/`protoreflect` — **first-party Google** | `prost-reflect` — community | `DynamicMessage` — first-party, most mature anywhere |
| Runtime .proto compile | `bufbuild/protocompile` — powers buf | `protox` — newer, pure Rust | protoc-jar or shelling out |
| Codegen-free serving | `grpc.UnknownServiceHandler` — designed for this, proven in proxies | hand-rolled tower `Service` — feasible, off the paved road | `ServerCallHandler` registration — supported |
| CEL | `cel-go` — **the reference implementation** | community crates, immature | `cel-java` — solid |
| Single binary / startup | static binary, ~10ms | static binary, ~5ms | JVM: ~1s + 200MB, or GraalVM native-image (notoriously painful with grpc-netty) |
| Ecosystem gravity | buf, grpcurl, grpcui, gripmock — this tool category lives in Go | tonic ecosystem growing | the *old* mocking world (WireMock, MockServer) |
| Time to v1 | baseline | ~1.5–2× | ~1.2× core, but distribution ergonomics never fully recover |

**Decision: Go for the core.** It satisfies the first-priority language preference, and it is the only column where every load-bearing dependency is first-party or ecosystem-canonical. Rust is genuinely viable but pays a hand-rolling tax for zero user-visible benefit (a mock server's bottleneck is the dev loop, not throughput). The JVM's dynamic-protobuf maturity is real, but a dev-loop tool that starts in a second and idles at 200MB contradicts "very approachable" — Kotlin's strength is redirected to where it wins: **the JVM SDK**.

### Dependencies (core)

| Concern | Choice | Why |
|---|---|---|
| gRPC server | `google.golang.org/grpc` | `UnknownServiceHandler`, reflection, health libraries included |
| Protobuf runtime | `google.golang.org/protobuf` (`dynamicpb`, `protoreflect`) | first-party dynamic messages |
| Proto compilation | `github.com/bufbuild/protocompile` | runtime `.proto` → descriptors, no protoc |
| Matching expressions | `github.com/google/cel-go` | reference CEL implementation |
| Admin API | `connectrpc.com/connect` | gRPC + JSON + gRPC-Web from one handler |
| CLI | `github.com/spf13/cobra` | ecosystem standard, completions |
| File watching | `github.com/fsnotify/fsnotify` | hot reload |
| Dashboard | Vite + React + TypeScript → `embed.FS` | most contributor-approachable UI stack; ships inside the binary |

### Quality gates

- `buf` lint + breaking-change detection on `api/` protos in CI (the admin API is a public contract from day one).
- `golangci-lint`; race detector on the full test suite.
- **Polyglot conformance matrix**: a scenario suite (stub + call + verify across all four streaming shapes, error details, metadata, deadlines) executed in CI by real generated **grpc-java** and **grpc-go** clients against the release binary — expanding to C++ and Rust (tonic) clients with their SDKs. This is simultaneously the regression suite and the public, living proof of the polyglot claim.

### Distribution

GoReleaser: macOS/Linux/Windows × amd64/arm64 binaries, Homebrew tap, Scoop manifest, multi-arch Docker image (`FROM scratch` + binary, sub-20MB), checksums + SBOM + signed releases. SDKs to Maven Central and pkg.go.dev; later crates.io and a CMake-consumable C++ release.

## 11. Repository layout

Monorepo:

```
simulacra/
├── api/                        # simulacra/admin/v1/*.proto  (buf module)
├── cmd/simulacra/              # main
├── internal/
│   ├── dataplane/              # dynamic gRPC server, reflection, health
│   ├── schema/                 # registry: protocompile, descriptors, reflection import
│   ├── stub/                   # store, YAML loading, hot reload
│   ├── match/                  # structured matchers + CEL engine, nearest-miss
│   ├── journal/                # ring buffer, queries, verification
│   └── admin/                  # ConnectRPC handlers
├── ui/                         # dashboard (Vite), built assets embedded
├── sdks/
│   ├── java/                   # Kotlin-authored JVM SDK + JUnit5 + Testcontainers
│   ├── go/                     # Go SDK + testcontainers-go
│   ├── rust/                   # v1.x
│   └── cpp/                    # v1.x
├── conformance/                # polyglot scenario matrix + protobuf feature corpus
│                               #   + support-matrix generator (emits SUPPORT.md)
└── docs/                       # docs site source (Astro Starlight, built at M4)
```

## 12. Roadmap

| Milestone | Deliverable | Exit criterion |
|---|---|---|
| **M1 — walking skeleton** | Schema registry (proto dir + descriptor sets), dynamic data plane, YAML stubs, unary matching (structured only), CLI `serve`/`check`, reflection + health | `simulacra serve --proto --stubs` answers a real grpcurl unary call correctly |
| **M2 — the capability core** | CEL matching, templating, all four streaming shapes, typed error details, delays, journal, verification, nearest-miss diagnostics, hot reload, protobuf conformance corpus (first pass) | conformance suite (Go client) green across all shapes; corpus covering §7's hard-case list |
| **M3 — the test story** | Admin API (ConnectRPC), reflection schema import, JVM SDK + JUnit5 + Testcontainers, Go SDK, `calls tail`, journal/stub CLI | a grpc-java integration test using the JVM SDK passes in CI via Testcontainers |
| **M4 — v1.0 public release** | Dashboard, docs site, generated protobuf support matrix published, GoReleaser pipeline (brew/Docker/binaries), Maven Central publishing, java conformance leg | tagged v1.0.0, installable via `brew install simulacra` and `docker run`, `SUPPORT.md` auto-generated from CI |
| **Post-v1** | Rust + C++ SDKs · proxy / record & replay (journal + schema registry are designed as its foundation) · scenario state machines (WireMock-style) · chaos-grade fault injection (bandwidth, resets, jitter distributions) · data-plane gRPC-Web · faker functions in templating | — |

Scenario state machines were deliberately cut from v1: `times` + `priority` covers sequential-response needs in tests, and shipping M1–M4 sooner matters more. They are the first post-v1 feature.

## 13. Risks

| Risk | Mitigation |
|---|---|
| **bavix/gripmock closes the gap** (it's active and good) | Differentiate where depth compounds: matching power, verification, SDK breadth, diagnostics quality. The conformance matrix is a publishable credibility asset gripmock lacks. Ship M1–M3 quickly. |
| **CEL learning curve deters casual users** | Structured matchers cover the common cases with zero learning; CEL is opt-in. Every CEL error message includes the expression, the failing input, and a docs link. |
| **Bidi semantics scope creep** | v1 bidi is the minimal reactive-rules model, explicitly documented as such; richer choreography arrives with scenario state machines post-v1. |
| **Dynamic protobuf edge cases** (editions, presence, well-known types) | Every load-bearing dependency is first-party (`protobuf-go`, `protocompile`) and tracks upstream; the conformance corpus (§7) exercises exactly these edges with real generated clients — and any gap that remains becomes a documented ❌ in the support matrix rather than a user-discovered surprise. |
| **Maintainer bandwidth (OSS solo start)** | Small core; SDKs mostly generated; UI read-only; milestones are individually shippable; non-goals are written down and enforced. |
| **Name collisions** ("Simulacra" is used by an indie game and scattered packages) | No conflict in the dev-tools category; binary name `simulacra` is unclaimed in brew/scoop. Do a trademark/package-registry sweep before the v1.0 announcement. |

## 14. What "very approachable" means, concretely

The north-star DX checklist every milestone is measured against:

1. **Zero to mock in one line**: `simulacra serve --proto ./protos --stubs ./stubs` — no protoc, no codegen, no config file required.
2. **Standard tools just work**: grpcurl, grpcui, Postman, buf curl — via served reflection.
3. **Any language can drive it**: the admin API speaks plain JSON over HTTP; `curl` is a supported client.
4. **Failures explain themselves**: every non-match produces a nearest-miss explanation, in the place you're already looking (response, log, CLI, dashboard).
5. **Test isolation is one line**: SDK lifecycle hooks reset stubs + journal per test.
6. **Stub files teach themselves**: published JSON Schema → editor autocomplete and inline validation.
7. **No undiscovered gaps**: protobuf feature support is proven by a per-release conformance corpus and published as a support matrix — if it isn't tested, it isn't claimed.
