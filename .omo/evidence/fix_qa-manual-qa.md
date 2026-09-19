# Manual QA matrix

Verified worktree SHA: `aaf0c098b2f3d3814c036be4a3cca900957b2a2e`

## surfaceEvidence

| scenario id | criterion reference | surface | exact invocation | verdict | artifactRefs |
|---|---|---|---|---|---|
| CLI-001 | TestStubAddPreflightSendsNoRPCsForAnUnknownField | Go CLI package test | `env GOCACHE=/private/tmp/simulacra-review-gocache go test ./internal/cli -race -count=1 -run 'TestStubAddPreflightSendsNoRPCsForAnUnknownField\|TestSchemaListReportsAFailedPayloadWrite\|TestStubAddReportsAFailedPayloadWrite\|TestVerifyReportsAFailedVerdictWriteAsOperational\|TestSchemaRegisterOfAnAlreadyRegisteredSetReportsNoNewFiles\|TestInterruptedStdinReadExitsTwo\|TestSIGTERMDuringStdinReadExitsTwo\|TestCallsTailExitsZeroOnRealSIG'` | PASS | run-log |
| CLI-002 | TestSchemaListReportsAFailedPayloadWrite | Go CLI package test | same exact invocation above | PASS | run-log |
| CLI-003 | TestStubAddReportsAFailedPayloadWrite | Go CLI package test | same exact invocation above | PASS | run-log |
| CLI-004 | TestVerifyReportsAFailedVerdictWriteAsOperational | Go CLI package test | same exact invocation above | PASS | run-log |
| CLI-005 | TestSchemaRegisterOfAnAlreadyRegisteredSetReportsNoNewFiles | Go CLI package test | same exact invocation above | PASS | run-log |
| CLI-006 | TestInterruptedStdinReadExitsTwo | Go CLI package test | same exact invocation above | PASS | run-log |
| CLI-007 | TestSIGTERMDuringStdinReadExitsTwo | Go CLI package test | same exact invocation above | PASS | run-log |
| CLI-008 | TestCallsTailExitsZeroOnRealSIG | Go CLI package test | same exact invocation above | PASS | run-log |

## adversarialCases

| scenario id | criterion reference | adversarial class | expected behavior | verdict | artifactRefs |
|---|---|---|---|---|---|
| ADV-001 | CLI-001 | unknown-field preflight | Reject unknown field without sending RPCs | PASS | run-log |
| ADV-002 | CLI-002, CLI-003, CLI-004 | failed payload/verdict writes | Report operational write failure | PASS | run-log |
| ADV-003 | CLI-005 | already-registered schema set | Report no new files | PASS | run-log |
| ADV-004 | CLI-006, CLI-007 | interrupted stdin and SIGTERM | Exit with status 2 | PASS | run-log |
| ADV-005 | CLI-008 | real signal during calls tail | Exit zero on real SIG | PASS | run-log |

## artifactRefs

| id | kind | description | path |
|---|---|---|---|
| run-log | test-output | Exact focused command, verified SHA, escalation note, and passing output | `/Users/yinghanhung/Projects/Simulacra/.omo/evidence/fix_qa-focused-cli-run.txt` |
