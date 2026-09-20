# Simulacra M3 Phase 6 — Reflection Schema Import: Design

**Goal (M3 §13.6):** `simulacra schema import --reflect host:port -o schema.binpb` — point the CLI at a
running gRPC server, walk its reflection endpoint, and write a self-contained `FileDescriptorSet`.

**Reference:** `PROPOSAL.md` §6 (schema management), §9 (CLI); M3 design
`docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §7 (CLI additions), §12 (testing),
cited below as "M3 §N". Phase 5 design `docs/superpowers/specs/2026-09-12-m3-p5-cli-client-commands-design.md`,
cited as "P5 §N"; its plan `docs/superpowers/plans/2026-09-13-m3-p5-cli-client-commands.md` carries the
numbered amendments cited below. Phase 5 is complete at `26e6158`.

---

## 1. Why this phase, now

M3 §14 flags the JVM SDK as the long pole and prescribes front-loading the Dockerfile, which argues
for re-sequencing to 8 → 7 → 9 and deferring this phase. We are not doing that:

- The `schema` command group ships `register` and `list` and is visibly missing its third leg. The
  phase 5 context it lands in — client seam, exit contract, payload/stderr split, signal-before-side-effect
  — is current.
- The "discover admin-API gaps early via the Go SDK" argument is weaker than it looks: phase 5 already
  exercised nearly every admin RPC as a real consumer.
- It closes PROPOSAL §6 and yields a conformance test that costs nothing extra: Simulacra importing
  Simulacra (M3 §12).

**A rationale explicitly rejected.** An earlier framing claimed this phase's descriptor-closure logic
would seed the Go SDK's `RegisterFiles`. It does not. The SDK walks in-memory
`protoreflect.FileDescriptor.Imports()` — roughly fifteen lines with no network fetcher — and forcing a
shared abstraction across the two would serve neither. No such abstraction is designed here.

## 2. Scope

### In scope

1. `simulacra schema import --reflect host:port [--plaintext] -o schema.binpb [--output json]`.
2. A reflection client over `grpc_reflection_v1` with a `grpc_reflection_v1alpha` fallback.
3. A transitive-closure algorithm producing a topologically ordered, self-contained `FileDescriptorSet`.

### Non-goals

- **`--register`** (fetch from upstream and register into our admin plane in one step). §7, D1.
- **`--header k=v`** for authenticated upstreams. §7, D2.
- **`--insecure-skip-verify`** for self-signed staging certificates. §7, D3.
- **`-o -`** (descriptor set to stdout). The summary line would have to move to stderr, complicating
  P5 §4's output contract for little gain.
- **Server-side upstream import** (a `SchemaService.ImportUpstream` RPC where the *server* dials
  staging) — already an M3 §2 non-goal, unchanged.

## 3. Pre-verified facts

Established by throwaway probes against real grpc-go servers before this design was finalized, and
since deleted. Each decision below that depends on one cites it.

| # | Fact | Why it matters |
|---|---|---|
| **F1** | A `grpc.ClientConn.NewStream` to `/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo` carrying **`grpc_reflection_v1` Go message types** works against a v1alpha-only server. Probed against a stock grpc-go server with only `reflection.NewServer(opts)` registered; `ListServices` decoded correctly. | One transport implementation covers both versions (§5). The two protos are wire-identical by construction — v1 was copied from v1alpha and both are frozen. |
| **F2** | `Unimplemented` surfaces at **`RecvMsg`**, not at `NewStream` and not at `SendMsg`. Against a server with no reflection registered: `NewStream` → nil, `SendMsg` → nil, `RecvMsg` → `code = Unimplemented desc = unknown service grpc.reflection.v1.ServerReflection`. | The version fallback cannot live in a constructor. It belongs at the first `Recv` (§5). |
| **F3** | Simulacra's own data plane registers **both** v1 (`reflection.NewServerV1`) and v1alpha (`reflection.NewServer`) — `internal/dataplane/server.go:60-61`. The v1 path wins. | The v1alpha fallback is **unreachable** against our own server. It needs a dedicated v1alpha-only test server (§6). |
| **F4** | grpc-go's reflection returns the **transitive closure in one response**: `FileContainingSymbol("shop.v1.OrderService")` against Simulacra returned 3 files — `shop/v1/order.proto` plus both of its well-known-type imports. This is grpc-go behavior, not a protocol guarantee. | Against grpc-go upstreams the common case is one round trip per service. The dependency recursion is a correctness safety net for other implementations, not the primary path (§4). |
| **F5** | A stock grpc-go server advertises its own reflection services in `ListServices` (`grpc.reflection.v1alpha.ServerReflection` appeared in the probe). **Simulacra does not** — its custom `ServiceInfoProvider` (`internal/dataplane/server.go:65`) lists registry services plus health only. | The skip list (§4) is needed for real upstreams, and its test **cannot** use Simulacra as the upstream. It needs a stock grpc-go server (§6). |

## 4. The closure algorithm

Lives in a new package `internal/schema/upstream`, not in `internal/cli` — it is the part worth
testing without a network.

```go
// fetcher retrieves descriptor bytes from an upstream server.
type fetcher interface {
	ListServices(ctx context.Context) ([]string, error)
	FileContainingSymbol(ctx context.Context, symbol string) ([][]byte, error)
	FileByFilename(ctx context.Context, name string) ([][]byte, error)
}

