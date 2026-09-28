# Simulacra M3 Phase 6 — Reflection Schema Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `simulacra schema import --reflect host:port -o schema.binpb` — walk a running gRPC server's reflection endpoint and write a self-contained, topologically ordered `FileDescriptorSet`.

**Architecture:** A new package `internal/schema/upstream` holds the two testable halves: a pure `Closure` algorithm over a `Fetcher` interface, and a reflection transport implementing `Fetcher` over one `ServerReflectionInfo` stream with a v1→v1alpha fallback. `internal/cli/schema_import.go` is a thin cobra command over them. Nothing in `server/`, `internal/admin/`, `internal/dataplane/`, `internal/stub/`, `internal/match/` or `internal/journal/` changes; `internal/schema` gains one exported function (Task 7).

**Tech Stack:** Go 1.25.0, module `github.com/yinghanhung/simulacra`. `google.golang.org/grpc` v1.82.0, `google.golang.org/protobuf` v1.36.11, `github.com/spf13/cobra` v1.10.2 are already direct dependencies. **No module is added** — `grpc_reflection_v1`, `grpc_reflection_v1alpha`, `credentials/insecure` and `test/bufconn` all ship inside grpc-go.

**Spec:** `docs/superpowers/specs/2026-09-20-m3-p6-reflection-import-design.md`, cited as "design §N". Approved at `f69f70c`. Phase 5 is complete at `26e6158`.

## Global Constraints

- **No new modules.** `go.mod` and `go.sum` stay unchanged. If a step seems to need a dependency, stop and re-read — it does not.
- **No changes to `api/` or `gen/`.** The admin contract was frozen in Phase 2. This phase adds no RPC.
- **The only files touched outside `internal/schema/upstream` and `internal/cli`** are `internal/schema/registry.go` and `server/server.go` — one exported wrapper each, both in Task 7. No behavior in either package changes.
- **`--out`/`-o` is the destination file; `--output` is the format.** `outputFlag` deliberately has no shorthand — see the comment at `internal/cli/output.go:18`. Do not add one.
- **Server and upstream diagnostics are surfaced verbatim.** A command may prefix its own source but never rewords or re-classifies what the upstream said (design §6, M3 §11).
- **Every test that discriminates carries a named mutation check** — state the mutation, confirm the test fails under it, revert. Carried over from the phase 5 plan's global constraints.
- **Run the full suite with `-count=1`.** Go's test cache will otherwise hide a real regression.
- **No `time.Sleep` to establish ordering a test's assertion depends on.** Synchronize on something real.
  **Executing Task 6 proved this constraint's earlier carve-out wrong and it has been withdrawn.** The
  carve-out permitted a sleep that "only widens the window," on the reasoning that every interleaving
  would still produce the asserted outcome. For a signal test that is false: there is a window *before
  the child installs its signal handler* in which the signal kills it by the default disposition, and
  `exec` then reports **exit status -1**, not the asserted 2. Both subtests failed deterministically on
  a cold build. A subprocess signal test therefore needs a real readiness proof — see Task 6, and
  `internal/cli/signal_test.go:127`, where phase 5 recorded the same lesson for the tail tests.

---

## Pre-verified facts

Established by throwaway probes before this plan was written, and since deleted. Each is carried from design §3; the tasks that depend on one cite it.

**F1 — one transport covers both reflection versions.** A `grpc.ClientConn.NewStream` to the **v1alpha** path carrying **`grpc_reflection_v1` Go message types** works against a v1alpha-only server. There are no v1alpha Go types in this codebase and no adapter pair.

**F2 — `SendMsg` is not a reliable error channel.** Against a v1alpha-only server on the v1 path, `SendMsg` is timing-dependent: at 0s delay it returned `nil`; at 50ms and 250ms it returned **`io.EOF`**. `RecvMsg` returned `Unimplemented` in all three. This matches grpc-go's documented `SendMsg` contract.

**F3 — the fallback is unreachable against ourselves.** Simulacra's data plane registers **both** v1 and v1alpha (`internal/dataplane/server.go:60-61`), so v1 always wins. The fallback needs a stock grpc-go server with only `reflection.NewServer(opts)` registered.

**F4 — grpc-go returns the transitive closure in one response.** `FileContainingSymbol("shop.v1.OrderService")` returned 3 files: `shop/v1/order.proto` plus both of its well-known-type imports. This is grpc-go behavior, not a protocol guarantee — the dependency recursion is a correctness safety net, not the primary path.

**F5 — Simulacra does not advertise its own reflection services.** A stock grpc-go server lists `grpc.reflection.v1alpha.ServerReflection` in `ListServices`; Simulacra's custom `ServiceInfoProvider` (`internal/dataplane/server.go:65`) lists registry services plus health only.

**F6 — an upstream health descriptor differing from ours fails the whole set.** Our compiled-in health exposes `Check`, `List`, `Watch`. A simulated upstream missing one RPC produced `file "grpc/health/v1/health.proto" is already registered with different content` from `RegisterSet`, rejecting the entire import. `List` is a recent grpc-go addition, so older and non-Go upstreams hit this routinely.

**F7 — `Header()` is a deterministic rendezvous that suppresses the error.** grpc-go's `Header()` waits for the terminal status, calls `cs.finish(err)`, and returns `(nil, nil)`, commented *"Do not return the error. The user should get it by calling Recv()"* (`stream.go:875-878`, pinned v1.82.0). Measured: after `Header()` returns, `SendMsg` is `io.EOF` **100/100 iterations over both TCP and bufconn**, across three runs. `ClientStream.Context()` is *not* a rendezvous — it does not complete until `RecvMsg` consumes the status.

---

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/schema/upstream/closure.go` | `Fetcher` interface, `SkippedServices`, `Closure` | 1, 2 |
| `internal/schema/upstream/closure_test.go` | Closure tests against a fake `Fetcher` — no network | 1, 2 |
| `internal/schema/upstream/client.go` | `StreamOpener`, reflection transport, version fallback, `Dial` | 3, 4 |
| `internal/schema/upstream/client_test.go` | Fake-stream seam tests + mutation check | 3 |
| `internal/schema/upstream/wire_test.go` | Real-server tests: v1, v1alpha-only, `Header()` rendezvous | 4 |
| `internal/cli/schema_import.go` | The cobra command, flags, atomic write, output | 5 |
| `internal/cli/schema_import_test.go` | Command tests, cancellation commit boundary | 5 |
| `internal/cli/schema_import_signal_test.go` | Built-binary SIGINT/SIGTERM tests | 6 |
| `internal/cli/schema_import_roundtrip_test.go` | Dogfood round-trip + health skip | 7 |
| `internal/schema/registry.go` | **Modify:** one exported wrapper, `NormalizeFileProto` | 7 |
| `server/server.go` | **Modify:** one accessor, `Registry()` | 7 |
| `internal/cli/schema.go` | **Modify:** register the new subcommand | 5 |

---

### Task 1: Closure core — services to a topologically ordered set

**Files:**
- Create: `internal/schema/upstream/closure.go`
- Test: `internal/schema/upstream/closure_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Fetcher interface{ ListServices(context.Context) ([]string, error); FileContainingSymbol(context.Context, string) ([][]byte, error); FileByFilename(context.Context, string) ([][]byte, error) }`; `var SkippedServices []string`; `func Closure(ctx context.Context, f Fetcher) (*descriptorpb.FileDescriptorSet, error)`.

> **Scope:** this task handles the case F4 describes — the upstream volunteers a file's dependencies alongside it. Fetching dependencies the upstream withheld, conflict detection and the cycle guard are **Task 2**, driven by their own tests. Do not write them here; `FileByFilename` is part of the `Fetcher` interface but goes unused until Task 2.

- [ ] **Step 1: Write the failing test**

Create `internal/schema/upstream/closure_test.go`:

```go
package upstream

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fakeFetcher is an in-memory upstream. files maps a filename to the
// descriptor served for it; symbols maps a service name to the filenames
// returned for FileContainingSymbol, mimicking F4 (a server may return the
// closure, or just the one file).
type fakeFetcher struct {
	services []string
	symbols  map[string][]string
	files    map[string]*descriptorpb.FileDescriptorProto
	// calls records every FileByFilename lookup, so a test can prove a
	// diamond dependency was fetched once rather than twice.
	calls []string
}

func (f *fakeFetcher) ListServices(context.Context) ([]string, error) {
	return f.services, nil
}

