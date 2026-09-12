package admin_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// testDeps builds a complete Deps over real core objects. Tests that assert on
// specific counts overwrite only the fields they care about.
func testDeps(t *testing.T) admin.Deps {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	return admin.Deps{
		Registry:  reg,
		Store:     stub.NewStore(),
		Journal:   journal.New(4),
		Version:   "test-version",
		DataAddr:  func() string { return "127.0.0.1:6565" },
		AdminAddr: func() string { return "127.0.0.1:6566" },
		Shutdown:  func() {},
		Stopping:  make(chan struct{}),
	}
}

// installed installs the control plane on a throwaway http.Server and serves
// its handler over plain HTTP/1.1. Every handler test runs this way: no ports,
// no h2c, no server facade. The h2c and lifecycle behavior is proven in
// server/admin_test.go, where it actually lives.
func installed(t *testing.T, deps admin.Deps) *httptest.Server {
	t.Helper()
	srv := &http.Server{}
	if err := admin.Install(srv, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestInstallServesHealthz(t *testing.T) {
	ts := installed(t, testDeps(t))
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /healthz body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("/healthz body = %q, want %q", body, "ok")
	}
}

// Every service is mounted. The assertion is "not 404", not "returns
// Unimplemented", so it keeps passing unchanged as the handlers are filled in.
func TestInstallMountsEveryServiceRoute(t *testing.T) {
	ts := installed(t, testDeps(t))
	for _, procedure := range []string{
		adminv1connect.ControlServiceGetServerInfoProcedure,
		adminv1connect.SchemaServiceListServicesProcedure,
		adminv1connect.StubServiceListStubsProcedure,
		adminv1connect.JournalServiceListCallsProcedure,
		adminv1connect.VerifyServiceVerifyCallsProcedure,
	} {
		resp, err := ts.Client().Post(ts.URL+procedure, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", procedure, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s is not mounted: 404", procedure)
		}
	}
}

// http2.ConfigureServer registers the graceful-shutdown hook that reaches
// hijacked h2c connections. Without it the admin plane keeps serving after the
// server reports it stopped. The behavioral proof is
// TestAdminPlaneStopsServingAfterWait in server/; this one fails fast, in the
// package that owns the call, so a refactor that drops it is caught here first.
func TestInstallConfiguresHTTP2(t *testing.T) {
	srv := &http.Server{}
	if err := admin.Install(srv, testDeps(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := srv.TLSNextProto["h2"]; !ok {
		t.Fatal(`srv.TLSNextProto has no "h2" entry; http2.ConfigureServer was not called`)
	}
}

func TestInstallRejectsIncompleteDeps(t *testing.T) {
	unset := map[string]func(*admin.Deps){
		"Registry":  func(d *admin.Deps) { d.Registry = nil },
		"Store":     func(d *admin.Deps) { d.Store = nil },
		"Journal":   func(d *admin.Deps) { d.Journal = nil },
		"Version":   func(d *admin.Deps) { d.Version = "" },
		"DataAddr":  func(d *admin.Deps) { d.DataAddr = nil },
		"AdminAddr": func(d *admin.Deps) { d.AdminAddr = nil },
		"Shutdown":  func(d *admin.Deps) { d.Shutdown = nil },
		"Stopping":  func(d *admin.Deps) { d.Stopping = nil },
	}
	for field, clearField := range unset {
		t.Run(field, func(t *testing.T) {
			deps := testDeps(t)
			clearField(&deps)
			err := admin.Install(&http.Server{}, deps)
			if err == nil {
				t.Fatalf("Install with %s unset returned a nil error", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("Install error = %q, want it to name the missing field %q", err, field)
			}
		})
	}
}

func TestInstallRejectsNilServer(t *testing.T) {
	if err := admin.Install(nil, testDeps(t)); err == nil {
		t.Fatal("Install(nil, deps) returned a nil error")
	}
}

// servedOverTCP installs the control plane on a real TCP listener, which the
// h2c handshake tests need: httptest's HTTP/1.1 client plumbing cannot send a
// raw prior-knowledge preface. Returns the address and a func reporting how
// many accepted connections are still open.
func servedOverTCP(t *testing.T, deps admin.Deps) (addr string, openConns func() int) {
	t.Helper()
	srv := &http.Server{}
	if err := admin.Install(srv, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	counted := &countingListener{Listener: inner, conns: map[net.Conn]struct{}{}}
	go func() { _ = srv.Serve(counted) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = inner.Close()
	})
	return inner.Addr().String(), counted.open
}

type countingListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &countedConn{Conn: c, lis: l}
	l.mu.Lock()
	l.conns[wrapped] = struct{}{}
	l.mu.Unlock()
	return wrapped, nil
}

func (l *countingListener) open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

type countedConn struct {
	net.Conn
	lis  *countingListener
	once sync.Once
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.lis.mu.Lock()
		delete(c.lis.conns, c)
		c.lis.mu.Unlock()
	})
	return err
}

