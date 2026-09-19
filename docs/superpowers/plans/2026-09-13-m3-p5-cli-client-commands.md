# Simulacra M3 Phase 5 — CLI Client Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `simulacra stub list|add|rm|export`, `calls list|tail`, `verify`, and `schema register|list` — client commands that talk to a running server's admin plane over Connect/HTTP-1.1 — with one shared client, one output contract, and a 0/1/2 exit contract CI can read.

**Architecture:** Everything lands in `internal/cli`. `client.go` owns address resolution and the five generated service clients behind a `clientFactory` seam; `output.go` owns `--output text|json`; `exit.go` owns the exit policy, applied via cobra's `ExecuteC` and a per-command annotation so `serve` and `check` are untouched. One core change exports the file-grammar splitter that already exists in `internal/stub`. Nothing in `server`, `internal/admin`, or any data-plane package changes.

**Tech Stack:** Go 1.25.0, module `github.com/yinghanhung/simulacra`. `github.com/spf13/cobra` v1.10.2, `connectrpc.com/connect` v1.20.0, `google.golang.org/protobuf` v1.36.11, `google.golang.org/grpc` v1.82.0 are already direct dependencies. No module is added.

**Spec:** `docs/superpowers/specs/2026-09-12-m3-p5-cli-client-commands-design.md`, cited as "design §N". Approved at `47f9598`. Phase 4b is complete at `74b74d6`.

## Global Constraints

- **No changes to `api/` or `gen/`.** The contract was frozen in Phase 2.
- **No new modules:** `go.mod` and `go.sum` stay unchanged.
- **No changes to `server/`, `internal/admin/`, `internal/dataplane/`, `internal/match/`, `internal/journal/`, `internal/schema/`.** The only file touched outside `internal/cli` is `internal/stub/loader.go` (Task 1).
- **`serve` and `check` keep exiting 1 on error.** The 0/1/2 table is scoped to the new client commands by annotation (design §5).
- **Server diagnostics are surfaced verbatim.** A command may prefix its own source (`stubs.yaml#2: `) but never rewords, truncates, or re-classifies what the server said (design §6, M3 §11).
- **protojson output is not byte-stable.** Tests decode JSON and assert on fields; they never compare rendered JSON as a string. The repo already records this in `server/admin_data_test.go`'s `jsonField` helper.
- **Every test that discriminates carries a named mutation check** — state the mutation, confirm the test fails under it, revert.
- Run the full suite with `-count=1`; Go's test cache will otherwise hide a real regression.

---

## Pre-verified facts

Established by throwaway probes against the real admin plane before this plan was written, and since deleted. The plan quotes code that depends on each.

**Transport and client**

- A connect client against a closed port returns `unavailable: dial tcp 127.0.0.1:1: connect: connection refused`; `connect.CodeOf` is `unavailable`.
- A unary call against a listener that accepts the connection and never writes blocks until its context deadline and surfaces as `deadline_exceeded`. This is why `--timeout` exists.
- `adminv1connect`'s five clients are declared as **interfaces** (`gen/simulacra/admin/v1/adminv1connect/stub.connect.go:50`), so a decorator needs no new abstraction.
- connect v1.20.0's client applies a receive cap only when configured (`envelope.go:342` guards on `readMaxBytes > 0`); nothing sets it by default.

**Cobra**

- `ExecuteC` sets `c.ctx = context.Background()` when no context was supplied (`cobra@v1.10.2/command.go:1085`), and `internal/cli/root.go:21` supplies none. **Cobra installs no signal handling.**
- `ExecuteC` returns the found subcommand **with its annotations** even when flag parsing fails: `--tiems` on an annotated `verify` returns `cmd="verify"`, `annotations=map[exit:client]`, `err="unknown flag: --tiems"`. An unknown top-level command returns the root.
- `cmd.Flags().Changed("addr")` is **false** when only `SIMULACRA_ADDR` is set, and the flag still holds its default.
- Cobra accepts a negative duration silently: `--timeout -1s` parses to `-1s` with a nil error.
- `newServeCmd()` already delegates to `newServeCmdWithListen(net.Listen)` (`internal/cli/serve.go:18`) — the injection precedent Task 3 follows.

**Signals (measured on a built binary driving the design's exact policy)**

- `calls tail` under a per-command `signal.NotifyContext`, sent a real SIGINT, exits **0**. Sent SIGTERM, also **0**.
- A unary command under the same context, sent a real SIGINT, exits **2**.
- `waitAndShutdownContextStop` forces immediately once its context is cancelled (`internal/cli/shutdown.go:53`). A root-level signal context would therefore collapse `serve`'s two-stage shutdown; the signal context is per client command for this reason.

**Rendering**

- `protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}` renders `VerifyCallsResponse{Passed: false}` as `"passed": false, "matched": 0, "explanation": "", "actual": []`, and a `Stub` with enums by name (`"STUB_SHAPE_UNARY"`) and `"hits": "0"` — int64 as a JSON string.
- `protojson.MarshalOptions.EmitDefaultValues` exists in the pinned v1.36.11 (`encoding/protojson/encode.go:103`).

**Stubs and schemas**

- `stubDocuments` has exactly one caller, `parseFile` (`internal/stub/loader.go:92`).
- `stubDocuments([]byte("[]\n"))` returns 0 documents and a nil error. `stub.RenderSequence(nil)` returns `"[]\n"`, so an empty export round-trips as zero stubs.
- A two-item stub file splits into 2 normalized documents, each rendered as block YAML without the leading `- `.
- Registering a `FileDescriptorSet` naming a path twice fails `invalid_argument: … proto: file appears multiple times: "google/protobuf/timestamp.proto"`.
- Registering a set with an unresolved import fails `invalid_argument: … descriptor sets must be self-contained; build with 'buf build -o' or 'protoc --include_imports'`.
- Registering `testdata/protos` onto an empty registry returns `registered_files` sorted (`google/protobuf/any.proto`, `google/protobuf/timestamp.proto`, `shop/v1/order.proto`) and `service_count` 2. Re-registering returns zero registered files and still succeeds.
- API-origin stub IDs are assigned `api-1`, `api-2`, … in creation order.

**Journal**

- Teardown ends `WatchCalls` with `UNAVAILABLE` and the message `server shutting down` (`internal/admin/journal.go:127`, pinned at `server/admin_data_test.go:398`).
- Slow-consumer eviction ends it with `RESOURCE_EXHAUSTED` (`internal/admin/journal_test.go:369`).
- After an eviction, the **same** client re-establishes a watch successfully and is delivering again within about 1ms.
- `WatchCalls`'s `Send(nil)` header flush is not delivered as a message: a client `Receive` against an idle watch blocks rather than returning an empty call.
- `srv.journal` is unexported, so `internal/cli` tests cannot record calls directly — they make real data-plane calls (Task 9's helper).

**testdata**

- `testdata/protos/shop/v1/order.proto` declares `shop.v1.OrderService` with `GetOrder` (unary), `WatchOrder` (server stream), `UploadOrders` (client stream), and `Chat` (bidi). `GetOrderRequest` has `order_id`, `customer`, `items`, `tags`; `GetOrderResponse` has `order_id`, `status`, `note`, `eta`, `extra`.

---

## File structure

Created in `internal/cli`:

| File | Responsibility | Task |
|---|---|---|
| `exit.go` | `errAssertionFailed`, `errInterrupted`, the annotation constants, `exitCode` | 2 |
| `exit_test.go` | Exit policy table, including flag errors and `serve`/`check` | 2 |
| `client.go` | `adminClient`, `clientFactory`, `clientFlags`, address resolution, `callContext`, `signalContext`, `rpcError` | 3 |
| `client_test.go` | Address precedence, base URL, negative timeout, deadline behaviour | 3 |
| `output.go` | `outputFlag`, `jsonOptions`, `streamJSONOptions`, `writeJSON`, `writeJSONLine`, `newTable` | 4 |
| `output_test.go` | Format validation, proto names, `passed: false`, NDJSON | 4 |
| `schema.go` | `schema register\|list`, `mergeDescriptorSets` | 5 |
| `schema_test.go` | Merge dedupe/conflict, register, list | 5 |
| `stub.go` | `stub list\|rm\|export\|add`, `stubAdder` | 6, 7, 8 |
| `stub_test.go` | Filters, export combinations, preflight, compensation | 6, 7, 8 |
| `calls.go` | `calls list\|tail`, `tailLoop` | 9, 10 |
| `calls_test.go` | Filters, tail delivery, reconnect, teardown | 9, 10 |
| `verify.go` | `verify`, `parseTimes` | 11 |
| `verify_test.go` | `--times` grammar, int32 bounds, verdicts, exit codes | 11 |
| `harness_test.go` | `startCommandServer`, `runCmd`, `decodeJSON`, data-plane call helper | 3, 9 |

Modified:

| File | Change | Task |
|---|---|---|
| `internal/stub/loader.go` | `stubDocuments` → exported `SplitDocuments` | 1 |
| `internal/cli/root.go` | `Execute` onto `ExecuteC`; four `AddCommand` calls | 2, 12 |

Files are named after the command they implement, following design §2 and the existing `serve.go` / `check.go`. That `internal/cli/stub.go` sits in package `cli` while package `stub` is imported into it is a collision in prose only — Go file names bind nothing. No existing file is renamed.

---

### Task 1: `stub.SplitDocuments`

Export the file-grammar splitter the CLI needs. `internal/stub/loader.go:107` already renders each sequence item of each `---` document as a normalized, self-contained per-stub document, expanding aliases so an item that aliased a sibling's anchor still stands alone. Exporting it — rather than writing a second splitter in `internal/cli` — is what keeps `stub.proto`'s promise that files and the API can never drift (design §7).

**Files:**
- Modify: `internal/stub/loader.go:92` (the caller) and `internal/stub/loader.go:107` (the function)
- Test: `internal/stub/loader_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func stub.SplitDocuments(data []byte) ([]string, error)` — splits one stub file's bytes into normalized per-stub documents, in file order. Zero documents and a nil error for an empty sequence. Used by Task 8.

- [ ] **Step 1: Write the failing test**

Append to `internal/stub/loader_test.go`:

```go
// SplitDocuments is the file-grammar splitter the CLI's `stub add` reuses, so
// that files and the admin API are split by one implementation (design §7).
func TestSplitDocumentsIsTheExportedFileSplitter(t *testing.T) {
	t.Run("multi-document file splits in order", func(t *testing.T) {
		file := []byte("- method: shop.v1.OrderService/GetOrder\n" +
			"  respond:\n    message: { note: first }\n" +
			"---\n" +
			"- method: shop.v1.OrderService/WatchOrder\n" +
			"  respond:\n    stream: [{ message: { note: second } }]\n")
		docs, err := stub.SplitDocuments(file)
		if err != nil {
			t.Fatalf("SplitDocuments: %v", err)
		}
		if len(docs) != 2 {
			t.Fatalf("got %d documents, want 2: %q", len(docs), docs)
		}
		if !strings.Contains(docs[0], "note: first") || strings.Contains(docs[0], "second") {
			t.Errorf("document 0 = %q, want only the first stub", docs[0])
		}
		if !strings.Contains(docs[1], "note: second") {
			t.Errorf("document 1 = %q, want the second stub", docs[1])
		}
		// Each document must stand alone as one mapping, not a list item.
		if strings.HasPrefix(docs[0], "- ") {
			t.Errorf("document 0 = %q, want a bare mapping", docs[0])
		}
	})

	t.Run("empty sequence yields no documents", func(t *testing.T) {
		docs, err := stub.SplitDocuments([]byte("[]\n"))
		if err != nil {
			t.Fatalf("SplitDocuments([]): %v", err)
		}
		if len(docs) != 0 {
			t.Fatalf("got %d documents, want 0: %q", len(docs), docs)
		}
	})

	t.Run("a bare mapping is rejected as not a stub file", func(t *testing.T) {
		_, err := stub.SplitDocuments([]byte("method: shop.v1.OrderService/GetOrder\n"))
		if err == nil {
			t.Fatal("SplitDocuments accepted a bare mapping; stub files hold a list")
		}
	})

	t.Run("aliases are expanded so each document stands alone", func(t *testing.T) {
		file := []byte("- &base\n  method: shop.v1.OrderService/GetOrder\n" +
			"  respond:\n    message: { note: shared }\n" +
			"- *base\n")
		docs, err := stub.SplitDocuments(file)
		if err != nil {
			t.Fatalf("SplitDocuments: %v", err)
		}
		if len(docs) != 2 {
			t.Fatalf("got %d documents, want 2", len(docs))
		}
		if strings.Contains(docs[1], "*base") || !strings.Contains(docs[1], "note: shared") {
			t.Errorf("document 1 = %q, want the alias expanded", docs[1])
		}
	})
}
```

If `internal/stub/loader_test.go` is an internal test (package `stub`), drop the `stub.` qualifier and the import; check the file's package clause first. If `strings` is not yet imported there, add it.

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/stub/ -run TestSplitDocumentsIsTheExportedFileSplitter -count=1
```

Expected: FAIL — `undefined: stub.SplitDocuments` (or `undefined: SplitDocuments`).

- [ ] **Step 3: Write the implementation**

In `internal/stub/loader.go`, rename the function and update its doc comment. Replace:

```go
// stubDocuments renders each sequence item of each YAML document as a
// normalized per-stub document. The struct decoder above already rejected
// non-sequence documents, so the sequence error here can only fire on
// shapes it also rejected; the count check in parseFile is the belt to
// that suspender.
func stubDocuments(data []byte) ([]string, error) {
```

with:

```go
// SplitDocuments renders each sequence item of each YAML document as a
// normalized per-stub document, in file order. It is the one splitter for the
// file grammar: parseFile uses it to load a directory, and the CLI's
// `stub add -f` uses it to turn a file into one CreateStub call per stub, so
// files and the admin API can never diverge on what "one stub" means.
//
// Documents are self-contained: aliases are expanded, comments and styling
// stripped. A file that is not a list of stubs is an error.
//
// The struct decoder in parseFile already rejected non-sequence documents, so
// the sequence error here can only fire on shapes it also rejected; the count
// check in parseFile is the belt to that suspender.
func SplitDocuments(data []byte) ([]string, error) {
```

Then update the single caller at `internal/stub/loader.go:92`:

```go
	docs, err := SplitDocuments(data)
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/stub/ -count=1
```

Expected: PASS, including the pre-existing loader tests.

- [ ] **Step 5: Confirm no stale reference remains**

```bash
grep -rn 'stubDocuments' --include='*.go' . | grep -v '^./gen/'
```

Expected: no output.

- [ ] **Step 6: Run the whole suite**

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

- [ ] **Step 7: Commit**

```bash
git add internal/stub/loader.go internal/stub/loader_test.go
git commit -m "feat(stub): export SplitDocuments as the one file-grammar splitter

The CLI's stub add splits a file client-side into one CreateStub call per
stub. Exporting the splitter parseFile already uses, rather than writing a
second one in internal/cli, is what keeps files and the admin API from
diverging on what one stub is."
```

---

### Task 2: The exit-code policy

`verify` must exit 1 when an assertion fails and 2 when it could not run, so a CI job can tell a failed test from a broken one. Today `Execute` exits 1 on every error (design §5).

The policy is scoped by a cobra annotation so `serve` and `check` are untouched. This works on flag errors too: `ExecuteC` returns the found subcommand with its annotations even when parsing failed.

**Files:**
- Create: `internal/cli/exit.go`, `internal/cli/exit_test.go`
- Modify: `internal/cli/root.go:20-26`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `var errAssertionFailed error` — returned only by `verify` on a failed verdict; maps to exit 1 with no `error:` prefix.
  - `var errInterrupted error` — returned by a non-`tail` client command cut short by a signal; maps to exit 2.
  - `const exitAnnotation = "exit"`, `const exitClient = "client"` — the annotation a client command carries.
  - `func clientAnnotations() map[string]string` — the annotation map every client command sets.
  - `func exitCode(cmd *cobra.Command, err error) int`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/exit_test.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

// The exit contract (design §5): 0 success, 1 assertion failed, 2 operational
// error — and the table governs annotated client commands only, so serve and
// check keep exiting 1 on any error.
func TestExitCodeContract(t *testing.T) {
	client := &cobra.Command{Use: "verify", Annotations: clientAnnotations()}
	plain := &cobra.Command{Use: "serve"}

	cases := []struct {
		name string
		cmd  *cobra.Command
		err  error
		want int
	}{
		{"client success", client, nil, 0},
		{"client assertion failure", client, errAssertionFailed, 1},
		{"client wrapped assertion failure", client, fmt.Errorf("verify: %w", errAssertionFailed), 1},
		{"client operational error", client, errors.New("connection refused"), 2},
		{"client interrupted", client, errInterrupted, 2},
		{"plain command error stays 1", plain, errors.New("boom"), 1},
		{"plain command success", plain, nil, 0},
		{"nil command error stays 1", nil, errors.New("unknown command"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.cmd, tc.err); got != tc.want {
				t.Fatalf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

// A typo'd flag on verify must not be reported as a failed assertion: it did
// not run. ExecuteC carries the subcommand's annotations through a flag error,
// which is what makes this reachable.
func TestFlagErrorOnClientCommandExitsTwo(t *testing.T) {
	sub := &cobra.Command{
		Use:         "verify",
		Annotations: clientAnnotations(),
		RunE:        func(*cobra.Command, []string) error { return nil },
	}
	sub.Flags().String("times", "", "")
	root := &cobra.Command{Use: "simulacra", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(sub)
	root.SetArgs([]string{"verify", "--tiems", "exactly=2"})

	cmd, err := root.ExecuteC()
	if err == nil {
		t.Fatal("expected a flag error")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2 — a typo'd flag is not a failed assertion", got)
	}
}

// An unknown top-level command resolves to the root, which carries no
// annotation, so it keeps the pre-existing exit 1.
func TestUnknownCommandExitsOne(t *testing.T) {
	root := &cobra.Command{Use: "simulacra", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(&cobra.Command{Use: "verify", Annotations: clientAnnotations()})
	root.SetArgs([]string{"bogus"})

	cmd, err := root.ExecuteC()
	if err == nil {
		t.Fatal("expected an unknown-command error")
	}
	if got := exitCode(cmd, err); got != 1 {
		t.Fatalf("exitCode = %d, want 1", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestExitCodeContract|TestFlagErrorOnClientCommandExitsTwo|TestUnknownCommandExitsOne' -count=1
```

Expected: FAIL — `undefined: clientAnnotations`, `undefined: exitCode`, `undefined: errAssertionFailed`, `undefined: errInterrupted`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/exit.go`:

```go
package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// errAssertionFailed marks a verdict of "no" — the assertion ran and did not
// hold. It is distinct from every operational failure so that a CI job can
// tell a failed test from a broken one (design §5). Only `verify` returns it.
var errAssertionFailed = errors.New("assertion failed")

// errInterrupted is what a client command other than `calls tail` returns when
// a signal cut it short. The work did not finish, so reporting success would
// tell a script otherwise (design §5).
var errInterrupted = errors.New("interrupted before completion")

const (
	// exitAnnotation scopes the 0/1/2 exit contract to the Phase 5 client
	// commands. Commands without it — serve, check, the root — keep exiting 1
	// on any error, so no shipped command changes its exit code.
	exitAnnotation = "exit"
	exitClient     = "client"
)

// clientAnnotations is the annotation map every Phase 5 client command sets.
// Setting it in one place is what keeps a later command from silently
// inheriting the wrong exit policy by forgetting the literal.
func clientAnnotations() map[string]string {
	return map[string]string{exitAnnotation: exitClient}
}

// exitCode maps the command that ran and the error it returned onto a process
// exit status. cmd is whatever ExecuteC resolved, which is the found
// subcommand even when its flags failed to parse, and nil is tolerated.
func exitCode(cmd *cobra.Command, err error) int {
	if err == nil {
		return 0
	}
	if cmd == nil || cmd.Annotations[exitAnnotation] != exitClient {
		return 1
	}
	if errors.Is(err, errAssertionFailed) {
		return 1
	}
	return 2
}
```

Replace `Execute` in `internal/cli/root.go`:

```go
func Execute() {
	root := newRootCmd()
	cmd, err := root.ExecuteC()
	// A failed assertion has already printed its verdict; it is the command's
	// output, not a diagnostic about the command (design §5).
	if err != nil && !errors.Is(err, errAssertionFailed) {
		root.PrintErrln("error:", err)
	}
	if code := exitCode(cmd, err); code != 0 {
		os.Exit(code)
	}
}
```

Add `"errors"` to `root.go`'s imports.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check the annotation scoping**

Change `exitCode`'s annotation guard to `if cmd == nil {` — dropping the annotation check, so every command gets the client policy. Confirm `TestExitCodeContract/plain_command_error_stays_1` FAILS. Revert.

This is the check that matters: without it, the test suite would still pass while `serve` silently moved from exit 1 to exit 2.

- [ ] **Step 6: Run the whole suite**

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

- [ ] **Step 7: Commit**

```bash
git add internal/cli/exit.go internal/cli/exit_test.go internal/cli/root.go
git commit -m "feat(cli): scope a 0/1/2 exit contract to client commands

verify must exit 1 when an assertion fails and 2 when it could not run, so
CI can tell a failed test from a broken job. The policy keys off a cobra
annotation rather than applying to every command, leaving serve and check
on their existing exit 1.

ExecuteC carries a subcommand's annotations through a flag error, so a
typo'd flag on verify exits 2 rather than reading as a failed assertion."
```

---

### Task 3: The admin client

One `adminClient` bundles the five generated service clients against one admin plane, resolves `--addr` against `SIMULACRA_ADDR`, and applies the per-RPC deadline. The fields are the generated **interfaces**, so Task 8's fault-injection tests wrap one without a new abstraction.

`clientFactory` is the injection point, following `newServeCmdWithListen(net.Listen)`'s existing precedent: production constructors pass `newAdminClient`, tests pass a factory that wraps a client in a decorator (design §8).

**Files:**
- Create: `internal/cli/client.go`, `internal/cli/client_test.go`, `internal/cli/harness_test.go`

**Interfaces:**
- Consumes: Task 2's `clientAnnotations`, `errInterrupted`.
- Produces:
  - `type adminClient struct { Schema …; Stub …; Journal …; Verify …; Control … }` with unexported `timeout`.
  - `type clientFactory func(*cobra.Command) (*adminClient, error)`.
  - `func newAdminClient(cmd *cobra.Command) (*adminClient, error)`.
  - `type clientFlags struct{}` with `func (*clientFlags) register(cmd *cobra.Command)` and `func (*clientFlags) registerNoTimeout(cmd *cobra.Command)`.
  - `func (c *adminClient) callContext(ctx context.Context) (context.Context, context.CancelFunc)`.
  - `func signalContext(cmd *cobra.Command) (context.Context, context.CancelFunc)`.
  - `func rpcError(sigCtx context.Context, err error) error`.
  - Test helpers: `startCommandServer(t) *server.Server`, `runCmd(t, cmd, args...) (stdout, stderr string, err error)`.

- [ ] **Step 1: Write the test harness**

Create `internal/cli/harness_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/server"
)

// startCommandServer boots a real in-process server with both planes on
// ephemeral ports. M3 §12 specifies command tests run against a real server,
// not a fake: the CLI's job is translating the admin contract, and a fake
// would be translating it a second time.
func startCommandServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.Start(context.Background(), server.Options{
		ProtoDirs:   []string{"../../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 64,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// adminAddr is the --addr value pointing at srv's admin plane.
func adminAddr(srv *server.Server) string {
	return srv.AdminAddr().String()
}

// runCmd executes cmd with args, capturing its streams separately so a test
// can assert that payload went to stdout and commentary to stderr (design §4).
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// decodeJSON unmarshals rendered output into a generic map. protojson output
// is deliberately not byte-stable, so tests decode and assert on fields rather
// than comparing strings — the same rule server/admin_data_test.go follows.
func decodeJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatalf("decoding %q: %v", text, err)
	}
	return fields
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/cli/client_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// probeCmd is a command carrying the client flags, used to drive
// newAdminClient the way a real command does.
func probeCmd(t *testing.T, capture func(*adminClient, *cobra.Command) error, args ...string) error {
	t.Helper()
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:         "probe",
		Annotations: clientAnnotations(),
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := newAdminClient(c)
			if err != nil {
				return err
			}
			return capture(client, c)
		},
	}
	flags.register(cmd)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	_, _, err := runCmd(t, cmd, args...)
	return err
}

// Precedence is flag > SIMULACRA_ADDR > default (design §3).
func TestAddrResolutionPrecedence(t *testing.T) {
	t.Run("default when neither is set", func(t *testing.T) {
		var got string
		if err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "http://localhost:6566" {
			t.Fatalf("baseURL = %q, want http://localhost:6566", got)
		}
	})

	t.Run("env wins over the default", func(t *testing.T) {
		t.Setenv("SIMULACRA_ADDR", "127.0.0.1:7000")
		var got string
		if err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "http://127.0.0.1:7000" {
			t.Fatalf("baseURL = %q, want the env value", got)
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("SIMULACRA_ADDR", "127.0.0.1:7000")
		var got string
		err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}, "--addr", "127.0.0.1:8000")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "http://127.0.0.1:8000" {
			t.Fatalf("baseURL = %q, want the flag value", got)
		}
	})

	t.Run("an explicit scheme is kept verbatim", func(t *testing.T) {
		var got string
		err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}, "--addr", "https://admin.example:443/simulacra")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "https://admin.example:443/simulacra" {
			t.Fatalf("baseURL = %q, want the URL unchanged", got)
		}
	})
}

// Cobra accepts a negative duration silently, and a negative deadline expires
// every context, so every RPC would fail instantly and the user would be
// debugging the server (design §3).
func TestNegativeTimeoutIsRejected(t *testing.T) {
	err := probeCmd(t, func(*adminClient, *cobra.Command) error { return nil },
		"--timeout", "-1s")
	if err == nil {
		t.Fatal("a negative --timeout was accepted")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error = %v, want it to name --timeout", err)
	}
}

func TestZeroTimeoutDisablesTheDeadline(t *testing.T) {
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Error("--timeout 0 still set a deadline")
		}
		return nil
	}, "--timeout", "0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestTimeoutAppliesADeadline(t *testing.T) {
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("no deadline applied")
		}
		if remaining := time.Until(deadline); remaining > 6*time.Second {
			t.Errorf("deadline is %v away, want about 5s", remaining)
		}
		return nil
	}, "--timeout", "5s")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// A server that accepts the connection and never responds is exactly what the
