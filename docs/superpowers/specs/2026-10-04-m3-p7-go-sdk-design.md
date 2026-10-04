# Simulacra M3 Phase 7 — Go SDK (`simulacratest`): Design

**Goal (M3 §13.7, resliced in §1):** a `testing.T`-integrated Go SDK. `simulacratest.Start(t)` boots an
in-process server in milliseconds; typed helpers register schemas, create stubs, verify calls, and
capture requests — all through the admin API, exactly as every other client does.

**Reference:** `PROPOSAL.md` §5 (stub model), §8 (verification), §9 (SDKs), §14 (DX checklist); M3 design
`docs/superpowers/specs/2026-07-25-m3-test-story-design.md`, cited as "M3 §N". Phase 6 design
`docs/superpowers/specs/2026-09-20-m3-p6-reflection-import-design.md`, cited as "P6 §N". Phase 6 is
complete at `aa064ed`.

---

## 1. Why this phase, now — and what changed from M3

Phase 6 closed the CLI. Every admin RPC now has a real consumer, and the next M3 deliverable on the
critical path is an SDK. The Go SDK goes first: it shares a toolchain with the server, it can run the
server in-process with no image, and it sets the shape — rendered stub documents, verbatim
diagnostics, per-test isolation — that the JVM SDK in phase 9 must match.

Three deviations from the M3 design, each recorded here so the M3 document is not silently
contradicted:

| M3 said | This design | Why |
|---|---|---|
| §13: phase 7 = `simulacratest` + `simulacratc`; phase 8 = Dockerfile + CI image | **Phase 7 = `simulacratest` only. Phase 8 = Dockerfile + CI image + `simulacratc`.** | `simulacratc`'s only meaningful test is a container smoke test, and the image does not exist until the Dockerfile does. Shipping it in phase 7 would land it untested. Each phase still lands green, and phase 9 still gets its image from phase 8. |
| §8: the whole SDK in a separate module `github.com/yinghanhung/simulacra/sdks/go` | **`simulacratest` lives in the core module** at `github.com/yinghanhung/simulacra/simulacratest`. Only `simulacratc` (phase 8) gets the `sdks/go` module. | M3 §8's stated reason for a separate module is to keep testcontainers-go's Docker dependency tree out of core. That reason applies only to `simulacratc`. `simulacratest` imports `server/` and is version-locked to it, the way `net/http/httptest` sits next to `net/http`; in a separate module it would need a `replace` directive for development and could skew against the server it embeds. It adds **no** dependency to core's `go.mod`. |
| §8: `Reset` = `ReplaceAllStubs(nil)` + `ResetJournal` | **`Reset` = one `ControlService.Reset{}`** | Same effect in one RPC, and it also restores file-origin `times` budgets (M3 §5, `ResetRequest`'s omitted-means-true fields). |

## 2. Scope

### In scope

1. Package `simulacratest` in the core module: an admin-API `Client` with typed helpers, and an
   in-process `Server` built on the `server/` facade.
2. A stub builder covering the unary grammar; a verify builder; typed call capture.
3. Automatic and explicit schema registration from generated Go descriptors.
4. **Core grammar addition:** `respond.literal` (§8) — the only behavior change outside `simulacratest/`.
5. **Test fixtures:** committed Go and grpc-go code for `testdata/protos/shop/v1` (§9).

### Non-goals

- **`simulacratc`, the Dockerfile, the CI image** — phase 8 (§1).
- **A builder for streaming shapes.** Server-stream scripts, client-stream, and bidi rules use
  `StubYAML`, which accepts the full grammar.
- **A shared server without a `testing.TB`** (e.g. one per package from `TestMain`). `server.Start` is
  fast enough per test; `Connect` (§3) covers an externally managed server.
- **Any admin API change.** `api/` and `gen/` are untouched.
- **README quickstarts** — phase 10, with the rest of the user docs.

## 3. Package boundary and lifecycle

Two types, one package:

- **`Client`** talks only to a running server's admin API. It holds an admin address, a data address,
  and the five generated connect clients. `simulacratest.Connect(t, adminAddr, dataAddr string) *Client`
  builds one. The data address is explicit, not read from `GetServerInfo`: in a container (phase 8),
  the server reports its in-container address, not the mapped port the test must dial. `Connect` gives
  an external-server mode now, and it is the type `simulacratc` will return in phase 8.
- **`Server`** embeds `*Client` and owns an in-process `*server.Server`. `Start(t, opts...)` calls
  `server.Start` with `DataAddr` and `AdminAddr` both `127.0.0.1:0`, `Watch: false`, and the options
  below, then registers cleanup and returns a `Server` whose `Client` points at the bound addresses.

**Everything goes through the admin API over loopback**, in-process included (M3 §8). The SDK never
touches the store, registry, or journal directly; it exercises the same surface as the CLI, the JVM
SDK, and `curl`.

**Transport.** A connect-go client over plain HTTP/1.1, as the CLI does (`internal/cli/client.go`) —
no h2c client-side. Each `Client` owns its `http.Transport`, so each `Client` must release it. Every
constructor that acquires a resource takes `t`, as `Dial` does: `Connect(t, adminAddr, dataAddr)`
registers `t.Cleanup(transport.CloseIdleConnections)`. `Start` builds its `Client` through the same
constructor and so gets the same cleanup, and phase 8's `simulacratc` will too. A suite of hundreds of
tests therefore accumulates no idle connections, whether its servers are in-process or external.

**`t` is passed to each call that does I/O, not captured at `Start`.** A server started in a parent
test is routinely used inside `t.Run` subtests. `FailNow` must run on the goroutine of the test that
is failing, so a `t` captured at `Start` would fail the wrong test, from the wrong goroutine. Builders
are plain values; only their terminal call takes `t`. Every method accepts `testing.TB`, so benchmarks
work too.

**Contexts: deliberately not `t.Context()`.** `t.Context()` is "canceled just before
Cleanup-registered functions are called" — and that is its *only* cancellation event. For a helper that
runs synchronously it therefore buys nothing, and it breaks exactly the calls that belong in cleanup:
`t.Cleanup(func() { sim.Verify(m).Exactly(t, 1) })`, or a shared server's `Reset`. Instead each admin
RPC runs under `context.Background()` bounded by a per-call timeout of 30 seconds — the CLI's
`defaultTimeout` (`internal/cli/client.go:26`), for the same reason: a server that accepts the
connection and never answers must not hang the test until `go test -timeout`. A helper therefore works
the same in a test body and in a cleanup function.

`Start`'s own cleanup calls `Shutdown` with a 10-second context — above the server's 6-second worst-case
teardown (`shutdownGrace` 5s + `dataGraceFloor` 1s, `server/server.go`); the watcher join that bound
excludes does not apply, because `Start` passes `Watch: false` — and reports a failure with `t.Errorf`.
A `Shutdown` that hits the 10-second context escalates to a hard stop, so cleanup always returns.

**Cleanup order.** Cleanups run last-in-first-out. `Start` registers shutdown, then its `Client`'s
transport cleanup; `Dial` and any user cleanup registered after `Start` run **before** both. So the
test's client connections close before the server stops, graceful shutdown never waits on them, and a
verifying cleanup registered after `Start` runs against a live server. A cleanup registered *before*
`Start` runs after the server is gone and fails with a transport error — the package doc says to
register verifying cleanups after `Start`.

## 4. API surface

```go
// Lifecycle (in-process)
sim := simulacratest.Start(t,
    simulacratest.WithProtoDir("testdata/protos"),   // optional; each option is repeatable
    simulacratest.WithDescriptorSet("api.binpb"),
    simulacratest.WithStubDir("testdata/stubs"),
    simulacratest.WithJournalSize(1024))             // default 1024, matching `serve`
conn := sim.Dial(t)                                   // *grpc.ClientConn to the data plane

// External server: same helpers, no server lifecycle; idle connections released on cleanup
c := simulacratest.Connect(t, "localhost:6566", "localhost:6565")

// Helpers on *Client (Server embeds it)
c.DataAddr() string
c.AdminAddr() string
c.Dial(t, opts ...grpc.DialOption) *grpc.ClientConn  // closed on cleanup
c.RegisterFiles(t, fds ...protoreflect.FileDescriptor)
c.StubYAML(t, doc string) *adminv1.Stub               // full grammar; exactly one stub mapping
c.Stub(method string) *StubBuilder                    // §5
c.Verify(method string) *VerifyBuilder                // §7
c.Calls(t, method string) []Call                      // §7; "" means every method
c.DeleteStub(t, id string)
c.Reset(t)                                            // ControlService.Reset{}
c.Admin() Admin                                       // the five generated clients
```

- **Method arguments** are full gRPC method names, `"/pkg.Svc/M"` or `"pkg.Svc/M"` — the forms the
  server already normalizes. protoc-gen-go-grpc generates them as constants
  (`shopv1.OrderService_GetOrder_FullMethodName`), so Go users get typed names with no new abstraction.
- **`StubYAML`** reads the document's top-level `method` with `yaml.v3` (already a core dependency)
  so it can run `ensure` (§6) like `Stub` does. If the document does not parse or has no `method`,
  it is sent as-is and the server's diagnostic is what the test sees.
- **`Admin()`** returns a struct of the five generated `adminv1connect` client interfaces — the same
  set the CLI's `adminClient` holds — as the escape hatch for anything not wrapped (`WatchCalls`,
  `ListServices`, `ExportStubs`).

