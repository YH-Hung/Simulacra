package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// stubService implements simulacra.admin.v1.StubService.
//
// It deliberately does not embed UnimplementedStubServiceHandler: an RPC added
// to the contract must fail the build here, not return Unimplemented at
// runtime.
type stubService struct {
	deps Deps
}

// CreateStub compiles one stub document and installs it as an API-origin stub
// (design §4.3).
func (s *stubService) CreateStub(
	_ context.Context,
	req *connect.Request[adminv1.CreateStubRequest],
) (*connect.Response[adminv1.CreateStubResponse], error) {
	compiled, err := compileDocument(stub.NewCompiler(s.deps.Registry), req.Msg.GetDocument(), "document")
	if err != nil {
		return nil, connectError(err)
	}
	s.deps.Store.Add(compiled)
	return connect.NewResponse(&adminv1.CreateStubResponse{Stub: stubEnvelope(infoOf(compiled))}), nil
}

// ListStubs lists stub envelopes in the store's order, optionally filtered by
// method and origin.
func (s *stubService) ListStubs(
	_ context.Context,
	req *connect.Request[adminv1.ListStubsRequest],
) (*connect.Response[adminv1.ListStubsResponse], error) {
	filter := stub.ListFilter{Method: normalizeMethod(req.Msg.GetMethod())}
	switch origin := req.Msg.GetOrigin(); origin {
	case adminv1.StubOrigin_STUB_ORIGIN_UNSPECIFIED:
	case adminv1.StubOrigin_STUB_ORIGIN_FILE:
		file := stub.OriginFile
		filter.Origin = &file
	case adminv1.StubOrigin_STUB_ORIGIN_API:
		api := stub.OriginAPI
		filter.Origin = &api
	default:
		return nil, connectError(invalidArgument(fmt.Errorf("origin %d is not a declared StubOrigin", origin)))
	}
	infos := s.deps.Store.List(filter)
	envelopes := make([]*adminv1.Stub, len(infos))
	for i, info := range infos {
		envelopes[i] = stubEnvelope(info)
	}
	return connect.NewResponse(&adminv1.ListStubsResponse{Stubs: envelopes}), nil
}

// DeleteStub removes an API-origin stub. The store enforces ownership: a
// file-origin stub is FAILED_PRECONDITION, an unknown id NOT_FOUND.
func (s *stubService) DeleteStub(
	_ context.Context,
	req *connect.Request[adminv1.DeleteStubRequest],
) (*connect.Response[adminv1.DeleteStubResponse], error) {
	id := req.Msg.GetId()
	if id == "" {
		return nil, connectError(invalidArgument(errors.New("id is required")))
	}
	if err := s.deps.Store.Remove(id); err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&adminv1.DeleteStubResponse{}), nil
}

// ReplaceAllStubs compiles every document before touching the store, then swaps
// the API-origin stubs in one step. The first failure is returned and the store
// is untouched; file-origin stubs and their budgets are never affected.
func (s *stubService) ReplaceAllStubs(
	_ context.Context,
	req *connect.Request[adminv1.ReplaceAllStubsRequest],
) (*connect.Response[adminv1.ReplaceAllStubsResponse], error) {
	compiler := stub.NewCompiler(s.deps.Registry)
	documents := req.Msg.GetDocuments()
	compiled := make([]*stub.Compiled, len(documents))
	for i, document := range documents {
		c, err := compileDocument(compiler, document, fmt.Sprintf("documents[%d]", i))
		if err != nil {
			return nil, connectError(err)
		}
		compiled[i] = c
	}
	if _, err := s.deps.Store.ReplaceOrigin(stub.OriginAPI, compiled); err != nil {
		return nil, connectError(err)
	}
	envelopes := make([]*adminv1.Stub, len(compiled))
	for i, c := range compiled {
		envelopes[i] = stubEnvelope(infoOf(c))
	}
	return connect.NewResponse(&adminv1.ReplaceAllStubsResponse{Stubs: envelopes}), nil
}