// deadline exists for: a closed port already fails fast, this does not.
func TestUnaryCallAgainstAHangingServerHitsTheDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn // hold it open, never respond
		}
	}()

	err = probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		_, err := c.Stub.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{}))
		return err
	}, "--addr", ln.Addr().String(), "--timeout", "500ms")
	if err == nil {
		t.Fatal("the call returned against a server that never responded")
	}
	if code := connect.CodeOf(err); code != connect.CodeDeadlineExceeded {
		t.Fatalf("code = %v, want deadline_exceeded", code)
	}
}

func TestUnreachableAddressIsUnavailable(t *testing.T) {
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		_, err := c.Stub.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{}))
		return err
	}, "--addr", "127.0.0.1:1")
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("code = %v, want unavailable", code)
	}
}

// A signal that arrives mid-call is reported as an interruption, not as
// whatever the transport happened to surface (design §5).
func TestRPCErrorReportsInterruptionWhenSignalled(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got := rpcError(cancelled, connect.NewError(connect.CodeUnavailable, errors.New("transport closed")))
	if !errors.Is(got, errInterrupted) {
		t.Fatalf("rpcError = %v, want errInterrupted", got)
	}

	live := context.Background()
	transport := connect.NewError(connect.CodeUnavailable, errors.New("transport closed"))
	if got := rpcError(live, transport); !errors.Is(got, transport) {
		t.Fatalf("rpcError = %v, want the transport error unchanged", got)
	}
}

// The client reaches a real server, proving the Connect-over-HTTP/1.1 path.
func TestClientReachesTheAdminPlane(t *testing.T) {
	srv := startCommandServer(t)
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		resp, err := c.Schema.ListServices(ctx, connect.NewRequest(&adminv1.ListServicesRequest{}))
		if err != nil {
			return err
		}
		if len(resp.Msg.GetServices()) == 0 {
			t.Error("no services reported by a server started with testdata protos")
		}
		return nil
	}, "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestAddrResolution|TestNegativeTimeout|TestZeroTimeout|TestTimeoutApplies|TestUnaryCallAgainst|TestUnreachableAddress|TestRPCError|TestClientReaches' -count=1
```

Expected: FAIL — `undefined: clientFlags`, `undefined: newAdminClient`, `undefined: rpcError`.

- [ ] **Step 4: Write the implementation**

Create `internal/cli/client.go`:

```go
package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

const (
	// addrEnv overrides --addr's default, but not an explicit flag.
	addrEnv = "SIMULACRA_ADDR"
	// defaultAddr matches what `serve --admin` binds by default.
	defaultAddr = "localhost:6566"
	// defaultTimeout bounds one unary RPC. A closed port fails fast on its
	// own; this is for a server or proxy that accepts the connection and
	// never returns headers (design §3).
	defaultTimeout = 30 * time.Second
)

// adminClient bundles the five generated service clients against one admin
// plane.
//
// The fields are the generated interfaces rather than concrete types, so a
// test can substitute a decorator around any one of them to inject a failure a
// healthy server cannot produce (design §8). Production always holds the
// generated clients.
type adminClient struct {
	Schema  adminv1connect.SchemaServiceClient
	Stub    adminv1connect.StubServiceClient
	Journal adminv1connect.JournalServiceClient
	Verify  adminv1connect.VerifyServiceClient
	Control adminv1connect.ControlServiceClient

	baseURL string
	timeout time.Duration
}

// clientFactory builds the client a command talks through. Production
// constructors pass newAdminClient; tests pass a factory that wraps one client
// in a decorator. This mirrors newServeCmdWithListen's existing seam.
type clientFactory func(*cobra.Command) (*adminClient, error)

// clientFlags registers the flags every client command shares.
type clientFlags struct{}

func (clientFlags) register(cmd *cobra.Command) {
	clientFlags{}.registerNoTimeout(cmd)
	cmd.Flags().Duration("timeout", defaultTimeout,
		"deadline for each RPC; 0 disables it")
}

// registerNoTimeout is for `calls tail`, whose stream is unbounded by design
// and which ends on a signal rather than a deadline (design §3).
func (clientFlags) registerNoTimeout(cmd *cobra.Command) {
	cmd.Flags().String("addr", defaultAddr,
		"admin plane address (overridden by the "+addrEnv+" environment variable)")
}

// newAdminClient builds the client from cmd's flags.
func newAdminClient(cmd *cobra.Command) (*adminClient, error) {
	addr, err := cmd.Flags().GetString("addr")
	if err != nil {
		return nil, err
	}
	// Changed() is false when only the environment set the address, which is
	// what keeps an explicit flag winning over the environment.
	if !cmd.Flags().Changed("addr") {
		if env := os.Getenv(addrEnv); env != "" {
			addr = env
		}
	}

	timeout := time.Duration(0)
	if cmd.Flags().Lookup("timeout") != nil {
		if timeout, err = cmd.Flags().GetDuration("timeout"); err != nil {
			return nil, err
		}
		if timeout < 0 {
			return nil, fmt.Errorf("--timeout must not be negative (got %s)", timeout)
		}
	}

	// Connect over HTTP/1.1 — the admin plane speaks it, so no h2c plumbing
	// is needed client-side.
	httpClient := &http.Client{}
	base := baseURL(addr)
	return &adminClient{
		Schema:  adminv1connect.NewSchemaServiceClient(httpClient, base),
		Stub:    adminv1connect.NewStubServiceClient(httpClient, base),
		Journal: adminv1connect.NewJournalServiceClient(httpClient, base),
		Verify:  adminv1connect.NewVerifyServiceClient(httpClient, base),
		Control: adminv1connect.NewControlServiceClient(httpClient, base),
		baseURL: base,
		timeout: timeout,
	}, nil
}

// baseURL turns an address into a base URL, leaving an explicit scheme alone
// so an admin plane behind a proxy or a path prefix is reachable.
func baseURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// callContext applies the per-RPC deadline. A zero timeout disables it, and
// `calls tail` never registers the flag at all.
func (c *adminClient) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// signalContext derives a command's interrupt-aware context.
//
// Cobra installs none: ExecuteC substitutes context.Background() when no
// context was supplied, so without this a client command dies on SIGINT by the
// process default — status 130 — rather than the exit code the contract
// promises.
//
// This is deliberately per command and never at the root. A root-level signal
// context would cancel serve's command context on the first interrupt, and
// waitAndShutdownContextStop forces immediately on a cancelled context, which
// would collapse serve's two-stage "interrupt again to force" behaviour.
func signalContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
}

