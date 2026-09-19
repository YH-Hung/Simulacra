# External Review — external-review
_Generated 2026-09-20 against aaf0c098b2f3d3814c036be4a3cca900957b2a2e_
_Plan: docs/superpowers/plans/2026-09-13-m3-p5-cli-client-commands.md_
_Implementation window: 733574367b3c41ac20f4141b6fb83d8838e7faff..aaf0c098b2f3d3814c036be4a3cca900957b2a2e; follow-up to the author's fixes_

## Verdict
**partially satisfied**: The four original reproduction cases are fixed, including the missing re-registration test. However, the interruption fix introduces a related named-file/FIFO regression: the command catches signals but leaves a synchronous file read blocked, and can subsequently report success after interruption. This is a narrow follow-up, not a new full requirement extraction.
- Requirements: 5 met / 1 partial / 0 deviated / 0 missing / 0 unverified (of 6 follow-up checks)
- Findings: 0 critical / 1 important / 1 minor
- Verification: 5 gate commands run, 5 passed, 0 failed, 0 could not run; manual probes separately confirm the original fixes and remaining regression.

## Requirement traceability
| ID | Requirement (plan section) | Status | Evidence |
| --- | --- | --- | --- |
| F1 | Strict local grammar preflight before any RPC (Task 8/design §6.1) | met | `internal/cli/stub.go:358-375`; zero-RPC counting regression passed; real binary rejects second unknown-field entry with no created output; unknown method still reaches server |
| F2 | Output failure is operational exit 2; delivered failed assertion stays 1 (Tasks 4,11) | met | `internal/cli/output.go:99-115`, `verify.go:72-92`; failed-writer tests passed; actual binary read-only-stdout probes return2, normal failed assertion returns 1 |
| F3 | Interrupted preflight input exits2 (design §5) | partial | `internal/cli/stub.go:278-290,345-347`; stdin held open now exits2 on both signals, but named FIFO blocks despite signals and later exits0 on EOF |
| F4 | Re-register schema reports zero new files (design §8) | met | `internal/cli/schema_test.go:266-287`; first register proves 1 new file, second proves 0; fresh race suite passes |
| F5 | Already-visible cancellation wins over simultaneous read completion (amendment9) | met | `internal/cli/stub.go:402-422`; read-result arm checks ctx.Err before returning; open-stdin subprocess probe avoids induced EOF |
| F6 | Tail signal tests prove child readiness (amendment10) | met | `internal/cli/signal_test.go:111-175`; delivered-call synchronization follows signal setup before WatchCalls; fresh tests pass |

## Gaps
### F3 — Cancellation-aware input must include named files (partial)
**Plan says:** Interrupted non-tail client commands exit 2.
**Found:** The handler is now installed before every preflight read, but only the stdin branch observes cancellation. The named-file branch still executes os.ReadFile synchronously.
**Impact:** Named pipes, including paths supplied via shell process substitution, can become uninterruptible with SIGINT/SIGTERM. If EOF arrives after cancellation, an empty input returns success.
**To satisfy:** Make both named-file open/read and stdin input cancellation-aware, and check cancellation before accepting successful preflight/zero-document completion. Add a real FIFO regression with its writer held open, then close the writer after signalling and assert exit 2.

## Bugs and potential issues
### Critical
_None._

### Important
#### 1. Named-pipe input hangs on signals and can later exit 0 — `internal/cli/stub.go:347`
**Confidence:** confirmed
**Failure scenario:** Create a FIFO, hold an O_RDWR descriptor open without writing, start `simulacra stub add -f <fifo>`, then send SIGINT and SIGTERM. The child remains running after each signal. In a separate run, close the FIFO writer after SIGINT: the child exits0 with empty stdout/stderr.
**Why it matters:** Moving signalContext before preflight suppresses the default signal termination, but os.ReadFile never observes cancellation. The stdin-only fix converts the named-file path from process termination into a hang and permits an interrupted command to report success.
**Recommended fix:** Apply cancellation-aware input handling to named paths as well, including a blocked open, and prefer an already-cancelled context before declaring completion. Preserve normal file/stdin grammar behavior and source diagnostics.

### Minor
#### 2. Unary signal test still guesses at handler readiness — `internal/cli/signal_test.go:195`
**Confidence:** likely
**Failure scenario:** On a sufficiently delayed process startup, SIGINT arrives during the fixed 500 ms wait before the child installs its handler, causing a false test failure.
**Why it matters:** A 60 s RPC deadline creates a wide window only after the RPC starts; it does not prove startup completed. The stdin test has the same acknowledged false-negative possibility. Neither is treated as evidence of a false pass.
**Recommended fix:** For the unary test, expose an accepted-connection event from the existing hanging-listener fixture and wait for it before signalling. No production-output change is needed. The stdin test's explicit interrupted diagnostic is useful and should remain.

## Out-of-plan changes
- Delivered-call synchronization for tail signal tests is justified: handler installation precedes stream setup, and observed output proves the relevant readiness state.
- The shared first-error payload writer is a proportionate fix; existing table/export/JSON paths already propagate their output failures.

## Plan defects
- The earlier prescription focused on stdin and did not account for a named path that can block. The general interruption guarantee still applies.
- The Python communicate correction is valid. Calling communicate immediately after signalling closes stdin, allowing EOF to race the handler. This review calls wait while retaining the stdin pipe, then communicates only after process exit. Both signals return2 with the interruption diagnostic under that test.
- No independent claim is made here that the author's historical mutation run failed 20/20; the readiness change is established directly from code ordering and fresh passing tests.

## Verification log
- `GOCACHE=/private/tmp/simulacra-review-gocache go build ./...` → pass.
- `GOCACHE=/private/tmp/simulacra-review-gocache go vet ./...` → pass.
- `GOCACHE=/private/tmp/simulacra-review-gocache go test ./... -race -count=1 -timeout 900s` → pass across all packages; localhost network permission enabled. Log: `/private/tmp/simulacra-fix-race.log`.
- Repository-pinned `bin/buf lint` → pass.
- `git diff --check` → pass.
- Real binary, stdin held open until child exit: SIGINT exit 2; SIGTERM exit 2; both print `interrupted before completion`.
- Real binary, read-only stdout: schema list/passing verify/failing verify all exit 2. Working stdout plus failed assertion returns 1 with verdict and no error diagnostic.
- Real binary, unknown-field second stub: local parse rejection, exit 2, no creation output. Unknown-method second stub: first create occurs, server rejects unknown method, rollback occurs, preserving server-owned validation boundary.
- Real binary, named FIFO: SIGINT still running after 1 s, SIGTERM still running after 1 s. Writer EOF after SIGINT yields exit 0.
- All root temporary servers and subprocesses were joined/stopped; FIFO fixtures auto-removed. No implementation code changed.