func Closure(ctx context.Context, f fetcher) (*descriptorpb.FileDescriptorSet, error)
```

`Closure` is pure given a `fetcher`, so the interesting cases are unit tests against a fake with no
server running (§6).

**Walk.** `ListServices` → `FileContainingSymbol` per surviving service → unmarshal every returned
`FileDescriptorProto` → for each unsatisfied name in its `dependency` list, `FileByFilename` → repeat
until closed. Per F4 the first response usually closes the set already; the recursion rarely fires
against grpc-go and is what makes other implementations safe.

Four decisions inside it:

- **Topological emit order** (dependencies first, post-order DFS). This matches what
  `protoc --include_imports` and `buf build -o` produce, and removes any question of whether a consumer
  tolerates arbitrary ordering. It also satisfies the self-containment rule phase 5 found our own
  `RegisterSchemas` enforces — it rejects a non-self-contained set with *"descriptor sets must be
  self-contained; build with 'buf build -o' or 'protoc --include_imports'"* — an imported set feeds straight into `schema register`.
- **Cycle guard.** Proto forbids circular imports, so a cycle means a broken upstream. An unguarded DFS
  would hang instead of saying so; a `visiting` set turns it into a named error.
- **Skip `grpc.reflection.v1.ServerReflection` and `grpc.reflection.v1alpha.ServerReflection`.** They
  describe the transport, not the API under test. Health is **not** skipped — mocking health is
  legitimate. Importing reflection into a mock is not a crash (our data plane registers the real
  reflection service ahead of `UnknownServiceHandler`, so a mocked one is merely shadowed) but it is
  noise in every `schema list` thereafter. Skipping at service enumeration is sufficient: the reflection
  files are not dependencies of user services.
- **Conflicting bytes for one filename → error naming the file.** Mirrors `mergeDescriptorSets`
  (`internal/cli/schema.go`). One server returning two definitions of one file is a server bug; saying so
  is more useful than silently keeping the first, even though the user cannot fix the upstream.

## 5. The reflection transport

`internal/schema/upstream/client.go` implements `fetcher` over a single bidirectional
`ServerReflectionInfo` stream.

**One implementation, two method paths (F1).** The client marshals `grpc_reflection_v1` Go types and
selects the path string. There are no v1alpha Go types in this codebase and no adapter pair.

**Fallback at first Recv (F2).** Open the stream on the v1 path, send, receive. If the first `RecvMsg`
returns `codes.Unimplemented`, discard that stream, open a new one on the v1alpha path, and replay the
request. Subsequent failures are real errors. The version is decided once per import and cached for the
rest of the walk.

**Transport security.** TLS with system roots by default, since "point at staging" is normally TLS.
`--plaintext` opts out, and is what the dogfood test and any localhost use needs.

## 6. CLI surface

`internal/cli/schema_import.go` — a new file rather than growing `schema.go`.

```
simulacra schema import --reflect host:port [--plaintext] -o schema.binpb [--output json]
```

**`--reflect` and `-o` are both required**, each erroring in the style `register` already uses for a
missing `--file`.

**A structural first.** `import` is the first command that sits under the 0/1/2 exit contract but is
**not** an admin-plane client — it dials an upstream, never our admin plane. So it takes
`clientAnnotations()` (P5 §5) but **not** `clientFlags{}`: `--addr` and `--timeout` would be meaningless
on it, and `SIMULACRA_ADDR` must not silently retarget an import. It registers its own `--timeout`
bounding the whole walk rather than one RPC. That the exit annotation and the flag set are separable is
a property phase 5's seam already has; this command is what demonstrates it.

**Exit codes.** 0 on success, 2 on any failure — including interruption, via `errInterrupted`. There is
no assertion here, so 1 is unreachable, which is correct: 1 is reserved for `verify`'s "the assertion
ran and did not hold".

**Signal and write ordering.** Per the phase 5 plan's amendment 11 (`docs/superpowers/plans/2026-09-13-m3-p5-cli-client-commands.md`)
— *establish the signal context and finish local validation before the first side effect* — `signalContext` is installed before dialing and before any
file is created. The set is buffered in memory (descriptor sets are KBs to low MBs), written to a temp
file in the destination directory, and `os.Rename`d into place. An interrupt or a mid-walk failure
therefore never leaves a truncated `schema.binpb` behind, and never clobbers a good existing one.

**Output.** A summary line to stdout via `newPayloadWriter` (matching `register`): `imported 3 file(s)
from 1 service(s) → schema.binpb`. `--output json` renders `{files, services, bytes, path}` for CI,
using the existing `outputFlag`/`writeJSON` helpers.

## 7. Decisions flagged for review

| | Decision | Rationale | Reversibility |
|---|---|---|---|
| **D1** | **No `--register`.** `import` writes a file; registering it is `schema register -f`. | The one-step form merges two servers' failure modes into one command's diagnostics. Two commands compose, and the file is worth having on disk regardless — check it in, reuse it in CI, feed it to `serve --descriptors`. | Purely additive later. |
| **D2** | **No `--header k=v`.** | Deferred, not dismissed. Real staging reflection endpoints often require `authorization`, and grpcurl has it — **this is the most likely first follow-up**, and should be revisited the moment a real authenticated upstream appears. | Purely additive. |
| **D3** | **No `--insecure-skip-verify`.** | Self-signed staging certs are common, but a TLS-verification bypass deserves its own decision rather than riding along in a phase about descriptor walking. | Purely additive. |

## 8. Testing

| Layer | Tests |
|---|---|
| `Closure` (fake fetcher, no server) | Diamond dependency fetched once; missing dependency errors naming both the file and its importer; conflicting bytes for one filename errors naming the file; cycle detected rather than hung; topological order asserted; reflection services skipped |
| Transport, v1 | Against Simulacra's own data plane (F3: the v1 path is what it exercises) |
| Transport, v1alpha fallback | **Requires a dedicated server** — a stock grpc-go server with only `reflection.NewServer(opts)` registered, because F3 makes the fallback unreachable against ourselves |
| Skip list | **Requires a stock grpc-go server** as the upstream, because per F5 Simulacra does not advertise its own reflection services and so cannot discriminate this test |
| Dogfood round-trip (M3 §12) | Boot `server.Start` with `testdata/protos`; import from its data plane; register the result into a **second** fresh server; assert `schema list` output matches across both. States "registry in → identical descriptor set out" as an observable equality rather than a byte comparison |
| CLI | Missing `--reflect`/`-o`; unreachable upstream → exit 2; interrupt mid-walk → exit 2 **and no file at the destination**; `--output json` shape; existing `schema.binpb` survives a failed import |

## 9. Risks

| Risk | Mitigation |
|---|---|
| v1/v1alpha wire-compatibility assumption (F1) silently breaks | Both protos are frozen; F1 probes the actual behavior rather than assuming it. The v1alpha fallback test would fail loudly if it regressed. |
| An upstream returns a partial closure that our recursion cannot complete | The missing-dependency error names the file and its importer, so the diagnostic points at the upstream's bug rather than ours. |
| Imported sets bloat with an upstream's entire service surface | Out of scope by design — `ListServices` is the unit of import. A `--service` filter is a natural follow-up alongside D2. |
