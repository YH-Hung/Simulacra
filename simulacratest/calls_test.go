package simulacratest_test

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

const (
	legacyFetch = "/conformance.v1.LegacyService/Fetch"
	legacyMake  = "/conformance.v1.LegacyService/Make"
)

// onlyCall returns the one call the server recorded for method.
func onlyCall(t *testing.T, sim *simulacratest.Server, method string) simulacratest.Call {
	t.Helper()
	calls := sim.Calls(t, method)
	if len(calls) != 1 {
		t.Fatalf("Calls(%s) returned %d calls; want 1", method, len(calls))
	}
	return calls[0]
}

func TestCallsAreOldestFirst(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1", "o-2", "o-3")
	// Both method forms name the same calls, and "" names every call.
	for _, method := range []string{getOrder, "shop.v1.OrderService/GetOrder", ""} {
		calls := sim.Calls(t, method)
		if len(calls) != 3 {
			t.Fatalf("Calls(%q) returned %d calls; want 3", method, len(calls))
		}
		for i, call := range calls {
			got := &shopv1.GetOrderRequest{}
			call.Request(t, got)
			if want := fmt.Sprintf("o-%d", i+1); got.GetOrderId() != want {
				t.Errorf("Calls(%q)[%d] order_id = %q; want %q", method, i, got.GetOrderId(), want)
			}
		}
	}
}

func TestCaptureTypedRequestAndResponse(t *testing.T) {
	sim := simulacratest.Start(t)
	sent := &shopv1.GetOrderRequest{
		OrderId:  "o-1",
		Customer: &shopv1.Customer{Id: "c-1", Region: shopv1.Region_EU},
		Items:    []*shopv1.Item{{Sku: "s", Qty: 2}},
		Tags:     []string{"a", "b"},
	}
	reply := &shopv1.GetOrderResponse{OrderId: "o-1", Status: shopv1.OrderStatus_ORDER_STATUS_SHIPPED, Note: "{{ x }}"}
	stub := sim.Stub(getOrder).Respond(t, reply)
	if _, err := shopv1.NewOrderServiceClient(sim.Dial(t)).GetOrder(t.Context(), sent); err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	call := onlyCall(t, sim, getOrder)
	gotReq, gotResp := &shopv1.GetOrderRequest{}, &shopv1.GetOrderResponse{}
	call.Request(t, gotReq)
	call.Response(t, gotResp)
	if !proto.Equal(gotReq, sent) {
		t.Errorf("Request = %v; want %v", gotReq, sent)
	}
	if !proto.Equal(gotResp, reply) {
		t.Errorf("Response = %v; want %v", gotResp, reply)
	}
	if call.StubID() != stub.GetId() {
		t.Errorf("StubID() = %q; want %q", call.StubID(), stub.GetId())
	}
}

func TestCaptureRefusesTheWrongType(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1")
	call := onlyCall(t, sim, getOrder)
	seq := call.Proto().GetSeq()
	// Each pair shares field 1 as a string, so decoding into the other type
	// would succeed without the name check.
	for _, tc := range []struct {
		name    string
		capture func(tb testing.TB)
		want    string
	}{
		{"Request", func(tb testing.TB) { call.Request(tb, &shopv1.GetOrderResponse{}) },
			fmt.Sprintf("simulacratest: Request: call seq=%d carries shop.v1.GetOrderRequest, not shop.v1.GetOrderResponse", seq)},
		{"Response", func(tb testing.TB) { call.Response(tb, &shopv1.GetOrderRequest{}) },
			fmt.Sprintf("simulacratest: Response: call seq=%d carries shop.v1.GetOrderResponse, not shop.v1.GetOrderRequest", seq)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := runRecorded(t, tc.capture); rec.FatalMsg() != tc.want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), tc.want)
			}
		})
	}
}

