package admin

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/journal"
)

// journalService implements simulacra.admin.v1.JournalService.
//
// It deliberately does not embed UnimplementedJournalServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type journalService struct {
	deps Deps
}

// ListCalls returns recorded calls newest first (design §4.4).
func (j *journalService) ListCalls(
	_ context.Context,
	req *connect.Request[adminv1.ListCallsRequest],
) (*connect.Response[adminv1.ListCallsResponse], error) {
	limit := req.Msg.GetLimit()
	if limit < 0 {
		return nil, connectError(invalidArgument(fmt.Errorf("limit must not be negative (got %d)", limit)))
	}
	// Filter normalizes the method and keeps the newest limit calls, oldest
	// first; the contract is newest first.
	calls := j.deps.Journal.Filter(journal.Filter{Method: req.Msg.GetMethod(), Limit: int(limit)})
	types := j.deps.Registry.Types()
	rendered := make([]*adminv1.Call, len(calls))
	for i, call := range calls {
		rendered[len(calls)-1-i] = renderCall(call, types)
	}
	return connect.NewResponse(&adminv1.ListCallsResponse{Calls: rendered}), nil
}

// ResetJournal clears retained calls; sequence numbers keep counting.
func (j *journalService) ResetJournal(
	_ context.Context,
	_ *connect.Request[adminv1.ResetJournalRequest],
) (*connect.Response[adminv1.ResetJournalResponse], error) {
	j.deps.Journal.Reset()
	return connect.NewResponse(&adminv1.ResetJournalResponse{}), nil
}

// WatchCalls streams calls as they are recorded, until the client leaves, the
// journal evicts it for falling behind, or teardown begins (design §6).
func (j *journalService) WatchCalls(
	ctx context.Context,
	req *connect.Request[adminv1.WatchCallsRequest],
	stream *connect.ServerStream[adminv1.WatchCallsResponse],
) error {
	sub := j.deps.Journal.Watch(ctx, req.Msg.GetMethod())
	defer sub.Close()
	// connect's server-streaming client blocks establishing the call until the
	// server writes something: response headers are sent with the first Send,
	// not when the handler starts. Without this, a caller cannot learn the
	// subscription exists until a matching call arrives to send — exactly the
	// thing a caller needs the open stream to observe. Send(nil) flushes
	// headers immediately and is not delivered to the client as a message.
	// This also locks the response headers in: any header WatchCalls adds in
	// the future must be set on stream before this call. None are set today.
	if err := stream.Send(nil); err != nil {
		return connectError(err)
	}
	types := j.deps.Registry.Types()
	render := func(call *journal.Call) *adminv1.WatchCallsResponse {
		return &adminv1.WatchCallsResponse{Call: renderCall(call, types)}
	}
	return connectError(streamCalls(ctx, j.deps.Stopping, sub.Calls(), sub.Err, render, stream.Send))
}

// streamCalls is WatchCalls' send loop, separated from the RPC so its shutdown
// precedence can be tested without timing.
//
// Teardown takes precedence over buffered calls. Selecting on stopping ends an
// idle stream, but when a call is buffered and stopping has closed, both cases
// are ready and Go picks one at random. So stopping is checked again, without
// blocking, after rendering and immediately before every send: once that check
// sees stopping closed, no further send starts. A send that passed its check
// just before teardown began can still run, and against a client that stopped
// reading it blocks until the server's drain bound force-closes the connection
// — connect offers no way to interrupt a send in progress.
//
// When the subscription ends, its own error wins: journal.ErrSlowConsumer
// after an eviction, reported only once the calls buffered before it have been
// sent. Otherwise the result is the context's error, or nil for a subscription
// closed while the client was still there.
func streamCalls(
	ctx context.Context,
	stopping <-chan struct{},
	calls <-chan *journal.Call,
	subscriptionErr func() error,
	render func(*journal.Call) *adminv1.WatchCallsResponse,
	send func(*adminv1.WatchCallsResponse) error,
) error {
	for {
		select {
		case <-stopping:
			return errShuttingDown()
		case call, ok := <-calls:
			if !ok {
				if err := subscriptionErr(); err != nil {
					return err
				}
				return ctx.Err()
			}
			response := render(call)
			select {
			case <-stopping:
				return errShuttingDown()
			default:
			}
			if err := send(response); err != nil {
				return err
			}
		}
	}
}

// errShuttingDown is how every open watch ends once teardown begins.
func errShuttingDown() error {
	return connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))
}
