# Simulacra M3 Phase 4b — Admin Data Services Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `SchemaService`, `StubService`, `JournalService`, and `VerifyService` on the Phase 4a control plane, with shared call rendering, the core-error → connect-code mapper, per-service request caps, and `WatchCalls` streams that end cleanly when teardown begins.

**Architecture:** `internal/admin` translates between the frozen `simulacra.admin.v1` contract and the Phase 3 core (`schema.Registry`, `stub.Store`, `journal.Journal`): one file per service, `render.go` for call rendering and the invalid-UTF-8 rule, `errors.go` for the mapper. Three small core changes make that translation exact — a typed unknown-method error, call snapshots on verification reports, and int32 bounds on stub integers. `server` changes by one `Deps` field, `Stopping`, which `WatchCalls` uses to end its streams when teardown begins.

**Tech Stack:** Go 1.25.0, module `github.com/yinghanhung/simulacra`. `connectrpc.com/connect` v1.20.0, `google.golang.org/grpc` v1.82.0, `google.golang.org/protobuf` v1.36.11, and `golang.org/x/net` v0.53.0 are already direct dependencies. No module is added.

**Spec:** `docs/superpowers/specs/2026-09-12-m3-p4b-admin-data-services-design.md`, cited as "design §N". Approved at `e21b2ab` on branch `feat/m3-p4b-admin-data-services`. Phase 4a is complete at `11446f4`.

## Global Constraints

- **No changes to `api/` or `gen/`.** The contract was frozen in Phase 2.
- **No new modules:** `go.mod` and `go.sum` stay unchanged.
- **Exactly three core changes** (design §3): `schema.ErrUnknownMethod` (Task 1); `journal.Miss.Call` and `journal.Report.UnexpectedMatches []*Call` (Task 2); int32 bounds in `stub.Compiler.Compile` (Task 3). `internal/match`, `internal/dataplane`, and `internal/cli` are not modified.
- **`server` production code changes only** by wiring `Deps.Stopping: s.stopping` in `startAdminPlane`, plus comment corrections (Task 6).
- **Every handler returns its error through `connectError`.** Every failure caused by the request's own content is wrapped with `invalidArgument` where it is consumed. Error messages are never rewritten (design §7).
- **Every `string` field written into an `adminv1` response passes through `validUTF8`** (design §5.2).
- **Request caps:** `maxRequestBytes = 4 << 20` for `ControlService`, `StubService`, `JournalService`, and `VerifyService`; `maxSchemaRequestBytes = 32 << 20` for `SchemaService`; the mux-wide and outer transport wrappers use `maxSchemaRequestBytes`. Each service route applies its cap both as `connect.WithReadMaxBytes` and as `http.MaxBytesHandler`, with the same value. Caps are never removed.
- **Unchanged from 4a and never "simplified":** `http2.ConfigureServer(srv, h2s)` stays in `Install`; `ReadTimeout` and `WriteTimeout` stay unset on the admin `http.Server`.
- **`WatchCalls` checks `Stopping` without blocking immediately before every `Send`, after rendering** (design §6).
- **No service struct embeds its `Unimplemented…Handler` once its last RPC lands.** Each shell embeds it only until then, so each red phase reports `unimplemented`.
- **Every test binds `:0`.** No test binds `127.0.0.1:6566`.
- **protojson output is not byte-stable** — protobuf-go deliberately randomizes whitespace — so tests decode `json` with `encoding/json` and never compare it as a string.
- **Every task lands green:** `go build ./... && go vet ./... && go test ./... -count=1`. Tasks 10 and 12 are additionally verified with `-race -count=1`.

## Pre-verified facts

Every claim below was executed against this repository before the plan was written, by throwaway probe tests that were deleted afterwards. The values are what the probes printed.

1. **Oversized stub integers compile today.** `stub.ParseDocument` followed by `stub.Compile` accepts `times: 2147483648` and `priority: 2147483648` on this 64-bit host, and `int32(...)` of either is `-2147483648`.
2. **Invalid UTF-8 reaches the journal.** A raw HTTP/2 client sending the metadata `x-probe: ok\xffbad`, and another sending the `:path` `/shop.v1.OrderService/Get\xffOrder`, are both accepted by grpc-go v1.82.0, and the journal records them unchanged. `proto.Marshal` of an `adminv1.MetadataEntry` holding such a value fails with `string field contains invalid UTF-8`, and `protojson.Marshal` fails too.
3. **`Registry.RegisterSet` accepts non-UTF-8 paths**, including a file `bad\xffsvc.proto` that declares `probe.v1.ProbeService`. `Registry.Services()` then lists that service with the raw path.
4. **YAML rejects invalid UTF-8:** `stub.ParseDocument` returns `parsing document: yaml: invalid leading UTF-8 octet`.
5. **protojson cannot render an unresolvable `Any`:** `proto: google.protobuf.Any: unable to resolve "type.googleapis.com/unknown.v1.Thing": not found`.
6. **`strings.ToValidUTF8(s, "�")`** turns `"ok\xffbad"` into `"ok�bad"`, and a run of bad bytes, `"a\xff\xfeb"`, into `"a�b"`.
7. **Nested size caps behave as designed.** Each service was mounted as `http.MaxBytesHandler(handler, cap)` with `connect.WithReadMaxBytes(cap)`, inside a 32 MiB mux-wide wrapper and a 32 MiB outer wrapper around `h2c.NewHandler`. Requests were padded with an unknown field.
   - 8 MiB to a 4 MiB unary route: `resource_exhausted` over connect-go/HTTP/1.1, `ResourceExhausted` over grpc-go/h2c.
   - 8 MiB to a 4 MiB server-streaming route (`WatchCalls`): the same on both.
   - 8 MiB `RegisterSchemasRequest` on the 32 MiB route: decoded and answered normally on both.
   - 40 MiB on the 32 MiB route: `ResourceExhausted` on both.

   The probe used x/net's prior-knowledge path. `Install`'s own `servePriorKnowledge` hands streams to the same `routes` handler.
8. **A padded `RegisterSchemasRequest` still registers.** A `FileDescriptorSet` built by ranging `Registry.Snapshot()` of an `AddProtoDir("testdata/protos")` registry through `protodesc.ToFileDescriptorProto` is self-contained. `RegisterSet` into an empty registry adds, once sorted, exactly `google/protobuf/any.proto`, `google/protobuf/timestamp.proto`, and `shop/v1/order.proto`, and `Services()` then has length 1.
9. **`UNAVAILABLE` mid-stream is observable.** A server-streaming handler that sends one message and then returns `connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))` is seen by connect-go over HTTP/1.1 as one message followed by `unavailable: server shutting down`, and by grpc-go over h2c as one message followed by `rpc error: code = Unavailable desc = server shutting down`.
10. **`stub.RenderSequence(nil)` returns `"[]\n"`** (already pinned by `TestRenderSequenceOfNothingIsEmptyList`), and `stub.LoadDirs` on a file holding it returns 0 stubs and no errors.
11. **connect v1.20.0** maps `http.MaxBytesError` to `CodeResourceExhausted` (`wrapIfMaxBytesError`), maps `context.Canceled` and `context.DeadlineExceeded` to `CodeCanceled` and `CodeDeadlineExceeded` (`wrapIfContextError`), and passes an existing `*connect.Error` through.
12. **`dataplane.New` registers `grpc/health/v1/health.proto` into the registry** (4a pre-verified fact 11). A schema-less server's registry therefore holds one service, and two after `testdata/protos` is registered.
13. **The data plane records one request and one response for a matched unary call.** `receiveSingleRequest` appends to `call.Requests` (`internal/dataplane/server.go:262`) and `sendSingle` appends to `call.Responses` (`:311`). `Call.Err` is set only on failure (`:89-93`).
14. **`testdata/protos/shop/v1/order.proto`:** `OrderService` declares, in this order, `GetOrder` (unary), `WatchOrder` (server-streaming), `UploadOrders` (client-streaming), and `Chat` (bidi, `ChatMessage` in and out). `GetOrderRequest` has `order_id` (1) and `payload google.protobuf.Any` (9); `GetOrderResponse` has `note` (3); `Customer` has `id` (1).
15. **Matcher explanations quote strings with `%q`** (`internal/match/explain.go`). A message rule on `order_id` explains a miss as `message order_id: expected to equal "o-1"; actual "o-2"` (`internal/journal/verify_test.go:140`).
16. **Store ids.** `Store.Add` mints `api-<n>` from a counter; `ReplaceOrigin(OriginAPI, …)` continues the same counter. A `ReplaceAllStubs` that fails during compilation never reaches the store, so it consumes no id.
17. **Invalid stubs never reach a running store.** The watcher rejects a reload as a whole on any load error (`reconcileStubDirs`, `server/watcher.go:189`), and `server.Start` fails on any invalid stub (`loadStubs`, `server/server.go:406`).
18. **Generated surface** (`gen/simulacra/admin/v1/adminv1connect`):
    - Handler interfaces `SchemaServiceHandler`, `StubServiceHandler`, `JournalServiceHandler`, and `VerifyServiceHandler`. `JournalServiceHandler` includes `WatchCalls(context.Context, *connect.Request[v1.WatchCallsRequest], *connect.ServerStream[v1.WatchCallsResponse]) error`.
    - Constructors `New<Service>Handler(svc, opts ...connect.HandlerOption) (string, http.Handler)` and embeddable `Unimplemented<Service>Handler` structs.
    - Procedure constants such as `JournalServiceWatchCallsProcedure`.
    - Message fields as used below: `RegisteredFiles`, `ServiceCount`, `Id`, `MatchedStubId`, `RequestMetadata`, `BinaryValues`, `Json`, `NearestMiss`, and `Times.Exactly *int32`.

## File structure

```
simulacra/
├── internal/schema/
│   ├── registry.go            # EDIT   Task 1: ErrUnknownMethod
│   └── registry_test.go       # EXTEND Task 1
├── internal/journal/
│   ├── verify.go              # EDIT   Task 2: Miss.Call, Report.UnexpectedMatches []*Call
│   └── verify_test.go         # EDIT   Task 2
├── internal/stub/
│   ├── stub.go                # EDIT   Task 3: int32 bounds in Compile
│   ├── stub_test.go           # EXTEND Task 3
│   └── document_test.go       # EXTEND Task 1: the typed error survives Compile/CompileMatch
├── internal/admin/
│   ├── errors.go              # NEW    Task 4: connectError, invalidArgument
│   ├── errors_test.go         # NEW    Task 4
│   ├── render.go              # NEW    Task 5: renderCall, validUTF8
│   ├── render_test.go         # NEW    Task 5
│   ├── admin.go               # EDIT   Task 6: Deps.Stopping, per-service caps, mounts
│   ├── admin_test.go          # EDIT   Task 6
│   ├── schema.go              # NEW    Task 6 shell → Task 7
│   ├── schema_test.go         # NEW    Task 7
│   ├── stub.go                # NEW    Task 6 shell → Task 8
│   ├── stub_test.go           # NEW    Task 8
│   ├── journal.go             # NEW    Task 6 shell → Task 9 → Task 10
│   ├── journal_test.go        # NEW    Tasks 9–10
│   ├── verify.go              # NEW    Task 6 shell → Task 11
│   ├── verify_test.go         # NEW    Task 11
│   └── export_test.go         # EXTEND Tasks 4, 5, 8, 10
└── server/
    ├── admin.go               # EDIT   Task 6: Deps.Stopping wiring, comments
    ├── server.go              # EDIT   Task 6: comments only
    ├── admin_test.go          # EDIT   Task 6: two stale comments
    └── admin_data_test.go     # NEW    Task 12: end to end
```

**Dependency order:**
- Tasks 1, 2, 3, 5, and 6 are independent of each other.
- Task 4 needs Task 1.
- Tasks 7–11 need Tasks 4, 5, and 6. In addition, Task 8 needs Tasks 1 and 3, Task 10 needs Task 9, and Task 11 needs Tasks 1 and 2.
- Task 12 comes last.

---

### Task 1: `schema.ErrUnknownMethod`

**Files:**
- Modify: `internal/schema/registry.go` (new declarations above `LookupMethod`; two returns inside it)
- Modify: `internal/schema/registry_test.go` (import `errors`; new test)
- Modify: `internal/stub/document_test.go` (imports `errors` and `internal/schema`; new test)

**Interfaces:**
- Consumes: nothing new.
- Produces: `var schema.ErrUnknownMethod error`. When `(*Registry).LookupMethod` fails because the service is absent, or the method is absent on a present service, the error satisfies `errors.Is(err, schema.ErrUnknownMethod)` and its text is unchanged. Tasks 4, 8, and 11 rely on this.

Design §3.1. The two not-registered errors become a small type whose `Error()` keeps today's text and whose `Is` matches the sentinel. A malformed name, and a name that resolves to something other than a service, stay untyped.

- [ ] **Step 1: Write the failing tests**

Add `"errors"` to the import block of `internal/schema/registry_test.go`, then append:

```go
// Unknown-method errors are typed so the admin plane can answer NOT_FOUND, and
// their text is pinned: loader and check diagnostics already print it.
func TestLookupMethodTypesUnknownMethods(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	cases := []struct {
		name     string
		method   string
		unknown  bool
		wantText string
	}{
		{"absent service", "/no.such.Service/GetOrder", true,
			`service "no.such.Service" is not registered (no schema source declares it)`},
		{"absent method", "/shop.v1.OrderService/NoSuchMethod", true,
			`method "NoSuchMethod" not found on service "shop.v1.OrderService"`},
		{"malformed", "garbage", false,
			`invalid method name "garbage" (want package.Service/Method)`},
		{"empty method segment", "shop.v1.OrderService/", false,
			`invalid method name "shop.v1.OrderService/" (want package.Service/Method)`},
		{"not a service", "shop.v1.GetOrderRequest/Get", false,
			`"shop.v1.GetOrderRequest" is not a service`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.LookupMethod(tc.method)
			if err == nil {
				t.Fatalf("LookupMethod(%q) returned a nil error", tc.method)
			}
			if got := errors.Is(err, ErrUnknownMethod); got != tc.unknown {
				t.Errorf("errors.Is(err, ErrUnknownMethod) = %v, want %v (err: %v)", got, tc.unknown, err)
			}
			if err.Error() != tc.wantText {
				t.Errorf("error text = %q, want %q", err.Error(), tc.wantText)
			}
		})
	}
}
```

In `internal/stub/document_test.go`, add `"errors"` and `"github.com/yinghanhung/simulacra/internal/schema"` to the imports, then append:

```go
// The admin plane classifies an unknown method with errors.Is, so the typed
// error must survive both compile entry points it passes through.
func TestUnknownMethodSurvivesCompileAndCompileMatch(t *testing.T) {
	reg := testRegistry(t)
	_, err := Compile(reg, Stub{Method: "shop.v1.OrderService/Nope"}, "document")
	if !errors.Is(err, schema.ErrUnknownMethod) {
		t.Errorf("Compile error = %v, want it to wrap schema.ErrUnknownMethod", err)
	}
	_, err = NewCompiler(reg).CompileMatch("no.such.Service/Get", nil)
	if !errors.Is(err, schema.ErrUnknownMethod) {
		t.Errorf("CompileMatch error = %v, want it to wrap schema.ErrUnknownMethod", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/schema/ ./internal/stub/ -run 'TestLookupMethodTypesUnknownMethods|TestUnknownMethodSurvivesCompileAndCompileMatch' -count=1`
Expected: FAIL — both packages fail to build: `undefined: ErrUnknownMethod` and `undefined: schema.ErrUnknownMethod`.

- [ ] **Step 3: Write the implementation**

In `internal/schema/registry.go`, insert immediately above the line `// LookupMethod resolves "pkg.Service/Method" or "/pkg.Service/Method".`:

```go
// ErrUnknownMethod reports a well-formed method name that no registered schema
// declares: the service is absent, or the method is absent on a present
// service. The admin plane answers it with NOT_FOUND.
var ErrUnknownMethod = errors.New("unknown method")

// unknownMethodError keeps LookupMethod's established message text, which
// loader and check diagnostics print, while matching ErrUnknownMethod.
type unknownMethodError struct{ msg string }

func (e *unknownMethodError) Error() string        { return e.msg }
func (e *unknownMethodError) Is(target error) bool { return target == ErrUnknownMethod }
```

Then, inside `LookupMethod`, replace the block from `d, err := r.current().files.FindDescriptorByName(...)` through `return m, nil` with:

```go
	d, err := r.current().files.FindDescriptorByName(protoreflect.FullName(svcName))
	if err != nil {
		return nil, &unknownMethodError{msg: fmt.Sprintf("service %q is not registered (no schema source declares it)", svcName)}
	}
	svc, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a service", svcName)
	}
	m := svc.Methods().ByName(protoreflect.Name(methodName))
	if m == nil {
		return nil, &unknownMethodError{msg: fmt.Sprintf("method %q not found on service %q", methodName, svcName)}
	}
	return m, nil
```

`errors` and `fmt` are already imported.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/schema/ ./internal/stub/ -count=1`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS. No message text changed, so no other test notices.

- [ ] **Step 6: Commit**

```bash
git add internal/schema/registry.go internal/schema/registry_test.go internal/stub/document_test.go
git commit -m "feat(schema): type unknown-method lookups as ErrUnknownMethod"
```

---

### Task 2: `journal.Report` carries call snapshots

**Files:**
- Modify: `internal/journal/verify.go` (`Miss`, `Report`, and the body of `Verify` from `type mismatch struct` to the end of the function)
- Modify: `internal/journal/verify_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:

  ```go
  type Miss struct {
  	Call    *Call
  	Reasons []string
  }

  type Report struct {
  	Pass              bool
  	Matched           int
  	Considered        int
  	Want              string
  	Misses            []Miss
  	UnexpectedMatches []*Call
  }
  ```

  Task 11 renders `Misses[i].Call` and every entry of `UnexpectedMatches`.

Design §3.2. `VerifyCalls` needs the calls, not their sequence numbers; joining sequence numbers against a second `List()` would read a different snapshot. `Verify` already works from its own `List()`, which clones, so the report carries those clones.

- [ ] **Step 1: Update the tests to the new shape**

In `internal/journal/verify_test.go`:

1. In `TestVerifyCountsMatchingCalls`, replace

   ```go
   			if !reflect.DeepEqual(report.UnexpectedMatches, tt.unexpected) {
   				t.Errorf("UnexpectedMatches = %v, want %v", report.UnexpectedMatches, tt.unexpected)
   			}
   ```

   with

   ```go
   			if got := seqsOf(report.UnexpectedMatches); !reflect.DeepEqual(got, tt.unexpected) {
   				t.Errorf("UnexpectedMatches seqs = %v, want %v", got, tt.unexpected)
   			}
   ```

   The table's `unexpected []uint64` field is unchanged.

2. In `TestVerifyFailureExplainsNonMatchingCall`, replace

   ```go
   	wantMisses := []Miss{{Seq: 3, Reasons: []string{`message order_id: expected to equal "o-1"; actual "o-2"`}}}
   	if report.Pass || report.Want != "exactly 3" || !reflect.DeepEqual(report.Misses, wantMisses) {
   		t.Fatalf("report = %#v, want failure with %#v", report, wantMisses)
   	}
   ```

   with

   ```go
   	if report.Pass || report.Want != "exactly 3" || len(report.Misses) != 1 {
   		t.Fatalf("report = %#v, want a failure with one miss", report)
   	}
   	miss := report.Misses[0]
   	wantReasons := []string{`message order_id: expected to equal "o-1"; actual "o-2"`}
   	if miss.Call == nil || miss.Call.Seq != 3 || !reflect.DeepEqual(miss.Reasons, wantReasons) {
   		t.Fatalf("miss = %+v, want the seq-3 call with reasons %q", miss, wantReasons)
   	}
   ```