func TestCaptureRefusesStreams(t *testing.T) {
	sim := simulacratest.Start(t)
	sim.StubYAML(t, `
method: shop.v1.OrderService/WatchOrder
respond:
  stream:
  - message: { note: a }
  - message: { note: b }
`)
	stream, err := shopv1.NewOrderServiceClient(sim.Dial(t)).WatchOrder(t.Context(), &shopv1.GetOrderRequest{})
	if err != nil {
		t.Fatalf("WatchOrder: %v", err)
	}
	for {
		if _, err := stream.Recv(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}

	call := onlyCall(t, sim, shopv1.OrderService_WatchOrder_FullMethodName)
	rec := runRecorded(t, func(tb testing.TB) { call.Response(tb, &shopv1.GetOrderResponse{}) })
	want := fmt.Sprintf("simulacratest: Response: call seq=%d has 2 responses; use Proto() for streaming calls", call.Proto().GetSeq())
	if rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestCaptureOfAFailedCallReportsItsStatus(t *testing.T) {
	sim := simulacratest.Start(t)
	sim.Stub(getOrder).RespondStatus(t, status.New(codes.NotFound, "gone"))
	if _, err := shopv1.NewOrderServiceClient(sim.Dial(t)).GetOrder(t.Context(), &shopv1.GetOrderRequest{}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetOrder error = %v; want NotFound", err)
	}

	call := onlyCall(t, sim, getOrder)
	rec := runRecorded(t, func(tb testing.TB) { call.Response(tb, &shopv1.GetOrderResponse{}) })
	want := fmt.Sprintf("simulacratest: Response: call seq=%d has 0 responses (status NOT_FOUND: gone); use Proto() for streaming calls", call.Proto().GetSeq())
	if rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestCaptureRejectsNilTarget(t *testing.T) {
	sim := simulacratest.Start(t)
	callGetOrder(t, sim, "o-1")
	call := onlyCall(t, sim, getOrder)
	for _, tc := range []struct {
		name    string
		capture func(tb testing.TB)
		want    string
	}{
		{"Request nil", func(tb testing.TB) { call.Request(tb, nil) }, "simulacratest: Request: target message is nil"},
		{"Request typed nil", func(tb testing.TB) { call.Request(tb, (*shopv1.GetOrderRequest)(nil)) }, "simulacratest: Request: target message is nil"},
		{"Response nil", func(tb testing.TB) { call.Response(tb, nil) }, "simulacratest: Response: target message is nil"},
		{"Response typed nil", func(tb testing.TB) { call.Response(tb, (*shopv1.GetOrderResponse)(nil)) }, "simulacratest: Response: target message is nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := runRecorded(t, tc.capture); rec.FatalMsg() != tc.want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), tc.want)
			}
		})
	}
}

func TestCallMetadataMergesBinaryValues(t *testing.T) {
	sim := simulacratest.Start(t)
	sim.Stub(getOrder).Respond(t, &shopv1.GetOrderResponse{})
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-text", "a", "x-raw-bin", "\x00\xff")
	if _, err := shopv1.NewOrderServiceClient(sim.Dial(t)).GetOrder(ctx, &shopv1.GetOrderRequest{}); err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	md := onlyCall(t, sim, getOrder).Metadata()
	if got := md["x-raw-bin"]; !slices.Equal(got, []string{"\x00\xff"}) {
		t.Errorf(`Metadata()["x-raw-bin"] = %q; want ["\x00\xff"]`, got)
	}
	if got := md["x-text"]; !slices.Equal(got, []string{"a"}) {
		t.Errorf(`Metadata()["x-text"] = %q; want ["a"]`, got)
	}
}

func TestCallStatusCarriesDetails(t *testing.T) {
	sim := simulacratest.Start(t)
	detail := &errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
		{Type: "STATE", Subject: "order/o-1", Description: "already shipped"},
	}}
	st, err := status.New(codes.FailedPrecondition, "shipped").WithDetails(detail)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	sim.Stub(getOrder).RespondStatus(t, st)
	if _, err := shopv1.NewOrderServiceClient(sim.Dial(t)).GetOrder(t.Context(), &shopv1.GetOrderRequest{}); err == nil {
		t.Fatalf("GetOrder succeeded; want FailedPrecondition")
	}

	got := onlyCall(t, sim, getOrder).Status()
	if got.Code() != codes.FailedPrecondition || got.Message() != "shipped" {
		t.Fatalf("Status() = %v %q; want FailedPrecondition %q", got.Code(), got.Message(), "shipped")
	}
	details := got.Details()
	if len(details) != 1 {
		t.Fatalf("Status().Details() = %v; want one", details)
	}
	if m, ok := details[0].(proto.Message); !ok || !proto.Equal(m, detail) {
		t.Fatalf("Status().Details()[0] = %v; want %v", details[0], detail)
	}
}