// ExportStubs renders the API-origin stubs, in ListStubs order, as one
// file-grammar sequence. Loading the file back keeps their relative order.
func (s *stubService) ExportStubs(
	_ context.Context,
	_ *connect.Request[adminv1.ExportStubsRequest],
) (*connect.Response[adminv1.ExportStubsResponse], error) {
	api := stub.OriginAPI
	infos := s.deps.Store.List(stub.ListFilter{Origin: &api})
	documents := make([]string, len(infos))
	for i, info := range infos {
		documents[i] = info.Document
	}
	rendered, err := stub.RenderSequence(documents)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&adminv1.ExportStubsResponse{
		Document:  validUTF8(rendered),
		StubCount: int32(len(infos)),
	}), nil
}

// compileDocument parses and compiles one stub document, labeling both parse
// and compile diagnostics with source. Both steps consume the caller's input,
// so both failures are marked; a typed error inside the compile step (an
// unknown method) still wins in connectError.
func compileDocument(compiler *stub.Compiler, document, source string) (*stub.Compiled, error) {
	parsed, normalized, err := stub.ParseDocument([]byte(document))
	if err != nil {
		return nil, invalidArgument(fmt.Errorf("%s: %w", source, err))
	}
	compiled, err := compiler.Compile(parsed, source)
	if err != nil {
		return nil, invalidArgument(err)
	}
	compiled.Document = normalized
	return compiled, nil
}

// normalizeMethod accepts "pkg.Service/Method" with or without the leading
// slash and returns the "/pkg.Service/Method" form the store and journal key
// on. Empty stays empty, meaning no filter.
func normalizeMethod(method string) string {
	if method == "" {
		return ""
	}
	return "/" + strings.TrimPrefix(method, "/")
}

// infoOf builds the stub.Info for a stub that Add or ReplaceOrigin has just
// installed and stamped with its id, origin, and source — stubEnvelope is
// what renders that Info into the wire envelope. Its hits are 0 by
// definition.
func infoOf(c *stub.Compiled) stub.Info {
	return stub.Info{
		ID:       c.ID,
		Method:   c.Method,
		Shape:    c.Shape,
		Priority: c.Priority,
		Times:    c.Times,
		Origin:   c.Origin,
		Source:   c.Source,
		Document: c.Document,
	}
}

// stubEnvelope renders a store envelope. priority and times convert losslessly
// because the compiler bounds them to int32 (design §3.3).
func stubEnvelope(info stub.Info) *adminv1.Stub {
	return &adminv1.Stub{
		Id:       validUTF8(info.ID),
		Method:   validUTF8(info.Method),
		Shape:    stubShape(info.Shape),
		Priority: int32(info.Priority),
		Times:    int32(info.Times),
		Origin:   stubOrigin(info.Origin),
		Source:   validUTF8(info.Source),
		Hits:     int64(info.Hits),
		Document: validUTF8(info.Document),
	}
}

func stubShape(shape match.Shape) adminv1.StubShape {
	switch shape {
	case match.Unary:
		return adminv1.StubShape_STUB_SHAPE_UNARY
	case match.ServerStream:
		return adminv1.StubShape_STUB_SHAPE_SERVER_STREAM
	case match.ClientStream:
		return adminv1.StubShape_STUB_SHAPE_CLIENT_STREAM
	case match.Bidi:
		return adminv1.StubShape_STUB_SHAPE_BIDI_STREAM
	default:
		return adminv1.StubShape_STUB_SHAPE_UNSPECIFIED
	}
}

func stubOrigin(origin stub.Origin) adminv1.StubOrigin {
	if origin == stub.OriginAPI {
		return adminv1.StubOrigin_STUB_ORIGIN_API
	}
	return adminv1.StubOrigin_STUB_ORIGIN_FILE
}
