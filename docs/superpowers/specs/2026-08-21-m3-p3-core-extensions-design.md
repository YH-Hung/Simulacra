# Simulacra M3 Phase 3 — Core Extensions: Design

**Goal (M3 design §13.3):** make the three core packages support everything the frozen
`simulacra.admin.v1` contract describes — a runtime-mutable schema registry, a stub store with
origins, IDs, and hit counts, and a journal that can be tailed — so Phase 4's handlers are pure
proto↔core translation with no new core logic.

**Reference:** `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §6 (core changes), §12
(testing), §13 (phasing); `docs/superpowers/specs/2026-08-07-m3-p2-admin-api-design.md` §1
(explicitly defers these items to Phase 3); `PROPOSAL.md` §5, §8.

Phase 1 (`server/` facade) is complete at `65c1380`. Phase 2 (`api/`, `gen/`, buf toolchain, CI
gates) is complete at `8e9b8a9` and **froze the admin contract** — this phase implements against
that contract and must not require changing it.

---

## 1. Scope

### In scope

1. **`internal/schema`** — copy-on-write registry: atomic snapshot swap, writer mutex,
   all-or-nothing registration, `RegisterSet`, and a read surface split between live and pinned
   consumers.
2. **`internal/stub`** — stub origins, IDs, normalized documents, hit counts; the per-stub decode
   entry point (`ParseDocument`); export rendering; the store operations `CreateStub`,
   `DeleteStub`, `ReplaceAllStubs`, `ListStubs`, and `ControlService.Reset` need.
3. **`internal/journal`** — `Watch` subscriptions with slow-consumer eviction, `Call.StubID`, and
   `Len`/`Cap` accessors.
4. **Verify surface** — a strict match-block decode plus a `CompileMatch` entry point, so
   `VerifyCalls.matcher_document` compiles without duplicating `Compiler.Compile`.
5. **Wiring** — the four consumer edits in `internal/dataplane`, `internal/stub`, and `server/`
   that the above require.

### Out of scope (later phases)

- Handlers, the h2c server, `/healthz`, `serve --admin` — **Phase 4**.
- The schema-less startup relaxation (`server.Options` may omit schema sources when the admin
  plane is enabled) — **Phase 4**: it is gated on `--admin` existing, and relaxing it earlier
  would let a server start that can never serve anything.
- Accessors on `server.Server` exposing the registry/store/journal — Phase 4 wires
  `internal/admin` from inside `server.Start`, so no public accessor is needed.
- CLI commands — Phase 5. Reflection import — Phase 6. SDKs — Phases 7/9. Dockerfile — Phase 8.
- `VerifySequence`, server-side upstream import, admin auth, TLS — M3 non-goals (M3 design §2).

### Behavior-preservation claim

This phase is behavior-preserving in practice. The one semantic change — API-origin stubs beating
file-origin stubs at equal priority — is unobservable until Phase 4 creates the first API stub.
Every existing test must pass with mechanical-only edits in the files that name a removed or
reshaped constructor — `stub.Store.Replace`, `stub.NewStore(stubs)`, or `schema.Registry.Files`:

| File | Reference |
|---|---|
| `internal/stub/store_test.go` | `store.Replace` (2 call sites), `NewStore(stubs)` |
| `internal/dataplane/server_test.go` | `store.Replace` (line 508), `stub.NewStore(stubs)` (4 call sites) |
| `internal/stub/template_test.go` | `match.NewCompiler(reg.Files())` (6 call sites) |
| `internal/match/match_test.go` | `reg.Files()` (lines 296, 352, 612) |
| `conformance/harness_test.go` | `match.NewCompiler(h.reg.Files())` (line 239), `stub.NewStore(compiled)` (line 106) |
| `server/watcher_test.go` | `stub.NewStore(initial)` (4 call sites) |

The `Files()` sites are a one-word rename to `Snapshot()`; the `NewStore(stubs)` sites become
`NewStore()` + `ReplaceOrigin` (§3.4), best wrapped once in a small test helper per package.

---

## 2. `internal/schema` — copy-on-write registry

### 2.1 The problem

M3 design §6 states it precisely: the raw `*protoregistry.Files` escapes the registry today.
`Registry.Files()` is handed to grpc reflection (`internal/dataplane/server.go:58`) and to the
match compiler's CEL environment (`internal/stub/stub.go:109`), and `Types()` binds it at
construction via `dynamicpb.NewTypes`. `protoregistry.Files` is not safe for mutation concurrent
with lookups, so any held reference would race with a runtime `RegisterSchemas`.

`dynamicpb`'s own documentation confirms the constraint verbatim: *"The Files registry is
retained, and changes to Files will be reflected in Types. It is not safe to concurrently change
the Files while calling Types methods."*

### 2.2 Structure

```go
type Registry struct {
	mu   sync.Mutex               // serializes load→build→swap; readers never take it
	snap atomic.Pointer[snapshot]
}

