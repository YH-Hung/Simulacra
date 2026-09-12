# Simulacra M3 Phase 5 — CLI Client Commands: Design

**Goal (M3 design §13.5):** ship the client commands — `stub`, `calls`, `verify`,
`schema register|list` — that talk to a running server's admin plane, with one shared client, one
output contract, and an exit-code contract CI can read.

**Reference:** `docs/superpowers/specs/2026-07-25-m3-test-story-design.md` §7 (CLI additions), §11
(error handling), §12 (testing), cited as "M3 §N";
`docs/superpowers/specs/2026-09-12-m3-p4b-admin-data-services-design.md` §4 (RPC semantics), §6
(`WatchCalls` lifecycle), §7 (error table), cited as "4b §N"; `PROPOSAL.md` §4.

Phase 4b is complete at `74b74d6`. The `simulacra.admin.v1` contract was frozen in Phase 2 and is
not modified here.

---

## 1. Scope

### In scope

1. `simulacra stub list|add|rm|export` (§6.1).
2. `simulacra calls list|tail` (§6.2).
3. `simulacra verify` (§6.3).
4. `simulacra schema register|list` (§6.4).
5. The shared admin client: `--addr` / `SIMULACRA_ADDR` resolution and the Connect-over-HTTP/1.1
   transport (§3).
6. The output contract: `--output text|json` on read commands (§4).
7. The exit-code contract: 0 success / 1 assertion failed / 2 operational error (§5).
8. One core change: exporting the file-grammar splitter `stub add` needs (§7).

### Out of scope

- `schema import --reflect` — Phase 6. Go SDK — Phase 7. Dockerfile — Phase 8. JVM SDK — Phase 9.
  The conformance parity leg — Phase 10.
- `api/` and `gen/`. No RPC is added, so `stub add` splits client-side (§6.1) rather than gaining a
  batch RPC.
- Admin authentication and TLS — M3 non-goals (M3 §2). `--addr` therefore speaks plaintext HTTP
  only, matching the loopback-by-default admin plane 4a shipped.
- `VerifySequence` — an M3 non-goal, so `verify` has no ordering assertions.

### Already shipped, despite M3 §7 listing it here

M3 §7 describes the **schema-less startup** relaxation alongside these commands. Phase 4a already
implemented it: `server/server.go:140` requires a schema source only when `AdminAddr` is empty, and
`server/admin_test.go:732` starts a server with no schema source and asserts a data-plane call
returns `Unimplemented`. `internal/cli/serve.go` passes the flags straight to `server.Start`, so
`serve` already boots schema-less. Phase 5 changes nothing here. `check` still requires a schema
source, as M3 §7 specifies.

### Dependency note

No new modules. The commands use the generated `adminv1connect` clients, `protojson` and
`descriptorpb` from `google.golang.org/protobuf`, `connectrpc.com/connect`, and `text/tabwriter`
from the standard library — all already required.

---

## 2. Package boundary

M3 §7's commands are client-side only: nothing in `server`, `internal/admin`, or any data-plane
package changes. The one exception is §7's core change, which exports a function that already
exists.

| File | Contents |
|---|---|
| `internal/cli/client.go` | `adminClient` — address resolution, the shared `http.Client`, the five generated service clients |
| `internal/cli/output.go` | `--output` flag, the protojson encoder, the `tabwriter` table helper |
| `internal/cli/exit.go` | `errAssertionFailed`, the command exit policy, `Execute`'s mapping |
| `internal/cli/stub.go` | `stub list\|add\|rm\|export` |
| `internal/cli/calls.go` | `calls list\|tail` |
| `internal/cli/verify.go` | `verify`, including `--times` parsing |
| `internal/cli/schema.go` | `schema register\|list`, including descriptor-set merging |
| `internal/cli/root.go` | four `AddCommand` calls, and `Execute` moved onto `ExecuteC` (modified) |
| `internal/stub/loader.go` | `stubDocuments` exported as `SplitDocuments` (modified, §7) |

Files are named after the command they implement, as `serve.go` and `check.go` already are. That a
file is called `stub.go` inside package `cli` while package `stub` is imported into it is a naming
collision only in prose: Go file names bind nothing.

---

## 3. The admin client

### Address resolution

`--addr` is registered on every client command, defaulting to `localhost:6566` — the default
`serve --admin` binds. Precedence is **flag > `SIMULACRA_ADDR` > default**. Cobra has no env
binding, so resolution is explicit: if the flag was not changed (`cmd.Flags().Changed("addr")` is
false) and `SIMULACRA_ADDR` is non-empty, the env value wins.

