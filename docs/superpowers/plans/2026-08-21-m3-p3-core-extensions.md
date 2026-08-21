# Simulacra M3 Phase 3 — Core Extensions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the three core packages support everything the frozen `simulacra.admin.v1` contract needs — a copy-on-write schema registry with `RegisterSet`, a stub store with origins/IDs/documents/hit counts and store-stamped ownership, and a journal with `Watch` subscriptions — so Phase 4's handlers are pure proto↔core translation.

**Architecture:** The registry becomes an `atomic.Pointer` to an immutable `{files, types}` snapshot with a writer mutex serializing every load→build→swap (§2 of the design). The stub store owns identity: `Add`/`ReplaceOrigin` stamp origin, source, and IDs; `NewStore` is empty-only so no ingest path skips validation (§3). Stub documents are normalized by re-rendering the input's `yaml.Node` (comments and style stripped), never by re-marshaling the struct (§3.1). The journal broadcasts to per-subscriber buffered channels under its existing write lock, with a `Subscription` value carrying the termination reason (§4).

**Tech Stack:** Go 1.25, module `github.com/yinghanhung/simulacra`. No new dependencies: `gopkg.in/yaml.v3`, `google.golang.org/protobuf`, `github.com/bufbuild/protocompile`, `google.golang.org/grpc` are all already in `go.mod`.

**Reference:** Design `docs/superpowers/specs/2026-08-21-m3-p3-core-extensions-design.md` (cited as "design §N" throughout). M3 design `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §6, §12, §13.3. Phase 2 is complete at `8e9b8a9`; the admin contract under `api/` is **frozen** — nothing in this phase may change `api/` or `gen/`.

## Global Constraints

- **No changes to `api/` or `gen/`.** The contract is frozen; this phase implements against it.
- **No new root-module dependencies.** Everything needed is already in `go.mod`.
- **No admin handlers, no h2c, no `/healthz`, no `serve --admin`, no schema-less startup** — all Phase 4 (design §1).
- Behavior-preserving in practice: the API-beats-file tie-break is unobservable until Phase 4 creates the first API stub (design §1).
- Removed surface: `schema.Registry.Files()`, `stub.Store.Replace`, and the stub-taking `stub.NewStore(stubs)`. The compiler finds every caller; the design §1 table lists all six affected test files.
- Errors from new entry points pass loader/CEL diagnostics through verbatim — never wrapped into something less precise (design §6).
- Every task lands green: `go build ./... && go vet ./... && go test ./...` at every commit.
- Makefile recipe lines are tabs, not spaces (unchanged from Phase 2; this phase adds no Makefile targets).

## Pre-verified facts

Every claim below was executed against this repo before the plan was written.

1. **buf images carry an unknown field.** `bin/buf build <dir> -o set.binpb` produces a buf *image*: each `FileDescriptorProto` carries buf's per-file extension (field 8042 — `is_import` etc.), which protobuf-go keeps as unknown bytes when parsing a plain `FileDescriptorSet`. Comparing a buf-image WKT (`google/protobuf/timestamp.proto` et al.) against the same file compiled by protocompile: **prototext-identical, `proto.Equal`-unequal, and equal after `SourceCodeInfo` strip + `DiscardUnknown` round-trip** — verified for `timestamp.proto`, `duration.proto`, `any.proto`, and a user file. The §2.6 normalization is therefore: clear `SourceCodeInfo`, discard unknown fields. Nothing else differed.
2. **`reflection.ServerOptions.DescriptorResolver` is the `protodesc.Resolver` interface** (`FindFileByPath` + `FindDescriptorByName`), so the registry itself can be passed with no adapter.
3. **`dynamicpb.NewTypes` takes the concrete `*protoregistry.Files`** and documents "not safe to concurrently change the Files while calling Types methods" — the constraint behind `Snapshot()` (design §2.5).
4. **Struct re-marshaling cannot normalize documents.** yaml.v3 marshals a nil map as `{}` and `omitempty` drops empty maps — either direction breaks `StepSpec`'s "exactly one of message/status" on round trip (a status step would gain `message: {}`; a `message: {}` step would lose it). Documents are therefore rendered from the input's `yaml.Node`, not the struct. Verified: node re-render preserves `message: {}`, is a fixed point, and its output passes `KnownFields(true)` strict decode.
5. **Node marshal preserves comments and input style** — a node parsed from JSON re-marshals as JSON flow style, and comments survive. Clearing `HeadComment`/`LineComment`/`FootComment` and `Style` on every node yields clean block YAML, still a fixed point, with multiline strings rendered as `|-` literals. This is the `normalizeNode` in Task 4.
6. **yaml.v3 classification behavior:** decoding an empty or whitespace-only input into a `yaml.Node` returns `io.EOF`; a mapping decodes as `DocumentNode>MappingNode`; a sequence as `DocumentNode>SequenceNode`; a second `---` document is detected by a second `Decode` call returning nil instead of `io.EOF`. JSON decodes through the same path (JSON is YAML).
7. **`yaml.Node.Decode` has no `KnownFields` control** — strict decodes must run from bytes via `yaml.NewDecoder` + `KnownFields(true)` (this is why parsing is two passes over the same bytes, design §3.2).
8. **In-package registry tests never touch `reg.files` directly** — `internal/schema/registry_test.go` uses only public methods, so the rewrite breaks no in-package test at compile time.
9. **Affected callers of removed surface**, verified by grep: production — `internal/dataplane/server.go:58` (`reg.Files()`), `internal/stub/stub.go:109` (`reg.Files()`), `server/server.go:102` (`stub.NewStore(stubs)`), `server/watcher.go:209` (`store.Replace`); tests — `internal/stub/store_test.go` (2× `Replace`, `NewStore(stubs)`), `internal/dataplane/server_test.go` (line 508 `Replace`, 4× `NewStore`), `internal/stub/template_test.go` (6× `reg.Files()`), `internal/match/match_test.go` (`reg.Files()` at 296, 352, 612), `conformance/harness_test.go` (line 239 `reg.Files()`, line 106 `NewStore`), `server/watcher_test.go` (4× `NewStore`).
10. **`journal.Verify` treats a nil matcher as match-all** (`verify.go:164`) and **`match.Compiler.Compile` returns an always-true `Compiled` for a nil `Block`** — so `ParseMatchDocument` returning `(nil, nil)` for empty input needs no downstream changes (design §3.2).
11. The pinned toolchain from Phase 2 is installed (`bin/buf` et al.); `bin/buf build <proto-dir> -o <out>` works on an arbitrary directory input.

## File structure

```
simulacra/
├── internal/schema/
│   ├── registry.go                  # REWRITE Tasks 1–2: snapshot, apply, RegisterSet, resolver
│   ├── registry_test.go             # EXTEND  Tasks 1–3: rollback, identity, WKT, races, union
│   └── testdata/
│       ├── wktset/scratch.proto     # NEW Task 2: proto importing three WKTs
│       └── wkt_image.binpb          # NEW Task 2: committed buf image of wktset/
├── internal/stub/
│   ├── stub.go                      # EDIT Task 4 (Origin, Compiled fields, Snapshot rename)
│   │                                #      Task 5 (CompileMatch)
│   ├── document.go                  # NEW  Task 4: ParseDocument, RenderSequence, normalize
│   │                                #      Task 5: ParseMatchDocument
│   ├── document_test.go             # NEW  Tasks 4–5
│   ├── loader.go                    # EDIT Task 4: parseFile harvests documents; LoadDirs stamps
│   ├── loader_test.go               # EXTEND Task 4: document round trips
│   ├── store.go                     # REWRITE Task 6: NewStore/Add/Remove/ReplaceOrigin/List/ResetStubs
│   ├── store_test.go                # EXTEND Task 6
│   └── template_test.go             # EDIT Task 1: Files() → Snapshot()
├── internal/match/match_test.go     # EDIT Task 1: Files() → Snapshot()
├── internal/journal/
│   ├── journal.go                   # EDIT Task 7: StubID, Len/Cap, broadcast in Record
│   ├── watch.go                     # NEW  Task 7: Subscription, ErrSlowConsumer
│   └── watch_test.go                # NEW  Task 7
├── internal/dataplane/
│   ├── server.go                    # EDIT Task 7 (StubID stamping), Task 8 (live resolver)
│   └── server_test.go               # EDIT Task 6 (store construction), Task 8 (runtime reflect)
├── server/
│   ├── server.go                    # EDIT Task 6: NewStore() + ReplaceOrigin
│   ├── server_test.go               # EXTEND Task 8: duplicate-root startup failure
│   ├── watcher.go                   # EDIT Task 6: ReplaceOrigin
│   └── watcher_test.go              # EDIT Task 6: store construction
└── conformance/harness_test.go      # EDIT Task 1 (Snapshot), Task 6 (store construction)
```

Dependency order: Task 1 → 2 → 3 (registry); Task 4 → 5 (documents) and Task 4 → 6 (store needs `Origin`); Task 7 (journal) independent after Task 6's store wiring; Task 8 last. Tasks 4–5 can run in parallel with 2–3.

---

### Task 1: Copy-on-write registry core

**Files:**
- Rewrite: `internal/schema/registry.go`
- Extend: `internal/schema/registry_test.go`
- Modify: `internal/stub/stub.go:109` (`reg.Files()` → `reg.Snapshot()`)
- Modify: `internal/stub/template_test.go` (6 sites), `internal/match/match_test.go:296,352,612`, `conformance/harness_test.go:239` (same rename)

**Interfaces:**
- Produces: `Registry.Snapshot() *protoregistry.Files`; `Registry` implements `protodesc.Resolver` (`FindFileByPath(string) (protoreflect.FileDescriptor, error)`, `FindDescriptorByName(protoreflect.FullName) (protoreflect.Descriptor, error)`); unexported `(*Registry).apply(fn func(*protoregistry.Files) error) error` and `addNew(files *protoregistry.Files, fd protoreflect.FileDescriptor, added *[]string) error` (Task 2 reuses both). `Files()` no longer exists.
- Consumes: nothing from other tasks.

- [ ] **Step 1: Write the failing tests**

Append to `internal/schema/registry_test.go` (imports it will need: `google.golang.org/protobuf/reflect/protoreflect` if not present — check the existing import block and add only what is missing):

```go
// A snapshot taken before a mutation must not observe the mutation: the
// registry must swap a fresh Files rather than mutate the one it handed out.
func TestSnapshotIsImmutableAcrossMutation(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	before := reg.Snapshot()
	beforeCount := before.NumFiles()

	if err := reg.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		t.Fatal(err)
	}
	if got := before.NumFiles(); got != beforeCount {
		t.Fatalf("earlier snapshot grew from %d to %d files; mutation must build a new snapshot", beforeCount, got)
	}
	if got := reg.Snapshot().NumFiles(); got <= beforeCount {
		t.Fatalf("current snapshot has %d files, want > %d after AddFile", got, beforeCount)
	}
	if _, err := before.FindFileByPath("grpc/health/v1/health.proto"); err == nil {
		t.Fatal("old snapshot resolves the newly added file; snapshots must be frozen")
	}
}

