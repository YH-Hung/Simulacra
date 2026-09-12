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
| `internal/cli/client.go` | `adminClient` — address resolution, the shared `http.Client`, the five generated service clients; `clientFactory`, the injection point the `…WithClient` constructors take (§8) |
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

### Deadlines

`calls tail` holds a stream open indefinitely, so the client carries **no `http.Client.Timeout`** —
a client-wide timeout would kill the stream.

That rules out a global timeout; it does not rule out a per-call one. **`--timeout` bounds each
unary RPC**, default 30s, `0` to disable, applied as a deadline layered on the command context.
**A negative value is a flag error (exit 2).** Cobra accepts one silently — verified:
`--timeout -1s` parses to `-1s` with a nil error — and `context.WithTimeout` with a negative
duration yields an already-expired context, so every RPC would fail instantly with
`deadline_exceeded` and the user would be debugging the server. The command validates
`timeout >= 0` before its first call.
A closed port does fail fast (§11), but a server or proxy that accepts the connection and never
returns headers does not, and `verify` — the CI-facing command — is exactly where an indefinite
hang is worst. `stub add` applies it per RPC, not per command.

`calls tail` does not register `--timeout`: its stream is unbounded by design, and it ends on
signal, not deadline.

### Interruption

**Cobra installs no signal handling**, verified: `ExecuteC` sets `c.ctx = context.Background()`
when no context was supplied (`cobra@v1.10.2/command.go:1085`), and `Execute` supplies none
(`internal/cli/root.go:21`). `serve` handles interrupts today only because it runs its own
`signal.Notify` (`internal/cli/serve.go:57`). Without a signal-aware context, a client command dies
on SIGINT by the process default — status 130 — not the 0 §5 promises.

Each client command therefore derives its own context:
`signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)`, with `defer stop()`.

**The signal context is scoped to client commands and is never installed at the root.** Putting
`signal.NotifyContext` around `ExecuteContextC` at the entry point would regress `serve`: the first
SIGINT would both arrive on serve's own channel *and* cancel the command context, and
`waitAndShutdownContextStop` treats a cancelled context as "no longer granting a graceful wait
period" — it forces immediately (`internal/cli/shutdown.go:53`). That destroys the two-stage
"interrupt again to force" contract `serve` prints to the user, which
`TestShutdownSecondSignalForces` pins. Neither existing test would catch it: both drive
`waitAndShutdown*` directly with their own contexts, so the regression would surface only in the
real binary. Three commits in Phase 4a went into separating teardown start from force escalation;
Phase 5 does not undo that from the entry point.

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
| 2 | Operational error — unreachable server, bad flags, any non-OK RPC code, or an interrupted command that is not `calls tail` |

**This table governs the Phase 5 client commands only.** `serve` and `check` keep exiting 1 on
every error: no shipped command changes its exit code in this phase. This was §10.1's open
question and it is now closed — see §10.1.

M3 §7 specifies "exit 0 pass / 1 fail (CI-scriptable)" for `verify`. Taken literally with today's
`Execute`, which exits 1 on every error, a CI job cannot tell a failed assertion from a server it
could not reach — the job looks like a test failure. Splitting out 2 keeps §7's sentence true for
`verify` and makes it useful.

### Mechanism

`Execute` moves from `root.Execute()` to `root.ExecuteC()`, which returns the command that ran.
Client commands carry `Annotations{"exit": "client"}`; the policy is:

- Command annotated `client`: `errors.Is(err, errAssertionFailed)` → **1**, any other error → **2**.
- Anything else (`serve`, `check`, an unknown command, no subcommand): any error → **1**, unchanged.

The annotation is what scopes the table above, so the two statements describe one rule rather than
competing ones.

`ExecuteC` returns the *found subcommand with its annotations* even when flag parsing fails —
verified: `--tiems` on an annotated `verify` returns `cmd="verify"`, `annotations=map[exit:client]`,
`err="unknown flag: --tiems"`. So a typo'd flag on `verify` exits 2, not 1, which is the whole
point. An unknown top-level command returns the root and keeps exiting 1.

