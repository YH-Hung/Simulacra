package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/server"
)

// The M3 §12 test: registry in -> identical descriptor set out.
//
// Two assertions, because one is not enough. `schema list` renders only
// service names, method names, input/output type names and streaming flags,
// so an import that dropped every field of every message would leave both
// listings identical and still register. Only the descriptor comparison
// establishes preservation (design §8).
func TestSchemaImportRoundTripPreservesDescriptors(t *testing.T) {
	source := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	if _, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", source.DataAddr().String(), "--plaintext", "-o", out); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Register the imported set into a second, empty server.
	dest, err := server.Start(context.Background(), server.Options{
		DataAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", JournalSize: 8,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(dest.Stop)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}

	// Assertion 1 (smoke): the same number of services are registered.
	srcCount := source.ServiceCount()
	if _, _, err := runCmd(t, newSchemaCmd(), "register",
		"--addr", adminAddr(dest), "-f", out); err != nil {
		t.Fatalf("register: %v", err)
	}
	dstCount := dest.ServiceCount()
	if srcCount != dstCount {
		t.Fatalf("services: source %d, destination %d", srcCount, dstCount)
	}

	// Assertion 2 (the real one): every imported file is byte-identical to
	// the descriptors the source server was built from, under the
	// registry's own equality policy. This does not need the server's own
	// registry -- server.Server has no accessor for it, deliberately, since
	// it is an internal type the public wiring facade must not leak -- it
	// only needs the same testdata source startCommandServer built it from.
	want := schema.NewRegistry()
	if err := want.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("building the comparison registry: %v", err)
	}
	for _, imported := range set.GetFile() {
		path := imported.GetName()
		fd, err := want.FindFileByPath(path)
		if err != nil {
			t.Fatalf("the comparison registry has no %s, yet it was imported: %v", path, err)
		}
		wantFile := schema.NormalizeFileProto(protodesc.ToFileDescriptorProto(fd))
		got := schema.NormalizeFileProto(imported)
		if !proto.Equal(wantFile, got) {
			t.Fatalf("%s did not survive the round trip; the import is lossy", path)
		}
	}
}

// F6: health is skipped as a root, so the imported set never carries it and
// registration succeeds regardless of whether the upstream's health.proto
// agrees with our compiled-in one.
//
// This test's upstream (startCommandServer) happens to serve a
// byte-identical health.proto, so on its own "registers cleanly" would pass
// whether or not the skip exists -- only "health is absent from the set"
// discriminates. To reproduce the premise the skip actually guards against, a
// health.proto that genuinely disagrees is constructed directly (one RPC
// dropped, as the F6 probe in the design did) and shown to be rejected by a
// registry that already holds the real one, the same mechanism
// RegisterSchemas uses. The registration half of F6 -- that a disagreeing
// upstream would poison an unskipped import -- is proven by that probe here;
// design §3 records the same fact from the original wire-level reproduction.
func TestSchemaImportSkipsHealthSoRegistrationSucceeds(t *testing.T) {
	source := startCommandServer(t)
	out := filepath.Join(t.TempDir(), "schema.binpb")

	if _, _, err := runCmd(t, newSchemaCmd(), "import",
		"--reflect", source.DataAddr().String(), "--plaintext", "-o", out); err != nil {
		t.Fatalf("import: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	healthPath := grpc_health_v1.File_grpc_health_v1_health_proto.Path()
	for _, f := range set.GetFile() {
		if f.GetName() == healthPath {
			t.Fatalf("the import carries %s; health is implemented by the data "+
				"plane and an upstream copy that disagrees with ours fails the "+
				"entire registration (F6)", healthPath)
		}
	}

	// Prove the premise: a health.proto that genuinely disagrees with the
	// compiled-in one is rejected outright, so skipping it is load-bearing
	// and not merely harmless.
	differing := proto.Clone(protodesc.ToFileDescriptorProto(
		grpc_health_v1.File_grpc_health_v1_health_proto)).(*descriptorpb.FileDescriptorProto)
	svc := differing.GetService()[0]
	svc.Method = svc.Method[:len(svc.Method)-1]

	probe := schema.NewRegistry()
	if err := probe.AddFile(grpc_health_v1.File_grpc_health_v1_health_proto); err != nil {
		t.Fatalf("seeding the probe registry with the real health.proto: %v", err)
	}
	if _, err := probe.RegisterSet(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{differing},
	}); err == nil {
		t.Fatal("registering a health.proto with a dropped method succeeded; " +
			"the premise this test rests on -- that a disagreeing health.proto " +
			"is genuinely rejected -- does not hold")
	}

	// And the actual import -- which skips health entirely -- registers
	// cleanly into a server that already has its own health, unlike the
	// differing descriptor just shown to be rejected above.
	dest, err := server.Start(context.Background(), server.Options{
		DataAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", JournalSize: 8,
	})
	if err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(dest.Stop)
	if _, _, err := runCmd(t, newSchemaCmd(), "register",
		"--addr", adminAddr(dest), "-f", out); err != nil {
		t.Fatalf("register: %v", err)
	}
}
