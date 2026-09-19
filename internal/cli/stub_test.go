package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/server"
)

// getOrderStub is one valid unary stub document in the file grammar.
func getOrderStub(note string) string {
	return fmt.Sprintf("method: shop.v1.OrderService/GetOrder\nrespond:\n  message: { note: %s }\n", note)
}

// stubClient is a direct admin client for arranging and inspecting state,
// independent of the command under test.
func stubClient(t *testing.T, srv *server.Server) adminv1connect.StubServiceClient {
	t.Helper()
	return adminv1connect.NewStubServiceClient(httpClientForTest(t), "http://"+adminAddr(srv))
}

// createStub installs one API-origin stub and returns its id.
func createStub(t *testing.T, srv *server.Server, document string) string {
	t.Helper()
	resp, err := stubClient(t, srv).CreateStub(context.Background(),
		connect.NewRequest(&adminv1.CreateStubRequest{Document: document}))
	if err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	return resp.Msg.GetStub().GetId()
}

// listStubIDs returns the ids currently in the store, in List order.
func listStubIDs(t *testing.T, srv *server.Server) []string {
	t.Helper()
	resp, err := stubClient(t, srv).ListStubs(context.Background(),
		connect.NewRequest(&adminv1.ListStubsRequest{}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	var ids []string
	for _, s := range resp.Msg.GetStubs() {
		ids = append(ids, s.GetId())
	}
	return ids
}

func TestStubListText(t *testing.T) {
	srv := startCommandServer(t)
	id := createStub(t, srv, getOrderStub("first"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	for _, want := range []string{"ID", "METHOD", "ORIGIN", "HITS", "TIMES", id,
		"/shop.v1.OrderService/GetOrder", "api"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	// times: 0 means unlimited in the grammar, and the table says so rather
	// than printing a bare 0 the reader must decode.
	if !strings.Contains(stdout, "unlimited") {
		t.Errorf("stdout does not render times 0 as unlimited:\n%s", stdout)
	}
}

func TestStubListRendersAFiniteTimesAsANumber(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, "method: shop.v1.OrderService/GetOrder\ntimes: 3\nrespond:\n  message: { note: n }\n")

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	if strings.Contains(stdout, "unlimited") {
		t.Errorf("a times of 3 rendered as unlimited:\n%s", stdout)
	}
	if !strings.Contains(stdout, "3") {
		t.Errorf("stdout does not show times 3:\n%s", stdout)
	}
}

func TestStubListEmptyReportsToStderr(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	if !strings.Contains(stderr, "no stubs") {
		t.Errorf("stderr = %q, want a no-stubs note", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want it empty so a pipe sees nothing", stdout)
	}
}

func TestStubListMethodFilter(t *testing.T) {
	srv := startCommandServer(t)
	kept := createStub(t, srv, getOrderStub("kept"))
	other := createStub(t, srv,
		"method: shop.v1.OrderService/WatchOrder\nrespond:\n  stream: [{ message: { note: other } }]\n")

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("stub list: %v", err)
	}
	if !strings.Contains(stdout, kept) {
		t.Errorf("stdout is missing the matching stub %s:\n%s", kept, stdout)
	}
	if strings.Contains(stdout, other) {
		t.Errorf("stdout includes the filtered-out stub %s:\n%s", other, stdout)
	}
}

func TestStubListOriginFilter(t *testing.T) {
	srv := startCommandServer(t)
	api := createStub(t, srv, getOrderStub("api"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--origin", "file")
	if err != nil {
		t.Fatalf("stub list --origin file: %v", err)
	}
	if strings.Contains(stdout, api) {
		t.Errorf("an api-origin stub survived --origin file:\n%s", stdout)
	}

	cmd = newStubCmd()
	stdout, _, err = runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--origin", "api")
	if err != nil {
		t.Fatalf("stub list --origin api: %v", err)
	}
	if !strings.Contains(stdout, api) {
		t.Errorf("stdout is missing the api-origin stub:\n%s", stdout)
	}
}

func TestStubListRejectsAnUnknownOrigin(t *testing.T) {
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "list", "--addr", "127.0.0.1:1", "--origin", "both")
	if err == nil {
		t.Fatal("--origin both was accepted")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Fatalf("error = %v, want it to name --origin", err)
	}
}

func TestStubListJSON(t *testing.T) {
	srv := startCommandServer(t)
	id := createStub(t, srv, getOrderStub("json"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("stub list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	stubs, _ := fields["stubs"].([]any)
	if len(stubs) != 1 {
		t.Fatalf("stubs = %v, want one entry", fields["stubs"])
	}
	first, _ := stubs[0].(map[string]any)
	if first["id"] != id {
		t.Errorf("id = %v, want %s", first["id"], id)
	}
	if first["origin"] != "STUB_ORIGIN_API" {
		t.Errorf("origin = %v, want the enum name", first["origin"])
	}
}

func TestStubRemove(t *testing.T) {
	srv := startCommandServer(t)
	first := createStub(t, srv, getOrderStub("first"))
	second := createStub(t, srv, getOrderStub("second"))

	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "rm", "--addr", adminAddr(srv), first)
	if err != nil {
		t.Fatalf("stub rm: %v", err)
	}
	ids := listStubIDs(t, srv)
	if len(ids) != 1 || ids[0] != second {
		t.Fatalf("remaining ids = %v, want only %s", ids, second)
	}
}

func TestStubRemoveRequiresAnID(t *testing.T) {
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "rm", "--addr", "127.0.0.1:1")
	if err == nil {
		t.Fatal("stub rm ran with no id")
	}
}

// Deletes are not rolled back: a removed stub cannot be recreated with its ID.
// The command reports what it did remove before failing (design §6.1).
func TestStubRemoveFailsFastAndReportsWhatItRemoved(t *testing.T) {
	srv := startCommandServer(t)
	first := createStub(t, srv, getOrderStub("first"))
	survivor := createStub(t, srv, getOrderStub("survivor"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "rm", "--addr", adminAddr(srv), first, "api-does-not-exist", survivor)
	if err == nil {
		t.Fatal("stub rm succeeded with an unknown id")
	}
	if !strings.Contains(stdout, first) {
		t.Errorf("stdout = %q, want it to name the stub that was removed before the failure", stdout)
	}
	// The third id was never attempted: fail fast.
	ids := listStubIDs(t, srv)
	if len(ids) != 1 || ids[0] != survivor {
		t.Fatalf("remaining ids = %v, want only %s — rm must stop at the first failure", ids, survivor)
	}
}

// A file-origin stub belongs to the hot-reload watcher, not the API. Deleting
// one is FAILED_PRECONDITION, and the server's diagnostic reaches the user
// unchanged (design §6.1, §8).
func TestStubRemoveOfAFileOriginStubSurfacesTheServerDiagnostic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stubs.yaml"),
		[]byte("- method: shop.v1.OrderService/GetOrder\n  respond:\n    message: { note: from-file }\n"),
		0o600); err != nil {
		t.Fatalf("write stub file: %v", err)
	}
	srv := startCommandServerWithStubs(t, dir)

	ids := listStubIDs(t, srv)
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want the one file-origin stub", ids)
	}

	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "rm", "--addr", adminAddr(srv), ids[0])
	if err == nil {
		t.Fatal("removing a file-origin stub succeeded")
	}
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want failed_precondition; err = %v", code, err)
	}
	if remaining := listStubIDs(t, srv); len(remaining) != 1 {
		t.Fatalf("remaining = %v, want the stub still installed", remaining)
	}
}

func TestStubExportToStdout(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("exported"))

	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("stub export: %v", err)
	}
	if !strings.Contains(stdout, "note: exported") {
		t.Fatalf("stdout = %q, want the stub document", stdout)
	}
	// The count is commentary: it must not pollute a piped document.
	if !strings.Contains(stderr, "1 stub") {
		t.Errorf("stderr = %q, want the stub count", stderr)
	}
	if strings.Contains(stdout, "stub(s)") {
		t.Errorf("the count leaked into stdout: %q", stdout)
	}
}

