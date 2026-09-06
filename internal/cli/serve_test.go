package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/server"
)

func TestServeJournalSizeFlagDefaultsTo1024(t *testing.T) {
	cmd := newServeCmd()
	flag := cmd.Flags().Lookup("journal-size")
	if flag == nil {
		t.Fatal("--journal-size flag is missing")
	}
	if flag.DefValue != "1024" {
		t.Fatalf("--journal-size default = %q, want 1024", flag.DefValue)
	}
}

func TestServeWatchFlagDefaultsTrue(t *testing.T) {
	cmd := newServeCmd()
	flag := cmd.Flags().Lookup("watch")
	if flag == nil {
		t.Fatal("--watch flag is missing")
	}
	if flag.DefValue != "true" {
		t.Fatalf("--watch default = %q, want true", flag.DefValue)
	}
	if err := cmd.Flags().Set("watch", "false"); err != nil {
		t.Fatalf("set --watch=false: %v", err)
	}
}

func TestServeExecuteContextCancellationReturnsWithinBound(t *testing.T) {
	listener := newBlockingListener()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newServeCmdWithListen(func(string, string) (net.Listener, error) { return listener, nil })
	ready := make(chan struct{})
	writer := &notifyWriter{needle: "data plane listening", seen: ready}
	cmd.SetOut(writer)
	cmd.SetErr(writer)
	cmd.SetArgs([]string{
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0",
		"--admin", "off",
		"--watch=false",
	})
	result := make(chan error, 1)
	go func() { result <- cmd.ExecuteContext(ctx) }()
	select {
	case <-ready:
	case err := <-result:
		t.Fatalf("serve returned before becoming ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not become ready")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ExecuteContext returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecuteContext did not return within one second of cancellation")
	}
}

type blockingListener struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingListener() *blockingListener {
	return &blockingListener{closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingListener) Addr() net.Addr { return fixedAddr("test-listener") }

type fixedAddr string

func (a fixedAddr) Network() string { return "test" }
func (a fixedAddr) String() string  { return string(a) }

type notifyWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	needle string
	seen   chan struct{}
	once   sync.Once
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.needle) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func TestCommandOutputSerializesConcurrentWrites(t *testing.T) {
	cmd := &cobra.Command{}
	var buffer bytes.Buffer
	cmd.SetOut(&buffer)
	cmd.SetErr(&buffer)
	output := &commandOutput{cmd: cmd}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			output.Printf("out %d\n", i)
			output.PrintErrln("err", i)
		}()
	}
	wg.Wait()
}

func TestServeWithRuntimeCancelsAndJoinsSignalWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waiterCanceled := make(chan struct{})
	releaseWaiter := make(chan struct{})
	waiterDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(waiterCanceled)
		<-releaseWaiter
		close(waiterDone)
	}()

	returned := make(chan struct{})
	go func() {
		_ = serveWithRuntime(cancel, waiterDone, func() error { return nil })
		close(returned)
	}()
	select {
	case <-waiterCanceled:
	case <-time.After(time.Second):
		t.Fatal("signal waiter context was not canceled after Serve returned")
	}
	select {
	case <-returned:
		t.Fatal("serveWithRuntime returned before signal waiter exited")
	default:
	}
	close(releaseWaiter)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("serveWithRuntime did not join signal waiter")
	}
}

func TestWaitAndShutdownRunsGracefulOnSignal(t *testing.T) {
	canceled := make(chan struct{})
	graceful := func() { close(canceled) }
	sig := make(chan os.Signal, 1)
	shutdownDone := make(chan struct{})
	go func() {
		waitAndShutdown(sig, time.Second, graceful, func() {}, func(...any) {})
		close(shutdownDone)
	}()
	sig <- os.Interrupt
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("graceful shutdown was not invoked on signal")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("signal shutdown did not finish")
	}
}

func TestServeRejectsNonPositiveJournalSize(t *testing.T) {
	for _, size := range []string{"0", "-1"} {
		t.Run(size, func(t *testing.T) {
			cmd := newServeCmd()
			cmd.SetArgs([]string{"--journal-size", size})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "journal-size must be greater than zero") {
				t.Fatalf("Execute error = %v, want journal-size validation", err)
			}
		})
	}
}

