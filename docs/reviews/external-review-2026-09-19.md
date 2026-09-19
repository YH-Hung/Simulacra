# External Review — external-review
_Generated 2026-09-19 against 733574367b3c41ac20f4141b6fb83d8838e7faff_
_Plan: docs/superpowers/plans/2026-09-13-m3-p5-cli-client-commands.md_
_Implementation window: c989faa..733574367b3c41ac20f4141b6fb83d8838e7faff; current tracked tree unchanged_

## Verdict
**partially satisfied**: All planned command groups and the main architecture are implemented. Three reproduced edge-path defects violate preflight and exit-code guarantees, and one specified CLI regression test is absent. Passing the existing suite does not establish complete plan satisfaction.
- Requirements: 30 met / 4 partial / 1 deviated / 0 missing / 0 unverified (of 35)
- Findings: 0 critical / 3 important / 0 minor
- Verification: 9 gate command attempts, 6 passed, 0 product failures, 3 initially could not run because of sandbox restrictions; successful retries cover all required gates. Manual probes separately confirmed all three findings.

## Requirement traceability

All named tests below passed in the fresh full `go test ./... -race -count=1 -timeout 900s` run. Rows group closely related assertions into verifiable contract outcomes; historical commit/TDD/mutation execution steps are not treated as runtime requirements.

| ID | Requirement (plan section) | Status | Evidence |
| --- | --- | --- | --- |
| R1 | Export and reuse the file splitter, preserving document order, aliases and empty lists (Task 1) | met | `internal/stub/loader.go:92,115`; TestSplitDocumentsIsTheExportedFileSplitter |
| R2 | Preserve frozen API/generated and protected server packages (global constraints) | met | Protected-path diff from `74b74d6` is empty |
| R3 | Add no dependencies (global constraints) | met | Empty diff for `go.mod` and `go.sum` |
| R4 | Register all four command groups (Task 12) | met | `internal/cli/root.go:18`; subprocess help and command tests |
| R5 | Share HTTP client across five generated service interfaces and use factory seams (Task 3) | met | `internal/cli/client.go:97`; TestClientReachesTheAdminPlane |
| R6 | Flag > environment > default address, preserving full URLs (Task 3) | met | `internal/cli/client.go:69`; TestAddrResolutionPrecedence; binary environment probes |
| R7 | Per-RPC deadline, zero disables, negatives reject (Task 3) | met | `internal/cli/client.go:85,121`; timeout and hanging-server tests |
| R8 | Tail has no unary timeout (Tasks 3, 10) | met | `internal/cli/calls.go:135`; TestCallsTailHasNoTimeoutFlag |
| R9 | Command interruption exits 2 except tail, which exits 0 (Tasks 3, 12) | partial | `internal/cli/stub.go:269,277`; blocked-stdin subprocess exits 130/143 |
| R10 | Preserve serve/check exit and graceful shutdown behavior (Tasks 2, 12) | met | `internal/cli/exit.go:39`; TestExitCodeContract; TestServeFirstInterruptIsStillGraceful |
| R11 | Operational errors exit 2; assertion failure exits 1 without error prefix (Tasks 2, 11) | partial | `internal/cli/verify.go:73`; text output errors are discarded; standard exit tests pass |
| R12 | Proto names, default JSON values, compact NDJSON (Task 4) | met | `internal/cli/output.go:47,57`; output tests and TestVerifyJSON |
| R13 | Validate text/json; format has no shorthand and is read-only (Task 4) | met | `internal/cli/output.go:25`; output validation and export shorthand tests |
| R14 | Payload stdout, commentary stderr, including write-command success records (amendments) | deviated (equivalent) | `internal/cli/stub.go:123,297`; intentional amendment; binary stdout tests |
| R15 | Stub list filters, columns, finite/unlimited counts and empty notice (Task 6) | met | `internal/cli/stub.go:36`; stub-list tests |
| R16 | Read all input files and stdin, split before sending, preserve order (Task 8) | met | `internal/cli/stub.go:269,307`; stdin, ordering, missing-file and duplicate-stdin tests |
| R17 | Invalid local stub grammar sends zero RPCs (Task 8, design §6.1) | partial | `internal/cli/stub.go:336`; valid item plus unknown-field item creates then compensates |
| R18 | Add every valid document in order (Task 8) | met | `internal/cli/stub.go:280`; TestStubAddCreatesEveryStubInFileOrder |
| R19 | Reverse-order compensation, report orphaned IDs on failure (Task 8) | met | `internal/cli/stub.go:369`; rollback and incomplete-rollback tests |
| R20 | Lost CreateStub response warns of unidentified stubs (amendments) | met | `internal/cli/stub.go:391,421`; TestStubAddReportsPossibleUnidentifiedStubsWhenACreateResponseIsLost |
| R21 | Rollback has an independent bounded context (Task 8) | met | `internal/cli/stub.go:370`; TestStubAddRollsBackUnderACancelledCommandContext |
| R22 | Remove IDs fail-fast, preserve earlier deletion records and server diagnostics (Task 6) | met | `internal/cli/stub.go:95`; remove and file-origin diagnostic tests |
| R23 | Export format/destination compose; count on stderr (Task 7) | met | `internal/cli/stub.go:132`; TestStubExportFormatAndDestinationCompose; binary export probes |
| R24 | Export/add round-trip, including empty export (Tasks 7, 8) | met | `internal/cli/stub.go:160,307`; TestStubExportThenAddRoundTrips; TestStubAddOfAnEmptyExportAddsNothing |
| R25 | Calls list filters, limit and newest-first order (Task 9) | met | `internal/cli/calls.go:24`; calls-list tests |
| R26 | Tail emits decoded requests in text and one JSON object per line (Task 10) | met | `internal/cli/calls.go:116`; delivery and JSON line tests; sink-error defect is tracked by R11 |
| R27 | Eviction warns/reconnects; UNAVAILABLE terminates (Task 10) | met | `internal/cli/calls.go:151`; tail-loop eviction and teardown tests |
| R28 | Verify times grammar, duplicate keys and int32 overflow checks (Task 11) | met | `internal/cli/verify.go:109,174`; TestParseTimesGrammar; TestParseTimesRejectsValuesOutsideInt32 |
| R29 | Server owns Times combinations, matcher parsing and method resolution (Task 11) | met | `internal/cli/verify.go:55`; combination-validation and mistyped-method tests |
| R30 | Verify verdict, actual calls and nearest-miss diagnostics (Task 11) | met | `internal/cli/verify.go:63`; passing/failing/nearest-miss/JSON tests; ordinary binary verdict probes |
| R31 | Merge descriptor sets, deduplicate and reject conflicts before one RPC (Task 5) | met | `internal/cli/schema.go:37,54,156`; merge and one-call integration tests |
| R32 | Register returns file/service counts and preserves server errors (Task 5) | met | `internal/cli/schema.go:54`; register diagnostic tests and binary registration probe |
| R33 | Schema list includes both streaming markers (Task 5) | met | `internal/cli/schema.go:108`; TestSchemaListText; TestSchemaListJSON |
| R34 | Injectable clients enable fault tests against real servers (Tasks 3, 8) | met | `internal/cli/client.go:55`; counting, failed rollback and lost-response integration tests |
| R35 | Pin CLI re-registration success with zero new files (design §8 testing) | partial | `internal/cli/schema.go:59`; underlying admin idempotence test passes, but no CLI test asserts `0 new file(s)` |