## 5. The stub builder

```go
sim.Stub(shopv1.OrderService_GetOrder_FullMethodName).
    MatchField("order_id", simulacratest.Eq("o-123")).
    MatchField("customer.region", simulacratest.In(shopv1.Region_EU, shopv1.Region_UK)).
    MatchMetadata("x-tenant", simulacratest.Eq("acme")).
    Expr(`size(message.items) <= 10`).
    Priority(10).Times(1).
    Header("x-mock", "simulacra").Trailer("x-served-by", "go-sdk").
    DelayRange(50*time.Millisecond, 200*time.Millisecond).
    Respond(t, &shopv1.GetOrderResponse{ /* ... */ })   // or RespondStatus(t, st)
```

### What it sends

One stub mapping rendered as **JSON** and sent with `CreateStub` — the JVM SDK's choice in M3 §9, so
both SDKs produce the same document for the same stub. JSON is a YAML subset, so the server's strict
loader takes it unchanged. The terminal call returns the `*adminv1.Stub` envelope.

Builder methods take no `t`. Misuse — an unsupported matcher value, the same operator twice on one
path, a duplicate header key, an invalid header value — is collected and reported once by the terminal
call through `t.Fatalf`.

The builder does not check the method's RPC shape. `Respond` on a streaming method produces a document
the loader rejects, and the loader's diagnostic is passed through verbatim.

### Matchers

`Eq, Ne, In, Matches, Present, Contains` construct a `Matcher`. The same values serve `MatchField`
(message paths, dotted for nesting) and `MatchMetadata`, because both are `match.Rules` in the grammar
(`internal/match/match.go:85`). Repeated `MatchField` calls on one path merge their operators.

Values are rendered the way the matcher's literal parser reads them (`literalFor`,
`internal/match/match.go:462`):