// writePrefaceProbe sends only the first half of the h2c client preface. The
// second half, "SM\r\n\r\n", never arrives.
func writePrefaceProbe(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "PRI * HTTP/2.0\r\n\r\n"); err != nil {
		t.Fatalf("write preface probe: %v", err)
	}
	return conn
}

func waitForOpenConns(t *testing.T, open func() int, want int, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := open(); got == want {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s: %d connection(s) still open after %v, want %d", what, open(), timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A client that opens the h2c handshake and then vanishes must not leave the
// connection behind. x/net's h2c returns from its preface read without closing
// the connection it hijacked, and the caller returns before installing its own
// deferred close — so against the unpatched path this connection stayed open
// past 11s, outliving ReadHeaderTimeout, which hijacking had already cleared.
func TestTruncatedH2CPrefaceDoesNotLeakOnDisconnect(t *testing.T) {
	addr, open := servedOverTCP(t, testDeps(t))
	conn := writePrefaceProbe(t, addr)
	waitForOpenConns(t, open, 1, 2*time.Second, "preface probe was never accepted")

	if err := conn.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}
	waitForOpenConns(t, open, 0, 3*time.Second, "connection leaked after the client disconnected")
}

// The slowloris variant: the client stays connected and simply never completes
// the preface. Only the handshake bound closes this one.
func TestTruncatedH2CPrefaceTimesOutWhenHeld(t *testing.T) {
	t.Cleanup(admin.SetHandshakeTimeout(250 * time.Millisecond))
	addr, open := servedOverTCP(t, testDeps(t))
	writePrefaceProbe(t, addr)
	waitForOpenConns(t, open, 1, 2*time.Second, "preface probe was never accepted")
	waitForOpenConns(t, open, 0, 5*time.Second, "held handshake was never bounded")
}

// A completed handshake must still work, and must not inherit the bound: the
// connection is a live h2 session from that point on.
func TestCompletedH2CHandshakeStillServes(t *testing.T) {
	t.Cleanup(admin.SetHandshakeTimeout(250 * time.Millisecond))
	addr, _ := servedOverTCP(t, testDeps(t))

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := &adminv1.GetServerInfoResponse{}
	if err := conn.Invoke(ctx, adminv1connect.ControlServiceGetServerInfoProcedure,
		&adminv1.GetServerInfoRequest{}, out); err != nil {
		t.Fatalf("GetServerInfo over h2c: %v", err)
	}
	if out.Version != "test-version" {
		t.Fatalf("version = %q, want %q", out.Version, "test-version")
	}

	// Well past the handshake bound: a session that inherited it would be dead.
	time.Sleep(600 * time.Millisecond)
	if err := conn.Invoke(ctx, adminv1connect.ControlServiceGetServerInfoProcedure,
		&adminv1.GetServerInfoRequest{}, out); err != nil {
		t.Fatalf("second GetServerInfo after the handshake bound elapsed: %v", err)
	}
}

// An oversized request must be refused rather than allocated. Unlimited is the
// connect default, and a probe drove an 8 MiB body to a 200 before this cap.
func TestOversizedRequestIsRejected(t *testing.T) {
	ts := installed(t, testDeps(t))
	url := ts.URL + adminv1connect.ControlServiceGetServerInfoProcedure

	// A GetServerInfoRequest carrying its payload in an unknown field: it would
	// decode and answer normally if it were allowed through, so a rejection can
	// only come from the size cap.
	oversized := protowire.AppendTag(nil, 1000, protowire.BytesType)
	oversized = protowire.AppendBytes(oversized, bytes.Repeat([]byte{0x41}, 8<<20))

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(oversized))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := ts.Client().Do(req)
	if err != nil {
		// A refusal that closes the connection is also a rejection.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("an %d-byte request returned 200; the size cap is not applied", len(oversized))
	}
}

