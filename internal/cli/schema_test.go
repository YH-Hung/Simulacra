package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/yinghanhung/simulacra/internal/schema"
)

// testdataSet returns testdata/protos as a self-contained FileDescriptorSet.
func testdataSet(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	reg.Snapshot().RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
		return true
	})
	return set
}

// writeSet marshals set to a temp file and returns its path.
func writeSet(t *testing.T, set *descriptorpb.FileDescriptorSet) string {
	t.Helper()
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "set.binpb")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// Two sets built independently both embed the well-known types they import,
// so merging must dedupe by path — the server rejects a set naming a path
// twice (design §6.4).
func TestMergeDescriptorSetsDedupesSharedImports(t *testing.T) {
	full := testdataSet(t)
	if len(full.GetFile()) < 3 {
		t.Fatalf("testdata set has %d files, expected at least 3", len(full.GetFile()))
	}
	a := writeSet(t, full)
	b := writeSet(t, full) // the same files again, as a second --include_imports build would

	merged, err := mergeDescriptorSets([]string{a, b})
	if err != nil {
		t.Fatalf("mergeDescriptorSets: %v", err)
	}
	seen := map[string]int{}
	for _, f := range merged.GetFile() {
		seen[f.GetName()]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times in the merged set, want 1", name, count)
		}
	}
	if len(merged.GetFile()) != len(full.GetFile()) {
		t.Fatalf("merged %d files, want %d", len(merged.GetFile()), len(full.GetFile()))
	}
}

// Silently keeping one of two conflicting definitions would register a schema
// the user never described.
func TestMergeDescriptorSetsRejectsConflictingDefinitions(t *testing.T) {
	full := testdataSet(t)
	a := writeSet(t, full)

	conflicting := proto.Clone(full).(*descriptorpb.FileDescriptorSet)
	target := conflicting.GetFile()[0]
	target.Options = &descriptorpb.FileOptions{GoPackage: proto.String("example.com/divergent")}
	b := writeSet(t, conflicting)

	_, err := mergeDescriptorSets([]string{a, b})
	if err == nil {
		t.Fatal("merged two conflicting definitions of the same path")
	}
	if !strings.Contains(err.Error(), target.GetName()) {
		t.Errorf("error %v does not name the conflicting file %s", err, target.GetName())
	}
	if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("error %v does not name both source files", err)
	}
}

func TestMergeDescriptorSetsReportsUnreadableFile(t *testing.T) {
	_, err := mergeDescriptorSets([]string{filepath.Join(t.TempDir(), "absent.binpb")})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestSchemaRegisterSendsOneAllOrNothingCall(t *testing.T) {
	srv := startCommandServer(t)
	full := testdataSet(t)
	half := len(full.GetFile()) / 2
	a := writeSet(t, &descriptorpb.FileDescriptorSet{File: full.GetFile()[:half]})
	b := writeSet(t, &descriptorpb.FileDescriptorSet{File: full.GetFile()[half:]})

	cmd := newSchemaCmd()
	stdout, _, err := runCmd(t, cmd, "register", "--addr", adminAddr(srv), "-f", a, "-f", b)
	if err != nil {
		t.Fatalf("schema register: %v", err)
	}
	if !strings.Contains(stdout, "service(s)") {
		t.Fatalf("stdout = %q, want a service count", stdout)
	}
}

// A set that is not self-contained fails server-side; the diagnostic reaches
// the user unchanged (M3 §11).
func TestSchemaRegisterSurfacesTheServerDiagnosticVerbatim(t *testing.T) {
	srv := startCommandServer(t)
	full := testdataSet(t)
	var orderOnly *descriptorpb.FileDescriptorProto
	for _, f := range full.GetFile() {
		if strings.HasSuffix(f.GetName(), "order.proto") {
			orderOnly = f
		}
	}
	if orderOnly == nil {
		t.Fatal("order.proto not found in the testdata set")
	}
	path := writeSet(t, &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{orderOnly}})

	cmd := newSchemaCmd()
	_, _, err := runCmd(t, cmd, "register", "--addr", adminAddr(srv), "-f", path)
	if err == nil {
		t.Fatal("registering a set with unresolved imports succeeded")
	}
	if !strings.Contains(err.Error(), "self-contained") {
		t.Fatalf("error = %v, want the server's self-contained diagnostic", err)
	}
}

func TestSchemaRegisterRequiresAFile(t *testing.T) {
	cmd := newSchemaCmd()
	_, _, err := runCmd(t, cmd, "register", "--addr", "127.0.0.1:1")
	if err == nil {
		t.Fatal("schema register ran with no --file")
	}
	if !strings.Contains(err.Error(), "--file") {
		t.Fatalf("error = %v, want it to name --file", err)
	}
}

