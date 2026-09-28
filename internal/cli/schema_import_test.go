package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestSchemaImportRequiresReflect(t *testing.T) {
	_, _, err := runCmd(t, newSchemaCmd(), "import", "-o", filepath.Join(t.TempDir(), "s.binpb"))
	if err == nil {
		t.Fatal("import succeeded without --reflect, want an error")
	}
	if !strings.Contains(err.Error(), "--reflect") {
		t.Fatalf("error %q does not name --reflect", err)
	}
}

func TestSchemaImportRequiresOut(t *testing.T) {
	_, _, err := runCmd(t, newSchemaCmd(), "import", "--reflect", "127.0.0.1:1")
	if err == nil {
		t.Fatal("import succeeded without --out, want an error")
	}
	if !strings.Contains(err.Error(), "--out") {
		t.Fatalf("error %q does not name --out", err)
	}
}

// The happy path, against Simulacra's own data plane.
func TestSchemaImportWritesADescriptorSet(t *testing.T) {
	srv := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	stdout, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", srv.DataAddr().String(), "--plaintext", "-o", out)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(stdout, "schema.binpb") {
		t.Fatalf("summary %q does not name the destination", stdout)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the imported set: %v", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatalf("the written file is not a FileDescriptorSet: %v", err)
	}
	var found bool
	for _, f := range set.GetFile() {
		if strings.Contains(f.GetName(), "order.proto") {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported %d file(s) but not order.proto", len(set.GetFile()))
	}
}

