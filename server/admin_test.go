package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

// startWithAdmin starts a server with both planes on ephemeral ports. Every
// test binds :0 — the default admin port :6566 must never appear in a test, or
// parallel runs collide.
func startWithAdmin(t *testing.T, opts Options) *Server {
	t.Helper()
	if len(opts.ProtoDirs) == 0 && len(opts.DescriptorSetPaths) == 0 {
		opts.ProtoDirs = []string{"../testdata/protos"}
	}
	if opts.DataAddr == "" {
		opts.DataAddr = "127.0.0.1:0"
	}
	if opts.AdminAddr == "" {
		opts.AdminAddr = "127.0.0.1:0"
	}
	if opts.JournalSize == 0 {
		opts.JournalSize = 16
	}
	srv, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// adminClient speaks the Connect protocol over HTTP/1.1 — the path the CLI and
// Go SDK use. Its own transport keeps idle connections from leaking between
// tests.
func adminClient(t *testing.T, srv *Server) adminv1connect.ControlServiceClient {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	return adminv1connect.NewControlServiceClient(client, "http://"+srv.AdminAddr().String())
}

// adminGRPCConn dials the admin plane the way a native gRPC client does:
// cleartext HTTP/2 with prior knowledge, which h2c serves by hijacking the
// connection.
func adminGRPCConn(t *testing.T, srv *Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(srv.AdminAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func getServerInfoOverGRPC(ctx context.Context, conn *grpc.ClientConn) (*adminv1.GetServerInfoResponse, error) {
	out := &adminv1.GetServerInfoResponse{}
	err := conn.Invoke(ctx, adminv1connect.ControlServiceGetServerInfoProcedure,
		&adminv1.GetServerInfoRequest{}, out)
	return out, err
}

// holdActiveHTTP1Request opens a connection to the admin plane and writes a
// complete request head whose Content-Length body never arrives. net/http
// treats a connection as active from the first byte read, so
// http.Server.Shutdown blocks on it — the case that makes step 1 of teardown
// need a context that force cancels.
//
// The settle sleep is the one soft spot: the connection is tracked at accept,
// which is observable, but "active" happens when the server's read loop
// consumes the head, which is not. Too short a settle affects this helper's
// two callers differently. TestLongLivedAdminRequestDoesNotHoldTeardownOpen
// only goes vacuous — teardown finishes early and its assertion passes
// trivially. TestStopEscalatesAdminShutdownBlockedOnHTTP1 can actually flake:
// it t.Fatals if GracefulStop returns within 200ms, and if net/http has not
// yet marked the connection active, Shutdown closes it as idle and that
// fatal fires.
func holdActiveHTTP1Request(t *testing.T, srv *Server) {
	t.Helper()
	addr := srv.AdminAddr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial admin plane: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	head := "POST " + adminv1connect.ControlServiceResetProcedure + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Type: application/proto\r\n" +
		"Content-Length: 64\r\n" +
		"\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatalf("write request head: %v", err)
	}
	waitFor(t, 2*time.Second, "the admin listener to track the connection",
		func() bool { return srv.adminLis.count() > 0 })
	time.Sleep(200 * time.Millisecond)
}

// recordingListener notes whether it was closed, so a failed Start can be
// checked for a leaked listener.
type recordingListener struct {
	net.Listener
	mu     sync.Mutex
	closed bool
}

func (l *recordingListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.Listener.Close()
}

func (l *recordingListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// recordingReporter collects every message the server reports.
type recordingReporter struct{ messages chan string }

func newRecordingReporter() *recordingReporter {
	return &recordingReporter{messages: make(chan string, 64)}
}

func (r *recordingReporter) Printf(format string, args ...any) {
	select {
	case r.messages <- format:
	default:
	}
}

func (r *recordingReporter) PrintErrln(args ...any) {
	select {
	case r.messages <- "err":
	default:
	}
}

// M3 §14 lists grpc-java <-> connect-go h2c as a top risk deferred to Phase 9.
// A native grpc-go client proves the transport now, when a fix is cheap,
// rather than discovering it inside the milestone's exit criterion.
//
// The count assertions below pin GetServerInfo to the *running server's* own
// registry, store and journal rather than to some other instance. Before they
// existed, this test asserted only Version, AdminAddr and DataAddr — three of
// GetServerInfo's seven fields — so startAdminPlane handing admin.Deps a
// fresh stub.NewStore() or journal.New(n) instead of the server's own
// s.store/s.journal would still have left the whole suite green. Starting
// with a stub directory (rather than Options{}) is what makes the StubCount
// assertion discriminate: an assertion of 0 == 0 would not catch a mis-wired
// store.
func TestAdminPlaneServesNativeGRPCClientOverH2C(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
`), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	const journalSize = 16
	srv := startWithAdmin(t, Options{StubDirs: []string{dir}, JournalSize: journalSize})
	conn := adminGRPCConn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := getServerInfoOverGRPC(ctx, conn)
	if err != nil {
		t.Fatalf("GetServerInfo over h2c: %v", err)
	}
	if info.Version != Version {
		t.Errorf("version = %q, want %q", info.Version, Version)
	}
	if info.AdminAddr != srv.AdminAddr().String() {
		t.Errorf("admin_addr = %q, want %q", info.AdminAddr, srv.AdminAddr().String())
	}
	if info.DataAddr != srv.DataAddr().String() {
		t.Errorf("data_addr = %q, want %q", info.DataAddr, srv.DataAddr().String())
	}
	if info.ServiceCount != int32(srv.ServiceCount()) {
		t.Errorf("service_count = %d, want %d (srv.ServiceCount())", info.ServiceCount, srv.ServiceCount())
	}
	if got := srv.StubCount(); got == 0 {
		t.Fatal("precondition: StubCount = 0, so this assertion could not catch a mis-wired store")
	}
	if info.StubCount != int32(srv.StubCount()) {
		t.Errorf("stub_count = %d, want %d (srv.StubCount())", info.StubCount, srv.StubCount())
	}
	if info.JournalCapacity != int32(journalSize) {
		t.Errorf("journal_capacity = %d, want %d (the JournalSize the server was started with)",
			info.JournalCapacity, journalSize)
	}
}

func TestAdminPlaneServesHealthz(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	resp, err := http.Get("http://" + srv.AdminAddr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("/healthz body = %q, want %q", body, "ok")
	}
}

func TestAdminAddrIsNilWhenDisabled(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	if got := srv.AdminAddr(); got != nil {
		t.Fatalf("AdminAddr with the admin plane disabled = %v, want nil", got)
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want %q — M4 sets a real one via ldflags", Version, "dev")
	}
}

// A server asked for a control plane that cannot provide one must not start
// half-working.
func TestAdminBindFailureIsFatalAndLeavesNothingRunning(t *testing.T) {
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "stub.yaml")
	if err := os.WriteFile(stubPath, []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: one }
`), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	bindErr := errors.New("admin bind refused for test")
	reporter := newRecordingReporter()
	var dataLis *recordingListener
	calls := 0
	_, err := Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		Watch:       true,
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
		Reporter:    reporter,
		// Start binds the data listener first and the admin listener second
		// (design §3.2), so call 2 is the admin bind.
		Listen: func(network, address string) (net.Listener, error) {
			calls++
			if calls == 1 {
				inner, lerr := net.Listen(network, address)
				if lerr != nil {
					return nil, lerr
				}
				dataLis = &recordingListener{Listener: inner}
				return dataLis, nil
			}
			return nil, bindErr
		},
	})
	if err == nil {
		t.Fatal("Start with a failing admin bind succeeded; it must be fatal")
	}
	if !errors.Is(err, bindErr) {
		t.Fatalf("Start error = %v, want it to wrap %v", err, bindErr)
	}
	if dataLis == nil {
		t.Fatal("the data listener was never bound; check the Listen call order assumption")
	}
	if !dataLis.isClosed() {
		t.Fatal("the data listener was left open after a fatal admin bind failure")
	}

	// A watcher left running would reload on the next change and report it.
	drainMessages(reporter)
	if err := os.WriteFile(stubPath, []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: two }
`), 0o644); err != nil {
		t.Fatalf("rewrite stub: %v", err)
	}
	select {
	case msg := <-reporter.messages:
		t.Fatalf("the stub watcher is still running after a fatal admin bind failure: %q", msg)
	case <-time.After(time.Second):
	}
}

func drainMessages(r *recordingReporter) {
	for {
		select {
		case <-r.messages:
		default:
			return
		}
	}
}

// The discriminating test. Asserting only that Shutdown returned, or that a
// fresh dial is refused, passes against the broken mechanism: the surviving
// h2c connection is invisible to both. This holds one open across shutdown and
// tries again on that same connection.
//
// The elapsed-time bound on GracefulStop+Wait is what makes the retried-RPC
// assertion below actually load-bearing. Without it, a missing
// http2.ConfigureServer is invisible: http.Server.Shutdown still returns
// immediately (it closes listeners unconditionally), the warmed connection
// then sits undrained until the force-close backstop in step 3 kills it
// after the full shutdownGrace, and the retried RPC still fails — just
// roughly 5s slower. The force-close would silently absorb the defect into
// latency instead of a failure. With ConfigureServer wired correctly, an
// idle h2c connection drains to zero in milliseconds, so teardown finishes
// far inside the budget.
func TestAdminPlaneStopsServingAfterWait(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	conn := adminGRPCConn(t, srv)
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer warmCancel()
	if _, err := getServerInfoOverGRPC(warmCtx, conn); err != nil {
		t.Fatalf("warm-up GetServerInfo: %v", err)
	}

	start := time.Now()
	srv.GracefulStop()
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	// Against a fraction of the budget: taking the whole shutdownGrace is
	// exactly the bug (the force-close backstop masking a missing
	// http2.ConfigureServer), and it would otherwise pass as a merely slow
	// test.
	if elapsed := time.Since(start); elapsed > shutdownGrace/2 {
		t.Fatalf("GracefulStop+Wait took %v, want well under %v; the admin drain "+
			"likely fell through to the force-close backstop instead of draining "+
			"the warmed h2c connection on its own", elapsed, shutdownGrace/2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := getServerInfoOverGRPC(ctx, conn); err == nil {
		t.Fatal("the admin plane served an RPC on a pre-existing connection after Wait returned")
	}
}

// Phase 4b's WatchCalls makes this load-bearing: a client tailing calls holds a
// connection open indefinitely, and only the bound stops it holding teardown
// open too. Tested before the RPC that needs it exists.
func TestLongLivedAdminRequestDoesNotHoldTeardownOpen(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	holdActiveHTTP1Request(t, srv)

	start := time.Now()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("teardown was held open by a long-lived admin request")
	}
	if elapsed := time.Since(start); elapsed > shutdownGrace+2*time.Second {
		t.Fatalf("teardown took %v, want no more than about %v", elapsed, shutdownGrace)
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// startWithBreakableListeners starts both planes on listeners the test can
// break on demand. Start binds the data listener first and the admin listener
// second (design §3.2), which is how the two are told apart here.
//
// It also starts with a real stub directory and Watch: true so a listener
// failure injected here exercises supervise's watcher join
// (s.watcher.stop()), not just the cross-plane teardown: a server started
// with no watcher would leave that path uncovered by both failure-injection
// tests below.
func startWithBreakableListeners(t *testing.T) (srv *Server, data, adminL *breakableListener) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
`), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	data = newBreakableListener(t)
	adminL = newBreakableListener(t)
	calls := 0
	var err error
	srv, err = Start(context.Background(), Options{
		ProtoDirs:   []string{"../testdata/protos"},
		StubDirs:    []string{dir},
		Watch:       true,
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
		Listen: func(network, address string) (net.Listener, error) {
			calls++
			if calls == 1 {
				return data, nil
			}
			return adminL, nil
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, data, adminL
}

func TestDataPlaneFailureTearsDownAdminPlane(t *testing.T) {
	srv, data, _ := startWithBreakableListeners(t)
	data.breakNow()

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, errListenerBroken) {
			t.Fatalf("Wait = %v, want the originating error %v", err, errListenerBroken)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("Wait never returned after the data listener broke; the admin plane is holding it open")
	}
	// The admin plane must be gone too, not merely un-joined.
	if _, err := net.DialTimeout("tcp", srv.AdminAddr().String(), time.Second); err == nil {
		t.Fatal("the admin plane still accepts connections after the data plane died")
	}
}

func TestAdminPlaneFailureTearsDownDataPlane(t *testing.T) {
	srv, _, adminL := startWithBreakableListeners(t)
	adminL.breakNow()

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, errListenerBroken) {
			t.Fatalf("Wait = %v, want the originating error %v", err, errListenerBroken)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("Wait never returned after the admin listener broke; supervise is not watching it")
	}
	// The data plane must be gone too, not merely un-joined.
	if _, err := net.DialTimeout("tcp", srv.DataAddr().String(), time.Second); err == nil {
		t.Fatal("the data plane still accepts connections after the admin plane died")
	}
}