| Go value | Rendered as | Why |
|---|---|---|
| all signed and unsigned integer widths | JSON number | The matcher **rejects** a quoted `"123"` for an integer field ("got string literal"), so protojson's int64-as-string convention cannot be used here. yaml.v3 decodes JSON integers to `int`, or `uint64` above `MaxInt64`, exactly (F5). |
| `float32` | `float64(float32(v))` | The server widens float32 literals the same way, so 0.1 matches the float32 0.1 a client sent. |
| `protoreflect.Enum` | value name; the number when the value has no name | Enums by name (PROPOSAL §5); numbers keep open proto3 enums matchable. |
| `string`, `bool`, `float64` | JSON scalar | — |
| `[]byte`, `proto.Message`, maps, NaN, ±Inf | **refused** before sending, pointing at `Expr` | Structured matchers do not support bytes, message, or map fields. JSON cannot express a non-finite float. A `[]byte` would marshal to a base64 string and silently match a string field instead of failing. |

`Expr(cel)` sets the CEL escape hatch. It is sent verbatim.

### `Respond(t, msg proto.Message)`

1. **Check the type against the RPC.** `msg`'s full name must equal the method's output type, taken
   from the `ensure` cache (§6): the method descriptor's `Output()` when the service came from
   `GlobalFiles`, otherwise `MethodInfo.output_type` from `ListServices` — a bare `FullName()`
   (`internal/admin/schema.go:76`), so it compares directly. A mismatch is misuse:
   `Respond: got shop.v1.GetOrderRequest, GetOrder returns shop.v1.GetOrderResponse`. Without this
   check a wrong message whose fields happen to fit is accepted: `GetOrderRequest` and
   `GetOrderResponse` both declare `string order_id = 1`, so a request holding only `order_id` renders
   to JSON the server decodes into a `GetOrderResponse` without complaint, and the guard below —
   which decodes into `msg`'s own type — cannot see it. When the method is unknown to both the server
   and the test binary, the check is skipped; `CreateStub` fails `NOT_FOUND` regardless.
2. Render `msg` with `protojson.MarshalOptions{UseProtoNames: true, Resolver: c.types}` — field names
   as written in the `.proto` (PROPOSAL §5), enums by name, 64-bit integers as strings, well-known
   types in their natural forms. Presence is preserved: unset fields are omitted, set-to-default
   presence fields are emitted. `c.types` is the client's resolver (§6).
3. Set **`respond.literal: true`** (§8), so no string in the body is re-read as a template.
4. Run the **round-trip guard**, then send.

**The round-trip guard** reproduces the server's decode path locally — `yaml.v3` into
`map[string]any`, `encoding/json`, then `protojson.UnmarshalOptions{Resolver: c.types}` into a fresh
message of `msg`'s type (the path `stub.BuildMessage` takes, `internal/stub/stub.go:319`) — and compares
the result with `msg` by **both** `proto.Equal` and deterministic-marshal bytes. `proto.Equal` alone is
not enough: it compares floats with `==`, so it cannot see `-0.0` becoming `+0.0`. The resolver
matters: protojson's default is `GlobalTypes`, which does not hold extensions or `Any` payload types
that exist only in files the test registered — `conformance.v1.ext_tag` decodes as an unknown field
under the default and correctly under `c.types`.

If the two differ, `Respond` refuses rather than install a response that differs from the one the test
passed. The known case is **unknown fields**: protojson drops them silently and the grammar cannot carry
them. Any other loss the YAML hop introduces (F6) is caught the same way.

### `RespondStatus(t, st *status.Status)`

- **Code** is rendered as its canonical name (`FAILED_PRECONDITION`) from a 16-entry table, because the
  server parses codes with `codes.Code.UnmarshalJSON` (`internal/stub/status.go:37`), which accepts
  that form and does not accept `codes.Code.String()`'s `FailedPrecondition` (F4).
- **Message** is sent as-is. Status messages are never templated.
- **Details** come from `st.Proto().Details`. Each `Any` is unpacked with
  `anypb.UnmarshalNew(a, proto.UnmarshalOptions{Resolver: c.types})` and rendered as
  `{type: <full name>, value: <protojson>}`, each through the same round-trip guard. A detail whose
  type neither the test binary links nor the test registered is refused, naming its type URL.
- An `OK` status is passed through, and the server's existing diagnostic ("status code OK is not
  allowed here") is what the test sees.

### Response options

- `Header(k, v)` and `Trailer(k, v)` refuse a **duplicate key**: the grammar holds one value per key
  (`map[string]string`, `internal/stub/stub.go:31`), and last-wins would silently drop the first. They
  also refuse a value that is **not valid UTF-8**: `encoding/json` replaces invalid bytes with U+FFFD,
  which would corrupt a `-bin` value without any error. Every other metadata rule (reserved keys,
  printable ASCII) is the server's to diagnose.
- `Delay(d)` and `DelayRange(lo, hi)` render via `time.Duration.String()`, which `ParseDelay`'s
  `time.ParseDuration` reads back (`internal/stub/delay.go:18`).
- `Priority(n)`, `Times(n)` map one-to-one onto the grammar.

## 6. Schema registration

### Explicit

`RegisterFiles(t, fds...)` walks each file's `Imports()` depth-first, dependencies before importers,
deduplicated by path, converts each with `protodesc.ToFileDescriptorProto`, and sends one
`RegisterSchemas`. That RPC is all-or-nothing and idempotent (`Registry.RegisterSet`,
`internal/schema/registry.go:334`), so registering a file twice is a no-op. As P6 §1 anticipated, this
walk shares nothing with phase 6's reflection `Closure`: it is a short DFS over in-memory descriptors
with no fetcher.

### The client's type resolver

Registering a file on the server does not make its types resolvable **in the test process**:
protojson and `proto.Unmarshal` resolve extensions and `Any` payloads through `GlobalTypes` by
default, and a file the test built at run time — a `dynamicpb` schema, a descriptor set read from
disk — is not there. So each `Client` keeps a local `protoregistry.Files` and the
`dynamicpb.NewTypes` view over it, and `RegisterFiles` adds each file there as well as sending it.

- **The caller's own descriptor instances** are registered, not re-parsed copies. A `dynamicpb`
  message carries the descriptor it was built from, so its extension types must be built from that
  same instance (F7).
