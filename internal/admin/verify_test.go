package admin_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
)

func verifyClient(t *testing.T, deps admin.Deps) adminv1connect.VerifyServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewVerifyServiceClient(ts.Client(), ts.URL)
}

// verifyFixture records GetOrder calls for o-1, o-1, and o-2 (seqs 1–3) and a
// WatchOrder call (seq 4) that no GetOrder verification may count. testDeps'
// journal holds exactly these four.
func verifyFixture(t *testing.T) admin.Deps {
	t.Helper()
	deps := testDeps(t)
	for _, orderID := range []string{"o-1", "o-1", "o-2"} {
		recordGetOrder(t, deps, orderID)
	}
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/WatchOrder"})
	return deps
}

const orderOneMatcher = "message:\n  order_id: { eq: o-1 }\n"

func TestVerifyCallsPassesAndCountsOnlyItsMethod(t *testing.T) {
	client := verifyClient(t, verifyFixture(t))
	cases := []struct {
		name    string
		req     *adminv1.VerifyCallsRequest
		matched int32
	}{
		{"matcher, method without slash", &adminv1.VerifyCallsRequest{Method: "shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{Exactly: proto.Int32(2)}}, 2},
		{"empty matcher counts every call to the method", &adminv1.VerifyCallsRequest{
			Method: "/shop.v1.OrderService/GetOrder", Times: &adminv1.Times{Exactly: proto.Int32(3)}}, 3},
		{"never", &adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: "message:\n  order_id: { eq: o-9 }\n", Times: &adminv1.Times{Never: true}}, 0},
		{"range", &adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{AtLeast: proto.Int32(1), AtMost: proto.Int32(2)}}, 2},
	}
	for _, tc := range cases {
		resp, err := client.VerifyCalls(context.Background(), connect.NewRequest(tc.req))
		if err != nil {
			t.Fatalf("%s: VerifyCalls: %v", tc.name, err)
		}
		msg := resp.Msg
		if !msg.Passed || msg.Matched != tc.matched || len(msg.Actual) != 0 {
			t.Errorf("%s: passed/matched/actual = %v/%d/%d, want true/%d/0", tc.name, msg.Passed, msg.Matched, len(msg.Actual), tc.matched)
		}
		if !strings.HasPrefix(msg.Explanation, "passed: /shop.v1.OrderService/GetOrder") {
			t.Errorf("%s: explanation = %q, want it to start with the verdict and the normalized method", tc.name, msg.Explanation)
		}
	}
}