3. Append:

   ```go
   // seqsOf lists the sequence numbers of calls, preserving nil so a table can
   // state "no calls" as a nil want.
   func seqsOf(calls []*Call) []uint64 {
   	if calls == nil {
   		return nil
   	}
   	seqs := make([]uint64, len(calls))
   	for i, call := range calls {
   		seqs[i] = call.Seq
   	}
   	return seqs
   }

   // A report carries the calls its verdict was computed from — whole calls, not
   // sequence numbers a caller would have to look up again in a journal that may
   // have been reset or overwritten since.
   func TestVerifyReportCarriesTheJudgedCalls(t *testing.T) {
   	j, orderOne, _ := verificationFixture(t)
   	under, err := Verify(j, "/shop.v1.OrderService/GetOrder", orderOne, Times{Exactly: intPtr(3)})
   	if err != nil {
   		t.Fatalf("Verify (too few): %v", err)
   	}
   	over, err := Verify(j, "/shop.v1.OrderService/GetOrder", orderOne, Times{AtMost: intPtr(1)})
   	if err != nil {
   		t.Fatalf("Verify (too many): %v", err)
   	}
   	j.Reset()

   	if len(under.Misses) != 1 || under.Misses[0].Call == nil {
   		t.Fatalf("too-few Misses = %+v, want one miss carrying its call", under.Misses)
   	}
   	request := under.Misses[0].Call.Requests[0]
   	if got := request.Get(request.Descriptor().Fields().ByName("order_id")).String(); got != "o-2" {
   		t.Errorf("miss call order_id = %q, want o-2, the call that did not match", got)
   	}
   	if got := seqsOf(over.UnexpectedMatches); !reflect.DeepEqual(got, []uint64{1, 2}) {
   		t.Fatalf("too-many UnexpectedMatches seqs = %v, want [1 2]", got)
   	}
   	for _, call := range over.UnexpectedMatches {
   		if call.Method != "/shop.v1.OrderService/GetOrder" || len(call.Requests) != 1 {
   			t.Errorf("unexpected match = %+v, want a whole GetOrder call", call)
   		}
   	}
   }
   ```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/journal/ -count=1`
Expected: FAIL — the test file does not build: `miss.Call undefined (type Miss has no field or method Call)`, and `report.UnexpectedMatches` (type `[]uint64`) cannot be passed to `seqsOf`.

- [ ] **Step 3: Write the implementation**

In `internal/journal/verify.go`, replace the `Miss` and `Report` declarations with:

```go
// Miss explains why a considered call did not satisfy the matcher. Call is the
// snapshot the verdict was computed from.
type Miss struct {
	Call    *Call
	Reasons []string
}

// Report summarizes a journal verification. Misses and UnexpectedMatches carry
// the call snapshots Verify judged, so a caller rendering them shows exactly
// the calls the verdict came from.
type Report struct {
	Pass              bool
	Matched           int
	Considered        int
	Want              string
	Misses            []Miss
	UnexpectedMatches []*Call
}
```

In `Verify`, replace everything from `type mismatch struct {` to the end of the function with:

```go
	type mismatch struct {
		call  *Call
		input match.Input
	}
	var mismatches []mismatch
	var matchedCalls []*Call
	for _, call := range j.List() {
		if call == nil || (wantMethod != "" && call.Method != wantMethod) {
			continue
		}
		report.Considered++
		input, err := callInput(call)
		if err != nil {
			return Report{}, err
		}
		if matcher == nil || matcher.Eval(input) {
			report.Matched++
			matchedCalls = append(matchedCalls, call)
			continue
		}
		mismatches = append(mismatches, mismatch{call: call, input: input})
	}
	report.Pass = times.ok(report.Matched)
	if report.Pass {
		return report, nil
	}
	if !times.under(report.Matched) {
		report.UnexpectedMatches = matchedCalls
		return report, nil
	}
	report.Misses = make([]Miss, 0, len(mismatches))
	for _, missed := range mismatches {
		report.Misses = append(report.Misses, Miss{
			Call:    missed.call,
			Reasons: matcher.Explain(missed.input),
		})
	}
	return report, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/journal/ -count=1`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS. Only `internal/journal`'s own tests read `Report`.

- [ ] **Step 6: Commit**

```bash
git add internal/journal/verify.go internal/journal/verify_test.go
git commit -m "feat(journal): carry the judged call snapshots on Verify reports"
```

---

### Task 3: Bound stub `priority` and `times` to int32

**Files:**
- Modify: `internal/stub/stub.go` (import `math`; the start of `Compiler.Compile`)
- Modify: `internal/stub/stub_test.go` (new test and helper)

**Interfaces:**
- Consumes: nothing new.
- Produces: `Compiler.Compile` rejects `times` above 2147483647 with `<source>: times must be at most 2147483647 (got N)`, and `priority` outside the int32 range with `<source>: priority must be between -2147483648 and 2147483647 (got N)`. Task 8's envelope conversion relies on it.

Design §3.3. The bound lives in the compiler because stub files and API documents share it, so an out-of-range value fails the same way from a file (startup, hot reload, `check`) and from an API document.

- [ ] **Step 1: Write the failing test**

Append to `internal/stub/stub_test.go`:

```go
// priority and times cross the admin API as int32, so Compile — shared by stub
// files and API documents — rejects values outside that range (design §3.3).
// Both ingest paths are exercised: a document through ParseDocument, and a
// file through LoadDirs, which serve, hot reload, and check all call.
func TestCompileBoundsPriorityAndTimesToInt32(t *testing.T) {
	reg := testRegistry(t)
	const stubPrefix = "method: shop.v1.OrderService/GetOrder\nrespond:\n  message: {}\n"
	rejected := []struct {
		line string
		want string
	}{
		{"times: 2147483648", "times must be at most 2147483647 (got 2147483648)"},
		{"priority: 2147483648", "priority must be between -2147483648 and 2147483647 (got 2147483648)"},
		{"priority: -2147483649", "priority must be between -2147483648 and 2147483647 (got -2147483649)"},
	}
	for _, tc := range rejected {
		t.Run("document "+tc.line, func(t *testing.T) {
			s, _, err := ParseDocument([]byte(stubPrefix + tc.line + "\n"))
			if err != nil {
				t.Fatalf("ParseDocument: %v", err)
			}
			if _, err := Compile(reg, s, "document"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile error = %v, want one containing %q", err, tc.want)
			}
		})
		t.Run("file "+tc.line, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "stubs.yaml", asFileItem(stubPrefix+tc.line+"\n"))
			stubs, errs := LoadDirs(reg, []string{dir})
			if len(stubs) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.want) {
				t.Fatalf("LoadDirs = %d stub(s), errors %v; want no stubs and one error containing %q",
					len(stubs), errs, tc.want)
			}
		})
	}

	// The int32 boundary values themselves are accepted.
	for _, doc := range []string{
		stubPrefix + "priority: -2147483648\ntimes: 2147483647\n",
		stubPrefix + "priority: 2147483647\n",
	} {
		s, _, err := ParseDocument([]byte(doc))
		if err != nil {
			t.Fatalf("ParseDocument: %v", err)
		}
		c, err := Compile(reg, s, "document")
		if err != nil {
			t.Fatalf("Compile at the int32 boundary: %v", err)
		}
		if c.Priority != s.Priority || c.Times != s.Times {
			t.Fatalf("compiled priority/times = %d/%d, want %d/%d", c.Priority, c.Times, s.Priority, s.Times)
		}
	}
}

// asFileItem turns a single-stub document into a one-item stub file.
func asFileItem(doc string) string {
	return "- " + strings.ReplaceAll(strings.TrimSuffix(doc, "\n"), "\n", "\n  ") + "\n"
}
```

`writeFile` already exists in `internal/stub/loader_test.go`, in the same package.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/stub/ -run TestCompileBoundsPriorityAndTimesToInt32 -count=1`
Expected: FAIL — every `document …` subtest reports `Compile error = <nil>`, and every `file …` subtest reports `LoadDirs = 1 stub(s), errors []`.

- [ ] **Step 3: Write the implementation**

Add `"math"` to the imports of `internal/stub/stub.go`. In `Compiler.Compile`, directly after the existing negative-`times` check, insert:

```go
	// priority and times cross the admin API as int32. Bounding them here, in
	// the compiler stub files and API documents share, keeps one accepted range
	// for both and stops it depending on the width of Go's int (design §3.3).
	if s.Times > math.MaxInt32 {
		return nil, fmt.Errorf("%s: times must be at most %d (got %d)", source, math.MaxInt32, s.Times)
	}
	if s.Priority < math.MinInt32 || s.Priority > math.MaxInt32 {
		return nil, fmt.Errorf("%s: priority must be between %d and %d (got %d)",
			source, math.MinInt32, math.MaxInt32, s.Priority)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/stub/ -count=1`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS. No existing stub fixture uses an out-of-range value.

- [ ] **Step 6: Commit**

```bash
git add internal/stub/stub.go internal/stub/stub_test.go
git commit -m "feat(stub): bound priority and times to int32 at compile time"
```

---

### Task 4: The core-error → connect-code mapper

**Files:**
- Create: `internal/admin/errors.go`
- Create: `internal/admin/errors_test.go`
- Modify: `internal/admin/export_test.go`

**Interfaces:**
- Consumes: `schema.ErrUnknownMethod` (Task 1); `stub.ErrStubNotFound`, `*stub.FileOwnedError`, and `journal.ErrSlowConsumer` (Phase 3).
- Produces, in package `admin`: `func invalidArgument(err error) error` and `func connectError(err error) error`. Test-only exports `admin.InvalidArgument` and `admin.ConnectError`. Every handler in Tasks 7–11 returns through `connectError`.

Design §7. Rows are checked in order: pass-through, then typed core errors, then the input mark, then `INTERNAL`. A typed error inside a marked one therefore wins.

- [ ] **Step 1: Write the failing tests**

Append to `internal/admin/export_test.go`:

```go
// Test-only views of the error mapper.
var (
	ConnectError    = connectError
	InvalidArgument = invalidArgument
)
```

Create `internal/admin/errors_test.go`:

```go
package admin_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// The design §7 table, row by row. Every mapped error comes back as a
// *connect.Error whose message is the original error's text, unchanged.
func TestConnectErrorMapsTheDesignTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"unknown method", fmt.Errorf("document: %w", schema.ErrUnknownMethod), connect.CodeNotFound},
		{"unknown method inside an input mark",
			admin.InvalidArgument(fmt.Errorf("document: %w", schema.ErrUnknownMethod)), connect.CodeNotFound},
		{"unknown stub id", fmt.Errorf("%w: %q", stub.ErrStubNotFound, "api-9"), connect.CodeNotFound},
		{"file-owned stub", &stub.FileOwnedError{ID: "stubs/a.yaml#0", Source: "stubs/a.yaml#0"},
			connect.CodeFailedPrecondition},
		{"slow consumer", journal.ErrSlowConsumer, connect.CodeResourceExhausted},
		{"marked input", admin.InvalidArgument(errors.New("parsing stub document: yaml: line 2: bad")),
			connect.CodeInvalidArgument},
		{"unmarked untyped", errors.New("rendering stub sequence: boom"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := admin.ConnectError(tc.err)
			var connectErr *connect.Error
			if !errors.As(got, &connectErr) {
				t.Fatalf("ConnectError(%v) = %T, want a *connect.Error", tc.err, got)
			}
			if connectErr.Code() != tc.want {
				t.Errorf("code = %v, want %v", connectErr.Code(), tc.want)
			}
			if connectErr.Message() != tc.err.Error() {
				t.Errorf("message = %q, want the original text %q", connectErr.Message(), tc.err.Error())
			}
		})
	}
}

// A connect error a handler built itself, and context errors connect maps on
// its own, come back as the very same value.
func TestConnectErrorPassesThroughConnectAndContextErrors(t *testing.T) {
	for _, err := range []error{
		connect.NewError(connect.CodeUnavailable, errors.New("server shutting down")),
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("sending: %w", context.Canceled),
	} {
		if got := admin.ConnectError(err); got != err {
			t.Errorf("ConnectError(%v) = %v, want the same error back", err, got)
		}
	}
	if got := admin.ConnectError(nil); got != nil {
		t.Errorf("ConnectError(nil) = %v, want nil", got)
	}
	if got := admin.InvalidArgument(nil); got != nil {
		t.Errorf("InvalidArgument(nil) = %v, want nil", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestConnectError' -count=1`
Expected: FAIL — the test package does not build: `undefined: connectError` and `undefined: invalidArgument` in `export_test.go`.

- [ ] **Step 3: Write the implementation**

Create `internal/admin/errors.go`:

```go
package admin

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// inputError marks a failure caused by the content of the request itself.
type inputError struct{ err error }

func (e *inputError) Error() string { return e.err.Error() }
func (e *inputError) Unwrap() error { return e.err }

// invalidArgument marks err as caused by the caller's input. Handlers apply it
// exactly where they parse, compile, or validate what the request carried.
// connectError turns it into INVALID_ARGUMENT unless a typed core error inside
// it says something more specific.
func invalidArgument(err error) error {
	if err == nil {
		return nil
	}
	return &inputError{err: err}
}

// connectError maps the error a handler is about to return onto a connect code
// (design §7). Rows are checked in order, and the message is never rewritten.
//
// An unmarked, untyped error is INTERNAL. The loader, compiler, CEL, and
// RegisterSet diagnostics are untyped, so they become INVALID_ARGUMENT only
// through an invalidArgument mark: a handler that forgets one surfaces INTERNAL
// in its tests instead of reporting a server fault as the client's.
func connectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Built by the handler, or mapped by connect itself.
		return err
	}
	var fileOwned *stub.FileOwnedError
	var input *inputError
	switch {
	case errors.Is(err, schema.ErrUnknownMethod), errors.Is(err, stub.ErrStubNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.As(err, &fileOwned):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, journal.ErrSlowConsumer):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.As(err, &input):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/admin/errors.go internal/admin/errors_test.go internal/admin/export_test.go
git commit -m "feat(admin): map core errors to connect codes"
```

---

### Task 5: Call rendering and `validUTF8`

**Files:**
- Create: `internal/admin/render.go`
- Create: `internal/admin/render_test.go`
- Modify: `internal/admin/export_test.go`

**Interfaces:**
- Consumes: `(*schema.Registry).Types() *schema.Types` and `journal.Call` (Phase 3).
- Produces, in package `admin`: `func validUTF8(s string) string` and `func renderCall(call *journal.Call, types *schema.Types) *adminv1.Call`. Test-only exports `admin.ValidUTF8` and `admin.RenderCall`. Test helpers in `render_test.go`, used by later tasks: `getOrderMessages(t, reg, orderID, note) (*dynamicpb.Message, *dynamicpb.Message)` and `jsonFields(t, text) map[string]any`.

Design §5. Rendering never fails: a message protojson cannot render keeps its wire bytes and gets an empty `json`, and every string passes through `validUTF8`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/admin/export_test.go`:

```go
// Test-only views of call rendering.
var (
	RenderCall = renderCall
	ValidUTF8  = validUTF8
)
```

Create `internal/admin/render_test.go`:

```go
package admin_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// getOrderMessages builds a GetOrder request with order_id and a GetOrder
// response with note, against reg.
func getOrderMessages(t *testing.T, reg *schema.Registry, orderID, note string) (*dynamicpb.Message, *dynamicpb.Message) {
	t.Helper()
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	req := dynamicpb.NewMessage(method.Input())
	req.Set(method.Input().Fields().ByName("order_id"), protoreflect.ValueOfString(orderID))
	resp := dynamicpb.NewMessage(method.Output())
	resp.Set(method.Output().Fields().ByName("note"), protoreflect.ValueOfString(note))
	return req, resp
}

// jsonFields decodes a DecodedMessage's json. protojson output is not
// byte-stable, so tests compare decoded fields and never the string.
func jsonFields(t *testing.T, text string) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatalf("decoding json %q: %v", text, err)
	}
	return fields
}

func TestRenderCallRendersMessagesMetadataAndStatus(t *testing.T) {
	reg := testDeps(t).Registry
	req, resp := getOrderMessages(t, reg, "o-1", "shipped")
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	call := &journal.Call{
		Seq:    7,
		Method: "/shop.v1.OrderService/GetOrder",
		Metadata: metadata.MD{
			"x-tenant":  {"acme", "beta"},
			"trace-bin": {"\x00\xff"},
			"a-first":   {"1"},
		},
		Requests:  []*dynamicpb.Message{req},
		Responses: []*dynamicpb.Message{resp},
		StubID:    "api-3",
		Start:     start,
		Duration:  1500 * time.Millisecond,
	}

	got := admin.RenderCall(call, reg.Types())

	if got.Seq != 7 || got.Method != "/shop.v1.OrderService/GetOrder" || got.MatchedStubId != "api-3" {
		t.Errorf("seq/method/matched_stub_id = %d/%q/%q", got.Seq, got.Method, got.MatchedStubId)
	}
	if !got.Start.AsTime().Equal(start) || got.Duration.AsDuration() != 1500*time.Millisecond {
		t.Errorf("start/duration = %v/%v", got.Start.AsTime(), got.Duration.AsDuration())
	}

	var keys []string
	for _, entry := range got.RequestMetadata {
		keys = append(keys, entry.Key)
	}
	if want := []string{"a-first", "trace-bin", "x-tenant"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("metadata keys = %q, want %q (sorted)", keys, want)
	}
	binary, text := got.RequestMetadata[1], got.RequestMetadata[2]
	if len(binary.Values) != 0 || len(binary.BinaryValues) != 1 || string(binary.BinaryValues[0]) != "\x00\xff" {
		t.Errorf("trace-bin entry = %v, want its bytes in binary_values only", binary)
	}
	if len(text.BinaryValues) != 0 || !reflect.DeepEqual(text.Values, []string{"acme", "beta"}) {
		t.Errorf("x-tenant entry = %v, want [acme beta] in values only", text)
	}

	if len(got.Requests) != 1 || len(got.Responses) != 1 {
		t.Fatalf("requests/responses = %d/%d, want 1/1", len(got.Requests), len(got.Responses))
	}
	request := got.Requests[0]
	if request.TypeName != "shop.v1.GetOrderRequest" {
		t.Errorf("request type_name = %q, want shop.v1.GetOrderRequest", request.TypeName)
	}
	fields := jsonFields(t, request.Json)
	if fields["order_id"] != "o-1" {
		t.Errorf("request json = %s, want order_id o-1 under its proto field name", request.Json)
	}
	if _, camel := fields["orderId"]; camel {
		t.Errorf("request json = %s uses the JSON name; matchers use proto names", request.Json)
	}
	decoded := dynamicpb.NewMessage(req.Descriptor())
	if err := proto.Unmarshal(request.WireBytes, decoded); err != nil || !proto.Equal(decoded, req) {
		t.Errorf("request wire_bytes do not decode back to the request (err %v)", err)
	}
	if note := jsonFields(t, got.Responses[0].Json)["note"]; note != "shipped" {
		t.Errorf("response json = %s, want note shipped", got.Responses[0].Json)
	}

	if got.Status == nil || got.Status.Code != 0 || got.Status.Message != "" || len(got.Status.Details) != 0 {
		t.Errorf("status of a call with no Err = %v, want a present status with code 0", got.Status)
	}
}

