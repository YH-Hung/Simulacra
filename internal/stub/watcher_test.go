package stub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchRecursivelyDebouncesAndAddsNewDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "existing")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan struct{}, 10)
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, []string{root}, 30*time.Millisecond, func() {
			changes <- struct{}{}
		})
	}()
	time.Sleep(50 * time.Millisecond)

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
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch after cancellation = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}

func TestAttachCreatedDirectoryToleratesTransientAddFailure(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("directory disappeared")
	called := false
	attachCreatedDirectory(dir, func(string) (os.FileInfo, error) {
		return info, nil
	}, func(string) error {
		called = true
		return wantErr
	})
	if !called {
		t.Fatal("dynamic attach was not attempted")
	}
}

func TestWatchReturnsInitialRootSetupError(t *testing.T) {
	err := Watch(context.Background(), []string{filepath.Join(t.TempDir(), "missing")}, time.Millisecond, func() {})
	if err == nil {
		t.Fatal("Watch initial setup error = nil, want error")
	}
}

func waitForWatchedChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watched change")
	}
}