- **`c.types` resolves `GlobalTypes` first, then the local view.** Generated messages must get their
  generated extension and payload types; the local view only fills in what the binary does not link.
- **A path already present** — in the local snapshot, or in `GlobalFiles` — is handled by the
  extension-identity rule below: refused when the file declares extensions and the instance differs,
  otherwise skipped, matching the server registry's first-registration-wins rule for `AddFile`
  (`internal/schema/registry.go:162`).
- **Local registration follows the server.** `RegisterFiles` sends `RegisterSchemas` first and adds
  the files locally only after it succeeds, so the client never resolves types the server rejected.

`c.types` is the resolver for everything the SDK serializes or captures: `Respond`'s rendering and
round-trip guard (§5), `RespondStatus`'s detail unpacking (§5), and `Calls`' typed capture (§7).
Files that `ensure` registers from `GlobalFiles` need no local entry; `GlobalTypes` already holds them.

**Different descriptor instances of one file decode extensions silently wrong.** `dynamicpb` stores a
decoded extension under the extension descriptor the *resolver* supplied and answers `Has`/`Get` only
for that same descriptor instance. So whenever the caller's extension descriptor is not the instance
`c.types` resolves, the value is present but invisible: decoding succeeds with no error and no unknown
bytes, yet `Has(ext)` through the caller's descriptor is `false` and `Get` returns the default. F7
reproduces this two ways:

- **Different extendee instances** — two independent compiles of `legacy.proto`, where the message and
  its extension share a file. The decoded extension's `ContainingMessage()` is not the target message's
  descriptor instance.
- **Shared extendee, different extension instances** — an extension file compiled twice against *one*
  instance of the message file it extends. `ContainingMessage()` **is** the target's descriptor, so no
  inspection of the decoded message can tell; only the extension descriptors differ.

The SDK closes both, one at registration and one at decode:

1. **Extension-identity rule (`RegisterFiles`).** For every file in the walked closure that declares any
   extension — at file scope or nested inside a message — the instance `c.types` would resolve that
   file's extensions from must be the instance passed in. Concretely: if `GlobalFiles` holds the path,
   it must be that very instance (`GlobalTypes` wins resolution, so any other copy would always lose);
   else if the local snapshot holds the path, it must be that very instance. A different instance is
   refused before anything is sent, naming the file:
   `conformance/v1/legacy.proto declares extensions and is already registered with this Client as a
   different descriptor instance; extensions are matched by descriptor identity, so values decoded
   through the registered copy would be invisible through this one — register one instance and build
   extension values from it`. Files declaring no extensions keep the first-wins skip: a distinct
   instance of such a file can only cause the first case, which step 2 catches.
2. **Containing-message check (every decode).** After each decode through `c.types` — the round-trip
   guard, detail unpacking, typed capture — the SDK walks the decoded message recursively (singular
   message fields, repeated message elements, message map values, message-valued extensions) and
   requires every populated extension field's `ContainingMessage()` to be the **same instance** as the
   descriptor of the message holding it. F7 confirmed `Range` yields the resolver's extension
   descriptor, so the check sees the first case. A mismatch is refused, naming the file:
   `message built from a different descriptor instance of conformance/v1/legacy.proto than the one
   registered with this Client; build messages from the FileDescriptor passed to RegisterFiles`.

For typed capture this turns a silently invisible extension into a failure. For `Respond`, the guard's
equality comparison would already refuse in both cases, but with no clue why; these checks name the
cause. Generated messages and extensions always pass: both come from the same `GlobalFiles` instances.

**The one residue no check can see**, documented on `RegisterFiles` and in the package doc: a caller who
builds extension values from an extension descriptor it **never passed** to `RegisterFiles`, sharing an
extendee with one it did. Nothing in the message or the registry reveals that third instance. The rule
for callers is one sentence — *build extension values only from extension descriptors you registered,
or from generated code*.

**The local registry is copy-on-write.** `dynamicpb.NewTypes` documents that "it is not safe to
concurrently change the Files while calling Types methods", and parallel subtests sharing one `Client`
can call `RegisterFiles` while another subtest's `Respond` or typed capture is resolving types. So the
client mirrors the server registry's design (M3 §6): an `atomic.Pointer` to an immutable snapshot —
a `protoregistry.Files` and the `dynamicpb.Types` over it — that is never mutated once published.
`RegisterFiles` builds a fresh `Files` from the current snapshot's files plus the new ones, wraps it,
and swaps it in. A **writer mutex** serializes the whole check→RPC→build→swap sequence: the atomic
pointer alone would let two concurrent registrations both build from the same snapshot and lose one,
and releasing the mutex between the extension-identity check and the swap would let a concurrent
registration slip a different instance of the same path in between. Holding it across the RPC
serializes registrations, which is rare and cheap — the server serializes its writers the same way —
and readers never touch it.
Each operation loads one snapshot at its start and resolves through it until it finishes, so a single
`Respond` or capture never sees two different registries.

### Automatic

`Stub`, `StubYAML`, and `Verify` call `ensure(method)` before their RPC. `Calls` does not:
`ListCalls` filters by normalized name and never touches the registry
(`internal/admin/journal.go:34`), so registering on a read would be a side effect with no purpose.
`Verify` needs it because the server compiles the matcher against the method
(`internal/admin/verify.go:45`).

`ensure` does at most one lookup per service per `Client`:

1. Service already cached → done.
2. `ListServices`. If the server already lists the service → cache, done.
3. `protoregistry.GlobalFiles.FindDescriptorByName(service)`. If the test binary links the service's
   generated code → register its file and imports through the explicit path above → cache, done.
4. Otherwise do nothing. The RPC that follows fails with the server's `NOT_FOUND` ("service … is not
   registered"), passed through verbatim, followed by one SDK-sourced hint line naming `RegisterFiles`
   and `WithProtoDir`.

