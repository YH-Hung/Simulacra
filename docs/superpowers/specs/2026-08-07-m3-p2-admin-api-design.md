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
├── buf.yaml                     # NEW  v2 workspace: modules[api], lint STANDARD, breaking WIRE_JSON
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
export PATH := $(GOBIN):$(PATH)

.PHONY: tools generate lint-api breaking-api

tools:         ## install pinned codegen binaries into ./bin
	GOBIN=$(GOBIN) go -C tools install tool

generate: tools
	buf generate

lint-api: tools
	buf lint

breaking-api: tools
	buf breaking --against '.git#branch=main'
```

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
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
```

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
  rpc ListServices(ListServicesRequest) returns (ListServicesResponse);
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
  rpc ListStubs(ListStubsRequest) returns (ListStubsResponse);
  rpc DeleteStub(DeleteStubRequest) returns (DeleteStubResponse);
  rpc ReplaceAllStubs(ReplaceAllStubsRequest) returns (ReplaceAllStubsResponse);
  rpc ExportStubs(ExportStubsRequest) returns (ExportStubsResponse);
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
  string method = 1;         // optional filter
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
import "google/protobuf/any.proto";
import "google/protobuf/duration.proto";
import "google/protobuf/timestamp.proto";

service JournalService {
  rpc ListCalls(ListCallsRequest) returns (ListCallsResponse);
  rpc WatchCalls(WatchCallsRequest) returns (stream WatchCallsResponse);
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

message MetadataEntry {
  string key = 1;
  repeated string values = 2;
}

message CallStatus {
  int32 code = 1;                              // gRPC status code; 0 is OK
  string message = 2;
  repeated google.protobuf.Any details = 3;
}

message Call {
  uint64 seq = 1;
  string method = 2;
  repeated MetadataEntry metadata = 3;
  repeated DecodedMessage requests = 4;
  repeated DecodedMessage responses = 5;
  CallStatus status = 6;
  string matched_stub_id = 7;
  google.protobuf.Timestamp start = 8;
  google.protobuf.Duration duration = 9;
}

message ListCallsRequest {
  string method = 1;         // optional filter
  int32 limit = 2;           // 0 means no limit
}
message ListCallsResponse { repeated Call calls = 1; }  // newest first

message WatchCallsRequest { string method = 1; }
message WatchCallsResponse { Call call = 1; }

message ResetJournalRequest {}
message ResetJournalResponse {}
```

`metadata` is a `repeated MetadataEntry` rather than a `map<string, string>` because gRPC metadata
is multi-valued (`metadata.MD` is `map[string][]string`) and proto3 maps cannot hold repeated
values.

### 4.4 `verify.proto`

```proto
import "simulacra/admin/v1/journal.proto";

service VerifyService {
  rpc VerifyCalls(VerifyCallsRequest) returns (VerifyCallsResponse);
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
  string method = 1;
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
  rpc GetServerInfo(GetServerInfoRequest) returns (GetServerInfoResponse);
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
        run: |
          if git cat-file -e origin/main:api 2>/dev/null; then
            make breaking-api
          else
            echo "baseline origin/main has no api/ yet; skipping the first breaking check"
          fi
      - run: make generate
      - name: verify gen/ is in sync with api/
        run: git add -A gen && git diff --cached --exit-code -- gen
```

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
  `main` branch. `--against '.git#ref=origin/main'` resolves on both push and PR events.

Hence the guard above and `ref=origin/main` in the Makefile. The guard skips exactly one run — the
one that introduces `api/` — and is a permanent no-op afterwards. This replaces the two-commit
fallback an earlier draft of this section proposed.

---

## 7. Testing

Phase 2 ships no behavior, so the tests gate the *contract and the pipeline*:

| Gate | What it catches |
|---|---|
| `go build ./...` | Generated code that does not compile; a missing `connectrpc.com/connect` dependency |
| `internal/admin/contract_test.go` | A service or RPC silently dropped from the protos; a hand-edit to `gen/` that removes surface |
| CI sync check | `gen/` regenerated from a different `api/`, or edited by hand |
| `buf lint` | Convention drift in a public contract |
| `buf breaking` | An incompatible change to a published API |

`contract_test.go` lives in `internal/admin` — the package Phase 4 fills with handlers — so it
never has to move. `doc.go` exists in Phase 2 only to give the package a home and a doc comment.

The test asserts, against a literal expectation table:

1. Each of the five service descriptors is reachable from the generated file descriptors.
2. Each service has exactly the expected set of RPC names, with the expected streaming flags
   (`WatchCalls` server-streaming, everything else unary).
3. The connect constructors exist, via compile-time references such as
   `var _ = adminv1connect.NewStubServiceClient`.
4. `Times`' presence semantics survive codegen: setting only `at_least` leaves `exactly` and
   `at_most` absent, and `at_least`+`at_most` together are both present. This is the field-presence
   behavior §5's oneof would have lost, so it is worth a direct assertion.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| `buf breaking` has no baseline on the bootstrap commit | Confirmed to fail hard; handled by the CI existence guard (§6) |
| Generator version drift produces spurious sync-check diffs | Every binary is pinned in `tools/go.mod`; nobody uses a system buf |
| `connectrpc.com/connect` enters the core module a phase early | Deliberate: it is dependency-free beyond protobuf, and it makes `buf.gen.yaml`, `gen/`'s layout, and the sync check final rather than re-cut in Phase 4 |
| Contract test ossifies the API, making legitimate additions annoying | It asserts an expectation table, not a golden file: adding an RPC is a one-line test edit, which is the right amount of friction for a public contract |
| Committed `gen/` drifts from `api/` | The CI sync check is exactly this guard |