func TestSchemaImportJSONOutput(t *testing.T) {
	srv := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	stdout, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", srv.DataAddr().String(), "--plaintext", "-o", out, "--output", "json")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var got struct {
		Files    []string `json:"files"`
		Services int      `json:"services"`
		Bytes    int      `json:"bytes"`
		Path     string   `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
	if len(got.Files) == 0 || got.Bytes == 0 || got.Path != out {
		t.Fatalf("summary = %+v, want files, a byte count and the destination", got)
	}
}

// An upstream that accepts the connection and never answers must not hang
// the command forever: --timeout bounds the whole walk, and a timeout is an
// operational failure like any other -- no destination file.
func TestSchemaImportTimesOutOnAHangingUpstream(t *testing.T) {
	addr := hangingListener(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	_, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", addr, "--plaintext", "--timeout", "200ms", "-o", out)
	if err == nil {
		t.Fatal("import succeeded against a hanging upstream, want a timeout error")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("a destination file was created despite the timeout (stat err = %v)", statErr)
	}
	// A deadline and a signal both cancel the walk's context, but they are
	// not the same thing to a user: one is the --timeout they set, the other
	// is the Ctrl-C they pressed. Reporting a timeout as an interruption
	// sends them looking for a signal that never arrived.
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error %q does not say it timed out", err)
	}
	if errors.Is(err, errInterrupted) {
		t.Fatalf("a --timeout expiry reported itself as a signal interruption: %q", err)
	}
	if !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("error %q does not point at the flag that governs it", err)
	}
}

// The publication boundary's deadline translation, tested through the seam
// RunE actually calls. A mutation check established that without this test,
// deleting the translation outright broke nothing.
func TestPublishImportReportsADeadlineAsATimeout(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	err := publishImport(expired, 2*time.Second, dest, []byte("new"))
	if err == nil {
		t.Fatal("publishImport succeeded on an expired deadline, want an error")
	}
	if errors.Is(err, errInterrupted) {
		t.Fatalf("a deadline at the publication boundary reported itself as a signal: %q", err)
	}
	for _, want := range []string{"timed out", "before publishing to", "--timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q does not contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("the destination was published despite the deadline (stat err = %v)", statErr)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil {
		t.Fatal(readErr)
	} else if len(entries) != 0 {
		t.Fatalf("directory holds %d entries, want none: a temporary or probe file was left behind", len(entries))
	}
}

// A signal at the same boundary still reports an interruption, not a timeout.
func TestPublishImportReportsASignalAsAnInterruption(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "schema.binpb")
	err := publishImport(cancelled, 2*time.Second, dest, []byte("new"))
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %q, want errInterrupted for a signal", err)
	}
}

// walkEnded names the stage that was in progress, so a deadline at the
// publication boundary does not claim the import was still fetching -- and so
// neither stage reports a deadline as the signal sentinel. Both remain exit 2.
func TestWalkEndedDistinguishesDeadlineFromSignalAtEitherStage(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	signalled, cancelSignalled := context.WithCancel(context.Background())
	cancelSignalled()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		what string
		// wantSubstrings must all appear; wantInterrupted picks the sentinel.
		wantSubstrings  []string
		wantInterrupted bool
	}{
		{
			name:           "deadline while fetching",
			ctx:            expired,
			what:           "importing from 10.0.0.1:443",
			wantSubstrings: []string{"timed out", "importing from 10.0.0.1:443", "--timeout"},
		},
		{
			name:           "deadline at the publication boundary",
			ctx:            expired,
			what:           "before publishing to /tmp/schema.binpb",
			wantSubstrings: []string{"timed out", "before publishing to /tmp/schema.binpb", "--timeout"},
		},
		{
			name:            "signal while fetching",
			ctx:             signalled,
			what:            "importing from 10.0.0.1:443",
			wantInterrupted: true,
		},
		{
			name:            "signal at the publication boundary",
			ctx:             signalled,
			what:            "before publishing to /tmp/schema.binpb",
			wantInterrupted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := walkEnded(tc.ctx, 2*time.Second, tc.what)
			if err == nil {
				t.Fatal("walkEnded returned nil on a done context")
			}
			if tc.wantInterrupted {
				if !errors.Is(err, errInterrupted) {
					t.Fatalf("err = %q, want errInterrupted for a signal", err)
				}
				return
			}
			if errors.Is(err, errInterrupted) {
				t.Fatalf("a deadline reported itself as a signal interruption: %q", err)
			}
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %q does not contain %q", err, want)
				}
			}
		})
	}
}

// A live context is not an ending: walkEnded must not invent a failure.
func TestWalkEndedIsNilOnALiveContext(t *testing.T) {
	if err := walkEnded(context.Background(), time.Second, "importing from x"); err != nil {
		t.Fatalf("walkEnded on a live context = %v, want nil", err)
	}
}

// A negative --timeout is a usage error, matching newAdminClient's own
// validation for the flag of the same name.
func TestSchemaImportRejectsNegativeTimeout(t *testing.T) {
	_, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", "127.0.0.1:1", "--timeout", "-1s",
		"-o", filepath.Join(t.TempDir(), "s.binpb"))
	if err == nil {
		t.Fatal("import succeeded with a negative --timeout, want an error")
	}
	if !strings.Contains(err.Error(), "--timeout must not be negative") {
		t.Fatalf("error %q does not name --timeout", err)
	}
}

// An unreachable upstream is an operational failure, not a written file.
func TestSchemaImportUnreachableUpstreamWritesNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "schema.binpb")
	_, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", "127.0.0.1:1", "--plaintext", "-o", out)
	if err == nil {
		t.Fatal("import succeeded against a closed port, want an error")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("a destination file was created on failure (stat err = %v)", statErr)
	}
}

// The commit boundary: a context cancelled before the rename must leave an
// existing destination byte-identical, with no temp file beside it.
func TestSchemaImportCancelledBeforeRenamePreservesDestination(t *testing.T) {
	srv := startCommandServer(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "schema.binpb")
	original := []byte("the previous descriptor set")
	if err := os.WriteFile(out, original, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := newSchemaCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"import", "--reflect", srv.DataAddr().String(),
		"--plaintext", "-o", out})
	// Cancel before the command runs: the walk fails or the pre-rename check
	// fires, and either way publication must not happen.
	cancel()
	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("import succeeded under a cancelled context, want an error")
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("the destination is gone: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("destination = %q, want it untouched (%q)", got, original)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only the destination: a temp file was left behind", names)
	}
}

// publish must not replace the destination once the context is done. This
// tests the guard directly: the command-level test cancels before the walk,
// so it never reaches this code at all.
func TestPublishRefusesToRenameAfterCancellation(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	original := []byte("the previous descriptor set")
	if err := os.WriteFile(dest, original, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := publish(ctx, dest, []byte("the newly imported set"))
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("publish err = %v, want errInterrupted", err)
	}
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("the destination is gone: %v", readErr)
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
			"the temp file was left behind", len(entries))
	}
}

// An upstream that advertises nothing but services the data plane implements
// itself (health, its own reflection) must still succeed and still write the
// file, but must say so on stderr: silence here would read as a useful
// import when nothing was actually importable.
func TestSchemaImportWarnsWhenNothingIsImportable(t *testing.T) {
	addr := healthOnlyUpstream(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	stdout, stderr, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", addr, "--plaintext", "-o", out)
	if err != nil {
		t.Fatalf("import: %v (stderr: %s)", err, stderr)
	}
	if !strings.Contains(stderr, "no services were importable") {
		t.Fatalf("stderr = %q, want a note that nothing was importable", stderr)
	}
	if !strings.Contains(stdout, "imported 0 file(s)") {
		t.Fatalf("stdout = %q, want the usual summary line", stdout)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("the destination file was not written: %v", statErr)
	}
}

// healthOnlyUpstream is a stock grpc-go server advertising only health and
// its own two reflection services -- every one of them in Closure's skip
// list -- so nothing it advertises is ever importable.
func healthOnlyUpstream(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	healthpb.RegisterHealthServer(s, health.NewServer())
	reflection.Register(s)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

// The same call on a live context must publish.
func TestPublishReplacesTheDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), dest, []byte("new")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("destination = %q, want %q", got, "new")
	}
}

// os.CreateTemp creates 0600. Without preserving the destination's own mode
// across the rename, overwriting an existing 0644 (or other) file would
// silently tighten it to 0600.
func TestPublishPreservesTheDestinationsMode(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	if err := os.WriteFile(dest, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), dest, []byte("new")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 0640 preserved from the prior destination", got)
	}
}

// A destination that does not yet exist gets the conventional 0644, not
// os.CreateTemp's 0600.
func TestPublishGivesANewDestinationConventionalMode(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.binpb")
	if err := publish(context.Background(), dest, []byte("new")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %o, want 0644", got)
	}
}
