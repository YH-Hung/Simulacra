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
- `presence.optional.set` / `presence.optional.unset` / `presence.oneof` /
  `presence.message_vs_scalar`
- `wkt.timestamp` / `wkt.duration` / `wkt.wrappers` / `wkt.struct` /
  `wkt.value` / `wkt.fieldmask`; templated natural Struct/Value/ListValue
  forms are covered by `wkt.struct_value_listvalue.template`
- `map.message_values`, `repeated.packed`, `structural.recursive`,
  `structural.large_message`
- `any.registered`, `any.registered.templated`, `any.unregistered`
  (registry-first type resolution, template repacking, type-URL matching +
  opaque bytes, and load-time rejection when building)
- `unknown_fields.preserved`
- `proto2.required_and_defaults`, `proto2.extensions`, `proto2.groups`
- `editions.basic`

Known M2 limits (documented, not silent): no `messages` templating of
lists/maps as template *results*; status details are static; bidi stubs
select on metadata only at stream open.
