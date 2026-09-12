# Simulacra M3 Phase 4b — Admin Data Services: Design

**Goal (M3 design §13.4, second half):** serve the four data services — `SchemaService`,
`StubService`, `JournalService`, `VerifyService` — on the control plane Phase 4a stood up, together
with the call rendering they share and the core-error → connect-code table they need.

**Reference:** `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §3 (decision A3), §5
(admin API), §11 (error handling), §12 (testing);
`docs/superpowers/specs/2026-08-21-m3-p3-core-extensions-design.md` §3–§4 (the core surfaces
translated here) and §6 (the error mapping it anticipated);
`docs/superpowers/specs/2026-08-22-m3-p4a-admin-transport-design.md` §2–§3 (package boundary,
teardown); `PROPOSAL.md` §4, §8.

Phase 4a is complete at `11446f4`. The `simulacra.admin.v1` contract was frozen in Phase 2 and is
not modified here.

---

## 1. Scope

### In scope

1. `internal/admin` handlers for `SchemaService` (2 RPCs), `StubService` (5), `JournalService` (3),
   and `VerifyService` (1), registered by `Install` on the mux 4a built.
2. Rendering — `journal.Call` → `adminv1.Call`, shared by `ListCalls`, `WatchCalls`, and
   `VerifyCalls` — and the invalid-UTF-8 rule every 4b response follows (§5).
3. The core-error → connect-code mapper M3 §11 describes and 4a deferred (§7).
4. `Deps.Stopping`, so `WatchCalls` ends its streams when teardown begins, with teardown taking
   precedence over buffered calls (§6).
5. Per-route request-size caps: 32 MiB for `SchemaService`, 4 MiB for every other service (§8).
6. Three small core changes: `schema.ErrUnknownMethod`, call snapshots on `journal.Report`, and
   int32 bounds on stub `priority` and `times` (§3).

### Out of scope

- CLI client commands — Phase 5. Reflection import — Phase 6. SDKs — Phases 7 and 9. Dockerfile —
  Phase 8. The conformance parity leg — Phase 10.
- `api/` and `gen/`.
- `VerifySequence`, server-side upstream import, admin authentication, TLS — M3 non-goals (M3 §2).
- 4a's `holdActiveHTTP1Request` soft spot. 4a expected `WatchCalls` to replace that fixture with a
  genuinely long-lived request, but §6 makes `WatchCalls` end when teardown begins, so it can no
  longer hold teardown open. Those tests stay as they are.

### Dependency note

No new modules. Rendering uses `protojson`, `timestamppb`, and `durationpb` from
`google.golang.org/protobuf`, already required (the generated `journal.pb.go` imports the latter
two).

---

## 2. Package boundary and wiring

4a's split holds: `internal/admin` owns what is served, `server` owns when it runs.

| File | Contents |
|---|---|
| `internal/admin/schema.go` | `schemaService` |
| `internal/admin/stub.go` | `stubService`, envelope conversion |
| `internal/admin/journal.go` | `journalService`, including `WatchCalls` (§6) |
| `internal/admin/verify.go` | `verifyService` |
| `internal/admin/render.go` | `journal.Call` → `adminv1.Call`, and `validUTF8` (§5) |
| `internal/admin/errors.go` | the mapper and `invalidArgument` (§7) |

Each service struct holds `Deps` and, like `controlService`, does **not** embed its generated
`Unimplemented…Handler`: an RPC added to the contract must fail the build here, not return
`Unimplemented` at runtime. Every handler returns its error through the mapper.

### `Deps.Stopping`

```go
// Stopping is closed when server teardown begins. WatchCalls ends its
// streams on it.
Stopping <-chan struct{}
```

`validate` requires it. `server/admin.go` passes `s.stopping`, which `Start` creates in the `Server`
literal (`server/server.go:177`) before it calls `startAdminPlane` (`server/server.go:202`). This is
the only change in `server`, and it amends 4a §2's expectation that 4b would touch nothing there —
see decision 1.

---

## 3. Core changes

### 3.1 `schema.ErrUnknownMethod`

M3 §11 maps an unknown method to `NOT_FOUND`, but `LookupMethod` returns untyped errors, so a
handler cannot tell "not registered" from "malformed". The two not-registered cases — the service
is absent, or the method is absent on a present service — become
`errors.Is(err, schema.ErrUnknownMethod)`. Their text is unchanged, pinned by a test, because
loader and `check` diagnostics already print it. A malformed name stays untyped — and malformed
means more than a missing separator: the service name and the method name are checked against
protobuf identifier syntax (`protoreflect.FullName.IsValid` and `Name.IsValid`) before the registry
is consulted, so `"garbage"`, `"pkg.Svc/"`, `"bad service/GetOrder"`, `"pkg.Svc/Get Order"`, and an
extra separator such as `"pkg.Svc/extra/GetOrder"` are all `INVALID_ARGUMENT`. Only a syntactically
valid name the registry does not declare is `NOT_FOUND`. The distinction is load-bearing:
`NOT_FOUND` tells an SDK to register its schemas, which can never help a name no schema could
declare.

The chain reaches the handlers intact: `Compiler.Compile` wraps with `%w`
(`internal/stub/stub.go:142`) and `CompileMatch` returns the error as it is.

### 3.2 `journal.Report` carries call snapshots

`VerifyCallsResponse.actual` needs the calls, but `Report` holds only sequence numbers. Joining
them against a second `List()` would read a different snapshot: a `Reset` or ring overwrite between
the two reads silently drops entries from `actual`, and nothing tells the client. So `Verify`
returns the snapshots it judged:

```go
type Miss struct {
	Call    *Call // was: Seq uint64
	Reasons []string
}