## Gaps

### R17 — Local grammar preflight (partial)
**Plan says:** Any file that does not parse as the stub-file grammar fails before the first RPC.
**Found:** SplitDocuments validates YAML/sequence shape but does not perform the strict per-stub decode. The malformed-file test covers only a non-list top-level shape.
**Impact:** Bad input can create live stubs before rollback; concurrent calls may observe them, and rollback may fail.
**To satisfy:** Strictly parse each normalized document before sending any RPC; add unknown-field and scalar-item counting-client tests.

### R9 — Interruption during input (partial)
**Plan says:** Interrupted client commands other than tail exit 2.
**Found:** Signal handling is installed after blocking preflight reads.
**Impact:** SIGINT/SIGTERM during stdin input exits 130/143.
**To satisfy:** Establish handling before input and make the blocking read cancellation-aware; test real signals while stdin remains open.

### R11 — Operational output errors (partial)
**Plan says:** Operational errors exit 2, distinct from success and assertion failure.
**Found:** Several text payload writes discard errors, unlike JSON/export paths.
**Impact:** Scripts receive success or assertion failure although the requested output was never delivered.
**To satisfy:** Propagate text write errors; cover failed output sinks, including streaming.

### R35 — Dedicated CLI re-registration coverage (partial)
**Plan says:** Re-registering an unchanged set reports `0 new file(s)` and succeeds; the testing matrix calls for this case.
**Found:** Searched all schema CLI test names/bodies and registration assertions; no dedicated test. Admin-level idempotence is covered. The QA case named `register_duplicate` actually added six files, so it does not prove the zero-new-files path.
**Impact:** A coverage gap, not a reproduced runtime defect.
**To satisfy:** Register the same set twice through the command, asserting the second exit and count.

