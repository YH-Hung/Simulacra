package admin

import (
	"context"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// controlService implements simulacra.admin.v1.ControlService.
//
// It deliberately does not embed UnimplementedControlServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type controlService struct {
	deps Deps
}

// GetServerInfo is a pure read of state the server already owns.
func (c *controlService) GetServerInfo(
	_ context.Context,
	_ *connect.Request[adminv1.GetServerInfoRequest],
) (*connect.Response[adminv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&adminv1.GetServerInfoResponse{
		Version:         c.deps.Version,
		DataAddr:        c.deps.DataAddr(),
		AdminAddr:       c.deps.AdminAddr(),
		ServiceCount:    int32(len(c.deps.Registry.Services())),
		StubCount:       int32(c.deps.Store.Len()),
		JournalCount:    int32(c.deps.Journal.Len()),
		JournalCapacity: int32(c.deps.Journal.Cap()),
	}), nil
}

// Reset implements the contract's presence rule: an omitted field means true
// (reset it), an explicit false skips.
//
// Written against the pointer rather than the generated GetStubs()/GetJournal(),
// which flatten nil to false and would invert the documented default — the
// single easiest way to get this RPC wrong.
func (c *controlService) Reset(
	_ context.Context,
	req *connect.Request[adminv1.ResetRequest],
) (*connect.Response[adminv1.ResetResponse], error) {
	if req.Msg.Stubs == nil || *req.Msg.Stubs {
		c.deps.Store.ResetStubs()
	}
	if req.Msg.Journal == nil || *req.Msg.Journal {
		c.deps.Journal.Reset()
	}
	return connect.NewResponse(&adminv1.ResetResponse{}), nil
}

// Shutdown starts server teardown and returns. The handler stays dumb on
// purpose: it must not block, because the teardown it triggers waits on this
// very response to flush.
func (c *controlService) Shutdown(
	_ context.Context,
	_ *connect.Request[adminv1.ShutdownRequest],
) (*connect.Response[adminv1.ShutdownResponse], error) {
	c.deps.Shutdown()
	return connect.NewResponse(&adminv1.ShutdownResponse{}), nil
}
