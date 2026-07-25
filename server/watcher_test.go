package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// captureReporter records reload progress for assertions (replaces the
// cobra-backed commandOutput used by these tests when they lived in cli).
type captureReporter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *captureReporter) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(&r.buf, format, args...)
}

func (r *captureReporter) PrintErrln(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(&r.buf, args...)
}

func (r *captureReporter) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func testRegistry(t *testing.T) *schema.Registry {
	t.Helper()
	reg, err := buildRegistry(context.Background(), []string{"../testdata/protos"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestReconcileStubDirsKeepsInvalidStoreAndResetsValidBudget(t *testing.T) {
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
	reg := testRegistry(t)
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("initial Select = nil, want limited stub")
	}

	reporter := &captureReporter{}
	write("{{{ invalid yaml")
	if _, err := reconcileStubDirs(context.Background(), reporter, reg, store, []string{dir}, true); err == nil {
		t.Fatal("reconcile error = nil, want invalid-stub error")
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after invalid reload = %v, want old exhausted store", got)
	}
	if !strings.Contains(reporter.String(), "stub error:") {
		t.Fatalf("invalid reload output = %q, want stub error", reporter.String())
	}

	reporter = &captureReporter{}
	write(valid)
	if _, err := reconcileStubDirs(context.Background(), reporter, reg, store, []string{dir}, true); err != nil {
		t.Fatalf("reconcile valid = %v", err)
	}
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("Select after valid reload = nil, want reset times budget")
	}
	if !strings.Contains(reporter.String(), "1 stub(s) reloaded") {
		t.Fatalf("valid reload output = %q, want count", reporter.String())
	}
}

func TestReconcileStubDirsDoesNothingAfterCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub.yaml"), []byte(`
- method: shop.v1.OrderService/GetOrder
  times: 1
  respond: { message: {} }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := testRegistry(t)
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("initial limited stub did not select")
	}

	reporter := &captureReporter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reconcileStubDirs(ctx, reporter, reg, store, []string{dir}, true); err == nil {
		t.Fatal("reconcile error = nil, want context cancellation")
	}
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after canceled reload = %v, want exhausted original store", got)
	}
	if reporter.String() != "" {
		t.Fatalf("canceled reload output = %q, want none", reporter.String())
	}
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
	reg := testRegistry(t)
	initial, loadErrs := stub.LoadDirs(reg, []string{dir})
	if len(loadErrs) != 0 {
		t.Fatal(loadErrs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("initial limited stub did not select")
	}

	reporter := &captureReporter{}
	ctx, cancel := context.WithCancel(context.Background())
	watch := func(ctx context.Context, _ []string, opts stub.WatchOptions) error {
		write(2) // mutation after the initial load but before watches report ready
		opts.Ready()
		<-ctx.Done()
		return nil
	}
	reconcile := func(reloadCtx context.Context) error {
		_, err := reconcileStubDirs(reloadCtx, reporter, reg, store, []string{dir}, true)
		return err
	}
	watcher, err := startStubWatcher(ctx, []string{dir}, reporter, func(reloadCtx context.Context) {
		_ = reconcile(reloadCtx)
	}, reconcile, watch)
	if err != nil {
		t.Fatal(err)
	}
	if store.Select(method, match.Input{}) == nil {
		t.Fatal("startup reconciliation did not install mutation made before watcher readiness")
	}
	cancel()
	watcher.stop()
}

func TestStartStubWatcherReturnsStartupReconciliationError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	if err := os.WriteFile(path, []byte(`
- method: shop.v1.OrderService/GetOrder
  respond: { message: {} }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := testRegistry(t)
	initial, loadErrs := stub.LoadDirs(reg, []string{dir})
	if len(loadErrs) != 0 {
		t.Fatal(loadErrs)
	}
	store := stub.NewStore(initial)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan struct{})
	watch := func(ctx context.Context, _ []string, opts stub.WatchOptions) error {
		if err := os.WriteFile(path, []byte("{{{ invalid yaml"), 0o644); err != nil {
			return err
		}
		opts.Ready()
		<-ctx.Done()
		close(exited)
		return nil
	}
	reconcile := func(reloadCtx context.Context) error {
		_, err := reconcileStubDirs(reloadCtx, discardReporter{}, reg, store, []string{dir}, false)
		return err
	}
	watcher, err := startStubWatcher(ctx, []string{dir}, discardReporter{}, func(context.Context) {}, reconcile, watch)
	if watcher.cancel != nil {
		defer watcher.stop()
	}
	if err == nil {
		t.Fatal("startStubWatcher error = nil, want startup reconciliation error")
	}
	select {
	case <-exited:
	default:
		t.Fatal("startStubWatcher returned startup reconciliation error without joining watcher")
	}
}

func TestStartStubWatcherReturnsInitialAttachError(t *testing.T) {
	want := errors.New("attach denied")
	watch := func(context.Context, []string, stub.WatchOptions) error { return want }
	_, err := startStubWatcher(context.Background(), []string{"stubs"}, discardReporter{}, func(context.Context) {}, func(context.Context) error { return nil }, watch)
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
		_, err := startStubWatcher(ctx, []string{"stubs"}, discardReporter{}, func(context.Context) {}, func(context.Context) error { return nil }, watch)
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
		watcher, err := startStubWatcher(ctx, []string{"stubs"}, discardReporter{}, func(context.Context) {
			installed.Store(2)
		}, func(context.Context) error {
			close(startupEntered)
			<-releaseStartup
			installed.Store(1)
			return nil
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

func TestWatcherRunStopJoinsGoroutine(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	watcher, _ := startReadyWatcher(context.Background(), func(ctx context.Context, ready func()) error {
		ready()
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil
	}, func(error) {})
	<-started
	watcher.stop() // cancels then joins
	select {
	case <-canceled:
	default:
		t.Fatal("watcherRun.stop returned before the watcher goroutine exited")
	}
}