// Everything rendering cannot represent faithfully degrades instead of
// failing: an Any holding an unregistered type, an unresolvable status detail,
// and strings holding invalid UTF-8. The final marshals are the discriminator:
// a response carrying one invalid UTF-8 string does not serialize at all.
func TestRenderCallDegradesWhatItCannotRepresent(t *testing.T) {
	reg := testDeps(t).Registry
	req, _ := getOrderMessages(t, reg, "o-1", "")
	payload := req.Descriptor().Fields().ByName("payload")
	unknown := dynamicpb.NewMessage(payload.Message())
	unknown.Set(payload.Message().Fields().ByName("type_url"),
		protoreflect.ValueOfString("type.googleapis.com/unknown.v1.Thing"))
	unknown.Set(payload.Message().Fields().ByName("value"), protoreflect.ValueOfBytes([]byte{0x0a, 0x01, 'x'}))
	req.Set(payload, protoreflect.ValueOfMessage(unknown))

	customerType, err := reg.Types().FindMessageByName("shop.v1.Customer")
	if err != nil {
		t.Fatalf("FindMessageByName: %v", err)
	}
	customer := customerType.New()
	customer.Set(customer.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString("c-1"))
	known, err := anypb.New(customer.Interface())
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	unresolvable := &anypb.Any{TypeUrl: "type.googleapis.com/unknown.v1.Thing", Value: []byte{0x0a, 0x01, 'x'}}

	call := &journal.Call{
		Seq:      1,
		Method:   "/shop.v1.OrderService/Get\xffOrder",
		Metadata: metadata.MD{"x-probe": {"ok\xffbad"}},
		Requests: []*dynamicpb.Message{req},
		Err: status.FromProto(&spb.Status{
			Code:    int32(codes.NotFound),
			Message: "bad\xffmessage",
			Details: []*anypb.Any{known, unresolvable},
		}),
		StubID: "stubs/\xff.yaml#0",
	}

	got := admin.RenderCall(call, reg.Types())

	if got.Requests[0].Json != "" || len(got.Requests[0].WireBytes) == 0 {
		t.Errorf("request with an unregistered Any: json %q, %d wire bytes; want empty json and the wire bytes kept",
			got.Requests[0].Json, len(got.Requests[0].WireBytes))
	}
	if got.Status.Code != int32(codes.NotFound) || got.Status.Message != "bad�message" {
		t.Errorf("status = %d %q, want NotFound with U+FFFD in the message", got.Status.Code, got.Status.Message)
	}
	if len(got.Status.Details) != 2 {
		t.Fatalf("details = %d, want 2", len(got.Status.Details))
	}
	if d := got.Status.Details[0]; d.TypeName != "shop.v1.Customer" || jsonFields(t, d.Json)["id"] != "c-1" {
		t.Errorf("resolvable detail = %v, want shop.v1.Customer rendered to json", d)
	}
	if d := got.Status.Details[1]; d.TypeName != "unknown.v1.Thing" || d.Json != "" || string(d.WireBytes) != "\x0a\x01x" {
		t.Errorf("unresolvable detail = %v, want the URL's type name, the raw bytes, and empty json", d)
	}
	if got.Method != "/shop.v1.OrderService/Get�Order" {
		t.Errorf("method = %q, want U+FFFD in place of the invalid byte", got.Method)
	}
	if values := got.RequestMetadata[0].Values; len(values) != 1 || values[0] != "ok�bad" {
		t.Errorf("x-probe values = %q, want [ok�bad]", values)
	}
	if got.MatchedStubId != "stubs/�.yaml#0" {
		t.Errorf("matched_stub_id = %q, want U+FFFD in place of the invalid byte", got.MatchedStubId)
	}
	if _, err := proto.Marshal(got); err != nil {
		t.Fatalf("the rendered call does not marshal: %v", err)
	}
	if _, err := protojson.Marshal(got); err != nil {
		t.Fatalf("the rendered call does not marshal to JSON: %v", err)
	}
}

func TestValidUTF8(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"plain":      "plain",
		"café":       "café",
		"ok\xffbad":  "ok�bad",
		"a\xff\xfeb": "a�b",
	}
	for in, want := range cases {
		if got := admin.ValidUTF8(in); got != want {
			t.Errorf("ValidUTF8(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestRenderCall|TestValidUTF8' -count=1`
Expected: FAIL — the test package does not build: `undefined: renderCall` and `undefined: validUTF8` in `export_test.go`.

- [ ] **Step 3: Write the implementation**

Create `internal/admin/render.go`:

```go
package admin

import (
	"sort"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// validUTF8 replaces each run of invalid UTF-8 in s with U+FFFD and returns
// valid input unchanged. Every string field the admin plane writes into a
// response passes through it (design §5.2): proto3 refuses to marshal a string
// holding invalid UTF-8, and one such string fails the whole response — a
// single bad header would fail ListCalls for the entire journal. The journal,
// store, and registry keep the original bytes; this is presentation only.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "�") }

// renderCall turns a recorded call into its wire form (design §5.1). It never
// fails. Types resolve through the registry at render time, so a type
// registered after the call was recorded still renders.
func renderCall(call *journal.Call, types *schema.Types) *adminv1.Call {
	return &adminv1.Call{
		Seq:             call.Seq,
		Method:          validUTF8(call.Method),
		RequestMetadata: renderMetadata(call.Metadata),
		Requests:        renderMessages(call.Requests, types),
		Responses:       renderMessages(call.Responses, types),
		Status:          renderStatus(call, types),
		MatchedStubId:   validUTF8(call.StubID),
		Start:           timestamppb.New(call.Start),
		Duration:        durationpb.New(call.Duration),
	}
}

// renderMetadata emits one entry per key, keys sorted. The key suffix alone
// decides the field: "-bin" keys carry the bytes the application saw, and every
// other key carries text.
func renderMetadata(md metadata.MD) []*adminv1.MetadataEntry {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]*adminv1.MetadataEntry, 0, len(keys))
	for _, key := range keys {
		entry := &adminv1.MetadataEntry{Key: validUTF8(key)}
		if strings.HasSuffix(strings.ToLower(key), "-bin") {
			for _, value := range md[key] {
				entry.BinaryValues = append(entry.BinaryValues, []byte(value))
			}
		} else {
			for _, value := range md[key] {
				entry.Values = append(entry.Values, validUTF8(value))
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func renderMessages(messages []*dynamicpb.Message, types *schema.Types) []*adminv1.DecodedMessage {
	decoded := make([]*adminv1.DecodedMessage, 0, len(messages))
	for _, message := range messages {
		if message != nil {
			decoded = append(decoded, decodeMessage(message, types))
		}
	}
	return decoded
}

// decodeMessage renders one message for a client that may not know its type.
// wire_bytes and json are each best effort: a field that cannot be produced is
// left empty rather than failing the RPC. Both marshals allow partial messages,
// because the journal holds whatever the data plane decoded. json uses proto
// field names because matcher paths do.
func decodeMessage(message proto.Message, types *schema.Types) *adminv1.DecodedMessage {
	decoded := &adminv1.DecodedMessage{
		TypeName: validUTF8(string(message.ProtoReflect().Descriptor().FullName())),
	}
	if wire, err := (proto.MarshalOptions{Deterministic: true, AllowPartial: true}).Marshal(message); err == nil {
		decoded.WireBytes = wire
	}
	text, err := (protojson.MarshalOptions{UseProtoNames: true, AllowPartial: true, Resolver: types}).Marshal(message)
	if err == nil {
		decoded.Json = validUTF8(string(text))
	}
	return decoded
}

// renderStatus always returns a status. The data plane records Err only on
// failure, so a nil Err is code 0.
func renderStatus(call *journal.Call, types *schema.Types) *adminv1.CallStatus {
	if call.Err == nil {
		return &adminv1.CallStatus{}
	}
	st := call.Err.Proto()
	rendered := &adminv1.CallStatus{Code: st.GetCode(), Message: validUTF8(st.GetMessage())}
	for _, detail := range st.GetDetails() {
		rendered.Details = append(rendered.Details, decodeDetail(detail, types))
	}
	return rendered
}

// decodeDetail resolves a status detail's type through the registry. A detail
// whose type does not resolve keeps its raw bytes and the type name its URL
// carries, with json empty.
func decodeDetail(detail *anypb.Any, types *schema.Types) *adminv1.DecodedMessage {
	if messageType, err := types.FindMessageByURL(detail.GetTypeUrl()); err == nil {
		message := messageType.New().Interface()
		if err := (proto.UnmarshalOptions{AllowPartial: true, Resolver: types}).Unmarshal(detail.GetValue(), message); err == nil {
			return decodeMessage(message, types)
		}
	}
	name := detail.GetTypeUrl()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return &adminv1.DecodedMessage{TypeName: validUTF8(name), WireBytes: detail.GetValue()}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS.

- [ ] **Step 5: Mutation-check the discriminator**

Temporarily change `Values = append(entry.Values, validUTF8(value))` to `Values = append(entry.Values, value)` in `renderMetadata`, and run `go test ./internal/admin/ -run TestRenderCallDegradesWhatItCannotRepresent -count=1`.
Expected: FAIL at `the rendered call does not marshal`. Restore the line and re-run: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/admin/render.go internal/admin/render_test.go internal/admin/export_test.go
git commit -m "feat(admin): render journal calls and sanitize response strings"
```

---

### Task 6: `Install` mounts the data services — `Deps.Stopping` and per-service caps

**Files:**
- Modify: `internal/admin/admin.go` (the cap constants, `Deps`, `validate`, `Install`)
- Create: `internal/admin/schema.go`, `internal/admin/stub.go`, `internal/admin/journal.go`, `internal/admin/verify.go` (shells)
- Modify: `internal/admin/admin_test.go` (`testDeps`, `TestInstallRejectsIncompleteDeps`, the route test, a new cap test and helper)
- Modify: `server/admin.go` (wire `Stopping`; two comments)
- Modify: `server/server.go` (comments only)
- Modify: `server/admin_test.go` (two comments only)

**Interfaces:**
- Consumes: nothing from Tasks 1–5.
- Produces:
  - `Deps.Stopping <-chan struct{}`, required by `validate`.
  - `const maxSchemaRequestBytes = 32 << 20`.
  - Shell types `schemaService`, `stubService`, `journalService`, and `verifyService`. Each is `struct { adminv1connect.Unimplemented<Service>Handler; deps Deps }`. Tasks 7–11 implement them and remove the embedded field.
  - Test helper `padded[T proto.Message](msg T, n int) T` in `admin_test.go`.

Design §2 and §8. After this task every service route exists and enforces its own cap. The four data services still answer `Unimplemented`, which the cap test tolerates: a request the cap lets through only has to be decoded and answered, whatever the answer.

- [ ] **Step 1: Write the failing tests**

In `internal/admin/admin_test.go`:

1. Add `"slices"` and `"google.golang.org/protobuf/proto"` to the imports.
2. In `testDeps`, add `Stopping: make(chan struct{}),` after `Shutdown: func() {},`.
3. In `TestInstallRejectsIncompleteDeps`, add this entry to the `unset` map:

   ```go
   		"Stopping":  func(d *admin.Deps) { d.Stopping = nil },
   ```

4. Replace the whole `TestInstallMountsControlServiceRoute` function, including its comment, with:

   ```go
   // Every service is mounted. The assertion is "not 404", not "returns
   // Unimplemented", so it keeps passing unchanged as the handlers are filled in.
   func TestInstallMountsEveryServiceRoute(t *testing.T) {
   	ts := installed(t, testDeps(t))
   	for _, procedure := range []string{
   		adminv1connect.ControlServiceGetServerInfoProcedure,
   		adminv1connect.SchemaServiceListServicesProcedure,
   		adminv1connect.StubServiceListStubsProcedure,
   		adminv1connect.JournalServiceListCallsProcedure,
   		adminv1connect.VerifyServiceVerifyCallsProcedure,
   	} {
   		resp, err := ts.Client().Post(ts.URL+procedure, "application/json", strings.NewReader("{}"))
   		if err != nil {
   			t.Fatalf("POST %s: %v", procedure, err)
   		}
   		resp.Body.Close()
   		if resp.StatusCode == http.StatusNotFound {
   			t.Errorf("%s is not mounted: 404", procedure)
   		}
   	}
   }
   ```

5. Append:

   ```go
   // padded appends an unknown field of n bytes to msg. Decoding skips it, so the
   // request means exactly what it meant before; only a size cap can tell the two
   // apart.
   func padded[T proto.Message](msg T, n int) T {
   	field := protowire.AppendTag(nil, 1000, protowire.BytesType)
   	msg.ProtoReflect().SetUnknown(protowire.AppendBytes(field, bytes.Repeat([]byte{'A'}, n)))
   	return msg
   }

   // Each service caps its own requests (design §8). SchemaService takes whole
   // descriptor sets and accepts up to 32 MiB; every other service stays at 4 MiB.
   // A request the cap lets through is decoded and answered — whatever the answer,
   // it is not ResourceExhausted.
   func TestRequestSizeCapsArePerService(t *testing.T) {
   	ts := installed(t, testDeps(t))
   	ctx := context.Background()
   	schemas := adminv1connect.NewSchemaServiceClient(ts.Client(), ts.URL)

   	_, err := schemas.RegisterSchemas(ctx, connect.NewRequest(padded(&adminv1.RegisterSchemasRequest{}, 8<<20)))
   	if code := connect.CodeOf(err); err != nil && (code == connect.CodeResourceExhausted || code == connect.CodeUnknown) {
   		t.Errorf("an 8 MiB RegisterSchemas request = %v; SchemaService must accept it", err)
   	}
   	_, err = schemas.RegisterSchemas(ctx, connect.NewRequest(padded(&adminv1.RegisterSchemasRequest{}, 40<<20)))
   	if code := connect.CodeOf(err); code != connect.CodeResourceExhausted {
   		t.Errorf("a 40 MiB RegisterSchemas request = %v (code %v), want ResourceExhausted", err, code)
   	}

   	control := adminv1connect.NewControlServiceClient(ts.Client(), ts.URL)
   	stubs := adminv1connect.NewStubServiceClient(ts.Client(), ts.URL)
   	journals := adminv1connect.NewJournalServiceClient(ts.Client(), ts.URL)
   	verify := adminv1connect.NewVerifyServiceClient(ts.Client(), ts.URL)
   	const oversized = 8 << 20
   	others := map[string]struct {
   		call  func() error
   		codes []connect.Code // any one of these is accepted
   	}{
   		"ControlService.GetServerInfo": {
   			call: func() error {
   				_, err := control.GetServerInfo(ctx, connect.NewRequest(padded(&adminv1.GetServerInfoRequest{}, oversized)))
   				return err
   			},
   			codes: []connect.Code{connect.CodeResourceExhausted},
   		},
   		"StubService.ReplaceAllStubs": {
   			call: func() error {
   				_, err := stubs.ReplaceAllStubs(ctx, connect.NewRequest(padded(&adminv1.ReplaceAllStubsRequest{}, oversized)))
   				return err
   			},
   			codes: []connect.Code{connect.CodeResourceExhausted},
   		},
   		"JournalService.WatchCalls": {
   			call: func() error {
   				// Bounded: a WatchCalls request the cap wrongly admits stays open.
   				watchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
   				defer cancel()
   				stream, err := journals.WatchCalls(watchCtx, connect.NewRequest(padded(&adminv1.WatchCallsRequest{}, oversized)))
   				if err != nil {
   					return err
   				}
   				defer stream.Close()
   				for stream.Receive() {
   				}
   				return stream.Err()
   			},
   			// Flaky as a single code: over the streaming protocol the server can
   			// reject the oversized request while the client is still writing the
   			// 8 MiB body, so the client sometimes observes the broken transport
   			// (CodeInternal) before it ever reads the server's actual status
   			// (CodeResourceExhausted). Either way the request was rejected, which
   			// is the only thing production behavior promises here — a stream that
   			// the cap wrongly admits instead blocks until the context above
   			// expires, returning CodeDeadlineExceeded, which is neither of these.
   			codes: []connect.Code{connect.CodeResourceExhausted, connect.CodeInternal},
   		},
   		"VerifyService.VerifyCalls": {
   			call: func() error {
   				_, err := verify.VerifyCalls(ctx, connect.NewRequest(padded(&adminv1.VerifyCallsRequest{}, oversized)))
   				return err
   			},
   			codes: []connect.Code{connect.CodeResourceExhausted},
   		},
   	}
   	for name, c := range others {
   		code := connect.CodeOf(c.call())
   		if !slices.Contains(c.codes, code) {
   			t.Errorf("an 8 MiB %s request = code %v, want one of %v", name, code, c.codes)
   		}
   	}
   }
   ```

`TestOversizedRequestIsRejected` stays exactly as it is.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -count=1`
Expected: FAIL — the test package does not build: `unknown field Stopping in struct literal of type admin.Deps`.

- [ ] **Step 3: Write the implementation**

**`internal/admin/admin.go`.** Replace the `maxRequestBytes` doc comment and constant with:

```go
// maxRequestBytes caps both a single Connect message and the whole HTTP request
// stream for every service except SchemaService. The admin plane is
// unauthenticated by design (auth and TLS are M3 non-goals), so without a cap
// any peer can make the server allocate whatever it sends: a probe drove an
// 8 MiB GetServerInfo request to a 200 response.
//
// 4 MiB matches grpc-go's own default receive limit, which is the size callers
// of a gRPC-shaped API already expect.
const maxRequestBytes = 4 << 20

// maxSchemaRequestBytes is SchemaService's cap. RegisterSchemas carries whole
// descriptor sets, and `buf build` images include source info by default and
// grow with the repository (design §8). Raise it deliberately if a real set
// needs more; never remove the cap.
const maxSchemaRequestBytes = 32 << 20
```

In `Deps`, add after the `Shutdown` field:

```go

	// Stopping is closed when server teardown begins. WatchCalls ends its
	// streams on it, so an open tail does not hold teardown for the whole
	// grace period (design §6).
	Stopping <-chan struct{}
```

In `validate`, add after the `Shutdown` check:

```go
	if d.Stopping == nil {
		missing = append(missing, "Stopping")
	}
```

Replace the whole `Install` function body (its doc comment stays) with:

```go
func Install(srv *http.Server, deps Deps) error {
	if srv == nil {
		return errors.New("admin: Install requires a non-nil *http.Server")
	}
	if err := deps.validate(); err != nil {
		return err
	}

	mux := http.NewServeMux()
	// Each service caps both one decoded message (connect.WithReadMaxBytes) and
	// its own request stream (http.MaxBytesHandler), with the same limit;
	// connect turns an http.MaxBytesError into RESOURCE_EXHAUSTED. SchemaService
	// alone carries descriptor sets, so it alone gets the larger cap (design §8).
	path, handler := adminv1connect.NewControlServiceHandler(&controlService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	path, handler = adminv1connect.NewSchemaServiceHandler(&schemaService{deps: deps},
		connect.WithReadMaxBytes(maxSchemaRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxSchemaRequestBytes))
	path, handler = adminv1connect.NewStubServiceHandler(&stubService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	path, handler = adminv1connect.NewJournalServiceHandler(&journalService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	path, handler = adminv1connect.NewVerifyServiceHandler(&verifyService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})

	// routes bounds every request that reaches the mux, at the largest route
	// cap; each route enforces its own cap inside it. It is used both for
	// streams on connections this package upgrades itself and, inside
	// h2c.NewHandler, for streams on connections h2c upgrades — the h2 path
	// never passes through the outer wrapper below, so bounding only there
	// would leave every post-upgrade RPC unlimited.
	routes := http.MaxBytesHandler(mux, maxSchemaRequestBytes)

	h2s := &http2.Server{}
	srv.Handler = &transport{
		h2s: h2s,
		// The outer cap covers HTTP/1.1 requests and, importantly, the h2c
		// upgrade path: x/net's h2cUpgrade does io.ReadAll(r.Body) before any
		// handler runs, so this cap must be at least the largest route cap.
		fallback: http.MaxBytesHandler(h2c.NewHandler(routes, h2s), maxSchemaRequestBytes),
		streams:  routes,
	}
	// Mandatory, not tuning. See the doc comment above.
	if err := http2.ConfigureServer(srv, h2s); err != nil {
		return fmt.Errorf("admin: configuring http/2: %w", err)
	}
	return nil
}
```

**The four shells.** Create `internal/admin/schema.go`:

```go
package admin

import "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"

// schemaService implements simulacra.admin.v1.SchemaService. The embedded
// Unimplemented handler stands in until its RPCs are implemented and is
// removed then, so a later contract addition fails the build instead of
// answering Unimplemented.
type schemaService struct {
	adminv1connect.UnimplementedSchemaServiceHandler
	deps Deps
}
```

Create `internal/admin/stub.go`, `internal/admin/journal.go`, and `internal/admin/verify.go` the same way, each with its own complete content:

```go
package admin

import "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"

// stubService implements simulacra.admin.v1.StubService. The embedded
// Unimplemented handler stands in until its RPCs are implemented and is
// removed then, so a later contract addition fails the build instead of
// answering Unimplemented.
type stubService struct {
	adminv1connect.UnimplementedStubServiceHandler
	deps Deps
}
```

```go
package admin

import "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"

// journalService implements simulacra.admin.v1.JournalService. The embedded
// Unimplemented handler stands in until its RPCs are implemented and is
// removed then, so a later contract addition fails the build instead of
// answering Unimplemented.
type journalService struct {
	adminv1connect.UnimplementedJournalServiceHandler
	deps Deps
}
```

```go
package admin

import "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"

// verifyService implements simulacra.admin.v1.VerifyService. The embedded
// Unimplemented handler stands in until its RPCs are implemented and is
// removed then, so a later contract addition fails the build instead of
// answering Unimplemented.
type verifyService struct {
	adminv1connect.UnimplementedVerifyServiceHandler
	deps Deps
}
```

**`server/admin.go`.** In the `admin.Deps{…}` literal inside `startAdminPlane`, add after `Shutdown: s.begin,`:

```go
		// Closed by begin, before runTeardown starts, so WatchCalls ends its
		// streams instead of holding the admin drain open.
		Stopping: s.stopping,
```

Replace

```go
		// requests on a keep-alive connection. Deliberately absent:
		// ReadTimeout and WriteTimeout, which would each cap the duration of
		// a whole request/response and so would break Phase 4b's WatchCalls
		// server-streaming RPC. Do not add them to "complete" this set.
```

with

```go
		// requests on a keep-alive connection. Deliberately absent:
		// ReadTimeout and WriteTimeout, which would each cap the duration of
		// a whole request/response and so would break WatchCalls, a
		// server-streaming RPC. Do not add them to "complete" this set.
```

and in `stopAdminPlane` replace

```go
	// Step 2: the real drain. This is what lets an in-flight ShutdownResponse
	// flush and lets long-lived requests end. Phase 4b's WatchCalls makes it
	// load-bearing: a client tailing calls holds a connection open
	// indefinitely, and only this bound stops it holding teardown open too.
```

with

```go
	// Step 2: the real drain. This is what lets an in-flight ShutdownResponse
	// flush and lets long-lived requests end. WatchCalls ends its own streams
	// when Stopping closes, but a Send already past its shutdown check can
	// still block on a client that stopped reading; only this bound stops that
	// holding teardown open.
```

**`server/server.go`.** In the `shutdownGrace` doc comment replace

```go
// consume the whole thing: with a single stuck HTTP/1.1 request that is
// already possible, and it becomes the normal case once Phase 4b's
// WatchCalls lets a client hold a connection open indefinitely. Regardless of
```

with

```go
// consume the whole thing: a single stuck HTTP/1.1 request, or a WatchCalls
// Send blocked on a client that stopped reading, is enough. Regardless of
```

In the `dataGraceFloor` doc comment replace

```go
// runs to the deadline — the normal case once Phase 4b's WatchCalls lets a
// client hold a connection open indefinitely — would hand stopDataPlane an
```

with

```go
// runs to the deadline — a stuck HTTP/1.1 request, or a WatchCalls Send
// blocked on a client that stopped reading — would hand stopDataPlane an
```

In `Start` replace `// (Phase 4b's RegisterSchemas) — the container/SDK path, where the server` with `// (RegisterSchemas) — the container/SDK path, where the server`.

**`server/admin_test.go`.** Replace

```go
// Phase 4b's WatchCalls makes this load-bearing: a client tailing calls holds a
// connection open indefinitely, and only the bound stops it holding teardown
// open too. Tested before the RPC that needs it exists.
```

with

```go
// A long-lived admin request must not hold teardown open; only the drain bound
// ends it. WatchCalls ends its own streams when Stopping closes, but a Send
// blocked on a client that stopped reading still relies on this bound.
```

and replace

```go
// dataGraceFloor. runTeardown runs the admin drain first, and a single stuck
// HTTP/1.1 request — the normal case once Phase 4b's WatchCalls lets an admin
// client hold a connection open — makes that drain burn the whole
// shutdownGrace budget. Without a floor, stopDataPlane would then receive an
```

with

```go
// dataGraceFloor. runTeardown runs the admin drain first, and a single stuck
// HTTP/1.1 request makes that drain burn the whole shutdownGrace budget.
// Without a floor, stopDataPlane would then receive an
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ ./server/ -count=1`
Expected: PASS. The `server` suite passing also proves the wiring, because `Install` now rejects a `Deps` without `Stopping` and every `server` test with the admin plane on would fail to start.

- [ ] **Step 5: Confirm no stale references remain**

Run: `grep -rn --include='*.go' 'Phase 4b' . | grep -v '^./gen/'` and `gofmt -l internal server`
Expected: both print nothing.

- [ ] **Step 6: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/admin/admin.go internal/admin/admin_test.go internal/admin/schema.go internal/admin/stub.go internal/admin/journal.go internal/admin/verify.go server/admin.go server/server.go server/admin_test.go
git commit -m "feat(admin): mount the data services with per-service caps and Deps.Stopping"
```

---

### Task 7: `SchemaService`

**Files:**
- Modify: `internal/admin/schema.go` (replace the shell)
- Create: `internal/admin/schema_test.go`

**Interfaces:**
- Consumes: `connectError` and `invalidArgument` (Task 4); `validUTF8` (Task 5); the `schemaService` shell (Task 6); `(*schema.Registry).RegisterSet`, `Services`, `LookupMethod`, and `LookupMessage` (Phase 3).
- Produces: `(*schemaService).RegisterSchemas` and `(*schemaService).ListServices`, plus test helpers in `schema_test.go`: `testdataFiles(t) []*descriptorpb.FileDescriptorProto`, `marshalSet(t, files...) []byte`, and `emptyRegistryDeps(t) admin.Deps`.

Design §4.2 and §5.2. The non-UTF-8 descriptor path test runs here, at the handler level: nothing in `server` participates, and what it pins is the handler's own response serialization.

- [ ] **Step 1: Write the failing tests**

Create `internal/admin/schema_test.go`:

```go
package admin_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/schema"
)

func schemaClient(t *testing.T, deps admin.Deps) adminv1connect.SchemaServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewSchemaServiceClient(ts.Client(), ts.URL)
}

// testdataFiles returns testdata/protos as descriptor protos: a self-contained
// set of google/protobuf/any.proto, google/protobuf/timestamp.proto, and
// shop/v1/order.proto.
func testdataFiles(t *testing.T) []*descriptorpb.FileDescriptorProto {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	var files []*descriptorpb.FileDescriptorProto
	reg.Snapshot().RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		files = append(files, protodesc.ToFileDescriptorProto(fd))
		return true
	})
	return files
}

func marshalSet(t *testing.T, files ...*descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	raw, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: files})
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return raw
}