// A failed load must leave the served snapshot untouched (all-or-nothing,
// design §2.3) — this now covers AddProtoDir too, which previously could
// leave a half-loaded registry.
func TestFailedLoadLeavesRegistryUntouched(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	before := reg.Snapshot()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.proto"), []byte("syntax = \"proto3\";\npackage broken;\nmessage M { this is not proto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddProtoDir(context.Background(), dir); err == nil {
		t.Fatal("AddProtoDir of a broken tree succeeded, want error")
	}
	if reg.Snapshot() != before {
		t.Fatal("failed AddProtoDir swapped the snapshot; must be all-or-nothing")
	}
}

// Descriptor identity is stable across registrations (design §2.3):
// pre-existing FileDescriptor values are reused verbatim in each candidate.
func TestDescriptorIdentityStableAcrossRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	d1, err := reg.FindDescriptorByName("shop.v1.OrderService")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		t.Fatal(err)
	}
	d2, err := reg.FindDescriptorByName("shop.v1.OrderService")
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("descriptor identity changed across an unrelated registration")
	}
}
```

Add these imports to the test file if absent: `os`, `path/filepath`, `healthpb "google.golang.org/grpc/health/grpc_health_v1"`. If `testdata/protos` does not define `shop.v1.OrderService`, check `server/server_test.go` (it uses `../testdata/protos` and calls `shop.v1.OrderService/GetOrder`) — the path from `internal/schema` is `../../testdata/protos`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/schema/ -run 'TestSnapshot|TestFailedLoad|TestDescriptorIdentity' -v`

Expected: compile error — `reg.Snapshot undefined` (and `FindDescriptorByName` undefined on `*Registry`).

- [ ] **Step 3: Rewrite the registry core**

Replace the type definition, constructor, and every mutator in `internal/schema/registry.go`. The full new content of the changed region (package comment, `LookupMethod`, `LookupMessage`, `Services`, and the `Types` fallback methods keep their current bodies except where shown):

```go
type Registry struct {
	mu   sync.Mutex // serializes load→build→swap; readers never take it
	snap atomic.Pointer[snapshot]
}

// snapshot is an immutable pairing of a descriptor index and its derived
// dynamic type table. Once stored in Registry.snap it is never mutated;
// mutation builds a fresh snapshot and swaps the pointer (design §2.2).
type snapshot struct {
	files *protoregistry.Files
	types *dynamicpb.Types
}

func newSnapshot(files *protoregistry.Files) *snapshot {
	return &snapshot{files: files, types: dynamicpb.NewTypes(files)}
}

func NewRegistry() *Registry {
	r := &Registry{}
	r.snap.Store(newSnapshot(new(protoregistry.Files)))
	return r
}

func (r *Registry) current() *snapshot { return r.snap.Load() }

// Snapshot returns the current descriptor set. Read-only by convention:
// registering into a returned snapshot is a data race with every other
// holder. It exists for the two consumers that need the concrete type —
// match.NewCompiler (RangeFiles + dynamicpb.NewTypes) — everything else
// should use the Registry's own resolver methods, which stay live across
// registrations.
func (r *Registry) Snapshot() *protoregistry.Files { return r.current().files }

// FindFileByPath implements protodesc.Resolver against the live snapshot.
func (r *Registry) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	return r.current().files.FindFileByPath(path)
}

// FindDescriptorByName implements protodesc.Resolver against the live snapshot.
func (r *Registry) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	return r.current().files.FindDescriptorByName(name)
}

// apply runs one mutation as load→build→swap: it copies the current snapshot
// into a fresh candidate, lets fn extend the candidate, and publishes the
// candidate only if fn succeeds. The mutex makes concurrent registrations
// serialize instead of losing updates; the failed-fn path leaves the served
// snapshot byte-for-byte untouched (all-or-nothing, design §2.3).
func (r *Registry) apply(fn func(candidate *protoregistry.Files) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	candidate := new(protoregistry.Files)
	var copyErr error
	r.current().files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		copyErr = candidate.RegisterFile(fd)
		return copyErr == nil
	})
	if copyErr != nil {
		return fmt.Errorf("copying registry snapshot: %w", copyErr)
	}
	if err := fn(candidate); err != nil {
		return err
	}
	r.snap.Store(newSnapshot(candidate))
	return nil
}

// addNew registers fd and, first, all of its imports into files. A path that
// is already present is skipped (first registration wins). Newly registered
// paths are appended to added when it is non-nil.
func addNew(files *protoregistry.Files, fd protoreflect.FileDescriptor, added *[]string) error {
	if _, err := files.FindFileByPath(fd.Path()); err == nil {
		return nil
	}
	imps := fd.Imports()
	for i := 0; i < imps.Len(); i++ {
		if err := addNew(files, imps.Get(i).FileDescriptor, added); err != nil {
			return err
		}
	}
	if err := files.RegisterFile(fd); err != nil {
		return err
	}
	if added != nil {
		*added = append(*added, fd.Path())
	}
	return nil
}

// AddFile registers a file descriptor and its imports as one atomic swap.
func (r *Registry) AddFile(fd protoreflect.FileDescriptor) error {
	return r.apply(func(candidate *protoregistry.Files) error {
		return addNew(candidate, fd, nil)
	})
}
```

`AddProtoDir` keeps its walk-and-compile body verbatim up to and including `compiler.Compile(ctx, names...)`; the loop that used to call `r.AddFile(fd)` per file becomes one swap:

```go
	return r.apply(func(candidate *protoregistry.Files) error {
		for _, fd := range compiled {
			if err := addNew(candidate, fd, nil); err != nil {
				return err
			}
		}
		return nil
	})
```

`AddDescriptorSetFile` keeps its read/unmarshal/validate body up to `protodesc.NewFiles(set)`; the trailing `RangeFiles`/`AddFile` loop becomes:

```go
	return r.apply(func(candidate *protoregistry.Files) error {
		var regErr error
		files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			regErr = addNew(candidate, fd, nil)
			return regErr == nil
		})
		return regErr
	})
```

Reads switch from `r.files` to the live snapshot — in `LookupMethod` and `LookupMessage` replace `r.files.FindDescriptorByName` with `r.current().files.FindDescriptorByName`; in `Services` replace `r.files.RangeFiles` with `r.current().files.RangeFiles`. Delete the `Files()` method entirely.

`Types` changes from binding a `*dynamicpb.Types` at construction to delegating live (design §2.5):

```go
func (r *Registry) Types() *Types { return &Types{reg: r} }

// Types is a registry-first, global-fallback protobuf type resolver. It reads
// the registry's current snapshot on every call, so a message type registered
// after Types was constructed still resolves.
type Types struct {
	reg *Registry
}
```

In each of the four `Types` methods, replace `t.dyn` with `t.reg.current().types` (bodies otherwise unchanged).

New imports in `registry.go`: `sync`, `sync/atomic`. The `dynamicpb` import stays.

- [ ] **Step 4: Run the schema package tests**

Run: `go test ./internal/schema/ -v`

Expected: all PASS, including the three new tests and every pre-existing test (pre-verified fact 8: no in-package test touches `reg.files`).

- [ ] **Step 5: Rename the cross-package `Files()` callers**

Four mechanical edits (pre-verified fact 9):

- `internal/stub/stub.go:109`: `match.NewCompiler(reg.Files())` → `match.NewCompiler(reg.Snapshot())`
- `internal/stub/template_test.go`: all six `match.NewCompiler(reg.Files())` → `match.NewCompiler(reg.Snapshot())`
- `internal/match/match_test.go:296`: `return reg.Files()` → `return reg.Snapshot()`; lines 352 and 612: `NewCompiler(reg.Files())` → `NewCompiler(reg.Snapshot())`
- `conformance/harness_test.go:239`: `match.NewCompiler(h.reg.Files())` → `match.NewCompiler(h.reg.Snapshot())`

- [ ] **Step 6: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: everything passes. `dataplane/server.go:58` still compiles because `reg` is passed in Task 8; until then it reads — wait, it calls `reg.Files()`. **It does not compile.** Make the Task 8 edit now to keep the tree green (it is the same one-line direction):

- `internal/dataplane/server.go:58`: `DescriptorResolver: reg.Files(),` → `DescriptorResolver: reg,`

This is safe immediately: `*schema.Registry` now implements `protodesc.Resolver`, and passing the registry live is exactly the design §2.5 endpoint. Task 8 adds the test that proves runtime registrations become reflectable; the wiring itself lands here.

Re-run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS across the board (including conformance).

- [ ] **Step 7: Commit**

```bash
git add internal/schema/registry.go internal/schema/registry_test.go internal/stub/stub.go internal/stub/template_test.go internal/match/match_test.go conformance/harness_test.go internal/dataplane/server.go
git commit -m "feat(schema): copy-on-write registry with immutable snapshots"
```

### Task 2: RegisterSet with normalized idempotency

**Files:**
- Modify: `internal/schema/registry.go` (add `RegisterSet`, `descriptorsEquivalent`, `normalizeFileProto`)
- Extend: `internal/schema/registry_test.go`
- Create: `internal/schema/testdata/wktset/scratch.proto`, `internal/schema/testdata/wkt_image.binpb`