// An intentional stop must report nil, not a plane's incidental serve error.
func TestIntentionalStopReportsNoError(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	srv.GracefulStop()
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait after GracefulStop = %v, want nil", err)
	}
}

// The HTTP/1.1 detail is what makes this cover step 1 of the sequence: an h2c
// connection leaves adminSrv.Shutdown returning instantly, so the blocking
// path would go untested and the bug would survive a green suite.
func TestStopEscalatesAdminShutdownBlockedOnHTTP1(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	holdActiveHTTP1Request(t, srv)

	gracefulDone := make(chan struct{})
	go func() { srv.GracefulStop(); close(gracefulDone) }()
	select {
	case <-gracefulDone:
		t.Fatal("GracefulStop returned instantly; the held request is not blocking teardown")
	case <-time.After(200 * time.Millisecond):
	}

	stopDone := make(chan struct{})
	go func() { srv.Stop(); close(stopDone) }()

	// Against a fraction of the budget: waiting the whole shutdownGrace is
	// exactly the bug, and it would otherwise pass as a merely slow test.
	bound := time.After(shutdownGrace / 2)
	for _, w := range []struct {
		name string
		done <-chan struct{}
	}{{"Stop", stopDone}, {"GracefulStop", gracefulDone}} {
		select {
		case <-w.done:
		case <-bound:
			t.Fatalf("%s did not return within %v; adminSrv.Shutdown is not force-responsive",
				w.name, shutdownGrace/2)
		}
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// The response flushes because the drain waits for its connection, not because
// adminSrv.Shutdown blocks. This asserts both halves: the client receives the
// response *and* the server subsequently stops.
func TestShutdownRPCRespondsThenTheServerStops(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	client := adminClient(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown RPC: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait after the Shutdown RPC = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server did not stop after the Shutdown RPC")
	}
}

// The same over h2c, where the connection is hijacked and the response path is
// the one http.Server.Shutdown cannot see at all.
func TestShutdownRPCOverH2CRespondsThenTheServerStops(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	conn := adminGRPCConn(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, adminv1connect.ControlServiceShutdownProcedure,
		&adminv1.ShutdownRequest{}, &adminv1.ShutdownResponse{}); err != nil {
		t.Fatalf("Shutdown over h2c: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait after the h2c Shutdown RPC = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server did not stop after the h2c Shutdown RPC")
	}
}

// Calling Server.Shutdown twice must be safe: begin()'s stopOnce makes
// repeated calls to begin() idempotent, so the second Shutdown here is a
// no-op on an already-torn-down server and still reports a clean result. The
// cross-entry-point case — the RPC and Server.Shutdown racing each other — is
// covered separately by TestShutdownRPCConcurrentWithServerShutdown.
func TestShutdownIsSafeToCallTwice(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	ctx1, cancel1 := context.WithTimeout(context.Background(), shutdownGrace+5*time.Second)
	defer cancel1()
	if err := srv.Shutdown(ctx1); err != nil {
		t.Fatalf("first Shutdown = %v, want nil", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := srv.Shutdown(ctx2); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}

func TestShutdownRPCConcurrentWithServerShutdown(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	client := adminClient(t, srv)

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace+5*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()
	// The RPC may or may not land before the admin plane stops accepting;
	// either outcome is fine. What must hold is that nothing deadlocks and
	// Wait reports a clean stop.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{}))

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Server.Shutdown = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatal("Server.Shutdown raced with the Shutdown RPC and deadlocked")
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// The test that fails if the policy ever regresses to unbounded graceful. An
// open bidi stream is exactly what a failed test leaves behind, and grpc's
// GracefulStop waits for it forever.
func TestShutdownWithOpenBidiStreamCompletesWithinTheGraceBound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(blockingBidiStub), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	srv := startWithAdmin(t, Options{StubDirs: []string{dir}})
	stream, desc := openChatStream(t, srv)
	if got := recvChatText(t, stream, desc); got != "open" {
		t.Fatalf("on_open text = %q, want %q", got, "open")
	}

	client := adminClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Shutdown(ctx, connect.NewRequest(&adminv1.ShutdownRequest{})); err != nil {
		t.Fatalf("Shutdown RPC: %v", err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server never stopped with an open bidi stream; the graceful wait is unbounded")
	}
	if elapsed := time.Since(start); elapsed > shutdownGrace+2*time.Second {
		t.Fatalf("teardown took %v, want no more than about %v", elapsed, shutdownGrace)
	}
}

// TestDataPlaneGetsGuaranteedGraceFloorWhenAdminDrainConsumesTheBudget pins
// dataGraceFloor. runTeardown runs the admin drain first, and a single stuck
// HTTP/1.1 request — the normal case once Phase 4b's WatchCalls lets an admin
// client hold a connection open — makes that drain burn the whole
// shutdownGrace budget. Without a floor, stopDataPlane would then receive an
// already-expired deadline and hard-kill the still-open bidi stream with zero
// graceful window.
//
// The lower-bound assertion below is the discriminating one. Without
// dataGraceFloor, the data phase contributes essentially nothing once the
// admin drain has already spent the whole budget, so elapsed lands at
// roughly shutdownGrace. With the floor, stopDataPlane is handed a deadline
// extended by dataGraceFloor, so it spends that whole guaranteed second
// blocked on the still-open Chat stream (grpc's GracefulStop cannot return
// with a stream open) before being force-stopped, landing elapsed at roughly
// shutdownGrace + dataGraceFloor. Do not simplify this lower bound away as a
// pointless minimum — it is the only assertion here that a reverted
// runTeardown fails.
func TestDataPlaneGetsGuaranteedGraceFloorWhenAdminDrainConsumesTheBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(blockingBidiStub), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	srv := startWithAdmin(t, Options{StubDirs: []string{dir}})

	stream, desc := openChatStream(t, srv)
	if got := recvChatText(t, stream, desc); got != "open" {
		t.Fatalf("on_open text = %q, want %q", got, "open")
	}
	holdActiveHTTP1Request(t, srv)

	start := time.Now()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace + dataGraceFloor + 10*time.Second):
		t.Fatal("teardown never completed with the admin drain consuming the full budget")
	}

	elapsed := time.Since(start)
	if elapsed > shutdownGrace+dataGraceFloor+2*time.Second {
		t.Fatalf("teardown took %v, want no more than about %v", elapsed, shutdownGrace+dataGraceFloor)
	}
	// The discriminating assertion: see the doc comment above.
	if elapsed < shutdownGrace+dataGraceFloor/2 {
		t.Fatalf("teardown took %v, want at least %v — the data plane did not get its "+
			"guaranteed dataGraceFloor window after the admin drain consumed the budget",
			elapsed, shutdownGrace+dataGraceFloor/2)
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// The container/SDK boot path: healthy first, schemas later.
func TestStartWithoutSchemaSourceSucceedsWhenAdminIsOn(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start with no schema source and the admin plane on: %v", err)
	}
	t.Cleanup(srv.Stop)

	resp, err := http.Get("http://" + srv.AdminAddr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// Every data-plane call answers Unimplemented until schemas arrive.
	conn, err := grpc.NewClient(srv.DataAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The request and response types are irrelevant: the server rejects the
	// call at method lookup, before it reads a message.
	err = conn.Invoke(ctx, "/shop.v1.OrderService/GetOrder",
		&adminv1.GetServerInfoRequest{}, &adminv1.GetServerInfoResponse{})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("data-plane call on a schema-less server = %v (code %v), want Unimplemented", err, got)
	}
}