// rpcError converts a failed RPC into the error the command returns. When the
// command's own signal context has ended, the failure is an interruption
// whatever the transport reported: a cancelled call surfaces as a transport
// error that says nothing about why.
func rpcError(sigCtx context.Context, err error) error {
	if sigCtx.Err() != nil {
		return errInterrupted
	}
	return err
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 6: Mutation-check the address precedence**

Drop the `Changed("addr")` guard so the environment always wins:

```go
	if env := os.Getenv(addrEnv); env != "" {
		addr = env
	}
```

Confirm `TestAddrResolutionPrecedence/flag_wins_over_env` FAILS. Revert.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/client.go internal/cli/client_test.go internal/cli/harness_test.go
git commit -m "feat(cli): add the shared admin client and its injection seam

One adminClient bundles the five generated service clients against one
admin plane, resolving --addr against SIMULACRA_ADDR with the flag winning,
and bounding each unary RPC with --timeout. A negative timeout is rejected:
cobra accepts one silently and it would expire every context, sending the
user to debug the server.

The fields are the generated interfaces and commands take a clientFactory,
following newServeCmdWithListen, so later tests can inject failures a
healthy server cannot produce.

signalContext is per command, never at the root: a root-level signal
context cancels serve's context on the first interrupt, and
waitAndShutdownContextStop forces on a cancelled context, which would
collapse serve's two-stage shutdown."
```

---

### Task 4: The output contract

`--output text|json` on read commands. JSON is the response message rendered as the frozen contract shape; text is a `tabwriter` table. Payload goes to stdout, commentary to stderr, so `stub export` and `--output json` pipe cleanly (design §4).

`--output` has **no shorthand**: `-o` belongs to `stub export --out` (Task 7).

**Files:**
- Create: `internal/cli/output.go`, `internal/cli/output_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type outputFlag struct{ format string }` with `register(*cobra.Command)`, `validate() error`, `json() bool`.
  - `func writeJSON(w io.Writer, msg proto.Message) error` — indented, one document.
  - `func writeJSONLine(w io.Writer, msg proto.Message) error` — compact, one line, for `calls tail`.
  - `func newTable(w io.Writer) *tabwriter.Writer`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/output_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func TestOutputFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		format string
		wantOK bool
	}{
		{"text", true},
		{"json", true},
		{"yaml", false},
		{"JSON", false},
		{"", false},
	} {
		t.Run(tc.format, func(t *testing.T) {
			f := &outputFlag{format: tc.format}
			err := f.validate()
			if tc.wantOK && err != nil {
				t.Fatalf("validate(%q) = %v, want nil", tc.format, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("validate(%q) = nil, want an error", tc.format)
			}
		})
	}
}

// -o must not be bound to --output: it is stub export's --out (design §4).
func TestOutputFlagHasNoShorthand(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	(&outputFlag{}).register(cmd)
	flag := cmd.Flags().Lookup("output")
	if flag == nil {
		t.Fatal("--output was not registered")
	}
	if flag.Shorthand != "" {
		t.Fatalf("--output has shorthand %q; -o belongs to stub export's --out", flag.Shorthand)
	}
	if flag.DefValue != "text" {
		t.Fatalf("--output default = %q, want text", flag.DefValue)
	}
}

// Scalar defaults are emitted so a script never reads null for a false, and
// field names are proto names, matching matcher paths and the stub grammar.
func TestJSONRendersDefaultsAndProtoNames(t *testing.T) {
	var buf bytes.Buffer
	resp := &adminv1.VerifyCallsResponse{Passed: false, Matched: 0}
	if err := writeJSON(&buf, resp); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	fields := decodeJSON(t, buf.String())
	passed, ok := fields["passed"]
	if !ok {
		t.Fatalf("passed is absent from %s; a script would read null", buf.String())
	}
	if passed != false {
		t.Fatalf("passed = %v, want false", passed)
	}
	if _, ok := fields["matched"]; !ok {
		t.Fatalf("matched is absent from %s", buf.String())
	}
}

func TestJSONUsesProtoFieldNamesAndEnumNames(t *testing.T) {
	var buf bytes.Buffer
	resp := &adminv1.ListStubsResponse{Stubs: []*adminv1.Stub{{
		Id:     "api-1",
		Method: "/shop.v1.OrderService/GetOrder",
		Shape:  adminv1.StubShape_STUB_SHAPE_UNARY,
		Origin: adminv1.StubOrigin_STUB_ORIGIN_API,
	}}}
	if err := writeJSON(&buf, resp); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	text := buf.String()
	if !strings.Contains(text, `"STUB_SHAPE_UNARY"`) {
		t.Errorf("enums are not rendered by name: %s", text)
	}
	// matched_stub_id would be matchedStubId under camelCase; assert a
	// snake_case key is present on this message instead.
	fields := decodeJSON(t, text)
	stubs, _ := fields["stubs"].([]any)
	if len(stubs) != 1 {
		t.Fatalf("stubs = %v, want one entry", fields["stubs"])
	}
	first, _ := stubs[0].(map[string]any)
	if _, ok := first["id"]; !ok {
		t.Errorf("id is absent: %s", text)
	}
}

// A stream has no enclosing document, so tail emits one compact object per
// line (design §4).
func TestWriteJSONLineIsSingleLineAndNewlineTerminated(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 2; i++ {
		msg := &adminv1.WatchCallsResponse{Call: &adminv1.Call{Seq: uint64(i + 1), Method: "/shop.v1.OrderService/GetOrder"}}
		if err := writeJSONLine(&buf, msg); err != nil {
			t.Fatalf("writeJSONLine: %v", err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		fields := decodeJSON(t, line)
		call, _ := fields["call"].(map[string]any)
		if call == nil {
			t.Fatalf("line %d has no call: %q", i, line)
		}
		// int64/uint64 render as JSON strings; that is protojson's rule and
		// scripts need `| tonumber`.
		if _, ok := call["seq"].(string); !ok {
			t.Errorf("line %d seq = %#v, want a JSON string", i, call["seq"])
		}
	}
}

func TestNewTableAlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	w := newTable(&buf)
	if _, err := w.Write([]byte("ID\tMETHOD\nlong-id-value\t/a/B\nx\t/c/D\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if strings.Index(lines[1], "/a/B") != strings.Index(lines[2], "/c/D") {
		t.Errorf("columns are not aligned:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestOutputFlag|TestJSON|TestWriteJSONLine|TestNewTable' -count=1
```

Expected: FAIL — `undefined: outputFlag`, `undefined: writeJSON`, `undefined: writeJSONLine`, `undefined: newTable`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/output.go`:

```go
package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	outputText = "text"
	outputJSON = "json"
)

// outputFlag is the format flag read commands carry. It has no shorthand: -o
// is stub export's --out, and binding it here would collide on the one command
// that takes both.
type outputFlag struct {
	format string
}

func (f *outputFlag) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.format, "output", outputText,
		`output format: "text" or "json"`)
}

func (f *outputFlag) validate() error {
	switch f.format {
	case outputText, outputJSON:
		return nil
	}
	return fmt.Errorf("--output must be %q or %q (got %q)", outputText, outputJSON, f.format)
}

func (f *outputFlag) json() bool { return f.format == outputJSON }

// jsonOptions renders a response as the frozen contract shape.
//
// UseProtoNames matches the names matcher paths and the stub grammar use.
// EmitDefaultValues keeps scripts total: without it a false `passed` is absent
// and `jq .passed` reads null. EmitUnpopulated is deliberately not used — it
// would also emit null for unset message fields.
//
// Note for tests: protojson output is not byte-stable. Decode it and assert on
// fields; never compare it as a string.
var jsonOptions = protojson.MarshalOptions{
	UseProtoNames:     true,
	EmitDefaultValues: true,
	Multiline:         true,
	Indent:            "  ",
}

// streamJSONOptions is jsonOptions for `calls tail`: a stream has no enclosing
// document, so each call is one compact object on its own line.
var streamJSONOptions = protojson.MarshalOptions{
	UseProtoNames:     true,
	EmitDefaultValues: true,
}

func writeJSON(w io.Writer, msg proto.Message) error {
	raw, err := jsonOptions.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}

func writeJSONLine(w io.Writer, msg proto.Message) error {
	raw, err := streamJSONOptions.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}

// newTable returns a tabwriter over w. Callers write tab-separated rows and
// Flush.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check the defaults rule**

Set `EmitDefaultValues: false` in `jsonOptions`. Confirm `TestJSONRendersDefaultsAndProtoNames` FAILS with `passed is absent`. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/output.go internal/cli/output_test.go
git commit -m "feat(cli): add the --output text|json contract

JSON is the response message in the frozen contract shape: proto field
names, enums by name, and scalar defaults emitted so a script never reads
null for a false. calls tail gets a compact one-object-per-line variant,
since a stream has no enclosing document.

--output takes no shorthand; -o belongs to stub export's --out."
```

---

### Task 5: `schema register|list`

The first real command group, proving Tasks 2–4 compose. `register`'s merge is the one piece of genuine client-side logic: registering a descriptor set that names a path twice fails outright, and duplicates are the **common** case, because any two sets built with `--include_imports` embed the well-known types they use (design §6.4).

**Files:**
- Create: `internal/cli/schema.go`, `internal/cli/schema_test.go`

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:
  - `func newSchemaCmd() *cobra.Command` and `func newSchemaCmdWithClient(newClient clientFactory) *cobra.Command`.
  - `func mergeDescriptorSets(paths []string) (*descriptorpb.FileDescriptorSet, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/schema_test.go`:

```go
package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/yinghanhung/simulacra/internal/schema"
)

// testdataSet returns testdata/protos as a self-contained FileDescriptorSet.
func testdataSet(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	reg.Snapshot().RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
		return true
	})
	return set
}

// writeSet marshals set to a temp file and returns its path.
func writeSet(t *testing.T, set *descriptorpb.FileDescriptorSet) string {
	t.Helper()
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "set.binpb")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// Two sets built independently both embed the well-known types they import,
// so merging must dedupe by path — the server rejects a set naming a path
// twice (design §6.4).
func TestMergeDescriptorSetsDedupesSharedImports(t *testing.T) {
	full := testdataSet(t)
	if len(full.GetFile()) < 3 {
		t.Fatalf("testdata set has %d files, expected at least 3", len(full.GetFile()))
	}
	a := writeSet(t, full)
	b := writeSet(t, full) // the same files again, as a second --include_imports build would

	merged, err := mergeDescriptorSets([]string{a, b})
	if err != nil {
		t.Fatalf("mergeDescriptorSets: %v", err)
	}
	seen := map[string]int{}
	for _, f := range merged.GetFile() {
		seen[f.GetName()]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times in the merged set, want 1", name, count)
		}
	}
	if len(merged.GetFile()) != len(full.GetFile()) {
		t.Fatalf("merged %d files, want %d", len(merged.GetFile()), len(full.GetFile()))
	}
}

// Silently keeping one of two conflicting definitions would register a schema
// the user never described.
func TestMergeDescriptorSetsRejectsConflictingDefinitions(t *testing.T) {
	full := testdataSet(t)
	a := writeSet(t, full)

	conflicting := proto.Clone(full).(*descriptorpb.FileDescriptorSet)
	target := conflicting.GetFile()[0]
	target.Options = &descriptorpb.FileOptions{GoPackage: proto.String("example.com/divergent")}
	b := writeSet(t, conflicting)

	_, err := mergeDescriptorSets([]string{a, b})
	if err == nil {
		t.Fatal("merged two conflicting definitions of the same path")
	}
	if !strings.Contains(err.Error(), target.GetName()) {
		t.Errorf("error %v does not name the conflicting file %s", err, target.GetName())
	}
	if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("error %v does not name both source files", err)
	}
}

func TestMergeDescriptorSetsReportsUnreadableFile(t *testing.T) {
	_, err := mergeDescriptorSets([]string{filepath.Join(t.TempDir(), "absent.binpb")})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestSchemaRegisterSendsOneAllOrNothingCall(t *testing.T) {
	srv := startCommandServer(t)
	full := testdataSet(t)
	half := len(full.GetFile()) / 2
	a := writeSet(t, &descriptorpb.FileDescriptorSet{File: full.GetFile()[:half]})
	b := writeSet(t, &descriptorpb.FileDescriptorSet{File: full.GetFile()[half:]})

	cmd := newSchemaCmd()
	stdout, _, err := runCmd(t, cmd, "register", "--addr", adminAddr(srv), "-f", a, "-f", b)
	if err != nil {
		t.Fatalf("schema register: %v", err)
	}
	if !strings.Contains(stdout, "service(s)") {
		t.Fatalf("stdout = %q, want a service count", stdout)
	}
}

// A set that is not self-contained fails server-side; the diagnostic reaches
// the user unchanged (M3 §11).
func TestSchemaRegisterSurfacesTheServerDiagnosticVerbatim(t *testing.T) {
	srv := startCommandServer(t)
	full := testdataSet(t)
	var orderOnly *descriptorpb.FileDescriptorProto
	for _, f := range full.GetFile() {
		if strings.HasSuffix(f.GetName(), "order.proto") {
			orderOnly = f
		}
	}
	if orderOnly == nil {
		t.Fatal("order.proto not found in the testdata set")
	}
	path := writeSet(t, &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{orderOnly}})

	cmd := newSchemaCmd()
	_, _, err := runCmd(t, cmd, "register", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("registering a set with unresolved imports succeeded")
	}
	if !strings.Contains(err.Error(), "self-contained") {
		t.Fatalf("error = %v, want the server's self-contained diagnostic", err)
	}
}

func TestSchemaRegisterRequiresAFile(t *testing.T) {
	cmd := newSchemaCmd()
	_, _, err := runCmd(t, cmd, "register", "--addr", "127.0.0.1:1")
	if err == nil {
		t.Fatal("schema register ran with no --file")
	}
	if !strings.Contains(err.Error(), "--file") {
		t.Fatalf("error = %v, want it to name --file", err)
	}
}

func TestSchemaListText(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newSchemaCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("schema list: %v", err)
	}
	if !strings.Contains(stdout, "shop.v1.OrderService") {
		t.Fatalf("stdout = %q, want the service name", stdout)
	}
	if !strings.Contains(stdout, "GetOrder") {
		t.Fatalf("stdout = %q, want the method names", stdout)
	}
	// WatchOrder is server-streaming; the marker must show on the right side.
	if !strings.Contains(stdout, "WatchOrder(") || !strings.Contains(stdout, "returns (stream ") {
		t.Fatalf("stdout = %q, want a stream marker on WatchOrder's response", stdout)
	}
	// UploadOrders is client-streaming; the marker must show on the left.
	if !strings.Contains(stdout, "UploadOrders(stream ") {
		t.Fatalf("stdout = %q, want a stream marker on UploadOrders' request", stdout)
	}
}