The resolved value becomes a base URL: used verbatim when it already carries an `http://` or
`https://` scheme, otherwise prefixed with `http://`. Accepting a full URL costs one branch and
saves the user who has the admin plane behind a path prefix or a proxy.

### Transport

One `*http.Client` with a default `*http.Transport`, shared by the five service clients. This is
the Connect protocol over HTTP/1.1 — **no h2c client-side**, which is why M3 §7 can promise client
commands need no HTTP/2 plumbing. 4b proved the path end to end: `server/admin_data_test.go:372`
runs the data services over exactly this client.

`calls tail` holds a stream open indefinitely, so the client carries **no global timeout**. Unary
commands inherit the command's context, which cobra cancels on interrupt. A per-call `--timeout`
is deliberately omitted: a dial to a dead address already fails promptly with `unavailable` (§11),
and a timeout flag nobody has asked for is a flag to maintain.

---

## 4. Output contract

`--output` accepts `text` (default) or `json`, and is registered **only on read commands**:
`stub list`, `stub export`, `calls list`, `calls tail`, `verify`, `schema list`. M3 §7 words it as
"`--output json` on every read command", and holding to that avoids inventing a CLI-defined JSON
shape for `stub add`, whose one invocation is many RPCs. Any other value is a flag error (exit 2,
§5).

**`--output` has no shorthand.** M3 §7 uses `-o` for a different purpose in the same sentence
(`stub export -o ./stubs`), and `-o` is the conventional shorthand for `--output`, so binding it to
the format flag would collide on the one command that needs both. On `stub export` the destination
flag is `--out`, with `-o` as *its* shorthand; `--output` is always spelled in full.

### JSON

The response message, rendered with:

```go
protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true, Multiline: true, Indent: "  "}
```

- **`UseProtoNames`** matches the house style §9 already fixes for the stub DSL and 4b §5.1 fixed
  for rendered call JSON: snake_case field names, the same names matcher paths use.
- **`EmitDefaultValues`** keeps scripts total. Verified: a `VerifyCallsResponse` with `passed` false
  renders `"passed": false` rather than omitting the field, so `jq .passed` is never `null`.
  `EmitUnpopulated` is not used — it would also emit `null` for unset message fields.
- The shape is the frozen contract, so it needs no separate versioning, and no server-rendered text
  is re-parsed. `Call.requests[].json` stays an **escaped string** exactly as the API returns it;
  `jq -r '.calls[0].requests[0].json'` unwraps one. Splicing it in as nested JSON was rejected:
  4b §5.1 lets rendering degrade to an empty `json`, and 4b §5.2 may substitute U+FFFD, so the
  field is not guaranteed to parse.
- **int64 fields render as JSON strings** — protojson's rule, verified for `Stub.hits` and
  therefore true of `Call.seq`. Scripts need `| tonumber`. Documented, not worked around: making
  them numbers would mean leaving protojson.

`calls tail` is the exception to `Multiline`: a stream has no single enclosing document, so it emits
**newline-delimited JSON**, one compact object per call, flushed per call.

### Text

`text/tabwriter`, column-aligned. Every command prints its payload to **stdout** and its commentary
— counts, warnings, stream-ended notices — to **stderr**, so `stub export` and `--output json` can
be piped without filtering.

---

## 5. Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Assertion failed — `verify` only |
| 2 | Operational error — unreachable server, bad flags, any non-OK RPC code |

M3 §7 specifies "exit 0 pass / 1 fail (CI-scriptable)" for `verify`. Taken literally with today's
`Execute`, which exits 1 on every error, a CI job cannot tell a failed assertion from a server it
could not reach — the job looks like a test failure. Splitting out 2 keeps §7's sentence true for
`verify` and makes it useful.

### Mechanism

`Execute` moves from `root.Execute()` to `root.ExecuteC()`, which returns the command that ran.
Client commands carry `Annotations{"exit": "client"}`; the policy is:

- Command annotated `client`: `errors.Is(err, errAssertionFailed)` → **1**, any other error → **2**.
- Anything else (`serve`, `check`, an unknown command, no subcommand): any error → **1**, unchanged.

