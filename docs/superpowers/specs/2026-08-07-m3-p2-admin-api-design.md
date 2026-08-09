# Simulacra M3 Phase 2 — Admin API Protos, buf Module, Committed Codegen: Design

**Goal (M3 design §13.2):** land `api/simulacra/admin/v1/*.proto` as a buf module, commit the
generated Go code under `gen/`, and gate both with CI lint / breaking / sync checks.

**Reference:** `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §5 (admin API surface),
§6 (core changes the API mirrors), §13 (phasing). `PROPOSAL.md` §4 (control plane).

Phase 1 (`server/` facade) is complete at `65c1380`. This phase adds no runtime behavior: it
defines the public admin contract and the toolchain that keeps generated code honest. Handlers
arrive in Phase 4.

---

## 1. Scope

### In scope

1. `api/simulacra/admin/v1/{schema,stub,journal,verify,control}.proto` — the complete §5 surface.
2. A buf v2 workspace at the repo root (`buf.yaml`, `buf.gen.yaml`) with `api` as its one module.
3. Committed Go codegen: `gen/simulacra/admin/v1` (package `adminv1`) and
   `gen/simulacra/admin/v1/adminv1connect` (package `adminv1connect`).
4. A pinned, hermetic codegen toolchain in a nested `tools/` module, driven by a `Makefile`.
5. CI: `buf lint`, `buf breaking` against `main`, and a "`gen/` is in sync" check.
6. One Go contract test that fails if the generated surface loses a service or an RPC.

### Out of scope (later phases)

- Handler implementations, the h2c server, `/healthz`, `serve --admin` — **Phase 4**.
- Core extensions the API describes (store origins/IDs, registry mutability, journal watch,
  `Call.StubID`) — **Phase 3**.
- CLI client commands — Phase 5. Reflection import — Phase 6. SDKs — Phases 7/9. Dockerfile — Phase 8.
- `VerifySequence`, server-side upstream import, admin auth, TLS — M3 non-goals (design §2).

---

## 2. Repository layout

```
simulacra/
├── buf.yaml                     # NEW  v2 workspace: modules[api], lint STANDARD, breaking FILE
├── buf.gen.yaml                 # NEW  local plugins → gen/
├── Makefile                     # NEW  tools / generate / lint-api / breaking-api
├── bin/                         # NEW  gitignored; pinned tool binaries land here
├── api/
│   └── simulacra/admin/v1/
│       ├── schema.proto         # NEW
│       ├── stub.proto           # NEW
│       ├── journal.proto        # NEW
│       ├── verify.proto         # NEW
│       └── control.proto        # NEW
├── gen/simulacra/admin/v1/      # NEW  committed, generated, never hand-edited
│   ├── *.pb.go                  #      package adminv1
│   └── adminv1connect/*.go      #      package adminv1connect
├── internal/admin/
│   ├── doc.go                   # NEW  package placeholder; Phase 4 fills it with handlers
│   └── contract_test.go         # NEW  guards the generated surface
├── tools/go.mod                 # NEW  nested module; tool directives only
└── .gitignore                   # EDIT add /bin/
```

**Why the buf module lives under `api/` but the config lives at the root.** buf v2 configures a
workspace in one root `buf.yaml` that lists its modules. Root config means `buf lint`,
`buf breaking`, and `buf generate` all run from the repo root with no `-C` gymnastics, and adding
a second module later (should the data-plane conformance corpus ever want one) is a one-line
change. The module path is still `api`, exactly as design §5 specifies.

---

## 3. Toolchain

`tools/go.mod` is a nested Go module (`github.com/yinghanhung/simulacra/tools`) whose only purpose
is to pin executables via Go 1.24 `tool` directives:

- `github.com/bufbuild/buf/cmd/buf`
- `google.golang.org/protobuf/cmd/protoc-gen-go`
- `connectrpc.com/connect/cmd/protoc-gen-connect-go`

```make
GOBIN := $(CURDIR)/bin
# Invoke the pinned buf by absolute path, with ./bin prepended to PATH so buf
# can find its protoc-gen-* plugins. Setting PATH inline rather than with a
# global `export` is required for GNU Make 3.81 (stock macOS), which does not
# propagate exported variables to recipe lines with no shell metacharacters.
BUF := PATH="$(GOBIN):$$PATH" $(GOBIN)/buf

TOOLS := $(GOBIN)/buf $(GOBIN)/protoc-gen-go $(GOBIN)/protoc-gen-connect-go

# Overridable because origin/main is the right baseline only on pull requests;
# see §7 for what CI passes on a push.
BREAKING_AGAINST ?= origin/main

.DEFAULT_GOAL := lint-api

.PHONY: tools generate lint-api breaking-api

# These are file targets, not phony, so make skips the install recipe once the
# pinned binaries are newer than tools/go.mod and tools/go.sum — routine
# invocations of generate/lint-api/breaking-api don't reinstall the toolchain
# every time. All three are listed so deleting any one of them reinstalls.
$(TOOLS): tools/go.mod tools/go.sum
	GOBIN=$(GOBIN) go -C tools install tool

tools: $(TOOLS)             ## phony convenience alias for a fresh clone

generate: $(TOOLS)
	$(BUF) generate

lint-api: $(TOOLS)
	$(BUF) lint

breaking-api: $(TOOLS)
	$(BUF) breaking --against '.git#ref=$(BREAKING_AGAINST)'
```

This is the final state, reached in three corrections after the first implementation: `export PATH`
does not reach recipe lines with no shell metacharacters under GNU Make 3.81, so plain `buf`
invocations failed to find the plugins and were replaced with the `BUF :=` inline-PATH form; the
phony `tools` target was converted to the file-target form above so that
`lint-api`/`generate`/`breaking-api` skip reinstalling the toolchain when nothing pinning it changed;
and that file target was then widened from `$(GOBIN)/buf` alone to all three binaries.

Keying the sentinel on buf alone was wrong even though one `go install tool` writes all three: buf's
mtime only stands in for the others when `bin/` is complete. Delete `bin/protoc-gen-go` from an
otherwise-current `bin/` and make sees its sentinel satisfied, skips the install, and `make generate`
then fails inside buf on a missing plugin — reproduced before the fix. GNU Make 3.81 has no
grouped-target (`&:`) syntax, so `$(TOOLS): …` is an ordinary multi-target rule: the recipe runs once
per out-of-date target, but the first run installs all three, leaving the rest up to date, so it
still runs exactly once. Verified for all-present (no-op), one-missing, and all-missing.

Three properties this buys:

- **Hermetic and pinned.** Everyone — contributor and CI — runs the same buf and the same
  `protoc-gen-*`, which is the precondition for a meaningful "`gen/` is in sync" check. A
  version-drifted generator produces a diff and reds the build for no real reason.
- **The public module stays clean.** `go get github.com/yinghanhung/simulacra` must not drag in
  buf's dependency graph. A nested module keeps that graph out of the root `go.mod` entirely;
  root-module `tool` directives would not.
- **No network dependency at generate time.** Plugins are local binaries, not BSR remote plugins.

Nested modules are excluded from the root module's `./...` patterns automatically, so
`go build ./...` and `go test ./...` never see `tools/`.

**No `buf.lock`.** The protos import only well-known types (`any`, `duration`, `timestamp`), which
buf bundles. There is no BSR dependency, so generation works offline.

### `buf.gen.yaml`

```yaml
version: v2
clean: true
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
```

`clean: true` makes `buf generate` remove files it no longer writes, not just add or update them.
Without it, deleting or renaming a proto leaves the corresponding `.pb.go` / `.connect.go` orphaned
in `gen/` — the "gen/ is in sync" CI check (§6) does not catch this, because a plain `git add -A gen`
+ `git diff --cached` sees no change to a file nothing touched. A rename is worse: the stale and new
files would both register the same proto path and panic at package init.

No managed mode. Each `.proto` declares its own
`option go_package = "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1;adminv1";`. The
explicit `;adminv1` suffix is what yields package `adminv1`; managed mode's `go_package_prefix`
would leave protoc-gen-go to derive the package name from the last path element and produce
`package v1`.

### Dependency added to the root module

`connectrpc.com/connect` (current v1). Generated connect code lives in the root module, so the
dependency lands here rather than in Phase 4. It has no dependencies beyond
`google.golang.org/protobuf`. Phase 4 adds only `golang.org/x/net` (for h2c).

---

## 4. Proto surface

All five files share `syntax = "proto3"`, `package simulacra.admin.v1`, and the `go_package`
option above. The layout is one file per service, with the messages that service owns. Every RPC
gets a dedicated `<Rpc>Request` / `<Rpc>Response` pair (buf lint `RPC_REQUEST_RESPONSE_UNIQUE`),
which is why empty request/response messages appear rather than `google.protobuf.Empty`.

### 4.1 `schema.proto`

```proto
service SchemaService {
  rpc RegisterSchemas(RegisterSchemasRequest) returns (RegisterSchemasResponse);
  rpc ListServices(ListServicesRequest) returns (ListServicesResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

message RegisterSchemasRequest {
  // Serialized google.protobuf.FileDescriptorSet, applied all-or-nothing.
  bytes descriptor_set = 1;
}
message RegisterSchemasResponse {
  repeated string registered_files = 1;  // newly added paths; empty on idempotent re-register
  int32 service_count = 2;               // services in the registry after the swap
}

message ListServicesRequest {}
message ListServicesResponse { repeated ServiceInfo services = 1; }

message ServiceInfo {
  string name = 1;                  // fully qualified, e.g. shop.v1.OrderService
  string file = 2;                  // descriptor path it came from
  repeated MethodInfo methods = 3;
}
message MethodInfo {
  string name = 1;                  // bare method name, e.g. GetOrder
  string input_type = 2;            // fully qualified
  string output_type = 3;
  bool client_streaming = 4;
  bool server_streaming = 5;
}
```

### 4.2 `stub.proto`

```proto
service StubService {
  rpc CreateStub(CreateStubRequest) returns (CreateStubResponse);
  rpc ListStubs(ListStubsRequest) returns (ListStubsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc DeleteStub(DeleteStubRequest) returns (DeleteStubResponse);
  rpc ReplaceAllStubs(ReplaceAllStubsRequest) returns (ReplaceAllStubsResponse);
  rpc ExportStubs(ExportStubsRequest) returns (ExportStubsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

enum StubOrigin {
  STUB_ORIGIN_UNSPECIFIED = 0;
  STUB_ORIGIN_FILE = 1;
  STUB_ORIGIN_API = 2;
}

enum StubShape {
  STUB_SHAPE_UNSPECIFIED = 0;
  STUB_SHAPE_UNARY = 1;
  STUB_SHAPE_SERVER_STREAM = 2;
  STUB_SHAPE_CLIENT_STREAM = 3;
  STUB_SHAPE_BIDI_STREAM = 4;
}

// Stub is the read-only envelope around a stub document (design §3 decision A3).
message Stub {
  string id = 1;
  string method = 2;         // normalized "/pkg.Service/Method"
  StubShape shape = 3;
  int32 priority = 4;
  int32 times = 5;           // 0 means unlimited, mirroring the file grammar
  StubOrigin origin = 6;
  string source = 7;         // "<path>#<index>" for files, "api" for API stubs
  int64 hits = 8;
  string document = 9;       // normalized YAML mapping for this one stub
}

message CreateStubRequest {
  // Exactly one stub as a YAML or JSON mapping. A sequence is INVALID_ARGUMENT.
  string document = 1;
}
message CreateStubResponse { Stub stub = 1; }

message ListStubsRequest {
  string method = 1;         // optional filter, normalized "/pkg.Service/Method" as in Stub.method
  StubOrigin origin = 2;     // optional filter; UNSPECIFIED means all origins
}
message ListStubsResponse { repeated Stub stubs = 1; }

message DeleteStubRequest { string id = 1; }
message DeleteStubResponse {}

message ReplaceAllStubsRequest { repeated string documents = 1; }
message ReplaceAllStubsResponse { repeated Stub stubs = 1; }

message ExportStubsRequest {}
message ExportStubsResponse {
  string document = 1;       // one file-grammar YAML sequence of the API-origin stubs
  int32 stub_count = 2;
}
```

`ExportStubs` is not a redundant `ListStubs`: the envelope carries per-stub **mappings** (the API
grammar), while export returns a single YAML **sequence** — the file grammar — ready to write to
disk. That difference is the whole point of `simulacra stub export`.

### 4.3 `journal.proto`

```proto
import "google/protobuf/duration.proto";
import "google/protobuf/timestamp.proto";

service JournalService {
  rpc ListCalls(ListCallsRequest) returns (ListCallsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc WatchCalls(WatchCallsRequest) returns (stream WatchCallsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc ResetJournal(ResetJournalRequest) returns (ResetJournalResponse);
}

// DecodedMessage carries a data-plane message to clients that do not know its
// type: JSON for argument capture in any language, wire_bytes for typed capture
// against the client's own generated classes.
message DecodedMessage {
  string type_name = 1;      // fully-qualified message name
  bytes wire_bytes = 2;      // canonical protobuf encoding
  string json = 3;           // protojson, rendered against the server registry
}

// Exactly one of values / binary_values is populated, chosen by the key:
// "-bin" keys carry arbitrary bytes, every other key carries ASCII text.
message MetadataEntry {
  string key = 1;
  repeated string values = 2;        // keys not ending in "-bin"
  repeated bytes binary_values = 3;  // keys ending in "-bin", raw (not base64) bytes
}

message CallStatus {
  int32 code = 1;                              // gRPC status code; 0 is OK
  string message = 2;
  repeated DecodedMessage details = 3;         // deliberately not google.protobuf.Any
}

message Call {
  uint64 seq = 1;
  string method = 2;
  // request_metadata is deliberately qualified, not "metadata": it holds
  // request metadata only, and response headers/trailers (stub.Respond.Metadata,
  // stub.Respond.Trailers) are a plausible future addition that would leave an
  // unqualified name ambiguous.
  repeated MetadataEntry request_metadata = 3;
  repeated DecodedMessage requests = 4;
  repeated DecodedMessage responses = 5;
  CallStatus status = 6;
  string matched_stub_id = 7;
  google.protobuf.Timestamp start = 8;
  google.protobuf.Duration duration = 9;
}

message ListCallsRequest {
  string method = 1;         // optional filter, normalized "/pkg.Service/Method" as in Stub.method
  int32 limit = 2;           // 0 means no limit
}
message ListCallsResponse { repeated Call calls = 1; }  // newest first

message WatchCallsRequest { string method = 1; }  // optional filter, same form as ListCallsRequest.method
message WatchCallsResponse { Call call = 1; }

message ResetJournalRequest {}
message ResetJournalResponse {}
```

`request_metadata` is a `repeated MetadataEntry` rather than a `map<string, string>` because gRPC
metadata is multi-valued (`metadata.MD` is `map[string][]string`) and proto3 maps cannot hold
repeated values. It is named `request_metadata`, not `metadata`, because response headers and
trailers (`stub.Respond.Metadata`, `stub.Respond.Trailers`) are a plausible future addition to
`Call`, and an unqualified name would then be permanently ambiguous; renaming after `api/` reaches
`main` would be a JSON-name break, so the qualified name was adopted before that cost applied.

`MetadataEntry` splits text from binary values because a proto3 `string` must be valid UTF-8 and
`proto.Marshal` rejects one that is not — verified: marshaling `{0x00, 0xff, 0xfe, 0x80}` into a
`repeated string` field fails with `string field contains invalid UTF-8`. gRPC `-bin` metadata is
arbitrary bytes (grpc-go's `metadata.MD` holds it already base64-decoded), so the obvious direct
conversion from `metadata.MD` would produce journal entries that fail to serialize at all. Making
every value `bytes` would also work but would base64 ordinary text metadata in JSON, so the split
follows the same `-bin` rule gRPC itself uses; the key tells a client which field to read.

`CallStatus.details` is `DecodedMessage`, not `google.protobuf.Any`, for a related runtime failure.
Status details are built from dynamically registered types (`internal/stub.compileStatus` packs
whatever the stub's schema registry resolves), which are absent from `protoregistry.GlobalTypes`.
Connect v1.20's default JSON codec marshals with a zero `protojson.MarshalOptions`
(`codec.go:159`), whose nil `Resolver` falls back to `GlobalTypes` (`encode.go:148`) — so an `Any`
holding a runtime-only type serializes fine over binary protobuf and fails over JSON, breaking
`ListCalls`/`WatchCalls` for exactly the stubs that set details. `DecodedMessage` already solves this
for request/response payloads: the server renders `json` against its own registry, and `wire_bytes`
still lets a client decode against its own generated types. Reusing it avoids requiring every
deployment to install a registry-aware JSON codec.

### 4.4 `verify.proto`

```proto
import "simulacra/admin/v1/journal.proto";

service VerifyService {
  rpc VerifyCalls(VerifyCallsRequest) returns (VerifyCallsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

// Times mirrors internal/journal.Times. Deliberately NOT a oneof: at_least and
// at_most combine into a range. never excludes every numeric assertion. The
// server validates with the existing journal.Times.Validate.
message Times {
  optional int32 exactly = 1;
  optional int32 at_least = 2;
  optional int32 at_most = 3;
  bool never = 4;
}

message VerifyCallsRequest {
  string method = 1;             // normalized "/pkg.Service/Method" as in Stub.method
  string matcher_document = 2;   // stub-grammar match block; empty matches any call to method
  Times times = 3;
}
message VerifyCallsResponse {
  bool passed = 1;
  int32 matched = 2;
  string explanation = 3;                // human-readable verdict
  repeated CallExplanation actual = 4;   // populated on failure
}

message CallExplanation {
  Call call = 1;
  string nearest_miss = 2;   // why this call did not match, from the shared diagnostic engine
}
```

### 4.5 `control.proto`

```proto
service ControlService {
  rpc GetServerInfo(GetServerInfoRequest) returns (GetServerInfoResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc Reset(ResetRequest) returns (ResetResponse);
  rpc Shutdown(ShutdownRequest) returns (ShutdownResponse);
}

message GetServerInfoRequest {}
message GetServerInfoResponse {
  string version = 1;
  string data_addr = 2;
  string admin_addr = 3;
  int32 service_count = 4;
  int32 stub_count = 5;
  int32 journal_count = 6;
  int32 journal_capacity = 7;
}

message ResetRequest {
  // Presence-tracked: an omitted field means true (reset it); explicit false skips.
  //
  // Forward-compatibility trap: this "omitted means true" convention applies
  // only to stubs and journal. Any reset target added later must default to
  // NOT resetting when omitted, or every existing client already sending a
  // bare ResetRequest{} would silently start resetting it too.
  optional bool stubs = 1;
  optional bool journal = 2;
}
message ResetResponse {}

message ShutdownRequest {}
message ShutdownResponse {}
```

The binary has no version string today (`internal/cli/root.go`); `version` is declared now and
populated when Phase 4 wires `GetServerInfo`, from a build-stamped variable.

---

## 5. Corrections to the M3 design §5

Reading the core types surfaced three places where §5's sketch does not match the code. The protos
follow the code; §5's table is the older sketch.

| §5 says | Code says | Resolution |
|---|---|---|
| `times` is a `oneof exactly/at_least/at_most` + `never` | `journal.Times` (`internal/journal/verify.go:17`) allows `at_least` **and** `at_most` together as a range, enforced by `Validate` | `Times` is a plain message with three `optional int32` fields and a `bool never`. A oneof would make legal assertions unrepresentable. |
| `Call` carries `matched_stub_id` | `journal.Call` has `StubSource` only; `StubID` arrives in Phase 3 (design §6) | The field is declared now — the proto is the contract. Phase 3 adds the core field; Phase 4 populates it. |
| Stub envelope has a `shape` | `match.Shape` has five values: the four RPC shapes plus `BidiRule` (a bidi stub authored with `rules:` rather than a script) | `StubShape` exposes the four RPC shapes. `BidiRule` is an authoring mode, not a wire shape, and is already visible in the returned `document`. |

---

## 6. CI

A new `api` job in `.github/workflows/ci.yml`, alongside the unchanged `test` job:

```yaml
  api:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0            # buf breaking needs main's history
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
      - run: make tools
      - run: make lint-api
      - name: buf breaking
        env:
          EVENT_NAME: ${{ github.event_name }}
          BEFORE_SHA: ${{ github.event.before }}
          FORCED: ${{ github.event.forced }}
        run: |
          set -euo pipefail
          if [ "$EVENT_NAME" = "push" ]; then
            baseline="$BEFORE_SHA"
            if [ "$baseline" = "0000000000000000000000000000000000000000" ]; then
              echo "branch was just created; no prior API state to break"
              exit 0
            fi
            if ! git cat-file -e "$baseline^{commit}" 2>/dev/null; then
              echo "pre-push commit $baseline not in checkout (forced=$FORCED); fetching it"
              git fetch --no-tags --quiet origin "$baseline" || true
            fi
            if ! git cat-file -e "$baseline^{commit}" 2>/dev/null; then
              echo "::error::pre-push commit $baseline is unreachable (forced=$FORCED); cannot compare api/ against the state this push replaced"
              exit 1
            fi
          else
            baseline=origin/main
            git rev-parse --verify -q "$baseline" >/dev/null || { echo "$baseline did not resolve"; exit 1; }
          fi
          if git cat-file -e "$baseline:api" 2>/dev/null; then
            make breaking-api BREAKING_AGAINST="$baseline"
          else
            echo "baseline $baseline has no api/ yet; skipping the first breaking check"
          fi
      - run: make generate
      - run: go build ./...
      - name: verify gen/ is in sync with api/
        run: git add -A gen && git diff --cached --exit-code -- gen
```

Three corrections after initial implementation: the existence guard originally checked only
`git cat-file -e origin/main:api`, which exits nonzero both when `origin/main` has no `api/` tree and
when `origin/main` fails to resolve at all — an unresolvable ref would have silently disabled the
breaking check forever instead of just for the bootstrap commit, so `git rev-parse --verify` now
fails the job loudly in that case. `go build ./...` runs after `make generate`, so a freshly
regenerated `gen/` is asserted to compile in CI, not just the committed bytes the `test` job already
covers — this also turns the rename/duplicate-registration failure mode into a loud build error
rather than a silent pass. And the baseline is now chosen per event.

**The baseline must differ by event.** `origin/main` is correct on a pull request and useless on a
push. Checkout runs *after* the push, and `fetch-depth: 0` fetches all branches, so `origin/main`
already points at the pushed tip: the comparison is HEAD-versus-HEAD and cannot fail, which quietly
exempts every commit that lands on `main` without a pull request — the one case the push trigger
exists to cover. `github.event.before` is the pre-push tip and is the only baseline that makes the
check mean anything there.

**A missing baseline fails the job.** A force push can orphan the pre-push tip, and `fetch-depth: 0`
fetches branches, not unreachable objects — so the commit may be absent from the checkout. The step
first asks the remote for it by SHA, which recovers the normal case. If it is still unreachable the
step exits 1 rather than skipping: skipping would mean a force push carrying a breaking change
lands with a green build, which is exactly the scenario the push trigger is meant to catch, and
"the baseline is gone" is not evidence that nothing broke. `github.event.forced` is carried into the
message so the log distinguishes a force push from a genuinely anomalous missing commit.

Branch creation is the single remaining skip, and it is not a hole: `before` is all zeros because no
prior state exists, so there is no published contract for the push to break.

Verified by extracting this `run:` block and exercising every path. Against the real repository: a
push whose pre-push commit contains `api/` runs the comparison and exits nonzero on a real break; a
push predating `api/` takes the bootstrap skip; branch creation skips; an unreachable pre-push SHA
exits 1 after the fetch attempt is refused by the remote; the pull-request path resolves
`origin/main`. Against a purpose-built force-push fixture (a bare remote whose tip was rewritten,
cloned over `file://` so the transfer is a real one and the orphaned commit is genuinely absent from
the checkout): the fetch recovers the orphaned commit and the comparison then runs. `buf breaking`
accepts a raw commit SHA in `.git#ref=`, which this depends on, and fetch-by-SHA against the real
GitHub remote was confirmed to work for a reachable SHA. Whether GitHub serves a *force-pushed-away*
SHA was not tested — doing so would require force-pushing the real repository — and the fail-closed
branch exists precisely so that either answer is safe.

The sync check stages before diffing so that **added and deleted** generated files fail it, not
just modified ones — a plain `git diff` would silently pass a newly generated file.

The existing `test` job needs no change; `go build ./...` and `go test -race ./...` now also cover
`gen/` and `internal/admin`.

**Bootstrap of `buf breaking` — resolved by testing, not left to chance.** Two facts were verified
against buf v1.72.0 while writing the implementation plan:

- Against a baseline with no `buf.yaml` — true of `origin/main` before this phase — buf cannot
  scope a module boundary and free-scans the whole repository instead of just `api/`. It does not
  silently pass, but the exact symptom depends on what other protos the repository contains: here
  it surfaces as import-resolution errors from the pre-existing `conformance/protos/**` fixtures,
  not a clean "missing api/" message. Either way the failure is hard, so the commit that first
  introduces `api/` would red the build without a guard.
- `--against '.git#branch=main'` does not resolve on pull-request checkouts, which have no local
  `main` branch. `--against '.git#ref=…'` resolves on both push and PR events, and accepts a raw
  commit SHA, which is what the push path passes.

Hence the guard above and the overridable `BREAKING_AGAINST` in the Makefile. The guard skips
exactly one run — the one that introduces `api/` — and is a permanent no-op afterwards. This
replaces the two-commit fallback an earlier draft of this section proposed.

---

## 7. Testing

Phase 2 ships no behavior, so the tests gate the *contract and the pipeline*:

| Gate | What it catches |
|---|---|
| `go build ./...` | Generated code that does not compile; a missing `connectrpc.com/connect` dependency |
| `internal/admin/contract_test.go` | A service, RPC, enum value, or idempotency level silently dropped or changed; a hand-edit to `gen/` that removes surface |
| CI sync check | `gen/` regenerated from a different `api/`, or edited by hand |
| `buf lint` | Convention drift in a public contract |
| `buf breaking` | An incompatible change to a published API |

`contract_test.go` lives in `internal/admin` — the package Phase 4 fills with handlers — so it
never has to move. `doc.go` exists in Phase 2 only to give the package a home and a doc comment.

The test asserts, against a literal expectation table:

1. Each of the five service descriptors is reachable from the generated file descriptors.
2. Each service has exactly the expected set of RPC names, with the expected streaming flags
   (`WatchCalls` server-streaming, everything else unary) and the expected fully-qualified
   request/response message names — the last of these added in the final review pass so a type
   swap on an unchanged RPC name fails the test directly, not just a name-and-streaming-flags check.
3. Each RPC's `idempotency_level`. `RPC_SAME_IDEMPOTENCY_LEVEL` is breaking under the `FILE`
   category, so a level dropped by accident can only be restored with a breaking exception.
4. The connect constructors exist, via compile-time references such as
   `var _ = adminv1connect.NewStubServiceClient`.
5. Both enums, by value name *and* number. Renumbering is silent at compile time and wrong on the
   wire: a client that already serialized `STUB_ORIGIN_API` as `2` keeps sending `2`.
6. `Times`' presence semantics survive codegen: setting only `at_least` leaves `exactly` and
   `at_most` absent, and `at_least`+`at_most` together are both present. This is the field-presence
   behavior §5's oneof would have lost, so it is worth a direct assertion.
7. `ResetRequest` distinguishes omitted from explicit `false`.
8. The two journal shapes whose "natural" definitions fail only at runtime: `binary_values` is
   `bytes` and round-trips invalid UTF-8, and `CallStatus.details` is `DecodedMessage` rather than
   `google.protobuf.Any`. See §4.3 for why each matters.

Every assertion above was mutation-tested: the proto was changed to the wrong form, `gen/`
regenerated, and the corresponding test confirmed to fail.

**What this does not cover.** The table asserts the service surface, enums, idempotency levels, and
the specific field decisions listed above — not every field of every message. From the second
landing onward that gap is closed by `buf breaking`, which checks all of it. The bootstrap commit is
the exception: it has no baseline, so its field-level shape is reviewed rather than machine-checked.
Expanding the table to every field would restate the whole schema in Go for one commit's worth of
coverage, so the decisions that are expensive to reverse are asserted and the rest is not.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| `buf breaking` has no baseline on the bootstrap commit | Confirmed to fail hard; handled by the CI existence guard (§6) |
| Generator version drift produces spurious sync-check diffs | Every binary is pinned in `tools/go.mod`; nobody uses a system buf |
| `connectrpc.com/connect` enters the core module a phase early | Deliberate: it is dependency-free beyond protobuf, and it makes `buf.gen.yaml`, `gen/`'s layout, and the sync check final rather than re-cut in Phase 4 |
| Contract test ossifies the API, making legitimate additions annoying | It asserts an expectation table, not a golden file: adding an RPC is a one-line test edit, which is the right amount of friction for a public contract |
| Committed `gen/` drifts from `api/` | The CI sync check is exactly this guard |