**Interfaces:**
- Consumes: `apply` and `addNew` from Task 1 (exact signatures in Task 1's Produces).
- Produces: `Registry.RegisterSet(set *descriptorpb.FileDescriptorSet) (added []string, err error)` — Phase 4's `SchemaService.RegisterSchemas` backend; `added` is `RegisterSchemasResponse.registered_files`.

- [ ] **Step 1: Create the WKT fixture**

```bash
mkdir -p internal/schema/testdata/wktset
cat > internal/schema/testdata/wktset/scratch.proto <<'PROTO'
// Test fixture: a file importing three well-known types, used to prove that
// a buf-built image registers cleanly against a protocompile-built registry
// (design §2.6). Regenerate the committed image with:
//   bin/buf build internal/schema/testdata/wktset -o internal/schema/testdata/wkt_image.binpb
syntax = "proto3";

package scratch.v1;

import "google/protobuf/any.proto";
import "google/protobuf/duration.proto";
import "google/protobuf/timestamp.proto";

message Thing {
  string id = 1;
  google.protobuf.Timestamp at = 2;
  google.protobuf.Duration ttl = 3;
  google.protobuf.Any payload = 4;
}

service ThingService {
  rpc GetThing(Thing) returns (Thing);
}
PROTO
bin/buf build internal/schema/testdata/wktset -o internal/schema/testdata/wkt_image.binpb
```

(If `bin/buf` is missing, run `make tools` first — the Phase 2 Makefile installs the pinned toolchain.)

- [ ] **Step 2: Write the failing tests**

Append to `internal/schema/registry_test.go` (add imports `google.golang.org/protobuf/proto`, `google.golang.org/protobuf/types/descriptorpb`, `strings` as needed):

```go
func loadWKTImage(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	data, err := os.ReadFile("testdata/wkt_image.binpb")
	if err != nil {
		t.Fatal(err)
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(data, set); err != nil {
		t.Fatal(err)
	}
	return set
}

// The §2.6 risk test: a buf image carrying its own copies of the well-known
// types must register as a no-op against a registry whose WKTs came from
// protocompile. buf images also carry a per-file extension (field 8042) that
// lands in unknown fields; equivalence must see through both.
func TestRegisterSetIdempotentAcrossToolchains(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "testdata/wktset"); err != nil {
		t.Fatal(err)
	}
	added, err := reg.RegisterSet(loadWKTImage(t))
	if err != nil {
		t.Fatalf("RegisterSet of an equivalent buf image: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want none (every file already registered)", added)
	}
}

func TestRegisterSetAddsAllFilesToEmptyRegistryThenNoops(t *testing.T) {
	reg := NewRegistry()
	set := loadWKTImage(t)
	added, err := reg.RegisterSet(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != len(set.File) {
		t.Fatalf("added %d files, want all %d", len(added), len(set.File))
	}
	if _, err := reg.LookupMethod("scratch.v1.ThingService/GetThing"); err != nil {
		t.Fatalf("registered method not resolvable: %v", err)
	}
	again, err := reg.RegisterSet(set)
	if err != nil {
		t.Fatalf("re-registering the identical set: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second register added %v, want none", again)
	}
}

// Same path, different content → error naming the file, registry untouched.
func TestRegisterSetConflictRollsBack(t *testing.T) {
	reg := NewRegistry()
	set := loadWKTImage(t)
	if _, err := reg.RegisterSet(set); err != nil {
		t.Fatal(err)
	}
	before := reg.Snapshot()

	conflicting := proto.Clone(set).(*descriptorpb.FileDescriptorSet)
	for _, f := range conflicting.File {
		if f.GetName() == "scratch.proto" {
			f.MessageType[0].Field[0].Number = proto.Int32(99) // id: 1 → 99
		}
	}
	_, err := reg.RegisterSet(conflicting)
	if err == nil {
		t.Fatal("conflicting set registered, want error")
	}
	if !strings.Contains(err.Error(), "scratch.proto") {
		t.Fatalf("error %q does not name the conflicting file", err)
	}
	if reg.Snapshot() != before {
		t.Fatal("failed RegisterSet swapped the snapshot; must be all-or-nothing")
	}
}

func TestRegisterSetRejectsEmptyAndNonSelfContainedSets(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.RegisterSet(&descriptorpb.FileDescriptorSet{}); err == nil {
		t.Fatal("empty set accepted, want error")
	}
	set := loadWKTImage(t)
	partial := &descriptorpb.FileDescriptorSet{}
	for _, f := range set.File {
		if f.GetName() == "scratch.proto" { // imports absent → not self-contained
			partial.File = append(partial.File, f)
		}
	}
	if _, err := reg.RegisterSet(partial); err == nil {
		t.Fatal("non-self-contained set accepted, want error")
	} else if !strings.Contains(err.Error(), "self-contained") {
		t.Fatalf("error %q should point at self-containment", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/schema/ -run TestRegisterSet -v`

Expected: compile error — `reg.RegisterSet undefined`.

- [ ] **Step 4: Implement RegisterSet**

Append to `internal/schema/registry.go`:

```go
// RegisterSet registers every file of a serialized-set image all-or-nothing:
// on any error the served registry is untouched. The set must be
// self-contained (every import present). Idempotent: a path already
// registered with equivalent content is skipped; the same path with
// different content fails, naming the file. Returns the paths newly added
// (empty when every file was already present).
func (r *Registry) RegisterSet(set *descriptorpb.FileDescriptorSet) ([]string, error) {
	if len(set.GetFile()) == 0 {
		return nil, errors.New("descriptor set contains no files")
	}
	incoming, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("loading descriptor set (descriptor sets must be self-contained; build with `buf build -o` or `protoc --include_imports`): %w", err)
	}
	var added []string
	err = r.apply(func(candidate *protoregistry.Files) error {
		var rangeErr error
		incoming.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			existing, findErr := candidate.FindFileByPath(fd.Path())
			if findErr == nil {
				if !descriptorsEquivalent(existing, fd) {
					rangeErr = fmt.Errorf("file %q is already registered with different content", fd.Path())
				}
				return rangeErr == nil
			}
			rangeErr = addNew(candidate, fd, &added)
			return rangeErr == nil
		})
		return rangeErr
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// descriptorsEquivalent compares two copies of the same proto file for
// semantic equality, seeing through representation-only differences.
func descriptorsEquivalent(a, b protoreflect.FileDescriptor) bool {
	return proto.Equal(
		normalizeFileProto(protodesc.ToFileDescriptorProto(a)),
		normalizeFileProto(protodesc.ToFileDescriptorProto(b)),
	)
}

// normalizeFileProto strips the two representation-only differences observed
// between toolchains (design §2.6, verified empirically): source code info,
// and unknown fields — buf images carry a per-file extension (field 8042)
// that plain FileDescriptorSet parsing keeps as unknown bytes. Descriptor
// meaning may not be touched here: loosening equivalence further must never
// mask a real conflict, which the admin contract requires to fail.
func normalizeFileProto(fdp *descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto {
	clone := proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	clone.SourceCodeInfo = nil
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return clone
	}
	out := new(descriptorpb.FileDescriptorProto)
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(wire, out); err != nil {
		return clone
	}
	return out
}
```

One subtlety worth a comment where `addNew` is called from `RegisterSet`: a file registered from the incoming set keeps descriptor-internal references to the *incoming* copies of its imports even when the candidate already indexed equivalent copies — safe precisely because equivalence was just verified, and lookups by name always resolve the candidate's copy.

Note on ordering: `RangeFiles` order is unspecified, and that is fine — `addNew` registers imports before importers recursively, and a file whose path the candidate already holds is skipped at recursion time but still equivalence-checked when the outer range reaches its own visit.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/schema/ -v`

Expected: all PASS. The idempotency test is the design's named risk — if `TestRegisterSetIdempotentAcrossToolchains` fails here, diff the two `normalizeFileProto` outputs with `prototext` and extend the normalization for the specific field found (never by skipping conflicting files — design §2.6).

- [ ] **Step 6: Commit**

```bash
git add internal/schema/registry.go internal/schema/registry_test.go internal/schema/testdata
git commit -m "feat(schema): RegisterSet with toolchain-normalized idempotency"
```

### Task 3: Registry concurrency suite

**Files:**
- Extend: `internal/schema/registry_test.go`

**Interfaces:**
- Consumes: `RegisterSet` (Task 2), `Snapshot`/resolver methods (Task 1), plus the existing `stub.Compile` / CEL path via `internal/stub` — **no**: `internal/schema` cannot import `internal/stub` (import cycle: stub → schema). The CEL escape path is exercised with `match.NewCompiler` directly (match imports nothing from schema).
- Produces: nothing; pure tests.

- [ ] **Step 1: Write the concurrent-writers union test**

Append to `internal/schema/registry_test.go`:

```go
// buildSet compiles nothing: it hand-builds a minimal one-file set with a
// unique package so disjoint registrations cannot conflict.
func buildSet(t *testing.T, pkg string) *descriptorpb.FileDescriptorSet {
	t.Helper()
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String(pkg + ".proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Msg"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("id"),
				Number: proto.Int32(1),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}},
		}},
	}}}
}

// The lost-update check -race cannot make (design §2.2): two registrations
// racing on the same base snapshot must both land; an atomic pointer alone
// would silently drop one.
func TestConcurrentDisjointRegistrationsBothLand(t *testing.T) {
	for round := 0; round < 50; round++ {
		reg := NewRegistry()
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = reg.RegisterSet(buildSet(t, fmt.Sprintf("race%da", i)))
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d: %v", round, i, err)
			}
		}
		for i := 0; i < 2; i++ {
			name := protoreflect.FullName(fmt.Sprintf("race%da.Msg", i))
			if _, err := reg.FindDescriptorByName(name); err != nil {
				t.Fatalf("round %d: %s missing after concurrent registration: %v", round, name, err)
			}
		}
	}
}
```

Add imports as needed: `fmt`, `sync`.

- [ ] **Step 2: Write the escape-path race test**

The three references that escaped the old registry (design §7): reflection-style resolver lookups, CEL compile+eval, dynamic `Any`/type resolution. Append:

```go
// All three historical escape paths running against a mutating registry.
// Run under -race: the copy-on-write snapshots must make every read safe.
func TestReadsRaceRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	types := reg.Types()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: register a fresh disjoint set per iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := reg.RegisterSet(buildSet(t, fmt.Sprintf("w%d", i))); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Reader 1: resolver lookups (what grpc reflection does).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := reg.FindDescriptorByName("shop.v1.OrderService"); err != nil {
				t.Error(err)
				return
			}
			if _, err := reg.FindFileByPath(method.ParentFile().Path()); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Reader 2: CEL compile + eval over a snapshot (what stub compilation does).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			compiler := match.NewCompiler(reg.Snapshot())
			compiled, err := compiler.Compile(method.Input(), &match.Block{Expr: `message.order_id == "x"`}, match.Unary)
			if err != nil {
				t.Error(err)
				return
			}
			msg := dynamicpb.NewMessage(method.Input())
			compiled.Eval(match.Input{Message: msg})
		}
	}()

	// Reader 3: dynamic type resolution (what templates and Any details do).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := types.FindMessageByName(method.Input().FullName()); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}
```

Add imports: `time`, `github.com/yinghanhung/simulacra/internal/match`, `google.golang.org/protobuf/types/dynamicpb`. (`match` imports `protoregistry` but not `schema`, so no cycle.)

- [ ] **Step 3: Run under race**

Run: `go test ./internal/schema/ -race -run 'TestConcurrent|TestReadsRace' -v`

Expected: PASS with no race reports. Then the full package: `go test ./internal/schema/ -race` — PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/schema/registry_test.go
git commit -m "test(schema): concurrency suite for the copy-on-write registry"
```

### Task 4: Stub origins, documents, and the per-stub decode boundary

**Files:**
- Modify: `internal/stub/stub.go` (add `Origin` type; `ID`, `Origin`, `Document` fields on `Compiled`)
- Create: `internal/stub/document.go`, `internal/stub/document_test.go`
- Modify: `internal/stub/loader.go` (`parseFile` harvests per-stub documents; `LoadDirs` stamps `ID` and `Document`)
- Extend: `internal/stub/loader_test.go`