Each cache entry records the service's methods and their output types — from the method descriptors
in step 3, from `ListServices`' `MethodInfo` in step 2 — which is what `Respond`'s type check reads
(§5). The cache is mutex-guarded so parallel subtests sharing one server do not register twice.

**Why step 2 exists.** A server provisioned with `WithProtoDir` or `WithDescriptorSet`, or an external
server, may hold the same files compiled from source with representation differences — a vendored copy
with a different `go_package`, say. Re-sending the generated copy would fail "already registered with
different content". Checking first means automatic registration only fills gaps and never collides with
explicit provisioning. A collision on a **shared import** is still possible; it fails loudly, the
server's error names the file, and the SDK prefixes that the file came from the test binary.

**Error-detail and `Any` payload types** are registered only when they are among the method's
imports. Standard `google.rpc.*` details resolve everywhere, because the server binary links
`errdetails` (blank import in `internal/stub/status.go`). Custom detail types need `RegisterFiles`.

### Known in-process / container difference

Message, `Any`, and extension lookups fall back to the process-global registries
(`internal/schema/registry.go:247`, `:279`–`:312`; `internal/match/cel_adapter.go:245`–`:278`).
In-process, those are the **test binary's** globals, so an unregistered custom detail or `Any` type
resolves in-process but would fail against a container. Service lookup has no fallback
(`LookupMethod`, `internal/schema/registry.go:210`), so automatic registration is honestly testable
in-process; the SDK's test asserts that `ListServices` lists the service after `Stub()`, not merely
that the call worked. The package doc states the difference, and phase 8's container smoke test is
where container behavior is proven.

## 7. Verify and Calls

### Verify

```go
ok := sim.Verify(shopv1.OrderService_GetOrder_FullMethodName).
    MatchField("order_id", simulacratest.Eq("o-123")).
    Exactly(t, 2)          // or AtLeast(t, n), AtMost(t, n), Between(t, lo, hi), Never(t)

sim.Verify(method).MatchYAML(`message: { order_id: { eq: o-123 } }`).Never(t)
```

- Matcher methods and value rendering are **the same code** as the stub builder's, so a matcher cannot
  mean one thing in a stub and another in a verify. `MatchYAML` takes the full match grammar; combining
  it with structured matchers on one builder is misuse.
- `matcher_document` is the bare match block, the form `stub.ParseMatchDocument` expects
  (`internal/admin/verify.go:39`). `Between` sets `at_least` and `at_most` together, which `Times`
  allows (`verify.proto`).
- **Failure** calls `t.Errorf` with the server's `explanation` verbatim, then each actual call's
  request JSON with its `nearest_miss` verbatim — M3 §11's one diagnostic engine, the same text on every
  surface — and returns `false`. A pass returns `true`. Transport errors go through `t.Fatalf`.

### Calls

```go
calls := sim.Calls(t, shopv1.OrderService_GetOrder_FullMethodName)  // oldest first
calls[0].Request(t, &shopv1.GetOrderRequest{})
calls[0].Response(t, &shopv1.GetOrderResponse{})
calls[0].Metadata() metadata.MD
calls[0].Status() *status.Status
calls[0].StubID() string
calls[0].Proto() *adminv1.Call
```

- **Order.** `ListCalls` is newest-first (`internal/admin/journal.go:32`). The SDK **reverses** it so
  `calls[0]` is the first call, which is what test code expects; the doc comment says so. When the ring
  has wrapped, `Calls` returns what is retained.
- **Typed capture is checked.** `Request` and `Response` require exactly one message — otherwise they
  fail, pointing at `Proto()` for streams — and compare `DecodedMessage.type_name` with the target's
  full name before unmarshalling. Unmarshalling into the wrong type usually "succeeds" with the data
  in unknown fields: exactly the silent mock bug PROPOSAL §2 exists to rule out. They unmarshal with
  `proto.UnmarshalOptions{Resolver: c.types}` (§6), so a registered extension lands in its extension
  field rather than in unknown bytes.
- **Metadata** merges `values` and `binary_values` back into grpc-go's `metadata.MD` form.

## 8. Core change: `respond.literal`

**The gap.** Any string value in a response message body that contains `{{…}}` is compiled as a CEL
template site (`templateSiteRE` and `hasSites`, `internal/stub/template.go:27`, `:97`), and the grammar
has no escape. The regex is non-greedy, so even a CEL string literal cannot carry `}}`; the only
workaround today is `{`/`}` escapes inside an expression. This affects file users as much as
the SDK: a mock of a notification service that returns `Hello {{ name }}` cannot be written plainly.

**The grammar.** One field, `Literal bool \`yaml:"literal"\``, on `stub.Respond`
(`internal/stub/stub.go:30`). When `true`, **every message body in the stub** is built straight from
the document and never scanned for sites: `respond.message`, `stream[].message`, `on_open[].message`,
and `rules[].send[].message`. Status messages, details, metadata, and trailers are never templated
(templates are compiled only at `internal/stub/stub.go:207` and `:295`), so the flag does not touch
them. Omitted or `false` keeps today's behavior.

```yaml
- method: notify.v1.Templates/Get
  respond:
    literal: true
    message: { body: "Hello {{ name }}" }   # sent exactly as written
```

**Implementation.** The flag passes from `compilePlan` to `compileTemplate` to `newTemplate`, and in
literal mode takes the existing static path (`BuildMessage`). That path already serves bodies with no
sites, so this adds no rendering code — only a new way to reach it. No CEL environment or projection is
built for a literal stub.

**Contract impact.**

- **Admin API: none.** Documents pass through (M3 decision A3), and the envelope's `document` is
  re-rendered from the YAML node tree (`ParseDocument`, `internal/stub/document.go:22`), so
  `literal: true` survives `ListStubs` and `ExportStubs` with no extra code.
