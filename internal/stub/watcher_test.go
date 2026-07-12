package stub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWatchRecursivelyDebouncesAndAddsNewDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "existing")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan struct{}, 10)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WatchWithOptions(ctx, []string{root}, WatchOptions{
			Debounce: 30 * time.Millisecond,
			OnChange: func(context.Context) { changes <- struct{}{} },
			Ready:    func() { close(ready) },
		})
	}()
	<-ready

	file := filepath.Join(nested, "stub.yaml")
	for i := range 5 {
		if err := os.WriteFile(file, []byte{byte('0' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	waitForWatchedChange(t, changes)
	select {
	case <-changes:
		t.Fatal("write burst produced more than one debounced callback")
	case <-time.After(60 * time.Millisecond):
	}

	created := filepath.Join(root, "created", "deeper")
	if err := os.MkdirAll(created, 0o755); err != nil {
		t.Fatal(err)
	}
	waitForWatchedChange(t, changes)
	if err := os.WriteFile(filepath.Join(created, "new.yaml"), []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForWatchedChange(t, changes)

	cancel()
	waitForWatchReturn(t, done)
}

func TestWatchReattachesRecreatedConfiguredRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "stubs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	changes := make(chan struct{}, 10)
	done := make(chan error, 1)
	go func() {
		done <- WatchWithOptions(ctx, []string{root}, WatchOptions{
			Debounce: 20 * time.Millisecond,
			OnChange: func(context.Context) { changes <- struct{}{} },
			Ready:    func() { close(ready) },
		})
	}()
	<-ready

	old := filepath.Join(parent, "old-stubs")
	if err := os.Rename(root, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "initial.yaml"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForWatchedChange(t, changes)

	if err := os.WriteFile(filepath.Join(root, "later.yaml"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForWatchedChange(t, changes)
	cancel()
	waitForWatchReturn(t, done)
}

func TestWatcherErrorReconcilesAndSchedulesReload(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	ready := make(chan struct{})
	changes := make(chan struct{}, 1)
	reported := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchWithBackend(ctx, []string{root}, WatchOptions{
			Debounce: 10 * time.Millisecond,
			OnChange: func(context.Context) { changes <- struct{}{} },
			OnError:  func(err error) { reported <- err },
			Ready:    func() { close(ready) },
		}, backend)
	}()
	<-ready
	initialAdds := backend.addCount(root)
	backend.errs <- fsnotify.ErrEventOverflow
	if err := waitForError(t, reported); !errors.Is(err, fsnotify.ErrEventOverflow) {
		t.Fatalf("reported error = %v, want ErrEventOverflow", err)
	}
	waitForWatchedChange(t, changes)
	if got := backend.addCount(root); got <= initialAdds {
		t.Fatalf("root Add calls = %d, want reconciliation after overflow (initial %d)", got, initialAdds)
	}
	cancel()
	waitForWatchReturn(t, done)
}

func TestDynamicAttachErrorReportedAndRetried(t *testing.T) {
	root := t.TempDir()
	created := filepath.Join(root, "created")
	backend := newFakeWatchBackend()
	backend.addErr = func(path string, attempt int) error {
		if path == created && attempt == 1 {
			return syscall.EACCES
		}
		return nil
	}
	ready := make(chan struct{})
	reported := make(chan error, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchWithBackend(ctx, []string{root}, WatchOptions{
			Debounce: 10 * time.Millisecond,
			OnChange: func(context.Context) {},
			OnError:  func(err error) { reported <- err },
			Ready:    func() { close(ready) },
		}, backend)
	}()
	<-ready
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}
	backend.events <- fsnotify.Event{Name: created, Op: fsnotify.Create}
	if err := waitForError(t, reported); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("reported error = %v, want EACCES", err)
	}
	deadline := time.After(time.Second)
	for backend.addCount(created) < 2 {
		select {
		case <-backend.added:
		case <-deadline:
			t.Fatal("dynamic directory attach was not retried")
		}
	}
	cancel()
	waitForWatchReturn(t, done)
}

func TestWatchCancellationDoesNotWaitForBlockedCallback(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	ready := make(chan struct{})
	entered := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchWithBackend(ctx, []string{root}, WatchOptions{
			Debounce: time.Millisecond,
			OnChange: func(context.Context) {
				close(entered)
				<-release
			},
			Ready: func() { close(ready) },
		}, backend)
	}()
	<-ready
	backend.events <- fsnotify.Event{Name: filepath.Join(root, "stub.yaml"), Op: fsnotify.Write}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}
	cancel()
	waitForWatchReturn(t, done)
	close(release)
}

func TestWatchRecoversCallbackPanicAndReportsIt(t *testing.T) {
	root := t.TempDir()
	backend := newFakeWatchBackend()
	ready := make(chan struct{})
	reported := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchWithBackend(ctx, []string{root}, WatchOptions{
			Debounce: time.Millisecond,
			OnChange: func(context.Context) { panic("boom") },
			OnError:  func(err error) { reported <- err },
			Ready:    func() { close(ready) },
		}, backend)
	}()
	<-ready
	backend.events <- fsnotify.Event{Name: filepath.Join(root, "stub.yaml"), Op: fsnotify.Write}
	if err := waitForError(t, reported); !strings.Contains(err.Error(), "boom") {
		t.Fatalf("panic report = %v, want boom", err)
	}
	cancel()
	waitForWatchReturn(t, done)
}

func TestWatchReturnsInitialRootSetupError(t *testing.T) {
	err := Watch(context.Background(), []string{filepath.Join(t.TempDir(), "missing")}, time.Millisecond, func() {})
	if err == nil {
		t.Fatal("Watch initial setup error = nil, want error")
	}
}

type fakeWatchBackend struct {
	events chan fsnotify.Event
	errs   chan error
	added  chan struct{}

	mu       sync.Mutex
	adds     map[string]int
	addErr   func(path string, attempt int) error
	closeOne sync.Once
}

func newFakeWatchBackend() *fakeWatchBackend {
	return &fakeWatchBackend{
		events: make(chan fsnotify.Event, 20),
		errs:   make(chan error, 20),
		added:  make(chan struct{}, 20),
		adds:   make(map[string]int),
	}
}

func (f *fakeWatchBackend) Add(path string) error {
	f.mu.Lock()
	f.adds[path]++
	attempt := f.adds[path]
	errFn := f.addErr
	f.mu.Unlock()
	select {
	case f.added <- struct{}{}:
	default:
	}
	if errFn != nil {
		return errFn(path, attempt)
	}
	return nil
}

func (f *fakeWatchBackend) Remove(string) error           { return nil }
func (f *fakeWatchBackend) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeWatchBackend) Errors() <-chan error          { return f.errs }
func (f *fakeWatchBackend) Close() error {
	f.closeOne.Do(func() {
		close(f.events)
		close(f.errs)
	})
	return nil
}

func (f *fakeWatchBackend) addCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adds[path]
}

func waitForWatchedChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watched change")
	}
}

func waitForError(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watcher error")
		return fmt.Errorf("unreachable")
	}
}

func waitForWatchReturn(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return")
	}
}
