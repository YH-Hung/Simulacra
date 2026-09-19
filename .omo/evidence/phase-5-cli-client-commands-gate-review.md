# Phase 5 CLI Client Commands — Gate Review

- **recommendation:** APPROVE
- **reviewed SHA:** `733574367b3c41ac20f4141b6fb83d8838e7faff`
- **diff base:** `c989faa`
- **review scope:** `/Users/yinghanhung/.codex/worktrees/review-cli-client-commands/Simulacra`
- **blockers:** none

## Original intent

Ship `simulacra stub list|add|rm|export`, `calls list|tail`, `verify`, and `schema register|list` as admin-plane client commands with a shared Connect/HTTP client, consistent text/JSON output, CI-readable 0/1/2 exits, per-unary deadlines, and command-scoped signal handling. Preserve all frozen API/generated files and existing `serve`/`check` behavior. Reuse the existing stub-file splitter rather than create a second grammar.

## Desired outcome

Users can inspect and mutate a running Simulacra server from the CLI, pipe payloads from stdout, distinguish failed assertions from operational failures, stop a tail cleanly, and safely preflight/compensate multi-stub additions within the limits documented by the design.

## User outcome review

The reviewed artifact implements the full command surface and the shared contracts. The two whole-branch defects recorded in the plan amendments are fixed: text payloads use `OutOrStdout`, and ambiguous `CreateStub` outcomes no longer claim a clean rollback. The race-enabled full suite and API lint pass at the reviewed SHA. No concrete design criterion is violated.

## Requirement matrix

| Requirement | Result | Evidence |
|---|---|---|
| Command registration and package boundary | PASS | `internal/cli/root.go:18-25`; diff changes only plan, `internal/cli`, and `internal/stub/{loader.go,loader_test.go}` |
| Shared address/client/deadline contract | PASS | `internal/cli/client.go:58-126`; address precedence and real hanging-server deadline tests in `internal/cli/client_test.go:39-159` |
| Per-command signal scope; tail 0, unary 2, serve unchanged | PASS | `internal/cli/client.go:128-140`; subprocess tests in `internal/cli/signal_test.go:111-181,223-243` |
| Output text/JSON and stdout/stderr split | PASS | `internal/cli/output.go:25-84`; explicit stdout writes at command payload sites; binary pipe tests in `internal/cli/signal_test.go:183-221` |
| Exit 0/1/2 contract scoped to client commands | PASS | `internal/cli/exit.go:9-47`, `internal/cli/root.go:29-39`, `internal/cli/exit_test.go:14-79` |
| `stub list|rm|export|add` | PASS | `internal/cli/stub.go:36-434`; real-server, rollback, lost-response, stdin, and round-trip cases in `internal/cli/stub_test.go` |
| `calls list|tail` | PASS | `internal/cli/calls.go:24-192`; filters/order/limit, delivery, eviction reconnect, teardown, and timeout absence in `internal/cli/calls_test.go` |
| `verify` grammar/verdict/nearest-miss behavior | PASS | `internal/cli/verify.go:18-180`; int32 boundaries, server-owned combination validation, verdict and operational paths in `internal/cli/verify_test.go` |
| `schema register|list` merge and render behavior | PASS | `internal/cli/schema.go:27-179`; dedupe/conflict/server diagnostic/stream-marker tests in `internal/cli/schema_test.go` |
| Single exported stub splitter reused by loader and CLI | PASS | `internal/stub/loader.go:92,102-140`; `internal/cli/stub.go:334-339`; splitter tests at `internal/stub/loader_test.go:185-246` |
| Frozen contracts/modules and forbidden package guard rails | PASS | `git diff --stat 74b74d6..HEAD` is empty for `api/`, `gen/`, `go.mod`, `go.sum`, `server/`, `internal/admin`, `internal/dataplane`, `internal/match`, `internal/journal`, `internal/schema` |
| Required verification | PASS | `go build ./...`; `go vet ./...`; `go test ./... -race -count=1 -timeout 900s`; `make lint-api` |

## Direct remove-ai-slops / programming pass

No blocker tied to a success criterion. Production files contain no debug leftovers, new dependency, unsafe escape, unchecked panic, or stale reference. The command seams are required by the design and used for fault injection. `internal/cli/stub.go` exceeds the generic 250 pure-LOC preference, but it owns the four explicitly grouped `stub` subcommands in the prescribed file structure; this is a maintenance note rather than a failed Phase 5 criterion. Tests assert observable RPC/CLI behavior rather than requested deletions or implementation-only parsing.

## Non-blocking coverage notes

1. `TestSchemaRegisterSendsOneAllOrNothingCall` does not count RPCs; its interdependent fixture only indirectly discriminates the one-call requirement (`internal/cli/schema_test.go:106-123`). The post-execution amendments explicitly record this as a non-blocking follow-up.
2. The design test matrix calls for re-registering the same descriptor set and asserting `0 new file(s)`. No dedicated test does that. Production output derives the count directly from `registered_files` (`internal/cli/schema.go:54-63`), and full integration coverage passes, so this is a coverage gap rather than evidence of incorrect behavior.
3. The plan requires named mutation checks for discriminating tests. No mutation-run artifact or code-review report was present in the worktree. The tests themselves discriminate the key behaviors, and the complete race suite is green; retain the mutation evidence with future execution artifacts.
4. `TestCallsListNewestFirst` compares two JSON string sequence values lexicographically (`internal/cli/calls_test.go:45-66`). It is valid for the two-call fixture and is already documented as a copy hazard in the amendments.

## Checked artifacts

- `docs/superpowers/plans/2026-09-13-m3-p5-cli-client-commands.md`, including post-execution amendments
- `docs/superpowers/specs/2026-09-12-m3-p5-cli-client-commands-design.md`
- Diff `c989faa..733574367b3c41ac20f4141b6fb83d8838e7faff`
- All changed production and test files under `internal/cli`
- `internal/stub/loader.go` and `internal/stub/loader_test.go`
- Full build, vet, race test, and API lint command output reproduced during review

## Exact evidence gaps

- No executor evidence bundle, code-review report, manual QA matrix, or notepad path was supplied or present under `.omo` in the reviewed worktree.
- No retained output proves the plan's manual mutation-check steps were executed.
- No dedicated automated re-registration test proves the `0 new file(s)` text case.

These gaps do not prove a stated user-visible criterion fails and therefore do not block approval.
