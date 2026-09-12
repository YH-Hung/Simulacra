package admin

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// inputError marks a failure caused by the content of the request itself.
type inputError struct{ err error }

func (e *inputError) Error() string { return e.err.Error() }
func (e *inputError) Unwrap() error { return e.err }

// invalidArgument marks err as caused by the caller's input. Handlers apply it
// exactly where they parse, compile, or validate what the request carried.
// connectError turns it into INVALID_ARGUMENT unless a typed core error inside
// it says something more specific.
func invalidArgument(err error) error {
	if err == nil {
		return nil
	}
	return &inputError{err: err}
}

// connectError maps the error a handler is about to return onto a connect code
// (design §7). Rows are checked in order, and the message is never rewritten.
//
// An unmarked, untyped error is INTERNAL. The loader, compiler, CEL, and
// RegisterSet diagnostics are untyped, so they become INVALID_ARGUMENT only
// through an invalidArgument mark: a handler that forgets one surfaces INTERNAL
// in its tests instead of reporting a server fault as the client's.
func connectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Built by the handler, or mapped by connect itself.
		return err
	}
	var fileOwned *stub.FileOwnedError
	var input *inputError
	switch {
	case errors.Is(err, schema.ErrUnknownMethod), errors.Is(err, stub.ErrStubNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.As(err, &fileOwned):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, journal.ErrSlowConsumer):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.As(err, &input):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