**Interfaces:**
- Consumes: nothing new (Task 1's `Snapshot` rename already landed in this package).
- Produces: `type Origin uint8` (`OriginFile`, `OriginAPI`, `String() string` → `"file"`/`"api"`); `Compiled.ID string`, `Compiled.Origin Origin`, `Compiled.Document string`; `ParseDocument(data []byte) (Stub, string, error)` (stub, normalized document, error); `RenderSequence(docs []string) (string, error)`. Task 6's store stamps `Origin`; Phase 4 calls `ParseDocument` then `Compile` then sets `Document`.

**Design deviation, recorded:** the design (§3.2) wrote `ParseDocument(data []byte) (Stub, error)` and (§3.1) "re-marshaling the decoded Stub". Pre-verified fact 4 rules out struct re-marshaling (nil-vs-empty maps break the step grammar both ways), so the document is rendered from the input's node tree and `ParseDocument` returns it — same intent, one extra return value.

- [ ] **Step 1: Add the Origin type and Compiled fields**

In `internal/stub/stub.go`, above the `Compiled` type:

```go
// Origin records who owns a stub: the hot-reload watcher (file) or an admin
// API client (api). The store stamps it on ingest (design §3.4); compilers
// leave it zero.
type Origin uint8

const (
	OriginFile Origin = iota
	OriginAPI
)

func (o Origin) String() string {
	if o == OriginAPI {
		return "api"
	}
	return "file"
}
```

Extend `Compiled` (existing fields unchanged):

```go
type Compiled struct {
	Method   string // normalized "/pkg.Service/Method"
	Shape    match.Shape
	Priority int
	Times    int
	Source   string
	ID       string // store-managed: file stubs carry Source, API stubs get "api-<n>"
	Origin   Origin // store-managed: stamped on ingest
	Document string // normalized single-stub YAML mapping (admin Stub.document)
	matcher  *match.Compiled
	plan     *Plan
}
```

- [ ] **Step 2: Write the failing document tests**

Create `internal/stub/document_test.go`:

```go
package stub

import (
	"strings"
	"testing"
)

func TestParseDocumentAcceptsYAMLMapping(t *testing.T) {
	s, doc, err := ParseDocument([]byte("# a comment\nmethod: a.B/C # inline\nrespond:\n  message: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Method != "a.B/C" {
		t.Fatalf("method = %q", s.Method)
	}
	want := "method: a.B/C\nrespond:\n    message: {}\n"
	if doc != want {
		t.Fatalf("normalized document:\n%q\nwant:\n%q", doc, want)
	}
}

func TestParseDocumentAcceptsJSONAndNormalizesToYAML(t *testing.T) {
	s, doc, err := ParseDocument([]byte(`{"method": "a.B/C", "times": 3, "respond": {"message": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Method != "a.B/C" || s.Times != 3 {
		t.Fatalf("stub = %+v", s)
	}
	if strings.Contains(doc, "{\"") {
		t.Fatalf("document kept JSON flow style:\n%s", doc)
	}
	// Normalization is a fixed point: re-parsing the document re-renders it.
	s2, doc2, err := ParseDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if doc2 != doc {
		t.Fatalf("not a fixed point:\n%q\nvs\n%q", doc, doc2)
	}
	if s2.Method != s.Method || s2.Times != s.Times {
		t.Fatalf("round trip changed the stub: %+v vs %+v", s, s2)
	}
}

func TestParseDocumentRejectsSequence(t *testing.T) {
	_, _, err := ParseDocument([]byte("- method: a.B/C\n"))
	if err == nil || !strings.Contains(err.Error(), "files take lists") {
		t.Fatalf("err = %v, want sequence rejection pointing at the file grammar", err)
	}
}

func TestParseDocumentRejectsMultipleDocuments(t *testing.T) {
	_, _, err := ParseDocument([]byte("method: a.B/C\n---\nmethod: a.B/D\n"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("err = %v, want exactly-one rejection", err)
	}
}

func TestParseDocumentRejectsEmptyInput(t *testing.T) {
	for _, in := range []string{"", "   \n", "\n\n"} {
		if _, _, err := ParseDocument([]byte(in)); err == nil {
			t.Fatalf("empty input %q accepted, want error", in)
		}
	}
}

func TestParseDocumentRejectsUnknownFieldWithPosition(t *testing.T) {
	_, _, err := ParseDocument([]byte("method: a.B/C\nbogus: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want unknown-field error with the input's own position", err)
	}
}

func TestParseDocumentRejectsScalar(t *testing.T) {
	if _, _, err := ParseDocument([]byte("just a string\n")); err == nil {
		t.Fatal("scalar document accepted, want mapping-required error")
	}
}

func TestRenderSequenceRoundTripsThroughFileGrammar(t *testing.T) {
	// Canonical per-stub documents come from ParseDocument itself, so this
	// test never hardcodes yaml.v3's indentation choices.
	var docs []string
	for _, in := range []string{
		"method: a.B/C\nrespond:\n  message: {}\n",
		"method: a.B/D\ntimes: 2\nrespond:\n  stream:\n  - message:\n      x: 1\n  - status:\n      code: 5\n",
	} {
		_, doc, err := ParseDocument([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
	}
	out, err := RenderSequence(docs)
	if err != nil {
		t.Fatal(err)
	}
	// The output must be one file-grammar sequence parseFile accepts.
	dir := t.TempDir()
	path := dir + "/export.yaml"
	if err := writeTestFile(t, path, out); err != nil {
		t.Fatal(err)
	}
	stubs, perStub, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile of rendered export: %v", err)
	}
	if len(stubs) != 2 {
		t.Fatalf("parsed %d stubs, want 2", len(stubs))
	}
	if stubs[0].Method != "a.B/C" || stubs[1].Method != "a.B/D" || stubs[1].Times != 2 {
		t.Fatalf("round-tripped stubs differ: %+v", stubs)
	}
	if len(perStub) != 2 || perStub[0] != docs[0] || perStub[1] != docs[1] {
		t.Fatalf("per-stub documents did not round trip:\n%q\nwant\n%q", perStub, docs)
	}
}

func TestRenderSequenceOfNothingIsEmptyList(t *testing.T) {
	out, err := RenderSequence(nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "[]\n" {
		t.Fatalf("empty export = %q, want %q", out, "[]\n")
	}
}
```

And a tiny helper at the bottom of the same file:

```go
func writeTestFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0o644)
}
```

(import `os`). Note the expected indentation in golden strings is yaml.v3's default four spaces.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/stub/ -run 'TestParseDocument|TestRenderSequence' -v`

Expected: compile error — `ParseDocument` undefined.

- [ ] **Step 4: Implement document.go**

Create `internal/stub/document.go`:

```go
package stub

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseDocument decodes exactly one stub mapping — the admin API's
// CreateStub grammar — in YAML or JSON form, with the same strictness the
// file loader applies (unknown fields are errors). It returns the decoded
// stub and its normalized document: the input's own node tree re-rendered as
// block YAML with comments and styling stripped. Rendering from the node
// rather than the struct keeps nil-versus-empty distinctions the struct
// cannot represent (an empty message step must stay `message: {}`).
func ParseDocument(data []byte) (Stub, string, error) {
	node, err := decodeSingleDocument(data)
	if err != nil {
		return Stub{}, "", err
	}
	if node.Kind == yaml.SequenceNode {
		return Stub{}, "", errors.New("document is a sequence; the API takes one stub mapping per call (files take lists)")
	}
	if node.Kind != yaml.MappingNode {
		return Stub{}, "", errors.New("document must be a YAML or JSON mapping holding one stub")
	}
	// Strict decode runs from the original bytes, not the node, for two
	// reasons: yaml.Node.Decode has no KnownFields control, and errors must
	// carry the caller's own line numbers.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Stub
	if err := dec.Decode(&s); err != nil {
		return Stub{}, "", fmt.Errorf("parsing stub document: %w", err)
	}
	doc, err := renderNode(node)
	if err != nil {
		return Stub{}, "", err
	}
	return s, doc, nil
}

// decodeSingleDocument parses data into exactly one YAML document node and
// returns its content node. Empty input and multi-document input are errors.
func decodeSingleDocument(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("document is empty; expected one stub mapping")
		}
		return nil, fmt.Errorf("parsing document: %w", err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("document holds more than one YAML document; expected exactly one stub mapping")
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("document is empty; expected one stub mapping")
	}
	return doc.Content[0], nil
}

// normalizeNode strips comments and input styling in place so rendering is
// canonical block YAML regardless of how the input was written.
func normalizeNode(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	n.Style = 0
	for _, c := range n.Content {
		normalizeNode(c)
	}
}

// renderNode renders one mapping node as a normalized document.
func renderNode(n *yaml.Node) (string, error) {
	normalizeNode(n)
	out, err := yaml.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("rendering normalized document: %w", err)
	}
	return string(out), nil
}

// RenderSequence assembles normalized per-stub documents into one
// file-grammar YAML sequence (ExportStubs). Assembling nodes rather than
// concatenating indented text keeps block scalars and nesting correct.
func RenderSequence(docs []string) (string, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for i, doc := range docs {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
			return "", fmt.Errorf("document %d: %w", i, err)
		}
		if len(node.Content) == 0 {
			return "", fmt.Errorf("document %d is empty", i)
		}
		seq.Content = append(seq.Content, node.Content[0])
	}
	out, err := yaml.Marshal(seq)
	if err != nil {
		return "", fmt.Errorf("rendering stub sequence: %w", err)
	}
	return string(out), nil
}
```

- [ ] **Step 5: Rework parseFile to harvest per-stub documents**

In `internal/stub/loader.go`, change `parseFile` to return the normalized document beside each stub. The struct decode is untouched (it keeps whole-file error positions); a second pass over the same bytes walks the node tree:

```go
// parseFile reads one YAML file holding a list of stubs, decoding every
// `---` document in the file. Parsing is strict: unknown keys are errors,
// so unsupported/future syntax fails loudly instead of being ignored. The
// second return value carries each stub's normalized document (design §3.1),
// harvested from a parallel node pass over the same bytes.
func parseFile(path string) ([]Stub, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var stubs []Stub
	for {
		var doc []Stub
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		stubs = append(stubs, doc...)
	}
	docs, err := stubDocuments(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(docs) != len(stubs) {
		return nil, nil, fmt.Errorf("parsing %s: %d stubs but %d documents; file structure not understood", path, len(stubs), len(docs))
	}
	return stubs, docs, nil
}

// stubDocuments renders each sequence item of each YAML document as a
// normalized per-stub document.
func stubDocuments(data []byte) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []string
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		seq := doc.Content[0]
		if seq.Kind != yaml.SequenceNode {
			return nil, errors.New("stub files hold a list of stubs")
		}
		for _, item := range seq.Content {
			rendered, err := renderNode(item)
			if err != nil {
				return nil, err
			}
			docs = append(docs, rendered)
		}
	}
}
```

Note: the struct decoder accepts a document whose content is a sequence — that is the file grammar — so `stubDocuments` returning "stub files hold a list of stubs" for a non-sequence document can only fire on shapes the struct decode also rejected; the count check is the belt to that suspender.

In `LoadDirs`, the per-file loop changes to stamp `ID` and `Document`:

```go
	for _, path := range paths {
		stubs, docs, err := parseFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i, s := range stubs {
			source := fmt.Sprintf("%s#%d", path, i)
			c, err := compiler.Compile(s, source)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			c.ID = source
			c.Document = docs[i]
			out = append(out, c)
		}
	}
```

(`Origin` is stamped by the store on ingest — design §3.4 — not here.)

- [ ] **Step 6: Add the loader round-trip test**

Append to `internal/stub/loader_test.go` (reuse its existing registry/compile helpers — read the file first and follow its pattern for building a registry; it already loads `testdata` protos for compile tests):

```go
// Every file-loaded stub carries a normalized document that re-parses to the
// same stub — the file/API single-grammar guarantee (design §3.1).
func TestLoadDirsStampsRoundTrippableDocuments(t *testing.T) {
	reg := testRegistry(t) // whatever helper loader_test already uses; follow the file's pattern
	dir := t.TempDir()
	writeStubFile(t, dir, "s.yaml", `
# file comment
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: o-1 }
  respond:
    message: {}
- method: shop.v1.OrderService/GetOrder
  priority: 5
  respond:
    message:
      order_id: o-2
`)
	stubs, errs := LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(stubs) != 2 {
		t.Fatalf("loaded %d stubs, want 2", len(stubs))
	}
	for i, c := range stubs {
		if c.ID != c.Source || c.ID == "" {
			t.Fatalf("stub %d: ID = %q, Source = %q; file stubs carry Source as ID", i, c.ID, c.Source)
		}
		if c.Document == "" {
			t.Fatalf("stub %d: empty Document", i)
		}
		s, normalized, err := ParseDocument([]byte(c.Document))
		if err != nil {
			t.Fatalf("stub %d document does not re-parse: %v\n%s", i, err, c.Document)
		}
		if normalized != c.Document {
			t.Fatalf("stub %d document is not a normalization fixed point", i)
		}
		re, err := Compile(reg, s, c.Source)
		if err != nil {
			t.Fatalf("stub %d document does not re-compile: %v", i, err)
		}
		if re.Method != c.Method || re.Priority != c.Priority || re.Times != c.Times || re.Shape != c.Shape {
			t.Fatalf("stub %d round trip changed compiled fields: %+v vs %+v", i, re, c)
		}
	}
}
```

If `loader_test.go` lacks a `writeStubFile`-style helper, add one (`os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)` with `t.Helper()` and a fatal on error). Match the existing registry-helper name — do not invent `testRegistry` if the file calls it something else.

- [ ] **Step 7: Run the package**

Run: `go test ./internal/stub/ -v`

Expected: all PASS — the new document tests, the loader round trip, and every pre-existing loader/compiler test (parseFile's struct decode is unchanged).

- [ ] **Step 8: Commit**

```bash
git add internal/stub/stub.go internal/stub/document.go internal/stub/document_test.go internal/stub/loader.go internal/stub/loader_test.go
git commit -m "feat(stub): origins, IDs, and normalized per-stub documents"
```

### Task 5: ParseMatchDocument and CompileMatch

**Files:**
- Modify: `internal/stub/document.go`, `internal/stub/stub.go`
- Extend: `internal/stub/document_test.go`

**Interfaces:**
- Consumes: `decodeSingleDocument` (Task 4).
- Produces: `ParseMatchDocument(data []byte) (*match.Block, error)` — nil block for empty input; `(*Compiler).CompileMatch(method string, b *match.Block) (*match.Compiled, error)`. Phase 4's `VerifyCalls` is `ParseMatchDocument` → `CompileMatch` → `journal.Verify`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/stub/document_test.go`:

```go
func TestParseMatchDocumentEmptyMeansMatchAll(t *testing.T) {
	for _, in := range []string{"", "   ", "\n"} {
		b, err := ParseMatchDocument([]byte(in))
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		if b != nil {
			t.Fatalf("input %q: block = %+v, want nil (match-all)", in, b)
		}
	}
}

func TestParseMatchDocumentDecodesStrictly(t *testing.T) {
	b, err := ParseMatchDocument([]byte("message:\n  order_id: { eq: o-1 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Message["order_id"]["eq"] != "o-1" {
		t.Fatalf("block = %+v", b)
	}
	if _, err := ParseMatchDocument([]byte("bogus: 1\n")); err == nil {
		t.Fatal("unknown matcher field accepted, want error")
	}
	if _, err := ParseMatchDocument([]byte("- message\n")); err == nil {
		t.Fatal("sequence matcher accepted, want error")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/stub/ -run TestParseMatchDocument -v`

Expected: compile error — `ParseMatchDocument` undefined.

- [ ] **Step 3: Implement**

Append to `internal/stub/document.go` (add import `strings` and `github.com/yinghanhung/simulacra/internal/match`):

```go
// ParseMatchDocument decodes a stub-grammar match block
// (VerifyCalls.matcher_document) with the loader's strictness. Empty or
// whitespace-only input returns a nil block — the match-all value the
// contract requires ("empty matches any call to method"); match.Compile and
// journal.Verify already treat nil as match-all.
func ParseMatchDocument(data []byte) (*match.Block, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	node, err := decodeSingleDocument(data)
	if err != nil {
		return nil, err
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("matcher document must be a mapping (metadata/message/expr)")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var b match.Block
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("parsing matcher document: %w", err)
	}
	return &b, nil
}
```

Append to `internal/stub/stub.go` after `Compile`:

```go
// CompileMatch compiles a bare match block against a method — the matcher
// half of Compile, for VerifyCalls. A nil block compiles to match-all.
func (c *Compiler) CompileMatch(method string, b *match.Block) (*match.Compiled, error) {
	m, err := c.reg.LookupMethod(method)
	if err != nil {
		return nil, err
	}
	return c.matcher.Compile(m.Input(), b, match.ShapeOf(m))
}
```

- [ ] **Step 4: Add a CompileMatch test**

Append to `internal/stub/document_test.go` (reuse the registry helper found in Step 6 of Task 4):

```go
func TestCompileMatchCompilesAgainstTheMethod(t *testing.T) {
	reg := testRegistry(t) // same helper name as the loader tests actually use
	c := NewCompiler(reg)
	compiled, err := c.CompileMatch("shop.v1.OrderService/GetOrder", &match.Block{
		Message: map[string]match.Rules{"order_id": {"eq": "o-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled == nil {
		t.Fatal("nil compiled matcher")
	}
	if _, err := c.CompileMatch("no.Such/Method", nil); err == nil {
		t.Fatal("unknown method accepted, want error")
	}
	if _, err := c.CompileMatch("shop.v1.OrderService/GetOrder", &match.Block{
		Message: map[string]match.Rules{"no_such_field": {"eq": "x"}},
	}); err == nil {
		t.Fatal("bad field path accepted, want compile error")
	}
}
```

(import `github.com/yinghanhung/simulacra/internal/match` in the test file.)

- [ ] **Step 5: Run and commit**

Run: `go test ./internal/stub/ -v` — expected: all PASS.

```bash
git add internal/stub/document.go internal/stub/document_test.go internal/stub/stub.go
git commit -m "feat(stub): strict matcher documents and CompileMatch for VerifyCalls"
```

### Task 6: Store rework and its wiring

**Files:**
- Rewrite: `internal/stub/store.go` (keep `Miss`, `Selection`, `candidateSnapshot`, `Select`, `SelectOrExplain`, `Explain`, `snapshotLocked`, `explainSnapshot`, `inputWithTime`, `Len`, `CountFor` verbatim; everything else changes)
- Extend: `internal/stub/store_test.go`
- Modify: `server/server.go:102`, `server/watcher.go:209`
- Modify (mechanical): `internal/dataplane/server_test.go`, `server/watcher_test.go`, `conformance/harness_test.go` — every `NewStore(stubs)` / `Replace` site (pre-verified fact 9)

**Interfaces:**
- Consumes: `Origin`, `OriginFile`, `OriginAPI`, `Compiled.ID/Origin/Document` (Task 4).
- Produces: `NewStore() *Store`; `(*Store).Add(c *Compiled) string`; `(*Store).Remove(id string) error` with `var ErrStubNotFound` and `type FileOwnedError struct{ ID, Source string }`; `(*Store).ReplaceOrigin(origin Origin, stubs []*Compiled) ([]string, error)`; `(*Store).List(f ListFilter) []Info` with `type Info` (`ID, Method string; Shape match.Shape; Priority, Times int; Origin Origin; Source string; Hits int; Document string`) and `type ListFilter struct{ Method string; Origin *Origin }`; `(*Store).ResetStubs()`. `Replace` and the stub-taking `NewStore` no longer exist.

- [ ] **Step 1: Write the failing store tests**

Append to `internal/stub/store_test.go`. First a helper beside the file's existing stub-construction helpers (read them and reuse; they build `*Compiled` values with distinct `Source` strings — if any two share a `Source`, give them distinct ones, since `Source` is now the file-stub ID):

```go
// storeWith builds a store the way server.Start now does: empty, then one
// validated file-origin ingest.
func storeWith(t *testing.T, stubs ...*Compiled) *Store {
	t.Helper()
	s := NewStore()
	if _, err := s.ReplaceOrigin(OriginFile, stubs); err != nil {
		t.Fatal(err)
	}
	return s
}
```

Then the behavior tests:

```go
func TestAddStampsAPIOwnership(t *testing.T) {
	c := compiledForTest(t, "shop.v1.OrderService/GetOrder") // reuse/adapt the file's existing constructor helper
	c.Origin = OriginFile                                    // a wrong pre-set value must be overwritten
	c.Source = "left-over"
	s := NewStore()
	id := s.Add(c)
	if id != "api-1" {
		t.Fatalf("id = %q, want api-1", id)
	}
	if c.Origin != OriginAPI || c.Source != "api" || c.ID != "api-1" {
		t.Fatalf("Add did not stamp ownership: %+v", c)
	}
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].Origin != OriginAPI || infos[0].ID != "api-1" {
		t.Fatalf("List = %+v", infos)
	}
}

func TestAPIBeatsFileAtEqualPriority(t *testing.T) {
	file := compiledForTest(t, method)
	api := compiledForTest(t, method) // same priority
	s := storeWith(t, file)
	s.Add(api)
	if got := s.Select(method, matchingInput()); got != api {
		t.Fatalf("Select = %v, want the API stub at equal priority", got)
	}
}

func TestHigherPriorityFileBeatsLowerPriorityAPI(t *testing.T) {
	file := compiledForTest(t, method)
	file.Priority = 10
	api := compiledForTest(t, method)
	s := storeWith(t, file)
	s.Add(api)
	if got := s.Select(method, matchingInput()); got != file {
		t.Fatalf("Select = %v, want the priority-10 file stub", got)
	}
}

func TestRemoveRefusesFileOriginAndUnknownIDs(t *testing.T) {
	file := compiledForTest(t, method)
	file.ID = "stubs/a.yaml#0"
	file.Source = "stubs/a.yaml#0"
	s := storeWith(t, file)
	err := s.Remove("stubs/a.yaml#0")
	var owned *FileOwnedError
	if !errors.As(err, &owned) {
		t.Fatalf("Remove(file-origin) = %v, want *FileOwnedError", err)
	}
	if owned.Source != "stubs/a.yaml#0" {
		t.Fatalf("owned.Source = %q", owned.Source)
	}
	if err := s.Remove("nope"); !errors.Is(err, ErrStubNotFound) {
		t.Fatalf("Remove(unknown) = %v, want ErrStubNotFound", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d after refused removes, want 1", s.Len())
	}
}

func TestRemoveDeletesAPIStub(t *testing.T) {
	s := NewStore()
	id := s.Add(compiledForTest(t, method))
	if err := s.Remove(id); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}

func TestReplaceOriginPreservesOtherOriginAndItsCounters(t *testing.T) {
	limited := compiledForTest(t, method)
	limited.Times = 2
	s := NewStore()
	s.Add(limited) // API stub with a budget
	if got := s.Select(method, matchingInput()); got != limited {
		t.Fatal("setup select failed")
	}
	// A file hot-reload must not replenish the API stub's budget.
	fresh := compiledForTest(t, method)
	fresh.ID, fresh.Source = "f#0", "f#0"
	fresh.Priority = -1 // below the API stub, so selection still hits the API stub
	if _, err := s.ReplaceOrigin(OriginFile, []*Compiled{fresh}); err != nil {
		t.Fatal(err)
	}
	if got := s.Select(method, matchingInput()); got != limited {
		t.Fatalf("Select = %v, want the API stub's second use", got)
	}
	if got := s.Select(method, matchingInput()); got != fresh {
		t.Fatalf("Select = %v, want fallthrough to file stub: API budget must be spent (2 uses), not replenished by the reload", got)
	}
}

func TestReplaceOriginRejectsDuplicateIDsUntouched(t *testing.T) {
	a := compiledForTest(t, method)
	a.ID, a.Source = "dup#0", "dup#0"
	s := storeWith(t, a)
	b := compiledForTest(t, method)
	b.ID, b.Source = "same#0", "same#0"
	c := compiledForTest(t, method)
	c.ID, c.Source = "same#0", "same#0"
	if _, err := s.ReplaceOrigin(OriginFile, []*Compiled{b, c}); err == nil {
		t.Fatal("duplicate ids accepted, want error")
	} else if !strings.Contains(err.Error(), "same#0") {
		t.Fatalf("error %q does not name the duplicate id", err)
	}
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].ID != "dup#0" {
		t.Fatalf("failed ReplaceOrigin changed the store: %+v", infos)
	}
}

func TestReplaceOriginAssignsIDsToAPIDocuments(t *testing.T) {
	s := NewStore()
	ids, err := s.ReplaceOrigin(OriginAPI, []*Compiled{compiledForTest(t, method), compiledForTest(t, method)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == ids[1] || ids[0] == "" {
		t.Fatalf("ids = %v, want two distinct assigned ids", ids)
	}
}

func TestResetStubsDropsAPIAndRestoresFileBudgets(t *testing.T) {
	limited := compiledForTest(t, method)
	limited.Times = 1
	limited.ID, limited.Source = "f#0", "f#0"
	s := storeWith(t, limited)
	s.Add(compiledForTest(t, method))
	if got := s.Select(method, matchingInput()); got == nil {
		t.Fatal("setup select failed")
	}
	s.ResetStubs()
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].Origin != OriginFile {
		t.Fatalf("after ResetStubs: %+v, want only the file stub", infos)
	}
	if infos[0].Hits != 0 {
		t.Fatalf("Hits = %d after ResetStubs, want 0 (budget restored)", infos[0].Hits)
	}
}

func TestListFiltersByMethodAndOrigin(t *testing.T) {
	f := compiledForTest(t, method)
	f.ID, f.Source = "f#0", "f#0"
	s := storeWith(t, f)
	s.Add(compiledForTest(t, method))
	api := OriginAPI
	if got := s.List(ListFilter{Origin: &api}); len(got) != 1 || got[0].Origin != OriginAPI {
		t.Fatalf("List(api) = %+v", got)
	}
	if got := s.List(ListFilter{Method: "/no.Such/Method"}); len(got) != 0 {
		t.Fatalf("List(unknown method) = %+v", got)
	}
	if got := s.List(ListFilter{}); len(got) != 2 {
		t.Fatalf("List(all) = %+v", got)
	}
}

func TestListReportsHits(t *testing.T) {
	c := compiledForTest(t, method)
	s := NewStore()
	s.Add(c)
	s.Select(method, matchingInput())
	s.Select(method, matchingInput())
	infos := s.List(ListFilter{})
	if len(infos) != 1 || infos[0].Hits != 2 {
		t.Fatalf("Hits = %+v, want 2", infos)
	}
}
```

`method`, `matchingInput()`, and `compiledForTest` stand for whatever the existing `store_test.go` helpers are actually called — read the file first and use its real helper names and its real method constant; the tests above express behavior, and the helpers must construct `*Compiled` values that match `matchingInput()`. Add imports `errors`, `strings` as needed.

Also update the two existing `store.Replace(...)` tests: `TestReplaceAtomicallyResetsTimesBudget` becomes a `ReplaceOrigin(OriginFile, ...)` call asserting the same budget-reset behavior for the replaced origin, and `TestLenCountsEveryMethodAndFollowsReplace` swaps `NewStore(...)`/`Replace(...)` for `storeWith(...)`/`ReplaceOrigin(OriginFile, ...)` — same assertions. Every other `NewStore([]*Compiled{...})` in the file becomes `storeWith(t, ...)`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/stub/ -run 'TestAdd|TestAPIBeats|TestRemove|TestReplaceOrigin|TestResetStubs|TestList' -v`

Expected: compile errors — `NewStore()` arity, `Add`/`Remove`/`ReplaceOrigin`/`List`/`ResetStubs` undefined.

- [ ] **Step 3: Rewrite store.go**

Replace `Store`, `entry`, `NewStore`, `buildIndex`, and `Replace` in `internal/stub/store.go` with:

```go
// Store holds compiled stubs grouped by method and selects the stub for a
// request: highest priority first; at equal priority API-origin stubs beat
// file-origin stubs (a test overrides a sandbox default without priority
// arithmetic), and within an origin ingest order breaks ties. Stubs whose
// `times` budget is spent are skipped. Selection consumes one use atomically.
//
// The store owns stub identity (design §3.4): Add and ReplaceOrigin stamp
// Origin — and, for API stubs, ID and Source — on the values they install,
// so no ingest path can create a stub with wrong ownership.
type Store struct {
	mu       sync.Mutex
	byMethod map[string][]*entry
	nextID   uint64 // "api-<n>" assignment; monotonic, never reused
	nextSeq  uint64 // ingest order for same-origin tie-breaking
}

type entry struct {
	stub *Compiled
	used int
	seq  uint64
}

// ErrStubNotFound reports a Remove of an id the store does not hold.
var ErrStubNotFound = errors.New("no stub with this id")

// FileOwnedError reports a Remove of a file-origin stub, which only its
// file can remove (FAILED_PRECONDITION at the admin surface).
type FileOwnedError struct {
	ID     string
	Source string
}

func (e *FileOwnedError) Error() string {
	return fmt.Sprintf("stub %s is owned by %s; edit or remove the file", e.ID, e.Source)
}

// Info is the read-only envelope for one registered stub (admin Stub).
// Hits is the stub's consumed times budget: it resets when the stub's own
// origin is replaced or reset — it is not a lifetime total.
type Info struct {
	ID       string
	Method   string
	Shape    match.Shape
	Priority int
	Times    int
	Origin   Origin
	Source   string
	Hits     int
	Document string
}

// ListFilter narrows List output; the zero value lists everything.
type ListFilter struct {
	Method string  // normalized "/pkg.Service/Method"; "" means all
	Origin *Origin // nil means all origins
}

// NewStore returns an empty store. Population goes through Add and
// ReplaceOrigin only, so every ingest is validated and stamped.
func NewStore() *Store {
	return &Store{byMethod: make(map[string][]*entry)}
}

func originRank(o Origin) int {
	if o == OriginAPI {
		return 0
	}
	return 1
}

func sortEntries(entries []*entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.stub.Priority != b.stub.Priority {
			return a.stub.Priority > b.stub.Priority
		}
		if ra, rb := originRank(a.stub.Origin), originRank(b.stub.Origin); ra != rb {
			return ra < rb
		}
		return a.seq < b.seq
	})
}

// Add installs one API-origin stub, stamping Origin, Source, and a
// store-assigned id on c, and returns the id.
func (s *Store) Add(c *Compiled) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	c.ID = fmt.Sprintf("api-%d", s.nextID)
	c.Origin = OriginAPI
	c.Source = "api"
	s.nextSeq++
	entries := append(s.byMethod[c.Method], &entry{stub: c, seq: s.nextSeq})
	sortEntries(entries)
	s.byMethod[c.Method] = entries
	return c.ID
}