func TestSchemaListJSON(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newSchemaCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("schema list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	services, _ := fields["services"].([]any)
	if len(services) == 0 {
		t.Fatalf("services is empty in %s", stdout)
	}
	first, _ := services[0].(map[string]any)
	if _, ok := first["methods"]; !ok {
		t.Fatalf("methods absent: %s", stdout)
	}
	// Proto names, not camelCase: client_streaming, not clientStreaming.
	methods, _ := first["methods"].([]any)
	m0, _ := methods[0].(map[string]any)
	if _, ok := m0["client_streaming"]; !ok {
		t.Fatalf("client_streaming absent (camelCase leaked?): %s", stdout)
	}
}

func TestSchemaListRejectsAnInvalidOutputFormat(t *testing.T) {
	cmd := newSchemaCmd()
	_, _, err := runCmd(t, cmd, "list", "--addr", "127.0.0.1:1", "--output", "yaml")
	if err == nil {
		t.Fatal("--output yaml was accepted")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestMergeDescriptorSets|TestSchema' -count=1
```

Expected: FAIL — `undefined: mergeDescriptorSets`, `undefined: newSchemaCmd`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/schema.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newSchemaCmd() *cobra.Command { return newSchemaCmdWithClient(newAdminClient) }

func newSchemaCmdWithClient(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Inspect and register the descriptors a running server serves",
	}
	cmd.AddCommand(newSchemaRegisterCmd(newClient), newSchemaListCmd(newClient))
	return cmd
}

func newSchemaRegisterCmd(newClient clientFactory) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:         "register",
		Short:       "Register descriptor sets with a running server",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) == 0 {
				return errors.New("--file <descriptor-set> is required")
			}
			merged, err := mergeDescriptorSets(files)
			if err != nil {
				return err
			}
			raw, err := proto.Marshal(merged)
			if err != nil {
				return fmt.Errorf("encoding the merged descriptor set: %w", err)
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Schema.RegisterSchemas(callCtx,
				connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: raw}))
			if err != nil {
				return rpcError(ctx, err)
			}
			// Zero newly-registered files is a success: re-registering an
			// unchanged set is a no-op, not a failure.
			cmd.Printf("registered %d new file(s); %d service(s) available\n",
				len(resp.Msg.GetRegisteredFiles()), resp.Msg.GetServiceCount())
			return nil
		},
	}
	clientFlags{}.register(cmd)
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil,
		"serialized FileDescriptorSet to register, e.g. a buf image (repeatable)")
	return cmd
}

func newSchemaListCmd(newClient clientFactory) *cobra.Command {
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List the services a running server is serving",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Schema.ListServices(callCtx,
				connect.NewRequest(&adminv1.ListServicesRequest{}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				return writeJSON(cmd.OutOrStdout(), resp.Msg)
			}
			services := resp.Msg.GetServices()
			if len(services) == 0 {
				cmd.PrintErrln("no services registered")
				return nil
			}
			for _, svc := range services {
				cmd.Printf("%s (%s)\n", svc.GetName(), svc.GetFile())
				for _, m := range svc.GetMethods() {
					cmd.Printf("  %s(%s%s) returns (%s%s)\n",
						m.GetName(),
						streamMarker(m.GetClientStreaming()), m.GetInputType(),
						streamMarker(m.GetServerStreaming()), m.GetOutputType())
				}
			}
			return nil
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	return cmd
}

func streamMarker(streaming bool) string {
	if streaming {
		return "stream "
	}
	return ""
}

// mergedFile records where a file path was first seen, so a conflict can name
// both sources.
type mergedFile struct {
	path string
	file *descriptorpb.FileDescriptorProto
}

// mergeDescriptorSets reads each path as a FileDescriptorSet and merges them
// into one, keeping the first occurrence of each file path.
//
// One merged call, because RegisterSchemas is all-or-nothing and sequential
// per-file calls would forfeit that.
//
// Deduping is not an optimization. Registering a set that names a path twice
// fails outright ("file appears multiple times"), and duplicates are the
// common case: any two sets built with --include_imports or `buf build -o`
// both embed the well-known types they use. A later file carrying the same
// path with different contents is an error rather than a silent pick, because
// keeping one of two conflicting definitions would register a schema the user
// never described.
func mergeDescriptorSets(paths []string) (*descriptorpb.FileDescriptorSet, error) {
	merged := &descriptorpb.FileDescriptorSet{}
	seen := map[string]mergedFile{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var set descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(raw, &set); err != nil {
			return nil, fmt.Errorf("parsing %s as a FileDescriptorSet: %w", path, err)
		}
		for _, file := range set.GetFile() {
			name := file.GetName()
			if prior, ok := seen[name]; ok {
				if !proto.Equal(prior.file, file) {
					return nil, fmt.Errorf(
						"%s and %s both define %s with different contents; "+
							"register them separately, or rebuild both from one source",
						prior.path, path, name)
				}
				continue
			}
			seen[name] = mergedFile{path: path, file: file}
			merged.File = append(merged.File, file)
		}
	}
	return merged, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check the dedupe**

Delete the `if prior, ok := seen[name]; ok` block so every file is appended. Confirm `TestMergeDescriptorSetsDedupesSharedImports` FAILS. Revert.

Then change the conflict branch from `return nil, fmt.Errorf(...)` to `continue` (first wins silently). Confirm `TestMergeDescriptorSetsRejectsConflictingDefinitions` FAILS. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/schema.go internal/cli/schema_test.go
git commit -m "feat(cli): add schema register and schema list

register merges every --file into one FileDescriptorSet and sends a single
RegisterSchemas, preserving the RPC's all-or-nothing guarantee that
sequential calls would forfeit. Merging dedupes by path because the server
rejects a set naming a path twice and duplicates are the common case: any
two sets built with --include_imports embed the well-known types they use.
A path defined twice with different contents is an error naming both
files, not a silent pick."
```

---

### Task 6: `stub list` and `stub rm`

`stub list` renders M3 §7's five columns. `stub rm` deletes by id, fail fast and **without rollback**: a deleted stub cannot be recreated with its ID, so there is no state to roll back to — the asymmetry with `stub add` is necessity, not oversight (design §6.1).

**Files:**
- Create: `internal/cli/stub.go`, `internal/cli/stub_test.go`

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:
  - `func newStubCmd() *cobra.Command`, `func newStubCmdWithClient(newClient clientFactory) *cobra.Command`.
  - `func parseOrigin(text string) (adminv1.StubOrigin, error)`.
  - `func originName(o adminv1.StubOrigin) string`, `func timesText(times int32) string`.
  - Test helper `createStub(t, srv, document string) string` returning the new stub's id.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/stub_test.go`:

```go
package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/server"
)

// getOrderStub is one valid unary stub document in the file grammar.
func getOrderStub(note string) string {
	return fmt.Sprintf("method: shop.v1.OrderService/GetOrder\nrespond:\n  message: { note: %s }\n", note)
}

// stubClient is a direct admin client for arranging and inspecting state,
// independent of the command under test.
func stubClient(t *testing.T, srv *server.Server) adminv1connect.StubServiceClient {
	t.Helper()
	return adminv1connect.NewStubServiceClient(httpClientForTest(t), "http://"+adminAddr(srv))
}

// createStub installs one API-origin stub and returns its id.
func createStub(t *testing.T, srv *server.Server, document string) string {
	t.Helper()
	resp, err := stubClient(t, srv).CreateStub(context.Background(),
		connect.NewRequest(&adminv1.CreateStubRequest{Document: document}))
	if err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	return resp.Msg.GetStub().GetId()
}

// listStubIDs returns the ids currently in the store, in List order.
func listStubIDs(t *testing.T, srv *server.Server) []string {
	t.Helper()
	resp, err := stubClient(t, srv).ListStubs(context.Background(),
		connect.NewRequest(&adminv1.ListStubsRequest{}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	var ids []string
	for _, s := range resp.Msg.GetStubs() {
		ids = append(ids, s.GetId())
	}
	return ids
}

func TestStubListText(t *testing.T) {
	srv := startCommandServer(t)
	id := createStub(t, srv, getOrderStub("first"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	for _, want := range []string{"ID", "METHOD", "ORIGIN", "HITS", "TIMES", id,
		"/shop.v1.OrderService/GetOrder", "api"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	// times: 0 means unlimited in the grammar, and the table says so rather
	// than printing a bare 0 the reader must decode.
	if !strings.Contains(stdout, "unlimited") {
		t.Errorf("stdout does not render times 0 as unlimited:\n%s", stdout)
	}
}

func TestStubListRendersAFiniteTimesAsANumber(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, "method: shop.v1.OrderService/GetOrder\ntimes: 3\nrespond:\n  message: { note: n }\n")

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	if strings.Contains(stdout, "unlimited") {
		t.Errorf("a times of 3 rendered as unlimited:\n%s", stdout)
	}
	if !strings.Contains(stdout, "3") {
		t.Errorf("stdout does not show times 3:\n%s", stdout)
	}
}

func TestStubListEmptyReportsToStderr(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	if !strings.Contains(stderr, "no stubs") {
		t.Errorf("stderr = %q, want a no-stubs note", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want it empty so a pipe sees nothing", stdout)
	}
}

func TestStubListMethodFilter(t *testing.T) {
	srv := startCommandServer(t)
	kept := createStub(t, srv, getOrderStub("kept"))
	other := createStub(t, srv,
		"method: shop.v1.OrderService/WatchOrder\nrespond:\n  stream: [{ message: { note: other } }]\n")

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	if !strings.Contains(stdout, kept) {
		t.Errorf("stdout is missing the matching stub %s:\n%s", kept, stdout)
	}
	if strings.Contains(stdout, other) {
		t.Errorf("stdout includes the filtered-out stub %s:\n%s", other, stdout)
	}
}

func TestStubListOriginFilter(t *testing.T) {
	srv := startCommandServer(t)
	api := createStub(t, srv, getOrderStub("api"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--origin", "file")
	if err != nil {
		t.Fatalf("stub list --origin file: %v", err)
	}
	if strings.Contains(stdout, api) {
		t.Errorf("an api-origin stub survived --origin file:\n%s", stdout)
	}

	cmd = newStubCmd()
	stdout, _, err = runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--origin", "api")
	if err != nil {
		t.Fatalf("stub list --origin api: %v", err)
	}
	if !strings.Contains(stdout, api) {
		t.Errorf("stdout is missing the api-origin stub:\n%s", stdout)
	}
}

func TestStubListRejectsAnUnknownOrigin(t *testing.T) {
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "list", "--addr", "127.0.0.1:1", "--origin", "both")
	if err == nil {
		t.Fatal("--origin both was accepted")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Fatalf("error = %v, want it to name --origin", err)
	}
}

func TestStubListJSON(t *testing.T) {
	srv := startCommandServer(t)
	id := createStub(t, srv, getOrderStub("json"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("stub list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	stubs, _ := fields["stubs"].([]any)
	if len(stubs) != 1 {
		t.Fatalf("stubs = %v, want one entry", fields["stubs"])
	}
	first, _ := stubs[0].(map[string]any)
	if first["id"] != id {
		t.Errorf("id = %v, want %s", first["id"], id)
	}
	if first["origin"] != "STUB_ORIGIN_API" {
		t.Errorf("origin = %v, want the enum name", first["origin"])
	}
}

func TestStubRemove(t *testing.T) {
	srv := startCommandServer(t)
	first := createStub(t, srv, getOrderStub("first"))
	second := createStub(t, srv, getOrderStub("second"))

	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "rm", "--addr", adminAddr(srv), first)
	if err != nil {
		t.Fatalf("stub rm: %v", err)
	}
	ids := listStubIDs(t, srv)
	if len(ids) != 1 || ids[0] != second {
		t.Fatalf("remaining ids = %v, want only %s", ids, second)
	}
}

func TestStubRemoveRequiresAnID(t *testing.T) {
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "rm", "--addr", "127.0.0.1:1")
	if err == nil {
		t.Fatal("stub rm ran with no id")
	}
}

// Deletes are not rolled back: a removed stub cannot be recreated with its ID.
// The command reports what it did remove before failing (design §6.1).
func TestStubRemoveFailsFastAndReportsWhatItRemoved(t *testing.T) {
	srv := startCommandServer(t)
	first := createStub(t, srv, getOrderStub("first"))
	survivor := createStub(t, srv, getOrderStub("survivor"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "rm", "--addr", adminAddr(srv), first, "api-does-not-exist", survivor)
	if err == nil {
		t.Fatal("stub rm succeeded with an unknown id")
	}
	if !strings.Contains(stdout, first) {
		t.Errorf("stdout = %q, want it to name the stub that was removed before the failure", stdout)
	}
	// The third id was never attempted: fail fast.
	ids := listStubIDs(t, srv)
	if len(ids) != 1 || ids[0] != survivor {
		t.Fatalf("remaining ids = %v, want only %s — rm must stop at the first failure", ids, survivor)
	}
}
```

Add this helper to `internal/cli/harness_test.go`:

```go
// httpClientForTest is the HTTP/1.1 client the test-side admin clients use.
func httpClientForTest(t *testing.T) *http.Client {
	t.Helper()
	client := &http.Client{}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
```

and add `"net/http"` to that file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestStubList|TestStubRemove' -count=1
```

Expected: FAIL — `undefined: newStubCmd`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/stub.go`:

```go
package cli

import (
	"fmt"
	"strconv"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newStubCmd() *cobra.Command { return newStubCmdWithClient(newAdminClient) }

func newStubCmdWithClient(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stub",
		Short: "Inspect and manage the stubs a running server is serving",
	}
	cmd.AddCommand(
		newStubListCmd(newClient),
		newStubRemoveCmd(newClient),
	)
	return cmd
}

func newStubListCmd(newClient clientFactory) *cobra.Command {
	var method, origin string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List stubs",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			originEnum, err := parseOrigin(origin)
			if err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Stub.ListStubs(callCtx, connect.NewRequest(&adminv1.ListStubsRequest{
				Method: method,
				Origin: originEnum,
			}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				return writeJSON(cmd.OutOrStdout(), resp.Msg)
			}
			stubs := resp.Msg.GetStubs()
			if len(stubs) == 0 {
				cmd.PrintErrln("no stubs")
				return nil
			}
			table := newTable(cmd.OutOrStdout())
			fmt.Fprintln(table, "ID\tMETHOD\tORIGIN\tHITS\tTIMES")
			for _, s := range stubs {
				fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\n",
					s.GetId(), s.GetMethod(), originName(s.GetOrigin()),
					s.GetHits(), timesText(s.GetTimes()))
			}
			return table.Flush()
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "only stubs for this method")
	cmd.Flags().StringVar(&origin, "origin", "", `only stubs of this origin: "file" or "api"`)
	return cmd
}

func newStubRemoveCmd(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "rm <id>...",
		Short:       "Remove stubs by id",
		Args:        cobra.MinimumNArgs(1),
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()

			// Fail fast, and do not roll back: a removed stub cannot be
			// recreated with its ID, so there is no prior state to restore.
			// What the command owes the user is an accurate account of what it
			// did before it stopped.
			for _, id := range args {
				callCtx, cancel := client.callContext(ctx)
				_, err := client.Stub.DeleteStub(callCtx,
					connect.NewRequest(&adminv1.DeleteStubRequest{Id: id}))
				cancel()
				if err != nil {
					return rpcError(ctx, fmt.Errorf("%s: %w", id, err))
				}
				cmd.Printf("removed %s\n", id)
			}
			return nil
		},
	}
	clientFlags{}.register(cmd)
	return cmd
}

// parseOrigin maps the --origin flag onto the filter enum. An empty flag means
// every origin.
func parseOrigin(text string) (adminv1.StubOrigin, error) {
	switch text {
	case "":
		return adminv1.StubOrigin_STUB_ORIGIN_UNSPECIFIED, nil
	case "file":
		return adminv1.StubOrigin_STUB_ORIGIN_FILE, nil
	case "api":
		return adminv1.StubOrigin_STUB_ORIGIN_API, nil
	}
	return adminv1.StubOrigin_STUB_ORIGIN_UNSPECIFIED,
		fmt.Errorf(`--origin must be "file" or "api" (got %q)`, text)
}

func originName(o adminv1.StubOrigin) string {
	switch o {
	case adminv1.StubOrigin_STUB_ORIGIN_FILE:
		return "file"
	case adminv1.StubOrigin_STUB_ORIGIN_API:
		return "api"
	default:
		return "unknown"
	}
}

// timesText renders a use budget. Zero means unlimited in the file grammar,
// and the table says so rather than printing a bare 0 the reader must decode.
func timesText(times int32) string {
	if times == 0 {
		return "unlimited"
	}
	return strconv.Itoa(int(times))
}
```

`cobra.MinimumNArgs(1)` covers the empty-args case, so `rm` needs no hand-rolled check.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check the fail-fast guarantee**

In `newStubRemoveCmd`, change the error branch from `return …` to `cmd.PrintErrln(err); continue`. Confirm `TestStubRemoveFailsFastAndReportsWhatItRemoved` FAILS — with `continue`, the third id is deleted too, so `survivor` disappears. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/stub.go internal/cli/stub_test.go internal/cli/harness_test.go
git commit -m "feat(cli): add stub list and stub rm

list renders the five-column envelope table, showing a times of 0 as
unlimited rather than a bare 0 the reader must decode. rm deletes by id,
failing fast and reporting what it removed before stopping; it does not
roll back, because a removed stub cannot be recreated with its ID."
```

---

### Task 7: `stub export`

`--output` chooses **what** is written, `--out` chooses **where**. The two are orthogonal and every combination is defined; none is rejected (design §6.1).

**Files:**
- Modify: `internal/cli/stub.go` (add `newStubExportCmd`, register it)
- Modify: `internal/cli/stub_test.go`

**Interfaces:**
- Consumes: Tasks 2–4, Task 6's `newStubCmdWithClient`.
- Produces: `func newStubExportCmd(newClient clientFactory) *cobra.Command`, and `func writeOut(cmd *cobra.Command, path string, render func(io.Writer) error) error`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/stub_test.go`:

```go
func TestStubExportToStdout(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("exported"))

	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub export: %v", err)
	}
	if !strings.Contains(stdout, "note: exported") {
		t.Fatalf("stdout = %q, want the stub document", stdout)
	}
	// The count is commentary: it must not pollute a piped document.
	if !strings.Contains(stderr, "1 stub") {
		t.Errorf("stderr = %q, want the stub count", stderr)
	}
	if strings.Contains(stdout, "stub(s)") {
		t.Errorf("the count leaked into stdout: %q", stdout)
	}
}

func TestStubExportToFile(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("to-file"))
	path := filepath.Join(t.TempDir(), "stubs.yaml")

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv), "--out", path)
	if err != nil {
		t.Fatalf("stub export --out: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want it empty when writing to a file", stdout)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the exported file: %v", err)
	}
	if !strings.Contains(string(raw), "note: to-file") {
		t.Fatalf("file = %q, want the stub document", raw)
	}
}

// -o is --out's shorthand, not --output's (design §4).
func TestStubExportShorthandIsOut(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("shorthand"))
	path := filepath.Join(t.TempDir(), "stubs.yaml")

	cmd := newStubCmd()
	if _, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv), "-o", path); err != nil {
		t.Fatalf("stub export -o: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the exported file: %v", err)
	}
	if !strings.Contains(string(raw), "note: shorthand") {
		t.Fatalf("-o did not write the document: %q", raw)
	}
}

// Format and destination compose: all four combinations are defined.
func TestStubExportFormatAndDestinationCompose(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("composed"))

	t.Run("json to stdout", func(t *testing.T) {
		cmd := newStubCmd()
		stdout, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv), "--output", "json")
		if err != nil {
			t.Fatalf("export --output json: %v", err)
		}
		fields := decodeJSON(t, stdout)
		if _, ok := fields["document"]; !ok {
			t.Fatalf("document absent: %s", stdout)
		}
		if fields["stub_count"] != "1" && fields["stub_count"] != float64(1) {
			t.Fatalf("stub_count = %#v, want 1", fields["stub_count"])
		}
	})

	t.Run("json to a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stubs.json")
		cmd := newStubCmd()
		if _, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv),
			"--out", path, "--output", "json"); err != nil {
			t.Fatalf("export --out --output json: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		fields := decodeJSON(t, string(raw))
		if _, ok := fields["document"]; !ok {
			t.Fatalf("document absent from the file: %s", raw)
		}
	})
}

func TestStubExportOfAnEmptyStoreSucceeds(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("exporting an empty store: %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("stdout = %q, want the empty sequence", stdout)
	}
	if !strings.Contains(stderr, "0 stub") {
		t.Errorf("stderr = %q, want a zero count", stderr)
	}
}

func TestStubExportReportsAnUnwritablePath(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv),
		"--out", filepath.Join(t.TempDir(), "no-such-dir", "stubs.yaml"))
	if err == nil {
		t.Fatal("export to an unwritable path succeeded")
	}
}
```

Add `"os"`, `"path/filepath"`, and `"io"` (if not already present) to `internal/cli/stub_test.go`'s imports.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestStubExport' -count=1
```

Expected: FAIL — `unknown command "export" for "stub"`.

- [ ] **Step 3: Write the implementation**

Add to `internal/cli/stub.go`, and register it in `newStubCmdWithClient`:

```go
	cmd.AddCommand(
		newStubListCmd(newClient),
		newStubRemoveCmd(newClient),
		newStubExportCmd(newClient),
	)
```

```go
func newStubExportCmd(newClient clientFactory) *cobra.Command {
	var outPath string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export API-origin stubs as a stub file",
		Long: "Export API-origin stubs as one file-grammar YAML sequence.\n\n" +
			"--output chooses what is written, --out chooses where it goes; the two compose.",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Stub.ExportStubs(callCtx,
				connect.NewRequest(&adminv1.ExportStubsRequest{}))
			if err != nil {
				return rpcError(ctx, err)
			}
			render := func(w io.Writer) error {
				if out.json() {
					return writeJSON(w, resp.Msg)
				}
				_, err := io.WriteString(w, resp.Msg.GetDocument())
				return err
			}
			if err := writeOut(cmd, outPath, render); err != nil {
				return err
			}
			// Commentary goes to stderr so a piped or redirected document
			// carries only the payload.
			cmd.PrintErrf("%d stub(s)\n", resp.Msg.GetStubCount())
			return nil
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVarP(&outPath, "out", "o", "",
		"write to this file instead of stdout")
	return cmd
}

// writeOut sends render's output to path, or to the command's stdout when path
// is empty. The file is created with 0o644 so an exported stub file is
// readable by the tooling that will load it back.
func writeOut(cmd *cobra.Command, path string, render func(io.Writer) error) error {
	if path == "" {
		return render(cmd.OutOrStdout())
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := render(file); err != nil {
		file.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}
```

Add `"io"` and `"os"` to `internal/cli/stub.go`'s imports.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check the stream split**

Change the count line from `cmd.PrintErrf` to `cmd.Printf`. Confirm `TestStubExportToStdout` FAILS on `the count leaked into stdout`. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/stub.go internal/cli/stub_test.go
git commit -m "feat(cli): add stub export

--output chooses what is written and --out chooses where, so the two
compose rather than conflict and no combination is rejected. -o is --out's
shorthand; --output keeps none. The stub count goes to stderr so a piped
document carries only the payload."
```

---

### Task 8: `stub add`

The largest task. `stub add` is **preflight-then-compensate**, and says so rather than claiming an atomicity it cannot deliver (design §6.1):

- **Preflight** reads and splits every file before the first RPC, so a malformed input file sends nothing at all.
- **The RPC phase compensates**: on the first rejection it deletes what it created, in reverse order, under its **own bounded context** — compensation must still run when the command context is already cancelled, which is exactly what a lost or timed-out create produces.
- `CreateStub` mutates the store before it returns, so a lost response orphans a stub the client cannot name. The command reports that rather than claiming a clean rollback.

**Files:**
- Modify: `internal/cli/stub.go`, `internal/cli/stub_test.go`

**Interfaces:**
- Consumes: Task 1's `stub.SplitDocuments`; Tasks 2–4; Task 6's `newStubCmdWithClient`.
- Produces:
  - `func newStubAddCmd(newClient clientFactory) *cobra.Command`.
  - `type pendingStub struct{ source string; document string }`.
  - `func preflightStubs(cmd *cobra.Command, paths []string) ([]pendingStub, error)`.
  - `const rollbackTimeout = 10 * time.Second`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/stub_test.go`:

```go
// writeStubFile writes a stub file holding the given documents as a list.
func writeStubFile(t *testing.T, name string, items ...string) string {
	t.Helper()
	var b strings.Builder
	for _, item := range items {
		b.WriteString("- ")
		b.WriteString(strings.ReplaceAll(strings.TrimSuffix(item, "\n"), "\n", "\n  "))
		b.WriteString("\n")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestStubAddCreatesEveryStubInFileOrder(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml", getOrderStub("one"), getOrderStub("two"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err != nil {
		t.Fatalf("stub add: %v", err)
	}
	if ids := listStubIDs(t, srv); len(ids) != 2 {
		t.Fatalf("created %d stubs, want 2: %v", len(ids), ids)
	}
	if !strings.Contains(stdout, "api-1") || !strings.Contains(stdout, "api-2") {
		t.Errorf("stdout = %q, want both created ids", stdout)
	}
}

func TestStubAddReadsStdin(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	cmd.SetIn(strings.NewReader("- " + strings.ReplaceAll(getOrderStub("stdin"), "\n", "\n  ")))
	if _, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", "-"); err != nil {
		t.Fatalf("stub add -f -: %v", err)
	}
	if ids := listStubIDs(t, srv); len(ids) != 1 {
		t.Fatalf("created %d stubs, want 1", len(ids))
	}
}

// Preflight: a malformed file sends ZERO RPCs. Asserting "no stubs created"
// alone would also hold if the server had rejected every document, so the
// test counts calls (design §8).
func TestStubAddPreflightSendsNoRPCsForAMalformedFile(t *testing.T) {
	srv := startCommandServer(t)
	good := writeStubFile(t, "good.yaml", getOrderStub("good"))
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("method: not-a-list\n"), 0o600); err != nil {
		t.Fatalf("write bad.yaml: %v", err)
	}

	counter := &countingStubClient{}
	cmd := newStubCmdWithClient(countingFactory(t, srv, counter))
	_, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", good, "-f", bad)
	if err == nil {
		t.Fatal("stub add accepted a malformed file")
	}
	if counter.creates != 0 {
		t.Fatalf("preflight sent %d CreateStub calls, want 0", counter.creates)
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("error = %v, want it to name the failing file", err)
	}
}

// A server-side rejection rolls back every prior create in this invocation.
func TestStubAddRollsBackOnRejection(t *testing.T) {
	srv := startCommandServer(t)
	preexisting := createStub(t, srv, getOrderStub("preexisting"))
	// The third document names a method that does not exist: only the server
	// can reject it, so preflight passes and the RPC phase must compensate.
	path := writeStubFile(t, "stubs.yaml",
		getOrderStub("one"),
		getOrderStub("two"),
		"method: shop.v1.OrderService/NoSuchMethod\nrespond:\n  message: {}\n")

	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add succeeded with an unknown method")
	}
	if !strings.Contains(stderr+stdout+err.Error(), "no stubs were added") {
		t.Errorf("output does not say no stubs were added:\nstdout=%q\nstderr=%q\nerr=%v", stdout, stderr, err)
	}
	if !strings.Contains(err.Error(), "stubs.yaml#2") {
		t.Errorf("error = %v, want it to name the failing document as <file>#<index>", err)
	}
	// The store is back to exactly what it held before the command ran.
	ids := listStubIDs(t, srv)
	if len(ids) != 1 || ids[0] != preexisting {
		t.Fatalf("ids = %v, want only the pre-existing %s — rollback did not run", ids, preexisting)
	}
}

// When a rollback delete itself fails, the command says the rollback was
// incomplete and names the orphan. It never claims a clean rollback it did not
// achieve. A healthy server cannot produce this, so it is injected (design §8).
func TestStubAddReportsAnIncompleteRollback(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml",
		getOrderStub("one"),
		"method: shop.v1.OrderService/NoSuchMethod\nrespond:\n  message: {}\n")

	failing := &failingDeleteStubClient{}
	cmd := newStubCmdWithClient(failingFactory(t, srv, failing))
	stdout, stderr, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add succeeded with an unknown method")
	}
	combined := stdout + stderr + err.Error()
	if !strings.Contains(combined, "incomplete") {
		t.Errorf("output does not report an incomplete rollback:\n%s", combined)
	}
	if !strings.Contains(combined, "api-1") {
		t.Errorf("output does not name the orphaned id:\n%s", combined)
	}
	if strings.Contains(combined, "no stubs were added") {
		t.Errorf("output claims a clean rollback it did not achieve:\n%s", combined)
	}
}

// Compensation must still run when the command context is already done, which
// is exactly the state a timed-out or interrupted create leaves behind. The
// rollback therefore uses its own context (design §6.1).
func TestStubAddRollsBackUnderACancelledCommandContext(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml",
		getOrderStub("one"),
		"method: shop.v1.OrderService/NoSuchMethod\nrespond:\n  message: {}\n")

	cancelling := &cancelAfterCreateStubClient{}
	cmd := newStubCmdWithClient(cancellingFactory(t, srv, cancelling))
	if _, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path); err == nil {
		t.Fatal("stub add succeeded with an unknown method")
	}
	if ids := listStubIDs(t, srv); len(ids) != 0 {
		t.Fatalf("ids = %v, want none — rollback did not run under a done context", ids)
	}
}