func TestStubExportToFile(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("to-file"))
	path := filepath.Join(t.TempDir(), "stubs.yaml")

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv), "--out", path)
	if err != nil {
		t.Fatalf("stub export --out: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want it empty when writing to a file", stdout)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the exported file: %v", err)
	}
	if !strings.Contains(string(raw), "note: to-file") {
		t.Fatalf("file = %q, want the stub document", raw)
	}
}

// -o is --out's shorthand, not --output's (design §4).
func TestStubExportShorthandIsOut(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("shorthand"))
	path := filepath.Join(t.TempDir(), "stubs.yaml")

	cmd := newStubCmd()
	if _, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv), "-o", path); err != nil {
		t.Fatalf("stub export -o: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the exported file: %v", err)
	}
	if !strings.Contains(string(raw), "note: shorthand") {
		t.Fatalf("-o did not write the document: %q", raw)
	}
}

// Format and destination compose: all four combinations are defined.
func TestStubExportFormatAndDestinationCompose(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("composed"))

	t.Run("json to stdout", func(t *testing.T) {
		cmd := newStubCmd()
		stdout, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv), "--output", "json")
		if err != nil {
			t.Fatalf("export --output json: %v", err)
		}
		fields := decodeJSON(t, stdout)
		if _, ok := fields["document"]; !ok {
			t.Fatalf("document absent: %s", stdout)
		}
		if fields["stub_count"] != "1" && fields["stub_count"] != float64(1) {
			t.Fatalf("stub_count = %#v, want 1", fields["stub_count"])
		}
	})

	t.Run("json to a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stubs.json")
		cmd := newStubCmd()
		if _, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv),
			"--out", path, "--output", "json"); err != nil {
			t.Fatalf("export --out --output json: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		fields := decodeJSON(t, string(raw))
		if _, ok := fields["document"]; !ok {
			t.Fatalf("document absent from the file: %s", raw)
		}
	})
}