- **File stubs** get the fix through the same loader; `simulacra check` validates the field.
- **Docs:** a doc comment on the field now; user-facing grammar docs in phase 10.

**Scope fence.** This is the only behavior change outside `simulacratest/`. The plan's global
constraints permit edits in `internal/stub` for this field and nothing else.

## 9. Test fixtures and codegen

**Need.** Generated Go messages and a grpc-go client for `testdata/protos/shop/v1/order.proto`.
Without them, neither `GlobalFiles` registration, typed `Respond` and `Request`, nor "call the mock
with a real generated client" can be tested. Today `buf.gen.yaml` covers only `api/`, and
`protoc-gen-go-grpc` is not in `tools/`.

**Where.** `internal/testpb/shop/v1`, package `shopv1`. Committed; internal, so never public API;
importable by `simulacratest`'s external tests because they are in the same module.

**How.**

- A second template, `buf.gen.testdata.yaml`, runs `protoc-gen-go` and `protoc-gen-go-grpc` over
  `testdata/protos`. Its `clean` covers only `internal/testpb`; the existing template's `clean: true`
  keeps covering `gen/`.
- `testdata/protos` stays **out of** `buf.yaml`'s modules, so `STANDARD` lint and breaking detection
  never apply to fixtures — `enum Region { EU = 1; }` already fails the enum-value-prefix rule.
- `make generate` runs both templates. The CI step that checks `gen/` against `api/` also checks
  `internal/testpb`.
- `tools/go.mod` gains `google.golang.org/grpc/cmd/protoc-gen-go-grpc` as a tool. **Core `go.mod` does
  not change**: generated code needs only `grpc` and `protobuf`, already direct dependencies.

**The fixture stays byte-unchanged.** `order.proto` has no `go_package`. The import path is supplied by
the plugins' `M` option (`Mshop/v1/order.proto=github.com/yinghanhung/simulacra/internal/testpb/shop/v1;shopv1`).
Unlike buf managed mode, `M` does not rewrite the descriptor the plugin embeds, so the copy linked into
the test binary should be **equivalent** to what `WithProtoDir("testdata/protos")` compiles (F3). That
makes possible a test proving that `RegisterFiles` of the generated file onto a proto-dir server
registers `0 new`.

## 10. Error handling

| Situation | Behavior |
|---|---|
| Admin or transport error | `t.Fatalf`. The SDK adds only its own prefix (`simulacratest: CreateStub: `); the server's message passes through unchanged — P6's rule: never reword or reclassify an upstream diagnostic. |
| Verification verdict | `t.Errorf`, return `false` (§7). |
| Builder misuse | Collected; one `t.Fatalf` from the terminal call (§5). |
| `Respond` message is not the method's output type | `t.Fatalf` naming both full names (§5, step 1). |
| Decoded extension resolved from a different descriptor instance than the target's | `t.Fatalf` naming the file and pointing at `RegisterFiles` (§6, check 2). |
| `RegisterFiles` given a different instance of an extension-declaring file already resolvable | `t.Fatalf` naming the file, before anything is sent (§6, rule 1). |
| Round-trip guard mismatch | `t.Fatalf` naming the message type and that the stub grammar cannot represent the value; points at `StubYAML`. |
| Unregistered service | Server's `NOT_FOUND` verbatim, plus one SDK hint line (§6). |
| Shutdown failure in cleanup | `t.Errorf` (§3). |

Every helper calls `t.Helper()`, so failure lines point at the test.

## 11. Testing

The project conventions carry over: `-race -count=1`, no `time.Sleep` to order an assertion, and every
discriminating test carries a named mutation check (state the mutation, confirm the test fails, revert).