func (f *fakeFetcher) FileContainingSymbol(_ context.Context, sym string) ([][]byte, error) {
	return f.marshal(f.symbols[sym])
}

func (f *fakeFetcher) FileByFilename(_ context.Context, name string) ([][]byte, error) {
	f.calls = append(f.calls, name)
	return f.marshal([]string{name})
}

func (f *fakeFetcher) marshal(names []string) ([][]byte, error) {
	var out [][]byte
	for _, n := range names {
		fd, ok := f.files[n]
		if !ok {
			continue // an upstream that does not have the file returns nothing
		}
		raw, err := proto.Marshal(fd)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// file is a terse FileDescriptorProto builder for the tests.
func file(name string, deps ...string) *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:       proto.String(name),
		Dependency: deps,
	}
}

// names renders a set as its filenames, in emitted order.
func names(set *descriptorpb.FileDescriptorSet) []string {
	var out []string
	for _, f := range set.GetFile() {
		out = append(out, f.GetName())
	}
	return out
}

func TestClosureEmitsDependenciesBeforeDependents(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"shop.v1.OrderService"},
		symbols:  map[string][]string{"shop.v1.OrderService": {"order.proto", "ts.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"order.proto": file("order.proto", "ts.proto"),
			"ts.proto":    file("ts.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	got := names(set)
	want := []string{"ts.proto", "order.proto"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (dependencies must precede dependents)", got, want)
		}
	}
}

func TestClosureSkipsServicesTheDataPlaneImplements(t *testing.T) {
	f := &fakeFetcher{
		services: []string{
			"shop.v1.OrderService",
			"grpc.reflection.v1.ServerReflection",
			"grpc.reflection.v1alpha.ServerReflection",
			"grpc.health.v1.Health",
		},
		symbols: map[string][]string{
			"shop.v1.OrderService":                     {"order.proto"},
			"grpc.reflection.v1.ServerReflection":      {"reflection.proto"},
			"grpc.reflection.v1alpha.ServerReflection": {"reflection_alpha.proto"},
			"grpc.health.v1.Health":                    {"health.proto"},
		},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"order.proto":            file("order.proto"),
			"reflection.proto":       file("reflection.proto"),
			"reflection_alpha.proto": file("reflection_alpha.proto"),
			"health.proto":           file("health.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	got := names(set)
	if len(got) != 1 || got[0] != "order.proto" {
		t.Fatalf("got %v, want only [order.proto]; reflection and health are "+
			"implemented by the data plane and cannot be mocked (design §4)", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/schema/upstream/ -run TestClosure -v -count=1`
Expected: FAIL — the package does not exist yet (`no Go files in .../upstream`).

- [ ] **Step 3: Write the minimal implementation**

Create `internal/schema/upstream/closure.go`:

```go
// Package upstream imports descriptor sets from a running gRPC server's
// reflection endpoint.
//
// It has two halves that are deliberately separable: Closure is a pure walk
// over a Fetcher, testable with no network at all, and client.go implements
// Fetcher over a real reflection stream.
package upstream

import (
	"context"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Fetcher retrieves descriptor bytes from an upstream server. Every method
// returns serialized FileDescriptorProto messages, and a single call may
// return more than one: grpc-go answers FileContainingSymbol with the whole
// transitive closure (F4). That is an implementation's choice, not a
// guarantee, which is why Task 2 makes Closure resolve what is missing.
type Fetcher interface {
	ListServices(ctx context.Context) ([]string, error)
	FileContainingSymbol(ctx context.Context, symbol string) ([][]byte, error)
	FileByFilename(ctx context.Context, name string) ([][]byte, error)
}

// SkippedServices are the services Simulacra's data plane implements itself.
//
// This is a rule, not a list: all three are registered on our grpc.Server
// ahead of UnknownServiceHandler (internal/dataplane/server.go:44-61), so a
// stub for any of them is unreachable — importing them cannot enable mocking
// them, it only adds noise to every `schema list` thereafter.
//
// Health additionally breaks the import outright (F6). The data plane
// pre-registers its own compiled-in grpc/health/v1/health.proto, and
// RegisterSet rejects a same-path file whose content differs; because
// registration is all-or-nothing, one disagreeing upstream health descriptor
// fails the entire set and names a service the user never asked to import.
//
// Skipping happens at service enumeration only. A file that arrives as a
// genuine transitive dependency is kept — dropping it would produce a
// non-self-contained set, which RegisterSchemas rejects for a different
// reason (design §4, §9).
var SkippedServices = []string{
	"grpc.reflection.v1.ServerReflection",
	"grpc.reflection.v1alpha.ServerReflection",
	"grpc.health.v1.Health",
}

// Closure walks the upstream and returns a FileDescriptorSet in topological
// order: every file appears after the files it imports, which is what protoc
// --include_imports and buf build -o produce, and what our own
// RegisterSchemas requires.
func Closure(ctx context.Context, f Fetcher) (*descriptorpb.FileDescriptorSet, error) {
	services, err := f.ListServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the upstream's services: %w", err)
	}

	c := &closure{
		f:      f,
		byName: map[string]*descriptorpb.FileDescriptorProto{},
		done:   map[string]bool{},
	}

	skip := map[string]bool{}
	for _, s := range SkippedServices {
		skip[s] = true
	}
	for _, svc := range services {
		if skip[svc] {
			continue
		}
		raw, err := f.FileContainingSymbol(ctx, svc)
		if err != nil {
			return nil, fmt.Errorf("fetching the file containing %s: %w", svc, err)
		}
		if err := c.absorb(raw); err != nil {
			return nil, err
		}
	}
	return c.emitAll()
}

// closure is the walk's mutable state.
type closure struct {
	f      Fetcher
	byName map[string]*descriptorpb.FileDescriptorProto
	out    []*descriptorpb.FileDescriptorProto
	done   map[string]bool
}

// absorb decodes descriptors and files them by path, first one winning.
func (c *closure) absorb(raw [][]byte) error {
	for _, b := range raw {
		fd := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(b, fd); err != nil {
			return fmt.Errorf("parsing a descriptor returned by the upstream: %w", err)
		}
		name := fd.GetName()
		if _, ok := c.byName[name]; ok {
			continue
		}
		c.byName[name] = fd
	}
	return nil
}

// emitAll walks every absorbed file in sorted order, emitting dependencies
// first. Sorted so the output is deterministic rather than dependent on map
// iteration order.
func (c *closure) emitAll() (*descriptorpb.FileDescriptorSet, error) {
	roots := make([]string, 0, len(c.byName))
	for name := range c.byName {
		roots = append(roots, name)
	}
	sort.Strings(roots)
	for _, name := range roots {
		if err := c.emit(name); err != nil {
			return nil, err
		}
	}
	return &descriptorpb.FileDescriptorSet{File: c.out}, nil
}

// emit is the post-order DFS: a file is appended only after every file it
// imports has been.
func (c *closure) emit(name string) error {
	if c.done[name] {
		return nil
	}
	fd, ok := c.byName[name]
	if !ok {
		return fmt.Errorf("the upstream did not provide %q, which another file imports", name)
	}
	for _, dep := range fd.GetDependency() {
		if err := c.emit(dep); err != nil {
			return err
		}
	}
	c.done[name] = true
	c.out = append(c.out, fd)
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/schema/upstream/ -run TestClosure -v -count=1`
Expected: PASS — both `TestClosureEmitsDependenciesBeforeDependents` and `TestClosureSkipsServicesTheDataPlaneImplements`.

- [ ] **Step 5: Mutation check**

Confirm the ordering test discriminates. Temporarily comment out the dependency loop in `emit`, so files are appended without recursing into their imports first:

```go
	// MUTATION: skip the dependency recursion
	// for _, dep := range fd.GetDependency() {
	// 	if err := c.emit(dep); err != nil { return err }
	// }
```

Run: `go test ./internal/schema/upstream/ -run TestClosureEmitsDependenciesBeforeDependents -count=1`
Expected: FAIL with `got [order.proto ts.proto], want [ts.proto order.proto]`. **Revert the mutation** and re-run to confirm PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/schema/upstream/closure.go internal/schema/upstream/closure_test.go
git commit -m "feat(upstream): topologically ordered descriptor closure with a skip rule"
```

---

### Task 2: Closure robustness — recursion, diamonds, conflicts, cycles

**Files:**
- Modify: `internal/schema/upstream/closure.go`
- Test: `internal/schema/upstream/closure_test.go` (append)

**Interfaces:**
- Consumes: `Closure`, `Fetcher`, `closure`, `fakeFetcher`, `file`, `names` from Task 1.
- Produces: no new exported API. `Closure`'s guarantee strengthens from "topologically ordered" to "**self-contained**, topologically ordered": it now fetches dependencies the upstream withheld, rejects contradictory descriptors, and reports cycles.

- [ ] **Step 1: Write the failing tests**

Append to `internal/schema/upstream/closure_test.go`, and add `"strings"` to its import block:

```go
// An upstream that returns only the named file, never its dependencies —
// the opposite of grpc-go's behavior (F4), and the reason the recursion exists.
func TestClosureFetchesDependenciesTheUpstreamWithheld(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"shop.v1.OrderService"},
		symbols:  map[string][]string{"shop.v1.OrderService": {"order.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"order.proto": file("order.proto", "ts.proto"),
			"ts.proto":    file("ts.proto", "base.proto"),
			"base.proto":  file("base.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	got := names(set)
	want := []string{"base.proto", "ts.proto", "order.proto"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A diamond: two files importing one shared dependency. It must be fetched
// once and emitted once.
func TestClosureFetchesADiamondDependencyOnce(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"a.A", "b.B"},
		symbols: map[string][]string{
			"a.A": {"a.proto"},
			"b.B": {"b.proto"},
		},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"a.proto":      file("a.proto", "shared.proto"),
			"b.proto":      file("b.proto", "shared.proto"),
			"shared.proto": file("shared.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	var shared int
	for _, n := range names(set) {
		if n == "shared.proto" {
			shared++
		}
	}
	if shared != 1 {
		t.Fatalf("shared.proto emitted %d times, want 1; got %v", shared, names(set))
	}
	var fetches int
	for _, c := range f.calls {
		if c == "shared.proto" {
			fetches++
		}
	}
	if fetches != 1 {
		t.Fatalf("shared.proto fetched %d times, want exactly 1 (calls: %v)", fetches, f.calls)
	}
}

// An upstream that does not have a file something imports. The error must
// name both the missing file and who wanted it.
func TestClosureMissingDependencyNamesTheImporter(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"a.A"},
		symbols:  map[string][]string{"a.A": {"a.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"a.proto": file("a.proto", "gone.proto"),
		},
	}
	_, err := Closure(context.Background(), f)
	if err == nil {
		t.Fatal("Closure succeeded, want an error naming the missing dependency")
	}
	for _, want := range []string{"gone.proto", "a.proto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// conflictFetcher answers two services with two different definitions of one
// filename — something fakeFetcher's single files map cannot express.
type conflictFetcher struct {
	first, second *descriptorpb.FileDescriptorProto
}

func (c *conflictFetcher) ListServices(context.Context) ([]string, error) {
	return []string{"a.A", "b.B"}, nil
}

func (c *conflictFetcher) FileContainingSymbol(_ context.Context, sym string) ([][]byte, error) {
	fd := c.first
	if sym == "b.B" {
		fd = c.second
	}
	raw, err := proto.Marshal(fd)
	if err != nil {
		return nil, err
	}
	return [][]byte{raw}, nil
}

func (c *conflictFetcher) FileByFilename(context.Context, string) ([][]byte, error) {
	return nil, nil
}

// Two different definitions of one path from one server.
func TestClosureConflictingDefinitionsAreAnError(t *testing.T) {
	conflicting := file("dup.proto")
	conflicting.Package = proto.String("second")
	_, err := Closure(context.Background(), &conflictFetcher{
		first:  file("dup.proto"),
		second: conflicting,
	})
	if err == nil {
		t.Fatal("Closure succeeded, want an error naming the conflicting file")
	}
	if !strings.Contains(err.Error(), "dup.proto") {
		t.Fatalf("error %q does not name the conflicting file", err)
	}
}

// Proto forbids circular imports, so this can only come from a broken
// upstream — but it must be reported rather than recursed into forever.
func TestClosureDetectsAnImportCycle(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"a.A"},
		symbols:  map[string][]string{"a.A": {"a.proto", "b.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"a.proto": file("a.proto", "b.proto"),
			"b.proto": file("b.proto", "a.proto"),
		},
	}
	_, err := Closure(context.Background(), f)
	if err == nil {
		t.Fatal("Closure succeeded on a cyclic import graph, want an error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not report a cycle", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/schema/upstream/ -run TestClosure -v -count=1 -timeout 60s`
Expected: the two Task 1 tests PASS; the five new ones FAIL. Specifically:
- `TestClosureFetchesDependenciesTheUpstreamWithheld` — `the upstream did not provide "ts.proto"`
- `TestClosureFetchesADiamondDependencyOnce` — same shape, `shared.proto`
- `TestClosureMissingDependencyNamesTheImporter` — fails the "names the importer" assertion (the Task 1 message names only the missing file)
- `TestClosureConflictingDefinitionsAreAnError` — `Closure succeeded`
- `TestClosureDetectsAnImportCycle` — **stack overflow or timeout**, not a clean failure. That is the bug this task fixes.

- [ ] **Step 3: Add conflict detection to `absorb`**

Replace `absorb` in `internal/schema/upstream/closure.go`, and add an `importer` field to the `closure` struct:

```go
// closure is the walk's mutable state.
type closure struct {
	f      Fetcher
	byName map[string]*descriptorpb.FileDescriptorProto
	// importer records which file first named a dependency, so a missing one
	// can say who wanted it.
	importer map[string]string
	out      []*descriptorpb.FileDescriptorProto
	done     map[string]bool
	visiting map[string]bool
}

// absorb decodes descriptors and files them by path, first one winning.
func (c *closure) absorb(raw [][]byte) error {
	for _, b := range raw {
		fd := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(b, fd); err != nil {
			return fmt.Errorf("parsing a descriptor returned by the upstream: %w", err)
		}
		name := fd.GetName()
		if prior, ok := c.byName[name]; ok {
			// One server is one source of truth. Two different definitions of
			// one path is an upstream bug, and saying so is more useful than
			// silently keeping whichever arrived first.
			if !proto.Equal(prior, fd) {
				return fmt.Errorf(
					"the upstream returned two different definitions of %q; "+
						"this is a bug in the server being imported", name)
			}
			continue
		}
		c.byName[name] = fd
		for _, dep := range fd.GetDependency() {
			if _, ok := c.importer[dep]; !ok {
				c.importer[dep] = name
			}
		}
	}
	return nil
}
```

Update the constructor in `Closure` to initialize the new maps:

```go
	c := &closure{
		f:        f,
		byName:   map[string]*descriptorpb.FileDescriptorProto{},
		importer: map[string]string{},
		done:     map[string]bool{},
		visiting: map[string]bool{},
	}
```

- [ ] **Step 4: Add the dependency recursion**

Add `resolve` to `internal/schema/upstream/closure.go`:

```go
// resolve fetches dependencies the upstream did not volunteer, until the set
// is closed. Against grpc-go this usually does nothing (F4).
func (c *closure) resolve(ctx context.Context) error {
	for {
		var missing []string
		for _, fd := range c.byName {
			for _, dep := range fd.GetDependency() {
				if _, ok := c.byName[dep]; !ok {
					missing = append(missing, dep)
				}
			}
		}
		if len(missing) == 0 {
			return nil
		}
		// Sorted so a failure is reported deterministically rather than
		// depending on map iteration order.
		sort.Strings(missing)
		for _, dep := range missing {
			if _, ok := c.byName[dep]; ok {
				continue // a sibling fetch already brought it in
			}
			raw, err := c.f.FileByFilename(ctx, dep)
			if err != nil {
				return fmt.Errorf("fetching %q: %w", dep, err)
			}
			if err := c.absorb(raw); err != nil {
				return err
			}
			if _, ok := c.byName[dep]; !ok {
				return fmt.Errorf(
					"the upstream does not have %q, imported by %q; "+
						"the descriptor set it serves is not self-contained",
					dep, c.importer[dep])
			}
		}
	}
}
```

Call it from `Closure`, between the service loop and `emitAll`:

```go
	if err := c.resolve(ctx); err != nil {
		return nil, err
	}
	return c.emitAll()
```

- [ ] **Step 5: Add the cycle guard**

Replace `emit` in `internal/schema/upstream/closure.go`. It now carries the path that reached this file, so the error can show the cycle:

```go
// emit is the post-order DFS. Proto forbids circular imports, so a cycle
// means a broken upstream; without the visiting guard this would recurse
// until the stack gave out instead of saying so.
func (c *closure) emit(name string, stack []string) error {
	if c.done[name] {
		return nil
	}
	if c.visiting[name] {
		return fmt.Errorf("the upstream's descriptors import in a cycle: %s",
			strings.Join(append(stack, name), " -> "))
	}
	fd, ok := c.byName[name]
	if !ok {
		return fmt.Errorf("internal: %q was never absorbed", name)
	}
	c.visiting[name] = true
	for _, dep := range fd.GetDependency() {
		if err := c.emit(dep, append(stack, name)); err != nil {
			return err
		}
	}
	delete(c.visiting, name)
	c.done[name] = true
	c.out = append(c.out, fd)
	return nil
}
```

Update the call in `emitAll` to `c.emit(name, nil)`, and add `"strings"` to the file's import block.

> Note the error text changed: after `resolve`, every dependency is present, so an absent file in `emit` is an internal invariant failure rather than a user-facing missing-dependency report. `resolve` owns that message now, which is what `TestClosureMissingDependencyNamesTheImporter` asserts.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/schema/upstream/ -run TestClosure -v -count=1 -timeout 60s`
Expected: PASS — all seven `TestClosure*` tests.

- [ ] **Step 7: Mutation checks**

Two, each run separately. Revert each before starting the next.

**7a — the cycle guard.** Comment out the guard in `emit`:

```go
	// MUTATION: drop the cycle guard
	// if c.visiting[name] { return fmt.Errorf(...) }
```

Run: `go test ./internal/schema/upstream/ -run TestClosureDetectsAnImportCycle -count=1 -timeout 30s`
Expected: FAIL — a stack overflow or a timeout, not a clean pass. **Revert.**

**7b — conflict detection.** Replace the `proto.Equal` check in `absorb` with an unconditional `continue`:

```go
		if _, ok := c.byName[name]; ok {
			// MUTATION: accept whichever definition arrived first
			continue
		}
```

Run: `go test ./internal/schema/upstream/ -run TestClosureConflictingDefinitionsAreAnError -count=1`
Expected: FAIL with `Closure succeeded, want an error naming the conflicting file`. **Revert** and re-run the full `TestClosure` set to confirm PASS.

- [ ] **Step 8: Run the whole package under the race detector**

Run: `go test ./internal/schema/upstream/ -race -count=1`
Expected: PASS, no race reports.

- [ ] **Step 9: Commit**

```bash
git add internal/schema/upstream/closure.go internal/schema/upstream/closure_test.go
git commit -m "feat(upstream): resolve withheld dependencies, reject conflicts, detect cycles"
```

---

### Task 3: The reflection transport and its version fallback

**Files:**
- Create: `internal/schema/upstream/client.go`
- Test: `internal/schema/upstream/client_test.go`

**Interfaces:**
- Consumes: `Fetcher` from Task 1.
- Produces: `type StreamOpener func(ctx context.Context, path string) (grpc.ClientStream, error)`; `const PathV1, PathV1Alpha string`; `func NewFetcher(open StreamOpener) Fetcher`.

- [ ] **Step 1: Write the failing test**

Create `internal/schema/upstream/client_test.go`:

```go
package upstream

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	v1pb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fakeStream is a grpc.ClientStream whose send and receive outcomes are
// dictated by the test. It exists because the ordering that produces a
// send-side io.EOF is a race on a real connection (F2); here it is a
// parameter.
type fakeStream struct {
	grpc.ClientStream
	sendErr error
	recv    *v1pb.ServerReflectionResponse
	recvErr error
	sends   int
}

func (f *fakeStream) SendMsg(any) error { f.sends++; return f.sendErr }

func (f *fakeStream) RecvMsg(m any) error {
	if f.recvErr != nil {
		return f.recvErr
	}
	proto.Merge(m.(*v1pb.ServerReflectionResponse), f.recv)
	return nil
}

func (f *fakeStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeStream) Context() context.Context     { return context.Background() }
func (f *fakeStream) CloseSend() error             { return nil }

func listServicesResponse(names ...string) *v1pb.ServerReflectionResponse {
	var svcs []*v1pb.ServiceResponse
	for _, n := range names {
		svcs = append(svcs, &v1pb.ServiceResponse{Name: n})
	}
	return &v1pb.ServerReflectionResponse{
		MessageResponse: &v1pb.ServerReflectionResponse_ListServicesResponse{
			ListServicesResponse: &v1pb.ListServiceResponse{Service: svcs},
		},
	}
}

// The core regression: a send-side io.EOF means the stream is already done
// and the real status comes from RecvMsg. Treating it as fatal would surface
// a bare io.EOF and skip the fallback against exactly the servers the
// fallback exists for (F2).
func TestFallbackSurvivesSendSideEOF(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sendErr error
	}{
		{"send returns io.EOF (the racy ordering)", io.EOF},
		{"send returns nil (the fast ordering)", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened []string
			open := func(_ context.Context, path string) (grpc.ClientStream, error) {
				opened = append(opened, path)
				if path == PathV1 {
					return &fakeStream{
						sendErr: tc.sendErr,
						recvErr: status.Error(codes.Unimplemented, "unknown service"),
					}, nil
				}
				return &fakeStream{recv: listServicesResponse("shop.v1.OrderService")}, nil
			}
			got, err := NewFetcher(open).ListServices(context.Background())
			if err != nil {
				t.Fatalf("ListServices: %v", err)
			}
			if len(got) != 1 || got[0] != "shop.v1.OrderService" {
				t.Fatalf("services = %v, want [shop.v1.OrderService]", got)
			}
			want := []string{PathV1, PathV1Alpha}
			if len(opened) != len(want) || opened[0] != want[0] || opened[1] != want[1] {
				t.Fatalf("opened %v, want %v", opened, want)
			}
		})
	}
}

// Once a version answers, later calls must not re-probe v1.
func TestNegotiatedVersionIsReused(t *testing.T) {
	var opened []string
	open := func(_ context.Context, path string) (grpc.ClientStream, error) {
		opened = append(opened, path)
		if path == PathV1 {
			return &fakeStream{recvErr: status.Error(codes.Unimplemented, "unknown service")}, nil
		}
		return &fakeStream{recv: listServicesResponse("a.A")}, nil
	}
	f := NewFetcher(open)
	for i := 0; i < 3; i++ {
		if _, err := f.ListServices(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	var v1Probes int
	for _, p := range opened {
		if p == PathV1 {
			v1Probes++
		}
	}
	if v1Probes != 1 {
		t.Fatalf("probed v1 %d times, want 1; opened %v", v1Probes, opened)
	}
}

// Reflection reports a missing file in-band as an ErrorResponse, not as a
// stream error. It must not be mistaken for a successful empty result.
func TestInBandErrorResponseIsAnError(t *testing.T) {
	open := func(context.Context, string) (grpc.ClientStream, error) {
		return &fakeStream{recv: &v1pb.ServerReflectionResponse{
			MessageResponse: &v1pb.ServerReflectionResponse_ErrorResponse{
				ErrorResponse: &v1pb.ErrorResponse{
					ErrorCode:    int32(codes.NotFound),
					ErrorMessage: "file not found",
				},
			},
		}}, nil
	}
	_, err := NewFetcher(open).FileByFilename(context.Background(), "gone.proto")
	if err == nil {
		t.Fatal("FileByFilename succeeded on an ErrorResponse, want an error")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", status.Code(err))
	}
}

// A non-EOF send failure is a real transport failure and must not be
// swallowed into a fallback attempt.
func TestNonEOFSendErrorIsFatal(t *testing.T) {
	boom := errors.New("connection reset")
	open := func(context.Context, string) (grpc.ClientStream, error) {
		return &fakeStream{sendErr: boom}, nil
	}
	_, err := NewFetcher(open).ListServices(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the transport failure", err)
	}
}

func TestFileContainingSymbolReturnsDescriptorBytes(t *testing.T) {
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("a.proto")}
	raw, err := proto.Marshal(fd)
	if err != nil {
		t.Fatal(err)
	}
	open := func(context.Context, string) (grpc.ClientStream, error) {
		return &fakeStream{recv: &v1pb.ServerReflectionResponse{
			MessageResponse: &v1pb.ServerReflectionResponse_FileDescriptorResponse{
				FileDescriptorResponse: &v1pb.FileDescriptorResponse{
					FileDescriptorProto: [][]byte{raw},
				},
			},
		}}, nil
	}
	got, err := NewFetcher(open).FileContainingSymbol(context.Background(), "a.A")
	if err != nil {
		t.Fatalf("FileContainingSymbol: %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(raw) {
		t.Fatalf("got %d descriptor(s), want the one served", len(got))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/schema/upstream/ -run 'TestFallback|TestNegotiated|TestInBand|TestNonEOF|TestFileContaining' -v -count=1`
Expected: FAIL to compile — `undefined: NewFetcher`, `undefined: PathV1`, `undefined: PathV1Alpha`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/schema/upstream/client.go`:

```go
package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	v1pb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
)

// The two reflection method paths. We marshal grpc_reflection_v1 Go types
// against both: the v1 and v1alpha protos are wire-identical — v1 was copied
// from v1alpha and both are frozen — so one transport covers both versions
// and this codebase needs no v1alpha Go types at all (F1).
const (
	PathV1      = "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"
	PathV1Alpha = "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
)

// StreamOpener opens a bidirectional stream to one reflection method path.
// Production passes a closure over grpc.ClientConn.NewStream; tests pass a
// fake so the send/receive ordering is a parameter rather than a race (F2).
type StreamOpener func(ctx context.Context, path string) (grpc.ClientStream, error)

var streamDesc = &grpc.StreamDesc{
	StreamName:    "ServerReflectionInfo",
	ServerStreams: true,
	ClientStreams: true,
}

// NewFetcher returns a Fetcher over a reflection endpoint. The protocol
// version is negotiated on the first call and reused thereafter.
func NewFetcher(open StreamOpener) Fetcher { return &reflectFetcher{open: open} }

type reflectFetcher struct {
	open StreamOpener
	// path is the negotiated method path, empty until the first successful
	// round trip.
	path string
}

func (r *reflectFetcher) ListServices(ctx context.Context) ([]string, error) {
	resp, err := r.roundTrip(ctx, &v1pb.ServerReflectionRequest{
		MessageRequest: &v1pb.ServerReflectionRequest_ListServices{ListServices: ""},
	})
	if err != nil {
		return nil, err
	}
	if err := inBandError(resp); err != nil {
		return nil, err
	}
	ls := resp.GetListServicesResponse()
	if ls == nil {
		return nil, fmt.Errorf("the upstream answered ListServices with %T",
			resp.GetMessageResponse())
	}
	out := make([]string, 0, len(ls.GetService()))
	for _, s := range ls.GetService() {
		out = append(out, s.GetName())
	}
	return out, nil
}

func (r *reflectFetcher) FileContainingSymbol(ctx context.Context, symbol string) ([][]byte, error) {
	return r.files(ctx, &v1pb.ServerReflectionRequest{
		MessageRequest: &v1pb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: symbol,
		},
	})
}

func (r *reflectFetcher) FileByFilename(ctx context.Context, name string) ([][]byte, error) {
	return r.files(ctx, &v1pb.ServerReflectionRequest{
		MessageRequest: &v1pb.ServerReflectionRequest_FileByFilename{FileByFilename: name},
	})
}

func (r *reflectFetcher) files(ctx context.Context, req *v1pb.ServerReflectionRequest) ([][]byte, error) {
	resp, err := r.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := inBandError(resp); err != nil {
		return nil, err
	}
	fdr := resp.GetFileDescriptorResponse()
	if fdr == nil {
		return nil, fmt.Errorf("the upstream answered with %T", resp.GetMessageResponse())
	}
	return fdr.GetFileDescriptorProto(), nil
}

// inBandError converts reflection's ErrorResponse into a real error.
// Reflection reports a missing file this way rather than failing the stream,
// so without this a NOT_FOUND would read as a successful empty result.
func inBandError(resp *v1pb.ServerReflectionResponse) error {
	e := resp.GetErrorResponse()
	if e == nil {
		return nil
	}
	return status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage())
}

// roundTrip sends one request and reads one response, negotiating the
// reflection version on the first call.
func (r *reflectFetcher) roundTrip(ctx context.Context, req *v1pb.ServerReflectionRequest) (*v1pb.ServerReflectionResponse, error) {
	paths := []string{PathV1, PathV1Alpha}
	if r.path != "" {
		paths = []string{r.path}
	}
	var lastErr error
	for i, path := range paths {
		strm, err := r.open(ctx, path)
		if err != nil {
			return nil, err
		}
		// A send-side io.EOF means the stream is already done and carries no
		// status of its own; the real status comes from RecvMsg. Returning
		// here on any non-nil error — the obvious way to write it — would
		// surface a bare io.EOF and skip the fallback entirely against
		// precisely the servers the fallback exists for (F2).
		if err := strm.SendMsg(req); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		resp := &v1pb.ServerReflectionResponse{}
		err = strm.RecvMsg(resp)
		if err == nil {
			r.path = path
			return resp, nil
		}
		lastErr = err
		// Only an Unimplemented v1 earns a second attempt.
		if status.Code(err) == codes.Unimplemented && i < len(paths)-1 {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

// Dial connects to an upstream. TLS with system roots by default, because
// "point at staging" is normally TLS; plaintext is the explicit opt-out that
// localhost and our own dogfood test need.
func Dial(addr string, plaintext bool) (*grpc.ClientConn, error) {
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if plaintext {
		creds = insecure.NewCredentials()
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

// OpenerFor adapts a connection into a StreamOpener.
func OpenerFor(cc *grpc.ClientConn) StreamOpener {
	return func(ctx context.Context, path string) (grpc.ClientStream, error) {
		return cc.NewStream(ctx, streamDesc, path)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/schema/upstream/ -run 'TestFallback|TestNegotiated|TestInBand|TestNonEOF|TestFileContaining' -v -count=1`
Expected: PASS — six test cases including both `TestFallbackSurvivesSendSideEOF` subtests.

- [ ] **Step 5: Mutation check — the io.EOF exemption**

This is the mutation the whole seam exists to catch. In `roundTrip`, drop the exemption:

```go
		// MUTATION: treat any send error as fatal
		if err := strm.SendMsg(req); err != nil {
			return nil, err
		}
```

Run: `go test ./internal/schema/upstream/ -run TestFallbackSurvivesSendSideEOF -v -count=1`
Expected: FAIL on the `io.EOF` subtest with `ListServices: EOF`, while the `nil` subtest still passes. That asymmetry is the proof the test discriminates. **Revert the mutation** and re-run to confirm both PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/schema/upstream/client.go internal/schema/upstream/client_test.go
git commit -m "feat(upstream): reflection transport with a v1alpha fallback that survives a send-side EOF"
```

---

### Task 4: Wire tests against real servers

**Files:**
- Create: `internal/schema/upstream/wire_test.go`

**Interfaces:**
- Consumes: `NewFetcher`, `OpenerFor`, `PathV1`, `Closure` from Tasks 1–3.
- Produces: nothing new.

> **Why this is its own task:** Tasks 1–3 prove the logic against fakes. This proves the fakes match reality — including the v1alpha fallback, which is unreachable against Simulacra itself (F3), and the racy ordering, which F7 lets us force with no sleep.

- [ ] **Step 1: Write the failing test**

Create `internal/schema/upstream/wire_test.go`:

```go
package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	v1pb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphapb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

// reflectionVersions selects which reflection services a test server serves.
type reflectionVersions int

const (
	bothVersions reflectionVersions = iota
	v1AlphaOnly
)

// startReflectingServer starts a stock grpc-go server. Simulacra registers
// both versions (F3), so only a server we build ourselves can exercise the
// v1alpha fallback.
func startReflectingServer(t *testing.T, versions reflectionVersions) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	healthpb.RegisterHealthServer(s, health.NewServer())
	opts := reflection.ServerOptions{Services: s}
	// NewServer returns the v1alpha implementation; NewServerV1 the v1 one.
	v1alphapb.RegisterServerReflectionServer(s, reflection.NewServer(opts))
	if versions == bothVersions {
		v1pb.RegisterServerReflectionServer(s, reflection.NewServerV1(opts))
	}
	go func() { _ = s.Serve(lis) }()

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close(); s.Stop() })
	return cc
}

func TestWireListServicesOverV1(t *testing.T) {
	cc := startReflectingServer(t, bothVersions)
	got, err := NewFetcher(OpenerFor(cc)).ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !contains(got, "grpc.health.v1.Health") {
		t.Fatalf("services = %v, want health among them", got)
	}
}

// The fallback, against a server that speaks only v1alpha.
func TestWireFallsBackToV1Alpha(t *testing.T) {
	cc := startReflectingServer(t, v1AlphaOnly)
	f := NewFetcher(OpenerFor(cc))
	got, err := f.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !contains(got, "grpc.health.v1.Health") {
		t.Fatalf("services = %v, want health among them", got)
	}
	if p := f.(*reflectFetcher).path; p != PathV1Alpha {
		t.Fatalf("negotiated %q, want the v1alpha path", p)
	}
}

// The same fallback, with the racy ordering forced. Header() waits for the
// terminal status and then deliberately returns (nil, nil) -- grpc-go calls
// cs.finish(err) and comments "Do not return the error. The user should get
// it by calling Recv()" (stream.go:875-878). So after Header() returns, the
// stream is done and the next SendMsg is io.EOF, with no sleep anywhere (F7).
//
// This asserts only that the fallback SUCCEEDS. It deliberately does not
// assert which send outcome occurred: Header()-finishes-before-returning is a
// grpc-go implementation detail, not a documented contract, and pinning it
// here would turn a library change into a false alarm. The send outcomes
// themselves are covered at the seam in client_test.go.
func TestWireFallsBackWhenTheRejectionArrivesFirst(t *testing.T) {
	cc := startReflectingServer(t, v1AlphaOnly)

	var sawEOF bool
	open := func(ctx context.Context, path string) (grpc.ClientStream, error) {
		strm, err := cc.NewStream(ctx, streamDesc, path)
		if err != nil {
			return nil, err
		}
		if path == PathV1 {
			// THE RENDEZVOUS: block until the rejection has arrived.
			_, _ = strm.Header()
		}
		return &recordingStream{ClientStream: strm, sawEOF: &sawEOF}, nil
	}

	got, err := NewFetcher(open).ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if !contains(got, "grpc.health.v1.Health") {
		t.Fatalf("services = %v, want health among them", got)
	}
	// Informational: records that the ordering really was the racy one, without
	// failing the test if grpc-go ever changes when Header() returns.
	t.Logf("send-side io.EOF observed on the rejected v1 stream: %v", sawEOF)
}

// recordingStream notes whether SendMsg returned io.EOF.
type recordingStream struct {
	grpc.ClientStream
	sawEOF *bool
}

func (r *recordingStream) SendMsg(m any) error {
	err := r.ClientStream.SendMsg(m)
	if errors.Is(err, io.EOF) {
		*r.sawEOF = true
	}
	return err
}

// Closure over a real reflection endpoint, end to end.
func TestWireClosureIsSelfContained(t *testing.T) {
	cc := startReflectingServer(t, bothVersions)
	set, err := Closure(context.Background(), NewFetcher(OpenerFor(cc)))
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	// health and both reflection services are skipped, and this stock server
	// serves nothing else -- so the set is empty, which is itself the
	// assertion that the skip rule fires on a real ListServices (F5).
	if n := len(set.GetFile()); n != 0 {
		t.Fatalf("got %d file(s) %v, want none: every service this server "+
			"exposes is one the data plane implements itself", n, set.GetFile())
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/schema/upstream/ -run TestWire -v -count=1`
Expected: PASS — four tests. `TestWireFallsBackWhenTheRejectionArrivesFirst` should log `send-side io.EOF observed on the rejected v1 stream: true`.

- [ ] **Step 3: Confirm the fallback test discriminates**

Temporarily register v1 on the `v1AlphaOnly` server:

```go
	// MUTATION: serve v1 as well, so there is nothing to fall back from
	v1pb.RegisterServerReflectionServer(s, reflection.NewServerV1(opts))
```

Run: `go test ./internal/schema/upstream/ -run TestWireFallsBackToV1Alpha -count=1`
Expected: FAIL with `negotiated ".../grpc.reflection.v1...", want the v1alpha path`. **Revert the mutation** and re-run to confirm PASS.

- [ ] **Step 4: Run the package repeatedly to check for flakes**

Run: `go test ./internal/schema/upstream/ -race -count=10`
Expected: PASS all ten. Any flake here means a test is timing-dependent — fix it rather than re-running.

- [ ] **Step 5: Commit**

```bash
git add internal/schema/upstream/wire_test.go
git commit -m "test(upstream): wire tests for v1, the v1alpha fallback, and the racy ordering"
```

---

### Task 5: The `schema import` command

**Files:**
- Create: `internal/cli/schema_import.go`
- Modify: `internal/cli/schema.go:23` (register the subcommand)
- Test: `internal/cli/schema_import_test.go`

**Interfaces:**
- Consumes: `upstream.Dial`, `upstream.OpenerFor`, `upstream.NewFetcher`, `upstream.Closure` (Tasks 1–4); `clientAnnotations`, `signalContext`, `errInterrupted`, `newPayloadWriter`, `outputFlag` from the existing `internal/cli`.
- Produces: `func newSchemaImportCmd() *cobra.Command`.

> **Flag naming, settled by precedent:** `--out`/`-o` is the destination file, matching `stub export` (`internal/cli/stub.go:184`). `--output` is the text/json format flag and **has no shorthand** precisely so the two can coexist — see the comment at `internal/cli/output.go:18`.
>
> **On `--output json`:** the summary is CLI-local metadata, not a frozen admin-contract message, so it is rendered with `encoding/json` over a local struct rather than `writeJSON`, which takes a `proto.Message`. This is the one place the CLI renders JSON outside the protojson contract path; it is deliberate and scoped to this summary.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/schema_import_test.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestSchemaImportRequiresReflect(t *testing.T) {
	_, _, err := runCmd(t, newSchemaCmd(), "import", "-o", filepath.Join(t.TempDir(), "s.binpb"))
	if err == nil {
		t.Fatal("import succeeded without --reflect, want an error")
	}
	if !strings.Contains(err.Error(), "--reflect") {
		t.Fatalf("error %q does not name --reflect", err)
	}
}

func TestSchemaImportRequiresOut(t *testing.T) {
	_, _, err := runCmd(t, newSchemaCmd(), "import", "--reflect", "127.0.0.1:1")
	if err == nil {
		t.Fatal("import succeeded without --out, want an error")
	}
	if !strings.Contains(err.Error(), "--out") {
		t.Fatalf("error %q does not name --out", err)
	}
}

// The happy path, against Simulacra's own data plane.
func TestSchemaImportWritesADescriptorSet(t *testing.T) {
	srv := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	stdout, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", srv.DataAddr().String(), "--plaintext", "-o", out)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(stdout, "schema.binpb") {
		t.Fatalf("summary %q does not name the destination", stdout)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the imported set: %v", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatalf("the written file is not a FileDescriptorSet: %v", err)
	}
	var found bool
	for _, f := range set.GetFile() {
		if strings.Contains(f.GetName(), "order.proto") {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported %d file(s) but not order.proto", len(set.GetFile()))
	}
}

func TestSchemaImportJSONOutput(t *testing.T) {
	srv := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	stdout, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", srv.DataAddr().String(), "--plaintext", "-o", out, "--output", "json")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var got struct {
		Files    []string `json:"files"`
		Services int      `json:"services"`
		Bytes    int      `json:"bytes"`
		Path     string   `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
	if len(got.Files) == 0 || got.Bytes == 0 || got.Path != out {
		t.Fatalf("summary = %+v, want files, a byte count and the destination", got)
	}
}

// An unreachable upstream is an operational failure, not a written file.
func TestSchemaImportUnreachableUpstreamWritesNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "schema.binpb")
	_, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", "127.0.0.1:1", "--plaintext", "-o", out)
	if err == nil {
		t.Fatal("import succeeded against a closed port, want an error")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("a destination file was created on failure (stat err = %v)", statErr)
	}
}

// The commit boundary: a context cancelled before the rename must leave an
// existing destination byte-identical, with no temp file beside it.
func TestSchemaImportCancelledBeforeRenamePreservesDestination(t *testing.T) {
	srv := startCommandServer(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "schema.binpb")
	original := []byte("the previous descriptor set")
	if err := os.WriteFile(out, original, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := newSchemaCmd()
	cmd.SetArgs([]string{"import", "--reflect", srv.DataAddr().String(),
		"--plaintext", "-o", out})
	// Cancel before the command runs: the walk fails or the pre-rename check
	// fires, and either way publication must not happen.
	cancel()
	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("import succeeded under a cancelled context, want an error")
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("the destination is gone: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("destination = %q, want it untouched (%q)", got, original)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only the destination: a temp file was left behind", names)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestSchemaImport -v -count=1`
Expected: FAIL — `unknown command "import" for "schema"`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/schema_import.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"github.com/yinghanhung/simulacra/internal/schema/upstream"
)

func newSchemaImportCmd() *cobra.Command {
	var (
		reflectAddr string
		plaintext   bool
		outPath     string
	)
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a descriptor set from a running server's reflection endpoint",
		// import is under the 0/1/2 exit contract but is NOT an admin-plane
		// client: it dials an upstream, never our admin plane. So it carries
		// the annotation without clientFlags -- --addr and --timeout would be
		// meaningless here, and SIMULACRA_ADDR must not silently retarget an
		// import (design §6).
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if reflectAddr == "" {
				return errors.New("--reflect <host:port> is required")
			}
			if outPath == "" {
				return errors.New("--out <path> is required")
			}
			if err := out.validate(); err != nil {
				return err
			}

			// Signal context first, before dialing and before any file
			// exists: the phase 5 lesson is to establish the command's
			// guarantees before the first side effect.
			ctx, stop := signalContext(cmd)
			defer stop()

			cc, err := upstream.Dial(reflectAddr, plaintext)
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", reflectAddr, err)
			}
			defer func() { _ = cc.Close() }()

			set, err := upstream.Closure(ctx, upstream.NewFetcher(upstream.OpenerFor(cc)))
			if err != nil {
				if ctx.Err() != nil {
					return errInterrupted
				}
				return fmt.Errorf("importing from %s: %w", reflectAddr, err)
			}
			raw, err := proto.Marshal(set)
			if err != nil {
				return fmt.Errorf("encoding the imported descriptor set: %w", err)
			}
			if err := publish(ctx, outPath, raw); err != nil {
				return err
			}

			files := make([]string, 0, len(set.GetFile()))
			for _, f := range set.GetFile() {
				files = append(files, f.GetName())
			}
			return writeImportSummary(cmd, out.json(), files, countServices(set), len(raw), outPath)
		},
	}
	cmd.Flags().StringVar(&reflectAddr, "reflect", "",
		"address of the server to import from, e.g. staging.internal:443")
	cmd.Flags().BoolVar(&plaintext, "plaintext", false,
		"connect without TLS")
	cmd.Flags().StringVarP(&outPath, "out", "o", "",
		"write the descriptor set here")
	out.register(cmd)
	return cmd
}

// publish writes raw to path atomically.
//
// os.Rename is the commit boundary and it is NOT context-aware: it will
// happily publish after the walk's context has been cancelled, so atomic
// replacement alone does not give cancellation safety. The explicit check
// below narrows that window; it cannot close it, and past the rename a
// successful publication has intentionally replaced the destination
// (design §6).
func publish(ctx context.Context, path string, raw []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Harmless after a successful rename, and the cleanup path for every
	// failure from here on.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if ctx.Err() != nil {
		return errInterrupted
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publishing %s: %w", path, err)
	}
	return nil
}

// countServices counts the services described by the imported set.
func countServices(set *descriptorpb.FileDescriptorSet) int {
	var n int
	for _, f := range set.GetFile() {
		n += len(f.GetService())
	}
	return n
}

// importSummary is CLI-local metadata, not an admin-contract message, so it
// renders with encoding/json rather than the protojson path writeJSON takes.
type importSummary struct {
	Files    []string `json:"files"`
	Services int      `json:"services"`
	Bytes    int      `json:"bytes"`
	Path     string   `json:"path"`
}

func writeImportSummary(cmd *cobra.Command, asJSON bool, files []string, services, bytes int, path string) error {
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(importSummary{
			Files: files, Services: services, Bytes: bytes, Path: path,
		})
	}
	payload := newPayloadWriter(cmd)
	payload.printf("imported %d file(s) from %d service(s) -> %s\n",
		len(files), services, path)
	return payload.err
}
```

Add `"google.golang.org/protobuf/types/descriptorpb"` to the import block.

Then register the subcommand — modify `internal/cli/schema.go:23`:

```go
	cmd.AddCommand(newSchemaRegisterCmd(newClient), newSchemaListCmd(newClient), newSchemaImportCmd())
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestSchemaImport -v -count=1`
Expected: PASS — six tests.

- [ ] **Step 5: Test the commit boundary directly**

The command-level cancellation test above cancels before the command runs, so the *walk* fails and
`publish` is never reached. It proves the exit path, not the guard. Test the guard where it lives —
append to `internal/cli/schema_import_test.go`:

```go
// publish must not replace the destination once the context is done. This
// tests the guard directly: the command-level test cancels before the walk,
// so it never reaches this code at all.
func TestPublishRefusesToRenameAfterCancellation(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	original := []byte("the previous descriptor set")
	if err := os.WriteFile(dest, original, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := publish(ctx, dest, []byte("the newly imported set"))
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("publish err = %v, want errInterrupted", err)
	}
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("the destination is gone: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("destination = %q, want it untouched", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the destination: "+
			"the temp file was left behind", len(entries))
	}
}

// The same call on a live context must publish.
func TestPublishReplacesTheDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), dest, []byte("new")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("destination = %q, want %q", got, "new")
	}
}
```

Add `"errors"` to the test file's import block.

Run: `go test ./internal/cli/ -run TestPublish -v -count=1`
Expected: PASS — both tests.

- [ ] **Step 6: Mutation check — the pre-rename guard**

Remove the commit-boundary check in `publish`:

```go
	// MUTATION: publish regardless of cancellation
	// if ctx.Err() != nil { return errInterrupted }
```

Run: `go test ./internal/cli/ -run TestPublish -v -count=1`
Expected: FAIL on `TestPublishRefusesToRenameAfterCancellation` with `publish err = <nil>, want errInterrupted`, while `TestPublishReplacesTheDestination` still passes. That asymmetry is the proof it discriminates. **Revert the mutation** and re-run to confirm both PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/schema_import.go internal/cli/schema_import_test.go internal/cli/schema.go
git commit -m "feat(cli): add schema import --reflect with an explicit commit boundary"
```

---

### Task 6: Signal handling end to end

**Files:**
- Create: `internal/cli/schema_import_signal_test.go`

**Interfaces:**
- Consumes: `buildBinary` and `exitStatusWithin` from `internal/cli/signal_test.go:47,116`.
- Produces: nothing new.

> **Why a built binary:** `signalContext` installs a real `signal.NotifyContext`, and the 0/1/2 exit contract is applied in `Execute` via `ExecuteC`. Neither is observable in-process. This follows the pattern already in `signal_test.go`.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/schema_import_signal_test.go`:

```go
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A signal during an import must exit 2 and leave an existing destination
// exactly as it was. The upstream here is a listener that accepts and never
// answers, so the walk is still in flight when the signal lands.
func TestSchemaImportInterruptedExitsTwoAndPreservesDestination(t *testing.T) {
	for _, sig := range []struct {
		name   string
		signal os.Signal
	}{
		{"SIGINT", os.Interrupt},
		{"SIGTERM", syscall.SIGTERM},
	} {
		t.Run(sig.name, func(t *testing.T) {
			bin := buildBinary(t)
			// hangingListener accepts the connection and never returns
			// headers -- the same helper the phase 5 client tests use.
			addr := hangingListener(t)

			dir := t.TempDir()
			out := filepath.Join(dir, "schema.binpb")
			original := []byte("the previous descriptor set")
			if err := os.WriteFile(out, original, 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, "schema", "import",
				"--reflect", addr, "--plaintext", "-o", out)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			// Give the child long enough to reach the dial; the assertion
			// below does not depend on exactly where it is.
			time.Sleep(300 * time.Millisecond)
			if err := cmd.Process.Signal(sig.signal); err != nil {
				t.Fatalf("signal: %v", err)
			}

			if code := exitStatusWithin(t, cmd, 10*time.Second); code != 2 {
				t.Fatalf("exit status %d, want 2 (stderr: %s)", code, stderr.String())
			}

			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("the destination is gone: %v", err)
			}
			if string(got) != string(original) {
				t.Fatalf("destination = %q, want it untouched", got)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("directory holds %d entries, want only the destination: "+
					"a temp file was left behind", len(entries))
			}
		})
	}
}
```

> **Readiness, not a sleep.** An earlier draft slept 300ms before signalling, justified as merely widening
> the window. That was wrong and the test failed deterministically on a cold build with `exit status -1`
> — the signal arriving before `signalContext` was installed, so the child died by the default
> disposition. Use `acceptedUpstream(t)` instead: a listener that never answers but closes a channel on
> its first accept. `schema import` installs its signal context *before* it dials, and `grpc.NewClient`
> connects lazily, so an accepted TCP connection proves the handler is already in place. This mirrors
> `waitForTailedCall` (`internal/cli/signal_test.go:127`), which replaced the same guess for the tail tests.
>
> **The destination-preserved and no-temp-file assertions here are belt-and-braces, not load-bearing.**
> The upstream never answers, so the walk never completes and `publish` is never reached — they cannot
> fail in this test. The commit boundary's real coverage is Task 5's
> `TestPublishRefusesToRenameAfterCancellation`. Say so in a comment so a later reader does not
> mistake them for proof.

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/cli/ -run TestSchemaImportInterrupted -v -count=1`
Expected: PASS — both subtests exit 2.

- [ ] **Step 3: Check for flakiness**

Run: `go test ./internal/cli/ -run TestSchemaImportInterrupted -count=5`
Expected: PASS all five.

- [ ] **Step 4: Commit**

```bash
git add internal/cli/schema_import_signal_test.go
git commit -m "test(cli): schema import exits 2 on a signal and preserves the destination"
```

---

### Task 7: Dogfood round-trip and the health skip

**Files:**
- Modify: `internal/schema/registry.go` (export one wrapper, Step 1)
- Modify: `server/server.go` (add a `Registry()` accessor, Step 4)
- Create: `internal/cli/schema_import_roundtrip_test.go`

**Interfaces:**
- Consumes: everything above; `startCommandServer` from `internal/cli/harness_test.go:32`.
- Produces: `func schema.NormalizeFileProto(*descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto`.

> **Why the export:** the round-trip must compare descriptors semantically, and the registry already owns the only correct equality policy — `normalizeFileProto` (`internal/schema/registry.go:398`), which strips source-code-info and buf's image extension and nothing else. Re-implementing it in a test would fork that policy. The test cannot live in `package schema` (it needs `server`, which imports `schema`, so the test binary would cycle) and `package schema_test` cannot see unexported names — hence one exported wrapper.

- [ ] **Step 1: Export the normalization**

Append to `internal/schema/registry.go`, immediately after `normalizeFileProto`:

```go
// NormalizeFileProto exposes the registry's file-equality policy: two
// same-path descriptors are the same file when their normalized forms are
// equal. Importers and tests that need to compare descriptors against what
// the registry would accept must use this rather than fork the policy --
// the surgical scope documented on normalizeFileProto is the contract.
func NormalizeFileProto(fdp *descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto {
	return normalizeFileProto(fdp)
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/cli/schema_import_roundtrip_test.go`:

```go
package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/server"
)

// The M3 §12 test: registry in -> identical descriptor set out.
//
// Two assertions, because one is not enough. `schema list` renders only
// service names, method names, input/output type names and streaming flags,
// so an import that dropped every field of every message would leave both
// listings identical and still register. Only the descriptor comparison
// establishes preservation (design §8).
func TestSchemaImportRoundTripPreservesDescriptors(t *testing.T) {
	source := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	if _, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", source.DataAddr().String(), "--plaintext", "-o", out); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Register the imported set into a second, empty server.
	dest, err := server.Start(context.Background(), server.Options{
		DataAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", JournalSize: 8,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(dest.Stop)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}

	// Assertion 1 (smoke): the same services are listed.
	srcServices := serviceNames(source)
	if _, _, err := runCmd(t, newSchemaCmd(), "register",
		"--addr", adminAddr(dest), "-f", out); err != nil {
		t.Fatalf("register: %v", err)
	}
	dstServices := serviceNames(dest)
	if len(srcServices) != len(dstServices) {
		t.Fatalf("services: source %v, destination %v", srcServices, dstServices)
	}

	// Assertion 2 (the real one): every imported file is byte-identical to
	// the source's, under the registry's own equality policy.
	for _, imported := range set.GetFile() {
		path := imported.GetName()
		fd, err := source.Registry().FindFileByPath(path)
		if err != nil {
			t.Fatalf("source has no %s, yet it was imported: %v", path, err)
		}
		want := schema.NormalizeFileProto(protodesc.ToFileDescriptorProto(fd))
		got := schema.NormalizeFileProto(imported)
		if !proto.Equal(want, got) {
			t.Fatalf("%s did not survive the round trip; the import is lossy", path)
		}
	}
}

// serviceNames lists a server's registered service names.
func serviceNames(srv *server.Server) []string {
	var out []string
	for _, svc := range srv.Registry().Services() {
		out = append(out, string(svc.FullName()))
	}
	return out
}

// F6: an upstream whose health.proto disagrees with our compiled-in one must
// not poison the import. Health is skipped as a root, so the set never
// carries it and registration succeeds.
func TestSchemaImportSkipsHealthSoRegistrationSucceeds(t *testing.T) {
	source := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	if _, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", source.DataAddr().String(), "--plaintext", "-o", out); err != nil {
		t.Fatalf("import: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	healthPath := grpc_health_v1.File_grpc_health_v1_health_proto.Path()
	for _, f := range set.GetFile() {
		if f.GetName() == healthPath {
			t.Fatalf("the import carries %s; health is implemented by the data "+
				"plane and an upstream copy that disagrees with ours fails the "+
				"entire registration (F6)", healthPath)
		}
	}

	// And it registers cleanly into a server that already has its own health.
	dest, err := server.Start(context.Background(), server.Options{
		DataAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", JournalSize: 8,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(dest.Stop)
	if _, _, err := runCmd(t, newSchemaCmd(), "register",
		"--addr", adminAddr(dest), "-f", out); err != nil {
		t.Fatalf("register: %v", err)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestSchemaImportRoundTrip -v -count=1`
Expected: FAIL to compile — `srv.Registry undefined`. `server.Server` exposes `ServiceCount()` but not the registry.

- [ ] **Step 4: Add the accessor**

Append to `server/server.go`, next to `ServiceCount` (`server/server.go:248`):

```go
// Registry exposes the schema registry this server serves. It exists for
// tests that must compare descriptors against what the server actually holds.
func (s *Server) Registry() *schema.Registry { return s.reg }
```

Confirm `internal/schema` is already imported in that file; it is, since the field is typed `*schema.Registry`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestSchemaImport -v -count=1`
Expected: PASS — all eight `TestSchemaImport*` tests.

- [ ] **Step 6: Mutation check — the health skip**

Remove health from the skip rule in `internal/schema/upstream/closure.go`:

```go
var SkippedServices = []string{
	"grpc.reflection.v1.ServerReflection",
	"grpc.reflection.v1alpha.ServerReflection",
	// MUTATION: "grpc.health.v1.Health",
}
```

Run: `go test ./internal/cli/ -run TestSchemaImportSkipsHealth -count=1`
Expected: FAIL with `the import carries grpc/health/v1/health.proto`. **Revert the mutation** and re-run to confirm PASS.

- [ ] **Step 7: Full verification**

Run each and confirm it passes before committing:

```bash
go build ./...
```

```bash
go vet ./...
```

```bash
go test ./... -race -count=1 -timeout 900s
```

```bash
git diff --check
```

- [ ] **Step 8: Commit**

```bash
git add server/server.go internal/schema/registry.go internal/cli/schema_import_roundtrip_test.go
git commit -m "test(cli): dogfood round-trip proves descriptor preservation and the health skip"
```

---

## Verification checklist

Run before declaring the phase done. Every command must pass; paste real output rather than asserting success.

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `go test ./... -race -count=1 -timeout 900s`
- [ ] `go test ./internal/schema/upstream/ -race -count=10` — the wire tests must not flake
- [ ] `git diff --check` — no whitespace damage
- [ ] `git status` — clean; **no `probe/` directory, no stray `.binpb` or `.tmp` files**
- [ ] Manual smoke against a real binary:

```bash
go build -o /tmp/simulacra ./cmd/simulacra && /tmp/simulacra serve --proto testdata/protos --listen 127.0.0.1:7565 --admin 127.0.0.1:7566 &
```

```bash
/tmp/simulacra schema import --reflect 127.0.0.1:7565 --plaintext -o /tmp/schema.binpb && /tmp/simulacra schema register --addr 127.0.0.1:7566 -f /tmp/schema.binpb
```

Expected: the import reports the files it wrote; the register reports `0 new file(s)` against the same server (they are already registered) and exits 0.

- [ ] Confirm the exit contract by hand:

```bash
/tmp/simulacra schema import --reflect 127.0.0.1:1 --plaintext -o /tmp/x.binpb; echo "exit=$?"
```

Expected: `exit=2`, and `/tmp/x.binpb` does not exist.