Scoping the new code to annotated commands is deliberate: **`serve` and `check` keep exiting 1 on
error**, so no shipped command changes its exit code.

### Interrupt is a success for `calls tail` only

**`calls tail` interrupted by a signal exits 0. Every other client command interrupted by a signal
exits 2, after compensating.**

The asymmetry is the point. `calls tail` has no completion criterion — being stopped *is* how it
ends, so an operator's Ctrl-C, or a supervisor's SIGTERM, is the command working. Every other
client command has a completion criterion it did not reach: an interrupted `verify` produced no
verdict, an interrupted `schema register` does not know whether the swap landed, an interrupted
`stub rm` stopped partway through its ids, and an interrupted `stub add` is exactly the ambiguous
case §6.1 describes — a `CreateStub` may have mutated the store without the client learning its ID.
Reporting 0 for any of those would tell a script the work finished.

So `calls tail` treats signal cancellation as success, and the other commands treat it as an
operational error: they compensate where they can (§6.1), report what they know, and exit 2.

Signal disposition is not inspected. §3's context subscribes to both SIGINT and SIGTERM, and
`signal.NotifyContext` does not report which one fired, so a rule phrased as "SIGINT is success"
could not be implemented as written and SIGTERM would fall through it. The rule is per command, not
per signal, which is both implementable and the distinction that actually matters.

Commands detect this by checking whether **their own signal context** was cancelled, not by
matching `CodeCanceled` — a server-side cancellation produces that code too, and it is not an
interrupt.

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

**Preflight.** Every file is read and split by `stub.SplitDocuments` (§7) **before the first RPC**.
A file that cannot be read, or that does not parse as the file grammar, fails the command with
exit 2 and **no RPC is sent at all**. This makes the largest class of failure — a malformed input
file — genuinely atomic, and leaves the RPC phase to handle only rejections the server alone can
detect (an unknown method, a CEL diagnostic).

**The RPC phase is compensated, not atomic.** Documents go out as one `CreateStub` each, in file
order. On the first rejection the command compensates: `DeleteStub` for every ID it recorded, in
reverse order, then `<file>#<index>: <server diagnostic>` and `no stubs were added`, exit 2. If a
rollback delete itself fails, it prints that the rollback was **incomplete** and names every
orphaned ID.

**This is best-effort compensation, and the design says so rather than claiming atomicity it cannot
deliver.** `CreateStub` mutates the store before it returns — `internal/admin/stub.go:35` calls
`Store.Add` and only then builds the response — so a response lost to a dropped connection, a
deadline, or an interrupt leaves a stub the client never learned the ID of and therefore cannot
delete. In that case the command reports that the store may hold stubs it could not identify, and
names the document that was in flight. It never reports a clean rollback it did not achieve.
**True atomicity needs a server-side batch RPC or an idempotency token on `CreateStub`** — a change
to a contract frozen in Phase 2, and so not Phase 5's to make.

**Rollback runs under its own bounded context**, derived from `context.Background()` with a short
deadline, not from the command context. Compensation must still run when the command context is
already cancelled or past its deadline — which is exactly the situation a lost or timed-out
`CreateStub` creates — and a rollback inheriting that context would be dead before it sent a byte.

Compensating rather than leaving partial state, because `CreateStub` is not idempotent: a re-run
after a partial failure duplicates everything that succeeded the first time.

`ReplaceAllStubs` is not used for `add` — it would delete the API stubs already present, which is
replacement, not addition.

**`stub rm <id>…`** → one `DeleteStub` per id, fail fast, **no rollback**. A deleted stub cannot be
recreated with its ID, so there is no state to roll back to; the command reports which ids were
removed before the failure. `FAILED_PRECONDITION` on a file-origin stub (4b §7) passes through
verbatim.

**`stub export [--out|-o <file>] [--output …]`** → `ExportStubs`.

The two flags are orthogonal and compose rather than conflict: **`--output` chooses what is
written, `--out` chooses where it goes.** Neither combination is rejected.

