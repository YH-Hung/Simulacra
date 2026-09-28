//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// publish must give a new destination the mode a normally-created file would
// get: 0644 narrowed by the process umask. An unconditional chmod to 0644
// bypasses the umask and hands out a world-readable schema to someone whose
// umask says otherwise.
//
// These tests set the umask, which is process-global, so they must never call
// t.Parallel() and must restore it.
func TestPublishHonoursUmaskForANewDestination(t *testing.T) {
	for _, tc := range []struct {
		name  string
		umask int
		want  os.FileMode
	}{
		{"restrictive umask 0077", 0o077, 0o600},
		{"conventional umask 0022", 0o022, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := syscall.Umask(tc.umask)
			t.Cleanup(func() { syscall.Umask(prior) })

			dest := filepath.Join(t.TempDir(), "schema.binpb")
			if err := publish(context.Background(), dest, []byte("new")); err != nil {
				t.Fatalf("publish: %v", err)
			}
			info, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tc.want {
				t.Fatalf("mode = %04o under umask %04o, want %04o", got, tc.umask, tc.want)
			}
		})
	}
}

// An existing destination keeps its own mode regardless of the umask: the
// umask governs creation, not replacement.
func TestPublishPreservesModeRegardlessOfUmask(t *testing.T) {
	prior := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(prior) })

	dest := filepath.Join(t.TempDir(), "schema.binpb")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), dest, []byte("new")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %04o, want 0644 preserved from the prior destination", got)
	}
}

// The replacement contents must never be readable more widely than the
// destination's own mode -- including while the temporary file is being
// written, not only after the rename. Permission checks happen at open(2), so
// a reader who opens the temporary file while it is permissive keeps a
// readable descriptor even after its mode is later narrowed. This observes the
// temporary file at the moment it holds the complete contents; the published
// file alone cannot show that window.
func TestPublishKeepsReplacementContentsPrivateWhileWriting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		existing  bool // the destination already exists, with mode 0600
		wantFinal os.FileMode
	}{
		{"replacing an existing 0600 destination", true, 0o600},
		{"creating a new destination", false, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// umask 0022 is the case that exposed it: 0644 narrowed by 0022
			// is still world-readable.
			prior := syscall.Umask(0o022)
			t.Cleanup(func() { syscall.Umask(prior) })

			dest := filepath.Join(t.TempDir(), "schema.binpb")
			if tc.existing {
				if err := os.WriteFile(dest, []byte("the previous schema"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dest, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			payload := []byte("the replacement schema")

			var observed os.FileMode
			var observedContents []byte
			err := publishObserved(context.Background(), dest, payload, func(tmpName string) {
				info, err := os.Stat(tmpName)
				if err != nil {
					t.Errorf("stat of the temporary file: %v", err)
					return
				}
				observed = info.Mode().Perm()
				observedContents, _ = os.ReadFile(tmpName)
			})
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			// Proves the observation point is the window that matters: the
			// complete replacement contents are already on disk.
			if string(observedContents) != string(payload) {
				t.Fatalf("hook observed %q, want the complete replacement contents %q",
					observedContents, payload)
			}
			if observed != 0o600 {
				t.Fatalf("the temporary file was %04o while holding the complete "+
					"replacement contents, want 0600", observed)
			}
			info, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tc.wantFinal {
				t.Fatalf("published mode = %04o, want %04o", got, tc.wantFinal)
			}
		})
	}
}

// If the existing destination's mode cannot be read, publish must refuse
// rather than fall back to the new-file mode: that fallback could publish the
// contents more widely than the file being replaced allowed. A symlink to
// itself makes stat fail with ELOOP deterministically.
func TestPublishRefusesWhenTheExistingModeIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	if err := os.Symlink("schema.binpb", dest); err != nil {
		t.Fatal(err)
	}

	err := publish(context.Background(), dest, []byte("new"))
	if err == nil {
		t.Fatal("publish succeeded although the destination's mode could not be read")
	}
	if !strings.Contains(err.Error(), "reading the mode of the existing") {
		t.Fatalf("err = %q, want it to say the existing destination's mode was unreadable", err)
	}
	// Nothing was published, and nothing was left behind.
	if target, lerr := os.Readlink(dest); lerr != nil || target != "schema.binpb" {
		t.Fatalf("the destination was replaced (readlink = %q, %v)", target, lerr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the destination: "+
			"a temporary or probe file was left behind", len(entries))
	}
}