type Report struct {
	Pass              bool
	Matched           int
	Considered        int
	Want              string
	Misses            []Miss
	UnexpectedMatches []*Call // was: []uint64
}
```

The calls come from `Verify`'s own `List()`, which already clones, so the report aliases nothing
the ring holds. `internal/journal`'s own tests consume `Report`, and so does
`conformance/harness_test.go`, which formats a verification failure's `Misses` and
`UnexpectedMatches` for a human reader.

### 3.3 Stub integers fit the envelope

`Stub.Priority` and `Stub.Times` decode into Go `int`, but `adminv1.Stub` carries them as `int32`.
`Compile` checks only that `times` is not negative, so on a 64-bit host `times: 2147483648` and
`priority: 2147483648` both compile today, and converting either to `int32` yields `-2147483648`
(verified, §12). `Compiler.Compile` gains the missing bounds — `priority` within the int32 range,
`times` within `0..2147483647` — each rejected with a diagnostic naming the field and the value.

The bound lives in the compiler, not in the handlers, because files and the API share the compiler
(M3 §3, decision A3: one grammar, one validator). That also settles oversized file-origin values:
there are none for `ListStubs` to report, because a stub file carrying one fails the same way —
`serve` refuses to start, as it does for any invalid stub (`loadStubs`, `server/server.go:406`); a
hot reload is rejected whole and reported, leaving the previously loaded stubs serving
(`reconcileStubDirs`, `server/watcher.go:189`); and `check` lists it. The bound also
stops the accepted range from depending on the width of Go's `int`.

`CreateStub` and `ReplaceAllStubs` compile before they touch the store, so an out-of-range value is
`INVALID_ARGUMENT` with the store untouched. The envelope conversion is then lossless; `hits` is
already `int64`.

This tightens the file grammar: a stub file that loads today with an out-of-range `priority` or
`times` stops loading — decision 11.

---

## 4. RPC semantics

### 4.1 Shared rules

- **Method filters** (`ListStubs.method`, `ListCalls.method`, `WatchCalls.method`) accept
  `pkg.Service/Method` with or without the leading slash and normalize to `/pkg.Service/Method`.
  They do not require the method to be registered: listing or tailing a method before its schema
  arrives is legitimate, and a mistyped filter shows nothing rather than producing a false verdict.
  `VerifyCalls.method` accepts the same two forms but, unlike a filter, must resolve (§4.5).
- **Envelopes** (`adminv1.Stub`) map `match.Shape` and `stub.Origin` to their enums one to one, and
  convert `priority` and `times` losslessly because §3.3 bounds them at compile time. `CreateStub`
  and `ReplaceAllStubs` report `hits: 0`, the value at creation.

### 4.2 `SchemaService`

**`RegisterSchemas`** decodes `descriptor_set` as a `FileDescriptorSet` and calls
`Registry.RegisterSet`. Undecodable bytes, an empty set, a set that is not self-contained, and a
path already registered with different content are all `INVALID_ARGUMENT`, carrying
`RegisterSet`'s message verbatim (it names the file). `registered_files` is sorted so the response
is deterministic; `service_count` is read after the swap.

**`ListServices`** returns every registered service sorted by fully-qualified name, each with its
descriptor path in `file` and its methods in declaration order: bare name, fully-qualified input
and output types, and both streaming flags.

### 4.3 `StubService`

**`CreateStub`** runs `stub.ParseDocument`, then `Compiler.Compile` with the source label
`document`, sets `Compiled.Document` from the normalized document, and calls `Store.Add`. Parse and
compile diagnostics pass through verbatim.

**`ListStubs`** lists every origin for `STUB_ORIGIN_UNSPECIFIED`; an enum value outside the
declared ones is `INVALID_ARGUMENT`. Order is the store's: methods sorted, selection order within a
method.

**`DeleteStub`**: an empty `id` is `INVALID_ARGUMENT`; an unknown one is `NOT_FOUND`; a file-origin
stub is `FAILED_PRECONDITION`, naming the owning file (`stub.FileOwnedError`).

**`ReplaceAllStubs`** parses and then compiles each document, in order, before touching the store.
The first failure is returned, prefixed `documents[i]:`, and the store is untouched. Otherwise one
`Store.ReplaceOrigin(OriginAPI, …)` swaps the API-origin stubs; file-origin stubs and their `times`
budgets are untouched (Phase 3 §3.4). An empty `documents` clears the API stubs. The response lists
the new envelopes in input order.

**`ExportStubs`** renders the API-origin stubs, in `ListStubs` order, as one file-grammar sequence
via `stub.RenderSequence`; `stub_count` is their number. Loading the exported file back yields the
same relative selection order, because file order becomes ingest order and the priority sort is
stable. With no API stubs the document is an empty sequence and `stub_count` is 0.

### 4.4 `JournalService`

**`ListCalls`** returns calls newest first. `limit` 0 means no limit, a negative `limit` is
`INVALID_ARGUMENT`, and `limit` N keeps the newest N, as `journal.Filter` already does.

**`ResetJournal`** calls `Journal.Reset`: retained calls are cleared and sequence numbers keep
counting.

**`WatchCalls`** — §6.

### 4.5 `VerifyService.VerifyCalls`

Validation, in order:

1. `method` is required.
2. `times` is converted to `journal.Times` and validated. An absent `times` is `INVALID_ARGUMENT`;
   there is no implied default. SDKs choose their own, and a server default could be added later
   without breaking anyone, whereas removing one would break clients.
3. `matcher_document` goes through `stub.ParseMatchDocument`; an empty document matches every call
   to the method.
4. `Compiler.CompileMatch(method, block)` resolves the method **even when the block is nil**, and an
   unknown method is `NOT_FOUND`. Without that, a mistyped method asserted with `never` passes
   silently — the classic verification false positive. The journal does record calls to
   unregistered methods, so this gives up counting them through `VerifyCalls`; `ListCalls` still
   shows them.

Then `journal.Verify`. The response:

- `passed` and `matched`.
- `explanation`: one line giving the verdict, the method, the expected count (`Report.Want`), the
  matched count, and how many calls were considered. Its wording is not a contract.
- `actual`, on failure only:
  - **Too few matches** — each call to the method that did not match, with `nearest_miss` set to
    that miss's reasons joined by `"; "`. That is the same engine (`match.Compiled.Explain`) and
    the same joiner the data plane's no-match error uses (`internal/dataplane/server.go:352`).
  - **Too many matches** — each matching call (`Report.UnexpectedMatches`), with `nearest_miss`
    empty: they matched, so there is no miss to explain.

---

## 5. Rendering

### 5.1 Calls

One function turns a `journal.Call` into an `adminv1.Call`. It runs on the handler goroutine, never
under the journal lock, and resolves types through `Registry.Types()`, so a type registered after
the call was recorded still resolves.

| Field | Rendering |
|---|---|
| `seq`, `method` | as recorded |
| `request_metadata` | one `MetadataEntry` per key, keys sorted; keys ending `-bin` fill `binary_values`, all others fill `values`, values in recorded order |
| `requests`, `responses` | one `DecodedMessage` each (below) |
| `status` | always set; a nil `Call.Err` is code 0, since the data plane sets `Err` only on failure (`internal/dataplane/server.go:89-93`); `details` renders each `Any` as a `DecodedMessage` after resolving its type URL |
| `matched_stub_id` | `Call.StubID` |
| `start`, `duration` | `timestamppb` / `durationpb` |

Every string in the table also passes through `validUTF8` (§5.2).

**`DecodedMessage`.** `type_name` is the descriptor's full name. `wire_bytes` is a deterministic
marshal. `json` is protojson with **proto field names** and the registry's resolver — proto names
because matcher field paths resolve by proto name (`internal/match/match.go:378`), so a key captured
from `json` is exactly what a test writes back into a matcher: `order_id`, not `orderId`. Both
marshals allow partial messages; the journal holds whatever the data plane decoded, and rendering
must not reject it.

**Rendering never fails an RPC.** If protojson cannot render a message — for example an `Any` field
holding a type the registry does not know (verified, §12) — that `DecodedMessage` keeps `type_name`
and `wire_bytes` and leaves `json` empty. A status detail whose type URL does not resolve keeps the
type name taken from the URL and its raw bytes, with `json` empty. One unrenderable call must not
make `ListCalls` unusable, and `wire_bytes` still lets a client holding the generated type decode
it. §5.2 closes the other way a response could fail to serialize.

### 5.2 Invalid UTF-8

Every `string` field in the admin contract is proto3, and protobuf refuses to marshal a proto3
string holding invalid UTF-8, in binary and in JSON alike (verified, §12). A single such string
fails serialization of the **whole** response, not just its own entry: one bad header on one call
would make `ListCalls` fail for the entire journal, and would end every `WatchCalls` stream it
reached.

Reachable today, each verified by driving it through the real ingress path (§12):

- **`Call.method`** — grpc-go accepts a `:path` containing invalid UTF-8, and the journal records it
  unchanged.
- **`MetadataEntry.values`** — grpc-go passes non-`-bin` metadata values through unvalidated
  (HTTP/2 field values may carry bytes ≥ 0x80), and the journal records them unchanged.
- **`RegisterSchemasResponse.registered_files` and `ServiceInfo.file`** — `RegisterSet` accepts a
  descriptor whose file path is not UTF-8. Here the failure would be worse than an error: the
  registry is already mutated when the response fails to serialize, so a successful registration
  is reported as failed, and `ListServices` fails from then on.

Also possible, though not reproducible on this development machine's filesystem: `Stub.id`,
`Stub.source`, and `Call.matched_stub_id` embed stub file paths, which filesystems such as ext4
allow to be arbitrary bytes.

**The rule.** Every string field that 4b writes into a response passes through one helper,
`validUTF8`, which replaces each run of invalid bytes with U+FFFD (`strings.ToValidUTF8`). The
rule is deliberately blanket — it covers fields that are valid today for reasons outside this
package, such as header-name validation or `%q` quoting in diagnostics — because a blanket rule is
one a reviewer can check without tracing where every value came from, and the helper returns
valid input unchanged.

**Why replace**, rather than the alternatives:

- Omitting the value hides that the header, call, or file existed.
- Moving the value into `binary_values` breaks the contract's documented rule that the key suffix
  alone decides which of `values` and `binary_values` an entry uses.
- Escaping (`\xff`) is indistinguishable from a client that literally sent a backslash.

**Presentation only.** The journal, the store, and the registry keep the bytes they received, so
matching and verification see exactly what the client sent; sanitizing at ingress instead would
change what matchers and response templates observe. `matched_stub_id` and `Stub.id` pass through
the same helper, so they still compare equal to each other; file-origin stubs cannot be deleted by
ID in any case, and API-origin IDs are ASCII.

---

## 6. `WatchCalls` lifecycle

The handler subscribes with `Journal.Watch`, passing the request context and the normalized method
filter, defers `Close`, sends `nil` on the stream, and loops: it selects on the subscription channel
and on `Deps.Stopping`, renders each call it receives, and sends it. Selecting on `Stopping` is what
ends an idle stream as soon as teardown begins.

The `Send(nil)` runs once, immediately after subscribing and before the loop. connect's
server-streaming client call does not return until the handler writes something — response headers
flush on the first `Send`, not when the handler starts — so without it, a stream that has no call
yet to deliver leaves the client's stream-establishing call blocked.

**Teardown takes precedence over buffered calls.** The select alone does not give it precedence:
when a call is buffered and `Stopping` has closed, both cases are ready, and Go chooses between
ready cases at random. The handler could then start another `Send` after teardown began, and a peer
that is slow to read can hold that `Send` for the whole grace period. So **`Stopping` is checked
again, without blocking, immediately before every `Send`** — after the call has been received and
rendered — and if it has closed, the handler returns `UNAVAILABLE` without sending.

The stream ends for exactly one of four reasons:

| Reason | Detected by | Client sees |
|---|---|---|
| Client cancels | the subscription closes with `Err()` nil and the request context done | `CANCELED` (connect's own context mapping) |
| Teardown begins | `Deps.Stopping`, observed by the select or by the check before a `Send` | `UNAVAILABLE`, "server shutting down"; calls still buffered are dropped |
| Slow consumer evicted | the subscription closes with `Err() == journal.ErrSlowConsumer`, after its buffered calls drain | the buffered calls, each still behind the check before its `Send`, then `RESOURCE_EXHAUSTED` ("client too slow — reconnect") |
| Send fails | `Send` returns an error | nothing; the connection is gone |

Draining before `RESOURCE_EXHAUSTED` gives the client a precise resume point: it holds every call up
to the last `seq` it received. Teardown does not drain — the server is going away, and calls
recorded in its last moments are not worth holding teardown for.

**Why end streams at all.** Otherwise a single `calls tail` holds every graceful shutdown for the
full `shutdownGrace` (5s), after which 4a's force-close hands the client a transport error rather
than a status. 4a's final review predicted this and sized `dataGraceFloor` for it. Ending the stream
on `Stopping` makes the common case fast and gives the client a status it can act on.

**The guarantee, and its limit.** Once a check before a `Send` observes `Stopping` closed, no
further `Send` starts — the header-flush `Send(nil)` is the one `Send` that runs before the loop
with no `Stopping` check, and that is harmless: it writes zero bytes, and HEADERS are not subject to
data flow control. What remains is the window between a check that passed and the `Send` it
guards: if teardown begins inside that window, that one `Send` still runs, and against a peer that
is not reading — flow control full — it blocks until 4a's drain bound force-closes the connection
(4a §3.4). So at most one message can be in flight after teardown begins, and how long it can hold
teardown is bounded by 4a, not by this design. Closing the window entirely would require
interrupting a `Send` in progress, which connect does not offer, and writing the response from a
second goroutine after the handler returns is not allowed.

---

## 7. Error handling

`internal/admin/errors.go` holds one mapper that every handler returns through, and an
`invalidArgument(err)` wrapper that handlers apply where they consume the caller's input. Rows are
evaluated in order:

| # | Error | Code |
|---|---|---|
| 1 | already a `*connect.Error`, or `context.Canceled` / `context.DeadlineExceeded` | returned unchanged; connect maps context errors itself (`wrapIfContextError`, connect v1.20.0) |
| 2 | `errors.Is(err, schema.ErrUnknownMethod)` | `NOT_FOUND` |
| 3 | `errors.Is(err, stub.ErrStubNotFound)` | `NOT_FOUND` |
| 4 | `errors.As` finds a `*stub.FileOwnedError` | `FAILED_PRECONDITION` |
| 5 | `errors.Is(err, journal.ErrSlowConsumer)` | `RESOURCE_EXHAUSTED` |
| 6 | wrapped by `invalidArgument` | `INVALID_ARGUMENT` |
| 7 | anything else | `INTERNAL` |

Messages pass through unchanged; the mapper never adds a prefix.

**Typed errors win over the input mark.** A `CreateStub` document naming an unregistered method fails
inside the marked compile step, and row 2 makes it `NOT_FOUND`, which lets an SDK say "register
schemas first" rather than "your document is malformed".

**Unmarked means `INTERNAL`.** The untyped diagnostics from the loader, compiler, CEL, and
`RegisterSet` become `INVALID_ARGUMENT` only through a mark. A handler that forgets one surfaces
`INTERNAL` in its tests — loudly — instead of reporting a server fault as the client's.

**Where marks go:** stub and matcher document parsing; `Compile` (which now includes §3.3's
bounds) and `CompileMatch`; descriptor-set decoding and `RegisterSet`; `journal.Times` validation;
and request-field checks — an empty `id` or `method`, a negative `limit`, an out-of-range origin
enum.

**Two outcomes bypass the mapper's typed rows.** A request over its size cap is
`RESOURCE_EXHAUSTED`, produced by connect before the handler runs (`wrapIfMaxBytesError`, connect
v1.20.0). Teardown's `UNAVAILABLE` is a `*connect.Error` that `WatchCalls` constructs, so row 1
passes it through.

This resolves a conflict between the earlier designs: Phase 3 §6 maps compile diagnostics to
`INVALID_ARGUMENT` wholesale, while M3 §11 maps an unknown method to `NOT_FOUND`. This design
follows M3 §11 for the unknown method and Phase 3 §6 for everything else (decision 4).

---

## 8. Request-size caps

4a capped every admin request at 4 MiB (`maxRequestBytes`, grpc's default receive limit) and left
Phase 4b to revisit the cap for descriptor sets. `buf build` images include source info by default
and grow with the repository; SDK-built sets carry none and stay small.

- **`SchemaService`**: 32 MiB (`maxSchemaRequestBytes`), applied both as the handler's
  `connect.WithReadMaxBytes` and as a per-route `http.MaxBytesHandler`.
- **Every other service**: 4 MiB, applied the same two ways.
- **The mux-wide and outer transport wrappers** rise to 32 MiB, the largest route cap. Inside them
  each route enforces its own cap; the outer wrapper's remaining job is bounding x/net's
  `h2cUpgrade` body read, which happens before routing.

`TestOversizedRequestIsRejected`, which sends 8 MiB to `ControlService`, keeps passing unchanged.

The cost is accepted: the worst-case allocation on the unauthenticated upgrade path grows from 4 to
32 MiB, on a plane that binds loopback by default.

---

## 9. Testing

**Handler tests** in `internal/admin`, per service, on the existing pattern — `installed()` plus the
generated connect client over HTTP/1.1: every RPC's success path, and every row of the §7 table
asserted as a code through a real RPC, not only through the mapper — with one reasoned exception:
row 7 (`INTERNAL`, the unmarked default) is asserted only at the unit level, in `errors_test.go`,
because no handler can be driven to an unmarked failure from outside; every marked path a handler
can reach is either a typed row or `invalidArgument`.

**Unit tests:**

- `errors_test.go` — the mapper as a table, including a typed error inside an `invalidArgument` mark
  (typed wins), an unmarked untyped error (`INTERNAL`), and context errors passed through.
- `render_test.go` — dynamic messages with proto-name JSON; an `Any` field holding an unregistered
  type (keeps `wire_bytes`, empty `json`); `-bin` metadata in `binary_values`; a nil `Err` rendered
  as code 0; decoded status details; an unresolvable detail; `validUTF8` on valid, invalid, and
  mixed input; stub envelope conversion with a synthetic non-UTF-8 `ID` and `Source`, which no
  development filesystem here can produce.
- `internal/schema` — `ErrUnknownMethod` for both not-registered cases and not for malformed names,
  with the existing message text pinned.
- `internal/journal` — `Report` carries the evaluated snapshots in both failure directions.
- `internal/stub` — `Compile` rejects `priority` of 2147483648 and of -2147483649 and `times` of
  2147483648, from a file and from a document alike, and accepts the int32 boundary values, which
  `ListStubs` then reports exactly.
- **`WatchCalls` shutdown precedence**, tested at the level of the send loop with injected channels
  and injected render and send functions, because no RPC-level arrangement can hold calls buffered
  while `Stopping` closes without depending on timing:
  - Calls buffered and `Stopping` already closed: no `Send` happens and the loop returns
    `UNAVAILABLE`. The case repeats 100 times, because an implementation relying on the select
    alone passes each repetition with probability ½.
  - `Stopping` closed from inside the render step of a received call: that call is not sent. This
    fails a check placed only at the top of the loop, or before rendering.

**Tests that must discriminate**, each written so a plausible wrong implementation fails it:

- `ReplaceAllStubs` with a bad third document leaves the previous API stubs in place, asserted by
  ID rather than by count.
- `CreateStub` with `times: 2147483648` is `INVALID_ARGUMENT` and leaves the store unchanged.
- `ListCalls` ordering and `limit`, with at least three calls.
- `VerifyCalls` failing in both directions, with `actual` asserted per direction; a mistyped method
  with `never` is `NOT_FOUND`, not a pass.
- Size caps: a `RegisterSchemas` request padded past 4 MiB with an unknown field is admitted and
  decoded, and the handler then answers on its merits — asserted as neither `RESOURCE_EXHAUSTED` nor
  `Unknown`, not as success: the padding leaves `descriptor_set` empty, which §4.2 makes
  `INVALID_ARGUMENT`. One past 32 MiB is rejected; a table over the other four services' procedures
  rejects 8 MiB for each.
- `WatchCalls` eviction delivers the buffered calls, then `RESOURCE_EXHAUSTED`.

**Integration tests** through `server.Start`:

- **The boot contract, in process** (M3 §12), with a native grpc-go client over h2c: start with no
  schema source → `RegisterSchemas` → `CreateStub` → a data-plane call → `ListCalls` shows the call
  with `matched_stub_id` → `VerifyCalls` passes, and fails with a nearest miss for a matcher the
  call does not satisfy.
- **Invalid UTF-8 from ingress to response.** A client writing raw HTTP/2 frames — as the §12 probe
  did — makes one data-plane call whose non-`-bin` metadata value holds invalid UTF-8 and another
  whose `:path` does. `ListCalls` and an open `WatchCalls` stream both succeed and render U+FFFD in
  place of the bad bytes, and a failing `VerifyCalls` against the metadata call renders it in
  `actual`. Removing `validUTF8` from either field must make the test fail.
- **A descriptor path that is not UTF-8.** `RegisterSchemas` with a set whose file — holding a
  service — has such a path succeeds, reports the path with U+FFFD, and `ListServices` still
  succeeds afterwards.
- **`WatchCalls` at teardown**, over h2c and over HTTP/1.1: with a stream open, `GracefulStop`
  returns well inside `shutdownGrace` and the stream ends `UNAVAILABLE`. Removing the `Stopping`
  case must make the test fail (about 5s against a sub-second bound); this is mutation-checked
  before the task is accepted.

**Stream readiness.** `Journal.Watch` runs before the header-flush `Send(nil)` (§6), so a
`WatchCalls` call that has returned proves the subscription is already registered: connect's
server-streaming client call cannot return before that first `Send` flushes headers. RPC-level
stream tests still record probe calls until the first one arrives before making their assertions,
but that is belt-and-braces now, not the necessity it once was. No sleeps.

**Gate:** `go build ./... && go vet ./... && go test ./... -race -count=1`. `-count=1` is not
optional: the test cache has hidden a real data race in this repository before.

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| A handler forgets an `invalidArgument` mark | The failure is `INTERNAL`, never a misattributed client error; every table row is asserted through a real RPC (§9) |
| Wrapping `LookupMethod` errors changes the text that loader and `check` output print | A test pins the text (§9) |
| A `Send` that passed its check just before teardown blocks on a peer that stopped reading | At most one such `Send`; 4a's drain bound and force-close end it (§6) |
| One invalid-UTF-8 string fails serialization of a whole response | `validUTF8` on every response string (§5.2), with an ingress-to-response regression test (§9) |
| A response string is added later without `validUTF8` | The rule is blanket, so review checks for the helper rather than tracing sources; the ingress test covers the fields reachable today |
| The int32 bound stops an existing stub file from loading | A `priority` or `times` beyond int32 has no meaning as a priority or a use budget; the diagnostic names the field and the value (§3.3) |
| `WatchCalls` tests race the subscription | Record probe calls until the first delivery; the precedence tests run at the loop level with injected channels (§9) |
| `ListCalls` or `ExportStubs` responses exceed the 4 MiB *receive* limit grpc-go and grpc-java clients default to | Nothing server-side; the SDKs raise their client limit — carried as a note into Phases 7 and 9 |
| The 32 MiB outer cap raises the worst-case allocation on the h2c upgrade path | Accepted (§8): loopback by default, a dev tool, still bounded |
| Changing `journal.Report`'s shape breaks a consumer | `internal/journal`'s own tests and `conformance/harness_test.go` consume it (verified by search); a shape change that only degrades the latter's formatting still compiles, so it must be checked by reading the diagnostic, not just by `go build` |

---

## 11. Decisions flagged for review

1. **`WatchCalls` ends at teardown with `UNAVAILABLE`**, via `Deps.Stopping` — human decision. It
   amends 4a §2's expectation that 4b would not touch `server`; the change is one field.
2. **Size caps: 32 MiB for `SchemaService`, 4 MiB elsewhere**, with the mux-wide and outer caps at
   32 MiB — human decision.
3. **An error mapper with input marks, where unmarked is `INTERNAL`** — human decision, chosen over
   codes at each call site and over typing every core diagnostic.
4. **An unknown method is `NOT_FOUND` everywhere**, including inside a stub document — following
   M3 §11 over Phase 3 §6's wholesale `INVALID_ARGUMENT` for compile diagnostics.
5. **`VerifyCalls` resolves the method even with an empty matcher**, trading the ability to count
   calls to an unregistered method for protection against typos.
6. **An absent `times` is rejected**, not defaulted.
7. **Rendering degrades** to an empty `json` instead of failing the RPC.
8. **`json` uses proto field names**, matching matcher paths.
9. **Teardown takes precedence over buffered watch calls** — from review. `Stopping` is checked
   before every `Send`, buffered calls are dropped, and at most one `Send` already past its check
   can outlive the start of teardown.
10. **`journal.Report` carries `*Call` snapshots** instead of sequence numbers — a core API change,
    taken because a second read cannot see the snapshot the verdict came from.
11. **Stub `priority` and `times` are bounded to int32 in the shared compiler** — from review. This
    tightens the file grammar rather than giving the API a validator files do not have.
12. **Invalid UTF-8 in response strings is replaced with U+FFFD**, for presentation only — from
    review, widened from metadata to every response string after probing found `:path` and
    descriptor paths reachable too. Not omitted, not moved to `binary_values`, not escaped.

---

## 12. Facts for the plan

Verified while writing and reviewing this design, the runtime claims by a throwaway probe driven
through the real ingress path and since deleted:

- connect v1.20.0 maps `http.MaxBytesError` to `RESOURCE_EXHAUSTED`, and context errors to
  `CANCELED` / `DEADLINE_EXCEEDED`, before a handler's error reaches the wire.
- `s.stopping` exists before `startAdminPlane` builds `Deps`.
- Only `internal/journal`'s tests consume `Report`; no test matches `LookupMethod`'s message text.
- Message matcher paths resolve by proto name; the data plane leaves `Call.Err` nil on success and
  joins nearest-miss reasons with `"; "`; explanations quote recorded strings with `%q`.
- On this 64-bit host, `times: 2147483648` and `priority: 2147483648` parse and compile, and
  converting either to `int32` yields `-2147483648`.
- grpc-go v1.82.0, sent raw HTTP/2 frames, accepts a non-`-bin` metadata value (`ok\xffbad`) and a
  `:path` (`/shop.v1.OrderService/Get\xffOrder`) containing invalid UTF-8; the journal records both
  unchanged. Its `decodeMetadataHeader` returns non-`-bin` values as they arrived.
- `proto.Marshal` of a `MetadataEntry` holding such a value fails ("string field contains invalid
  UTF-8"), and so does `protojson.Marshal`.
- `Registry.RegisterSet` accepts a `FileDescriptorSet` whose file path is invalid UTF-8 and returns
  that path as added.
- yaml.v3 rejects invalid UTF-8 input, so `stub.ParseDocument` does too.
- protojson returns an error for an `Any` whose type URL does not resolve.

To execute before the plan quotes code:

- `RenderSequence` of zero documents loads back through the file loader as zero stubs.
- A connect-go client over HTTP/1.1 and a grpc-go client over h2c both observe `UNAVAILABLE` when
  `WatchCalls` returns it mid-stream.
- A per-route `http.MaxBytesHandler` nested inside a larger mux-wide one enforces the smaller limit
  on both the HTTP/1.1 path and the prior-knowledge h2 path.
- A `RegisterSchemasRequest` padded with an unknown field decodes and registers normally (the §9
  size-cap test depends on it).