// makeWithExtTag registers files' legacy.proto, calls LegacyService/Make with
// a LegacyReply carrying ext_tag, and returns the recorded call.
func makeWithExtTag(t *testing.T, sim *simulacratest.Server, files map[string]protoreflect.FileDescriptor) simulacratest.Call {
	t.Helper()
	legacy := files[legacyPath]
	sim.RegisterFiles(t, legacy)
	thing := newMessage(legacy, "LegacyThing")
	thing.Set(thing.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString("n"))
	sim.Stub(legacyMake).Respond(t, thing)
	req := newMessage(legacy, "LegacyReply")
	req.Set(extension(legacy, "ext_tag"), protoreflect.ValueOfString("t"))
	if err := sim.Dial(t).Invoke(t.Context(), legacyMake, req, newMessage(legacy, "LegacyThing")); err != nil {
		t.Fatalf("Invoke %s: %v", legacyMake, err)
	}
	return onlyCall(t, sim, legacyMake)
}

func TestCaptureResolvesRegisteredExtensions(t *testing.T) {
	sim := simulacratest.Start(t)
	a := conformanceFiles(t)
	call := makeWithExtTag(t, sim, a)
	got := newMessage(a[legacyPath], "LegacyReply")
	call.Request(t, got)
	ext := extension(a[legacyPath], "ext_tag")
	if !got.Has(ext) || got.Get(ext).String() != "t" {
		t.Fatalf("captured ext_tag = %q (Has %v); want %q", got.Get(ext).String(), got.Has(ext), "t")
	}
}

func TestCaptureRefusesADifferentDescriptorInstance(t *testing.T) {
	sim := simulacratest.Start(t)
	a, b := conformanceFiles(t), conformanceFiles(t)
	call := makeWithExtTag(t, sim, a)
	got := newMessage(b[legacyPath], "LegacyReply")
	rec := runRecorded(t, func(tb testing.TB) { call.Request(tb, got) })
	if want := "simulacratest: Request: " + instanceMismatch(legacyPath); rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q (Has through its own ext_tag: %v); want %q",
			rec.FatalMsg(), got.Has(extension(b[legacyPath], "ext_tag")), want)
	}
}

func TestRegistrationRacesRespondAndCapture(t *testing.T) {
	sim := simulacratest.Start(t)
	legacy := conformanceFiles(t)[legacyPath]
	sim.RegisterFiles(t, legacy)
	conn := sim.Dial(t)
	// Registrations swap the Client's snapshot while Respond renders and
	// guards, and capture decodes, an extension through it.
	t.Run("group", func(t *testing.T) {
		for i := range 8 {
			t.Run("register/"+strconv.Itoa(i), func(t *testing.T) {
				t.Parallel()
				sim.RegisterFiles(t, syntheticFile(t, i))
			})
			t.Run("respond and capture/"+strconv.Itoa(i), func(t *testing.T) {
				t.Parallel()
				reply := newMessage(legacy, "LegacyReply")
				reply.Set(extension(legacy, "ext_tag"), protoreflect.ValueOfString("t"))
				sim.Stub(legacyFetch).Respond(t, reply)
				req := newMessage(legacy, "LegacyThing")
				req.Set(req.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString("n"))
				if err := conn.Invoke(t.Context(), legacyFetch, req, newMessage(legacy, "LegacyReply")); err != nil {
					t.Fatalf("Invoke %s: %v", legacyFetch, err)
				}
				// Fetch returns the LegacyReply; its request is a LegacyThing.
				got := newMessage(legacy, "LegacyReply")
				sim.Calls(t, legacyFetch)[0].Response(t, got)
				if !got.Has(extension(legacy, "ext_tag")) {
					t.Fatalf("captured response lost ext_tag: %v", got)
				}
			})
		}
	})
}