func TestStubAddRequiresAFile(t *testing.T) {
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "add", "--addr", "127.0.0.1:1")
	if err == nil {
		t.Fatal("stub add ran with no --file")
	}
	if !strings.Contains(err.Error(), "--file") {
		t.Fatalf("error = %v, want it to name --file", err)
	}
}

// export then add round-trips through the one splitter.
func TestStubExportThenAddRoundTrips(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("round-trip-a"))
	createStub(t, srv, getOrderStub("round-trip-b"))
	path := filepath.Join(t.TempDir(), "exported.yaml")

	if _, _, err := runCmd(t, newStubCmd(), "export", "--addr", adminAddr(srv), "--out", path); err != nil {
		t.Fatalf("stub export: %v", err)
	}
	target := startCommandServer(t)
	if _, _, err := runCmd(t, newStubCmd(), "add", "--addr", adminAddr(target), "-f", path); err != nil {
		t.Fatalf("stub add of the exported file: %v", err)
	}
	if ids := listStubIDs(t, target); len(ids) != 2 {
		t.Fatalf("round-tripped %d stubs, want 2: %v", len(ids), ids)
	}
}

func TestStubAddOfAnEmptyExportAddsNothing(t *testing.T) {
	srv := startCommandServer(t)
	path := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(path, []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := runCmd(t, newStubCmd(), "add", "--addr", adminAddr(srv), "-f", path); err != nil {
		t.Fatalf("stub add of an empty sequence: %v", err)
	}
	if ids := listStubIDs(t, srv); len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}
```

- [ ] **Step 2: Write the fault-injection decorators**

Append to `internal/cli/harness_test.go`. These wrap the real `StubServiceClient`, so every call except the injected one still reaches the real server.

```go
// countingStubClient counts CreateStub calls, so a preflight test can assert
// that zero RPCs were sent rather than merely that no stubs exist.
type countingStubClient struct {
	adminv1connect.StubServiceClient
	creates int
}

func (c *countingStubClient) CreateStub(ctx context.Context, req *connect.Request[adminv1.CreateStubRequest]) (*connect.Response[adminv1.CreateStubResponse], error) {
	c.creates++
	return c.StubServiceClient.CreateStub(ctx, req)
}

func countingFactory(t *testing.T, srv *server.Server, counter *countingStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		counter.StubServiceClient = client.Stub
		client.Stub = counter
		return client, nil
	}
}

// failingDeleteStubClient fails every DeleteStub, which a healthy server never
// does, so the incomplete-rollback path is reachable.
type failingDeleteStubClient struct {
	adminv1connect.StubServiceClient
}

func (c *failingDeleteStubClient) DeleteStub(context.Context, *connect.Request[adminv1.DeleteStubRequest]) (*connect.Response[adminv1.DeleteStubResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("injected delete failure"))
}

func failingFactory(t *testing.T, srv *server.Server, failing *failingDeleteStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		failing.StubServiceClient = client.Stub
		client.Stub = failing
		return client, nil
	}
}

// cancelAfterCreateStubClient cancels the command's context once a stub has
// been created, reproducing the state a timed-out or interrupted create leaves:
// rollback must still run.
type cancelAfterCreateStubClient struct {
	adminv1connect.StubServiceClient
	cancel context.CancelFunc
}

func (c *cancelAfterCreateStubClient) CreateStub(ctx context.Context, req *connect.Request[adminv1.CreateStubRequest]) (*connect.Response[adminv1.CreateStubResponse], error) {
	resp, err := c.StubServiceClient.CreateStub(ctx, req)
	if err == nil && c.cancel != nil {
		c.cancel()
	}
	return resp, err
}

func cancellingFactory(t *testing.T, srv *server.Server, cancelling *cancelAfterCreateStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		cmd.SetContext(ctx)
		cancelling.cancel = cancel
		t.Cleanup(cancel)
		cancelling.StubServiceClient = client.Stub
		client.Stub = cancelling
		return client, nil
	}
}
```

Add `"connectrpc.com/connect"`, `"errors"`, the `adminv1` and `adminv1connect` imports, and `"github.com/spf13/cobra"` to `harness_test.go` as needed.

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestStubAdd|TestStubExportThenAdd' -count=1
```

Expected: FAIL — `unknown command "add" for "stub"`.

- [ ] **Step 4: Write the implementation**

Add to `internal/cli/stub.go`. `newStubCmdWithClient`'s registration is now complete and reads:

```go
	cmd.AddCommand(
		newStubListCmd(newClient),
		newStubRemoveCmd(newClient),
		newStubExportCmd(newClient),
		newStubAddCmd(newClient),
	)
```