| Layer | Tests |
|---|---|
| Builder, no server | Golden JSON documents for every matcher value type: each integer width, `uint64 > MaxInt64`, `float32` widening, named and unnamed enum values. Refusal of `[]byte`, messages, maps, NaN, ±Inf. All 16 non-OK codes in the table are parsed by grpc-go's own `codes.Code.UnmarshalJSON`. Duplicate header key and non-UTF-8 header value refused. Round-trip guard refuses a message carrying unknown fields. |
| Response type check | `Respond(t, &shopv1.GetOrderRequest{OrderId: "o-1"})` on `GetOrder` is refused with both names, for a service resolved from `GlobalFiles` and for one resolved from `ListServices`. Mutation: remove the check — the stub is then created, and the generated client receives a `GetOrderResponse` carrying `order_id`, proving the server alone would not catch it. |
| `respond.literal` (`internal/stub`) | A `{{ message.order_id }}` string with `literal: true` comes back unchanged at every site position: top level, nested message, map value, repeated element, `Any` payload, server-stream step, bidi `send`. Regression: the same stub without the flag still renders the template. `CreateStub` with `literal: true` returns it in the envelope's `document`. Mutation: drop the flag in `compileTemplate`. |
| Registration | `ListServices` lists the service after `Stub()`. Against a `WithProtoDir` server, no `RegisterSchemas` is sent — observed through a decorator on the admin client, the seam P5 §8 introduced, exposed to tests via `export_test.go`. An unregistered service yields `NOT_FOUND` verbatim plus the hint. `RegisterFiles` of the generated shop file onto a proto-dir server registers `0 new`. |
| Typed responses, end to end | A generated grpc-go client calls a stub built from `Respond(msg)`; the reply equals `msg` by `proto.Equal` **and** deterministic bytes. Cases: a literal `{{ }}` string, 64-bit extremes, `Timestamp`, enums, `Any`. A table of hard cases on the conformance protos as `dynamicpb` messages, registered explicitly: proto3 `optional` presence, `oneof`, maps with message values, proto2 extensions and groups, editions. The extension case (`conformance.v1.ext_tag`) is the discriminating one for the resolver — mutation: render and guard with protojson's default resolver, which fails it with an unknown-field error. `RespondStatus` with `PreconditionFailure`, `BadRequest`, `RetryInfo` arrives with equal details. |
| Verify and Calls | Failure output contains **the same text a direct `VerifyCalls` returns**, compared against the RPC's own response, never hard-coded strings. The recording double embeds `testing.TB` (its unexported method makes a from-scratch fake impossible) and overrides `Errorf`/`Fatalf`, with `Fatalf` calling `runtime.Goexit` inside a goroutine the test joins. `Calls` is oldest first; a `type_name` mismatch is refused; `-bin` metadata merges back; a captured `dynamicpb` request carrying a registered extension has it in the extension field, not in unknown bytes. |
| Concurrency and lifecycle | One parent server shared across parallel `t.Run` subtests, each passing its own `t`, with `ensure`'s cache under `-race`. `Reset` removes API stubs, empties the journal, and restores a file stub's `times` budget. `Connect` against an in-process server's addresses runs the same helpers. Cleanup closes `Dial` connections before shutdown. |
| Descriptor-instance check | Two independent compiles of `legacy.proto`: register instance A, then capture a call into, and `Respond` with, a `LegacyReply` built from instance B carrying `ext_tag`. Both are refused naming the file. The same flow with instance A passes, and `Has(ext_tag)` is `true` through A's descriptor. Mutation: remove the check — capture then "succeeds" with `Has(ext_tag)` `false`, the silent failure F7 recorded. The check is also exercised on an extension nested inside a repeated message field. |
| Extension-identity rule | **Shared extendee:** an extension file built twice with `protodesc.NewFile` against one instance of its message file (F7's second case). `RegisterFiles(e1)` succeeds; `RegisterFiles(e2)` is refused naming the file, and the server receives no RPC for it (decorator count). Mutation: restore the silent first-wins skip — `RegisterFiles(e2)` passes, and capturing a call whose value was set through `e2`'s extension yields `Has` `false`, F7's silent failure. **Linked copy:** the `GlobalFiles` lookup goes through a seam (`export_test.go`) so a test can supply a registry that "links" a different instance of an extension file; `RegisterFiles` of the caller's instance is refused. Re-registering the *same* instance, and a distinct instance of a file declaring no extensions, both pass. |
| Local registry concurrency | Under `-race`: parallel subtests call `RegisterFiles` with synthetic files on distinct paths while others run `Respond` and typed capture of the `ext_tag` message through the same `Client`. Mutation: register into one shared, mutable `Files` instead of swapping snapshots — `-race` reports it. A functional union test, which `-race` cannot replace: concurrent `RegisterFiles` of disjoint files all land in the final snapshot (observed via `export_test.go`). Mutation: drop the writer mutex — registrations are lost. |
| Cleanup-time calls | A subtest registers `t.Cleanup(func() { sim.Verify(m).Exactly(t, 1) })` and `t.Cleanup(func() { sim.Reset(t) })` after `Start`; both run against the live server and the subtest passes. Mutation: run RPCs under `t.Context()` — both fail with cancellation. |
| Transport release | A server started directly with `server.Start` and a counting listener supplied through `server.Options.Listen` (the facade's existing seam) sees the admin connection a `Connect`-ed client opened close when that client's subtest ends — synchronized on the listener's own signal, no sleep. Mutation: drop the `Connect` cleanup — the connection stays open. |

The package doc comment carries one complete, compiled example.

## 12. Risks

| Risk | Mitigation |
|---|---|
| The YAML hop between the SDK's JSON and the server's protojson loses information the SDK cannot see (suspected: `-0.0`) | The round-trip guard reproduces that hop and refuses on any difference; F6 probes it before the plan. A loss found there is recorded as a known limit, not worked around in the SDK. |
| In-process tests pass where a container would fail, because the server shares the test binary's global registries (§6) | Documented in the package doc; auto-registration's own test checks `ListServices`, which has no fallback; phase 8's container smoke test proves container behavior. |
| Auto-registration collides with an explicitly provisioned shared import | Fails loudly with the file named and its provenance stated. Step 2 of `ensure` prevents the common case. |
| Go-generated well-known-type descriptors differ from protocompile's copies, so every auto-registration conflicts | F1 probes it. If they differ, the walk omits `google/protobuf/*` files the server already provides, and this spec is amended to say so. |
| A caller builds extension values from an extension descriptor it never registered, sharing an extendee with one it did (§6, residue) | Undetectable by construction. Documented on `RegisterFiles` and in the package doc. `Respond` still refuses through the guard's equality comparison (F7: `proto.Equal` is `false`); only typed capture is exposed, and only for `dynamicpb` callers — generated code cannot reach it. |
| The typed builder and the JVM DSL drift | Both render the same JSON document for the same stub; phase 9 can reuse this phase's golden documents as its own fixtures. |

## 13. Decisions flagged for review

| | Decision | Rationale | Reversibility |
|---|---|---|---|
| **D1** | **`simulacratest` in the core module** (§1). | Version-locked to `server/`; no new core dependency; M3 §8's reason for a separate module applies only to `simulacratc`. | Moving it later is a breaking import-path change for users — decide now. |
| **D2** | **`respond.literal`** added to the stub grammar (§8). | Closes a real gap for file users and makes the typed builder exact. | Additive; removing it later would be breaking. |
| **D3** | **Automatic registration from `GlobalFiles`**, skipped when the server already lists the service (§6). | Go's equivalent of M3 decision C2 ("no proto files in your test"). | Can be disabled later by an option; never silently wrong, because failures are loud. |
| **D4** | **The builder covers the unary grammar only** (§2). | Matches the JVM DSL sketch (M3 §9); `StubYAML` covers the rest. | Purely additive. |
| **D5** | **Verification failures are `t.Errorf` + `bool`**, not `t.Fatalf` (§7). | Several verifies in one test all report. | Changing to fatal later would alter test outcomes — decide now. |

## 14. Facts for the plan

Verified while writing this design, from the source at `aa064ed`:

- `server.Start` permits schema-less startup when `AdminAddr` is set (`server/server.go:140`) and
  returns bound addresses for `:0` listeners (`DataAddr`, `AdminAddr`).
- Response message bodies are built by `json.Marshal` of the YAML-decoded `map[string]any` followed by
  `protojson.Unmarshal` (`BuildMessage`, `internal/stub/stub.go:319`).
- Template sites are detected by `(?s)\{\{(.*?)\}\}` on string values at any depth, with no escape
  (`internal/stub/template.go:27`, `:97`), and compiled only for `respond.message` and step messages
  (`internal/stub/stub.go:207`, `:295`).
- `ParseDocument` renders the normalized document from the YAML node tree, not the struct
  (`internal/stub/document.go:22`), under `KnownFields(true)` strict decoding.
- Structured matcher literals: integer kinds require numeric literals; enums accept names or in-range
  numbers; bytes, message, and map fields are rejected (`internal/match/match.go:462`).
- `RegisterSet` treats same-path files as equal after stripping only source info and buf's image
  extension (`normalizeFileProto`, `internal/schema/registry.go:398`).
- `LookupMethod` has no global fallback (`internal/schema/registry.go:210`); `LookupMessage` and the
  type resolvers do (`:247`, `:279`–`:312`).
- `ListCalls` is newest first and filters by name only (`internal/admin/journal.go:32`).
- `VerifyCalls` parses `matcher_document` as a bare match block and compiles it against the registry
  (`internal/admin/verify.go:39`, `:45`).
- The server binary links `errdetails` (blank import, `internal/stub/status.go`).
- Response metadata is `map[string]string`; `-bin` values are raw bytes in a string; other values must
  be printable ASCII (`internal/stub/metadata.go:22`).
- `serve`'s default journal size is 1024 (`internal/cli/serve.go:82`); the CLI's per-RPC default
  timeout is 30 seconds (`internal/cli/client.go:26`).
- `ListServices` fills `MethodInfo.output_type` with the bare `FullName()` (`internal/admin/schema.go:76`).
- `testing.T.Context` is documented as "canceled just before Cleanup-registered functions are called"
  (Go 1.25 `go doc testing.T.Context`).
- `shop.v1.GetOrderRequest` and `shop.v1.GetOrderResponse` both declare `string order_id = 1`
  (`testdata/protos/shop/v1/order.proto`), so a request holding only `order_id` fits the response type.
- `conformance.v1.ext_tag` is a proto2 extension of `LegacyReply`
  (`conformance/protos/conformance/v1/legacy.proto:18`), absent from `GlobalTypes` in any test binary.
- `Registry.AddFile` skips a path that is already registered — first registration wins
  (`internal/schema/registry.go:162`).
- `adminv1connect`'s five clients are interfaces, so a decorator needs no new abstraction (P5 §11).

Verified by throwaway probe during spec review:

- **F7 — verified** by a throwaway probe (protocompile v0.14.1, protobuf v1.36.11; deleted) compiling
  `conformance/v1/legacy.proto` twice into instances A and B, with a local `Files` holding A and
  `dynamicpb.NewTypes` over it:
  - *Same instance:* wire and protojson decoding of a `LegacyReply` carrying `ext_tag` into an
    A-built message succeed; `Has(ext_tag)` is `true` and `Get` returns the value; `proto.Equal`
    to the source holds.
  - *Different instance:* decoding into a B-built message also succeeds — **no error, no panic, no
    unknown bytes** — but `Has(ext_tag)` through B's descriptor is `false` and `Get` returns `""`;
    `proto.Equal` to the source is `false`.
  - *Detection:* in the different-instance case, `Range` yields an extension field whose
    `ContainingMessage()` is not the decoded message's `Descriptor()` instance; in the same-instance
    case it is. This is §6's check 2.
  - *Shared extendee (second probe, built with `protodesc.NewFile`):* a message file `base.proto`
    built once, and an extension file `ext.proto` extending it built **twice** against that single
    instance — `e1`, `e2`. Local registry holds `base` and `e1`; the source value is set through
    `e2`'s extension. Wire and protojson decoding succeed with no error and no unknown bytes;
    `ContainingMessage()` **equals** the target's descriptor, so check 2 cannot see it; `Has` through
    `e2`'s extension is `false`, through `e1`'s is `true`; `proto.Equal` to the source is `false`. This
    is why §6 needs rule 1 at registration.
- `dynamicpb.NewTypes` documents that "it is not safe to concurrently change the Files while calling
  Types methods"; `protoregistry.Files` documents only its `Find` and `Range` methods as safe for
  concurrent use.

To establish by throwaway probes before the plan quotes code:

- **F1** — A generated-descriptor closure that includes `google/protobuf/*.proto` registers as `0 new`
  against a registry whose well-known types came from protocompile's standard imports.
- **F2** — `buf generate` with `buf.gen.testdata.yaml` accepts `testdata/protos` as an input while it is
  not a module in `buf.yaml`, without applying lint. Fallback: a separate `buf.yaml` inside
  `testdata/protos`.
- **F3** — With the `M` option, the descriptor embedded in `internal/testpb/shop/v1` is equivalent to
  the protocompile copy under `normalizeFileProto`. Fallback: add `go_package` to `order.proto` after
  confirming no golden output depends on its options.
- **F4** — grpc-go v1.82.0's `codes.Code.UnmarshalJSON` accepts `"FAILED_PRECONDITION"` and rejects
  `"FailedPrecondition"`, for all 16 non-OK codes.
- **F5** — yaml.v3 decodes JSON integer literals into `int` (and `uint64` above `MaxInt64`) exactly,
  including both 64-bit extremes.
- **F6** — protojson output → yaml.v3 → `encoding/json` → protojson is lossless across the hard cases
  in §11, or the exact losses are listed (suspected: `-0.0`).