| Invocation | Writes |
|---|---|
| `stub export` | the `document` verbatim, to stdout |
| `stub export --out stubs.yaml` | the `document` verbatim, to `stubs.yaml` |
| `stub export --output json` | the whole response (document plus `stub_count`), to stdout |
| `stub export --out stubs.json --output json` | the whole response, to `stubs.json` |

The `N stub(s)` count always goes to stderr, so the destination — file or stdout — carries only the
payload. Because the RPC returns one file-grammar sequence and `stub add` reads that grammar,
`stub export` then `stub add` round-trips through the same splitter in the default (text) form.

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
| Signal (Ctrl-C, or SIGTERM from a supervisor) | Exit **0** — `calls tail` has no completion criterion, so being stopped is how it ends (§5) |
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
can see: an unknown key, a value that is not an integer **in int32 range**, and an absent
`--times` (4b §11.6 rejects an absent `times` server-side, so the flag is required).

Values are parsed with `strconv.ParseInt(value, 10, 32)`, never `Atoi`. The wire fields are
`int32` (`verify.proto:19–21`), and on a 64-bit host `Atoi` followed by an `int32` conversion wraps
silently — verified: `Atoi("2147483648")` yields `2147483648`, and `int32` of it is
`-2147483648`. That would send the server a different assertion than the user wrote, under the
user's name. `ParseInt(…, 32)` rejects it as out of range instead. This is the same failure 4b §3.3
closed for stub `priority` and `times`; the CLI closes it at the flag.

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

### The fault-injection seam

Most tests need nothing but the real server. Two do: a **failed rollback delete** and a **lost
`CreateStub` response** (§6.1) cannot be produced by a healthy server at all.

The seam has two halves, and the interfaces are only the first.

**What to substitute** is free: the generated clients are already interfaces —
`adminv1connect.StubServiceClient` and its four siblings declare their RPCs as interface methods
(`gen/…/adminv1connect/stub.connect.go:50`). `adminClient` holds those interface types, so a
decorator wrapping the real client can count calls and fail or drop a chosen one. No new
abstraction, no mock server, and the production path stays the generated client.

**Where to substitute it** needs a named injection point, because a command otherwise builds its
own `adminClient` from `--addr` inside `RunE` and a test has nothing to reach. The repo already has
the pattern: `newServeCmd()` delegates to `newServeCmdWithListen(net.Listen)`
(`internal/cli/serve.go:18`), so the listener is injectable without the exported surface knowing.
Phase 5 follows it exactly:

```go
type clientFactory func(*cobra.Command) (*adminClient, error)

func newStubCmd() *cobra.Command { return newStubCmdWithClient(newAdminClient) }
func newStubCmdWithClient(newClient clientFactory) *cobra.Command { … }
```

— and the same pair for `calls`, `verify`, and `schema`. `root.go` calls the plain constructors;
tests call the `WithClient` form, passing a factory that builds the real `adminClient` against the
in-process server and wraps one of its five clients in the decorator. So the fault-injection tests
still run against a real server, differing from every other test in exactly one wrapped call.

**What does *not* need the seam**, correcting this design's earlier draft: proving that rollback
*ran* on the happy path needs only the real server. If rollback never ran, the two stubs created
before the rejection are still there, so `ListStubs` afterwards shows 2; if it ran, it shows 0. The
end state discriminates, and nothing else in the system removes API-origin stubs. The earlier claim
that the two cases "differ only in the intermediate state" was wrong. The seam is for the failure
paths only — which keeps the mocked surface as small as it can be.