```go
// rollbackTimeout bounds compensation. It is deliberately short: the work is a
// handful of deletes against a server that just answered.
const rollbackTimeout = 10 * time.Second

// pendingStub is one stub document with the source that produced it, so a
// diagnostic can name <file>#<index> rather than an opaque ordinal.
type pendingStub struct {
	source   string
	document string
}

func newStubAddCmd(newClient clientFactory) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create stubs from stub files",
		Long: "Create stubs from stub files.\n\n" +
			"Every file is read and split before the first request, so a malformed\n" +
			"file creates nothing. If the server rejects a stub, the stubs this\n" +
			"command already created are deleted.",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) == 0 {
				return errors.New("--file <stub-file> is required")
			}
			// Preflight: everything that can be checked without the server is
			// checked before anything is sent, so the largest class of failure
			// — a malformed input file — creates nothing at all.
			pending, err := preflightStubs(cmd, files)
			if err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()

			created := make([]string, 0, len(pending))
			for _, p := range pending {
				callCtx, cancel := client.callContext(ctx)
				resp, err := client.Stub.CreateStub(callCtx,
					connect.NewRequest(&adminv1.CreateStubRequest{Document: p.document}))
				cancel()
				if err != nil {
					return rollback(cmd, client, created, p, rpcError(ctx, err))
				}
				id := resp.Msg.GetStub().GetId()
				created = append(created, id)
				cmd.Printf("created %s\t%s\n", id, resp.Msg.GetStub().GetMethod())
			}
			return nil
		},
	}
	clientFlags{}.register(cmd)
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil,
		`stub file to create from, or "-" for stdin (repeatable)`)
	return cmd
}

// preflightStubs reads and splits every file before any RPC is sent.
func preflightStubs(cmd *cobra.Command, paths []string) ([]pendingStub, error) {
	var pending []pendingStub
	for _, path := range paths {
		var (
			raw []byte
			err error
		)
		if path == "-" {
			raw, err = io.ReadAll(cmd.InOrStdin())
		} else {
			raw, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		// The same splitter the file loader uses, so the CLI and a --stubs
		// directory can never disagree about what one stub is.
		docs, err := stub.SplitDocuments(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for i, doc := range docs {
			pending = append(pending, pendingStub{
				source:   fmt.Sprintf("%s#%d", path, i),
				document: doc,
			})
		}
	}
	return pending, nil
}

// rollback compensates for a failed create by deleting what this invocation
// made, newest first, then returns the error the command reports.
//
// This is compensation, not a transaction, and the wording never pretends
// otherwise. CreateStub mutates the store before it returns, so a response
// lost to a dropped connection, a deadline, or an interrupt leaves a stub whose
// id the client never learned — unnameable and therefore undeletable. True
// atomicity would need a server-side batch RPC or an idempotency token.
//
// The deletes run under their own bounded context rather than the command's.
// Compensation must still run when the command context is already cancelled or
// past its deadline, which is exactly what a lost or timed-out create produces;
// a rollback inheriting that context would be dead before it sent a byte.
func rollback(cmd *cobra.Command, client *adminClient, created []string, failed pendingStub, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()

	var orphaned []string
	for i := len(created) - 1; i >= 0; i-- {
		id := created[i]
		if _, err := client.Stub.DeleteStub(ctx,
			connect.NewRequest(&adminv1.DeleteStubRequest{Id: id})); err != nil {
			orphaned = append(orphaned, id)
		}
	}
	if len(orphaned) > 0 {
		cmd.PrintErrf(
			"rollback incomplete: %d stub(s) could not be removed and are still installed: %s\n",
			len(orphaned), strings.Join(orphaned, ", "))
		return fmt.Errorf("%s: %w", failed.source, cause)
	}
	cmd.PrintErrln("no stubs were added")
	return fmt.Errorf("%s: %w", failed.source, cause)
}
```

Add `"context"`, `"errors"`, `"strings"`, `"time"`, and `"github.com/yinghanhung/simulacra/internal/stub"` to `internal/cli/stub.go`'s imports — `errors` gets its first user here, in the `--file` check.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 6: Mutation-check the three guarantees**

Each of these must fail a specific test, and each guards a claim the design makes.

1. **Preflight.** Move the `preflightStubs` call to inside the create loop (split one file at a time). Confirm `TestStubAddPreflightSendsNoRPCsForAMalformedFile` FAILS with a non-zero create count. Revert.
2. **Rollback runs.** Replace `rollback(...)`'s delete loop body with `continue`. Confirm `TestStubAddRollsBackOnRejection` FAILS — the store keeps the two stubs. Revert.
3. **Rollback's independent context.** Change `rollback` to derive from the command context: `ctx, cancel := context.WithTimeout(cmd.Context(), rollbackTimeout)`. Confirm `TestStubAddRollsBackUnderACancelledCommandContext` FAILS. Revert.

- [ ] **Step 7: Run the whole suite**

```bash
go build ./... && go vet ./... && go test ./... -race -count=1
```

- [ ] **Step 8: Commit**

```bash
git add internal/cli/stub.go internal/cli/stub_test.go internal/cli/harness_test.go
git commit -m "feat(cli): add stub add with preflight and compensation

Every file is read and split before the first RPC, so a malformed file
creates nothing. The contract has no batch RPC, so the create phase
compensates instead: on the first rejection it deletes what it created,
newest first, under its own bounded context — compensation must still run
when the command context is already done, which is exactly what a
timed-out create leaves behind.

This is compensation, not a transaction, and the wording does not pretend
otherwise. CreateStub mutates the store before it returns, so a lost
response orphans a stub the client cannot name; a failed rollback delete
reports an incomplete rollback and names the orphans rather than claiming
a clean one."
```

---

### Task 9: `calls list`

`internal/cli` cannot record journal entries directly — `srv.journal` is unexported — so the tests make **real data-plane calls**, which is also the more honest fixture: it exercises the path a user's journal actually fills through.

**Files:**
- Create: `internal/cli/calls.go`, `internal/cli/calls_test.go`
- Modify: `internal/cli/harness_test.go` (data-plane helper)

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:
  - `func newCallsCmd() *cobra.Command`, `func newCallsCmdWithClient(newClient clientFactory) *cobra.Command`.
  - `func callLine(c *adminv1.Call) string` — the one-line summary shared by `list`.
  - Test helper `recordDataPlaneCall(t, srv, orderID string)`.

- [ ] **Step 1: Write the data-plane helper**

Append to `internal/cli/harness_test.go`:

```go
// recordDataPlaneCall makes one real GetOrder call against srv's data plane.
// This is how an internal/cli test produces a journal entry: srv.journal is
// unexported, so tests cannot record directly — and a real call is the fixture
// a user's journal is actually filled by.
//
// It installs a matching stub first, so the call is answered rather than
// failing UNIMPLEMENTED.
func recordDataPlaneCall(t *testing.T, srv *server.Server, orderID string) {
	t.Helper()
	createStub(t, srv, fmt.Sprintf(
		"method: shop.v1.OrderService/GetOrder\nmatch:\n  message:\n    order_id: %s\nrespond:\n  message: { order_id: %s }\n",
		orderID, orderID))

	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	desc, err := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}

	conn, err := grpc.NewClient(srv.DataAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	req := dynamicpb.NewMessage(desc.Input())
	req.Set(desc.Input().Fields().ByName("order_id"), protoreflect.ValueOfString(orderID))
	resp := dynamicpb.NewMessage(desc.Output())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/shop.v1.OrderService/GetOrder", req, resp); err != nil {
		t.Fatalf("data-plane GetOrder(%s): %v", orderID, err)
	}
}
```

Add to `harness_test.go`'s imports: `"fmt"`, `"time"`, `"google.golang.org/grpc"`, `"google.golang.org/grpc/credentials/insecure"`, `"google.golang.org/protobuf/reflect/protoreflect"`, `"google.golang.org/protobuf/types/dynamicpb"`, and `"github.com/yinghanhung/simulacra/internal/schema"`.

- [ ] **Step 2: Write the failing tests**

Create `internal/cli/calls_test.go`:

```go
package cli

import (
	"strings"
	"testing"
)

func TestCallsListText(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("calls list: %v", err)
	}
	for _, want := range []string{"SEQ", "METHOD", "CODE", "DURATION", "STUB",
		"/shop.v1.OrderService/GetOrder"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestCallsListEmptyReportsToStderr(t *testing.T) {
	srv := startCommandServer(t)
	stdout, stderr, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("calls list: %v", err)
	}
	if !strings.Contains(stderr, "no calls") {
		t.Errorf("stderr = %q, want a no-calls note", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
}

func TestCallsListNewestFirst(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")
	recordDataPlaneCall(t, srv, "o-2")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("calls list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	calls, _ := fields["calls"].([]any)
	if len(calls) < 2 {
		t.Fatalf("got %d calls, want at least 2: %s", len(calls), stdout)
	}
	first, _ := calls[0].(map[string]any)
	second, _ := calls[1].(map[string]any)
	// seq is a uint64 and therefore a JSON string; newest first means
	// descending.
	if first["seq"].(string) <= second["seq"].(string) {
		t.Errorf("calls are not newest-first: %v then %v", first["seq"], second["seq"])
	}
}

func TestCallsListLimit(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")
	recordDataPlaneCall(t, srv, "o-2")
	recordDataPlaneCall(t, srv, "o-3")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv),
		"--limit", "1", "--output", "json")
	if err != nil {
		t.Fatalf("calls list --limit: %v", err)
	}
	fields := decodeJSON(t, stdout)
	calls, _ := fields["calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %s", len(calls), stdout)
	}
}

func TestCallsListMethodFilter(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, stderr, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/WatchOrder")
	if err != nil {
		t.Fatalf("calls list --method: %v", err)
	}
	if strings.Contains(stdout, "GetOrder") {
		t.Errorf("a GetOrder call survived a WatchOrder filter:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no calls") {
		t.Errorf("stderr = %q, want a no-calls note", stderr)
	}
}

// The decoded request payload is reachable in JSON as an escaped string, which
// is the contract shape (design §4).
func TestCallsListJSONCarriesTheDecodedRequest(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-42")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("calls list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	calls, _ := fields["calls"].([]any)
	first, _ := calls[0].(map[string]any)
	requests, _ := first["requests"].([]any)
	if len(requests) == 0 {
		t.Fatalf("no requests recorded: %s", stdout)
	}
	req, _ := requests[0].(map[string]any)
	body, _ := req["json"].(string)
	if !strings.Contains(body, "o-42") {
		t.Fatalf("decoded request = %q, want the order id", body)
	}
	// Proto field names, not camelCase.
	if _, ok := req["type_name"]; !ok {
		t.Errorf("type_name absent (camelCase leaked?): %s", stdout)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run TestCallsList -count=1
```

Expected: FAIL — `undefined: newCallsCmd`.

- [ ] **Step 4: Write the implementation**

Create `internal/cli/calls.go`:

```go
package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newCallsCmd() *cobra.Command { return newCallsCmdWithClient(newAdminClient) }

func newCallsCmdWithClient(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calls",
		Short: "Inspect the calls a running server has recorded",
	}
	cmd.AddCommand(newCallsListCmd(newClient))
	return cmd
}

func newCallsListCmd(newClient clientFactory) *cobra.Command {
	var method string
	var limit int32
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List recorded calls, newest first",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Journal.ListCalls(callCtx, connect.NewRequest(&adminv1.ListCallsRequest{
				Method: method,
				Limit:  limit,
			}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				return writeJSON(cmd.OutOrStdout(), resp.Msg)
			}
			calls := resp.Msg.GetCalls()
			if len(calls) == 0 {
				cmd.PrintErrln("no calls")
				return nil
			}
			table := newTable(cmd.OutOrStdout())
			fmt.Fprintln(table, "SEQ\tMETHOD\tCODE\tDURATION\tSTUB")
			for _, c := range calls {
				fmt.Fprintln(table, callLine(c))
			}
			return table.Flush()
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "only calls to this method")
	cmd.Flags().Int32Var(&limit, "limit", 0, "return at most this many calls; 0 means no limit")
	return cmd
}

// callLine is the one-line summary `calls list` prints. Full detail belongs to
// --output json: a 1024-call journal rendered in full is not a readable
// default.
func callLine(c *adminv1.Call) string {
	return fmt.Sprintf("%d\t%s\t%d\t%s\t%s",
		c.GetSeq(), c.GetMethod(), c.GetStatus().GetCode(),
		c.GetDuration().AsDuration(), c.GetMatchedStubId())
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 6: Commit**

```bash
git add internal/cli/calls.go internal/cli/calls_test.go internal/cli/harness_test.go
git commit -m "feat(cli): add calls list

One line per call in text, full detail in --output json: a 1024-call
journal rendered in full is not a readable default. Tests make real
data-plane calls to fill the journal, since the server's journal is
unexported and a real call is the fixture a user's journal fills through."
```

---

### Task 10: `calls tail`

The reconnect loop is extracted from the RPC so its stream-end policy can be tested without eviction timing — the same separation 4b made for `streamCalls`. 4b already proves the server evicts and ends at teardown; what this task owns is what the CLI does about it (design §6.2).

**Files:**
- Modify: `internal/cli/calls.go`, `internal/cli/calls_test.go`

**Interfaces:**
- Consumes: Task 9's `newCallsCmdWithClient`.
- Produces:
  - `func newCallsTailCmd(newClient clientFactory) *cobra.Command`.
  - `type callStream interface { Receive() bool; Msg() *adminv1.WatchCallsResponse; Err() error; Close() error }`.
  - `func tailLoop(ctx context.Context, open func(context.Context) (callStream, error), emit func(*adminv1.Call) error, warn func(string)) error`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/calls_test.go`:

```go
// fakeCallStream replays a scripted stream: a run of calls, then an ending.
type fakeCallStream struct {
	calls []*adminv1.Call
	idx   int
	err   error
	msg   *adminv1.WatchCallsResponse
}

func (f *fakeCallStream) Receive() bool {
	if f.idx >= len(f.calls) {
		return false
	}
	f.msg = &adminv1.WatchCallsResponse{Call: f.calls[f.idx]}
	f.idx++
	return true
}
func (f *fakeCallStream) Msg() *adminv1.WatchCallsResponse { return f.msg }
func (f *fakeCallStream) Err() error                       { return f.err }
func (f *fakeCallStream) Close() error                     { return nil }

func call(seq uint64) *adminv1.Call {
	return &adminv1.Call{Seq: seq, Method: "/shop.v1.OrderService/GetOrder"}
}

// Eviction is the one ending that resumes: the CLI warns, reconnects, and
// says plainly that the calls in the gap are lost (design §6.2).
func TestTailLoopResumesAfterEviction(t *testing.T) {
	streams := []*fakeCallStream{
		{calls: []*adminv1.Call{call(1)}, err: connect.NewError(connect.CodeResourceExhausted, errors.New("slow consumer"))},
		{calls: []*adminv1.Call{call(2)}, err: connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))},
	}
	opened := 0
	open := func(context.Context) (callStream, error) {
		s := streams[opened]
		opened++
		return s, nil
	}
	var got []uint64
	var warnings []string
	err := tailLoop(context.Background(), open,
		func(c *adminv1.Call) error { got = append(got, c.GetSeq()); return nil },
		func(msg string) { warnings = append(warnings, msg) })

	if opened != 2 {
		t.Fatalf("opened %d streams, want 2 — eviction must reconnect", opened)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("delivered %v, want [1 2]", got)
	}
	if len(warnings) == 0 {
		t.Fatal("eviction produced no warning")
	}
	if !strings.Contains(strings.Join(warnings, " "), "lost") {
		t.Errorf("warning %q does not say calls in the gap are lost", warnings)
	}
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("final error = %v, want the unavailable ending to surface", err)
	}
}

// UNAVAILABLE is terminal and is NOT distinguished by message text: a dial
// failure produces the same code, and sniffing the server's wording would
// couple the CLI to it (design §6.2).
func TestTailLoopTreatsUnavailableAsTerminal(t *testing.T) {
	opened := 0
	open := func(context.Context) (callStream, error) {
		opened++
		return &fakeCallStream{err: connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))}, nil
	}
	err := tailLoop(context.Background(), open, func(*adminv1.Call) error { return nil }, func(string) {})
	if opened != 1 {
		t.Fatalf("opened %d streams, want 1 — unavailable must not reconnect", opened)
	}
	if err == nil {
		t.Fatal("unavailable did not surface as an error")
	}
}

func TestTailLoopStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opened := 0
	open := func(context.Context) (callStream, error) {
		opened++
		return &fakeCallStream{}, nil
	}
	if err := tailLoop(ctx, open, func(*adminv1.Call) error { return nil }, func(string) {}); err != nil {
		t.Fatalf("tailLoop on a cancelled context = %v, want nil", err)
	}
	if opened != 0 {
		t.Fatalf("opened %d streams on a cancelled context, want 0", opened)
	}
}

func TestTailLoopSurfacesAnOpenFailure(t *testing.T) {
	open := func(context.Context) (callStream, error) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	}
	if err := tailLoop(context.Background(), open, func(*adminv1.Call) error { return nil }, func(string) {}); err == nil {
		t.Fatal("an open failure did not surface")
	}
}

// End to end against a real server: a recorded call reaches the tail.
func TestCallsTailDeliversARecordedCall(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newCallsCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"tail", "--addr", adminAddr(srv), "--output", "json"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	// Record until the tail has picked one up, then stop it.
	deadline := time.After(20 * time.Second)
	for !strings.Contains(stdout.String(), "GetOrder") {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("tail delivered nothing; stdout=%q stderr=%q", stdout.String(), stderr.String())
		default:
		}
		recordDataPlaneCall(t, srv, "o-tail")
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	line := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)[0]
	fields := decodeJSON(t, line)
	if _, ok := fields["call"]; !ok {
		t.Fatalf("first line is not a WatchCallsResponse: %q", line)
	}
}

// Teardown ends the stream with UNAVAILABLE, which is terminal.
func TestCallsTailEndsWhenTheServerStops(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newCallsCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"tail", "--addr", adminAddr(srv)})

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	srv.Stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("tail returned nil when the server tore down; teardown is not a clean end")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("tail did not return after the server stopped")
	}
}

func TestCallsTailHasNoTimeoutFlag(t *testing.T) {
	cmd := newCallsCmd()
	for _, sub := range cmd.Commands() {
		if sub.Name() != "tail" {
			continue
		}
		if sub.Flags().Lookup("timeout") != nil {
			t.Fatal("calls tail registered --timeout; its stream is unbounded by design")
		}
		if sub.Flags().Lookup("addr") == nil {
			t.Fatal("calls tail is missing --addr")
		}
		return
	}
	t.Fatal("calls tail is not registered")
}
```