func TestSchemaListText(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newSchemaCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("schema list: %v", err)
	}
	if !strings.Contains(stdout, "shop.v1.OrderService") {
		t.Fatalf("stdout = %q, want the service name", stdout)
	}
	if !strings.Contains(stdout, "GetOrder") {
		t.Fatalf("stdout = %q, want the method names", stdout)
	}
	// WatchOrder is server-streaming; the marker must show on the right side.
	if !strings.Contains(stdout, "WatchOrder(") || !strings.Contains(stdout, "returns (stream ") {
		t.Fatalf("stdout = %q, want a stream marker on WatchOrder's response", stdout)
	}
	// UploadOrders is client-streaming; the marker must show on the left.
	if !strings.Contains(stdout, "UploadOrders(stream ") {
		t.Fatalf("stdout = %q, want a stream marker on UploadOrders' request", stdout)
	}
}

func TestSchemaListJSON(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newSchemaCmd()
	stdout, _, err := runCmd(t, cmd, "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("schema list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	services, _ := fields["services"].([]any)
	if len(services) == 0 {
		t.Fatalf("services is empty in %s", stdout)
	}
	first, _ := services[0].(map[string]any)
	if _, ok := first["methods"]; !ok {
		t.Fatalf("methods absent: %s", stdout)
	}
	// Proto names, not camelCase: client_streaming, not clientStreaming.
	methods, _ := first["methods"].([]any)
	m0, _ := methods[0].(map[string]any)
	if _, ok := m0["client_streaming"]; !ok {
		t.Fatalf("client_streaming absent (camelCase leaked?): %s", stdout)
	}
}

func TestSchemaListRejectsAnInvalidOutputFormat(t *testing.T) {
	cmd := newSchemaCmd()
	_, _, err := runCmd(t, cmd, "list", "--addr", "127.0.0.1:1", "--output", "yaml")
	if err == nil {
		t.Fatal("--output yaml was accepted")
	}
}

// A listing that could not be written is the whole outcome of the command:
// `simulacra schema list > services.txt` produced no listing, and exiting 0
// would tell a script it has one. The JSON path reports this by returning
// writeJSON's error; the text path must not be quieter (design §4, §5).
func TestSchemaListReportsAFailedPayloadWrite(t *testing.T) {
	srv := startCommandServer(t)

	cmd := newSchemaCmd()
	ran, _, err := runCmdWithFailingStdout(t, cmd, "list", "--addr", adminAddr(srv))
	if err == nil {
		t.Fatal("schema list succeeded though its listing could not be written")
	}
	if code := exitCode(ran, err); code != 2 {
		t.Fatalf("exit code = %d, want 2 — a lost payload is an operational failure", code)
	}
}

// selfContainedSet returns a minimal descriptor set holding one service under
// pkg, importing nothing.
//
// The register tests need a set the server does not already serve.
// startCommandServer boots with testdata/protos loaded, so registering the
// testdata set reports zero new files on the *first* call too — which would
// make a re-register test pass without the second call proving anything.
func selfContainedSet(pkg string) *descriptorpb.FileDescriptorSet {
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String(pkg + "/ping.proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("PingRequest")},
			{Name: proto.String("PingResponse")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("PingService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("Ping"),
				InputType:  proto.String("." + pkg + ".PingRequest"),
				OutputType: proto.String("." + pkg + ".PingResponse"),
			}},
		}},
	}}}
}

// Registering a set the server already has is a legitimate no-op, not a
// failure: a CI step that registers its schemas on every run must stay green
// on the second run (design §8).
//
// The first register is what makes the second mean anything — it establishes
// that this set really was new, so "0 new file(s)" is the server recognising
// it rather than the command never having registered anything.
func TestSchemaRegisterOfAnAlreadyRegisteredSetReportsNoNewFiles(t *testing.T) {
	srv := startCommandServer(t)
	path := writeSet(t, selfContainedSet("ping.v1"))

	first, _, err := runCmd(t, newSchemaCmd(), "register", "--addr", adminAddr(srv), "-f", path)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if !strings.Contains(first, "registered 1 new file(s)") {
		t.Fatalf("first register = %q, want it to report 1 new file", first)
	}

	again, _, err := runCmd(t, newSchemaCmd(), "register", "--addr", adminAddr(srv), "-f", path)
	if err != nil {
		t.Fatalf("re-registering an unchanged set failed: %v", err)
	}
	if !strings.Contains(again, "registered 0 new file(s)") {
		t.Fatalf("re-register = %q, want it to report 0 new files", again)
	}
	if !strings.Contains(again, "service(s) available") {
		t.Errorf("re-register = %q, want the service count alongside the file count", again)
	}
}