type snapshot struct {
	files *protoregistry.Files    // immutable once published
	types *dynamicpb.Types        // built from files, published atomically with it
}
```

The atomic pointer protects *readers*. It does not stop two concurrent registrations from both
loading S0, building S0+A and S0+B, and silently losing one on the second store — so a **writer
mutex serializes the whole load→build→swap sequence**. Registration is rare and cheap; readers
never touch the mutex.

### 2.3 One mutation path

Every mutator funnels through one unexported helper:

```go
func (r *Registry) apply(fn func(candidate *protoregistry.Files) error) error
```

It takes `mu`, builds a candidate by re-registering every file of the current snapshot into a
fresh `Files`, runs `fn` against the candidate, and publishes
`&snapshot{files: candidate, types: dynamicpb.NewTypes(candidate)}` only if `fn` returns nil.

Three consequences, all wanted:

- **All-or-nothing is free.** A failed registration discards the candidate; the served snapshot is
  byte-for-byte untouched. This is the rollback property the contract promises for
  `RegisterSchemas`, and it now also covers `AddProtoDir` and `AddDescriptorSetFile`, which today
  can leave a half-loaded registry behind.
- **One swap per operation.** `AddProtoDir` currently calls `AddFile` per compiled file; under
  copy-on-write that would be one swap per file. Funnelling through `apply` makes each public
  mutator exactly one swap.
- **Descriptor identity is stable.** Pre-existing `protoreflect.FileDescriptor` values are reused
  verbatim in each candidate — a `Files` is an index, not an owner — so descriptors that were
  already registered keep pointer identity across a registration. Only genuinely new files are new
  objects.

### 2.4 Public surface

| Method | Change |
|---|---|
| `NewRegistry()` | publishes an empty snapshot |
| `AddProtoDir(ctx, root)` | one swap; now all-or-nothing |
| `AddDescriptorSetFile(path)` | one swap; now all-or-nothing |
| `AddFile(fd)` | one swap (still used by `dataplane.New` for the health proto) |
| `RegisterSet(*descriptorpb.FileDescriptorSet) (added []string, err error)` | **new** — backs `SchemaService.RegisterSchemas` |
| `Snapshot() *protoregistry.Files` | **replaces `Files()`** — returns the current immutable snapshot |
| `FindFileByPath`, `FindDescriptorByName` | **new** — `Registry` implements `protodesc.Resolver`, delegating to the current snapshot |
| `LookupMethod`, `LookupMessage`, `Types`, `Services` | signatures unchanged; snapshot-backed |

`RegisterSet` returns the paths it newly added, which is exactly
`RegisterSchemasResponse.registered_files`; an empty slice means every file was already present.

### 2.5 Read surface: live vs. pinned

M3 design §6 says "`Registry.Files()` is removed" and the registry implements the resolver
interfaces. That is right for reflection and wrong for the match compiler, because
`dynamicpb.NewTypes` takes the **concrete** `*protoregistry.Files`, not an interface, and
`newCELProvider` needs `RangeFiles` — neither is expressible through `protodesc.Resolver`.

**Deviation from M3 design §6, recorded deliberately:** `Files()` is replaced by
`Snapshot() *protoregistry.Files`. The hazard §6 identified was mutation in place; copy-on-write
removes the registry itself as a mutator, since every registration builds a fresh `Files` and the
published one is never touched again. What copy-on-write cannot remove is the returned type's own
`RegisterFile` method — a caller *could* still mutate a snapshot. This is therefore a
**trusted-caller convention, not structural immutability**: the doc comment on `Snapshot` states
"read-only — registering into a snapshot is a data race with every other holder", and the type
cannot be narrowed to enforce it because the two consumers that need `Snapshot` at all need the
concrete type (`dynamicpb.NewTypes` and `RangeFiles`). The convention is in-tree only; snapshots
are never handed across an API boundary. Consumers split by what they actually need:

| Consumer | Gets | Why |
|---|---|---|
| grpc reflection (`dataplane`) | `reg` itself, as `protodesc.Resolver` | **live** — a service registered at runtime is reflectable immediately, which the JVM/Go SDK boot path depends on |
| `match.NewCompiler` (via `stub.NewCompiler`) | `reg.Snapshot()` | needs `RangeFiles` and `dynamicpb.NewTypes`; pinning is correct because compilers are constructed per load operation |
| `schema.Types` (response templates, typed error details) | live delegation to the current snapshot | lets a stub render an `Any` whose type was registered after the stub was created |

`reflection.ServerOptions.DescriptorResolver` is typed `protodesc.Resolver`, so passing `reg`
requires no adapter.

`schema.Types` changes from holding a `*dynamicpb.Types` to holding the `*Registry` and reading
`r.snap.Load().types` per call. Its registry-first / global-fallback behavior is unchanged.

### 2.6 `RegisterSet` semantics

- The set must be **self-contained**: every import must be present in the set, matching
  `AddDescriptorSetFile` today. `protodesc.NewFiles` enforces it; the existing error message
  ("descriptor sets must be self-contained; build with `buf build -o` or `protoc
  --include_imports`") is reused. Both SDKs walk the full dependency graph, so this is not a
  practical limitation; supporting incremental sets that lean on already-registered imports is a
  non-breaking addition later.
- **Idempotent.** A path already present in the registry is compared against the incoming file; if
  they are equivalent the file is skipped and omitted from `added`. If they differ, the whole
  registration fails with an error naming the file — surfaced as `INVALID_ARGUMENT` in Phase 4.
- Comparison is between `protodesc.ToFileDescriptorProto(existing)` and the incoming
  `FileDescriptorProto`, with `SourceCodeInfo` cleared on both sides and buf's image extension
  (field 8042) removed from the **top-level** unknown-field buffer only, compared by `proto.Equal`.

  **The normalization must stay surgical.** The first implementation round-tripped the whole
  message through `proto.UnmarshalOptions{DiscardUnknown: true}`, which is recursive: it also
  discarded unknown fields *nested* inside option messages, where custom options live whenever
  their extension is not linked into the binary. Two same-path files differing only in a custom
  option then compared equal and were silently accepted — precisely the conflict this contract
  exists to reject. Field 8042 is stripped by walking the raw unknown-field buffer and dropping
  that one field number; every other unknown byte survives. A regression test registers two files
  differing only in a custom option and requires the conflict.

**Named risk — the idempotency comparison is this phase's most likely surprise.** A descriptor set
produced by `buf build` carries its own copies of the well-known types. Those will meet a registry
that got its well-known types from protocompile's standard imports (`AddProtoDir`) or from the
Go-generated `healthpb.File_grpc_health_v1_health_proto` (`dataplane.New`). If those copies do not
compare equal, **every JVM SDK `RegisterSchemas` call fails** on `google/protobuf/*.proto` before
reaching the user's own types. §7 gives this a dedicated test, and the plan must verify the
comparison empirically (a real `buf build` set against a protocompile-built registry) before any
task is written against it.

If the copies prove representationally unequal, the fix is to **normalize the comparison** —
inspect the actual `FileDescriptorProto` diff and clear the specific fields that differ without
changing meaning (source info is already cleared; plausible candidates are toolchain-dependent
defaults of the same kind). What is *not* an option is skipping a non-equivalent file: the frozen
contract requires same-path-with-different-content to fail with `INVALID_ARGUMENT` naming the
file, and a silent skip would also let the rest of the set register against a dependency the
registry resolves differently than the set's author intended. Equivalence may loosen; the
conflict error may not.

---

## 3. `internal/stub` — origins, IDs, one document grammar

### 3.1 `Compiled`

```go
type Origin uint8
const (
	OriginFile Origin = iota   // owned by the hot-reload watcher
	OriginAPI                  // owned by the client that created it
)
func (o Origin) String() string   // "file" | "api"
```

`Compiled` gains three exported fields — `ID string`, `Origin Origin`, `Document string`. It does
**not** retain the decoded `Stub`: `Document` is rendered once at compile time and nothing
downstream needs the struct again (export composes normalized documents, §3.5). `Source` keeps its
current meaning:
`"<path-as-given>#<index>"` for file stubs, `"api"` for API stubs.

`Document` is the **normalized single-stub mapping** that `Stub.document` carries over the wire. It
is produced by re-rendering the input's own YAML node tree — comments and styling stripped, and
**aliases expanded** — not by echoing the input bytes and not by re-marshaling the struct.

Alias expansion is what makes a per-stub document stand alone. YAML lets one list item declare an
anchor (`&oid`) and a sibling item alias it (`*oid`); the whole-file decode resolves that happily,
but an item rendered in isolation would emit a bare `*oid` whose anchor lives in another document
— unparseable on its own and broken in an export. Expansion is depth-bounded, so a self-
referential anchor errors instead of expanding forever.
Because decoding is strict (`KnownFields(true)`), re-marshaling can lose nothing but comments and
key order — every field the grammar accepts is modeled on the struct, and every field it does not
accept was already rejected. That property is what makes one normalized form safe for both
origins, and it is asserted by a round-trip test (§7).

### 3.2 The shared decode boundary

M3 design §5 requires files and the API to share one decoder. Both routes decode the same `Stub`
struct through yaml.v3 with `KnownFields(true)`; they differ only in the outer shape (files hold a
sequence, possibly across `---` documents; the API carries exactly one mapping).

```go
func ParseDocument(data []byte) (Stub, error)          // exactly one stub mapping; YAML or JSON
func ParseMatchDocument(data []byte) (*match.Block, error) // VerifyCalls.matcher_document
```

Both are implemented as **two passes over the same bytes**:

1. Decode into a `yaml.Node` to classify: a sequence gets an error pointing at the file grammar
   ("the API takes one stub mapping; files take a list"), and a second `---` document gets
   "exactly one stub".
2. Decode into the target struct with a fresh `yaml.Decoder` and `KnownFields(true)`.

Two passes rather than classifying and then calling `node.Decode`, because `yaml.Node.Decode` has
no `KnownFields` control — decoding from the node would silently accept unknown fields and destroy
the strictness that is the whole point of the shared boundary. JSON needs no separate path: JSON is
valid YAML, and both forms are tested.

**Empty input diverges between the two.** The frozen contract says an empty
`VerifyCalls.matcher_document` matches any call to the method — and Phase 4 calls
`ParseMatchDocument` unconditionally — so `ParseMatchDocument` returns `(nil, nil)` for empty or
whitespace-only input rather than surfacing the decoder's EOF. A nil `*match.Block` is already the
"matches everything" value throughout the codebase (`match.Compiler.Compile` returns an
always-true `Compiled` for a nil block, and `journal.Verify` treats a nil matcher as match-all).
`ParseDocument` keeps the opposite rule: an empty `CreateStub.document` is `INVALID_ARGUMENT` —
there is no empty stub. Both behaviors are tested (§7).

`parseFile` keeps its current structure; the grammar is shared by construction (same struct, same
strictness), not by refactoring the file loader onto the single-document path.

### 3.3 Compiler additions

```go
func (c *Compiler) CompileMatch(method string, b *match.Block) (*match.Compiled, error)
```

The matcher half of `Compile`, exposed: resolve the method, take `match.ShapeOf`, compile the
block against the input descriptor. `VerifyCalls` becomes `ParseMatchDocument` → `CompileMatch` →
`journal.Verify`, with no logic duplicated from `Compile`.

`NewCompiler` now builds its match compiler from `reg.Snapshot()` (§2.5).

### 3.4 `Store`

```go
type Info struct {
	ID, Method string
	Shape      match.Shape
	Priority   int
	Times      int
	Origin     Origin
	Source     string
	Hits       int
	Document   string
}

type ListFilter struct {
	Method string   // "" means all
	Origin *Origin  // nil means all origins
}

func NewStore() *Store                            // empty; population is Add / ReplaceOrigin only

func (s *Store) Add(c *Compiled) string           // API-origin ingest
func (s *Store) Remove(id string) error
func (s *Store) ReplaceOrigin(origin Origin, stubs []*Compiled) ([]string, error)
func (s *Store) List(f ListFilter) []Info
func (s *Store) ResetStubs()
```

`Info` is exactly the `Stub` envelope the contract froze. `Replace` is **removed**; `Len` and
`CountFor` are unchanged.

**`NewStore` no longer takes stubs.** Today `server.Start` passes `LoadDirs` output straight to
`NewStore(stubs)`, whose signature cannot report an error — so the duplicate-ID check would be
enforced on every path *except* initial startup, and passing the same `--stubs` root twice would
boot a store holding duplicate IDs that only the first reload would reject. Making the constructor
empty leaves exactly two ingest paths, both validated: `server.Start` becomes
`store := stub.NewStore()` followed by `ReplaceOrigin(OriginFile, stubs)` with the error
propagated as a startup failure, and a test drives the duplicate-root case through `server.Start`
itself, not just the store unit.

**The store stamps ownership; callers cannot get it wrong.** `Origin`'s zero value is
`OriginFile`, and the compiler does not set it — so if ownership were a field callers must
remember to fill, the natural Phase 4 path (`ParseDocument` → `Compile` → `Add`) would silently
create a *file-owned* stub. Instead both ingest paths stamp it: `Add` is the API-origin ingest and
sets `Origin = OriginAPI`, `Source = "api"`, and a store-assigned ID on the stub it stores;
`ReplaceOrigin` stamps its `origin` argument onto every stub it installs. `Compiled.ID` and
`Compiled.Origin` are store-managed fields, documented as such.

**One ID rule, enforced where IDs enter.** `"api-<n>"` is a **reserved namespace only the store
mints**. `Add` always assigns from it. `ReplaceOrigin` assigns from it only for `OriginAPI` (the
`ReplaceAllStubs` path — fresh documents carry no IDs); a file-origin stub arriving without an ID
is a loader bug and is rejected, since minting an `api-` ID for it would put a file stub in the
API namespace. A caller-supplied ID must be unique across the whole store *and* outside the
reserved namespace — otherwise a later `Add` would mint the same ID and make `Remove` ambiguous.
The reserved check matches the exact generated form (`api-` followed by digits only), so a file
literally named `api-1` is unaffected: its ID is `api-1#0`.

File stubs arrive with `ID = Source` (stamped by `LoadDirs`), which is why two `--stubs` roots
containing the same relative path yield distinct IDs — the walk path includes the root — and why
the same root passed twice is caught as a duplicate.

**`ReplaceOrigin` validates completely before it mutates anything.** IDs, origins, and the ID
counter are computed into temporary state; only once the whole batch is known good does it stamp
the input stubs and swap the index. Stamping while walking the batch — the first implementation —
meant a duplicate late in the slice had already renamed its predecessors, consumed counter values,
and (if an input pointer was live under another origin) re-stamped a stored stub's ownership,
all while returning an error that promised the store was untouched.

**Ownership is enforced in the store, not in the handler.** `Remove` refuses a file-origin stub
with a typed error carrying the owning file, which Phase 4 maps to `FAILED_PRECONDITION` ("owned
by `<file>`; edit or remove the file"). Keeping the invariant next to the data means the CLI, the
SDKs, and any future surface cannot each re-derive it differently.

**`ResetStubs`** does both halves of `ControlService.Reset(stubs)` under a single lock: drop every
API-origin stub and zero the `used` counters of file-origin stubs. Composing it from two exported
calls would leave a window where the store is neither the old state nor the new one.
`ReplaceAllStubs([])` is a *different* operation — `ReplaceOrigin(OriginAPI, nil)` — which clears
API stubs without touching file budgets.

**Selection order** sorts on `(priority desc, API-before-file, load order)`, stably. Rationale from
M3 design §6: a test overrides a sandbox default without priority arithmetic.

**`ReplaceOrigin` touches only the named origin.** Entries of every other origin keep their
objects — and therefore their `used` counters — untouched. This is a correctness requirement, not
an optimization: a filesystem edit triggering a file reload must not replenish the `times` budgets
of API stubs a running test depends on, and symmetrically `ReplaceAllStubs` must not rewind file
budgets. A cross-origin preservation test covers both directions (§7).

**Hits and budget stay one counter** (the existing per-entry `used`), as M3 design §6 specifies.
The consequence is explicit and must be documented in the stub-model docs rather than discovered:
a stub's `hits` resets when *its own origin* is replaced (a hot reload for file stubs, a
`ReplaceAllStubs` for API stubs) and when `Reset` rewinds it. It measures consumption of the
currently-loaded stub, not lifetime traffic. The CLI and the M4 dashboard must not label it as a
lifetime total.

### 3.5 Export rendering

```go
func RenderSequence(docs []string) (string, error)
```

Lives in the grammar package, beside the parser it must round-trip with. It parses each normalized
mapping into a `yaml.Node`, assembles them into one sequence node, and marshals — producing
`ExportStubsResponse.document`, a file-grammar YAML sequence the CLI can write straight to disk.
Assembling nodes rather than concatenating indented text is what keeps it correct for block
scalars and nested structures. The guarantee that matters is the round trip: export →
`parseFile` → the same stubs (§7).

---

## 4. `internal/journal` — watch, StubID, counts

```go
const watchBuffer = 64
var ErrSlowConsumer = errors.New("journal: subscriber fell behind and was dropped")

func (j *Journal) Watch(ctx context.Context, method string) *Subscription

func (s *Subscription) Calls() <-chan *Call   // closed on drop, Close, or ctx done
func (s *Subscription) Close()
func (s *Subscription) Err() error            // safe any time; nil while live
```

**Why a `Subscription` and not the bare `Watch(ctx) (<-chan *Call, func())` of M3 design §6.** A
bare channel cannot tell Phase 4's handler *why* the stream ended, and the two reasons need
different gRPC outcomes: a clean unsubscribe or client cancel ends the stream with `OK`, while a
slow-consumer eviction must be `RESOURCE_EXHAUSTED` ("client too slow — reconnect"). Inferring the
reason from context state races and mislabels a clean `Close`. `Err()` makes the termination
reason a value: nil for cancel/`Close`, `ErrSlowConsumer` for eviction. Phase 4 maps it in one
place, and the core behavior is testable without a server.

**Ordering.** `Record` broadcasts while holding the write lock, so every subscriber observes calls
in `Seq` order and never observes a call the ring does not already hold.

**Filtering happens before the send**, inside the journal, so a tail on one method is not evicted
by unrelated traffic on another.

**Eviction, and the locking discipline that makes it safe.** Sends are non-blocking; a full buffer
sets `ErrSlowConsumer`, closes the channel, and unregisters the subscriber. The subtle hazard is
not double-close but **send-versus-close**: a `sync.Once` around `close` cannot stop `Record` from
sending into a channel that a concurrent consumer `Close` (or the ctx goroutine) is closing — a
send on a closed channel panics. So every channel state transition shares `Journal.mu`: `Record`
already broadcasts under the write lock, and `Close` takes the same lock to unregister and close.
Since eviction happens *inside* `Record` — which already holds `mu` — the close-and-unregister step
is a locked-state helper both paths call, one from under the lock and one after taking it, rather
than a public method calling itself recursively. A `sync.Once` still wraps the user-facing `Close`
for idempotence, but the mutex is what carries the guarantee.

**`Err` reads under the lock.** An earlier revision documented it as "read only after
`Calls` closes", relying on the close to provide the happens-before edge for the unsynchronized
read of `err`. That is technically sound and practically untenable: the contract is invisible to
the race detector, and the first test written against it violated it by one line (receiving a
*value* from `Calls` and then reading `Err`, with no close observed). `Err` therefore takes the
journal's read lock — the same lock `closeLocked` writes under — and returns nil while the
subscription is live. It is a once-per-stream teardown call; the mutex costs nothing next to a
caller-discipline contract Phase 4's handler would have to honor perfectly.

**The context bridge must itself be closeable.** One small goroutine per subscription maps
`ctx.Done()` onto `Close` — but a goroutine waiting on `ctx.Done()` alone outlives an explicit
`Close` for as long as the context lives, which for a `context.Background()` caller (the natural
choice for a CLI tail) is forever: a permanently leaked goroutine pinning the subscription. So the
subscription carries an internal `done` channel that every close path closes — explicit `Close`,
writer-side eviction, and the bridge itself — and the bridge selects on `ctx.Done()` *and* `done`.
§7 races both hazard pairs — `Close` against writer-side eviction, ctx cancellation against a
normal broadcast — and additionally asserts the bridge goroutine exits after an explicit `Close`
under a still-live context.

**Aliasing.** Subscribers receive the same retained `*Call` the ring holds. `Record` already clones
once for retention and nothing mutates a `Call` afterwards (`Reset` nils slots; it does not touch
call objects), so per-subscriber cloning would be pure cost. The contract is documented on `Watch`:
the delivered call is read-only.

Also: `Call.StubID` alongside the existing `StubSource` (feeding `Call.matched_stub_id`), and
`Len() int` / `Cap() int` for `GetServerInfoResponse.journal_count` / `journal_capacity`.

---

## 5. Wiring

The only edits outside the three core packages:

| File | Change |
|---|---|
| `internal/dataplane/server.go:58` | `DescriptorResolver: reg.Files()` → `DescriptorResolver: reg` |
| `internal/dataplane/server.go` (4 sites) | set `call.StubID = selected.ID` beside the existing `call.StubSource = selected.Source` |
| `internal/stub/stub.go:109` | `match.NewCompiler(reg.Files())` → `match.NewCompiler(reg.Snapshot())` |
| `internal/stub/loader.go` | `LoadDirs` stamps `ID = Source` (origin is stamped by the store on ingest, §3.4) |
| `server/server.go:102` | `stub.NewStore(stubs)` → `stub.NewStore()` + `ReplaceOrigin(stub.OriginFile, stubs)`, the error propagated as a startup failure |
| `server/watcher.go:209` | `store.Replace(stubs)` → `store.ReplaceOrigin(stub.OriginFile, stubs)`, reporting a uniqueness error through the existing stub-error path and leaving the store untouched |

`match.NewCompiler` keeps its `*protoregistry.Files` parameter. No signature change is needed once
the caller passes a snapshot, and inventing an interface there would buy nothing `dynamicpb` can
use.

---

## 6. Error handling

Core packages return typed Go errors; Phase 4 maps them to codes. The mapping the contract implies:

| Core error | Phase 4 code |
|---|---|
| `ParseDocument` / `ParseMatchDocument` / `Compile` diagnostics | `INVALID_ARGUMENT`, message passed through verbatim with source positions |
| `RegisterSet` conflict or non-self-contained set | `INVALID_ARGUMENT`, naming the file |
| `Store.Remove` unknown ID | `NOT_FOUND` |
| `Store.Remove` file-origin (typed, carries the owning file) | `FAILED_PRECONDITION` |
| `Subscription.Err() == ErrSlowConsumer` | `RESOURCE_EXHAUSTED` |

Loader and CEL diagnostics are already good; the requirement here is only that the new entry
points do not wrap them into something less precise.

---

## 7. Testing

M3 design §12's core row, made specific, plus the tests this design's decisions require.

**`internal/schema`**

- `-race` suite: reflection lookups, CEL compile + eval, and dynamic `Any`/type resolution running
  concurrently with `RegisterSet` — all three references that escaped before §2.5.
- All-or-nothing rollback: a conflicting set fails and the served registry is unchanged, asserted
  by comparing the full file list and a resolved descriptor before and after.
- Concurrent disjoint registrations both land and the final registry holds their union — a
  functional lost-update check, which `-race` cannot make (the lost update is a benign-looking
  atomic store, not a data race).
- **WKT idempotency:** a descriptor set produced by `buf build`, carrying its own
  `google/protobuf/*.proto`, registered against a registry built by `AddProtoDir` — must succeed
  and report only the non-WKT files as added. This is the §2.6 risk, and it is exactly the JVM SDK
  path.
- Descriptor identity stability: a descriptor resolved before a registration is pointer-identical
  to the same descriptor resolved after it.

**`internal/stub`**

- Origin, ID, and tie-break units, including duplicate relative paths across two `--stubs` roots
  and the same root passed twice.
- Origin stamping: `Add` produces an `OriginAPI` stub with `Source = "api"` regardless of what the
  compiler left on the fields; `ReplaceOrigin` stamps its argument onto every installed stub.
- Cross-origin preservation: a `ReplaceOrigin(OriginFile, …)` leaves API entries and their `used`
  counters untouched, and `ReplaceOrigin(OriginAPI, …)` leaves file budgets untouched.
- `Remove` refusing file-origin with the owning file named; `Remove` of an unknown ID.
- `ResetStubs` clears API stubs and restores file budgets in one step.
- `List` filters by method and by origin.
- `ParseDocument` in YAML and in JSON; sequence rejected; multi-document rejected; unknown field
  rejected; empty input rejected.
- `ParseMatchDocument` on empty and whitespace-only input returns a nil block (the match-all
  value); on a non-empty block, strictness matches `ParseDocument`.
- Document round trip: `Compiled.Document` → `ParseDocument` → identical compiled fields.
- Export round trip: `RenderSequence` output → `parseFile` → the same stubs.

**`server`**

- Startup with the same `--stubs` root passed twice fails through `server.Start` with the
  duplicate-ID error — the path that bypassed validation when `NewStore` took stubs directly.

**`internal/journal`**

- Broadcast ordering and the method filter.
- Slow-consumer eviction sets `ErrSlowConsumer` and closes the channel.
- `-race` tests for both hazard pairs: consumer `Close` against writer-side eviction, and context
  cancellation against a concurrent broadcast (the send-versus-close race of §4).
- Context cancellation closes the stream with a nil `Err`.
- Explicit `Close` under a still-live context terminates the bridge goroutine (leak check via
  goroutine count or a done-signal, not a sleep).
- `Len` / `Cap`.

**`internal/dataplane`**

- `StubID` recorded on the journal entry for each of the four shapes.
- A service registered at runtime becomes reflectable — the live-resolver property of §2.5, which
  no existing test covers because nothing could register at runtime before.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| WKT descriptor copies compare unequal, blocking every SDK register call (§2.6) | Comparison verified empirically during plan writing on the real `buf build` path; if unequal, the comparison is normalized against the inspected diff — the conflict error itself is contract-frozen and never loosened |
| Rebuilding a candidate `Files` per registration is O(files) | Registration is rare and startup-dominated; readers never pay it. If it ever matters, the fix is an incremental candidate, which the `apply` boundary already localizes |
| `yaml.v3` `omitempty` on the `Stub` struct changes the normalized document in a way that breaks the round trip | The round-trip test is the gate; tags are added field by field only where the zero value is genuinely absent, and `respond` keeps no `omitempty` because `respond: {}` is meaningful |
| Removing `Store.Replace`, `Registry.Files`, and the stub-taking `NewStore` breaks in-tree callers | Four production call sites and six test files reference them (§1, §5); the compiler finds them all, and the test-side changes are a rename or a two-line helper |
| Journal broadcast under the write lock slows `Record` | Sends are non-blocking into buffered channels; the worst case per subscriber is a channel send and an eviction, both O(1) |

---

## 9. Decisions flagged for review

1. **`Snapshot()` instead of removing `Files()`** (§2.5) — a deliberate deviation from M3 design
   §6, forced by `dynamicpb.NewTypes` taking a concrete `*protoregistry.Files`. Copy-on-write
   removes the registry as a mutator; read-only use of the returned value is a documented in-tree
   convention, not structural immutability.
2. **`Subscription` value instead of `(<-chan *Call, func())`** (§4) — more surface than M3 design
   §6 sketched, bought to make the `RESOURCE_EXHAUSTED` termination reason a value rather than an
   inference.
3. **`hits` collapsed onto the times budget** (§3.4) — as M3 design §6 specifies; the cost is that
   a stub's `hits` resets when its own origin is replaced and on `Reset`, which must be documented
   wherever it is displayed.
4. **Self-contained descriptor sets only** (§2.6) — incremental sets that lean on already-registered
   imports are rejected; adding them later is non-breaking.
5. **Ownership policy lives in the store** (§3.4) — `Add` and `ReplaceOrigin` stamp origins,
   `Remove` enforces them, and `NewStore` takes no stubs so no ingest path can skip validation.
   The Phase 4 handler inherits the rules rather than restating them.
