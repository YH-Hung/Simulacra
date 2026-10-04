package simulacratest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// VerifyBuilder accumulates one verification. Start one with Client.Verify and
// finish it with Exactly, AtLeast, AtMost, Between or Never, which run it.
// Builder methods take no testing.TB: misuse is collected and reported once,
// by the call that runs.
type VerifyBuilder struct {
	c      *Client
	method string
	match  matchSpec // the same accumulator StubBuilder uses

	yaml       string
	hasYAML    bool
	structured bool // MatchField, MatchMetadata or Expr was called
}

// Verify starts a verification of the calls made to method, given as
// "/pkg.Svc/M" or "pkg.Svc/M". Without a matcher it counts every call.
func (c *Client) Verify(method string) *VerifyBuilder { return &VerifyBuilder{c: c, method: method} }

// MatchField counts only calls whose request field at path satisfies ms. path
// is dotted for nesting; calls on one path merge their operators.
func (b *VerifyBuilder) MatchField(path string, ms ...Matcher) *VerifyBuilder {
	b.structured = true
	b.match.add("MatchField", path, ms)
	return b
}

// MatchMetadata counts only calls whose request metadata key satisfies ms.
func (b *VerifyBuilder) MatchMetadata(key string, ms ...Matcher) *VerifyBuilder {
	b.structured = true
	b.match.add("MatchMetadata", key, ms)
	return b
}

// Expr counts only calls for which the CEL expression holds. It is sent
// verbatim.
func (b *VerifyBuilder) Expr(cel string) *VerifyBuilder {
	b.structured = true
	b.match.setExpr(cel)
	return b
}

// MatchYAML counts only calls matching doc, a bare match block in the stub
// file grammar (message:, metadata:, expr:). It cannot be combined with
// MatchField, MatchMetadata or Expr.
func (b *VerifyBuilder) MatchYAML(doc string) *VerifyBuilder {
	b.yaml, b.hasYAML = doc, true
	return b
}

// Exactly passes when exactly n calls match.
func (b *VerifyBuilder) Exactly(t testing.TB, n int) bool {
	t.Helper()
	return b.run(t, &adminv1.Times{Exactly: proto.Int32(int32(n))})
}

// AtLeast passes when n or more calls match.
func (b *VerifyBuilder) AtLeast(t testing.TB, n int) bool {
	t.Helper()
	return b.run(t, &adminv1.Times{AtLeast: proto.Int32(int32(n))})
}

// AtMost passes when n or fewer calls match.
func (b *VerifyBuilder) AtMost(t testing.TB, n int) bool {
	t.Helper()
	return b.run(t, &adminv1.Times{AtMost: proto.Int32(int32(n))})
}

// Between passes when lo to hi calls match, inclusive.
func (b *VerifyBuilder) Between(t testing.TB, lo, hi int) bool {
	t.Helper()
	return b.run(t, &adminv1.Times{AtLeast: proto.Int32(int32(lo)), AtMost: proto.Int32(int32(hi))})
}

// Never passes when no call matches.
func (b *VerifyBuilder) Never(t testing.TB) bool {
	t.Helper()
	return b.run(t, &adminv1.Times{Never: true})
}

// matcherDocument is the bare match block the server parses: MatchYAML's text,
// else the structured matchers as JSON (a YAML subset, and the encoder the stub
// builder uses), else "", which the server reads as every call to the method.
func (b *VerifyBuilder) matcherDocument() string {
	if b.hasYAML {
		return b.yaml
	}
	block := b.match.block()
	if block == nil {
		return ""
	}
	// Cannot fail: matcherValue renders only strings, bools, integers and
	// finite floats.
	doc, _ := json.Marshal(block)
	return string(doc)
}

// run asks the server whether the journal satisfies times and reports a failed
// verification with t.Errorf, so the test goes on to show every failure. The
// text is the server's: its verdict, then each call it lists with the request
// JSON and the reason it missed, unchanged.
func (b *VerifyBuilder) run(t testing.TB, times *adminv1.Times) bool {
	t.Helper()
	errs := b.match.errors()
	if b.hasYAML && b.structured {
		errs = slices.Concat(errs, []string{"simulacratest: Verify: MatchYAML cannot be combined with MatchField, MatchMetadata or Expr"})
	}
	if len(errs) > 0 {
		t.Fatalf("%s", strings.Join(errs, "\n"))
	}
	b.c.ensure(t, b.method)
	ctx, cancel := b.c.rpcContext()
	defer cancel()
	resp, err := b.c.admin.Verify.VerifyCalls(ctx, connect.NewRequest(&adminv1.VerifyCallsRequest{
		Method:          b.method,
		MatcherDocument: b.matcherDocument(),
		Times:           times,
	}))
	if err != nil {
		var extra []string
		if connect.CodeOf(err) == connect.CodeNotFound {
			extra = b.c.notFoundHintFor(b.method)
		}
		b.c.fatalRPC(t, "VerifyCalls", err, extra...)
	}
	if resp.Msg.GetPassed() {
		return true
	}
	full, _, _, ok := normalizeMethod(b.method)
	if !ok {
		full = b.method
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "simulacratest: Verify %s: %s", full, resp.Msg.GetExplanation())
	for _, a := range resp.Msg.GetActual() {
		var requests []string
		for _, r := range a.GetCall().GetRequests() {
			requests = append(requests, r.GetJson())
		}
		shown := "(no request)"
		if len(requests) > 0 {
			shown = strings.Join(requests, ", ")
		}
		fmt.Fprintf(&msg, "\n  call seq=%d: %s\n    nearest miss: %s", a.GetCall().GetSeq(), shown, a.GetNearestMiss())
	}
	t.Errorf("%s", msg.String())
	return false
}