| Layer | Tests |
|---|---|
| Address resolution | Flag beats env beats default; a bare `host:port` gains `http://`, a full URL is kept verbatim |
| Exit codes | `verify` failure → 1 with no `error:` prefix; a typo'd flag on `verify` → 2; an unreachable `--addr` → 2; `serve`/`check` failures still → 1 |
| Interruption | A **real SIGINT** delivered to a running `calls tail` exits 0, not 130 — sent to the process under test, not simulated by cancelling a context, since the whole failure mode is that no signal handler is installed. The same signal to an in-flight **unary** command exits 2, not 0, and SIGTERM to `calls tail` also exits 0. A serve-side regression guard asserts the first SIGINT to `serve` still starts a *graceful* stop rather than forcing (§3) |
| Deadlines | `--timeout` bounds a unary call against a server that accepts the connection and never responds; a negative `--timeout` is a flag error (exit 2); `calls tail` has no `--timeout` flag |
| Output | `--output json` of a failing verify contains `"passed": false`; proto names not camelCase; `calls tail --output json` emits one object per line; an invalid `--output` is a flag error |
| `stub list` | Method and origin filters; `unlimited` for `times: 0`; the empty-list message |
| `stub add` | Preflight: a malformed file sends **zero** RPCs (asserted by call count, since "no stubs created" alone would also hold if the server rejected them); multi-stub file creates in order; a rejected stub rolls back every prior create, reports `<file>#<index>`, and leaves `ListStubs` showing what it showed before; a failed rollback (seam) names orphaned IDs and says the rollback was incomplete; a lost `CreateStub` response (seam) reports possible unidentified stubs; rollback still runs when the command context is already cancelled; stdin via `-` |
| `stub rm` | File-origin stub yields the verbatim `FAILED_PRECONDITION`; no rollback of prior deletes |
| `stub export` | Document written verbatim; `export` → `add` round-trips to an equal stub set; `-o` is the shorthand for `--out`, not `--output`; all four `--out` × `--output` combinations write what §6.1's table says, and none is rejected |
| `calls list` | Method filter, limit, newest-first order |
| `calls tail` | Delivers a call; eviction warns and resumes; teardown (`UNAVAILABLE`) exits 2; Ctrl-C exits 0. Runs under `-race` |
| `verify` | `--times` parsing for every key and the range form; an unknown key is a flag error; a repeated key is an error naming it; **`2147483648` and `-2147483649` are rejected as out of range**, with a mutation check that `Atoi`-plus-conversion would silently pass; combination validation is left to the server and its `INVALID_ARGUMENT` is surfaced; nearest-miss text reproduced verbatim |
| `schema register` | Two sets sharing well-known-type imports merge and register; the same path with differing bytes is a client-side error naming both files; a non-self-contained set surfaces the server diagnostic; re-register reports `0 new file(s)` and succeeds |
| `schema list` | Streaming markers on both sides |
| `stub.SplitDocuments` | The existing `parseFile` tests already cover the behaviour; one test pins the exported name and multi-document splitting |

Every discriminating test carries a **named mutation check** — the convention the 4b plan
established, and the guard against a test that a later cleanup step would satisfy anyway. The two
most at risk:

- **The `--times` bounds test.** `Atoi` followed by an `int32` conversion produces a *valid* wire
  value, so the server accepts it and the RPC succeeds. A test asserting only "the command
  succeeded" passes either way; it must assert the **rejection**, and the mutation check swaps
  `ParseInt(…, 32)` for `Atoi` and confirms the test fails.
- **The preflight test.** "No stubs were created" is true both when preflight short-circuited and
  when the server rejected every document, so it must count RPCs, not stubs.

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
| A root-level signal context regresses `serve`'s two-stage shutdown | The signal context is per client command, never at the entry point (§3); a regression guard asserts the first SIGINT to `serve` still starts a graceful stop (§8) |
| `stub add` loses a `CreateStub` response and orphans a stub it cannot name | Unavoidable without a contract change (§6.1); the command reports that the store may hold stubs it could not identify rather than claiming a clean rollback |
| Rollback inherits a cancelled command context and never runs | Rollback uses an independent bounded context (§6.1); tested with the command context already cancelled |
| An interrupted unary command reports success and a script treats the work as done | Exit 0 on signal is scoped to `calls tail` alone; every other client command exits 2 after compensating (§5) |
| A negative `--timeout` expires every context and the user debugs the server | Rejected as a flag error before the first call (§3); cobra accepts it silently, so the check must be explicit |
| `--timeout` fires during a legitimately slow `verify` on a large journal | 30s default with `0` to disable; the deadline is per RPC, not per command |
| A future client command forgets the `exit` annotation and reports 1 | The annotation is set by the shared constructor the command groups use, not per command |