Add `"bytes"`, `"context"`, `"errors"`, `"time"`, `"connectrpc.com/connect"`, and the `adminv1` import to `internal/cli/calls_test.go`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestTailLoop|TestCallsTail' -count=1
```

Expected: FAIL — `undefined: tailLoop`, `undefined: callStream`.

- [ ] **Step 3: Write the implementation**

Add to `internal/cli/calls.go`, registering the command in `newCallsCmdWithClient`:

```go
	cmd.AddCommand(newCallsListCmd(newClient), newCallsTailCmd(newClient))
```

```go
// callStream is the part of a WatchCalls stream tailLoop uses. Narrowing it to
// an interface is what lets the stream-end policy be tested without
// reproducing an eviction, the same separation 4b made for streamCalls.
type callStream interface {
	Receive() bool
	Msg() *adminv1.WatchCallsResponse
	Err() error
	Close() error
}

func newCallsTailCmd(newClient clientFactory) *cobra.Command {
	var method string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "tail",
		Short:       "Stream calls as they are recorded",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()

			open := func(ctx context.Context) (callStream, error) {
				return client.Journal.WatchCalls(ctx,
					connect.NewRequest(&adminv1.WatchCallsRequest{Method: method}))
			}
			emit := func(c *adminv1.Call) error {
				if out.json() {
					return writeJSONLine(cmd.OutOrStdout(), &adminv1.WatchCallsResponse{Call: c})
				}
				cmd.Printf("%s\n", callLine(c))
				for _, req := range c.GetRequests() {
					cmd.Printf("  %s\n", req.GetJson())
				}
				return nil
			}
			warn := func(msg string) { cmd.PrintErrln(msg) }

			if err := tailLoop(ctx, open, emit, warn); err != nil {
				return err
			}
			return nil
		},
	}
	// No --timeout: the stream is unbounded by design and ends on a signal.
	clientFlags{}.registerNoTimeout(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "only calls to this method")
	return cmd
}

// tailLoop streams calls until the context ends or a terminal error arrives.
//
// Eviction is the one ending that resumes. RESOURCE_EXHAUSTED means the
// journal dropped this consumer for falling behind; a fresh watch starts from
// now, so the calls in the gap are gone and the warning says so rather than
// implying a gapless stream.
//
// UNAVAILABLE is terminal, and deliberately not distinguished by message text.
// Server teardown reports "server shutting down" and an unreachable address
// reports a dial failure, but both are UNAVAILABLE — matching on the wording to
// tell them apart would couple the CLI to the server's phrasing, which is the
// opposite of surfacing diagnostics verbatim. Both deserve the same answer:
// the server is gone. Reconnecting would busy-loop against a server that is
// tearing down, and a tail that silently survived a restart would be reporting
// on a journal that no longer exists.
func tailLoop(
	ctx context.Context,
	open func(context.Context) (callStream, error),
	emit func(*adminv1.Call) error,
	warn func(string),
) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		stream, err := open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for stream.Receive() {
			if err := emit(stream.Msg().GetCall()); err != nil {
				stream.Close()
				return err
			}
		}
		err = stream.Err()
		stream.Close()

		if ctx.Err() != nil {
			return nil
		}
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			warn("simulacra: the journal dropped this watch for falling behind; " +
				"resuming from now — calls recorded in the gap are lost")
			continue
		}
		return err
	}
}
```

Add `"context"` to `internal/cli/calls.go`'s imports.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check both endings**

1. Change the `CodeResourceExhausted` branch to `return err`. Confirm `TestTailLoopResumesAfterEviction` FAILS (`opened 1 streams, want 2`). Revert.
2. Add `|| connect.CodeOf(err) == connect.CodeUnavailable` to the reconnect condition. Confirm `TestTailLoopTreatsUnavailableAsTerminal` FAILS. Revert.

- [ ] **Step 6: Run the whole suite under the race detector**

```bash
go test ./... -race -count=1 -timeout 900s
```

- [ ] **Step 7: Commit**

```bash
git add internal/cli/calls.go internal/cli/calls_test.go
git commit -m "feat(cli): add calls tail

Eviction is the one ending that resumes: the CLI warns, reconnects, and
says plainly that the calls in the gap are lost. UNAVAILABLE is terminal
and deliberately not told apart by message text — teardown and an
unreachable address both report it, and matching on the server's wording
would couple the CLI to it.