// emptyRegistryDeps is testDeps with nothing registered, so registration has
// something to add.
func emptyRegistryDeps(t *testing.T) admin.Deps {
	t.Helper()
	deps := testDeps(t)
	deps.Registry = schema.NewRegistry()
	return deps
}

func TestRegisterSchemasAddsFilesToTheServedRegistryIdempotently(t *testing.T) {
	deps := emptyRegistryDeps(t)
	client := schemaClient(t, deps)
	req := &adminv1.RegisterSchemasRequest{DescriptorSet: marshalSet(t, testdataFiles(t)...)}

	first, err := client.RegisterSchemas(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("RegisterSchemas: %v", err)
	}
	wantFiles := []string{"google/protobuf/any.proto", "google/protobuf/timestamp.proto", "shop/v1/order.proto"}
	if !reflect.DeepEqual(first.Msg.RegisteredFiles, wantFiles) {
		t.Errorf("registered_files = %q, want %q (sorted)", first.Msg.RegisteredFiles, wantFiles)
	}
	if first.Msg.ServiceCount != 1 {
		t.Errorf("service_count = %d, want 1", first.Msg.ServiceCount)
	}
	// Registered into the served registry, not a copy of it.
	if _, err := deps.Registry.LookupMethod("shop.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("after RegisterSchemas, deps.Registry cannot resolve GetOrder: %v", err)
	}

	again, err := client.RegisterSchemas(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("second RegisterSchemas: %v", err)
	}
	if len(again.Msg.RegisteredFiles) != 0 || again.Msg.ServiceCount != 1 {
		t.Errorf("re-registering = files %q, %d service(s); want no files and still 1 service",
			again.Msg.RegisteredFiles, again.Msg.ServiceCount)
	}
}

func TestRegisterSchemasRejectsBadSetsAndLeavesTheRegistryUntouched(t *testing.T) {
	files := testdataFiles(t)
	var order *descriptorpb.FileDescriptorProto
	var imports []*descriptorpb.FileDescriptorProto
	for _, file := range files {
		if file.GetName() == "shop/v1/order.proto" {
			order = file
		} else {
			imports = append(imports, file)
		}
	}
	conflicting := proto.Clone(order).(*descriptorpb.FileDescriptorProto)
	conflicting.MessageType = append(conflicting.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("Extra")})

	cases := []struct {
		name string
		set  []byte
		want string
	}{
		{"undecodable bytes", []byte{0x0a, 0x05}, "descriptor_set is not a valid FileDescriptorSet"},
		{"empty set", nil, "descriptor set contains no files"},
		{"missing imports", marshalSet(t, order), "descriptor sets must be self-contained"},
		{"same path, different content", marshalSet(t, append(imports, conflicting)...),
			`file "shop/v1/order.proto" is already registered with different content`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t) // testdata/protos is already registered
			_, err := schemaClient(t, deps).RegisterSchemas(context.Background(),
				connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: tc.set}))
			if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
				t.Fatalf("RegisterSchemas = %v (code %v), want InvalidArgument", err, code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if _, err := deps.Registry.LookupMessage("shop.v1.Extra"); err == nil {
				t.Error("a rejected set changed the served registry")
			}
		})
	}
}

// RegisterSet accepts a descriptor path that is not valid UTF-8, so the
// response and every later ListServices must still serialize (design §5.2).
// Without validUTF8 the registration succeeds and its own response fails.
func TestSchemaResponsesSurviveANonUTF8DescriptorPath(t *testing.T) {
	deps := emptyRegistryDeps(t)
	client := schemaClient(t, deps)
	bad := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("bad\xffsvc.proto"),
		Package:     proto.String("probe.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("ProbeService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("Do"),
				InputType:  proto.String(".probe.v1.Empty"),
				OutputType: proto.String(".probe.v1.Empty"),
			}},
		}},
	}
	registered, err := client.RegisterSchemas(context.Background(),
		connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: marshalSet(t, bad)}))
	if err != nil {
		t.Fatalf("RegisterSchemas: %v", err)
	}
	if want := []string{"bad�svc.proto"}; !reflect.DeepEqual(registered.Msg.RegisteredFiles, want) {
		t.Errorf("registered_files = %q, want %q", registered.Msg.RegisteredFiles, want)
	}
	listed, err := client.ListServices(context.Background(), connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		t.Fatalf("ListServices after registering a non-UTF-8 path: %v", err)
	}
	if len(listed.Msg.Services) != 1 || listed.Msg.Services[0].File != "bad�svc.proto" {
		t.Errorf("services = %v, want probe.v1.ProbeService from bad�svc.proto", listed.Msg.Services)
	}
}