// Remove deletes an API-origin stub by id.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, entries := range s.byMethod {
		for i, e := range entries {
			if e.stub.ID != id {
				continue
			}
			if e.stub.Origin != OriginAPI {
				return &FileOwnedError{ID: id, Source: e.stub.Source}
			}
			entries = append(entries[:i], entries[i+1:]...)
			if len(entries) == 0 {
				delete(s.byMethod, m)
			} else {
				s.byMethod[m] = entries
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrStubNotFound, id)
}

// ReplaceOrigin atomically replaces every stub of one origin, stamping
// origin on each installed stub and assigning ids to stubs that carry none
// (API documents; file stubs arrive with ID = Source). Entries of other
// origins — and their consumed times budgets — are untouched, so a file
// reload cannot replenish API budgets or vice versa. On a duplicate id the
// store is left unchanged and the error names the id. Returns installed ids
// in input order.
func (s *Store) ReplaceOrigin(origin Origin, stubs []*Compiled) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := make(map[string]string)
	for _, entries := range s.byMethod {
		for _, e := range entries {
			if e.stub.Origin != origin {
				taken[e.stub.ID] = e.stub.Source
			}
		}
	}
	ids := make([]string, 0, len(stubs))
	for _, c := range stubs {
		c.Origin = origin
		if c.ID == "" {
			s.nextID++
			c.ID = fmt.Sprintf("api-%d", s.nextID)
			if origin == OriginAPI {
				c.Source = "api"
			}
		}
		if owner, dup := taken[c.ID]; dup {
			return nil, fmt.Errorf("duplicate stub id %q (already used by %s)", c.ID, owner)
		}
		taken[c.ID] = c.Source
		ids = append(ids, c.ID)
	}
	byMethod := make(map[string][]*entry)
	for m, entries := range s.byMethod {
		for _, e := range entries {
			if e.stub.Origin != origin {
				byMethod[m] = append(byMethod[m], e)
			}
		}
	}
	for _, c := range stubs {
		s.nextSeq++
		byMethod[c.Method] = append(byMethod[c.Method], &entry{stub: c, seq: s.nextSeq})
	}
	for m := range byMethod {
		sortEntries(byMethod[m])
	}
	s.byMethod = byMethod
	return ids, nil
}

