package stub

import (
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
)

func TestCompileStatusWithPreconditionFailure(t *testing.T) {
	reg := testRegistry(t)
	got, err := compileStatus(reg, StatusSpec{
		Code:    "FAILED_PRECONDITION",
		Message: "customer must accept the terms",
		Details: []DetailSpec{{
			Type: "google.rpc.PreconditionFailure",
			Value: map[string]any{
				"violations": []any{map[string]any{
					"type":        "TERMS_OF_SERVICE",
					"subject":     "customers/c-1",
					"description": "terms have not been accepted",
				}},
			},
		}},
	}, false)
	if err != nil {
		t.Fatalf("compileStatus: %v", err)
	}
	if got.Code() != codes.FailedPrecondition {
		t.Errorf("Code() = %v, want %v", got.Code(), codes.FailedPrecondition)
	}
	if got.Message() != "customer must accept the terms" {
		t.Errorf("Message() = %q", got.Message())
	}
	details := got.Details()
	if len(details) != 1 {
		t.Fatalf("len(Details()) = %d, want 1", len(details))
	}
	precondition, ok := details[0].(*errdetails.PreconditionFailure)
	if !ok {
		t.Fatalf("Details()[0] = %T, want *errdetails.PreconditionFailure", details[0])
	}
	if len(precondition.Violations) != 1 {
		t.Fatalf("len(Violations) = %d, want 1", len(precondition.Violations))
	}
	if got := precondition.Violations[0].Subject; got != "customers/c-1" {
		t.Errorf("violation subject = %q, want %q", got, "customers/c-1")
	}
}

func TestCompileStatusPacksRegistryMessageDetail(t *testing.T) {
	reg := testRegistry(t)
	got, err := compileStatus(reg, StatusSpec{
		Code: "NOT_FOUND",
		Details: []DetailSpec{{
			Type: "shop.v1.Customer",
			Value: map[string]any{
				"id":     "c-1",
				"region": "EU",
			},
		}},
	}, false)
	if err != nil {
		t.Fatalf("compileStatus: %v", err)
	}
	details := got.Proto().Details
	if len(details) != 1 {
		t.Fatalf("len(Proto().Details) = %d, want 1", len(details))
	}
	if got, want := details[0].TypeUrl, "type.googleapis.com/shop.v1.Customer"; got != want {
		t.Errorf("detail type URL = %q, want %q", got, want)
	}
}

func TestCompileStatusRejectsOKUnlessAllowed(t *testing.T) {
	reg := testRegistry(t)
	spec := StatusSpec{Code: "OK", Message: "fine"}
	if _, err := compileStatus(reg, spec, false); err == nil || !strings.Contains(err.Error(), "OK") {
		t.Fatalf("compileStatus(..., false) error = %v, want mention of OK", err)
	}
	got, err := compileStatus(reg, spec, true)
	if err != nil {
		t.Fatalf("compileStatus(..., true): %v", err)
	}
	if got.Code() != codes.OK || got.Message() != "fine" {
		t.Errorf("allowed status = (%v, %q), want (OK, %q)", got.Code(), got.Message(), "fine")
	}
}

func TestCompileStatusErrors(t *testing.T) {
	reg := testRegistry(t)
	tests := []struct {
		name string
		spec StatusSpec
		want string
	}{
		{name: "empty code", spec: StatusSpec{}, want: "code"},
		{name: "unknown code", spec: StatusSpec{Code: "TOTALLY_UNKNOWN"}, want: "TOTALLY_UNKNOWN"},
		{name: "unknown detail type", spec: StatusSpec{
			Code:    "INTERNAL",
			Details: []DetailSpec{{Type: "no.such.Detail"}},
		}, want: "no.such.Detail"},
		{name: "invalid detail value", spec: StatusSpec{
			Code: "INVALID_ARGUMENT",
			Details: []DetailSpec{{
				Type:  "google.rpc.BadRequest",
				Value: map[string]any{"bogus": true},
			}},
		}, want: "bogus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileStatus(reg, tt.spec, false)
			if err == nil {
				t.Fatal("compileStatus error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}