`ExecuteC` returns the *found subcommand with its annotations* even when flag parsing fails —
verified: `--tiems` on an annotated `verify` returns `cmd="verify"`, `annotations=map[exit:client]`,
`err="unknown flag: --tiems"`. So a typo'd flag on `verify` exits 2, not 1, which is the whole
point. An unknown top-level command returns the root and keeps exiting 1.

Scoping the new code to annotated commands is deliberate: **`serve` and `check` keep exiting 1 on
error**, so no shipped command changes its exit code. See §10.1 — this is the open decision.

### Assertion failures are not errors

A failed `verify` has already printed its verdict to stdout. `Execute` therefore suppresses the
`error:` prefix for `errAssertionFailed` and exits 1 silently — the verdict is the output, not a
diagnostic about one.

---

## 6. The commands

All four groups surface server diagnostics **verbatim**. M3 §11's "one diagnostic engine, four
surfaces" means the CLI may prefix a message with its own source (`stubs.yaml#2: `) but never
rewords, truncates, or re-classifies what the server said.

### 6.1 `stub`

**`stub list [--method M] [--origin file|api] [--output …]`** → `ListStubs`.
`--origin` maps to `STUB_ORIGIN_FILE` / `STUB_ORIGIN_API`, omitted means every origin. Text is
M3 §7's five columns — `ID METHOD ORIGIN HITS TIMES` — with `times` rendered `unlimited` when 0,
mirroring the grammar's own meaning (`stub.proto`: "0 means unlimited"). An empty list prints
`no stubs` to stderr and exits 0.

**`stub add -f <file>… `** (repeatable; `-` reads stdin).
Each file is split by `stub.SplitDocuments` (§7) into per-stub documents, then sent as one
`CreateStub` per document, in file order. The contract has no batch RPC and Phase 5 does not add
one, so the sequence is **not atomic server-side**; the command makes it atomic client-side:

- On the first rejection, `DeleteStub` every ID this invocation created, in reverse order.
- Print `<file>#<index>: <server diagnostic>` and `no stubs were added`; exit 2.
- If a rollback delete itself fails, print that the rollback was **incomplete** and name every
  orphaned ID. The command never claims a clean rollback it did not achieve.

Rollback rather than partial state, because `CreateStub` is not idempotent: a re-run after a
partial failure duplicates everything that succeeded the first time.

`ReplaceAllStubs` is not used for `add` — it would delete the API stubs already present, which is
replacement, not addition.

**`stub rm <id>…`** → one `DeleteStub` per id, fail fast, **no rollback**. A deleted stub cannot be
recreated with its ID, so there is no state to roll back to; the command reports which ids were
removed before the failure. `FAILED_PRECONDITION` on a file-origin stub (4b §7) passes through
verbatim.

**`stub export [--out|-o <file>] [--output …]`** → `ExportStubs`. Writes `document` **verbatim** to
the path, or to stdout when `--out` is omitted; the `N stub(s)` count goes to stderr. Because the RPC
returns one file-grammar sequence and `stub add` reads that grammar, `export` then `add`
round-trips through the same splitter. `--output json` emits the whole response (document plus
`stub_count`) instead.

### 6.2 `calls`

**`calls list [--method M] [--limit N] [--output …]`** → `ListCalls`, newest first. Text is one
line per call: `SEQ METHOD CODE DURATION STUB`. Detail belongs to `--output json`; a 1024-call
journal printed in full detail is not a readable default.

**`calls tail [--method M] [--output …]`** → `WatchCalls`. Text prints a header line per call plus
the indented decoded request protojson, which is M3 §7's "prints decoded protojson". JSON is
newline-delimited (§4).

The server's `Send(nil)` header flush (4b, `internal/admin/journal.go:69`) is **not** delivered as a
message — verified: a client `Receive` against an idle watch blocks rather than returning an empty
call. So the loop needs no filtering for it.

Stream endings, from 4b §6 and its tests:

| Ending | Behaviour |
|---|---|
| `RESOURCE_EXHAUSTED` (slow-consumer eviction, `internal/admin/journal_test.go:369`) | Warn on stderr that calls were dropped, reconnect, **resume from now** — and say plainly that calls in the gap are lost. This is M3 §7's "warns and resumes from now" |
| `UNAVAILABLE` (`errShuttingDown`, `internal/admin/journal.go:127`, pinned at `server/admin_data_test.go:398`) | Print the server's message, exit **2**. No reconnect |
| Context cancelled (Ctrl-C) | Exit **0** |
| Any other code | Print verbatim, exit **2** |