// List returns envelopes in deterministic order: methods sorted, entries in
// selection order within a method.
func (s *Store) List(f ListFilter) []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	methods := make([]string, 0, len(s.byMethod))
	for m := range s.byMethod {
		if f.Method == "" || m == f.Method {
			methods = append(methods, m)
		}
	}
	sort.Strings(methods)
	var out []Info
	for _, m := range methods {
		for _, e := range s.byMethod[m] {
			if f.Origin != nil && e.stub.Origin != *f.Origin {
				continue
			}
			out = append(out, Info{
				ID:       e.stub.ID,
				Method:   e.stub.Method,
				Shape:    e.stub.Shape,
				Priority: e.stub.Priority,
				Times:    e.stub.Times,
				Origin:   e.stub.Origin,
				Source:   e.stub.Source,
				Hits:     e.used,
				Document: e.stub.Document,
			})
		}
	}
	return out
}

// ResetStubs drops every API-origin stub and restores file-origin times
// budgets in one atomic step (ControlService.Reset stub semantics).
func (s *Store) ResetStubs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, entries := range s.byMethod {
		kept := entries[:0]
		for _, e := range entries {
			if e.stub.Origin == OriginAPI {
				continue
			}
			e.used = 0
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(s.byMethod, m)
		} else {
			s.byMethod[m] = kept
		}
	}
}
```

Delete `buildIndex` and `Replace`. Add import `errors`. Everything listed as "keep verbatim" in the Files block stays byte-identical.

- [ ] **Step 4: Wire server/ onto the new surface**

`server/server.go:102` — replace

```go
	store := stub.NewStore(stubs)
