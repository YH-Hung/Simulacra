package simulacratest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/reflect/protoregistry"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

// defaultRPCTimeout bounds one admin RPC. A closed port fails fast on its own;
// this is for a server or proxy that accepts the connection and never answers
// (the CLI's defaultTimeout, for the same reason).
const defaultRPCTimeout = 30 * time.Second

// Admin is the five generated admin clients against one server — the escape
// hatch for anything the Client does not wrap.
type Admin struct {
	Schema  adminv1connect.SchemaServiceClient
	Stub    adminv1connect.StubServiceClient
	Journal adminv1connect.JournalServiceClient
	Verify  adminv1connect.VerifyServiceClient
	Control adminv1connect.ControlServiceClient
}

// Client talks to a running server's admin API and dials its data plane. Start
// builds one for an in-process server; Connect builds one for any other.
type Client struct {
	adminAddr string
	dataAddr  string
	admin     Admin
	timeout   time.Duration

	regMu       sync.Mutex // serializes registerFiles from check to swap; readers never take it
	snap        atomic.Pointer[typeSnapshot]
	globalFiles *protoregistry.Files // protoregistry.GlobalFiles, except under test
}

// Connect returns a Client for a server already running at adminAddr (the
// admin plane) and dataAddr (the data plane). The data address is explicit
// rather than read from the server, because behind a container's port mapping
// the server reports an address the test cannot dial.
//
// The Client owns an HTTP transport and releases its idle connections when t
// finishes.
func Connect(t testing.TB, adminAddr, dataAddr string) *Client {
	t.Helper()
	// Connect over HTTP/1.1 like the CLI: the admin plane speaks it, so no h2c
	// plumbing is needed client-side. Each Client gets its own transport so it
	// can release exactly its own connections.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: transport}
	base := baseURL(adminAddr)
	c := &Client{
		adminAddr: adminAddr,
		dataAddr:  dataAddr,
		admin: Admin{
			Schema:  adminv1connect.NewSchemaServiceClient(httpClient, base),
			Stub:    adminv1connect.NewStubServiceClient(httpClient, base),
			Journal: adminv1connect.NewJournalServiceClient(httpClient, base),
			Verify:  adminv1connect.NewVerifyServiceClient(httpClient, base),
			Control: adminv1connect.NewControlServiceClient(httpClient, base),
		},
		timeout:     defaultRPCTimeout,
		globalFiles: protoregistry.GlobalFiles,
	}
	c.snap.Store(newTypeSnapshot(new(protoregistry.Files)))
	return c
}

// baseURL turns an address into a base URL, leaving an explicit scheme alone
// so an admin plane behind a proxy or a path prefix is reachable.
func baseURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// AdminAddr is the admin-plane address the Client was built with.
func (c *Client) AdminAddr() string { return c.adminAddr }

// DataAddr is the data-plane address the Client was built with.
func (c *Client) DataAddr() string { return c.dataAddr }

// Admin returns the generated admin clients.
func (c *Client) Admin() Admin { return c.admin }

// Dial opens a gRPC connection to the data plane, closed when t finishes.
// Options are applied after the insecure-credentials default, so a caller can
// override it.
func (c *Client) Dial(t testing.TB, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(c.dataAddr, opts...)
	if err != nil {
		t.Fatalf("simulacratest: Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// StubYAML creates one stub from a document in the stub file grammar and
// returns the server's envelope for it. The server parses it with the same
// strict loader as stub files, so a malformed document fails the test with the
// server's own diagnostic.
func (c *Client) StubYAML(t testing.TB, doc string) *adminv1.Stub {
	t.Helper()
	ctx, cancel := c.rpcContext()
	defer cancel()
	resp, err := c.admin.Stub.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: doc}))
	if err != nil {
		c.fatalRPC(t, "CreateStub", err)
	}
	return resp.Msg.GetStub()
}

// DeleteStub removes the stub with the given id.
func (c *Client) DeleteStub(t testing.TB, id string) {
	t.Helper()
	ctx, cancel := c.rpcContext()
	defer cancel()
	if _, err := c.admin.Stub.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: id})); err != nil {
		c.fatalRPC(t, "DeleteStub", err)
	}
}

// Reset clears every API-created stub, restores file stubs to their loaded
// state, and empties the call journal. Both targets are omitted from the
// request, which the server reads as "reset both".
func (c *Client) Reset(t testing.TB) {
	t.Helper()
	ctx, cancel := c.rpcContext()
	defer cancel()
	if _, err := c.admin.Control.Reset(ctx, connect.NewRequest(&adminv1.ResetRequest{})); err != nil {
		c.fatalRPC(t, "Reset", err)
	}
}

// rpcContext is the context for one admin RPC. It is deliberately not derived
// from t.Context, which is canceled before cleanups run (see the package doc).
func (c *Client) rpcContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.timeout)
}

// fatalRPC fails the test with an admin error. The server's message passes
// through unchanged after the SDK's own prefix; each extra is an
// SDK-sourced line appended below it.
func (c *Client) fatalRPC(t testing.TB, op string, err error, extra ...string) {
	t.Helper()
	msg := fmt.Sprintf("simulacratest: %s: %v", op, err)
	for _, line := range extra {
		msg += "\n" + line
	}
	t.Fatalf("%s", msg)
}