func TestListServicesDescribesEveryServiceSortedByName(t *testing.T) {
	deps := testDeps(t)
	if err := deps.Registry.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		t.Fatalf("AddFile(health): %v", err)
	}
	resp, err := schemaClient(t, deps).ListServices(context.Background(),
		connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	var names []string
	for _, svc := range resp.Msg.Services {
		names = append(names, svc.Name)
	}
	if want := []string{"grpc.health.v1.Health", "shop.v1.OrderService"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service names = %q, want %q", names, want)
	}
	order := resp.Msg.Services[1]
	if order.File != "shop/v1/order.proto" {
		t.Errorf("file = %q, want shop/v1/order.proto", order.File)
	}
	want := []*adminv1.MethodInfo{
		{Name: "GetOrder", InputType: "shop.v1.GetOrderRequest", OutputType: "shop.v1.GetOrderResponse"},
		{Name: "WatchOrder", InputType: "shop.v1.GetOrderRequest", OutputType: "shop.v1.GetOrderResponse",
			ServerStreaming: true},
		{Name: "UploadOrders", InputType: "shop.v1.GetOrderRequest", OutputType: "shop.v1.GetOrderResponse",
			ClientStreaming: true},
		{Name: "Chat", InputType: "shop.v1.ChatMessage", OutputType: "shop.v1.ChatMessage",
			ClientStreaming: true, ServerStreaming: true},
	}
	if len(order.Methods) != len(want) {
		t.Fatalf("methods = %d, want %d", len(order.Methods), len(want))
	}
	for i := range want {
		if !proto.Equal(order.Methods[i], want[i]) {
			t.Errorf("method %d = %v, want %v (declaration order)", i, order.Methods[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestRegisterSchemas|TestSchemaResponses|TestListServices' -count=1 -v`
Expected: FAIL — every test reports `unimplemented: simulacra.admin.v1.SchemaService.RegisterSchemas is not implemented` or `… ListServices is not implemented`, from the embedded Unimplemented handler.

- [ ] **Step 3: Write the implementation**

Replace the whole of `internal/admin/schema.go` with:

```go
package admin

import (
	"context"
	"fmt"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// schemaService implements simulacra.admin.v1.SchemaService.
//
// It deliberately does not embed UnimplementedSchemaServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type schemaService struct {
	deps Deps
}

// RegisterSchemas applies a descriptor set to the served registry,
// all-or-nothing (design §4.2).
func (s *schemaService) RegisterSchemas(
	_ context.Context,
	req *connect.Request[adminv1.RegisterSchemasRequest],
) (*connect.Response[adminv1.RegisterSchemasResponse], error) {
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(req.Msg.GetDescriptorSet(), set); err != nil {
		return nil, connectError(invalidArgument(fmt.Errorf("descriptor_set is not a valid FileDescriptorSet: %w", err)))
	}
	added, err := s.deps.Registry.RegisterSet(set)
	if err != nil {
		return nil, connectError(invalidArgument(err))
	}
	sort.Strings(added)
	files := make([]string, len(added))
	for i, path := range added {
		files[i] = validUTF8(path)
	}
	return connect.NewResponse(&adminv1.RegisterSchemasResponse{
		RegisteredFiles: files,
		ServiceCount:    int32(len(s.deps.Registry.Services())),
	}), nil
}

// ListServices lists every registered service, sorted by fully-qualified name,
// with its methods in declaration order (design §4.2).
func (s *schemaService) ListServices(
	_ context.Context,
	_ *connect.Request[adminv1.ListServicesRequest],
) (*connect.Response[adminv1.ListServicesResponse], error) {
	services := s.deps.Registry.Services()
	sort.Slice(services, func(i, j int) bool { return services[i].FullName() < services[j].FullName() })
	infos := make([]*adminv1.ServiceInfo, len(services))
	for i, svc := range services {
		infos[i] = serviceInfo(svc)
	}
	return connect.NewResponse(&adminv1.ListServicesResponse{Services: infos}), nil
}

func serviceInfo(svc protoreflect.ServiceDescriptor) *adminv1.ServiceInfo {
	info := &adminv1.ServiceInfo{
		Name: validUTF8(string(svc.FullName())),
		File: validUTF8(svc.ParentFile().Path()),
	}
	methods := svc.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		info.Methods = append(info.Methods, &adminv1.MethodInfo{
			Name:            validUTF8(string(m.Name())),
			InputType:       validUTF8(string(m.Input().FullName())),
			OutputType:      validUTF8(string(m.Output().FullName())),
			ClientStreaming: m.IsStreamingClient(),
			ServerStreaming: m.IsStreamingServer(),
		})
	}
	return info
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS, including `TestRequestSizeCapsArePerService`: its 8 MiB `RegisterSchemas` request now answers `InvalidArgument` (an empty set) instead of `Unimplemented`, and neither is `ResourceExhausted`.

- [ ] **Step 5: Commit**

```bash
git add internal/admin/schema.go internal/admin/schema_test.go
git commit -m "feat(admin): SchemaService"
```

---

### Task 8: `StubService`

**Files:**
- Modify: `internal/admin/stub.go` (replace the shell)
- Create: `internal/admin/stub_test.go`
- Modify: `internal/admin/export_test.go`

**Interfaces:**
- Consumes:
  - `connectError` and `invalidArgument` (Task 4); `validUTF8` (Task 5); the `stubService` shell (Task 6).
  - The int32 bounds (Task 3) and `schema.ErrUnknownMethod` (Task 1).
  - From Phase 3: `stub.ParseDocument`, `stub.NewCompiler(...).Compile`, `stub.RenderSequence`, and `(*stub.Store).Add`, `List`, `Remove`, `ReplaceOrigin`, `Select`.
  - The test helper `loadStubsInto(t, deps, yaml)` from `control_test.go`.
- Produces:
  - `(*stubService).CreateStub`, `ListStubs`, `DeleteStub`, `ReplaceAllStubs`, and `ExportStubs`.
  - Unexported helpers `normalizeMethod(string) string`, `compileDocument(*stub.Compiler, document, source string) (*stub.Compiled, error)`, `infoOf(*stub.Compiled) stub.Info`, and `stubEnvelope(stub.Info) *adminv1.Stub`. Task 11 uses `normalizeMethod`.
  - Test-only export `admin.StubEnvelope`.

Design §4.1, §4.3, and §5.2. `compileDocument` labels parse diagnostics with the same source as compile diagnostics — `document:` for `CreateStub`, `documents[i]:` for `ReplaceAllStubs` — so every failure names what failed the same way; the diagnostic text itself is unchanged.

- [ ] **Step 1: Write the failing tests**

Append to `internal/admin/export_test.go`:

```go
// StubEnvelope is a test-only view of the stub envelope conversion.
var StubEnvelope = stubEnvelope
```

Create `internal/admin/stub_test.go`:

```go
package admin_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func stubClient(t *testing.T, deps admin.Deps) adminv1connect.StubServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewStubServiceClient(ts.Client(), ts.URL)
}

// getOrderStub is one API stub document for GetOrder, answering with note.
func getOrderStub(note string, priority int) string {
	return fmt.Sprintf("method: shop.v1.OrderService/GetOrder\npriority: %d\nrespond:\n  message: { note: %s }\n",
		priority, note)
}

// apiStubIDs lists the ids of the API-origin stubs in the served store, in
// List order.
func apiStubIDs(deps admin.Deps) []string {
	api := stub.OriginAPI
	var ids []string
	for _, info := range deps.Store.List(stub.ListFilter{Origin: &api}) {
		ids = append(ids, info.ID)
	}
	return ids
}

func TestCreateStubInstallsAnAPIStubFromYAMLOrJSON(t *testing.T) {
	deps := testDeps(t)
	client := stubClient(t, deps)
	cases := []struct {
		document string
		want     *adminv1.Stub
	}{
		{
			document: "method: shop.v1.OrderService/GetOrder\nrespond:\n  message: { note: from-yaml }\n",
			want: &adminv1.Stub{Id: "api-1", Method: "/shop.v1.OrderService/GetOrder",
				Shape: adminv1.StubShape_STUB_SHAPE_UNARY, Origin: adminv1.StubOrigin_STUB_ORIGIN_API, Source: "api"},
		},
		{
			document: `{"method": "shop.v1.OrderService/WatchOrder", "priority": 3, "times": 2,` +
				` "respond": {"stream": [{"message": {"note": "from-json"}}]}}`,
			want: &adminv1.Stub{Id: "api-2", Method: "/shop.v1.OrderService/WatchOrder",
				Shape: adminv1.StubShape_STUB_SHAPE_SERVER_STREAM, Priority: 3, Times: 2,
				Origin: adminv1.StubOrigin_STUB_ORIGIN_API, Source: "api"},
		},
	}
	for _, tc := range cases {
		resp, err := client.CreateStub(context.Background(),
			connect.NewRequest(&adminv1.CreateStubRequest{Document: tc.document}))
		if err != nil {
			t.Fatalf("CreateStub(%s): %v", tc.document, err)
		}
		_, normalized, err := stub.ParseDocument([]byte(tc.document))
		if err != nil {
			t.Fatalf("ParseDocument: %v", err)
		}
		tc.want.Document = normalized
		if !proto.Equal(resp.Msg.Stub, tc.want) {
			t.Errorf("CreateStub envelope = %v, want %v", resp.Msg.Stub, tc.want)
		}
	}
	if ids := apiStubIDs(deps); !reflect.DeepEqual(ids, []string{"api-1", "api-2"}) {
		t.Errorf("API stubs in the served store = %q, want [api-1 api-2]", ids)
	}
}

func TestCreateStubErrorCodesLeaveTheStoreUntouched(t *testing.T) {
	cases := []struct {
		name     string
		document string
		code     connect.Code
		want     string
	}{
		{"empty", "", connect.CodeInvalidArgument, "document is empty"},
		{"sequence", "- method: shop.v1.OrderService/GetOrder\n", connect.CodeInvalidArgument, "document is a sequence"},
		{"unknown field", "method: shop.v1.OrderService/GetOrder\nbogus: 1\n", connect.CodeInvalidArgument, "bogus"},
		{"malformed method", "method: garbage\n", connect.CodeInvalidArgument, `invalid method name "garbage"`},
		{"unregistered method", "method: shop.v1.OrderService/Nope\n", connect.CodeNotFound,
			`method "Nope" not found on service "shop.v1.OrderService"`},
		{"times beyond int32", "method: shop.v1.OrderService/GetOrder\ntimes: 2147483648\n",
			connect.CodeInvalidArgument, "times must be at most 2147483647"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			_, err := stubClient(t, deps).CreateStub(context.Background(),
				connect.NewRequest(&adminv1.CreateStubRequest{Document: tc.document}))
			if code := connect.CodeOf(err); code != tc.code {
				t.Fatalf("CreateStub = %v (code %v), want %v", err, code, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if n := deps.Store.Len(); n != 0 {
				t.Errorf("store holds %d stub(s) after a rejected CreateStub, want 0", n)
			}
		})
	}
}

func TestListStubsReportsEnvelopesAndFilters(t *testing.T) {
	deps := testDeps(t)
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  priority: 2147483647
  times: 2147483647
  respond:
    message: { note: file-origin }
`)
	client := stubClient(t, deps)
	ctx := context.Background()
	if _, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{
		Document: "method: shop.v1.OrderService/WatchOrder\nrespond:\n  stream:\n  - message: { note: api-origin }\n",
	})); err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	// Spend one use of the file stub so hits is observable.
	if deps.Store.Select("/shop.v1.OrderService/GetOrder", match.Input{}) == nil {
		t.Fatal("precondition: the file stub did not select")
	}

	all, err := client.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	if len(all.Msg.Stubs) != 2 {
		t.Fatalf("ListStubs = %d stubs, want 2", len(all.Msg.Stubs))
	}
	file, api := all.Msg.Stubs[0], all.Msg.Stubs[1]
	if file.Origin != adminv1.StubOrigin_STUB_ORIGIN_FILE || !strings.HasSuffix(file.Source, "stubs.yaml#0") || file.Id != file.Source {
		t.Errorf("file stub envelope = %v, want FILE origin with id = source = <dir>/stubs.yaml#0", file)
	}
	// The int32 boundary values convert exactly (design §3.3).
	if file.Priority != 2147483647 || file.Times != 2147483647 || file.Hits != 1 {
		t.Errorf("file stub priority/times/hits = %d/%d/%d, want 2147483647/2147483647/1",
			file.Priority, file.Times, file.Hits)
	}
	if api.Id != "api-1" || api.Origin != adminv1.StubOrigin_STUB_ORIGIN_API || api.Shape != adminv1.StubShape_STUB_SHAPE_SERVER_STREAM {
		t.Errorf("api stub envelope = %v, want api-1, API origin, server-streaming", api)
	}

	filters := []struct {
		name string
		req  *adminv1.ListStubsRequest
		want []string
	}{
		{"API origin", &adminv1.ListStubsRequest{Origin: adminv1.StubOrigin_STUB_ORIGIN_API}, []string{"api-1"}},
		{"file origin", &adminv1.ListStubsRequest{Origin: adminv1.StubOrigin_STUB_ORIGIN_FILE}, []string{file.Id}},
		{"method without slash", &adminv1.ListStubsRequest{Method: "shop.v1.OrderService/WatchOrder"}, []string{"api-1"}},
		{"method with slash", &adminv1.ListStubsRequest{Method: "/shop.v1.OrderService/GetOrder"}, []string{file.Id}},
	}
	for _, f := range filters {
		resp, err := client.ListStubs(ctx, connect.NewRequest(f.req))
		if err != nil {
			t.Fatalf("%s: ListStubs: %v", f.name, err)
		}
		var ids []string
		for _, s := range resp.Msg.Stubs {
			ids = append(ids, s.Id)
		}
		if !reflect.DeepEqual(ids, f.want) {
			t.Errorf("%s: ids = %q, want %q", f.name, ids, f.want)
		}
	}

	_, err = client.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{Origin: adminv1.StubOrigin(7)}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("ListStubs with origin 7 = %v (code %v), want InvalidArgument", err, code)
	}
}

func TestDeleteStubRemovesAPIStubsOnly(t *testing.T) {
	deps := testDeps(t)
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: file-origin }
`)
	client := stubClient(t, deps)
	ctx := context.Background()
	created, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: getOrderStub("api", 0)}))
	if err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	fileOrigin := stub.OriginFile
	fileID := deps.Store.List(stub.ListFilter{Origin: &fileOrigin})[0].ID

	cases := []struct {
		name string
		id   string
		code connect.Code
		want string
	}{
		{"empty id", "", connect.CodeInvalidArgument, "id is required"},
		{"file-origin stub", fileID, connect.CodeFailedPrecondition, "edit or remove the file"},
		{"unknown id", "api-99", connect.CodeNotFound, `no stub with this id: "api-99"`},
	}
	for _, tc := range cases {
		_, err := client.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: tc.id}))
		if code := connect.CodeOf(err); code != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: DeleteStub = %v (code %v), want %v containing %q", tc.name, err, code, tc.code, tc.want)
		}
	}

	if _, err := client.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: created.Msg.Stub.Id})); err != nil {
		t.Fatalf("DeleteStub(%s): %v", created.Msg.Stub.Id, err)
	}
	if ids := apiStubIDs(deps); len(ids) != 0 || deps.Store.Len() != 1 {
		t.Fatalf("after DeleteStub: API ids %q, store size %d; want none and the file stub kept", ids, deps.Store.Len())
	}
	_, err = client.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: created.Msg.Stub.Id}))
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("deleting %s twice = %v (code %v), want NotFound", created.Msg.Stub.Id, err, code)
	}
}

func TestReplaceAllStubsIsAllOrNothing(t *testing.T) {
	deps := testDeps(t)
	// Priority 10 makes the file stub, not an API stub, the one Select spends.
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  priority: 10
  respond:
    message: { note: file-origin }
`)
	client := stubClient(t, deps)
	ctx := context.Background()
	for _, note := range []string{"first", "second"} {
		if _, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: getOrderStub(note, 0)})); err != nil {
			t.Fatalf("CreateStub: %v", err)
		}
	}
	if selected := deps.Store.Select("/shop.v1.OrderService/GetOrder", match.Input{}); selected == nil || selected.Origin != stub.OriginFile {
		t.Fatalf("precondition: selected %+v, want the file stub", selected)
	}
	before := apiStubIDs(deps)

	_, err := client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{Documents: []string{
		getOrderStub("third", 0),
		getOrderStub("fourth", 0),
		"method: shop.v1.OrderService/GetOrder\nbogus: 1\n",
	}}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "documents[2]: ") {
		t.Fatalf("ReplaceAllStubs with a bad third document = %v (code %v), want InvalidArgument naming documents[2]", err, code)
	}
	if after := apiStubIDs(deps); !reflect.DeepEqual(after, before) {
		t.Fatalf("API stubs after a rejected ReplaceAllStubs = %q, want the previous %q", after, before)
	}

	_, err = client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{Documents: []string{
		getOrderStub("third", 0),
		"method: shop.v1.OrderService/Nope\n",
	}}))
	if code := connect.CodeOf(err); code != connect.CodeNotFound || !strings.Contains(err.Error(), "documents[1]: ") {
		t.Fatalf("ReplaceAllStubs naming an unregistered method = %v (code %v), want NotFound naming documents[1]", err, code)
	}

	replaced, err := client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{Documents: []string{
		getOrderStub("third", 0),
		getOrderStub("fourth", 0),
	}}))
	if err != nil {
		t.Fatalf("ReplaceAllStubs: %v", err)
	}
	var ids []string
	for _, s := range replaced.Msg.Stubs {
		ids = append(ids, s.Id)
	}
	if want := []string{"api-3", "api-4"}; !reflect.DeepEqual(ids, want) || !reflect.DeepEqual(apiStubIDs(deps), want) {
		t.Fatalf("replaced ids = %q, store API ids = %q; want %q in both", ids, apiStubIDs(deps), want)
	}
	fileOrigin := stub.OriginFile
	if files := deps.Store.List(stub.ListFilter{Origin: &fileOrigin}); len(files) != 1 || files[0].Hits != 1 {
		t.Fatalf("file stubs after ReplaceAllStubs = %+v, want the one file stub with its spent use kept", files)
	}

	if _, err := client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{})); err != nil {
		t.Fatalf("ReplaceAllStubs(no documents): %v", err)
	}
	if ids := apiStubIDs(deps); len(ids) != 0 || deps.Store.Len() != 1 {
		t.Fatalf("after ReplaceAllStubs(no documents): API ids %q, store size %d; want none and the file stub kept",
			ids, deps.Store.Len())
	}
}

func TestExportStubsRendersAPIStubsAsOneLoadableFile(t *testing.T) {
	deps := testDeps(t)
	client := stubClient(t, deps)
	ctx := context.Background()

	empty, err := client.ExportStubs(ctx, connect.NewRequest(&adminv1.ExportStubsRequest{}))
	if err != nil {
		t.Fatalf("ExportStubs(no stubs): %v", err)
	}
	if empty.Msg.Document != "[]\n" || empty.Msg.StubCount != 0 {
		t.Errorf("empty export = %q, %d; want %q, 0", empty.Msg.Document, empty.Msg.StubCount, "[]\n")
	}

	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: file-origin }
`)
	for _, doc := range []string{getOrderStub("low", 0), getOrderStub("high", 5)} {
		if _, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: doc})); err != nil {
			t.Fatalf("CreateStub: %v", err)
		}
	}
	listed, err := client.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{Origin: adminv1.StubOrigin_STUB_ORIGIN_API}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	exported, err := client.ExportStubs(ctx, connect.NewRequest(&adminv1.ExportStubsRequest{}))
	if err != nil {
		t.Fatalf("ExportStubs: %v", err)
	}
	if exported.Msg.StubCount != 2 {
		t.Fatalf("stub_count = %d, want 2 (file-origin stubs are not exported)", exported.Msg.StubCount)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exported.yaml"), []byte(exported.Msg.Document), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, errs := stub.LoadDirs(deps.Registry, []string{dir})
	if len(errs) != 0 || len(loaded) != 2 {
		t.Fatalf("loading the export = %d stub(s), errors %v; want 2 and none", len(loaded), errs)
	}
	for i, c := range loaded {
		want := listed.Msg.Stubs[i]
		if c.Document != want.Document || int32(c.Priority) != want.Priority {
			t.Errorf("loaded stub %d = priority %d; want ListStubs order and documents (%v)", i, c.Priority, want)
		}
	}
}

