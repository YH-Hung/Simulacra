package simulacratest_test

import (
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

func TestCodeNamesAreWhatTheServerParses(t *testing.T) {
	for c := codes.Code(1); c <= 16; c++ {
		var got codes.Code
		if err := got.UnmarshalJSON([]byte(`"` + simulacratest.CodeName(c) + `"`)); err != nil || got != c {
			t.Errorf("code %d: name %q parses to %v, %v", c, simulacratest.CodeName(c), got, err)
		}
	}
}

func TestRespondStatusDeliversTypedDetails(t *testing.T) {
	sim := simulacratest.Start(t)
	details := []protoadapt.MessageV1{
		&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
			{Type: "STATE", Subject: "order/o-1", Description: "already shipped {{ x }}"},
		}},
		&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{
			{Field: "order_id", Description: "closed"},
		}},
		&errdetails.RetryInfo{RetryDelay: durationpb.New(1500000000)},
	}
	st, err := status.New(codes.FailedPrecondition, "order already shipped").WithDetails(details...)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	sim.Stub(getOrder).Header("x-h", "hv").Trailer("x-t", "tv").RespondStatus(t, st)

	var header, trailer metadata.MD
	_, err = shopv1.NewOrderServiceClient(sim.Dial(t)).GetOrder(t.Context(), &shopv1.GetOrderRequest{OrderId: "o-1"},
		grpc.Header(&header), grpc.Trailer(&trailer))
	got, ok := status.FromError(err)
	if !ok || got.Code() != codes.FailedPrecondition || got.Message() != "order already shipped" {
		t.Fatalf("GetOrder error = %v; want FailedPrecondition %q", err, "order already shipped")
	}
	gotDetails := got.Details()
	if len(gotDetails) != len(details) {
		t.Fatalf("got %d details %v; want %d", len(gotDetails), gotDetails, len(details))
	}
	for i, d := range gotDetails {
		if m, ok := d.(proto.Message); !ok || !proto.Equal(m, protoadapt.MessageV2Of(details[i])) {
			t.Errorf("detail %d = %v; want %v", i, d, details[i])
		}
	}
	if h := header.Get("x-h"); len(h) != 1 || h[0] != "hv" {
		t.Errorf("header x-h = %v; want [hv]", h)
	}
	if v := trailer.Get("x-t"); len(v) != 1 || v[0] != "tv" {
		t.Errorf("trailer x-t = %v; want [tv]", v)
	}
}

func TestRespondStatusRefusesAnUnresolvableDetail(t *testing.T) {
	sim := simulacratest.Start(t)
	st := status.FromProto(&spb.Status{Code: int32(codes.NotFound), Details: []*anypb.Any{
		{TypeUrl: "type.googleapis.com/nope.v1.Missing"},
	}})
	rec := runRecorded(t, func(tb testing.TB) { sim.Stub(getOrder).RespondStatus(tb, st) })
	want := "simulacratest: RespondStatus: detail 0: cannot resolve type.googleapis.com/nope.v1.Missing"
	if rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestRespondStatusRefusesWhatItCannotName(t *testing.T) {
	sim := simulacratest.Start(t)
	for _, tc := range []struct {
		name string
		st   *status.Status
		want string
	}{
		{"nil", nil, "simulacratest: RespondStatus: status is nil"},
		{"unknown code", status.New(codes.Code(17), "x"), "simulacratest: RespondStatus: code 17 has no canonical name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runRecorded(t, func(tb testing.TB) { sim.Stub(getOrder).RespondStatus(tb, tc.st) })
			if rec.FatalMsg() != tc.want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), tc.want)
			}
		})
	}
}

func TestRespondStatusReportsBuilderErrorsFirst(t *testing.T) {
	sim := simulacratest.Start(t)
	rec := runRecorded(t, func(tb testing.TB) {
		sim.Stub(getOrder).Header("X-A", "1").Header("x-a", "2").RespondStatus(tb, nil)
	})
	want := `simulacratest: Header("x-a"): duplicate key (metadata keys are case-insensitive)`
	if rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestRespondStatusOKShowsTheServerDiagnostic(t *testing.T) {
	sim := simulacratest.Start(t)
	rec := runRecorded(t, func(tb testing.TB) { sim.Stub(getOrder).RespondStatus(tb, status.New(codes.OK, "")) })
	if got := rec.FatalMsg(); !strings.Contains(got, "status code OK is not allowed here") {
		t.Fatalf("FatalMsg() = %q; want it to contain the server's OK diagnostic", got)
	}
}

func TestRespondStatusRefusesAMessageTheYAMLHopWouldAlter(t *testing.T) {
	sim := simulacratest.Start(t)
	const want = "simulacratest: RespondStatus: the status message does not survive the stub grammar's JSON round trip unchanged (invalid UTF-8 or a raw U+0085, for example); use StubYAML for full control"
	for _, tc := range []struct{ name, msg string }{
		{"invalid UTF-8", "a\xffb"},
		{"raw U+0085", "a\u0085b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runRecorded(t, func(tb testing.TB) {
				sim.Stub(getOrder).RespondStatus(tb, status.New(codes.NotFound, tc.msg))
			})
			if rec.FatalMsg() != want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
			}
		})
	}
}
