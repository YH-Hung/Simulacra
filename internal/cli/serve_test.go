package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
