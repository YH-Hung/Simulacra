# Conformance corpus

This package proves the whole pipeline — schema load → request decode →
match → template → response encode → client decode — with a real grpc-go
client, and is the source of truth for the public support matrix
(`SUPPORT.md`, generated at M4 per PROPOSAL.md §7).

## Feature IDs

Every corpus subtest is named by a stable feature ID (`wkt.timestamp`,
`any.registered`, `presence.oneof`, …). The M4 generator maps test results
to matrix states:

| State | Meaning |
|---|---|
| ✅ Supported | in the corpus, passing, covered by semver |
| ❌ Not supported | documented failing behavior, with an issue link |
| ⬜ Untested | **not claimed** — treated as unsupported until it enters the corpus |

The policy is one line: **if it isn't tested, it isn't claimed.**

## Current coverage (M2, first pass)

- `shapes.*` — stub + call + verify across unary, server-, client-, and
  bidirectional streaming; typed error details; metadata; deadlines
- `int64.precision`, `bytes.roundtrip`
- `presence.optional.set` / `presence.optional.unset` / `presence.oneof`
- `wkt.timestamp` / `wkt.duration` / `wkt.wrappers` / `wkt.struct` / `wkt.fieldmask`
- `map.message_values`, `repeated.packed`, `structural.recursive`
- `any.registered`, `any.unregistered` (type-URL matching + opaque bytes,
  load-time rejection when building)
- `unknown_fields.preserved`
- `proto2.required_and_defaults`, `proto2.extensions`
- `editions.basic`

Known M2 limits (documented, not silent): no `messages` templating of
lists/maps as template *results*; status details are static; bidi stubs
select on metadata only at stream open.