func TestVerifyCallsExplainsEachMissWhenTooFewMatch(t *testing.T) {
	resp, err := verifyClient(t, verifyFixture(t)).VerifyCalls(context.Background(),
		connect.NewRequest(&adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{Exactly: proto.Int32(3)}}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	msg := resp.Msg
	if msg.Passed || msg.Matched != 2 || len(msg.Actual) != 1 {
		t.Fatalf("passed/matched/actual = %v/%d/%d, want false/2/1", msg.Passed, msg.Matched, len(msg.Actual))
	}
	for _, want := range []string{"failed:", "matched 2 of 3", "exactly 3"} {
		if !strings.Contains(msg.Explanation, want) {
			t.Errorf("explanation = %q, want it to contain %q", msg.Explanation, want)
		}
	}
	miss := msg.Actual[0]
	if miss.Call.Seq != 3 || jsonFields(t, miss.Call.Requests[0].Json)["order_id"] != "o-2" {
		t.Errorf("actual call = %v, want seq 3 carrying order_id o-2", miss.Call)
	}
	if want := `message order_id: expected to equal "o-1"; actual "o-2"`; miss.NearestMiss != want {
		t.Errorf("nearest_miss = %q, want %q", miss.NearestMiss, want)
	}
}

func TestVerifyCallsListsTheMatchesWhenTooManyMatch(t *testing.T) {
	resp, err := verifyClient(t, verifyFixture(t)).VerifyCalls(context.Background(),
		connect.NewRequest(&adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrder",
			MatcherDocument: orderOneMatcher, Times: &adminv1.Times{AtMost: proto.Int32(1)}}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	msg := resp.Msg
	if msg.Passed || msg.Matched != 2 {
		t.Fatalf("passed/matched = %v/%d, want false/2", msg.Passed, msg.Matched)
	}
	var seqs []uint64
	for _, actual := range msg.Actual {
		seqs = append(seqs, actual.Call.Seq)
		if actual.NearestMiss != "" {
			t.Errorf("matching call %d has nearest_miss %q, want empty: it matched", actual.Call.Seq, actual.NearestMiss)
		}
	}
	if !reflect.DeepEqual(seqs, []uint64{1, 2}) {
		t.Errorf("actual seqs = %v, want [1 2], the calls that matched", seqs)
	}
}

func TestVerifyCallsRejectsBadRequests(t *testing.T) {
	const getOrder = "/shop.v1.OrderService/GetOrder"
	never := &adminv1.Times{Never: true}
	cases := []struct {
		name string
		req  *adminv1.VerifyCallsRequest
		code connect.Code
		want string
	}{
		{"no method", &adminv1.VerifyCallsRequest{Times: never}, connect.CodeInvalidArgument, "method is required"},
		{"no times", &adminv1.VerifyCallsRequest{Method: getOrder}, connect.CodeInvalidArgument, "one times assertion is required"},
		{"contradictory times", &adminv1.VerifyCallsRequest{Method: getOrder,
			Times: &adminv1.Times{Never: true, Exactly: proto.Int32(0)}}, connect.CodeInvalidArgument, "never cannot be combined"},
		{"unknown matcher field", &adminv1.VerifyCallsRequest{Method: getOrder, MatcherDocument: "bogus: 1\n", Times: never},
			connect.CodeInvalidArgument, "bogus"},
		{"bad field path", &adminv1.VerifyCallsRequest{Method: getOrder,
			MatcherDocument: "message:\n  no_such: { eq: x }\n", Times: never}, connect.CodeInvalidArgument, "no_such"},
		{"malformed method", &adminv1.VerifyCallsRequest{Method: "garbage", Times: never},
			connect.CodeInvalidArgument, "invalid method name"},
		// The typo guard: without it, never would pass against a method nothing calls.
		{"mistyped method with never", &adminv1.VerifyCallsRequest{Method: "/shop.v1.OrderService/GetOrdr", Times: never},
			connect.CodeNotFound, `method "GetOrdr" not found`},
		{"unregistered service", &adminv1.VerifyCallsRequest{Method: "/no.such.Service/Get", Times: never},
			connect.CodeNotFound, `service "no.such.Service" is not registered`},
	}
	client := verifyClient(t, verifyFixture(t))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.VerifyCalls(context.Background(), connect.NewRequest(tc.req))
			if code := connect.CodeOf(err); code != tc.code {
				t.Fatalf("VerifyCalls = %v (code %v), want %v", err, code, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// malformedMethods cannot be protobuf identifiers, so no registered schema
// could ever declare them.
var malformedMethods = []string{
	"shop.v1.OrderService/Get Order",
	"shop.v1.OrderService/1",
	"bad service/GetOrder",
	"shop.v1.OrderService/extra/GetOrder",
	"//shop.v1.OrderService/GetOrder",
}

// A malformed method name is the caller's mistake, not a missing schema.
// NOT_FOUND would send an SDK off to register a schema that cannot exist.
func TestVerifyCallsRejectsMalformedMethodIdentifiers(t *testing.T) {
	client := verifyClient(t, verifyFixture(t))
	for _, method := range malformedMethods {
		_, err := client.VerifyCalls(context.Background(), connect.NewRequest(&adminv1.VerifyCallsRequest{
			Method: method,
			Times:  &adminv1.Times{Never: true},
		}))
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
			t.Errorf("VerifyCalls(%q) = %v (code %v), want InvalidArgument", method, err, code)
		}
	}
}