`UNAVAILABLE` is deliberately terminal and deliberately **not** distinguished by message text. A
dial failure also yields `unavailable` (verified: `unavailable: dial tcp …: connect: connection
refused`), so the code alone cannot separate "shutting down" from "unreachable" — and sniffing the
message string to try would couple the CLI to the server's wording, the opposite of §11's rule.
Both deserve the same answer: the server is gone, say so and stop. Reconnecting would busy-loop
against a server that is tearing down, and a `tail` that silently survives a restart is reporting on
a journal that no longer exists.

### 6.3 `verify`

**`verify --method M [--match-file m.yaml] --times <spec> [--output …]`** → `VerifyCalls`.

`--times` is repeatable and comma-separated, taking `exactly=N`, `at-least=N`, `at-most=N`, and the
bare word `never`; `--times at-least=1,at-most=3` is a range. Each key sets its field on the proto
`Times`. **The CLI does not validate combinations.** `journal.Times.Validate` already owns those
rules and `internal/admin/verify.go:34` already calls it, returning `INVALID_ARGUMENT` with the
reason; duplicating them client-side is how two surfaces drift. The CLI rejects only what it alone
can see: an unknown key, a non-integer value, and an absent `--times` (4b §11.6 rejects an absent
`times` server-side, so the flag is required).

Keys accumulate across occurrences and across comma-separated groups, so `--times at-least=1
--times at-most=3` and `--times at-least=1,at-most=3` are the same assertion. **A key given twice
is an error naming it**, rather than last-one-wins: a scripted caller that assembles its flags from
two places would otherwise get a verdict it did not ask for, silently.

`--match-file` is sent as `matcher_document` bytes with no client-side parsing — the server owns the
grammar. Omitted, it is empty, which 4b §4.5 defines as matching any call to the method. Note that
4b resolves the method even for an empty matcher, so a mistyped method is `NOT_FOUND` rather than a
silent pass.

Text output is the server's `explanation` verbatim, then on failure each entry of `actual` as a
call header with its `nearest_miss` indented beneath. Exit 0 when `passed`, 1 when not (§5), 2 on
any RPC or flag error.

### 6.4 `schema`

**`schema register -f <file>… `** (repeatable) → one `RegisterSchemas`.

Each file is parsed as a `FileDescriptorSet` and their `file` lists are merged into one set, which
is sent in a single call. One call, because `RegisterSchemas` is all-or-nothing (4b §4.2) and
sequential calls would forfeit that.

Merging **must dedupe by file path**, and this is the command's one real piece of logic. Verified:
registering a set containing the same path twice fails with `invalid_argument: … proto: file
appears multiple times: "google/protobuf/timestamp.proto"`. Duplicates are the *common* case, not
the exotic one — any two sets built with `--include_imports` (or `buf build -o`) both embed the
well-known types they use. So:

- Merge in argument order, keeping the first occurrence of each `name`.
- If a later file carries the **same path but different bytes**, fail with an error naming the path
  and both source files. Silently keeping one of two conflicting definitions would register a
  schema the user never described.

A merged set that is still not self-contained fails server-side with the existing diagnostic
(verified: `descriptor sets must be self-contained; build with 'buf build -o' or 'protoc
--include_imports' … could not resolve import "google/protobuf/timestamp.proto"`), which passes
through verbatim.

Output: the `registered_files` count and the resulting `service_count`. `registered_files` is empty
when every file was already registered — a no-op re-register is a success, and the text says
`0 new file(s)` rather than implying failure.

**`schema list [--output …]`** → `ListServices`. Text prints each service as `name (file)` followed
by indented `Method(Input) returns (Output)` lines, marking `stream` on either side.

---

## 7. Core change: `stub.SplitDocuments`

`internal/stub/loader.go:107` already holds exactly the splitter `stub add` needs: unexported
`stubDocuments` renders each sequence item of each `---` document in a stub file as a normalized,
self-contained per-stub document — expanding aliases, so an item that aliased a sibling's anchor
still stands alone. Phase 5 exports it as `SplitDocuments` and leaves the behaviour untouched. Its
only current caller is `parseFile` (`internal/stub/loader.go:92`), verified by search.

This is what makes M3 §7's "split syntactically client-side" safe, and what keeps `stub.proto`'s
promise that "files and the API can never drift" literally true: **one splitter, used by the file
loader and the CLI**, feeding documents to the same `ParseDocument` the server runs. A second,
CLI-local YAML splitter would be a second grammar in all but name.