```

with

```go
	store := stub.NewStore()
	if _, err := store.ReplaceOrigin(stub.OriginFile, stubs); err != nil {
		return nil, err
	}
```

`server/watcher.go:209` (in `reconcileStubDirs`) — replace

```go
	store.Replace(stubs)
```

with

```go
	if _, err := store.ReplaceOrigin(stub.OriginFile, stubs); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		reporter.PrintErrln("stub error:", err)
		return 0, err
	}
```

(the store is untouched on error — same contract the load-error path above it already provides).

- [ ] **Step 5: Mechanically update the remaining test callers**

- `internal/stub/store_test.go`: done in Step 1.
- `internal/dataplane/server_test.go`: the 4 `stub.NewStore(stubs)`-style sites become a local helper identical to `storeWith` but on the `stub.` package (name it `storeWith` too); line 508's `store.Replace(stubs[:1])` becomes `if _, err := store.ReplaceOrigin(stub.OriginFile, stubs[:1]); err != nil { t.Fatal(err) }`. **Caution:** the compiled stubs in that test share sources? If `ReplaceOrigin` reports duplicate ids in any dataplane test, the fixture stubs need distinct `Source` strings — set them where the stubs are compiled, mirroring what `LoadDirs` produces (`"<file>#<index>"`).
- `server/watcher_test.go`: the 4 `stub.NewStore(initial)` sites become the same helper.
- `conformance/harness_test.go:106`: same replacement.

- [ ] **Step 6: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS everywhere. Pay attention to conformance — it compiles stubs with per-scenario sources; distinct by construction, but this run is the proof.

- [ ] **Step 7: Commit**

```bash
git add internal/stub/store.go internal/stub/store_test.go server/server.go server/watcher.go server/watcher_test.go internal/dataplane/server_test.go conformance/harness_test.go
git commit -m "feat(stub): store-owned origins, ids, and per-origin replacement"
```

### Task 7: Journal watch, StubID, and counts

**Files:**
- Modify: `internal/journal/journal.go` (add `StubID` to `Call`; `Len`/`Cap`; broadcast in `Record`)
- Create: `internal/journal/watch.go`, `internal/journal/watch_test.go`
- Modify: `internal/dataplane/server.go` (4 sites: stamp `call.StubID`)
- Extend: `internal/dataplane/server_test.go` (assert `StubID` on a journal entry)

**Interfaces:**
- Consumes: `Compiled.ID` (Task 4).
- Produces: `Call.StubID string`; `(*Journal).Watch(ctx context.Context, method string) *Subscription`; `(*Subscription).Calls() <-chan *Call`, `Close()`, `Err() error`; `var ErrSlowConsumer`; `(*Journal).Len() int`, `(*Journal).Cap() int`. Phase 4 maps `Err() == ErrSlowConsumer` → `RESOURCE_EXHAUSTED`, nil → clean end.

- [ ] **Step 1: Write the failing watch tests**

Create `internal/journal/watch_test.go`:

```go
package journal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func recordCall(j *Journal, method string) {
	j.Record(&Call{Method: method})
}

func TestWatchDeliversInSeqOrder(t *testing.T) {
	j := New(16)
	sub := j.Watch(context.Background(), "")
	defer sub.Close()
	for i := 0; i < 5; i++ {
		recordCall(j, "/a.B/C")
	}
	var last uint64
	for i := 0; i < 5; i++ {
		select {
		case call := <-sub.Calls():
			if call.Seq <= last {
				t.Fatalf("out of order: %d after %d", call.Seq, last)
			}
			last = call.Seq
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for call")
		}
	}
}

func TestWatchFiltersByMethodBeforeBuffering(t *testing.T) {
	j := New(16)
	sub := j.Watch(context.Background(), "a.B/Wanted") // unnormalized on purpose
	defer sub.Close()
	// Flood with unrelated traffic far past the buffer size: the filter runs
	// before the buffered send, so this must not evict the subscriber.
	for i := 0; i < watchBuffer*3; i++ {
		recordCall(j, "/a.B/Noise")
	}
	recordCall(j, "/a.B/Wanted")
	select {
	case call := <-sub.Calls():
		if call.Method != "/a.B/Wanted" {
			t.Fatalf("method = %q", call.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber was starved or dropped by unrelated traffic")
	}
	if err := sub.Err(); err != nil {
		t.Fatalf("Err = %v, want nil (never fell behind on its own method)", err)
	}
}

func TestSlowConsumerIsDroppedWithErrSlowConsumer(t *testing.T) {
	j := New(4)
	sub := j.Watch(context.Background(), "")
	for i := 0; i < watchBuffer+1; i++ { // one past the buffer with no reader
		recordCall(j, "/a.B/C")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-sub.Calls():
			if !ok {
				if !errors.Is(sub.Err(), ErrSlowConsumer) {
					t.Fatalf("Err = %v, want ErrSlowConsumer", sub.Err())
				}
				return
			}
		case <-deadline:
			t.Fatal("channel never closed after overflow")
		}
	}
}

func TestCloseEndsStreamWithNilErr(t *testing.T) {
	j := New(4)
	sub := j.Watch(context.Background(), "")
	sub.Close()
	if _, ok := <-sub.Calls(); ok {
		t.Fatal("channel still open after Close")
	}
	if sub.Err() != nil {
		t.Fatalf("Err = %v, want nil after clean Close", sub.Err())
	}
	sub.Close() // idempotent
	recordCall(j, "/a.B/C") // must not panic on a closed subscription
}

func TestContextCancelEndsStreamWithNilErr(t *testing.T) {
	j := New(4)
	ctx, cancel := context.WithCancel(context.Background())
	sub := j.Watch(ctx, "")
	cancel()
	select {
	case _, ok := <-sub.Calls():
		if ok {
			t.Fatal("got a call, want close")
		}
	case <-time.After(time.Second):
		t.Fatal("channel never closed after ctx cancel")
	}
	if sub.Err() != nil {
		t.Fatalf("Err = %v, want nil", sub.Err())
	}
}

// The context bridge must terminate on explicit Close even when the context
// lives on (design §4: a Background-context tail must not leak a goroutine).
func TestCloseTerminatesContextBridge(t *testing.T) {
	j := New(4)
	sub := j.Watch(context.Background(), "")
	done := make(chan struct{})
	go func() {
		<-sub.bridgeDone() // test-only accessor, see watch.go
		close(done)
	}()
	sub.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge goroutine did not exit after Close under a live context")
	}
}

// Race pair 1: consumer Close against writer-side eviction.
func TestCloseRacesEviction(t *testing.T) {
	for i := 0; i < 100; i++ {
		j := New(4)
		sub := j.Watch(context.Background(), "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 0; k < watchBuffer+8; k++ {
				recordCall(j, "/a.B/C")
			}
		}()
		go func() {
			defer wg.Done()
			sub.Close()
		}()
		wg.Wait()
	}
}

// Race pair 2: context cancellation against a concurrent broadcast — the
// send-versus-close hazard (design §4).
func TestCancelRacesBroadcast(t *testing.T) {
	for i := 0; i < 100; i++ {
		j := New(4)
		ctx, cancel := context.WithCancel(context.Background())
		sub := j.Watch(ctx, "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 0; k < 16; k++ {
				recordCall(j, fmt.Sprintf("/a.B/C%d", k))
			}
		}()
		go func() {
			defer wg.Done()
			cancel()
		}()
		wg.Wait()
		<-sub.Calls() // drain until close is fine; just ensure no panic
		_ = sub.Err()
	}
}

func TestLenAndCap(t *testing.T) {
	j := New(3)
	if j.Cap() != 3 || j.Len() != 0 {
		t.Fatalf("Cap=%d Len=%d, want 3, 0", j.Cap(), j.Len())
	}
	for i := 0; i < 5; i++ {
		recordCall(j, "/a.B/C")
	}
	if j.Len() != 3 {
		t.Fatalf("Len = %d after overflow, want 3", j.Len())
	}
	j.Reset()
	if j.Len() != 0 || j.Cap() != 3 {
		t.Fatalf("after Reset: Len=%d Cap=%d, want 0, 3", j.Len(), j.Cap())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/journal/ -run 'TestWatch|TestSlow|TestClose|TestContext|TestCancel|TestLenAndCap' -v`

Expected: compile error — `j.Watch` undefined.

- [ ] **Step 3: Implement watch.go and the journal edits**

Create `internal/journal/watch.go`:

```go
package journal

import (
	"context"
	"errors"
)

// watchBuffer is each subscriber's channel depth. A subscriber that falls
// this far behind is dropped (design §4).
const watchBuffer = 64

// ErrSlowConsumer reports a subscriber evicted for falling watchBuffer calls
// behind (RESOURCE_EXHAUSTED at the admin surface: reconnect).
var ErrSlowConsumer = errors.New("journal: subscriber fell behind and was dropped")

// Subscription is one live tail of the journal. Calls returns the stream
// channel; once it closes, Err reports why: nil for a clean Close or context
// cancellation, ErrSlowConsumer for an eviction. Delivered calls are the
// journal's own retained snapshots and must be treated as read-only.
type Subscription struct {
	journal *Journal
	method  string
	calls   chan *Call
	done    chan struct{}
	err     error // written under journal.mu before calls closes
}

// Watch subscribes to calls recorded from now on, optionally filtered to one
// method ("" means all; the filter runs before buffering, so unrelated
// traffic cannot evict a filtered subscriber). Canceling ctx closes the
// subscription; so does Close.
func (j *Journal) Watch(ctx context.Context, method string) *Subscription {
	s := &Subscription{
		journal: j,
		method:  normalizeMethod(method),
		calls:   make(chan *Call, watchBuffer),
		done:    make(chan struct{}),
	}
	j.mu.Lock()
	if j.subs == nil {
		j.subs = make(map[*Subscription]struct{})
	}
	j.subs[s] = struct{}{}
	j.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.done:
		}
	}()
	return s
}

