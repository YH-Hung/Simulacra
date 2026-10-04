package simulacratest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

// callGetOrder stubs GetOrder and calls it once per id.
func callGetOrder(t *testing.T, sim *simulacratest.Server, ids ...string) {
	t.Helper()
	sim.Stub(getOrder).Respond(t, &shopv1.GetOrderResponse{})
	orders := shopv1.NewOrderServiceClient(sim.Dial(t))
	for _, id := range ids {
		if _, err := orders.GetOrder(t.Context(), &shopv1.GetOrderRequest{OrderId: id}); err != nil {
			t.Fatalf("GetOrder(%q): %v", id, err)
		}
	}
}

func TestVerifyPassesAndFails(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1", "o-1")
	verify := func() *simulacratest.VerifyBuilder {
		return sim.Verify(getOrder).MatchField("order_id", simulacratest.Eq("o-1"))
	}

	if !verify().Exactly(t, 2) {
		t.Fatalf("Exactly(2) = false after two matching calls; want true")
	}

	var ok bool
	rec := runRecorded(t, func(tb testing.TB) { ok = verify().Exactly(tb, 3) })

	if ok {
		t.Errorf("Exactly(3) = true after two matching calls; want false")
	}
	if rec.FatalMsg() != "" {
		t.Errorf("FatalMsg() = %q; want a failed verification to leave the test running", rec.FatalMsg())
	}
	if got := rec.Errors(); len(got) != 1 {
		t.Errorf("Errors() = %q; want exactly one", got)
	}
}

// Two calls are journaled, so each case's verdict pins which Times fields the
// builder sent.
func TestVerifyTimes(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1", "o-2")
	for _, tc := range []struct {
		name string
		do   func(tb testing.TB, b *simulacratest.VerifyBuilder) bool
		want bool
	}{
		{"Exactly 2", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.Exactly(tb, 2) }, true},
		{"Exactly 1", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.Exactly(tb, 1) }, false},
		{"AtLeast 1", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.AtLeast(tb, 1) }, true},
		{"AtLeast 2", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.AtLeast(tb, 2) }, true},
		{"AtLeast 3", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.AtLeast(tb, 3) }, false},
		{"AtMost 3", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.AtMost(tb, 3) }, true},
		{"AtMost 2", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.AtMost(tb, 2) }, true},
		{"AtMost 1", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.AtMost(tb, 1) }, false},
		{"Between 1 and 3", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.Between(tb, 1, 3) }, true},
		{"Between 3 and 4", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.Between(tb, 3, 4) }, false},
		{"Between 0 and 1", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.Between(tb, 0, 1) }, false},
		{"Never", func(tb testing.TB, b *simulacratest.VerifyBuilder) bool { return b.Never(tb) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			rec := runRecorded(t, func(tb testing.TB) { got = tc.do(tb, sim.Verify(getOrder)) })
			if got != tc.want || rec.FatalMsg() != "" {
				t.Errorf("verification = %v (fatal %q); want %v", got, rec.FatalMsg(), tc.want)
			}
		})
	}
}