// File stub ids and sources are paths, which filesystems such as ext4 allow to
// hold invalid UTF-8. No development filesystem here can produce one, so the
// conversion is tested directly.
func TestStubEnvelopeSanitizesPathDerivedStrings(t *testing.T) {
	got := admin.StubEnvelope(stub.Info{
		ID:     "stubs/\xff.yaml#0",
		Source: "stubs/\xff.yaml#0",
		Method: "/shop.v1.OrderService/GetOrder",
		Origin: stub.OriginFile,
	})
	if got.Id != "stubs/�.yaml#0" || got.Source != got.Id {
		t.Errorf("id/source = %q/%q, want U+FFFD in place of the invalid byte", got.Id, got.Source)
	}
	if _, err := proto.Marshal(got); err != nil {
		t.Fatalf("the envelope does not marshal: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -count=1`
Expected: FAIL — the test package does not build: `undefined: stubEnvelope` in `export_test.go`.

- [ ] **Step 3: Write the implementation**

Replace the whole of `internal/admin/stub.go` with:

```go
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// stubService implements simulacra.admin.v1.StubService.
//
// It deliberately does not embed UnimplementedStubServiceHandler: an RPC added
// to the contract must fail the build here, not return Unimplemented at
// runtime.
type stubService struct {
	deps Deps
}

// CreateStub compiles one stub document and installs it as an API-origin stub
// (design §4.3).
func (s *stubService) CreateStub(
	_ context.Context,
	req *connect.Request[adminv1.CreateStubRequest],
) (*connect.Response[adminv1.CreateStubResponse], error) {
	compiled, err := compileDocument(stub.NewCompiler(s.deps.Registry), req.Msg.GetDocument(), "document")
	if err != nil {
		return nil, connectError(err)
	}
	s.deps.Store.Add(compiled)
	return connect.NewResponse(&adminv1.CreateStubResponse{Stub: stubEnvelope(infoOf(compiled))}), nil
}

// ListStubs lists stub envelopes in the store's order, optionally filtered by
// method and origin.
func (s *stubService) ListStubs(
	_ context.Context,
	req *connect.Request[adminv1.ListStubsRequest],
) (*connect.Response[adminv1.ListStubsResponse], error) {
	filter := stub.ListFilter{Method: normalizeMethod(req.Msg.GetMethod())}
	switch origin := req.Msg.GetOrigin(); origin {
	case adminv1.StubOrigin_STUB_ORIGIN_UNSPECIFIED:
	case adminv1.StubOrigin_STUB_ORIGIN_FILE:
		file := stub.OriginFile
		filter.Origin = &file
	case adminv1.StubOrigin_STUB_ORIGIN_API:
		api := stub.OriginAPI
		filter.Origin = &api
	default:
		return nil, connectError(invalidArgument(fmt.Errorf("origin %d is not a declared StubOrigin", origin)))
	}
	infos := s.deps.Store.List(filter)
	envelopes := make([]*adminv1.Stub, len(infos))
	for i, info := range infos {
		envelopes[i] = stubEnvelope(info)
	}
	return connect.NewResponse(&adminv1.ListStubsResponse{Stubs: envelopes}), nil
}

// DeleteStub removes an API-origin stub. The store enforces ownership: a
// file-origin stub is FAILED_PRECONDITION, an unknown id NOT_FOUND.
func (s *stubService) DeleteStub(
	_ context.Context,
	req *connect.Request[adminv1.DeleteStubRequest],
) (*connect.Response[adminv1.DeleteStubResponse], error) {
	id := req.Msg.GetId()
	if id == "" {
		return nil, connectError(invalidArgument(errors.New("id is required")))
	}
	if err := s.deps.Store.Remove(id); err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&adminv1.DeleteStubResponse{}), nil
}

// ReplaceAllStubs compiles every document before touching the store, then swaps
// the API-origin stubs in one step. The first failure is returned and the store
// is untouched; file-origin stubs and their budgets are never affected.
func (s *stubService) ReplaceAllStubs(
	_ context.Context,
	req *connect.Request[adminv1.ReplaceAllStubsRequest],
) (*connect.Response[adminv1.ReplaceAllStubsResponse], error) {
	compiler := stub.NewCompiler(s.deps.Registry)
	documents := req.Msg.GetDocuments()
	compiled := make([]*stub.Compiled, len(documents))
	for i, document := range documents {
		c, err := compileDocument(compiler, document, fmt.Sprintf("documents[%d]", i))
		if err != nil {
			return nil, connectError(err)
		}
		compiled[i] = c
	}
	if _, err := s.deps.Store.ReplaceOrigin(stub.OriginAPI, compiled); err != nil {
		return nil, connectError(err)
	}
	envelopes := make([]*adminv1.Stub, len(compiled))
	for i, c := range compiled {
		envelopes[i] = stubEnvelope(infoOf(c))
	}
	return connect.NewResponse(&adminv1.ReplaceAllStubsResponse{Stubs: envelopes}), nil
}

// ExportStubs renders the API-origin stubs, in ListStubs order, as one
// file-grammar sequence. Loading the file back keeps their relative order.
func (s *stubService) ExportStubs(
	_ context.Context,
	_ *connect.Request[adminv1.ExportStubsRequest],
) (*connect.Response[adminv1.ExportStubsResponse], error) {
	api := stub.OriginAPI
	infos := s.deps.Store.List(stub.ListFilter{Origin: &api})
	documents := make([]string, len(infos))
	for i, info := range infos {
		documents[i] = info.Document
	}
	rendered, err := stub.RenderSequence(documents)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&adminv1.ExportStubsResponse{
		Document:  validUTF8(rendered),
		StubCount: int32(len(infos)),
	}), nil
}

// compileDocument parses and compiles one stub document, labeling both parse
// and compile diagnostics with source. Both steps consume the caller's input,
// so both failures are marked; a typed error inside the compile step (an
// unknown method) still wins in connectError.
func compileDocument(compiler *stub.Compiler, document, source string) (*stub.Compiled, error) {
	parsed, normalized, err := stub.ParseDocument([]byte(document))
	if err != nil {
		return nil, invalidArgument(fmt.Errorf("%s: %w", source, err))
	}
	compiled, err := compiler.Compile(parsed, source)
	if err != nil {
		return nil, invalidArgument(err)
	}
	compiled.Document = normalized
	return compiled, nil
}

// normalizeMethod accepts "pkg.Service/Method" with or without the leading
// slash and returns the "/pkg.Service/Method" form the store and journal key
// on. Empty stays empty, meaning no filter.
func normalizeMethod(method string) string {
	if method == "" {
		return ""
	}
	return "/" + strings.TrimPrefix(method, "/")
}

// infoOf is the envelope of a stub that Add or ReplaceOrigin has just
// installed and stamped with its id, origin, and source. Its hits are 0 by
// definition.
func infoOf(c *stub.Compiled) stub.Info {
	return stub.Info{
		ID:       c.ID,
		Method:   c.Method,
		Shape:    c.Shape,
		Priority: c.Priority,
		Times:    c.Times,
		Origin:   c.Origin,
		Source:   c.Source,
		Document: c.Document,
	}
}

// stubEnvelope renders a store envelope. priority and times convert losslessly
// because the compiler bounds them to int32 (design §3.3).
func stubEnvelope(info stub.Info) *adminv1.Stub {
	return &adminv1.Stub{
		Id:       validUTF8(info.ID),
		Method:   validUTF8(info.Method),
		Shape:    stubShape(info.Shape),
		Priority: int32(info.Priority),
		Times:    int32(info.Times),
		Origin:   stubOrigin(info.Origin),
		Source:   validUTF8(info.Source),
		Hits:     int64(info.Hits),
		Document: validUTF8(info.Document),
	}
}

func stubShape(shape match.Shape) adminv1.StubShape {
	switch shape {
	case match.Unary:
		return adminv1.StubShape_STUB_SHAPE_UNARY
	case match.ServerStream:
		return adminv1.StubShape_STUB_SHAPE_SERVER_STREAM
	case match.ClientStream:
		return adminv1.StubShape_STUB_SHAPE_CLIENT_STREAM
	case match.Bidi:
		return adminv1.StubShape_STUB_SHAPE_BIDI_STREAM
	default:
		return adminv1.StubShape_STUB_SHAPE_UNSPECIFIED
	}
}

func stubOrigin(origin stub.Origin) adminv1.StubOrigin {
	if origin == stub.OriginAPI {
		return adminv1.StubOrigin_STUB_ORIGIN_API
	}
	return adminv1.StubOrigin_STUB_ORIGIN_FILE
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS, including `TestRequestSizeCapsArePerService`, whose 8 MiB `ReplaceAllStubs` request is still rejected by the 4 MiB cap before the handler runs.

- [ ] **Step 5: Mutation-check the all-or-nothing guarantee**

Temporarily insert `_, _ = s.deps.Store.ReplaceOrigin(stub.OriginAPI, nil)` as the first statement of `ReplaceAllStubs`, so API stubs are cleared before the documents are compiled. Run `go test ./internal/admin/ -run TestReplaceAllStubsIsAllOrNothing -count=1`.
Expected: FAIL at `API stubs after a rejected ReplaceAllStubs = [], want the previous ["api-1" "api-2"]`. Remove the line and re-run: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/admin/stub.go internal/admin/stub_test.go internal/admin/export_test.go
git commit -m "feat(admin): StubService"
```

---

### Task 9: `JournalService.ListCalls` and `ResetJournal`

**Files:**
- Modify: `internal/admin/journal.go` (replace the shell; the Unimplemented embedding stays until Task 10)
- Create: `internal/admin/journal_test.go`

**Interfaces:**
- Consumes: `connectError` and `invalidArgument` (Task 4); `renderCall`, plus the test helpers `getOrderMessages` and `jsonFields` (Task 5); the `journalService` shell (Task 6); `(*journal.Journal).Filter`, `Reset`, and `Record` (Phase 3).
- Produces:
  - `(*journalService).ListCalls` and `(*journalService).ResetJournal`.
  - Test helpers in `journal_test.go`, used again by Tasks 10 and 11: `journalClient(t, deps)`, `recordGetOrder(t, deps, orderID)`, and `callSeqs([]*adminv1.Call) []uint64`.

Design §4.4. `journal.Filter` already normalizes the method and keeps the newest `Limit` calls, oldest first; the handler reverses them because the contract is newest first.

- [ ] **Step 1: Write the failing tests**

Create `internal/admin/journal_test.go`:

```go
package admin_test

import (
	"context"
	"reflect"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
)

func journalClient(t *testing.T, deps admin.Deps) adminv1connect.JournalServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewJournalServiceClient(ts.Client(), ts.URL)
}

// recordGetOrder records a GetOrder call carrying one request with orderID.
func recordGetOrder(t *testing.T, deps admin.Deps, orderID string) {
	t.Helper()
	req, _ := getOrderMessages(t, deps.Registry, orderID, "")
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder", Requests: []*dynamicpb.Message{req}})
}

func callSeqs(calls []*adminv1.Call) []uint64 {
	seqs := make([]uint64, len(calls))
	for i, call := range calls {
		seqs[i] = call.Seq
	}
	return seqs
}

func TestListCallsIsNewestFirstWithLimitAndMethod(t *testing.T) {
	deps := testDeps(t) // journal capacity 4: all four calls are retained
	recordGetOrder(t, deps, "o-1")
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/WatchOrder"})
	recordGetOrder(t, deps, "o-2")
	recordGetOrder(t, deps, "o-3")
	client := journalClient(t, deps)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *adminv1.ListCallsRequest
		want []uint64
	}{
		{"everything", &adminv1.ListCallsRequest{}, []uint64{4, 3, 2, 1}},
		{"limit", &adminv1.ListCallsRequest{Limit: 2}, []uint64{4, 3}},
		{"method without slash", &adminv1.ListCallsRequest{Method: "shop.v1.OrderService/GetOrder"}, []uint64{4, 3, 1}},
		{"method and limit", &adminv1.ListCallsRequest{Method: "/shop.v1.OrderService/GetOrder", Limit: 2}, []uint64{4, 3}},
	}
	for _, tc := range cases {
		resp, err := client.ListCalls(ctx, connect.NewRequest(tc.req))
		if err != nil {
			t.Fatalf("%s: ListCalls: %v", tc.name, err)
		}
		if got := callSeqs(resp.Msg.Calls); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: seqs = %v, want %v (newest first)", tc.name, got, tc.want)
		}
	}

	newest, err := client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{Limit: 1}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if orderID := jsonFields(t, newest.Msg.Calls[0].Requests[0].Json)["order_id"]; orderID != "o-3" {
		t.Errorf("newest call order_id = %v, want o-3; calls must be rendered, not just listed", orderID)
	}

	_, err = client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{Limit: -1}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("ListCalls with limit -1 = %v (code %v), want InvalidArgument", err, code)
	}
}

// One recorded call holding invalid UTF-8 must not make ListCalls fail for the
// whole journal (design §5.2).
func TestListCallsSurvivesInvalidUTF8InRecordedCalls(t *testing.T) {
	deps := testDeps(t)
	deps.Journal.Record(&journal.Call{
		Method:   "/shop.v1.OrderService/Get\xffOrder",
		Metadata: metadata.MD{"x-probe": {"ok\xffbad"}},
	})
	resp, err := journalClient(t, deps).ListCalls(context.Background(),
		connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls with an invalid UTF-8 call in the journal: %v", err)
	}
	call := resp.Msg.Calls[0]
	if call.Method != "/shop.v1.OrderService/Get�Order" || call.RequestMetadata[0].Values[0] != "ok�bad" {
		t.Errorf("rendered call = %v, want U+FFFD in the method and in the metadata value", call)
	}
}

func TestResetJournalClearsCallsAndKeepsCounting(t *testing.T) {
	deps := testDeps(t)
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
	client := journalClient(t, deps)
	ctx := context.Background()

	if _, err := client.ResetJournal(ctx, connect.NewRequest(&adminv1.ResetJournalRequest{})); err != nil {
		t.Fatalf("ResetJournal: %v", err)
	}
	cleared, err := client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(cleared.Msg.Calls) != 0 {
		t.Fatalf("after ResetJournal ListCalls = %v, want no calls", callSeqs(cleared.Msg.Calls))
	}

	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
	after, err := client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if got := callSeqs(after.Msg.Calls); !reflect.DeepEqual(got, []uint64{3}) {
		t.Errorf("seqs after reset = %v, want [3]: sequence numbers keep counting", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run 'TestListCalls|TestResetJournal' -count=1 -v`
Expected: FAIL — `unimplemented: simulacra.admin.v1.JournalService.ListCalls is not implemented` (and `… ResetJournal is not implemented`).

- [ ] **Step 3: Write the implementation**

Replace the whole of `internal/admin/journal.go` with:

```go
package admin

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/journal"
)

// journalService implements simulacra.admin.v1.JournalService. The embedded
// Unimplemented handler stands in for WatchCalls until it is implemented and is
// removed then.
type journalService struct {
	adminv1connect.UnimplementedJournalServiceHandler
	deps Deps
}

// ListCalls returns recorded calls newest first (design §4.4).
func (j *journalService) ListCalls(
	_ context.Context,
	req *connect.Request[adminv1.ListCallsRequest],
) (*connect.Response[adminv1.ListCallsResponse], error) {
	limit := req.Msg.GetLimit()
	if limit < 0 {
		return nil, connectError(invalidArgument(fmt.Errorf("limit must not be negative (got %d)", limit)))
	}
	// Filter normalizes the method and keeps the newest limit calls, oldest
	// first; the contract is newest first.
	calls := j.deps.Journal.Filter(journal.Filter{Method: req.Msg.GetMethod(), Limit: int(limit)})
	types := j.deps.Registry.Types()
	rendered := make([]*adminv1.Call, len(calls))
	for i, call := range calls {
		rendered[len(calls)-1-i] = renderCall(call, types)
	}
	return connect.NewResponse(&adminv1.ListCallsResponse{Calls: rendered}), nil
}

// ResetJournal clears retained calls; sequence numbers keep counting.
func (j *journalService) ResetJournal(
	_ context.Context,
	_ *connect.Request[adminv1.ResetJournalRequest],
) (*connect.Response[adminv1.ResetJournalResponse], error) {
	j.deps.Journal.Reset()
	return connect.NewResponse(&adminv1.ResetJournalResponse{}), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/admin/journal.go internal/admin/journal_test.go
git commit -m "feat(admin): JournalService.ListCalls and ResetJournal"
```

---

### Task 10: `JournalService.WatchCalls`

**Files:**
- Modify: `internal/admin/journal.go` (drop the Unimplemented embedding; add `WatchCalls` and `streamCalls`)
- Modify: `internal/admin/journal_test.go` (imports `errors`, `strings`, `time`; new helpers and tests)
- Modify: `internal/admin/export_test.go`

**Interfaces:**
- Consumes:
  - `Deps.Stopping` (Task 6); `renderCall` (Task 5); `connectError` (Task 4).
  - The test helpers `journalClient` and `recordGetOrder` (Task 9), and `jsonFields` (Task 5).
  - From Phase 3: `(*journal.Journal).Watch(ctx, method) *journal.Subscription`, with `Calls() <-chan *journal.Call`, `Err() error`, and `Close()`.
- Produces:
  - `(*journalService).WatchCalls`.
  - `func streamCalls(ctx context.Context, stopping <-chan struct{}, calls <-chan *journal.Call, subscriptionErr func() error, render func(*journal.Call) *adminv1.WatchCallsResponse, send func(*adminv1.WatchCallsResponse) error) error`, with the test-only export `admin.StreamCalls`.
  - Test helpers `watchedCalls`, `awaitWatching`, `nextCall`, and `probeStubID`.

Design §6. The send loop is a separate function so its shutdown precedence can be tested with injected channels, without timing. `WatchCalls` itself only subscribes and wires the loop to the subscription and the stream.

- [ ] **Step 1: Write the failing tests**

Append to `internal/admin/export_test.go`:

```go
// StreamCalls is a test-only view of WatchCalls' send loop.
var StreamCalls = streamCalls
```

Add `"errors"`, `"strings"`, and `"time"` to the imports of `internal/admin/journal_test.go`, then append:

```go
// probeResponse renders a call as a response carrying only its seq.
func probeResponse(call *journal.Call) *adminv1.WatchCallsResponse {
	return &adminv1.WatchCallsResponse{Call: &adminv1.Call{Seq: call.Seq}}
}

// bufferedCalls returns a channel already holding one call per seq.
func bufferedCalls(seqs ...uint64) chan *journal.Call {
	calls := make(chan *journal.Call, len(seqs))
	for _, seq := range seqs {
		calls <- &journal.Call{Seq: seq}
	}
	return calls
}

func noSubscriptionErr() error { return nil }

// With calls buffered and Stopping already closed, nothing may be sent. A loop
// that relies on its select alone picks between the two ready cases at random,
// so it passes any single run with probability one half; 100 runs leave luck no
// room.
func TestStreamCallsSendsNothingOnceStoppingIsClosed(t *testing.T) {
	for run := 0; run < 100; run++ {
		stopping := make(chan struct{})
		close(stopping)
		sent := 0
		err := admin.StreamCalls(context.Background(), stopping, bufferedCalls(1, 2, 3), noSubscriptionErr,
			probeResponse, func(*adminv1.WatchCallsResponse) error { sent++; return nil })
		if sent != 0 {
			t.Fatalf("run %d: sent %d call(s) after Stopping closed, want 0", run, sent)
		}
		if code := connect.CodeOf(err); code != connect.CodeUnavailable {
			t.Fatalf("run %d: err = %v (code %v), want Unavailable", run, err, code)
		}
	}
}

// Stopping closes while a received call is being rendered: that call must not
// be sent. This fails a shutdown check placed only at the top of the loop, or
// before rendering.
func TestStreamCallsChecksStoppingAfterRendering(t *testing.T) {
	stopping := make(chan struct{})
	sent := 0
	err := admin.StreamCalls(context.Background(), stopping, bufferedCalls(1), noSubscriptionErr,
		func(call *journal.Call) *adminv1.WatchCallsResponse {
			close(stopping)
			return probeResponse(call)
		},
		func(*adminv1.WatchCallsResponse) error { sent++; return nil })
	if sent != 0 {
		t.Fatalf("sent %d call(s) whose rendering overlapped Stopping closing, want 0", sent)
	}
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("err = %v (code %v), want Unavailable", err, code)
	}
}

// An evicted subscription's channel still holds the calls buffered before the
// eviction: they are delivered, in order, before the eviction is reported.
func TestStreamCallsDeliversBufferedCallsBeforeReportingEviction(t *testing.T) {
	calls := bufferedCalls(1, 2, 3)
	close(calls)
	var sent []uint64
	err := admin.StreamCalls(context.Background(), make(chan struct{}), calls,
		func() error { return journal.ErrSlowConsumer }, probeResponse,
		func(resp *adminv1.WatchCallsResponse) error { sent = append(sent, resp.Call.Seq); return nil })
	if !reflect.DeepEqual(sent, []uint64{1, 2, 3}) {
		t.Errorf("sent %v, want [1 2 3] before the eviction is reported", sent)
	}
	if !errors.Is(err, journal.ErrSlowConsumer) {
		t.Errorf("err = %v, want journal.ErrSlowConsumer", err)
	}
}

func TestStreamCallsEndsWithTheContextOrTheSendError(t *testing.T) {
	closed := func() chan *journal.Call {
		calls := make(chan *journal.Call)
		close(calls)
		return calls
	}
	neverSend := func(*adminv1.WatchCallsResponse) error {
		t.Error("send called with no call to send")
		return nil
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := admin.StreamCalls(canceled, make(chan struct{}), closed(), noSubscriptionErr, probeResponse, neverSend); !errors.Is(err, context.Canceled) {
		t.Errorf("subscription ended by a canceled context: err = %v, want context.Canceled", err)
	}
	if err := admin.StreamCalls(context.Background(), make(chan struct{}), closed(), noSubscriptionErr, probeResponse, neverSend); err != nil {
		t.Errorf("subscription closed under a live context: err = %v, want nil", err)
	}
	gone := errors.New("connection gone")
	err := admin.StreamCalls(context.Background(), make(chan struct{}), bufferedCalls(1), noSubscriptionErr,
		probeResponse, func(*adminv1.WatchCallsResponse) error { return gone })
	if !errors.Is(err, gone) {
		t.Errorf("send failure: err = %v, want the send error", err)
	}
}

// watchedCalls receives a WatchCalls stream on its own goroutine, so a test can
// record calls while waiting for them. The channel is unbuffered: a test that
// stops reading it stops the client from draining the connection. It closes
// when the stream ends, after which stream.Err is safe to read.
func watchedCalls(stream *connect.ServerStreamForClient[adminv1.WatchCallsResponse]) <-chan *adminv1.Call {
	calls := make(chan *adminv1.Call)
	go func() {
		defer close(calls)
		for stream.Receive() {
			calls <- stream.Msg().GetCall()
		}
	}()
	return calls
}

// probeStubID marks the calls awaitWatching records, so nextCall can skip them.
const probeStubID = "probe"

// awaitWatching records probe calls on method until the stream delivers one. A
// client cannot observe that its subscription is registered; the first
// delivered probe is the first moment a later Record is certain to reach it.
func awaitWatching(t *testing.T, j *journal.Journal, method string, calls <-chan *adminv1.Call) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		j.Record(&journal.Call{Method: method, StubID: probeStubID})
		select {
		case _, ok := <-calls:
			if !ok {
				t.Fatal("the stream ended before its subscription was observed")
			}
			return
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("the watch never delivered a probe call")
		}
	}
}

// nextCall returns the next call from the stream that is not a probe.
func nextCall(t *testing.T, calls <-chan *adminv1.Call) *adminv1.Call {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case call, ok := <-calls:
			if !ok {
				t.Fatal("the stream ended while a call was expected")
			}
			if call.MatchedStubId != probeStubID {
				return call
			}
		case <-timeout:
			t.Fatal("no call arrived")
		}
	}
}

func TestWatchCallsStreamsRenderedCallsForItsMethod(t *testing.T) {
	deps := testDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := journalClient(t, deps).WatchCalls(ctx,
		connect.NewRequest(&adminv1.WatchCallsRequest{Method: "shop.v1.OrderService/GetOrder"}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	calls := watchedCalls(stream)
	awaitWatching(t, deps.Journal, "/shop.v1.OrderService/GetOrder", calls)

	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/WatchOrder"})
	recordGetOrder(t, deps, "o-7")

	call := nextCall(t, calls)
	if call.Method != "/shop.v1.OrderService/GetOrder" {
		t.Fatalf("delivered a %q call, want only GetOrder calls", call.Method)
	}
	if orderID := jsonFields(t, call.Requests[0].Json)["order_id"]; orderID != "o-7" {
		t.Errorf("delivered call json = %s, want order_id o-7", call.Requests[0].Json)
	}
}

func TestWatchCallsEndsWithUnavailableWhenStoppingCloses(t *testing.T) {
	deps := testDeps(t)
	stopping := make(chan struct{})
	deps.Stopping = stopping
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := journalClient(t, deps).WatchCalls(ctx, connect.NewRequest(&adminv1.WatchCallsRequest{}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	calls := watchedCalls(stream)
	awaitWatching(t, deps.Journal, "/shop.v1.OrderService/GetOrder", calls)

	close(stopping)
	for range calls {
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeUnavailable {
		t.Fatalf("stream ended with %v (code %v), want Unavailable", stream.Err(), code)
	}
	if !strings.Contains(stream.Err().Error(), "server shutting down") {
		t.Errorf("stream error = %q, want it to say the server is shutting down", stream.Err())
	}
}

// A client that stops reading is evicted by the journal: the stream still
// delivers what was buffered, then ends with RESOURCE_EXHAUSTED. Each call
// carries 64 KiB of metadata, so the connection's buffers fill after a handful
// of calls; once the handler blocks in Send, its 64-call subscription buffer
// overflows. If a platform ever buffered all 64 MiB, the stream would never be
// evicted and the test would fail on its context deadline rather than pass.
func TestWatchCallsEndsWithResourceExhaustedWhenTheClientFallsBehind(t *testing.T) {
	deps := testDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := journalClient(t, deps).WatchCalls(ctx, connect.NewRequest(&adminv1.WatchCallsRequest{}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	calls := watchedCalls(stream)
	awaitWatching(t, deps.Journal, "/shop.v1.OrderService/GetOrder", calls)

	// Nothing reads calls while recording, so the client stops draining.
	bulk := metadata.MD{"x-bulk": {strings.Repeat("x", 64<<10)}}
	const recorded = 1000
	for i := 0; i < recorded; i++ {
		deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder", Metadata: bulk})
	}

	delivered := 0
	for range calls {
		delivered++
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeResourceExhausted {
		t.Fatalf("stream ended with %v (code %v) after %d of %d calls, want ResourceExhausted",
			stream.Err(), code, delivered, recorded)
	}
	if delivered >= recorded {
		t.Errorf("delivered %d of %d calls, want some dropped by the eviction", delivered, recorded)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -count=1`
Expected: FAIL — the test package does not build: `undefined: streamCalls` in `export_test.go`.

- [ ] **Step 3: Write the implementation**

In `internal/admin/journal.go`:

1. In the imports, add `"errors"` and remove `"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"`.
2. Replace the `journalService` declaration and its comment with:

   ```go
   // journalService implements simulacra.admin.v1.JournalService.
   //
   // It deliberately does not embed UnimplementedJournalServiceHandler: an RPC
   // added to the contract must fail the build here, not return Unimplemented at
   // runtime.
   type journalService struct {
   	deps Deps
   }
   ```

3. Append:

   ```go
   // WatchCalls streams calls as they are recorded, until the client leaves, the
   // journal evicts it for falling behind, or teardown begins (design §6).
   func (j *journalService) WatchCalls(
   	ctx context.Context,
   	req *connect.Request[adminv1.WatchCallsRequest],
   	stream *connect.ServerStream[adminv1.WatchCallsResponse],
   ) error {
   	sub := j.deps.Journal.Watch(ctx, req.Msg.GetMethod())
   	defer sub.Close()
   	// connect's server-streaming client blocks establishing the call until the
   	// server writes something: response headers are sent with the first Send,
   	// not when the handler starts. Without this, a caller cannot learn the
   	// subscription exists until a matching call arrives to send — exactly the
   	// thing a caller needs the open stream to observe. Send(nil) flushes
   	// headers immediately and is not delivered to the client as a message.
   	if err := stream.Send(nil); err != nil {
   		return connectError(err)
   	}
   	types := j.deps.Registry.Types()
   	render := func(call *journal.Call) *adminv1.WatchCallsResponse {
   		return &adminv1.WatchCallsResponse{Call: renderCall(call, types)}
   	}
   	return connectError(streamCalls(ctx, j.deps.Stopping, sub.Calls(), sub.Err, render, stream.Send))
   }

   // streamCalls is WatchCalls' send loop, separated from the RPC so its shutdown
   // precedence can be tested without timing.
   //
   // Teardown takes precedence over buffered calls. Selecting on stopping ends an
   // idle stream, but when a call is buffered and stopping has closed, both cases
   // are ready and Go picks one at random. So stopping is checked again, without
   // blocking, after rendering and immediately before every send: once that check
   // sees stopping closed, no further send starts. A send that passed its check
   // just before teardown began can still run, and against a client that stopped
   // reading it blocks until the server's drain bound force-closes the connection
   // — connect offers no way to interrupt a send in progress.
   //
   // When the subscription ends, its own error wins: journal.ErrSlowConsumer
   // after an eviction, reported only once the calls buffered before it have been
   // sent. Otherwise the result is the context's error, or nil for a subscription
   // closed while the client was still there.
   func streamCalls(
   	ctx context.Context,
   	stopping <-chan struct{},
   	calls <-chan *journal.Call,
   	subscriptionErr func() error,
   	render func(*journal.Call) *adminv1.WatchCallsResponse,
   	send func(*adminv1.WatchCallsResponse) error,
   ) error {
   	for {
   		select {
   		case <-stopping:
   			return errShuttingDown()
   		case call, ok := <-calls:
   			if !ok {
   				if err := subscriptionErr(); err != nil {
   					return err
   				}
   				return ctx.Err()
   			}
   			response := render(call)
   			select {
   			case <-stopping:
   				return errShuttingDown()
   			default:
   			}
   			if err := send(response); err != nil {
   				return err
   			}
   		}
   	}
   }

   // errShuttingDown is how every open watch ends once teardown begins.
   func errShuttingDown() error {
   	return connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))
   }
   ```

The `stream.Send(nil)` call right after `sub := j.deps.Journal.Watch(...)` is required, not optional: connect's server-streaming client does not consider the call established, and so does not return from its own call, until the handler writes something, so without it every RPC test of `WatchCalls` deadlocks until its context deadline instead of observing the open subscription.

`ListCalls` and `ResetJournal` stay exactly as Task 9 wrote them.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -race -count=1`
Expected: PASS.

- [ ] **Step 5: Mutation-check both shutdown checks**

1. Delete the inner `select { case <-stopping: … default: }` block from `streamCalls`. Run `go test ./internal/admin/ -run TestStreamCalls -count=1`.
   Expected: FAIL in `TestStreamCallsSendsNothingOnceStoppingIsClosed` (`sent N call(s) after Stopping closed`) and in `TestStreamCallsChecksStoppingAfterRendering`. Restore the block.
2. Delete the outer `case <-stopping:` arm and its `return`. Run `go test ./internal/admin/ -run TestWatchCallsEndsWithUnavailableWhenStoppingCloses -count=1`.
   Expected: FAIL after about 10s — the idle stream never ends, so it finishes with `deadline_exceeded` instead of `Unavailable`. Restore the arm and re-run both commands: PASS.

- [ ] **Step 6: Run the whole suite under the race detector**

Run: `go build ./... && go vet ./... && go test ./... -race -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/admin/journal.go internal/admin/journal_test.go internal/admin/export_test.go
git commit -m "feat(admin): JournalService.WatchCalls with teardown precedence"
```

---

### Task 11: `VerifyService.VerifyCalls`

**Files:**
- Modify: `internal/admin/verify.go` (replace the shell)
- Create: `internal/admin/verify_test.go`

**Interfaces:**
- Consumes:
  - `connectError` and `invalidArgument` (Task 4); `renderCall`, `validUTF8`, and the test helper `jsonFields` (Task 5); the `verifyService` shell (Task 6); `normalizeMethod` (Task 8); the test helper `recordGetOrder` (Task 9).
  - `schema.ErrUnknownMethod` (Task 1) and the call snapshots on `journal.Report` (Task 2).
  - From Phase 3: `stub.ParseMatchDocument`, `stub.NewCompiler(...).CompileMatch`, `journal.Verify`, and `journal.Times`.
- Produces: `(*verifyService).VerifyCalls`, plus unexported helpers `journalTimes(*adminv1.Times) journal.Times` and `explainVerdict(method string, report journal.Report) string`.

Design §4.5. Validation runs in order: method, then times, then the matcher document, then `CompileMatch`. `CompileMatch` resolves the method even when the matcher is empty, which is the guard against a mistyped method passing a `never` assertion.

- [ ] **Step 1: Write the failing tests**

Create `internal/admin/verify_test.go`:

```go
package admin_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
)

func verifyClient(t *testing.T, deps admin.Deps) adminv1connect.VerifyServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewVerifyServiceClient(ts.Client(), ts.URL)
}

// verifyFixture records GetOrder calls for o-1, o-1, and o-2 (seqs 1–3) and a
// WatchOrder call (seq 4) that no GetOrder verification may count. testDeps'
// journal holds exactly these four.
func verifyFixture(t *testing.T) admin.Deps {
	t.Helper()
	deps := testDeps(t)
	for _, orderID := range []string{"o-1", "o-1", "o-2"} {
		recordGetOrder(t, deps, orderID)
	}
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/WatchOrder"})
	return deps
}

const orderOneMatcher = "message:\n  order_id: { eq: o-1 }\n"

func TestVerifyCallsPassesAndCountsOnlyItsMethod(t *testing.T) {
	client := verifyClient(t, verifyFixture(t))
	cases := []struct {
		name    string
		req     *adminv1.VerifyCallsRequest
		matched int32
	}{
		{"matcher, method without slash", &adminv1.VerifyCallsRequest{Method: "shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{Exactly: proto.Int32(2)}}, 2},
		{"empty matcher counts every call to the method", &adminv1.VerifyCallsRequest{
			Method: "/shop.v1.OrderService/GetOrder", Times: &adminv1.Times{Exactly: proto.Int32(3)}}, 3},
		{"never", &adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: "message:\n  order_id: { eq: o-9 }\n", Times: &adminv1.Times{Never: true}}, 0},
		{"range", &adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{AtLeast: proto.Int32(1), AtMost: proto.Int32(2)}}, 2},
	}
	for _, tc := range cases {
		resp, err := client.VerifyCalls(context.Background(), connect.NewRequest(tc.req))
		if err != nil {
			t.Fatalf("%s: VerifyCalls: %v", tc.name, err)
		}
		msg := resp.Msg
		if !msg.Passed || msg.Matched != tc.matched || len(msg.Actual) != 0 {
			t.Errorf("%s: passed/matched/actual = %v/%d/%d, want true/%d/0", tc.name, msg.Passed, msg.Matched, len(msg.Actual), tc.matched)
		}
		if !strings.HasPrefix(msg.Explanation, "passed: /shop.v1.OrderService/GetOrder") {
			t.Errorf("%s: explanation = %q, want it to start with the verdict and the normalized method", tc.name, msg.Explanation)
		}
	}
}

func TestVerifyCallsExplainsEachMissWhenTooFewMatch(t *testing.T) {
	resp, err := verifyClient(t, verifyFixture(t)).VerifyCalls(context.Background(),
		connect.NewRequest(&adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{Exactly: proto.Int32(3)}}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	msg := resp.Msg
	if msg.Passed || msg.Matched != 2 || len(msg.Actual) != 1 {
		t.Fatalf("passed/matched/actual = %v/%d/%d, want false/2/1", msg.Passed, msg.Matched, len(msg.Actual))
	}
	for _, want := range []string{"failed:", "matched 2 of 3", "exactly 3"} {
		if !strings.Contains(msg.Explanation, want) {
			t.Errorf("explanation = %q, want it to contain %q", msg.Explanation, want)
		}
	}
	miss := msg.Actual[0]
	if miss.Call.Seq != 3 || jsonFields(t, miss.Call.Requests[0].Json)["order_id"] != "o-2" {
		t.Errorf("actual call = %v, want seq 3 carrying order_id o-2", miss.Call)
	}
	if want := `message order_id: expected to equal "o-1"; actual "o-2"`; miss.NearestMiss != want {
		t.Errorf("nearest_miss = %q, want %q", miss.NearestMiss, want)
	}
}

func TestVerifyCallsListsTheMatchesWhenTooManyMatch(t *testing.T) {
	resp, err := verifyClient(t, verifyFixture(t)).VerifyCalls(context.Background(),
		connect.NewRequest(&adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{AtMost: proto.Int32(1)}}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	msg := resp.Msg
	if msg.Passed || msg.Matched != 2 {
		t.Fatalf("passed/matched = %v/%d, want false/2", msg.Passed, msg.Matched)
	}
	var seqs []uint64
	for _, actual := range msg.Actual {
		seqs = append(seqs, actual.Call.Seq)
		if actual.NearestMiss != "" {
			t.Errorf("matching call %d has nearest_miss %q, want empty: it matched", actual.Call.Seq, actual.NearestMiss)
		}
	}
	if !reflect.DeepEqual(seqs, []uint64{1, 2}) {
		t.Errorf("actual seqs = %v, want [1 2], the calls that matched", seqs)
	}
}

func TestVerifyCallsRejectsBadRequests(t *testing.T) {
	const getOrder = "/shop.v1.OrderService/GetOrder"
	never := &adminv1.Times{Never: true}
	cases := []struct {
		name string
		req  *adminv1.VerifyCallsRequest
		code connect.Code
		want string
	}{
		{"no method", &adminv1.VerifyCallsRequest{Times: never}, connect.CodeInvalidArgument, "method is required"},
		{"no times", &adminv1.VerifyCallsRequest{Method: getOrder}, connect.CodeInvalidArgument, "one times assertion is required"},
		{"contradictory times", &adminv1.VerifyCallsRequest{Method: getOrder,
			Times: &adminv1.Times{Never: true, Exactly: proto.Int32(0)}}, connect.CodeInvalidArgument, "never cannot be combined"},
		{"unknown matcher field", &adminv1.VerifyCallsRequest{Method: getOrder, MatcherDocument: "bogus: 1\n", Times: never},
			connect.CodeInvalidArgument, "bogus"},
		{"bad field path", &adminv1.VerifyCallsRequest{Method: getOrder,
			MatcherDocument: "message:\n  no_such: { eq: x }\n", Times: never}, connect.CodeInvalidArgument, "no_such"},
		{"malformed method", &adminv1.VerifyCallsRequest{Method: "garbage", Times: never},
			connect.CodeInvalidArgument, "invalid method name"},
		// The typo guard: without it, never would pass against a method nothing calls.
		{"mistyped method with never", &adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrdr", Times: never},
			connect.CodeNotFound, `method "GetOrdr" not found`},
		{"unregistered service", &adminv1.VerifyCallsRequest{Method: "/no.such.Service/Get", Times: never},
			connect.CodeNotFound, `service "no.such.Service" is not registered`},
	}
	client := verifyClient(t, verifyFixture(t))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.VerifyCalls(context.Background(), connect.NewRequest(tc.req))
			if code := connect.CodeOf(err); code != tc.code {
				t.Fatalf("VerifyCalls = %v (code %v), want %v", err, code, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/admin/ -run TestVerifyCalls -count=1 -v`
Expected: FAIL — `unimplemented: simulacra.admin.v1.VerifyService.VerifyCalls is not implemented`.

- [ ] **Step 3: Write the implementation**

Replace the whole of `internal/admin/verify.go` with:

```go
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// verifyService implements simulacra.admin.v1.VerifyService.
//
// It deliberately does not embed UnimplementedVerifyServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type verifyService struct {
	deps Deps
}

// VerifyCalls asserts how many recorded calls to a method matched a matcher
// (design §4.5).
func (v *verifyService) VerifyCalls(
	_ context.Context,
	req *connect.Request[adminv1.VerifyCallsRequest],
) (*connect.Response[adminv1.VerifyCallsResponse], error) {
	method := normalizeMethod(req.Msg.GetMethod())
	if method == "" {
		return nil, connectError(invalidArgument(errors.New("method is required")))
	}
	times := journalTimes(req.Msg.GetTimes())
	if err := times.Validate(); err != nil {
		return nil, connectError(invalidArgument(fmt.Errorf("times: %w", err)))
	}
	block, err := stub.ParseMatchDocument([]byte(req.Msg.GetMatcherDocument()))
	if err != nil {
		return nil, connectError(invalidArgument(err))
	}
	// CompileMatch resolves the method even for a nil block: a mistyped method
	// asserted with never must be NOT_FOUND, not a silent pass.
	matcher, err := stub.NewCompiler(v.deps.Registry).CompileMatch(method, block)
	if err != nil {
		return nil, connectError(invalidArgument(err))
	}
	report, err := journal.Verify(v.deps.Journal, method, matcher, times)
	if err != nil {
		return nil, connectError(err)
	}

	types := v.deps.Registry.Types()
	resp := &adminv1.VerifyCallsResponse{
		Passed:      report.Pass,
		Matched:     int32(report.Matched),
		Explanation: validUTF8(explainVerdict(method, report)),
	}
	// Too few matches: the calls that missed, each with its reasons. Too many:
	// the calls that matched, with nothing to explain.
	for _, miss := range report.Misses {
		resp.Actual = append(resp.Actual, &adminv1.CallExplanation{
			Call:        renderCall(miss.Call, types),
			NearestMiss: validUTF8(strings.Join(miss.Reasons, "; ")),
		})
	}
	for _, call := range report.UnexpectedMatches {
		resp.Actual = append(resp.Actual, &adminv1.CallExplanation{Call: renderCall(call, types)})
	}
	return connect.NewResponse(resp), nil
}

// journalTimes converts the wire assertion. An absent Times converts to the
// zero value, which Validate rejects: there is no implied default.
func journalTimes(t *adminv1.Times) journal.Times {
	var times journal.Times
	if t == nil {
		return times
	}
	times.Never = t.GetNever()
	if t.Exactly != nil {
		n := int(*t.Exactly)
		times.Exactly = &n
	}
	if t.AtLeast != nil {
		n := int(*t.AtLeast)
		times.AtLeast = &n
	}
	if t.AtMost != nil {
		n := int(*t.AtMost)
		times.AtMost = &n
	}
	return times
}

// explainVerdict is VerifyCalls' one-line verdict. Its wording is not a
// contract.
func explainVerdict(method string, report journal.Report) string {
	verdict := "failed"
	if report.Pass {
		verdict = "passed"
	}
	return fmt.Sprintf("%s: %s matched %d of %d call(s) considered; want %s",
		verdict, method, report.Matched, report.Considered, report.Want)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/admin/ -count=1`
Expected: PASS.

- [ ] **Step 5: Mutation-check the typo guard**

Temporarily replace the `CompileMatch` call and its error check with:

```go
	var matcher *match.Compiled
	if block != nil {
		matcher, err = stub.NewCompiler(v.deps.Registry).CompileMatch(method, block)
		if err != nil {
			return nil, connectError(invalidArgument(err))
		}
	}
```

adding the import `"github.com/yinghanhung/simulacra/internal/match"`. Run `go test ./internal/admin/ -run TestVerifyCallsRejectsBadRequests -count=1`.
Expected: FAIL in `mistyped method with never` and `unregistered service` — both now pass verification instead of returning NotFound. Restore the original code and import and re-run: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/admin/verify.go internal/admin/verify_test.go
git commit -m "feat(admin): VerifyService.VerifyCalls"
```

---

### Task 12: End to end through `server`

**Files:**
- Create: `server/admin_data_test.go`

**Interfaces:**
- Consumes:
  - Every service from Tasks 7–11, and `Deps.Stopping` wired in Task 6.
  - The existing `server` test helpers `startWithAdmin`, `adminGRPCConn`, and `waitFor`, plus the `Server` fields `reg` and `journal`.
- Produces: nothing other tasks use.

Design §9's integration tests. There is no production change: the behavior already exists, and this task proves it through a real `server.Start`, over both h2c and HTTP/1.1. Because the tests pass on arrival, each one's discriminating power is established by a mutation check instead of a red phase.

- [ ] **Step 1: Write the helpers and the boot-contract test**

Create `server/admin_data_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// testdataDescriptorSet serializes testdata/protos as a self-contained
// FileDescriptorSet: the bytes an SDK sends to RegisterSchemas.
func testdataDescriptorSet(t *testing.T) []byte {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	reg.Snapshot().RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
		return true
	})
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return raw
}

// adminHTTPClient returns an HTTP/1.1 client for the admin plane — the Connect
// path the CLI and the Go SDK use — and the plane's base URL.
func adminHTTPClient(t *testing.T, srv *Server) (*http.Client, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	return client, "http://" + srv.AdminAddr().String()
}

// jsonField decodes one top-level field of a DecodedMessage's json. protojson
// output is not byte-stable, so tests never compare it as a string.
func jsonField(t *testing.T, text, field string) any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatalf("decoding json %q: %v", text, err)
	}
	return fields[field]
}

// metadataValue returns the first text value of key in a rendered call.
func metadataValue(call *adminv1.Call, key string) string {
	for _, entry := range call.GetRequestMetadata() {
		if entry.GetKey() == key && len(entry.GetValues()) > 0 {
			return entry.GetValues()[0]
		}
	}
	return ""
}

// rawDataPlaneCall sends one gRPC request to the data plane as hand-written
// HTTP/2 frames — HEADERS carrying path and extra, then an empty message — and
// waits for the response to end. It exists to put bytes on the wire that a
// well-behaved client library refuses to send, such as invalid UTF-8 in :path
// or in a metadata value.
func rawDataPlaneCall(t *testing.T, addr, path string, extra ...hpack.HeaderField) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial data plane: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("write preface: %v", err)
	}
	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	fields := append([]hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: path},
		{Name: ":authority", Value: "simulacra-test"},
		{Name: "content-type", Value: "application/grpc"},
		{Name: "te", Value: "trailers"},
	}, extra...)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatalf("encode %s: %v", field.Name, err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true}); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	// An empty, uncompressed gRPC message: a zero flag byte and a zero length.
	if err := framer.WriteData(1, true, make([]byte, 5)); err != nil {
		t.Fatalf("write data: %v", err)
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("the response never ended: %v", err)
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					t.Fatalf("write settings ack: %v", err)
				}
			}
		case *http2.HeadersFrame:
			if f.StreamEnded() {
				return
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("the data plane reset the stream: %v", f.ErrCode)
		case *http2.GoAwayFrame:
			t.Fatalf("the data plane sent GOAWAY: %v", f.ErrCode)
		}
	}
}

// The container/SDK boot contract, in process (M3 design §12), driven entirely
// through the admin API by a native gRPC client over h2c: a server started with
// no schema takes schemas, then a stub, serves a matching call, and verifies
// it.
func TestBootContractThroughTheAdminAPI(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	adminConn := adminGRPCConn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	registered := &adminv1.RegisterSchemasResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.SchemaServiceRegisterSchemasProcedure,
		&adminv1.RegisterSchemasRequest{DescriptorSet: testdataDescriptorSet(t)}, registered); err != nil {
		t.Fatalf("RegisterSchemas: %v", err)
	}
	// The data plane registered grpc.health.v1.Health at startup, so testdata
	// brings the count to two.
	if len(registered.RegisteredFiles) != 3 || registered.ServiceCount != 2 {
		t.Fatalf("RegisterSchemas = files %q, %d service(s); want 3 files and 2 services",
			registered.RegisteredFiles, registered.ServiceCount)
	}

	created := &adminv1.CreateStubResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.StubServiceCreateStubProcedure, &adminv1.CreateStubRequest{
		Document: "method: shop.v1.OrderService/GetOrder\nmatch:\n  message:\n    order_id: { eq: o-1 }\n" +
			"respond:\n  message: { note: from-api }\n",
	}, created); err != nil {
		t.Fatalf("CreateStub: %v", err)
	}

	dataConn, err := grpc.NewClient(srv.DataAddr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = dataConn.Close() })
	method, err := srv.reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod after RegisterSchemas: %v", err)
	}
	req := dynamicpb.NewMessage(method.Input())
	if err := (protojson.UnmarshalOptions{Resolver: srv.reg.Types()}).Unmarshal([]byte(`{"order_id":"o-1"}`), req); err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp := dynamicpb.NewMessage(method.Output())
	if err := dataConn.Invoke(ctx, "/shop.v1.OrderService/GetOrder", req, resp); err != nil {
		t.Fatalf("data-plane GetOrder: %v", err)
	}
	if note := resp.Get(method.Output().Fields().ByName("note")).String(); note != "from-api" {
		t.Fatalf("response note = %q, want from-api", note)
	}

	listed := &adminv1.ListCallsResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.JournalServiceListCallsProcedure, &adminv1.ListCallsRequest{}, listed); err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(listed.Calls) != 1 {
		t.Fatalf("ListCalls = %d calls, want 1", len(listed.Calls))
	}
	call := listed.Calls[0]
	if call.MatchedStubId != created.Stub.Id || call.Status.GetCode() != 0 || len(call.Requests) != 1 || len(call.Responses) != 1 {
		t.Fatalf("recorded call = %v, want matched_stub_id %s, code 0, one request and one response", call, created.Stub.Id)
	}
	if jsonField(t, call.Requests[0].Json, "order_id") != "o-1" || jsonField(t, call.Responses[0].Json, "note") != "from-api" {
		t.Errorf("recorded messages = %s / %s, want order_id o-1 and note from-api", call.Requests[0].Json, call.Responses[0].Json)
	}

	passed := &adminv1.VerifyCallsResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.VerifyServiceVerifyCallsProcedure, &adminv1.VerifyCallsRequest{
		Method:          "shop.v1.OrderService/GetOrder",
		MatcherDocument: "message:\n  order_id: { eq: o-1 }\n",
		Times:           &adminv1.Times{Exactly: proto.Int32(1)},
	}, passed); err != nil {
		t.Fatalf("VerifyCalls (o-1): %v", err)
	}
	if !passed.Passed {
		t.Errorf("VerifyCalls o-1 exactly 1 = %q, want a pass", passed.Explanation)
	}
	failed := &adminv1.VerifyCallsResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.VerifyServiceVerifyCallsProcedure, &adminv1.VerifyCallsRequest{
		Method:          "shop.v1.OrderService/GetOrder",
		MatcherDocument: "message:\n  order_id: { eq: o-2 }\n",
		Times:           &adminv1.Times{Exactly: proto.Int32(1)},
	}, failed); err != nil {
		t.Fatalf("VerifyCalls (o-2): %v", err)
	}
	if failed.Passed || len(failed.Actual) != 1 || !strings.Contains(failed.Actual[0].NearestMiss, `expected to equal "o-2"`) {
		t.Errorf("VerifyCalls o-2 exactly 1 = %v, want a failure whose nearest miss names o-2", failed)
	}
}
```

- [ ] **Step 2: Add the invalid-UTF-8 and teardown tests**

Append to `server/admin_data_test.go`:

```go
// recordUntilWatched records probe calls until arrived reports that the watch
// delivered one. A client cannot observe that its subscription is registered;
// the first delivered probe is the first moment later calls are certain to
// reach it. Probe calls carry StubID "probe".
func recordUntilWatched(t *testing.T, srv *Server, arrived func() bool) {
	t.Helper()
	waitFor(t, 5*time.Second, "the watch to deliver a probe call", func() bool {
		srv.journal.Record(&journal.Call{Method: "/probe.v1.Probe/Ready", StubID: "probe"})
		return arrived()
	})
}

// arrived reports whether ch yields a value within a short wait.
func arrived(ch <-chan struct{}) func() bool {
	return func() bool {
		select {
		case <-ch:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	}
}

// stopsPromptly runs GracefulStop and fails if it takes more than a fraction of
// shutdownGrace: waiting out the whole budget is exactly the bug.
func stopsPromptly(t *testing.T, srv *Server) {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace + dataGraceFloor + 5*time.Second):
		t.Fatal("GracefulStop never returned with a WatchCalls stream open")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("GracefulStop took %v with a WatchCalls stream open, want well under shutdownGrace (%v)",
			elapsed, shutdownGrace)
	}
}

// Bytes the data plane accepts but a proto3 string cannot carry must not break
// any response that renders them (design §5.2). Removing validUTF8 from the
// metadata value or from the method makes the RPCs below fail to serialize.
func TestInvalidUTF8FromIngressRendersInEveryCallResponse(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	httpClient, base := adminHTTPClient(t, srv)
	journals := adminv1connect.NewJournalServiceClient(httpClient, base)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stream, err := journals.WatchCalls(ctx, connect.NewRequest(&adminv1.WatchCallsRequest{}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	watched := make(chan *adminv1.Call)
	go func() {
		defer close(watched)
		for stream.Receive() {
			watched <- stream.Msg().GetCall()
		}
	}()
	recordUntilWatched(t, srv, func() bool {
		select {
		case <-watched:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	})

	rawDataPlaneCall(t, srv.DataAddr().String(), "/shop.v1.OrderService/GetOrder",
		hpack.HeaderField{Name: "x-probe", Value: "ok\xffbad"})
	rawDataPlaneCall(t, srv.DataAddr().String(), "/shop.v1.OrderService/Get\xffOrder")

	const wantValue, wantMethod = "ok�bad", "/shop.v1.OrderService/Get�Order"
	var sawValue, sawMethod bool
	for !sawValue || !sawMethod {
		select {
		case call, ok := <-watched:
			if !ok {
				t.Fatalf("WatchCalls ended before delivering both calls: %v", stream.Err())
			}
			sawValue = sawValue || metadataValue(call, "x-probe") == wantValue
			sawMethod = sawMethod || call.Method == wantMethod
		case <-ctx.Done():
			t.Fatalf("WatchCalls delivered the metadata call: %v, the :path call: %v; want both", sawValue, sawMethod)
		}
	}

	listed, err := journals.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	var listedValue, listedMethod bool
	for _, call := range listed.Msg.Calls {
		listedValue = listedValue || metadataValue(call, "x-probe") == wantValue
		listedMethod = listedMethod || call.Method == wantMethod
	}
	if !listedValue || !listedMethod {
		t.Errorf("ListCalls rendered the metadata call: %v, the :path call: %v; want both", listedValue, listedMethod)
	}

	verdict, err := adminv1connect.NewVerifyServiceClient(httpClient, base).VerifyCalls(ctx,
		connect.NewRequest(&adminv1.VerifyCallsRequest{
			Method:          "/shop.v1.OrderService/GetOrder",
			MatcherDocument: "metadata:\n  x-probe: { eq: nope }\n",
			Times:           &adminv1.Times{Exactly: proto.Int32(1)},
		}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	if verdict.Msg.Passed || len(verdict.Msg.Actual) != 1 || metadataValue(verdict.Msg.Actual[0].Call, "x-probe") != wantValue {
		t.Errorf("VerifyCalls = %v, want a failure whose actual call carries x-probe %q", verdict.Msg, wantValue)
	}
}

// A tail must not hold teardown for the grace period: WatchCalls ends its
// stream with UNAVAILABLE as soon as Stopping closes (design §6). Without that,
// GracefulStop waits out shutdownGrace, which stopsPromptly catches.
func TestWatchCallsEndsWithUnavailableWhenTheServerStops(t *testing.T) {
	t.Run("connect over HTTP/1.1", func(t *testing.T) {
		srv := startWithAdmin(t, Options{})
		httpClient, base := adminHTTPClient(t, srv)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := adminv1connect.NewJournalServiceClient(httpClient, base).WatchCalls(ctx,
			connect.NewRequest(&adminv1.WatchCallsRequest{}))
		if err != nil {
			t.Fatalf("WatchCalls: %v", err)
		}
		defer stream.Close()
		delivered := make(chan struct{}, 1)
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			for stream.Receive() {
				select {
				case delivered <- struct{}{}:
				default:
				}
			}
		}()
		recordUntilWatched(t, srv, arrived(delivered))

		stopsPromptly(t, srv)
		<-ended
		if err := stream.Err(); connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "server shutting down") {
			t.Fatalf("stream ended with %v, want unavailable: server shutting down", err)
		}
	})

	t.Run("grpc over h2c", func(t *testing.T) {
		srv := startWithAdmin(t, Options{})
		conn := adminGRPCConn(t, srv)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true},
			adminv1connect.JournalServiceWatchCallsProcedure)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := stream.SendMsg(&adminv1.WatchCallsRequest{}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		delivered := make(chan struct{}, 1)
		final := make(chan error, 1)
		go func() {
			for {
				if err := stream.RecvMsg(&adminv1.WatchCallsResponse{}); err != nil {
					final <- err
					return
				}
				select {
				case delivered <- struct{}{}:
				default:
				}
			}
		}()
		recordUntilWatched(t, srv, arrived(delivered))

		stopsPromptly(t, srv)
		if err := <-final; status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "server shutting down") {
			t.Fatalf("stream ended with %v, want Unavailable: server shutting down", err)
		}
	})
}
```

- [ ] **Step 3: Run the tests**

Run: `go test ./server/ -run 'TestBootContractThroughTheAdminAPI|TestInvalidUTF8FromIngressRendersInEveryCallResponse|TestWatchCallsEndsWithUnavailableWhenTheServerStops' -race -count=1 -v`
Expected: PASS. The behavior already exists; this task proves it end to end.

- [ ] **Step 4: Mutation-check each test's discriminator**

Apply each mutation alone, run the named test, confirm the failure, then restore the code before the next one:

1. In `internal/admin/render.go`, `renderMetadata`: change `validUTF8(value)` to `value`.
   Run `TestInvalidUTF8FromIngressRendersInEveryCallResponse`. Expected: FAIL — `WatchCalls ended before delivering both calls`, because the stream's `Send` cannot marshal the call.
2. In `internal/admin/render.go`, `renderCall`: change `Method: validUTF8(call.Method)` to `Method: call.Method`.
   Run the same test. Expected: FAIL the same way.
3. In `internal/admin/journal.go`, `streamCalls`: delete the outer `case <-stopping:` arm and its `return`.
   Run `TestWatchCallsEndsWithUnavailableWhenTheServerStops`. Expected: FAIL in both subtests — `GracefulStop took ~5s with a WatchCalls stream open`.
4. In `internal/admin/admin.go`, `Install`: change the `NewStubServiceHandler` line's `&stubService{deps: deps}` to `&stubService{deps: Deps{Registry: deps.Registry, Store: stub.NewStore(), Journal: deps.Journal}}` (`admin.go` already imports `internal/stub`).
   Run `TestBootContractThroughTheAdminAPI`. Expected: FAIL — the data-plane call answers `NotFound`, because the stub landed in a store the data plane never reads. This pins the server-to-admin wiring seam 4a's review flagged.

After restoring all four, re-run the Step 3 command: PASS.

- [ ] **Step 5: Run the whole suite under the race detector**

Run: `go build ./... && go vet ./... && go test ./... -race -count=1 -timeout 900s`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add server/admin_data_test.go
git commit -m "test(server): admin data services end to end over h2c and HTTP/1.1"
```

---

## Verification checklist

Run before declaring the phase done, from the repository root:

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 -timeout 900s
```

Then confirm the phase's guard rails:

- [ ] `git diff --stat 11446f4..HEAD -- api/ gen/ go.mod go.sum` prints nothing: no contract or module change.
- [ ] `git diff --stat 11446f4..HEAD -- internal/match internal/dataplane internal/cli` prints nothing.
- [ ] `git diff --stat 11446f4..HEAD -- internal/schema internal/journal internal/stub` lists only `registry.go`, `registry_test.go`, `verify.go`, `verify_test.go`, `stub.go`, `stub_test.go`, and `document_test.go`: exactly the three core changes.
- [ ] `grep -n 'Unimplemented' internal/admin/*.go | grep -v _test.go` prints nothing: no service struct still embeds an Unimplemented handler.
- [ ] `grep -rn --include='*.go' 'Phase 4b' . | grep -v '^./gen/'` prints nothing.
- [ ] `grep -rn ':6566' --include='*_test.go' .` shows only the three non-binding occurrences 4a already recorded: the fake `AdminAddr` string in `testDeps`, the `DefValue` assertion in `internal/cli/serve_test.go`, and a comment.
- [ ] `grep -n 'ConfigureServer' internal/admin/admin.go` still returns the call.
- [ ] `grep -n 'ReadTimeout\|WriteTimeout' server/admin.go` shows only the comment explaining their absence.
- [ ] `make lint-api` passes.

---

## Self-review notes

**Spec coverage.**

| Design section | Tasks |
|---|---|
| §1 scope; §2 package boundary and `Deps.Stopping` | 6–11 |
| §3.1 `ErrUnknownMethod` | 1, with its codes asserted in 4, 8, and 11 |
| §3.2 `Report` snapshots | 2, rendered in 11 |
| §3.3 int32 bounds | 3, asserted through the API in 8 |
| §4.1 method filters and envelopes | 8 (`ListStubs`), 9 and 10 (journal filters), 11 (`VerifyCalls.method`) |
| §4.2–§4.5 per-service semantics | 7, 8, 9–10, 11 |
| §5.1 rendering | 5 |
| §5.2 invalid UTF-8 | 5, 7, 8, 9, and 12 end to end |
| §6 `WatchCalls` lifecycle | 10, and 12 end to end |
| §7 error table | 4, with every row asserted through a real RPC in 7–11 |
| §8 size caps | 6 |
| §9 testing | Distributed to the tasks that own each behavior, and every discriminating test has a named mutation check |

**Deliberate deviations from the design.**

1. **Envelope sanitization test location.** The test for stub envelope sanitization lives in `internal/admin/stub_test.go` (Task 8), not in `render_test.go` where §9 lists it, because §2 places `stubEnvelope` in `stub.go`.
2. **Non-UTF-8 descriptor path test level.** It runs at the handler level (Task 7) rather than through `server.Start`, where §9's integration list places it. Nothing in `server` participates; what it pins is the handler's own response serialization.
3. **`CreateStub` parse diagnostic label.** Parse diagnostics carry the `document:` label that compile diagnostics already carry. §4.3 says diagnostics pass through verbatim, and the diagnostic text is still unchanged — only labeled — so both stub RPCs name the failing document the same way.

**Known soft spots.**

- **Eviction test.** The RPC-level eviction test (Task 10) relies on loopback socket buffers holding far less than the 64 MiB it pushes. If a platform ever buffered all of it, the stream would never be evicted and the test would fail on its 30s deadline — loudly, not as a silent pass.
- **Stream readiness.** It is established by recording probe calls until one is delivered. The probes are tagged so readers skip them, and the wait is bounded at 5s.
- **`stopsPromptly`'s 2s bound.** It sits well under `shutdownGrace` (5s), leaving room for race-detector slowdown. The failure it catches takes about 5s.

**Plan complete.**

## Post-execution amendments

- **Task 10, Step 3.** The `WatchCalls` code block was missing the `stream.Send(nil)` call right after `sub := j.deps.Journal.Watch(...)`. Execution found that without it, connect's server-streaming client does not return from establishing the call until the handler writes something, so the task's own RPC tests deadlocked until their context deadline; the block now includes the call, routed through `connectError`, matching the shipped `internal/admin/journal.go`.
- **Task 6, Step 1.** The `TestRequestSizeCapsArePerService` `others` map's `JournalService.WatchCalls` case, asserting exactly `ResourceExhausted`, was flaky under `-race`: execution found the client can observe the broken transport (`CodeInternal`) instead of the server's status when the oversized write races the server's rejection, so the map's value type now carries a list of accepted codes and that one entry accepts both `ResourceExhausted` and `CodeInternal`, while every other entry still asserts exactly `ResourceExhausted`.