// Calls is the stream channel; it closes when the subscription ends.
func (s *Subscription) Calls() <-chan *Call { return s.calls }

// Err reports why Calls closed. Read it only after Calls is closed.
func (s *Subscription) Err() error { return s.err }

// Close ends the subscription. Idempotent; Err stays nil on this path.
func (s *Subscription) Close() {
	s.journal.mu.Lock()
	s.closeLocked(nil)
	s.journal.mu.Unlock()
}

// closeLocked finalizes the subscription; the journal's write lock must be
// held. Membership in journal.subs is the "still open" flag, which makes
// close idempotent and — because every send in Record holds the same lock —
// makes send-on-closed-channel impossible by construction (design §4).
func (s *Subscription) closeLocked(err error) {
	if _, open := s.journal.subs[s]; !open {
		return
	}
	delete(s.journal.subs, s)
	s.err = err
	close(s.calls)
	close(s.done)
}

// bridgeDone exposes the internal done channel so tests can assert the
// context-bridge goroutine terminates on Close under a live context.
func (s *Subscription) bridgeDone() <-chan struct{} { return s.done }

// broadcastLocked delivers one retained call to every live subscriber; the
// journal's write lock must be held. Sends are non-blocking: a full buffer
// evicts the subscriber with ErrSlowConsumer. Deleting from the map during
// its own range is allowed in Go.
func (j *Journal) broadcastLocked(retained *Call) {
	for s := range j.subs {
		if s.method != "" && retained.Method != s.method {
			continue
		}
		select {
		case s.calls <- retained:
		default:
			s.closeLocked(ErrSlowConsumer)
		}
	}
}
```

In `internal/journal/journal.go`:

- `Call` gains `StubID string` after `StubSource` (`cloneCall`'s struct copy already covers it).
- `Journal` gains a field: `subs map[*Subscription]struct{}` (document: guarded by mu, like everything else).
- `Record` broadcasts the retained snapshot as its last act, still under the lock — after `j.entries[index] = retained` add:

```go
	j.broadcastLocked(retained)
```

- Append:

```go
// Len reports how many calls the ring currently retains.
func (j *Journal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.count
}

// Cap reports the ring capacity, fixed at construction.
func (j *Journal) Cap() int { return j.cap }
```

- [ ] **Step 4: Run the journal package under race**

Run: `go test ./internal/journal/ -race -v`

Expected: all PASS, no race reports, including the two dedicated race-pair tests.

- [ ] **Step 5: Stamp StubID in the dataplane**

In `internal/dataplane/server.go`, at each of the four sites that set `call.StubSource = selected.Source` (lines ~138, ~201, ~219, ~237), add beside it:

```go
	call.StubID = selected.ID
```

In `internal/dataplane/server_test.go`, find the existing unary-call test that asserts the journal entry (`StubSource`) and extend the assertion: after Task 6's `storeWith` construction, file-origin stubs carry `ID == Source`, so assert `entry.StubID == entry.StubSource` and non-empty. If no existing test asserts `StubSource`, add the check to the journal assertions of the main unary stub test.

- [ ] **Step 6: Run the full suite and commit**

Run: `go build ./... && go vet ./... && go test ./...` — expected: PASS.

```bash
git add internal/journal/journal.go internal/journal/watch.go internal/journal/watch_test.go internal/dataplane/server.go internal/dataplane/server_test.go
git commit -m "feat(journal): watch subscriptions with slow-consumer eviction and StubID"
```

### Task 8: Live reflection, startup validation, and phase gates

**Files:**
- Extend: `internal/dataplane/server_test.go` (runtime-reflectability test)
- Extend: `server/server_test.go` (duplicate-root startup failure)

**Interfaces:**
- Consumes: `RegisterSet` (Task 2), the live `DescriptorResolver: reg` wiring (landed in Task 1 Step 6), `NewStore()`/`ReplaceOrigin` startup path (Task 6).
- Produces: nothing; closing tests and the phase-complete gate.

- [ ] **Step 1: Write the runtime-reflectability test**

Append to `internal/dataplane/server_test.go`, modeled on the existing `TestReflectionListsAndResolvesServices` (same stream client; the helper `startServer` returns the registry):

```go
// A service registered at runtime must become reflectable immediately: the
// reflection server holds the registry itself, not a startup snapshot
// (design §2.5). Nothing could register at runtime before this phase, so no
// earlier test covers it.
func TestReflectionSeesRuntimeRegisteredService(t *testing.T) {
	reg, conn, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String("late.proto"),
		Package: proto.String("late.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Ping")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("LateService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("Ping"),
				InputType:  proto.String(".late.v1.Ping"),
				OutputType: proto.String(".late.v1.Ping"),
			}},
		}},
	}}}
	if _, err := reg.RegisterSet(set); err != nil {
		t.Fatal(err)
	}

	rc := v1reflectionpb.NewServerReflectionClient(conn)
	strm, err := rc.ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := strm.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: "late.v1.LateService",
		},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := strm.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetFileDescriptorResponse().GetFileDescriptorProto()) == 0 {
		t.Fatal("runtime-registered service is not reflectable")
	}
}
```

Add imports as needed: `proto "google.golang.org/protobuf/proto"`, `google.golang.org/protobuf/types/descriptorpb`.

- [ ] **Step 2: Write the duplicate-root startup test**

Append to `server/server_test.go` (same option pattern as `TestStartServesStubbedUnaryCall`):

```go
// Passing the same stub root twice must fail at startup with the
// duplicate-id error — the path that bypassed validation when NewStore took
// stubs directly (design §3.4).
func TestStartRejectsDuplicateStubRoots(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir, dir},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 16,
	})
	if err == nil {
		t.Fatal("Start with a duplicated stub root succeeded, want duplicate-id error")
	}
	if !strings.Contains(err.Error(), "duplicate stub id") {
		t.Fatalf("err = %v, want a duplicate-stub-id failure", err)
	}
}
```

(import `strings` if absent.)

- [ ] **Step 3: Run both new tests**

Run: `go test ./internal/dataplane/ -run TestReflectionSeesRuntime -v && go test ./server/ -run TestStartRejectsDuplicate -v`

Expected: both PASS (the wiring already landed in Tasks 1 and 6; these are the closing proofs). If `TestStartRejectsDuplicateStubRoots` fails because the same root twice yields the same walk paths and therefore the same IDs *within one origin batch* — that is exactly what `ReplaceOrigin`'s `taken` map must catch for stubs inside the batch; if it only checks against other origins, fix `ReplaceOrigin` (the Task 6 code seeds `taken` with other origins and then adds each new id as it validates, so in-batch duplicates are caught — verify against that code).

- [ ] **Step 4: Full phase gates**

Run, in order:

```
go build ./... && go vet ./...
go test ./...
go test ./internal/schema/ ./internal/stub/ ./internal/journal/ ./internal/dataplane/ ./server/ -race
go test ./conformance/ -count=1
```

Expected: every command passes. The race leg covers all packages this phase touched.

- [ ] **Step 5: Commit**

```bash
git add internal/dataplane/server_test.go server/server_test.go
git commit -m "test: runtime reflectability and duplicate-root startup validation"
```

Phase 3 is complete when: the registry mutates copy-on-write with `RegisterSet` idempotent across toolchains; the store owns origins/IDs/documents with per-origin replacement; the journal supports watch subscriptions with typed termination; and every gate in Step 4 passes. Phase 4 (`internal/admin` handlers + h2c + `/healthz` + `serve --admin`) consumes exactly the surfaces named in the Interfaces blocks above.

---

## Self-review

**Spec coverage** (against `2026-08-21-m3-p3-core-extensions-design.md`):

- §2.2–2.4 (snapshot, apply, mutator table) → Task 1. `RegisterSet` + §2.6 semantics → Task 2. ✓
- §2.5 read-surface split → Task 1 (Snapshot rename, live resolver wiring, live Types). ✓
- §3.1 `Compiled` fields and normalized documents → Task 4 (node-render, with the recorded deviation on `ParseDocument`'s signature). ✓
- §3.2 shared decode boundary, empty-input divergence → Tasks 4–5. ✓
- §3.3 `CompileMatch` → Task 5. ✓
- §3.4 store surface, stamping, ID rule, `NewStore()`, tie-break, cross-origin preservation, hits → Task 6. ✓
- §3.5 `RenderSequence` + round trip → Task 4. ✓
- §4 journal watch incl. locking discipline and the context-bridge `done` channel → Task 7. ✓
- §5 wiring table → Task 1 Step 6 (resolver), Task 4 Step 5 (loader), Task 6 Steps 4–5 (server/watcher/NewStore), Task 7 Step 5 (StubID). ✓
- §7 test list → every named test maps to a task step; the design's "WKT idempotency" and "concurrent-writers union" and "bridge leak" tests are Tasks 2, 3, 7. ✓

**Placeholder scan:** no TBD/TODO. Two deliberate soft references exist — store-test helper names (`compiledForTest`, `matchingInput`, `testRegistry`) are placeholders *by instruction*: the steps direct the implementer to read the file and use its real helpers, because inventing exact names for helpers this plan does not rewrite would be the drift the No-Placeholders rule exists to prevent. Every new symbol this plan itself introduces is fully specified.

**Type consistency:** `ParseDocument (Stub, string, error)` matches its uses in Tasks 4 (loader round trip) and the Interfaces blocks; `ReplaceOrigin ([]string, error)` consistent across Tasks 6 and 8; `Subscription.Err()`/`ErrSlowConsumer` consistent across Task 7 and the design's Phase 4 mapping; `RegisterSet ([]string, error)` consistent across Tasks 2, 3, 8. `storeWith` appears in Tasks 6 and 7 with the same shape. Registry helpers `apply`/`addNew` defined in Task 1, consumed in Task 2 with matching signatures.
