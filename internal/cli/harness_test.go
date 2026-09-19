package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/server"
)

// startCommandServer boots a real in-process server with both planes on
// ephemeral ports. M3 §12 specifies command tests run against a real server,
// not a fake: the CLI's job is translating the admin contract, and a fake
// would be translating it a second time.
func startCommandServer(t *testing.T) *server.Server {
	t.Helper()
	return startCommandServerWithStubs(t)
}

// startCommandServerWithStubs is startCommandServer with file-origin stubs
// loaded from stubDirs, for the tests that need a stub the API may not delete.
func startCommandServerWithStubs(t *testing.T, stubDirs ...string) *server.Server {
	t.Helper()
	srv, err := server.Start(context.Background(), server.Options{
		ProtoDirs:   []string{"../../testdata/protos"},
		StubDirs:    stubDirs,
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 64,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// adminAddr is the --addr value pointing at srv's admin plane.
func adminAddr(srv *server.Server) string {
	return srv.AdminAddr().String()
}

// httpClientForTest is the HTTP/1.1 client the test-side admin clients use.
func httpClientForTest(t *testing.T) *http.Client {
	t.Helper()
	client := &http.Client{}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// runCmd executes cmd with args, capturing its two streams into separate
// buffers.
//
// It cannot prove the stdout/stderr split of design §4, and no test here
// should be read as doing so. SetOut makes cobra's OutOrStderr — the writer
// behind cmd.Print, cmd.Printf and cmd.Println — return the stdout buffer,
// because OutOrStderr falls back to os.Stderr only when nothing has set an out
// writer. So a cmd.Printf that lands on stderr in the real binary is captured
// as stdout here, and a test asserting on these buffers cannot tell the two
// apart. The capture is still what every behavioural test needs; stream
// discipline is pinned separately, against the built binary with genuinely
// distinct pipes, in signal_test.go.
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// failingWriter is a stdout that cannot be written to — a full disk, a closed
// pipe, a redirect past the process's file-size limit.
//
// No healthy writer produces one, and nothing else can see the behaviour: with
// a working writer a checked and an unchecked payload write are
// indistinguishable, which is exactly how a silent one survives.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected write failure")
}

// runCmdWithFailingStdout runs cmd with a stdout that fails every write, and
// returns the subcommand ExecuteContextC resolved so the caller can put the
// error through the real exit-code policy rather than approximating it.
//
// stderr stays a working buffer on purpose: cobra's PrintErr* family writes
// there, and design §5 keeps a command's commentary out of its outcome — only
// a failed *payload* write is an operational failure.
func runCmdWithFailingStdout(t *testing.T, cmd *cobra.Command, args ...string) (*cobra.Command, string, error) {
	t.Helper()
	var stderr bytes.Buffer
	cmd.SetOut(failingWriter{})
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	ran, err := cmd.ExecuteContextC(context.Background())
	return ran, stderr.String(), err
}

// decodeJSON unmarshals rendered output into a generic map. protojson output
// is deliberately not byte-stable, so tests decode and assert on fields rather
// than comparing strings — the same rule server/admin_data_test.go follows.
func decodeJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatalf("decoding %q: %v", text, err)
	}
	return fields
}

// syncBuffer is an io.Writer safe for a writer goroutine plus test reads. The
// tail tests read a command's output while the command is still running, which
// a bare bytes.Buffer cannot support.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hangingListener returns the address of a listener that accepts connections
// and never responds, so an RPC stays in flight until its deadline. Task 12
// needs the same fixture for its own deadline test, so it lives here once
// rather than being duplicated verbatim across tasks.
func hangingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	return ln.Addr().String()
}

// countingStubClient counts CreateStub calls, so a preflight test can assert
// that zero RPCs were sent rather than merely that no stubs exist.
type countingStubClient struct {
	adminv1connect.StubServiceClient
	creates int
}

func (c *countingStubClient) CreateStub(ctx context.Context, req *connect.Request[adminv1.CreateStubRequest]) (*connect.Response[adminv1.CreateStubResponse], error) {
	c.creates++
	return c.StubServiceClient.CreateStub(ctx, req)
}

