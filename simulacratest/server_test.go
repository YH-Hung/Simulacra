package simulacratest_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

func TestStartServesBothPlanesAndShutsDownOnCleanup(t *testing.T) {
	var admin string
	t.Run("inner", func(t *testing.T) {
		sim := simulacratest.Start(t, simulacratest.WithProtoDir("../testdata/protos"))
		admin = sim.AdminAddr()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info, err := sim.Admin().Control.GetServerInfo(ctx, connect.NewRequest(&adminv1.GetServerInfoRequest{}))
		if err != nil || info.Msg.GetDataAddr() != sim.DataAddr() {
			t.Fatalf("GetServerInfo = %v, %v; want data addr %s", info, err, sim.DataAddr())
		}
	})
	if c, err := net.Dial("tcp", admin); err == nil { // shutdown ran with the subtest
		c.Close()
		t.Fatalf("admin plane %s still accepting after the test ended", admin)
	}
}

func TestStartReportsServerErrors(t *testing.T) {
	rec := runRecorded(t, func(tb testing.TB) {
		simulacratest.Start(tb, simulacratest.WithProtoDir("no-such-dir"))
	})
	if !strings.HasPrefix(rec.FatalMsg(), "simulacratest: Start: ") {
		t.Fatalf("FatalMsg() = %q; want the prefix %q", rec.FatalMsg(), "simulacratest: Start: ")
	}
}

func TestDialConnectionIsClosedWhenTheTestEnds(t *testing.T) {
	var conn *grpc.ClientConn
	t.Run("inner", func(t *testing.T) {
		sim := simulacratest.Start(t, simulacratest.WithProtoDir("../testdata/protos"))
		conn = sim.Dial(t)
		if got := conn.Target(); !strings.HasSuffix(got, sim.DataAddr()) {
			t.Fatalf("Dial target = %q; want the data address %s", got, sim.DataAddr())
		}
	})
	if got := conn.GetState(); got != connectivity.Shutdown {
		t.Fatalf("connection state after the test = %v; want %v", got, connectivity.Shutdown)
	}
}
