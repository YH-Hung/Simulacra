package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/stub"
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

func TestReloadStubDirsKeepsInvalidStoreAndResetsValidBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	valid := `
- method: shop.v1.OrderService/GetOrder
  times: 1
  respond: { message: { note: fresh } }
`
	write(valid)
	src := &sources{protoDirs: []string{"../../testdata/protos"}}
	reg, err := src.buildRegistry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("initial Select = nil, want limited stub")
	}

	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	write("{{{ invalid yaml")
	reloadStubDirs(cmd, reg, store, []string{dir})
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after invalid reload = %v, want old exhausted store", got)
	}
	if !strings.Contains(output.String(), "stub error:") {
		t.Fatalf("invalid reload output = %q, want stub error", output.String())
	}

	output.Reset()
	write(valid)
	reloadStubDirs(cmd, reg, store, []string{dir})
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("Select after valid reload = nil, want reset times budget")
	}
	if !strings.Contains(output.String(), "1 stub(s) reloaded") {
		t.Fatalf("valid reload output = %q, want count", output.String())
	}
}

func TestReloadStubDirsContextDoesNothingAfterCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	if err := os.WriteFile(path, []byte(`
- method: shop.v1.OrderService/GetOrder
  times: 1
  respond: { message: {} }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &sources{protoDirs: []string{"../../testdata/protos"}}
	reg, err := src.buildRegistry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("initial limited stub did not select")
	}

	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	printer := &commandOutput{cmd: cmd}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reloadStubDirsContext(ctx, printer, reg, store, []string{dir})
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after canceled reload = %v, want exhausted original store", got)
	}
	if output.Len() != 0 {
		t.Fatalf("canceled reload output = %q, want none", output.String())
	}
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

func TestStartStubWatcherReconcilesMutationBeforeReady(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	write := func(times int) {
		t.Helper()
		body := `
- method: shop.v1.OrderService/GetOrder
  times: ` + fmt.Sprint(times) + `
  respond: { message: {} }
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(1)
	src := &sources{protoDirs: []string{"../../testdata/protos"}}
	reg, err := src.buildRegistry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initial, loadErrs := stub.LoadDirs(reg, []string{dir})
	if len(loadErrs) != 0 {
		t.Fatal(loadErrs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("initial limited stub did not select")
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	watch := func(ctx context.Context, _ []string, opts stub.WatchOptions) error {
		write(2) // mutation after the initial load but before watches report ready
		opts.Ready()
		<-ctx.Done()
		return nil
	}
	reconcile := func(reloadCtx context.Context) {
		reloadStubDirsContext(reloadCtx, &commandOutput{cmd: cmd}, reg, store, []string{dir})
	}
	watcher, err := startStubWatcher(ctx, []string{dir}, &commandOutput{cmd: cmd}, reconcile, reconcile, watch)
	if err != nil {
		t.Fatal(err)
	}
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("startup reconciliation did not install mutation made before watcher readiness")
	}
	cancel()
	watcher.stop()
}

func TestStartStubWatcherReturnsInitialAttachError(t *testing.T) {
	want := errors.New("attach denied")
	watch := func(context.Context, []string, stub.WatchOptions) error { return want }
	_, err := startStubWatcher(context.Background(), []string{"stubs"}, discardReloadOutput{}, func(context.Context) {}, func(context.Context) {}, watch)
	if !errors.Is(err, want) {
		t.Fatalf("startStubWatcher error = %v, want attach error", err)
	}
}

func TestStartStubWatcherCancellationDuringStartupJoinsWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	exited := make(chan struct{})
	watch := func(ctx context.Context, _ []string, _ stub.WatchOptions) error {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := startStubWatcher(ctx, []string{"stubs"}, discardReloadOutput{}, func(context.Context) {}, func(context.Context) {}, watch)
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startStubWatcher error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startStubWatcher did not return after startup cancellation")
	}
	select {
	case <-exited:
	default:
		t.Fatal("startStubWatcher returned without joining watcher goroutine")
	}
}

func TestStartStubWatcherSerializesStartupReconcileBeforeCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startupEntered := make(chan struct{})
	releaseStartup := make(chan struct{})
	triggerChange := make(chan struct{})
	callbackDone := make(chan struct{})
	var installed atomic.Int32
	watch := func(ctx context.Context, _ []string, opts stub.WatchOptions) error {
		opts.Ready()
		<-triggerChange
		opts.OnChange(ctx)
		close(callbackDone)
		<-ctx.Done()
		return nil
	}
	result := make(chan watcherRun, 1)
	errs := make(chan error, 1)
	go func() {
		watcher, err := startStubWatcher(ctx, []string{"stubs"}, discardReloadOutput{}, func(context.Context) {
			installed.Store(2)
		}, func(context.Context) {
			close(startupEntered)
			<-releaseStartup
			installed.Store(1)
		}, watch)
		if err != nil {
			errs <- err
			return
		}
		result <- watcher
	}()
	<-startupEntered
	close(triggerChange)
	select {
	case <-callbackDone:
		t.Fatal("watch callback overlapped startup reconciliation")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseStartup)
	var watcher watcherRun
	select {
	case watcher = <-result:
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("startStubWatcher did not return")
	}
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("queued watch callback did not run after startup reconciliation")
	}
	if got := installed.Load(); got != 2 {
		t.Fatalf("installed version = %d, want newer callback version 2", got)
	}
	watcher.stop()
}

type discardReloadOutput struct{}

func (discardReloadOutput) Printf(string, ...any) {}
func (discardReloadOutput) PrintErrln(...any)     {}

func TestServeWithRuntimeStopsAndJoinsWatcherOnServeReturn(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	watcher := startWatcher(context.Background(), func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil
	}, func(error) {})
	<-started

	returned := make(chan struct{})
	shutdownDone := make(chan struct{})
	close(shutdownDone)
	go func() {
		_ = serveWithRuntime(watcher, func() {}, shutdownDone, func() error { return nil })
		close(returned)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("watcher was not canceled when Serve returned")
	}
	select {
	case <-returned:
		t.Fatal("serveWithWatcher returned before watcher goroutine exited")
	default:
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("serveWithWatcher did not join watcher goroutine")
	}
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
		_ = serveWithRuntime(watcherRun{}, cancel, waiterDone, func() error { return nil })
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

func TestWatcherCanceledDuringSignalShutdown(t *testing.T) {
	canceled := make(chan struct{})
	watcher := startWatcher(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		close(canceled)
		return nil
	}, func(error) {})

	sig := make(chan os.Signal, 1)
	shutdownDone := make(chan struct{})
	go func() {
		waitAndShutdown(sig, time.Second, watcher.cancelNow, func() {}, func(...any) {})
		close(shutdownDone)
	}()
	sig <- os.Interrupt
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("watcher was not canceled during shutdown")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("signal shutdown did not finish")
	}
	watcher.stop()
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