func TestVerifyFailureTextIsTheServers(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1", "o-2")
	const doc = "message: { order_id: { eq: o-9 } }"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	direct, err := sim.Admin().Verify.VerifyCalls(ctx, connect.NewRequest(&adminv1.VerifyCallsRequest{
		Method:          getOrder,
		MatcherDocument: doc,
		Times:           &adminv1.Times{Exactly: proto.Int32(1)},
	}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	resp := direct.Msg
	if resp.GetPassed() || len(resp.GetActual()) != 2 {
		t.Fatalf("VerifyCalls = %v; want a failure listing both calls", resp)
	}

	rec := runRecorded(t, func(tb testing.TB) { sim.Verify(getOrder).MatchYAML(doc).Exactly(tb, 1) })

	errs := rec.Errors()
	if len(errs) != 1 {
		t.Fatalf("Errors() = %q; want exactly one", errs)
	}
	got := errs[0]
	if want := "simulacratest: Verify " + getOrder + ": " + resp.GetExplanation() + "\n"; !strings.HasPrefix(got, want) {
		t.Errorf("failure = %q; want it to start %q", got, want)
	}
	for _, a := range resp.GetActual() {
		if a.GetNearestMiss() == "" {
			t.Fatalf("server gave no nearest miss for call %d; the test would prove nothing", a.GetCall().GetSeq())
		}
		var reqs []string
		for _, r := range a.GetCall().GetRequests() {
			reqs = append(reqs, r.GetJson())
		}
		for _, want := range []string{
			fmt.Sprintf("\n  call seq=%d: %s\n", a.GetCall().GetSeq(), strings.Join(reqs, ", ")),
			"\n    nearest miss: " + a.GetNearestMiss(),
		} {
			if !strings.Contains(got, want) {
				t.Errorf("failure = %q; want it to contain %q", got, want)
			}
		}
	}
}

// When too many calls match, the server lists the matching calls with nothing
// to explain, so the report has no nearest-miss line to print for them.
func TestVerifyOverCountListsMatchedCallsWithoutANearestMiss(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1", "o-1")
	const doc = "message: { order_id: { eq: o-1 } }"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	direct, err := sim.Admin().Verify.VerifyCalls(ctx, connect.NewRequest(&adminv1.VerifyCallsRequest{
		Method:          getOrder,
		MatcherDocument: doc,
		Times:           &adminv1.Times{Never: true},
	}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	resp := direct.Msg
	if resp.GetPassed() || len(resp.GetActual()) != 2 {
		t.Fatalf("VerifyCalls = %v; want a failure listing both matched calls", resp)
	}
	want := "simulacratest: Verify " + getOrder + ": " + resp.GetExplanation()
	for _, a := range resp.GetActual() {
		if a.GetNearestMiss() != "" {
			t.Fatalf("server gave nearest miss %q for a matched call; the test would prove nothing", a.GetNearestMiss())
		}
		var reqs []string
		for _, r := range a.GetCall().GetRequests() {
			reqs = append(reqs, r.GetJson())
		}
		want += fmt.Sprintf("\n  call seq=%d: %s", a.GetCall().GetSeq(), strings.Join(reqs, ", "))
	}

	rec := runRecorded(t, func(tb testing.TB) {
		sim.Verify(getOrder).MatchField("order_id", simulacratest.Eq("o-1")).Never(tb)
	})

	if got := rec.Errors(); !slices.Equal(got, []string{want}) {
		t.Errorf("Errors() = %q; want [%q]", got, want)
	}
	if got := rec.Errors(); len(got) == 1 && strings.Contains(got[0], "nearest miss:") {
		t.Errorf("failure = %q; want no nearest-miss line for calls that matched", got[0])
	}
}

// fakeVerify answers every VerifyCalls with resp.
type fakeVerify struct {
	adminv1connect.VerifyServiceClient
	resp *adminv1.VerifyCallsResponse
}

func (f fakeVerify) VerifyCalls(context.Context, *connect.Request[adminv1.VerifyCallsRequest]) (*connect.Response[adminv1.VerifyCallsResponse], error) {
	return connect.NewResponse(f.resp), nil
}

func TestVerifyFailureForACallWithoutRequests(t *testing.T) {
	sim := simulacratest.Start(t)
	simulacratest.WrapAdmin(sim.Client, func(a simulacratest.Admin) simulacratest.Admin {
		a.Verify = fakeVerify{a.Verify, &adminv1.VerifyCallsResponse{
			Explanation: "failed: x",
			Actual: []*adminv1.CallExplanation{
				{Call: &adminv1.Call{Seq: 7}, NearestMiss: "no match"},
			},
		}}
		return a
	})

	rec := runRecorded(t, func(tb testing.TB) { sim.Verify(getOrder).Never(tb) })

	want := "simulacratest: Verify " + getOrder + ": failed: x\n  call seq=7: (no request)\n    nearest miss: no match"
	if got := rec.Errors(); !slices.Equal(got, []string{want}) {
		t.Errorf("Errors() = %q; want [%q]", got, want)
	}
}

func TestVerifyAndStubRenderTheSameMatchBlock(t *testing.T) {
	sim := simulacratest.Start(t)
	stub := sim.Stub(getOrder).
		MatchField("order_id", simulacratest.Eq("o-1"), simulacratest.Ne("o-2")).
		MatchField("customer.region", simulacratest.In(shopv1.Region_EU, shopv1.Region_UK)).
		MatchMetadata("authorization", simulacratest.Present(true)).
		Expr("size(message.items) <= 10")
	verify := sim.Verify(getOrder).
		MatchField("order_id", simulacratest.Eq("o-1"), simulacratest.Ne("o-2")).
		MatchField("customer.region", simulacratest.In(shopv1.Region_EU, shopv1.Region_UK)).
		MatchMetadata("authorization", simulacratest.Present(true)).
		Expr("size(message.items) <= 10")

	got, want := simulacratest.VerifyMatchJSON(verify), simulacratest.StubMatchJSON(stub)

	if got != want || got == "" || got == "null" {
		t.Errorf("VerifyMatchJSON = %s; want the stub's %s", got, want)
	}
}

func TestVerifyAutoRegistersAndCountsNever(t *testing.T) {
	sim := simulacratest.Start(t)
	if got := listedServices(t, sim.Client); slices.Contains(got, "shop.v1.OrderService") {
		t.Fatalf("a schemaless server already lists %v", got)
	}

	if !sim.Verify(getOrder).Never(t) {
		t.Errorf("Never = false on an empty journal; want true")
	}

	if got := listedServices(t, sim.Client); !slices.Contains(got, "shop.v1.OrderService") {
		t.Errorf("ListServices after Verify = %v; want it to list shop.v1.OrderService", got)
	}
}

func TestVerifyRefusesMixingMatchYAML(t *testing.T) {
	sim := simulacratest.Start(t)
	const doc = "message: { order_id: { eq: o-1 } }"
	const want = "simulacratest: Verify: MatchYAML cannot be combined with MatchField, MatchMetadata or Expr"
	for name, build := range map[string]func() *simulacratest.VerifyBuilder{
		"MatchField": func() *simulacratest.VerifyBuilder {
			return sim.Verify(getOrder).MatchYAML(doc).MatchField("a", simulacratest.Eq("x"))
		},
		"MatchMetadata": func() *simulacratest.VerifyBuilder {
			return sim.Verify(getOrder).MatchYAML(doc).MatchMetadata("a", simulacratest.Present(true))
		},
		"Expr": func() *simulacratest.VerifyBuilder { return sim.Verify(getOrder).MatchYAML(doc).Expr("true") },
		"Expr before MatchYAML": func() *simulacratest.VerifyBuilder {
			return sim.Verify(getOrder).Expr("true").MatchYAML(doc)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := runRecorded(t, func(tb testing.TB) { build().Never(tb) })
			if rec.FatalMsg() != want {
				t.Errorf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
			}
		})
	}
}

func TestVerifyReportsMatcherMisuse(t *testing.T) {
	sim := simulacratest.Start(t)

	rec := runRecorded(t, func(tb testing.TB) {
		sim.Verify(getOrder).MatchField("f", simulacratest.Eq([]byte("x"))).Never(tb)
	})

	if want := `simulacratest: MatchField("f"): []uint8 is not supported by structured matchers; use Expr`; rec.FatalMsg() != want {
		t.Errorf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

// countingVerify counts VerifyCalls, so a test can assert a refusal sent none.
type countingVerify struct {
	adminv1connect.VerifyServiceClient
	calls int
}

func (c *countingVerify) VerifyCalls(ctx context.Context, req *connect.Request[adminv1.VerifyCallsRequest]) (*connect.Response[adminv1.VerifyCallsResponse], error) {
	c.calls++
	return c.VerifyServiceClient.VerifyCalls(ctx, req)
}

// A matcher string the YAML hop would alter matches something else on the
// server: Eq("a\u0085b") arrives as "a b", so Never would pass after a call
// carrying "a\u0085b". Every such string is refused before any RPC.
func TestVerifyRefusesStringsTheYAMLHopWouldAlter(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "a\u0085b")
	counter := &countingVerify{}
	simulacratest.WrapAdmin(sim.Client, func(a simulacratest.Admin) simulacratest.Admin {
		counter.VerifyServiceClient = a.Verify
		a.Verify = counter
		return a
	})
	const tail = " does not survive the stub grammar's JSON round trip unchanged (invalid UTF-8 or a raw U+0085, for example); use MatchYAML or StubYAML for full control"
	value := func(kind, key string) string { return fmt.Sprintf("simulacratest: %s(%q): value", kind, key) + tail }
	for _, tc := range []struct {
		name string
		b    *simulacratest.VerifyBuilder
		want string
	}{
		{"Eq raw U+0085", sim.Verify(getOrder).MatchField("order_id", simulacratest.Eq("a\u0085b")), value("MatchField", "order_id")},
		{"Eq invalid UTF-8", sim.Verify(getOrder).MatchField("order_id", simulacratest.Eq("a\xffb")), value("MatchField", "order_id")},
		{"In element", sim.Verify(getOrder).MatchField("order_id", simulacratest.In("x", "a\u0085b")), value("MatchField", "order_id")},
		{"Matches", sim.Verify(getOrder).MatchMetadata("k", simulacratest.Matches("a\u0085")), value("MatchMetadata", "k")},
		{"path", sim.Verify(getOrder).MatchField("a\u0085b", simulacratest.Present(true)),
			fmt.Sprintf("simulacratest: MatchField(%q): the path", "a\u0085b") + tail},
		{"metadata key", sim.Verify(getOrder).MatchMetadata("k\xff", simulacratest.Present(true)),
			fmt.Sprintf("simulacratest: MatchMetadata(%q): the key", "k\xff") + tail},
		{"Expr", sim.Verify(getOrder).Expr("message.order_id == 'a\u0085b'"), "simulacratest: Expr: the expression" + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ok bool
			rec := runRecorded(t, func(tb testing.TB) { ok = tc.b.Never(tb) })
			if ok || rec.FatalMsg() != tc.want {
				t.Errorf("Never = %v, FatalMsg() = %q; want false, %q", ok, rec.FatalMsg(), tc.want)
			}
		})
	}
	if counter.calls != 0 {
		t.Errorf("VerifyCalls sent %d times; want none", counter.calls)
	}
}

// A count Times cannot hold would narrow silently: 1<<32 is sent as 0.
func TestVerifyRefusesACountOutsideInt32(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1")
	var ok bool
	rec := runRecorded(t, func(tb testing.TB) { ok = sim.Verify(getOrder).AtLeast(tb, 1<<32) })
	if want := "simulacratest: Verify: count 4294967296 is outside 0..2147483647"; ok || rec.FatalMsg() != want {
		t.Errorf("AtLeast(1<<32) = %v, FatalMsg() = %q; want false, %q", ok, rec.FatalMsg(), want)
	}
}

func TestVerifyNotFoundGetsTheHintOnlyForAnUnknownService(t *testing.T) {
	sim := simulacratest.Start(t)
	serverError := func(method string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := sim.Admin().Verify.VerifyCalls(ctx, connect.NewRequest(&adminv1.VerifyCallsRequest{
			Method: method, Times: &adminv1.Times{Never: true},
		}))
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("VerifyCalls(%s) error = %v; want NOT_FOUND", method, err)
		}
		return err.Error()
	}
	const hint = "\nsimulacratest: hint: the test binary does not link generated code for nope.v1.Missing; register its schema with RegisterFiles or WithProtoDir"

	unknown := runRecorded(t, func(tb testing.TB) { sim.Verify("/nope.v1.Missing/Get").Never(tb) })
	mistyped := runRecorded(t, func(tb testing.TB) { sim.Verify("/shop.v1.OrderService/NoSuch").Never(tb) })

	if want := "simulacratest: VerifyCalls: " + serverError("/nope.v1.Missing/Get") + hint; unknown.FatalMsg() != want {
		t.Errorf("unknown service FatalMsg() = %q; want %q", unknown.FatalMsg(), want)
	}
	if want := "simulacratest: VerifyCalls: " + serverError("/shop.v1.OrderService/NoSuch"); mistyped.FatalMsg() != want {
		t.Errorf("mistyped method FatalMsg() = %q; want %q, with no hint", mistyped.FatalMsg(), want)
	}
}