The loop is extracted from the RPC so its stream-end policy is tested
without reproducing an eviction, the separation 4b made for streamCalls."
```

---

### Task 11: `verify`

The command M3 §7 calls CI-scriptable. Two things carry the weight: `--times` parses as **int32**, and a failed verdict exits 1 without an `error:` prefix, because the verdict is the output.

**Files:**
- Create: `internal/cli/verify.go`, `internal/cli/verify_test.go`

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:
  - `func newVerifyCmd() *cobra.Command`, `func newVerifyCmdWithClient(newClient clientFactory) *cobra.Command`.
  - `func parseTimes(specs []string) (*adminv1.Times, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/verify_test.go`:

```go
package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTimesGrammar(t *testing.T) {
	t.Run("exactly", func(t *testing.T) {
		times, err := parseTimes([]string{"exactly=2"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if times.GetExactly() != 2 {
			t.Fatalf("exactly = %d, want 2", times.GetExactly())
		}
	})

	t.Run("never", func(t *testing.T) {
		times, err := parseTimes([]string{"never"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if !times.GetNever() {
			t.Fatal("never was not set")
		}
	})

	t.Run("a comma-separated range", func(t *testing.T) {
		times, err := parseTimes([]string{"at-least=1,at-most=3"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if times.GetAtLeast() != 1 || times.GetAtMost() != 3 {
			t.Fatalf("range = [%d,%d], want [1,3]", times.GetAtLeast(), times.GetAtMost())
		}
	})

	t.Run("repeated flags accumulate", func(t *testing.T) {
		times, err := parseTimes([]string{"at-least=1", "at-most=3"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if times.GetAtLeast() != 1 || times.GetAtMost() != 3 {
			t.Fatalf("range = [%d,%d], want [1,3]", times.GetAtLeast(), times.GetAtMost())
		}
	})

	t.Run("an unknown key is rejected", func(t *testing.T) {
		if _, err := parseTimes([]string{"at-leest=1"}); err == nil {
			t.Fatal("an unknown key was accepted")
		}
	})

	t.Run("a non-integer is rejected", func(t *testing.T) {
		if _, err := parseTimes([]string{"exactly=two"}); err == nil {
			t.Fatal("a non-integer was accepted")
		}
	})

	// A key given twice is an error naming it, not last-one-wins: a caller
	// assembling flags from two places would otherwise get a verdict it did
	// not ask for, silently (design §6.3).
	t.Run("a repeated key is rejected", func(t *testing.T) {
		_, err := parseTimes([]string{"exactly=1", "exactly=2"})
		if err == nil {
			t.Fatal("a repeated key was accepted")
		}
		if !strings.Contains(err.Error(), "exactly") {
			t.Fatalf("error = %v, want it to name the repeated key", err)
		}
	})
}

// The wire fields are int32. Atoi followed by a conversion wraps silently,
// turning 2147483648 into -2147483648 and sending the server a different
// assertion under the user's name (design §6.3).
func TestParseTimesRejectsValuesOutsideInt32(t *testing.T) {
	for _, spec := range []string{
		"exactly=2147483648",
		"exactly=-2147483649",
		"at-least=2147483648",
		"at-most=9999999999999",
	} {
		t.Run(spec, func(t *testing.T) {
			if _, err := parseTimes([]string{spec}); err == nil {
				t.Fatalf("%s was accepted; it does not fit int32", spec)
			}
		})
	}

	// The boundaries themselves are valid.
	if _, err := parseTimes([]string{"exactly=2147483647"}); err != nil {
		t.Fatalf("the int32 maximum was rejected: %v", err)
	}
}

func TestVerifyRequiresTimes(t *testing.T) {
	_, _, err := runCmd(t, newVerifyCmd(), "--addr", "127.0.0.1:1",
		"--method", "shop.v1.OrderService/GetOrder")
	if err == nil {
		t.Fatal("verify ran without --times")
	}
	if !strings.Contains(err.Error(), "--times") {
		t.Fatalf("error = %v, want it to name --times", err)
	}
}

func TestVerifyPassExitsZero(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, _, err := runCmd(t, newVerifyCmd(), "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1")
	if err != nil {
		t.Fatalf("verify of a call that happened: %v", err)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("verify printed no verdict")
	}
}

// A failed assertion is the command's output, not a diagnostic about it: it
// maps to exit 1 and carries no error: prefix (design §5).
func TestVerifyFailureIsAnAssertionError(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	cmd := newVerifyCmd()
	stdout, _, err := runCmd(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=5")
	if err == nil {
		t.Fatal("verify passed an assertion that should have failed")
	}
	if !errors.Is(err, errAssertionFailed) {
		t.Fatalf("error = %v, want errAssertionFailed", err)
	}
	if got := exitCode(cmd, err); got != 1 {
		t.Fatalf("exitCode = %d, want 1", got)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("a failed verify printed no verdict")
	}
}

// The nearest-miss text is the shared diagnostic engine's, reproduced verbatim.
func TestVerifyPrintsNearestMissOnFailure(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	matcher := filepath.Join(t.TempDir(), "match.yaml")
	if err := os.WriteFile(matcher, []byte("message:\n  order_id: o-999\n"), 0o600); err != nil {
		t.Fatalf("write matcher: %v", err)
	}
	stdout, _, err := runCmd(t, newVerifyCmd(), "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder",
		"--match-file", matcher, "--times", "exactly=1")
	if err == nil {
		t.Fatal("verify matched a call it should not have")
	}
	if !strings.Contains(stdout, "o-1") {
		t.Errorf("stdout = %q, want the nearest-miss detail naming the actual value", stdout)
	}
}

// An operational failure is exit 2, not 1: the assertion never ran.
func TestVerifyAgainstAnUnreachableServerIsOperational(t *testing.T) {
	cmd := newVerifyCmd()
	_, _, err := runCmd(t, cmd, "--addr", "127.0.0.1:1",
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1")
	if err == nil {
		t.Fatal("verify succeeded against a closed port")
	}
	if errors.Is(err, errAssertionFailed) {
		t.Fatal("an unreachable server was reported as a failed assertion")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

// Combination validation belongs to the server; the CLI surfaces its
// INVALID_ARGUMENT rather than duplicating the rules (design §6.3).
func TestVerifyLeavesCombinationValidationToTheServer(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newVerifyCmd()
	_, _, err := runCmd(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "never,exactly=1")
	if err == nil {
		t.Fatal("never combined with exactly was accepted")
	}
	if errors.Is(err, errAssertionFailed) {
		t.Fatal("an invalid times combination was reported as a failed assertion")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

func TestVerifyJSON(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, _, err := runCmd(t, newVerifyCmd(), "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1",
		"--output", "json")
	if err != nil {
		t.Fatalf("verify --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	if fields["passed"] != true {
		t.Fatalf("passed = %v, want true: %s", fields["passed"], stdout)
	}
}

// A mistyped method is NOT_FOUND server-side, not a silent pass — 4b resolves
// the method even for an empty matcher.
func TestVerifyOfAMistypedMethodIsOperational(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newVerifyCmd()
	_, _, err := runCmd(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrdr", "--times", "never")
	if err == nil {
		t.Fatal("a mistyped method passed a never assertion")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'TestParseTimes|TestVerify' -count=1
```

Expected: FAIL — `undefined: parseTimes`, `undefined: newVerifyCmd`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/verify.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newVerifyCmd() *cobra.Command { return newVerifyCmdWithClient(newAdminClient) }

func newVerifyCmdWithClient(newClient clientFactory) *cobra.Command {
	var method, matchFile string
	var timeSpecs []string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Assert what a running server was asked to do",
		Long: "Assert what a running server was asked to do.\n\n" +
			"Exits 0 when the assertion holds, 1 when it does not, and 2 when it " +
			"could not run — so CI can tell a failed test from a broken job.",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			if len(timeSpecs) == 0 {
				return errors.New("--times is required, e.g. --times exactly=2")
			}
			times, err := parseTimes(timeSpecs)
			if err != nil {
				return err
			}
			var matcher []byte
			if matchFile != "" {
				if matcher, err = os.ReadFile(matchFile); err != nil {
					return fmt.Errorf("reading %s: %w", matchFile, err)
				}
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Verify.VerifyCalls(callCtx, connect.NewRequest(&adminv1.VerifyCallsRequest{
				Method:          method,
				MatcherDocument: string(matcher),
				Times:           times,
			}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				if err := writeJSON(cmd.OutOrStdout(), resp.Msg); err != nil {
					return err
				}
			} else {
				cmd.Println(resp.Msg.GetExplanation())
				for _, actual := range resp.Msg.GetActual() {
					cmd.Printf("%s\n", callLine(actual.GetCall()))
					cmd.Printf("  %s\n", actual.GetNearestMiss())
				}
			}
			if !resp.Msg.GetPassed() {
				// The verdict is already printed; this only sets the exit
				// code, and Execute suppresses the error: prefix for it.
				return errAssertionFailed
			}
			return nil
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "the method to assert on")
	cmd.Flags().StringVar(&matchFile, "match-file", "",
		"stub-grammar match block; omitted matches any call to the method")
	cmd.Flags().StringArrayVar(&timeSpecs, "times", nil,
		"how many calls must match: exactly=N, at-least=N, at-most=N, or never (repeatable, comma-separated)")
	return cmd
}

// parseTimes builds the Times assertion from --times specs.
//
// Keys accumulate across occurrences and comma-separated groups, so
// `--times at-least=1 --times at-most=3` and `--times at-least=1,at-most=3`
// are the same assertion. A key given twice is an error naming it rather than
// last-one-wins: a caller assembling flags from two places would otherwise get
// a verdict it did not ask for, silently.
//
// This validates only what the CLI alone can see — key spelling and integer
// syntax. Whether a combination is legal is journal.Times.Validate's, and the
// server already calls it; duplicating those rules here is how two surfaces
// drift.
func parseTimes(specs []string) (*adminv1.Times, error) {
	times := &adminv1.Times{}
	seen := map[string]bool{}
	for _, spec := range specs {
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, value, hasValue := strings.Cut(part, "=")
			if seen[key] {
				return nil, fmt.Errorf("--times %s given more than once", key)
			}
			seen[key] = true

			if key == "never" {
				if hasValue {
					return nil, errors.New("--times never takes no value")
				}
				times.Never = true
				continue
			}
			if !hasValue {
				return nil, fmt.Errorf("--times %s needs a value, e.g. %s=2", key, key)
			}
			n, err := parseTimesValue(key, value)
			if err != nil {
				return nil, err
			}
			switch key {
			case "exactly":
				times.Exactly = &n
			case "at-least":
				times.AtLeast = &n
			case "at-most":
				times.AtMost = &n
			default:
				return nil, fmt.Errorf(
					"--times %s is not a known key; use exactly=N, at-least=N, at-most=N, or never", key)
			}
		}
	}
	return times, nil
}

// parseTimesValue parses one count as int32.
//
// ParseInt with a 32-bit size, never Atoi: the wire fields are int32, and on a
// 64-bit host Atoi followed by an int32 conversion wraps silently — 2147483648
// becomes -2147483648, sending the server a different assertion than the user
// wrote, under the user's name.
func parseTimesValue(key, value string) (int32, error) {
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("--times %s=%s: %w", key, value, err)
	}
	return int32(n), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli/ -count=1
```

- [ ] **Step 5: Mutation-check the int32 bound and the verdict exit**

1. Replace `parseTimesValue`'s body with `n, err := strconv.Atoi(value)` and `return int32(n), err`. Confirm `TestParseTimesRejectsValuesOutsideInt32` FAILS. This is the mutation that matters most: the wrapped value is a *valid* wire value, so the RPC succeeds and a test asserting only "the command ran" would pass either way. Revert.
2. Change `return errAssertionFailed` to `return nil`. Confirm `TestVerifyFailureIsAnAssertionError` FAILS. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/verify.go internal/cli/verify_test.go
git commit -m "feat(cli): add verify

--times parses with ParseInt at 32 bits, never Atoi: the wire fields are
int32, and Atoi plus a conversion wraps 2147483648 into -2147483648,
sending the server a different assertion under the user's name. A key
given twice is an error naming it rather than last-one-wins.

Combination validation stays server-side, where journal.Times.Validate
already owns it. A failed verdict returns the assertion sentinel, so it
exits 1 with no error: prefix — the verdict is the output, not a
diagnostic about it."
```

---

### Task 12: Wiring, real signals, and the `serve` regression guard

The commands are registered, and the two things only a real process can prove are proven: that a signal reaches a client command at all, and that adding client commands did not change `serve`'s shutdown.

**Files:**
- Modify: `internal/cli/root.go`
- Create: `internal/cli/signal_test.go`

- [ ] **Step 1: Register the commands**

In `internal/cli/root.go`:

```go
	root.AddCommand(
		newServeCmd(),
		newCheckCmd(),
		newStubCmd(),
		newCallsCmd(),
		newVerifyCmd(),
		newSchemaCmd(),
	)
```

- [ ] **Step 2: Write the signal tests**

Create `internal/cli/signal_test.go`:

```go
package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildBinary compiles cmd/simulacra once per test binary. Signals are the one
// thing an in-process test cannot exercise honestly: the failure mode is that
// no handler is installed, and a test that cancels a context instead would
// pass against exactly that bug.
func buildBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "simulacra")
	build := exec.Command("go", "build", "-o", path, "../../cmd/simulacra")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/simulacra: %v\n%s", err, out)
	}
	return path
}

// waitForOutput blocks until the process has written needle to the buffer.
func waitForOutput(t *testing.T, read func() string, needle string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(read(), needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; output so far:\n%s", needle, read())
}

// exitStatus runs cmd to completion and returns its exit status.
func exitStatus(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("waiting for the process: %v", err)
	}
	return exit.ExitCode()
}

// calls tail has no completion criterion: being stopped is how it ends, so a
// signal is success (design §5).
func TestCallsTailExitsZeroOnRealSIGINT(t *testing.T) {
	binary := buildBinary(t)
	srv := startCommandServer(t)

	cmd := exec.Command(binary, "calls", "tail", "--addr", adminAddr(srv))
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Give the stream time to establish before signalling.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 0 {
		t.Fatalf("calls tail exited %d on SIGINT, want 0; output:\n%s", code, out.String())
	}
}

func TestCallsTailExitsZeroOnRealSIGTERM(t *testing.T) {
	binary := buildBinary(t)
	srv := startCommandServer(t)

	cmd := exec.Command(binary, "calls", "tail", "--addr", adminAddr(srv))
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 0 {
		t.Fatalf("calls tail exited %d on SIGTERM, want 0; output:\n%s", code, out.String())
	}
}

// Every other client command has a completion criterion it did not reach, so
// an interrupt is an operational failure (design §5).
func TestInterruptedUnaryCommandExitsTwo(t *testing.T) {
	binary := buildBinary(t)
	// A listener that accepts and never responds keeps the RPC in flight,
	// so the signal lands mid-call.
	ln := hangingListener(t)

	cmd := exec.Command(binary, "verify", "--addr", ln,
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1",
		"--timeout", "60s")
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 2 {
		t.Fatalf("interrupted verify exited %d, want 2; output:\n%s", code, out.String())
	}
}

// Regression guard: adding client commands must not change serve's shutdown.
// The first interrupt starts a GRACEFUL stop and says so; only a second one
// forces. A root-level signal context would break this, and neither existing
// shutdown test would catch it — both drive waitAndShutdown* directly.
func TestServeFirstInterruptIsStillGraceful(t *testing.T) {
	binary := buildBinary(t)
	cmd := exec.Command(binary, "serve",
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0", "--admin", "127.0.0.1:0", "--watch=false")
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForOutput(t, out.String, "data plane listening", 30*time.Second)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitForOutput(t, out.String, "interrupt again to force", 10*time.Second)
	if strings.Contains(out.String(), "forcing stop") {
		t.Fatalf("the first interrupt forced a stop:\n%s", out.String())
	}
	_ = cmd.Wait()
}
```

Add to `internal/cli/harness_test.go`:

```go
// syncBuffer is an io.Writer safe for a subprocess writer plus test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hangingListener returns the address of a listener that accepts connections
// and never responds, so an RPC stays in flight.
func hangingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	return ln.Addr().String()
}
```

Add `"bytes"`, `"sync"`, and `"net"` to `harness_test.go`'s imports.

- [ ] **Step 3: Run the tests**

```bash
go test ./internal/cli/ -run 'TestCallsTailExits|TestInterruptedUnary|TestServeFirstInterrupt' -v -count=1
```

Expected: PASS. If `TestServeFirstInterruptIsStillGraceful` fails, a signal context was installed at the root — move it back into the client commands.

- [ ] **Step 4: Mutation-check the interrupt asymmetry**

In `newCallsTailCmd`, make the tail return `errInterrupted` when the signal context ends (i.e. return `rpcError(ctx, err)` unconditionally instead of `tailLoop`'s nil). Confirm `TestCallsTailExitsZeroOnRealSIGINT` FAILS with exit 2. Revert.

Then, in `verify`, swallow the interrupt by returning `nil` when `ctx.Err() != nil`. Confirm `TestInterruptedUnaryCommandExitsTwo` FAILS with exit 0. Revert.

- [ ] **Step 5: Run the whole suite under the race detector**

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 -timeout 900s
```

- [ ] **Step 6: Commit**

```bash
git add internal/cli/root.go internal/cli/signal_test.go internal/cli/harness_test.go
git commit -m "feat(cli): register the client commands and pin signal behaviour

Signals are the one thing an in-process test cannot exercise honestly: the
failure mode is that no handler is installed, and a test that cancels a
context instead would pass against exactly that bug. These drive a built
binary with real SIGINT and SIGTERM.

Includes a regression guard that serve's first interrupt still starts a
graceful stop rather than forcing. A root-level signal context would break
that, and neither existing shutdown test would catch it — both drive
waitAndShutdown* directly with their own contexts."
```

---

## Verification checklist

Run before declaring the phase done, from the repository root:

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 -timeout 900s
```

Then confirm the phase's guard rails:

- [ ] `git diff --stat 74b74d6..HEAD -- api/ gen/ go.mod go.sum` prints nothing: no contract or module change.
- [ ] `git diff --stat 74b74d6..HEAD -- server/ internal/admin internal/dataplane internal/match internal/journal internal/schema` prints nothing.
- [ ] `git diff --stat 74b74d6..HEAD -- internal/stub` lists only `loader.go` and `loader_test.go`: exactly the one core change.
- [ ] `grep -rn 'stubDocuments' --include='*.go' . | grep -v '^./gen/'` prints nothing.
- [ ] `grep -rn --include='*.go' 'Phase 5' . | grep -v '^./gen/'` prints nothing.
- [ ] `grep -n 'ExecuteC' internal/cli/root.go` returns the call, and `grep -n 'root.Execute()' internal/cli/root.go` prints nothing.
- [ ] `grep -rn 'signal.NotifyContext' internal/cli/` shows it only in `client.go` — never in `root.go`.
- [ ] `grep -n 'strconv.Atoi' internal/cli/verify.go` prints nothing: counts parse at 32 bits.
- [ ] `simulacra verify --method shop.v1.OrderService/GetOrder --times exactly=1` against a server with no such call exits 1; against a closed port exits 2. Check with `echo $?`.
- [ ] `make lint-api` passes.

---

## Self-review notes

**Spec coverage.**

| Design section | Tasks |
|---|---|
| §1 scope | 1–12 |
| §2 package boundary | File structure; every task writes into it |
| §3 address resolution, deadlines, interruption | 3, with the real-signal proof in 12 |
| §4 output contract | 4, exercised by every read command in 5–11 |
| §5 exit codes, interrupt asymmetry | 2 and 12, with `verify`'s assertion path in 11 |
| §6.1 `stub` | 6 (`list`, `rm`), 7 (`export`), 8 (`add`) |
| §6.2 `calls` | 9 (`list`), 10 (`tail`) |
| §6.3 `verify` | 11 |
| §6.4 `schema` | 5 |
| §7 `SplitDocuments` | 1, consumed by 8 |
| §8 testing, the fault-injection seam | 3 (harness), 8 (decorators), and each task's own tests |
| §9 risks | Each risk's mitigation is a named test in the task that owns it |

**Deliberate deviations from the design.**

1. **`stub add`'s per-stub success lines go to stdout, not stderr.** §4 routes commentary to stderr, but the created ids are the command's result, not commentary about it — a script capturing `stub add`'s output wants them. The `no stubs were added` and `rollback incomplete` lines do go to stderr, as §4 requires.
2. **`calls tail`'s text mode prints the decoded request under a `callLine` header** rather than a bespoke header format. §6.2 asks for "a header line per call plus the indented decoded request protojson"; reusing `callLine` makes `list` and `tail` read identically, which §6.2 does not require but does not forbid.
3. **`stub rm` uses `cobra.MinimumNArgs(1)`** rather than a hand-rolled check. The design says only that an id is required.

**Known soft spots.**

- **`TestCallsTailDeliversARecordedCall` records in a loop until the tail picks one up.** The subscription is established asynchronously, so a single recorded call can race the watch. The loop is bounded at 20s and fails loudly rather than hanging.
- **The signal tests build `cmd/simulacra` per test.** That is several seconds each. If it becomes a drag, hoist the build into a `TestMain` — but do not replace the subprocess with an in-process cancellation, which would defeat the tests' entire purpose.
- **`TestInterruptedUnaryCommandExitsTwo` depends on the signal landing while the RPC is in flight.** The hanging listener holds the connection open and `--timeout 60s` keeps the call alive far longer than the 500ms wait, so the window is wide.
- **The `stub add` rollback tests assert the end state**, which discriminates here because nothing else removes API-origin stubs. Only the injected-failure paths need the decorator seam.

**Plan complete.**

---

## Post-execution amendments

Executed 2026-09-13 to 2026-09-19 on `feat/m3-p5-cli-client-commands`, twelve tasks, each
implemented by a fresh worker and reviewed before the next began. The plan's test code carried four
defects that execution found; all are corrected in the shipped branch but are recorded here because
the plan text above still shows the original.

1. **Task 6, Step 1 — unused import.** The `stub_test.go` import block lists
   `"github.com/spf13/cobra"`, which nothing in that file uses. Go rejects it. Task 8 adds no cobra
   use to that file either, so the import stays out.
2. **Tasks 9 and 11 — the match grammar, twice.** `recordDataPlaneCall`'s helper and
   `verify_test.go`'s `--match-file` fixture both wrote a match block as a bare scalar
   (`order_id: o-1`). `match.Rules` is `map[string]any` keyed by operator, so both were
   non-functional; the correct form is `order_id: { eq: o-1 }`, as `internal/stub/loader_test.go`
   already showed. A `respond:` block *is* a bare scalar — that half of the plan was right.
3. **Task 10, Step 1 — a data race.** `TestCallsTailDeliversARecordedCall` shares a bare
   `bytes.Buffer` between the test's polling goroutine and cobra's `RunE` goroutine. It reproduced
   100% under `-race`. Fixed by pulling Task 12's `syncBuffer` forward into `harness_test.go`; the
   package already had this precedent in `serve_test.go`'s `notifyWriter`.
4. **Task 3 / Task 12 — a duplicated fixture.** Both tasks define a listener that accepts and never
   responds. Defined once in Task 3's `harness_test.go` as `hangingListener`; Task 12 reuses it.

**Two defects the per-task structure could not catch**, found only by the whole-branch review:

5. **Every text payload shipped on stderr.** `cmd.Printf` routes through cobra's `OutOrStderr()`,
   which is `os.Stderr` unless something calls `SetOut` — and `Execute` never does. Measured on the
   built binary: `schema list` wrote 668 bytes to stderr and zero to stdout, while
   `schema list --output json` correctly used stdout. Design §4 requires payload on stdout.

   No task review could see it: `runCmd` calls `cmd.SetOut(&stdout)`, which makes `OutOrStderr()`
   return the *stdout* buffer, so every task's tests asserted the correct behaviour and passed
   against code that did the opposite. The ten payload sites now use
   `fmt.Fprintf(cmd.OutOrStdout(), …)`, `runCmd`'s doc comment no longer claims to prove the split,
   and two binary-level tests in `signal_test.go` pin it with genuinely separate pipes. **Any future
   command must be checked the same way — the in-process harness is blind to this by construction.**

6. **`stub add` claimed a rollback it could not know it achieved.** `rollback` branched only on
   whether the compensating deletes succeeded, printing `no stubs were added` even when the
   triggering `CreateStub` failed with a deadline, cancellation, or interrupt — exactly what
   design §6.1 forbids, since `CreateStub` mutates the store before returning. §8's lost-response
   test was never written into the plan either. Both are now in: `createRejected` classifies the
   cause (server verdicts keep the clean message; everything else reports that the store may hold an
   unidentified stub and names the in-flight document), and a `losingCreateStubClient` decorator
   drives the test.

**Deviations from the design, all deliberate.**

- `stub add`'s `created …` and `stub rm`'s `removed …` lines go to **stdout**, not stderr. They are
  each command's record of what it did, not commentary about it. Design §4's stdout/stderr split is
  otherwise followed exactly.
- `internal/admin/contract_test.go` still says "Phase 4" in a comment. Stripping it would have
  broken this phase's zero-diff guard rail on `internal/admin`; it belongs in its own change.

**Found by post-merge review, after the branch was first declared complete.** All four were
reproduced before being fixed, and the first three were shipped defects, not test-only issues.

7. **Malformed stub entries bypassed preflight.** `SplitDocuments` only splits YAML nodes; it does
   not strict-decode. So a file whose second entry carried an unknown field sent a real `CreateStub`
   for the first entry (`created api-1`) before the server rejected the second — breaking design
   §6.1's "no RPC is sent at all". `preflightStubs` now runs `stub.ParseDocument` on every document,
   the same parser the server's `compileDocument` calls, so the CLI still borrows the grammar rather
   than growing a second one. The boundary is unchanged: an unknown *method* or a CEL error needs
   the registry and is still caught server-side.

8. **Text output discarded write failures.** Every text payload site ignored `fmt.Fprintf`'s error
   while the JSON path returned `writeJSON`'s. Under `ulimit -f 0`: `schema list` wrote zero bytes
   and exited **0**, `--output json` exited 2, and a failing `verify` exited 1 having delivered no
   verdict — a CI job would have believed an assertion result it never received. Text writes are now
   checked. Note the ordering that keeps §5 intact: a *write* failure exits 2, but when the write
   succeeds a failed assertion still exits 1.

9. **Interrupting stdin broke the exit contract.** `stub add -f -` read stdin inside preflight,
   which ran before the signal context existed, so SIGINT gave 130 and SIGTERM 143 instead of 2. The
   signal context is now installed before preflight and the read is cancellation-aware. One subtlety
   worth keeping: when the read completes and the context cancels at the same moment, a plain
   `select` picks at random, so the cancellation is preferred explicitly — the same rule `rpcError`
   already follows.

10. **The `calls tail` signal tests raced.** Both synchronised with a 500ms sleep against the child
    installing its handler. They now wait for a delivered call, which *proves* the handler exists
    because `newCallsTailCmd` installs the signal context before opening the stream. A mutation that
    signals immediately fails 20/20.

11. **Amendment 9's fix was incomplete, and the gap it left was a regression.** Making the stdin
    read cancellation-aware covered `-f -` but not `-f <path>`, which still used a synchronous
    `os.ReadFile`. On a FIFO — or any file whose read blocks, such as one on a stalled network
    mount — the newly installed handler caught the signal, the context cancelled, and the read went
    on blocking. Measured: the command hung indefinitely, then exited **0** once the writer closed
    and the read returned empty, reporting success for an interrupted run that added nothing.

    It was strictly worse than before. At `6220b5c`, with no handler installed, the same signal
    killed the process promptly at 130 — a wrong exit code, but a terminating one. Installing the
    handler traded that for a hang plus a false success.

    `readStdin` is now `readSource`, serving both sources with identical cancellation semantics, and
    `preflightStubs` routes everything through it. Deliberately **no** FIFO detection: `os.Stat`
    cannot tell you whether a read will block, so the uniform treatment is both simpler and more
    correct. The lesson generalises past this command — *"a plain file read does not block"* is
    false, and any read reachable from a signal-handled path needs the same treatment.

Amendments 7, 9 and 11 are worth remembering together: both came from doing work — reading a file,
reading stdin — *before* the command's guarantees were in place. When adding a command, establish
the signal context and finish local validation before the first side effect.

**Follow-ups worth doing, none blocking.**

- `TestSchemaRegisterSendsOneAllOrNothingCall` discriminates only via its fixture's interdependent
  imports. A counting `SchemaServiceClient`, mirroring `countingStubClient`, would make the test's
  name true.
- `calls_test.go` compares journal `seq` values lexicographically. Correct for the two calls it
  records; a trap if copied into a test with ten or more.
- `adminClient.Control` has no consumer. Kept per design §2, documented as such.