The export is the whole change. No signature change, no behaviour change, no new validation.

---

## 8. Testing

Per M3 §12, command tests run against an **in-process server**: `server.Start` with
`DataAddr` and `AdminAddr` on `127.0.0.1:0`, then `--addr` pointed at `srv.AdminAddr().String()`.
A shared `startCommandServer(t)` helper in `internal/cli` owns that, with `t.Cleanup` teardown.

| Layer | Tests |
|---|---|
| Address resolution | Flag beats env beats default; a bare `host:port` gains `http://`, a full URL is kept verbatim |
| Exit codes | `verify` failure → 1 with no `error:` prefix; a typo'd flag on `verify` → 2; an unreachable `--addr` → 2; `serve`/`check` failures still → 1 |
| Output | `--output json` of a failing verify contains `"passed": false`; proto names not camelCase; `calls tail --output json` emits one object per line; an invalid `--output` is a flag error |
| `stub list` | Method and origin filters; `unlimited` for `times: 0`; the empty-list message |
| `stub add` | Multi-stub file creates in order; a rejected stub rolls back every prior create, reports `<file>#<index>`, and leaves `ListStubs` as it was; a failed rollback names orphaned IDs; stdin via `-` |
| `stub rm` | File-origin stub yields the verbatim `FAILED_PRECONDITION`; no rollback of prior deletes |
| `stub export` | Document written verbatim; `export` → `add` round-trips to an equal stub set; `-o` is the shorthand for `--out`, not `--output` |
| `calls list` | Method filter, limit, newest-first order |
| `calls tail` | Delivers a call; eviction warns and resumes; teardown (`UNAVAILABLE`) exits 2; Ctrl-C exits 0. Runs under `-race` |
| `verify` | `--times` parsing for every key and the range form; an unknown key is a flag error; combination validation is left to the server and its `INVALID_ARGUMENT` is surfaced; nearest-miss text reproduced verbatim |
| `schema register` | Two sets sharing well-known-type imports merge and register; the same path with differing bytes is a client-side error naming both files; a non-self-contained set surfaces the server diagnostic; re-register reports `0 new file(s)` and succeeds |
| `schema list` | Streaming markers on both sides |
| `stub.SplitDocuments` | The existing `parseFile` tests already cover the behaviour; one test pins the exported name and multi-document splitting |

Every discriminating test carries a **named mutation check** — the convention the 4b plan
established, and the guard against a test that a later cleanup step would satisfy anyway. The
rollback test is the one most at risk: asserting only "the store is unchanged at the end" passes
even if rollback never ran, because a server that rejected stub 3 and a server that rolled back
stubs 1–2 differ only in the intermediate state. It must assert the **`DeleteStub` calls happened**,
not just the end state.

---

## 9. Risks

| Risk | Mitigation |
|---|---|
| A rollback delete fails and the CLI claims a clean rollback | The message distinguishes complete from incomplete and names orphaned IDs (§6.1); tested |
| `stub add` rollback deletes a stub the user created in another session | Only IDs this invocation created are deleted, tracked in order; never a filter-based sweep |
| The CLI duplicates `Times` validation and drifts from the server | It validates only key spelling and integer syntax; combination rules stay server-side (§6.3) and a test asserts the server's `INVALID_ARGUMENT` is what surfaces |
| Merging descriptor sets silently picks one of two conflicting definitions | Same path with differing bytes is a hard client-side error naming both files (§6.4) |
| `calls tail` reconnect loop spins against a dead server | `UNAVAILABLE` is terminal, not retried (§6.2); only `RESOURCE_EXHAUSTED` reconnects |
| `calls tail` reconnect is mistaken for a gapless stream | The warning states that calls in the gap are lost (§6.2) |
| Distinguishing teardown from an unreachable server by message text | Explicitly not done — both are `unavailable` and both exit 2 (§6.2) |
| int64 fields as JSON strings surprise a script author | Documented (§4); it is protojson's rule, and leaving protojson to change it would cost the contract shape |
| `ListCalls` or `ExportStubs` responses exceed a client receive limit | Connect's Go client has no default receive cap (verified, §11); the 4 MiB concern 4b carried applies to grpc-go/grpc-java clients, which is Phases 7 and 9, not here |
| A future client command forgets the `exit` annotation and reports 1 | The annotation is set by the shared constructor the command groups use, not per command |

