package cli

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// acceptedUpstream is a listener that never answers, and tells the test when
// the child has actually connected.
//
// This is the readiness signal, and it replaces a sleep that guessed at how
// long the child takes to get there -- the lesson signal_test.go:127 already
// records for the tail tests. schema import installs its signal context
// before it dials, and grpc.NewClient connects lazily, so the TCP connection
// necessarily happens after the handler is in place. An accepted connection
// is proof where a sleep is a guess: if the signal lands first, the child
// dies by the default disposition and exec reports exit status -1, not 2.
func acceptedUpstream(t *testing.T) (addr string, connected <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan struct{})
	go func() {
		var held []net.Conn
		var once sync.Once
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, conn)
			once.Do(func() { close(ch) })
		}
	}()
	return ln.Addr().String(), ch
}

// A signal during an import must exit 2 and leave an existing destination
// exactly as it was. The upstream here is a listener that accepts and never
// answers, so the walk is still in flight when the signal lands.
func TestSchemaImportInterruptedExitsTwoAndPreservesDestination(t *testing.T) {
	for _, sig := range []struct {
		name   string
		signal os.Signal
	}{
		{"SIGINT", os.Interrupt},
		{"SIGTERM", syscall.SIGTERM},
	} {
		t.Run(sig.name, func(t *testing.T) {
			bin := buildBinary(t)
			addr, connected := acceptedUpstream(t)

			dir := t.TempDir()
			out := filepath.Join(dir, "schema.binpb")
			original := []byte("the previous descriptor set")
			if err := os.WriteFile(out, original, 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, "schema", "import",
				"--reflect", addr, "--plaintext", "-o", out)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			// The connection proves signalContext is already installed --
			// schema import sets it up before dialing, and grpc.NewClient
			// dials lazily, so this is the earliest safe moment to signal.
			select {
			case <-connected:
			case <-time.After(30 * time.Second):
				// Kill and reap before failing, matching exitStatusWithin's
				// own pattern (signal_test.go): a Fatal here must not leave
				// a hung child behind for the test binary to wait out later.
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("the child never connected to the upstream")
			}
			if err := cmd.Process.Signal(sig.signal); err != nil {
				t.Fatalf("signal: %v", err)
			}

			if code := exitStatusWithin(t, cmd, 10*time.Second); code != 2 {
				t.Fatalf("exit status %d, want 2 (stderr: %s)", code, stderr.String())
			}

			// The destination-preserved and no-temp-file checks below are
			// belt-and-braces, not this test's load-bearing assertion.
			// acceptedUpstream never answers ListServices, so Closure's walk
			// never returns and publish (schema_import.go) is never reached
			// regardless of when the signal lands -- these two pass
			// vacuously here because nothing ever attempted a write. The
			// commit boundary itself -- a signal arriving at or after the
			// temp file is written -- is exercised directly by
			// TestPublishRefusesToRenameAfterCancellation.
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("the destination is gone: %v", err)
			}
			if string(got) != string(original) {
				t.Fatalf("destination = %q, want it untouched", got)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("directory holds %d entries, want only the destination: "+
					"a temp file was left behind", len(entries))
			}
		})
	}
}