---

## 10. Decisions flagged for review

1. **`serve` and `check` keep exiting 1 on error** (§5) — **decided in review**. The 0/1/2 table is
   explicitly scoped to the Phase 5 client commands, so no shipped command changes its exit code.
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
9. **No `http.Client.Timeout`** (§3) — a client-wide timeout would kill `calls tail`'s stream.
   Superseded in part by decision 13, which adds the per-RPC deadline this originally conflated
   with it.
10. **`SplitDocuments` is an export, not a new implementation** (§7).
11. **A per-client-command signal context, not a root-level one** (§3) — from review. The review
    asked for `signal.NotifyContext` plus `ExecuteContextC`; scoping it to client commands is a
    deliberate narrowing, because the root-level form forces `serve` on the first interrupt.
12. **`stub add` is preflight-then-compensate, and is documented as best-effort** (§6.1) — from
    review, replacing a claim of client-side atomicity that `CreateStub`'s mutate-before-return
    makes impossible.
13. **`--timeout` bounds each unary RPC, default 30s; `calls tail` does not have it** (§3) — from
    review, reversing this design's original omission.
14. **`--times` values parse as int32, not `int`** (§6.3) — from review.
15. **Fault injection reuses the generated client interfaces** (§8) — from review, and scoped to
    the two failure paths a healthy server cannot produce; the happy-path rollback assertion needs
    no seam, correcting this design's earlier rationale.
16. **Signal cancellation exits 0 for `calls tail` and 2 for every other client command** (§5) —
    from review, narrowing an earlier blanket "interrupt is a success". The rule is per command,
    not per signal, because `signal.NotifyContext` does not report which signal fired.
17. **A negative `--timeout` is a flag error** (§3) — from review.
18. **`--out` and `--output` compose rather than conflict on `stub export`** (§6.1) — from review.
    Format and destination are orthogonal, so no combination is rejected.
19. **Commands take a `clientFactory` through a `…WithClient` constructor** (§8) — from review,
    following `newServeCmdWithListen`'s existing precedent rather than inventing a second pattern.

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
- cobra v1.10.2's `ExecuteC` sets `c.ctx = context.Background()` when no context was supplied
  (`command.go:1085`), and `internal/cli/root.go:21` supplies none. Cobra installs no signal
  handling; `serve` runs its own (`internal/cli/serve.go:57`).
- `waitAndShutdownContextStop` forces immediately once its context is cancelled
  (`internal/cli/shutdown.go:53`), pinned by `TestShutdownContextCanceledDuringGraceForcesAndJoins`;
  `TestShutdownSecondSignalForces` pins the two-stage signal path. Both drive the function directly
  with their own contexts, so neither would catch a root-level signal context.
- `CreateStub` calls `Store.Add` before building its response (`internal/admin/stub.go:35`), so a
  lost response leaves a stub the client cannot name.
- `strconv.Atoi("2147483648")` returns `2147483648` with a nil error on this 64-bit host, and
  `int32` of that is `-2147483648`. `strconv.ParseInt("2147483648", 10, 32)` returns
  `value out of range`.
- cobra accepts a negative duration flag silently: `--timeout -1s` parses to `-1s` with a nil
  error.
- `newServeCmd()` already delegates to `newServeCmdWithListen(net.Listen)`
  (`internal/cli/serve.go:18`), the injection precedent Phase 5 follows.
- `adminv1connect`'s five clients are declared as **interfaces**
  (`gen/…/adminv1connect/stub.connect.go:50`), so a test decorator needs no new abstraction.
- Nothing but `Store.Remove` and `ReplaceOrigin` removes API-origin stubs, so `ListStubs` after a
  failed `stub add` discriminates a rollback that ran from one that did not.
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
  `ExecuteC`.
- A real SIGINT to a process running `calls tail` under a per-command `signal.NotifyContext` exits
  0, and the same signal to `serve` still starts a graceful stop rather than forcing.
- A connect unary call against a listener that accepts and never writes headers blocks until its
  context deadline, and surfaces as `deadline_exceeded`.