// countingFactory does not need srv or t: it only wraps whatever client
// newAdminClient builds from cmd's own flags.
func countingFactory(counter *countingStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		counter.StubServiceClient = client.Stub
		client.Stub = counter
		return client, nil
	}
}

// failingDeleteStubClient fails every DeleteStub, which a healthy server never
// does, so the incomplete-rollback path is reachable.
type failingDeleteStubClient struct {
	adminv1connect.StubServiceClient
}

func (c *failingDeleteStubClient) DeleteStub(context.Context, *connect.Request[adminv1.DeleteStubRequest]) (*connect.Response[adminv1.DeleteStubResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("injected delete failure"))
}

// failingFactory does not need srv or t for the same reason countingFactory
// does not.
func failingFactory(failing *failingDeleteStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		failing.StubServiceClient = client.Stub
		client.Stub = failing
		return client, nil
	}
}

// losingCreateStubClient performs the real create and then throws the response
// away, reporting DEADLINE_EXCEEDED instead.
//
// This is the one failure a healthy server cannot stage: the stub genuinely
// lands in the store, and the client never learns its id, so it cannot be
// compensated for and cannot be named. It is what a dropped connection or an
// expired deadline leaves behind, and the only way to reach the branch that
// must not claim a clean rollback (design §6.1, §8).
type losingCreateStubClient struct {
	adminv1connect.StubServiceClient
}

func (c *losingCreateStubClient) CreateStub(ctx context.Context, req *connect.Request[adminv1.CreateStubRequest]) (*connect.Response[adminv1.CreateStubResponse], error) {
	if _, err := c.StubServiceClient.CreateStub(ctx, req); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeDeadlineExceeded,
		errors.New("injected lost response"))
}

// losingFactory does not need srv or t, for the same reason countingFactory
// does not.
func losingFactory(losing *losingCreateStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		losing.StubServiceClient = client.Stub
		client.Stub = losing
		return client, nil
	}
}

// cancelAfterCreateStubClient cancels the command's context once a stub has
// been created, reproducing the state a timed-out or interrupted create leaves:
// rollback must still run.
type cancelAfterCreateStubClient struct {
	adminv1connect.StubServiceClient
	cancel context.CancelFunc
}

func (c *cancelAfterCreateStubClient) CreateStub(ctx context.Context, req *connect.Request[adminv1.CreateStubRequest]) (*connect.Response[adminv1.CreateStubResponse], error) {
	resp, err := c.StubServiceClient.CreateStub(ctx, req)
	if err == nil && c.cancel != nil {
		c.cancel()
	}
	return resp, err
}

// cancellingFactory needs t to register the context's cancel with t.Cleanup;
// it does not need srv.
func cancellingFactory(t *testing.T, cancelling *cancelAfterCreateStubClient) clientFactory {
	return func(cmd *cobra.Command) (*adminClient, error) {
		client, err := newAdminClient(cmd)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		cmd.SetContext(ctx)
		cancelling.cancel = cancel
		t.Cleanup(cancel)
		cancelling.StubServiceClient = client.Stub
		client.Stub = cancelling
		return client, nil
	}
}

// recordDataPlaneCall makes one real GetOrder call against srv's data plane.
// This is how an internal/cli test produces a journal entry: srv.journal is
// unexported, so tests cannot record directly — and a real call is the fixture
// a user's journal is actually filled by.
//
// It installs a matching stub first, so the call is answered rather than
// failing UNIMPLEMENTED.
func recordDataPlaneCall(t *testing.T, srv *server.Server, orderID string) {
	t.Helper()
	createStub(t, srv, fmt.Sprintf(
		"method: shop.v1.OrderService/GetOrder\nmatch:\n  message:\n    order_id: { eq: %s }\nrespond:\n  message: { order_id: %s }\n",
		orderID, orderID))

	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	desc, err := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}

	conn, err := grpc.NewClient(srv.DataAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	req := dynamicpb.NewMessage(desc.Input())
	req.Set(desc.Input().Fields().ByName("order_id"), protoreflect.ValueOfString(orderID))
	resp := dynamicpb.NewMessage(desc.Output())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/shop.v1.OrderService/GetOrder", req, resp); err != nil {
		t.Fatalf("data-plane GetOrder(%s): %v", orderID, err)
	}
}
