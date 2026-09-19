# Phase 5 CLI client commands: consolidated review

Verdict: REQUEST CHANGES. The functional command surface is implemented, but three reproduced contract gaps prevent full satisfaction.

Reviewed commit: `733574367b3c41ac20f4141b6fb83d8838e7faff`; implementation base `c989faa`.
Review included the original plan, design, and final post-execution amendments. No production changes were made.
This report supersedes the earlier goal lane's blanket approval: the root's subsequent runtime probes exposed cases that the existing tests do not cover.

## Findings

### P2 — Strict stub grammar errors escape preflight

Location: `internal/cli/stub.go:336-344`.
`preflightStubs` only calls `SplitDocuments`, which checks YAML syntax and sequence shape. The strict decode used by the file loader and `stub.ParseDocument` is missing. An unknown field or scalar list element therefore reaches CreateStub. This violates design §6.1's zero-RPC promise for files that do not parse as the local stub grammar.

Reproduced against a real server with a two-item file: valid GetOrder stub, then `not_a_stub_field: true`. stdout reported `created api-1`, followed by server INVALID_ARGUMENT and compensation. The final store was empty, but the invalid input had already mutated it; concurrent requests or failed compensation make this materially different from preflight atomicity.

Fix: strictly parse every normalized document before any RPC, while leaving descriptor-dependent and CEL checks on the server. Add the malformed sequence-item/unknown-field cases to the counting-client preflight test.

### P2 — Signals during input preflight violate the exit contract

Location: `internal/cli/stub.go:269-278` (blocking stdin read at line 327).
The signal context is installed only after all preflight file reads. With `stub add -f -` waiting on open stdin, a real SIGINT exits 130 and SIGTERM exits 143, both without the required operational-error diagnostic/exit 2. The same ordering occurs before schema file and verify matcher reads.

Fix: establish command-level signal handling before blocking input and make the input path cancellation-aware. Moving NotifyContext alone is insufficient because io.ReadAll does not observe context cancellation. Add subprocess tests that interrupt an incomplete stdin stream.

### P2 — Text payload write errors are discarded

Locations: `internal/cli/verify.go:73-76`, `internal/cli/schema.go:109-117`, `internal/cli/calls.go:124-128`; corresponding writes in schema register and stub add/rm also ignore errors.
Text paths discard fmt write errors, then return success or assertion failure. This can conceal a full output filesystem or failed output descriptor. The JSON and export render paths already propagate these failures.

Reproduced against the same real server with stdout bound to a read-only descriptor:

| Command | Text exit | JSON exit |
|---|---:|---:|
| schema list | 0, no diagnostic | 2, bad file descriptor |
| verify --times never (passes) | 0, no verdict delivered | 2, bad file descriptor |
| verify --times exactly=1 (fails) | 1, no verdict delivered | 2, bad file descriptor |

Fix: propagate payload write errors and stop streaming on sink failure. For mutating commands, preserve an accurate account of completed mutations when reporting output failure. Add failing-writer tests.

## Plan coverage

| Requirement | Assessment |
|---|---|
| All planned stub/calls/verify/schema commands and root registration | Implemented |
| Shared client, address precedence, unary timeout, text/protojson/NDJSON formats | Implemented; text sink errors need correction |
| Exit contract and signal handling | Implemented for RPC phase; preflight interruption gap |
| Stub preflight and best-effort compensation | Compensation implemented, including lost response warning; strict preflight incomplete |
| Schema merging, verify int32 parsing, filters, export round trip, tail lifecycle | Implemented and covered by tests/QA |
| No frozen contract/module/server/data-plane changes | Satisfied; protected-path diff empty |
| Plan's manual mutation checks | Historical execution unverified: no retained mutation-run evidence found |

## Verification

- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `go test ./... -race -count=1 -timeout 900s`: PASS across all packages, after enabling localhost bind permissions for tests. GOCACHE was redirected to `/private/tmp/simulacra-review-gocache`.
- API lint: PASS with repository-pinned `bin/buf lint`; goal lane additionally ran `make lint-api` successfully.
- `git diff --check`: PASS.
- Protected-path diff from `74b74d6`: empty.
- Root used the built binary against a fresh actual local server, verified malformed-input mutations, output sink failures, and actual subprocess signals.
- Full race output: `/private/tmp/simulacra-review-race.log`.
- Runtime/lane ledger: `/private/tmp/simulacra-cli-review-ledger.md`.

## Reviewer reconciliation

Five parallel review lanes were used as required by review-work. Goal lane initially approved the regular paths; its conclusions are superseded for the reproduced edge cases above. Context lane independently confirmed the strict-preflight gap. Code lane corroborated the root-owned issues and found no separate proven blocker.

The security lane proposed a receive-size limit. The approved design explicitly allows uncapped Connect responses to support large exports and journal responses, so this is a future hardening tradeoff, not a Phase 5 implementation failure. A clean-EOF tail scenario was also excluded from blocking findings because the current server does not produce it in ordinary operation.

Additional test coverage worth adding: dedicated schema re-registration asserting `0 new file(s)`, and explicit RPC counting for all-or-nothing schema registration. The plan amendments already acknowledge the latter.
