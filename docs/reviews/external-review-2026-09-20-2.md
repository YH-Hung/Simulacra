# External Review — external-review
_Generated 2026-09-20 against 461cceaeffcb1d958cb395e7dd33c89d99d06c0f_
_Plan: docs/superpowers/plans/2026-09-13-m3-p5-cli-client-commands.md_
_Implementation window: aaf0c098b2f3d3814c036be4a3cca900957b2a2e..461cceaeffcb1d958cb395e7dd33c89d99d06c0f; named-source cancellation follow-up_

## Verdict
**fully satisfied** for the reviewed follow-up: the named-file/FIFO cancellation regression is fixed, normal input behavior remains correct, and the new FIFO readiness synchronization is sound. All previously reproduced blocking findings are now closed. The author's earlier unnamed suite failure remains unexplained; passing reruns do not establish its cause or prove that no intermittent failure exists.
- Requirements: 5 met / 0 partial / 0 deviated / 0 missing / 0 unverified (of 5 follow-up outcomes)
- Findings: 0 critical / 0 important / 0 minor newly introduced
- Verification: 6 gate/stress commands run successfully, 6 passed, 0 product failures. The QA lane's initial sandbox bind restriction was resolved by an authorized rerun. Manual binary probes passed separately.

## Requirement traceability
| ID | Requirement (plan section) | Status | Evidence |
| --- | --- | --- | --- |
| F1 | Named-source interruption terminates promptly with exit 2 (design §5/amendment 11) | met | `internal/cli/stub.go:344,402`; root FIFO probes with writer held open: SIGINT exit 2 and SIGTERM exit 2 with interruption diagnostics |
| F2 | Cancellation is not mistaken for successful empty input (amendment 11) | met | `internal/cli/stub.go:437`; explicit ctx.Err check in completed-read arm; real signal-then-EOF probe exits 2 |
| F3 | Regular files, stdin, and malformed-file preflight retain behavior (Task 8) | met | `internal/cli/stub.go:330-374`; actual server: regular file and stdin each create one stub/exit 0, malformed later entry rejects locally/exit 2, final count remains two; fresh full tests pass |
| F4 | FIFO regression tests establish handler readiness and bound hangs (amendment 11) | met | `internal/cli/signal_test.go:452`; blocking O_WRONLY open rendezvous happens after child's early signalContext; exitStatusWithin bounds termination; three FIFO tests pass ten repetitions each under race detector |
| F5 | Required build/test/lint gates still pass (verification checklist) | met | Fresh build, vet, full race suite, pinned buf lint, and diff whitespace check all pass at reviewed SHA |

## Gaps
_None within this follow-up._

## Bugs and potential issues
### Critical
_None._

### Important
_None newly found; the previously confirmed FIFO hang and false-success paths are closed._

### Minor
_None newly introduced. The previously noted startup-sleep flake risk in unary/stdin signal tests remains a nonblocking test-quality limitation._

## Out-of-plan changes
- `readStdin` became `readSource`, uniformly wrapping both input paths. This is directly justified by the discovered blocking named-file behavior; inode-type detection would not establish whether a read can block.
- FIFO tests synchronize on an open rendezvous instead of a fixed startup delay. The rendezvous proves the read endpoint is open after handler setup; it need not prove the goroutine has entered the read syscall to establish signal safety.

## Plan defects
_None newly blocking._

Two evidence qualifications:
- The signal-then-EOF test waits for the interruption diagnostic before closing the writer. On the fixed implementation, cancellation has already won by then. It is useful coverage against the former synchronous-read regression, but it does not independently exercise the simultaneous-ready select arm. The production ctx.Err check is correct by inspection; a separate focused cancelled-context/ready-result test would provide stronger coverage if desired.
- The author reported one full-suite failure without the test name or captured output. Server contamination is an unverified hypothesis, not a root cause. This review did not reproduce the failure and does not claim it fixed. Capture full `go test -json` output if it recurs rather than accepting a passing retry as diagnosis.

## Verification log
- `GOCACHE=/private/tmp/simulacra-review-gocache go build ./...` → pass.
- `GOCACHE=/private/tmp/simulacra-review-gocache go vet ./...` → pass.
- `GOCACHE=/private/tmp/simulacra-review-gocache go test ./... -race -count=1 -timeout 900s -json` → pass across all nine tested packages; 821 passing named-test/subtest events. Full JSON retained at `/private/tmp/simulacra-source-race.jsonl`.
- Pinned repository `bin/buf lint` → pass.
- `git diff --check` → pass.
- `GOCACHE=/private/tmp/simulacra-review-gocache go test ./internal/cli -race -count=10 -run 'TestInterruptedNamedPathReadExitsTwo|TestSIGTERMDuringNamedPathReadExitsTwo|TestSignalledThenEmptiedNamedPathStillExitsTwo'` → pass, 30 FIFO test invocations; package elapsed 2.949s. Initial sandbox attempt could not bind localhost; authorized identical rerun passed.
- Root subprocess FIFO probes used an O_WRONLY-open rendezvous: SIGINT/SIGTERM with writer held open both exit 2 promptly and report interruption.
- Root signal-then-EOF probe → exit 2, not success.
- Root stdin probes keep input open until child exit → both signals exit 2.
- Actual local server smoke: named-file creation exit 0; stdin creation exit 0; unknown-field second document exit 2/no created output; final store count 2.
- Five review lanes complete: goal/code/security/QA pass; context confirms readiness semantics and records the two evidence qualifications above. All bind to full SHA `461cceaeffcb1d958cb395e7dd33c89d99d06c0f`.
- No implementation changes made. All root probe processes were joined/stopped, FIFO/regular-file fixtures auto-removed, and the root temporary executable was removed after use.