func TestServeAdminFlagDefaultsTo6566(t *testing.T) {
	cmd := newServeCmd()
	flag := cmd.Flags().Lookup("admin")
	if flag == nil {
		t.Fatal("--admin flag is missing")
	}
	if flag.DefValue != "127.0.0.1:6566" {
		t.Fatalf("--admin default = %q, want %q", flag.DefValue, "127.0.0.1:6566")
	}
}

// runServe starts `serve` with args, waits for the readiness line, then cancels
// and returns the command's error and everything it printed.
func runServe(t *testing.T, listen func(string, string) (net.Listener, error), needle string, args ...string) (error, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newServeCmdWithListen(listen)
	ready := make(chan struct{})
	writer := &notifyWriter{needle: needle, seen: ready}
	cmd.SetOut(writer)
	cmd.SetErr(writer)
	cmd.SetArgs(args)
	result := make(chan error, 1)
	go func() { result <- cmd.ExecuteContext(ctx) }()
	select {
	case <-ready:
	case err := <-result:
		cancel()
		t.Fatalf("serve returned before printing %q: %v", needle, err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("serve never printed %q", needle)
	}
	cancel()
	select {
	case err := <-result:
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return err, writer.buf.String()
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not return within 20s of cancellation")
		return nil, ""
	}
}

func TestServeAdminOffBindsOnlyTheDataPlane(t *testing.T) {
	var binds atomic.Int32
	listen := func(network, address string) (net.Listener, error) {
		binds.Add(1)
		return net.Listen(network, address)
	}
	err, out := runServe(t, listen, "data plane listening",
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0",
		"--admin", "off",
		"--watch=false",
	)
	if err != nil {
		t.Fatalf("ExecuteContext = %v", err)
	}
	if got := binds.Load(); got != 1 {
		t.Fatalf("bound %d listener(s) with --admin off, want 1", got)
	}
	if strings.Contains(out, "admin plane") {
		t.Fatalf("output mentions the admin plane with --admin off:\n%s", out)
	}
}

func TestServeAdminAddressIsHonoredAndAnnounced(t *testing.T) {
	var binds atomic.Int32
	listen := func(network, address string) (net.Listener, error) {
		binds.Add(1)
		return net.Listen(network, address)
	}
	err, out := runServe(t, listen, "admin plane listening",
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0",
		"--admin", "127.0.0.1:0",
		"--watch=false",
	)
	if err != nil {
		t.Fatalf("ExecuteContext = %v", err)
	}
	if got := binds.Load(); got != 2 {
		t.Fatalf("bound %d listener(s) with --admin set, want 2", got)
	}
	if !strings.Contains(out, "admin plane listening on 127.0.0.1:") {
		t.Fatalf("output does not announce the bound admin address:\n%s", out)
	}
}

// The §5 redefinition, end to end: the CLI's signal waiter drives the
// whole-server GracefulStop/Stop pair and Wait returns.
func TestSignalShutdownStopsBothPlanes(t *testing.T) {
	srv, err := server.Start(context.Background(), server.Options{
		ProtoDirs:   []string{"../../testdata/protos"},
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	if srv.AdminAddr() == nil {
		t.Fatal("AdminAddr = nil, want a bound admin listener")
	}

	sig := make(chan os.Signal, 2)
	done := make(chan struct{})
	go func() {
		waitAndShutdown(sig, 10*time.Second, srv.GracefulStop, srv.Stop, func(...any) {})
		close(done)
	}()
	sig <- os.Interrupt
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("signal shutdown did not complete with the admin plane enabled")
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if _, err := net.DialTimeout("tcp", srv.AdminAddr().String(), time.Second); err == nil {
		t.Fatal("the admin plane still accepts connections after the signal shutdown")
	}
}

func TestCheckStillRequiresSchemaSources(t *testing.T) {
	cmd := newCheckCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--stubs", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "at least one schema source is required") {
		t.Fatalf("check error = %v, want a schema-source-required error", err)
	}
}
