package stub

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/yinghanhung/simulacra/internal/match"
)

func TestCompileServerStreamingPlan(t *testing.T) {
	reg := testRegistry(t)
	c, err := Compile(reg, Stub{
		Method: "shop.v1.OrderService/WatchOrder",
		Respond: Respond{Delay: "2s", Stream: []StepSpec{
			{Message: map[string]any{"status": "ORDER_STATUS_PENDING"}},
			{Delay: "25ms", Message: map[string]any{"status": "ORDER_STATUS_SHIPPED"}},
			{Status: &StatusSpec{Code: "UNAVAILABLE", Message: "backend hiccup"}},
		}},
	}, "stream.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Shape != match.ServerStream || len(c.Plan().Stream) != 3 {
		t.Fatalf("Shape/stream = %v/%d, want server-streaming/3", c.Shape, len(c.Plan().Stream))
	}
	if got := c.Plan().Stream[1].Delay; got == nil || got.Min != 25*time.Millisecond {
		t.Errorf("second delay = %#v, want 25ms", got)
	}
	if got := c.Plan().Delay; got == nil || got.Min != 2*time.Second || got.Max != 2*time.Second {
		t.Errorf("top-level delay = %#v, want 2s", got)
	}
	if got := c.Plan().Stream[2].Status; got == nil || got.Code() != codes.Unavailable {
		t.Errorf("terminal status = %v, want UNAVAILABLE", got)
	}
}

func TestCompileClientStreamingPlan(t *testing.T) {
	reg := testRegistry(t)
	c, err := Compile(reg, Stub{
		Method: "shop.v1.OrderService/UploadOrders",
		Match:  &match.Block{Expr: "size(messages) == 2"},
		Respond: Respond{Message: map[string]any{
			"note": "got {{ size(messages) }}",
		}},
	}, "upload.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Shape != match.ClientStream || c.Plan().Message == nil {
		t.Fatalf("Shape/message = %v/%v, want client-streaming/message", c.Shape, c.Plan().Message)
	}
}

func TestCompileBidiPlan(t *testing.T) {
	reg := testRegistry(t)
	c, err := Compile(reg, Stub{
		Method: "shop.v1.OrderService/Chat",
		Respond: Respond{
			OnOpen: []StepSpec{{Message: map[string]any{"text": "welcome"}}},
			Rules: []RuleSpec{{
				Match: &match.Block{Message: map[string]match.Rules{"text": {"matches": "^ping"}}},
				Send:  []StepSpec{{Message: map[string]any{"text": "pong"}}},
			}},
			OnClose: &CloseSpec{Status: &StatusSpec{Code: "OK"}},
		},
	}, "chat.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	plan := c.Plan()
	if c.Shape != match.Bidi || len(plan.Rules) != 1 || len(plan.OnOpen) != 1 || plan.OnClose == nil || plan.OnClose.Code() != codes.OK {
		t.Fatalf("compiled bidi plan = shape %v, rules %d, open %d, close %v", c.Shape, len(plan.Rules), len(plan.OnOpen), plan.OnClose)
	}
}

func TestCompileUnaryPlanControls(t *testing.T) {
	reg := testRegistry(t)
	c, err := Compile(reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Respond: Respond{
			Metadata: map[string]string{"x-mock": "simulacra"},
			Trailers: map[string]string{"x-served-by": "stub-42"},
			Delay:    "50ms..200ms",
			Status:   &StatusSpec{Code: "FAILED_PRECONDITION", Message: "not ready"},
		},
	}, "unary.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	plan := c.Plan()
	if c.Shape != match.Unary || plan.Header.Get("x-mock")[0] != "simulacra" || plan.Trailer.Get("x-served-by")[0] != "stub-42" {
		t.Fatalf("shape/metadata not compiled: %#v", plan)
	}
	if plan.Delay == nil || plan.Delay.Min != 50*time.Millisecond || plan.Delay.Max != 200*time.Millisecond {
		t.Errorf("delay = %#v", plan.Delay)
	}
	if plan.Status == nil || plan.Status.Code() != codes.FailedPrecondition {
		t.Errorf("status = %v", plan.Status)
	}
}

func TestCompilePlanErrors(t *testing.T) {
	reg := testRegistry(t)
	msg := func(text string) map[string]any { return map[string]any{"note": text} }
	chat := func(text string) map[string]any { return map[string]any{"text": text} }
	tests := []struct {
		name string
		stub Stub
		want string
	}{
		{"unary with stream", Stub{Method: "shop.v1.OrderService/GetOrder", Respond: Respond{Stream: []StepSpec{{Message: msg("x")}}}}, "stream"},
		{"unary message and status", Stub{Method: "shop.v1.OrderService/GetOrder", Respond: Respond{Message: msg("x"), Status: &StatusSpec{Code: "INTERNAL"}}}, "message"},
		{"server stream top message", Stub{Method: "shop.v1.OrderService/WatchOrder", Respond: Respond{Message: msg("x"), Stream: []StepSpec{{Message: msg("y")}}}}, "message"},
		{"step both", Stub{Method: "shop.v1.OrderService/WatchOrder", Respond: Respond{Stream: []StepSpec{{Message: msg("x"), Status: &StatusSpec{Code: "INTERNAL"}}}}}, "exactly one"},
		{"step neither", Stub{Method: "shop.v1.OrderService/WatchOrder", Respond: Respond{Stream: []StepSpec{{Delay: "1ms"}}}}, "exactly one"},
		{"terminal status not last", Stub{Method: "shop.v1.OrderService/WatchOrder", Respond: Respond{Stream: []StepSpec{{Status: &StatusSpec{Code: "INTERNAL"}}, {Message: msg("x")}}}}, "last"},
		{"bidi top message", Stub{Method: "shop.v1.OrderService/Chat", Respond: Respond{Message: chat("x"), OnOpen: []StepSpec{{Message: chat("welcome")}}}}, "message"},
		{"bidi rule empty send", Stub{Method: "shop.v1.OrderService/Chat", Respond: Respond{Rules: []RuleSpec{{Match: &match.Block{Message: map[string]match.Rules{"text": {"eq": "ping"}}}}}}}, "send"},
		{"bidi nothing", Stub{Method: "shop.v1.OrderService/Chat"}, "on_open"},
		{"client stream structured message matcher", Stub{Method: "shop.v1.OrderService/UploadOrders", Match: &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "x"}}}}, "message field matchers"},
		{"bad delay", Stub{Method: "shop.v1.OrderService/GetOrder", Respond: Respond{Delay: "soon"}}, "delay"},
		{"bad status code", Stub{Method: "shop.v1.OrderService/GetOrder", Respond: Respond{Status: &StatusSpec{Code: "BOGUS"}}}, "BOGUS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(reg, tt.stub, "errors.yaml#4")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Compile error = %v, want mention of %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "errors.yaml#4") {
				t.Errorf("error lacks source context: %v", err)
			}
		})
	}
}