func TestStubExportOfAnEmptyStoreSucceeds(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("exporting an empty store: %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("stdout = %q, want the empty sequence", stdout)
	}
	if !strings.Contains(stderr, "0 stub") {
		t.Errorf("stderr = %q, want a zero count", stderr)
	}
}

func TestStubExportReportsAnUnwritablePath(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "export", "--addr", adminAddr(srv),
		"--out", filepath.Join(t.TempDir(), "no-such-dir", "stubs.yaml"))
	if err == nil {
		t.Fatal("export to an unwritable path succeeded")
	}
}

// writeStubFile writes a stub file holding the given documents as a list.
func writeStubFile(t *testing.T, name string, items ...string) string {
	t.Helper()
	var b strings.Builder
	for _, item := range items {
		b.WriteString("- ")
		b.WriteString(strings.ReplaceAll(strings.TrimSuffix(item, "\n"), "\n", "\n  "))
		b.WriteString("\n")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestStubAddCreatesEveryStubInFileOrder(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml", getOrderStub("one"), getOrderStub("two"))

	cmd := newStubCmd()
	stdout, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err != nil {
		t.Fatalf("stub add: %v", err)
	}
	if ids := listStubIDs(t, srv); len(ids) != 2 {
		t.Fatalf("created %d stubs, want 2: %v", len(ids), ids)
	}
	if !strings.Contains(stdout, "api-1") || !strings.Contains(stdout, "api-2") {
		t.Errorf("stdout = %q, want both created ids", stdout)
	}
}

func TestStubAddReadsStdin(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	cmd.SetIn(strings.NewReader("- " + strings.ReplaceAll(getOrderStub("stdin"), "\n", "\n  ")))
	if _, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", "-"); err != nil {
		t.Fatalf("stub add -f -: %v", err)
	}
	if ids := listStubIDs(t, srv); len(ids) != 1 {
		t.Fatalf("created %d stubs, want 1", len(ids))
	}
}

// Preflight: a malformed file sends ZERO RPCs. Asserting "no stubs created"
// alone would also hold if the server had rejected every document, so the
// test counts calls (design §8).
func TestStubAddPreflightSendsNoRPCsForAMalformedFile(t *testing.T) {
	srv := startCommandServer(t)
	good := writeStubFile(t, "good.yaml", getOrderStub("good"))
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("method: not-a-list\n"), 0o600); err != nil {
		t.Fatalf("write bad.yaml: %v", err)
	}

	counter := &countingStubClient{}
	cmd := newStubCmdWithClient(countingFactory(counter))
	_, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", good, "-f", bad)
	if err == nil {
		t.Fatal("stub add accepted a malformed file")
	}
	if counter.creates != 0 {
		t.Fatalf("preflight sent %d CreateStub calls, want 0", counter.creates)
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("error = %v, want it to name the failing file", err)
	}
}

// A server-side rejection rolls back every prior create in this invocation.
func TestStubAddRollsBackOnRejection(t *testing.T) {
	srv := startCommandServer(t)
	preexisting := createStub(t, srv, getOrderStub("preexisting"))
	// The third document names a method that does not exist: only the server
	// can reject it, so preflight passes and the RPC phase must compensate.
	path := writeStubFile(t, "stubs.yaml",
		getOrderStub("one"),
		getOrderStub("two"),
		"method: shop.v1.OrderService/NoSuchMethod\nrespond:\n  message: {}\n")

	cmd := newStubCmd()
	stdout, stderr, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add succeeded with an unknown method")
	}
	if !strings.Contains(stderr+stdout+err.Error(), "no stubs were added") {
		t.Errorf("output does not say no stubs were added:\nstdout=%q\nstderr=%q\nerr=%v", stdout, stderr, err)
	}
	if !strings.Contains(err.Error(), "stubs.yaml#2") {
		t.Errorf("error = %v, want it to name the failing document as <file>#<index>", err)
	}
	// The store is back to exactly what it held before the command ran.
	ids := listStubIDs(t, srv)
	if len(ids) != 1 || ids[0] != preexisting {
		t.Fatalf("ids = %v, want only the pre-existing %s — rollback did not run", ids, preexisting)
	}
}