## Bugs and potential issues

### Critical
_None._

### Important

#### 1. Malformed stub entries can mutate the server before rejection — `internal/cli/stub.go:336`
**Confidence:** confirmed
**Failure scenario:** Feed a valid GetOrder stub followed by `- not_a_stub_field: true` to `stub add`. The actual server accepted the first item (`created api-1`), rejected the second with INVALID_ARGUMENT, and then received compensation. No RPC should have been sent.
**Why it matters:** Preflight atomicity is lost for locally detectable grammar errors. Temporary stubs are externally observable and failed compensation can leave them installed.
**Recommended fix:** Run the existing strict document parser on each normalized item during preflight, leaving schema/CEL-dependent checks server-side.

#### 2. Text commands silently ignore output failure — `internal/cli/verify.go:73`
**Confidence:** confirmed
**Failure scenario:** Against a real server, bind stdout to a read-only descriptor. `schema list` text exits 0; passing `verify` text exits 0; failing `verify` text exits 1. None delivers its payload or an error. The identical JSON invocations exit 2 with `bad file descriptor`.
**Why it matters:** Disk-full and descriptor errors can silently lose CI verdicts, listings, or mutation records. Text tail also continues after emitter write errors because its callback returns nil.
**Recommended fix:** Check and propagate writes at `verify.go:73-76`, `schema.go:61,109-114`, `calls.go:124-126`, and stub add/rm success output. For mutations, report already-completed work accurately when output fails.

#### 3. Open-stdin interruption uses default process exits — `internal/cli/stub.go:269`
**Confidence:** confirmed
**Failure scenario:** Launch `stub add -f -` with subprocess stdin open but no EOF, then deliver SIGINT/SIGTERM. Observed process return codes -2/-15, equivalent to shell exits 130/143, rather than 2. The handler is installed at line 277 only after input completes.
**Why it matters:** The documented CI exit contract varies depending on whether the signal arrives during input or during the RPC.
**Recommended fix:** Install command-scoped handling before preflight and explicitly cancel/unblock the input path. Merely moving NotifyContext earlier leaves io.ReadAll blocked. Test with real subprocess signals, not only context cancellation.

### Minor
_None._

## Out-of-plan changes
- Post-execution corrections to stdout routing, lost-response warnings, fixture grammar and test synchronization are justified and documented in the plan amendments.
- No risky unrelated tracked changes found. Frozen contracts, modules, and protected packages are unchanged.

## Plan defects
- The suggested preflight snippet only calls SplitDocuments although the prose promises full local file-grammar validation. Following that snippet does not satisfy the stronger stated requirement.
- The suggested signal setup occurs after preflight, leaving blocking input outside the promised interruption policy.
- Suggested text rendering ignores fmt write errors. The implementation follows that omission, but the resulting operational-error handling remains incorrect.
- Named mutation-check execution has no retained evidence. This review cannot reconstruct whether those historical process steps occurred; it does not infer failure merely from unchecked plan boxes.

## Verification log
- `go build ./...` → pass.
- `go vet ./...` → pass.
- `go test ./... -race -count=1 -timeout 900s` → pass, all packages. Used `/private/tmp/simulacra-review-gocache` and authorized localhost-network execution. Full output: `/private/tmp/simulacra-review-race.log`.
- Repository-pinned `bin/buf lint` → pass. Goal-review lane also reported successful `make lint-api`.
- `git diff --check` → pass.
- Protected-path `git diff --stat 74b74d6..HEAD -- api/ gen/ go.mod go.sum server/ internal/admin internal/dataplane internal/match internal/journal internal/schema` → pass, empty.
- Initial default-cache test invocation → could not run: sandbox cache access denied.
- Initial sandbox race run with temporary cache → could not run network tests: loopback bind denied; superseded by passing authorized run.
- Initial `make lint-api` in fresh worktree → could not reinstall tools using default cache; equivalent pinned lint succeeded.
- Real-server malformed-input, write-failure, and real-signal probes → reproduced all three important findings. Root ledger: `/private/tmp/simulacra-cli-review-ledger.md`.
- Additional QA logs: `/private/tmp/simulacra-cli-qa-scenarios.log`. The QA agent hit its usage limit before a terminal summary; its lane is inconclusive and is not counted as a passing review. Its shell-pipeline signal probe is excluded because background shells can inherit ignored SIGINT; the independent root subprocess probes supply the signal evidence.
- No implementation fixes were made. Temporary source fixtures were removed and no review server remained in the process check at finalization.