---

## 10. Decisions flagged for review

1. **`serve` and `check` keep exiting 1 on error** (§5) — the conservative reading of "no shipped
   command changes its exit code", taken because the question was not answered before writing. The
   alternative is uniform 0/1/2 across every command, which is more consistent and moves two
   commands from 1 to 2. **This is the open decision; say which you want.**
2. **`--output json` on read commands only** (§4), following M3 §7's wording, rather than inventing
   a CLI-defined JSON shape for the multi-RPC write commands.
3. **`stub add` rolls back on first rejection** (§6.1) — human decision, over partial state and over
   continue-past-failures.
4. **`stub rm` does not roll back** (§6.1) — asymmetric with `add` by necessity, not oversight.
5. **`stub export --out` names one file** (§6.1) — human decision, amending M3 §7's plural "YAML
   files"; it is what `ExportStubs` returns and it round-trips through `stub add`. `-o` is that
   flag's shorthand, so `--output` keeps none (§4).
6. **`calls tail` treats `UNAVAILABLE` as terminal** (§6.2) — including server teardown. Only
   eviction reconnects.
7. **`--times` combination validation stays server-side** (§6.3).
8. **Descriptor sets are merged client-side into one all-or-nothing call, deduped by path**
   (§6.4), over sequential per-file calls.
9. **No `--timeout` flag** (§3) — YAGNI, revisited if a real hang appears.
10. **`SplitDocuments` is an export, not a new implementation** (§7).

---

## 11. Facts for the plan

Verified while writing this design, the runtime claims by throwaway probes driven through the real
admin plane and since deleted:

- A connect client against a closed port returns `unavailable: dial tcp 127.0.0.1:1: connect:
  connection refused`; `connect.CodeOf` is `unavailable`. Teardown's `errShuttingDown` is also
  `unavailable`, so the two are indistinguishable by code.
- `protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}` renders a
  `VerifyCallsResponse{Passed: false}` as `"passed": false, "matched": 0, "explanation": "",
  "actual": []`, and a `Stub` with enums by name (`"STUB_SHAPE_UNARY"`) and `"hits": "0"` — int64 as
  a JSON string.
- `protojson.MarshalOptions.EmitDefaultValues` exists in the pinned `google.golang.org/protobuf`
  v1.36.11 (`encoding/protojson/encode.go:103`).
- Registering a `FileDescriptorSet` containing a path twice fails `invalid_argument` with
  `proto: file appears multiple times: "google/protobuf/timestamp.proto"`.
- Registering a set with an unresolved import fails `invalid_argument` with `descriptor sets must
  be self-contained; build with 'buf build -o' or 'protoc --include_imports'`.
- Registering `testdata/protos` onto an empty registry returns `registered_files` sorted
  (`google/protobuf/any.proto`, `google/protobuf/timestamp.proto`, `shop/v1/order.proto`) and
  `service_count` 2; re-registering returns zero registered files and still succeeds.
- `WatchCalls`'s `Send(nil)` header flush is not delivered as a message: a client `Receive` against
  an idle watch blocks rather than returning an empty call.
- cobra v1.10.2 has `ExecuteC` / `ExecuteContextC` and `Command.Annotations`. `ExecuteC` returns the
  found subcommand **with its annotations** when flag parsing fails (`--tiems` on `verify` returns
  `cmd="verify"`, `annotations=map[exit:client]`); an unknown top-level command returns the root.
- connect v1.20.0's client applies a receive cap only when one is configured: the check is guarded
  by `readMaxBytes > 0` (`envelope.go:342`) and nothing sets it by default, so a CLI client has no
  default limit on response size.
- `stubDocuments` has exactly one caller, `parseFile` (`internal/stub/loader.go:92`).
- `server.Start` requires a schema source only when `AdminAddr` is empty
  (`server/server.go:140`), and schema-less startup is already tested
  (`server/admin_test.go:732`).

To execute before the plan quotes code:

- `stub export` of an empty store, fed back through `stub add`, adds zero stubs without error
  (4b verified `RenderSequence` of zero documents loads as zero stubs; the CLI path is untested).
- A `calls tail` client observes `RESOURCE_EXHAUSTED` and can immediately re-establish a watch on
  the same connection.
- `cmd.Flags().Changed("addr")` is false when only `SIMULACRA_ADDR` is set, under cobra's
  `ExecuteContextC`.