// When a rollback delete itself fails, the command says the rollback was
// incomplete and names the orphan. It never claims a clean rollback it did not
// achieve. A healthy server cannot produce this, so it is injected (design §8).
func TestStubAddReportsAnIncompleteRollback(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml",
		getOrderStub("one"),
		"method: shop.v1.OrderService/NoSuchMethod\nrespond:\n  message: {}\n")

	failing := &failingDeleteStubClient{}
	cmd := newStubCmdWithClient(failingFactory(failing))
	stdout, stderr, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add succeeded with an unknown method")
	}
	combined := stdout + stderr + err.Error()
	if !strings.Contains(combined, "incomplete") {
		t.Errorf("output does not report an incomplete rollback:\n%s", combined)
	}
	if !strings.Contains(combined, "api-1") {
		t.Errorf("output does not name the orphaned id:\n%s", combined)
	}
	if strings.Contains(combined, "no stubs were added") {
		t.Errorf("output claims a clean rollback it did not achieve:\n%s", combined)
	}
}

// Compensation must still run when the command context is already done, which
// is exactly the state a timed-out or interrupted create leaves behind. The
// rollback therefore uses its own context (design §6.1).
func TestStubAddRollsBackUnderACancelledCommandContext(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml",
		getOrderStub("one"),
		"method: shop.v1.OrderService/NoSuchMethod\nrespond:\n  message: {}\n")

	cancelling := &cancelAfterCreateStubClient{}
	cmd := newStubCmdWithClient(cancellingFactory(t, cancelling))
	if _, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path); err == nil {
		t.Fatal("stub add succeeded with an unknown method")
	}
	if ids := listStubIDs(t, srv); len(ids) != 0 {
		t.Fatalf("ids = %v, want none — rollback did not run under a done context", ids)
	}
}

// A create whose response never arrived leaves a stub installed whose id the
// client never learned. It cannot be compensated for, so the command says the
// store may hold something it could not identify, names the document that was
// in flight, and does not claim the clean rollback it did not achieve
// (design §6.1). A healthy server cannot lose a response, so it is injected
// (design §8).
//
// The surviving-stub assertion is what makes the other two mean anything.
// Asserting on wording alone would also be satisfied by a command that printed
// the warning unconditionally, including where no stub was created; the orphan
// in the store is the evidence that a stub really is installed which this
// invocation can neither name nor delete, so "no stubs were added" would have
// been a lie rather than merely a different phrasing.
func TestStubAddReportsPossibleUnidentifiedStubsWhenACreateResponseIsLost(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml", getOrderStub("one"), getOrderStub("two"))

	losing := &losingCreateStubClient{}
	cmd := newStubCmdWithClient(losingFactory(losing))
	stdout, stderr, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add succeeded though the create reported a deadline")
	}
	combined := stdout + stderr + err.Error()
	if !strings.Contains(combined, "stubs.yaml#0") {
		t.Errorf("output does not name the document that was in flight:\n%s", combined)
	}
	if strings.Contains(combined, "no stubs were added") {
		t.Errorf("output claims a clean rollback it cannot know it achieved:\n%s", combined)
	}
	// The stub the lost response created is still installed, unnameable and
	// therefore undeleted — which is exactly why the claim above would be a
	// lie. The second document was never attempted: the command fails fast.
	if ids := listStubIDs(t, srv); len(ids) != 1 {
		t.Fatalf("ids = %v, want the one stub the lost create left behind", ids)
	}
}

// Stdin is consumed by the first read, so a second `-` contributes nothing.
// Silently adding zero documents for it would report success over input the
// command never saw.
func TestStubAddRejectsARepeatedStdinFile(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newStubCmd()
	cmd.SetIn(strings.NewReader("- " + strings.ReplaceAll(getOrderStub("stdin"), "\n", "\n  ")))

	_, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", "-", "-f", "-")
	if err == nil {
		t.Fatal("stub add accepted -f - twice")
	}
	if !strings.Contains(err.Error(), "-") {
		t.Errorf("error = %v, want it to name the repeated file", err)
	}
	// Preflight rejects before any RPC, so nothing was created.
	if ids := listStubIDs(t, srv); len(ids) != 0 {
		t.Fatalf("ids = %v, want none: the rejection happens in preflight", ids)
	}
}

