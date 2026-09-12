package admin_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// The design §7 table, row by row. Every mapped error comes back as a
// *connect.Error whose message is the original error's text, unchanged.
func TestConnectErrorMapsTheDesignTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"unknown method", fmt.Errorf("document: %w", schema.ErrUnknownMethod), connect.CodeNotFound},
		{"unknown method inside an input mark",
			admin.InvalidArgument(fmt.Errorf("document: %w", schema.ErrUnknownMethod)), connect.CodeNotFound},
		{"unknown stub id", fmt.Errorf("%w: %q", stub.ErrStubNotFound, "api-9"), connect.CodeNotFound},
		{"file-owned stub", &stub.FileOwnedError{ID: "stubs/a.yaml#0", Source: "stubs/a.yaml#0"},
			connect.CodeFailedPrecondition},
		{"slow consumer", journal.ErrSlowConsumer, connect.CodeResourceExhausted},
		{"marked input", admin.InvalidArgument(errors.New("parsing stub document: yaml: line 2: bad")),
			connect.CodeInvalidArgument},
		{"unmarked untyped", errors.New("rendering stub sequence: boom"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := admin.ConnectError(tc.err)
			var connectErr *connect.Error
			if !errors.As(got, &connectErr) {
				t.Fatalf("ConnectError(%v) = %T, want a *connect.Error", tc.err, got)
			}
			if connectErr.Code() != tc.want {
				t.Errorf("code = %v, want %v", connectErr.Code(), tc.want)
			}
			if connectErr.Message() != tc.err.Error() {
				t.Errorf("message = %q, want the original text %q", connectErr.Message(), tc.err.Error())
			}
		})
	}
}

// A connect error a handler built itself, and context errors connect maps on
// its own, come back as the very same value.
func TestConnectErrorPassesThroughConnectAndContextErrors(t *testing.T) {
	for _, err := range []error{
		connect.NewError(connect.CodeUnavailable, errors.New("server shutting down")),
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("sending: %w", context.Canceled),
	} {
		if got := admin.ConnectError(err); got != err {
			t.Errorf("ConnectError(%v) = %v, want the same error back", err, got)
		}
	}
	if got := admin.ConnectError(nil); got != nil {
		t.Errorf("ConnectError(nil) = %v, want nil", got)
	}
	if got := admin.InvalidArgument(nil); got != nil {
		t.Errorf("InvalidArgument(nil) = %v, want nil", got)
	}
}