// The cap must not disturb ordinary traffic.
func TestNormalSizedRequestStillSucceeds(t *testing.T) {
	client, _ := controlClient(t, testDeps(t))
	if _, err := client.GetServerInfo(context.Background(),
		connect.NewRequest(&adminv1.GetServerInfoRequest{})); err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
}

// padded appends an unknown field of n bytes to msg. Decoding skips it, so the
// request means exactly what it meant before; only a size cap can tell the two
// apart.
func padded[T proto.Message](msg T, n int) T {
	field := protowire.AppendTag(nil, 1000, protowire.BytesType)
	msg.ProtoReflect().SetUnknown(protowire.AppendBytes(field, bytes.Repeat([]byte{'A'}, n)))
	return msg
}

// Each service caps its own requests (design §8). SchemaService takes whole
// descriptor sets and accepts up to 32 MiB; every other service stays at 4 MiB.
// A request the cap lets through is decoded and answered — whatever the answer,
// it is not ResourceExhausted.
func TestRequestSizeCapsArePerService(t *testing.T) {
	ts := installed(t, testDeps(t))
	ctx := context.Background()
	schemas := adminv1connect.NewSchemaServiceClient(ts.Client(), ts.URL)

	_, err := schemas.RegisterSchemas(ctx, connect.NewRequest(padded(&adminv1.RegisterSchemasRequest{}, 8<<20)))
	if code := connect.CodeOf(err); err != nil && (code == connect.CodeResourceExhausted || code == connect.CodeUnknown) {
		t.Errorf("an 8 MiB RegisterSchemas request = %v; SchemaService must accept it", err)
	}
	_, err = schemas.RegisterSchemas(ctx, connect.NewRequest(padded(&adminv1.RegisterSchemasRequest{}, 40<<20)))
	if code := connect.CodeOf(err); code != connect.CodeResourceExhausted {
		t.Errorf("a 40 MiB RegisterSchemas request = %v (code %v), want ResourceExhausted", err, code)
	}

	control := adminv1connect.NewControlServiceClient(ts.Client(), ts.URL)
	stubs := adminv1connect.NewStubServiceClient(ts.Client(), ts.URL)
	journals := adminv1connect.NewJournalServiceClient(ts.Client(), ts.URL)
	verify := adminv1connect.NewVerifyServiceClient(ts.Client(), ts.URL)
	const oversized = 8 << 20
	others := map[string]struct {
		call  func() error
		codes []connect.Code // any one of these is accepted
	}{
		"ControlService.GetServerInfo": {
			call: func() error {
				_, err := control.GetServerInfo(ctx, connect.NewRequest(padded(&adminv1.GetServerInfoRequest{}, oversized)))
				return err
			},
			codes: []connect.Code{connect.CodeResourceExhausted},
		},
		"StubService.ReplaceAllStubs": {
			call: func() error {
				_, err := stubs.ReplaceAllStubs(ctx, connect.NewRequest(padded(&adminv1.ReplaceAllStubsRequest{}, oversized)))
				return err
			},
			codes: []connect.Code{connect.CodeResourceExhausted},
		},
		"JournalService.WatchCalls": {
			call: func() error {
				// Bounded: a WatchCalls request the cap wrongly admits stays open.
				watchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				stream, err := journals.WatchCalls(watchCtx, connect.NewRequest(padded(&adminv1.WatchCallsRequest{}, oversized)))
				if err != nil {
					return err
				}
				defer stream.Close()
				for stream.Receive() {
				}
				return stream.Err()
			},
			// Flaky as a single code: over the streaming protocol the server can
			// reject the oversized request while the client is still writing the
			// 8 MiB body, so the client sometimes observes the broken transport
			// (CodeInternal) before it ever reads the server's actual status
			// (CodeResourceExhausted). Either way the request was rejected, which
			// is the only thing production behavior promises here — a stream that
			// the cap wrongly admits instead blocks until the context above
			// expires, returning CodeDeadlineExceeded, which is neither of these.
			codes: []connect.Code{connect.CodeResourceExhausted, connect.CodeInternal},
		},
		"VerifyService.VerifyCalls": {
			call: func() error {
				_, err := verify.VerifyCalls(ctx, connect.NewRequest(padded(&adminv1.VerifyCallsRequest{}, oversized)))
				return err
			},
			codes: []connect.Code{connect.CodeResourceExhausted},
		},
	}
	for name, c := range others {
		code := connect.CodeOf(c.call())
		if !slices.Contains(c.codes, code) {
			t.Errorf("an 8 MiB %s request = code %v, want one of %v", name, code, c.codes)
		}
	}
}
