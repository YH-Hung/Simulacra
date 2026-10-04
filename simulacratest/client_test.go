package simulacratest_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/server"
	"github.com/yinghanhung/simulacra/simulacratest"
)

const getOrderDoc = `
method: shop.v1.OrderService/GetOrder
respond:
  message:
    note: from-api
`

func listStubs(t *testing.T, sim *simulacratest.Server, origin adminv1.StubOrigin) []*adminv1.Stub {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := sim.Admin().Stub.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{Origin: origin}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	return resp.Msg.GetStubs()
}

func TestStubYAMLAndDeleteStub(t *testing.T) {
	sim := simulacratest.Start(t, simulacratest.WithProtoDir("../testdata/protos"))

	stub := sim.StubYAML(t, getOrderDoc)
	if !strings.HasPrefix(stub.GetId(), "api-") {
		t.Fatalf("StubYAML id = %q; want an api- id", stub.GetId())
	}
	if got := listStubs(t, sim, adminv1.StubOrigin_STUB_ORIGIN_API); len(got) != 1 || got[0].GetId() != stub.GetId() {
		t.Fatalf("ListStubs = %v; want exactly stub %s", got, stub.GetId())
	}

	sim.DeleteStub(t, stub.GetId())
	if got := listStubs(t, sim, adminv1.StubOrigin_STUB_ORIGIN_API); len(got) != 0 {
		t.Fatalf("ListStubs after DeleteStub = %v; want none", got)
	}
}

func TestResetRestoresAPIStubsJournalAndFileBudgets(t *testing.T) {
	sim := simulacratest.Start(t,
		simulacratest.WithProtoDir("../testdata/protos"),
		simulacratest.WithStubDir("testdata/stubs"))
	orders := shopv1.NewOrderServiceClient(sim.Dial(t))
	getOrder := func() (*shopv1.GetOrderResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return orders.GetOrder(ctx, &shopv1.GetOrderRequest{OrderId: "o1"})
	}

	if resp, err := getOrder(); err != nil || resp.GetNote() != "from-file" {
		t.Fatalf("first GetOrder = %v, %v; want note from-file", resp, err)
	}
	if _, err := getOrder(); status.Code(err) != codes.NotFound {
		t.Fatalf("second GetOrder error = %v; want NOT_FOUND once the file stub's budget is spent", err)
	}
	sim.StubYAML(t, getOrderDoc)

	sim.Reset(t)

	if got := listStubs(t, sim, adminv1.StubOrigin_STUB_ORIGIN_API); len(got) != 0 {
		t.Errorf("API stubs after Reset = %v; want none", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls, err := sim.Admin().Journal.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if n := len(calls.Msg.GetCalls()); n != 0 {
		t.Errorf("journal after Reset holds %d call(s); want none", n)
	}
	if resp, err := getOrder(); err != nil || resp.GetNote() != "from-file" {
		t.Errorf("GetOrder after Reset = %v, %v; want note from-file again", resp, err)
	}
}

func TestHelpersWorkFromCleanup(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		sim := simulacratest.Start(t, simulacratest.WithProtoDir("../testdata/protos"))
		t.Cleanup(func() { sim.Reset(t) })
		t.Cleanup(func() { sim.StubYAML(t, getOrderDoc) })
		t.Cleanup(func() { sim.Verify(getOrder).AtLeast(t, 0) })
	})
}

func TestRPCTimeoutBoundsAHangingServer(t *testing.T) {
	rec := runRecorded(t, func(tb testing.TB) {
		c := simulacratest.Connect(tb, hangingListener(t), "")
		simulacratest.SetRPCTimeout(c, 100*time.Millisecond)
		c.Reset(tb)
	})
	for _, want := range []string{"simulacratest: Reset: ", "deadline_exceeded"} {
		if !strings.Contains(rec.FatalMsg(), want) {
			t.Errorf("FatalMsg() = %q; want it to contain %q", rec.FatalMsg(), want)
		}
	}
}

func TestConnectReleasesIdleConnectionsOnCleanup(t *testing.T) {
	// Listen is applied to the data listener first, then the admin listener
	// (server/server.go), so the admin plane's counter is the second one.
	var listeners []*closeCountingListener
	srv, err := server.Start(context.Background(), server.Options{
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
		Listen: func(network, address string) (net.Listener, error) {
			ln, err := net.Listen(network, address)
			if err != nil {
				return nil, err
			}
			counted := newCloseCountingListener(ln)
			listeners = append(listeners, counted)
			return counted, nil
		},
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	admin := listeners[1]

	t.Run("inner", func(t *testing.T) {
		c := simulacratest.Connect(t, srv.AdminAddr().String(), srv.DataAddr().String())
		c.Reset(t)
	})
	admin.waitAllClosed(t, 5*time.Second)
}