func TestStubAddRequiresAFile(t *testing.T) {
	cmd := newStubCmd()
	_, _, err := runCmd(t, cmd, "add", "--addr", "127.0.0.1:1")
	if err == nil {
		t.Fatal("stub add ran with no --file")
	}
	if !strings.Contains(err.Error(), "--file") {
		t.Fatalf("error = %v, want it to name --file", err)
	}
}

// export then add round-trips through the one splitter.
func TestStubExportThenAddRoundTrips(t *testing.T) {
	srv := startCommandServer(t)
	createStub(t, srv, getOrderStub("round-trip-a"))
	createStub(t, srv, getOrderStub("round-trip-b"))
	path := filepath.Join(t.TempDir(), "exported.yaml")

	if _, _, err := runCmd(t, newStubCmd(), "export", "--addr", adminAddr(srv), "--out", path); err != nil {
		t.Fatalf("stub export: %v", err)
	}
	target := startCommandServer(t)
	if _, _, err := runCmd(t, newStubCmd(), "add", "--addr", adminAddr(target), "-f", path); err != nil {
		t.Fatalf("stub add of the exported file: %v", err)
	}
	if ids := listStubIDs(t, target); len(ids) != 2 {
		t.Fatalf("round-tripped %d stubs, want 2: %v", len(ids), ids)
	}
}

func TestStubAddOfAnEmptyExportAddsNothing(t *testing.T) {
	srv := startCommandServer(t)
	path := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(path, []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := runCmd(t, newStubCmd(), "add", "--addr", adminAddr(srv), "-f", path); err != nil {
		t.Fatalf("stub add of an empty sequence: %v", err)
	}
	if ids := listStubIDs(t, srv); len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}

// Preflight splits the file, which proves it is a list of mappings; it does
// not prove each mapping is a stub. An unknown field is decidable locally, so
// it must fail before the first RPC — otherwise the entries ahead of it are
// created and then rolled back, which reaches the right end state by exactly
// the route design §6.1 rules out: "no RPC is sent at all".
//
// Counting CreateStub calls, not surviving stubs. "No stubs exist afterwards"
// is equally true when the server rejected the document and rollback cleaned
// up, so stub counts cannot tell a working preflight from a broken one.
func TestStubAddPreflightSendsNoRPCsForAnUnknownField(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "mixed.yaml",
		getOrderStub("good"),
		"method: shop.v1.OrderService/GetOrder\nbogus_field: nope\nrespond:\n  message: {}\n")

	counter := &countingStubClient{}
	cmd := newStubCmdWithClient(countingFactory(counter))
	stdout, _, err := runCmd(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add accepted an entry carrying an unknown field")
	}
	if counter.creates != 0 {
		t.Fatalf("preflight sent %d CreateStub calls, want 0", counter.creates)
	}
	if strings.Contains(stdout, "created ") {
		t.Errorf("stdout reports a create preflight should have prevented: %q", stdout)
	}
	if !strings.Contains(err.Error(), "mixed.yaml#1") {
		t.Errorf("error = %v, want the failing entry named as <file>#<index>", err)
	}
	if !strings.Contains(err.Error(), "bogus_field") {
		t.Errorf("error = %v, want the grammar diagnostic naming the field", err)
	}
}

// The created ids are what the caller came for, so a run that could not write
// them has not delivered — even though every stub really was created. Exiting
// 0 would tell a script it holds a list of ids it never received; the non-zero
// exit is what sends it to `stub list` instead (design §4, §5).
func TestStubAddReportsAFailedPayloadWrite(t *testing.T) {
	srv := startCommandServer(t)
	path := writeStubFile(t, "stubs.yaml", getOrderStub("one"))

	cmd := newStubCmd()
	ran, _, err := runCmdWithFailingStdout(t, cmd, "add", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("stub add succeeded though the created ids could not be written")
	}
	if code := exitCode(ran, err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	// The stub itself was created: the write failed, the RPC did not, and the
	// command must not pretend otherwise by rolling back work that succeeded.
	if ids := listStubIDs(t, srv); len(ids) != 1 {
		t.Fatalf("ids = %v, want the one stub the successful create installed", ids)
	}
}
