package schema

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/yinghanhung/simulacra/internal/match"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const testProtoDir = "../../testdata/protos"

func TestAddProtoDirAndLookupMethod(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}

	// Both slash forms must resolve (grpc-go hands us "/pkg.Svc/Method").
	for _, name := range []string{"/shop.v1.OrderService/GetOrder", "shop.v1.OrderService/GetOrder"} {
		m, err := reg.LookupMethod(name)
		if err != nil {
			t.Fatalf("LookupMethod(%q): %v", name, err)
		}
		if got := string(m.Input().FullName()); got != "shop.v1.GetOrderRequest" {
			t.Errorf("input type = %s, want shop.v1.GetOrderRequest", got)
		}
		if got := string(m.Output().FullName()); got != "shop.v1.GetOrderResponse" {
			t.Errorf("output type = %s, want shop.v1.GetOrderResponse", got)
		}
	}

	if _, err := reg.LookupMethod("/shop.v1.OrderService/NoSuchMethod"); err == nil {
		t.Error("expected error for unknown method")
	}
	if _, err := reg.LookupMethod("/no.such.Service/GetOrder"); err == nil {
		t.Error("expected error for unknown service")
	}
	if _, err := reg.LookupMethod("garbage"); err == nil {
		t.Error("expected error for malformed method name")
	}

	svcs := reg.Services()
	found := false
	for _, s := range svcs {
		if string(s.FullName()) == "shop.v1.OrderService" {
			found = true
		}
	}
	if !found {
		t.Errorf("Services() = %v, want to contain shop.v1.OrderService", svcs)
	}
}

func TestAddProtoDirEmpty(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), t.TempDir()); err == nil {
		t.Error("expected error for directory with no .proto files")
	}
}

// buildDescriptorSet marshals fd and its transitive imports, deps first —
// the same self-contained shape `buf build` and `protoc --include_imports` emit.
func buildDescriptorSet(t *testing.T, fd protoreflect.FileDescriptor) []byte {
	t.Helper()
	seen := map[string]bool{}
	set := new(descriptorpb.FileDescriptorSet)
	var add func(protoreflect.FileDescriptor)
	add = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		imps := f.Imports()
		for i := 0; i < imps.Len(); i++ {
			add(imps.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
	}
	add(fd)
	data, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling descriptor set: %v", err)
	}
	return data
}

func TestAddDescriptorSetFile(t *testing.T) {
	src := NewRegistry()
	if err := src.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	m, err := src.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	setPath := filepath.Join(t.TempDir(), "shop.binpb")
	if err := os.WriteFile(setPath, buildDescriptorSet(t, m.ParentFile()), 0o644); err != nil {
		t.Fatalf("writing descriptor set: %v", err)
	}

	reg := NewRegistry()
	if err := reg.AddDescriptorSetFile(setPath); err != nil {
		t.Fatalf("AddDescriptorSetFile: %v", err)
	}
	if _, err := reg.LookupMethod("shop.v1.OrderService/GetOrder"); err != nil {
		t.Errorf("LookupMethod after descriptor-set load: %v", err)
	}
}

func TestAddDescriptorSetFileRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "garbage.binpb")
	if err := os.WriteFile(p, []byte("not a descriptor set"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewRegistry().AddDescriptorSetFile(p); err == nil {
		t.Error("expected error for non-descriptor-set file")
	}
}

func TestLookupMessage(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.LookupMessage("shop.v1.Customer"); err != nil {
		t.Errorf("LookupMessage(shop.v1.Customer): %v", err)
	}
	if _, err := reg.LookupMessage("google.protobuf.FileDescriptorSet"); err != nil {
		t.Errorf("LookupMessage(google.protobuf.FileDescriptorSet): %v", err)
	}
	if _, err := reg.LookupMessage("no.such.Type"); err == nil {
		t.Error("expected error for unknown message type")
	}
	if _, err := reg.LookupMessage("shop.v1.OrderService"); err == nil {
		t.Error("expected error for non-message descriptor")
	}
}

func TestTypesResolvesRegistrySchemas(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatal(err)
	}
	types := reg.Types()
	if _, err := types.FindMessageByURL("type.googleapis.com/shop.v1.Customer"); err != nil {
		t.Errorf("FindMessageByURL(shop.v1.Customer): %v", err)
	}
	if _, err := types.FindMessageByName("google.protobuf.FileDescriptorSet"); err != nil {
		t.Errorf("FindMessageByName(FileDescriptorSet): %v", err)
	}
}

func TestTypesWrongKindDoesNotFallbackForMessages(t *testing.T) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("local_message_collision.proto"),
		Package: proto.String("google.protobuf"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("FileDescriptorSet"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{
				Name:   proto.String("FILE_DESCRIPTOR_SET_UNSPECIFIED"),
				Number: proto.Int32(0),
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	reg := NewRegistry()
	if err := reg.AddFile(file); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	types := reg.Types()

	if got, err := reg.LookupMessage("google.protobuf.FileDescriptorSet"); err == nil {
		t.Errorf("LookupMessage resolved global %v despite local wrong-kind collision", got.FullName())
	}
	if got, err := types.FindMessageByName("google.protobuf.FileDescriptorSet"); err == nil {
		t.Errorf("FindMessageByName resolved global %v despite local wrong-kind collision", got.Descriptor().FullName())
	}
	if got, err := types.FindMessageByURL("type.googleapis.com/google.protobuf.FileDescriptorSet"); err == nil {
		t.Errorf("FindMessageByURL resolved global %v despite local wrong-kind collision", got.Descriptor().FullName())
	}
}

func TestTypesWrongKindDoesNotFallbackForExtension(t *testing.T) {
	const (
		pkg           = "simulacra.schemafallback"
		hostName      = protoreflect.FullName(pkg + ".Host")
		extensionName = protoreflect.FullName(pkg + ".ext_tag")
		extensionNum  = protoreflect.FieldNumber(123)
	)
	global, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("global_extension_collision.proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Host"),
			ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{{
				Start: proto.Int32(100),
				End:   proto.Int32(200),
			}},
		}},
		Extension: []*descriptorpb.FieldDescriptorProto{{
			Name:     proto.String("ext_tag"),
			Number:   proto.Int32(int32(extensionNum)),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Extendee: proto.String("." + string(hostName)),
		}},
	}, nil)
	if err != nil {
		t.Fatalf("global protodesc.NewFile: %v", err)
	}
	if _, err := protoregistry.GlobalTypes.FindExtensionByName(extensionName); err != nil {
		if err := protoregistry.GlobalTypes.RegisterExtension(dynamicpb.NewExtensionType(global.Extensions().Get(0))); err != nil {
			t.Fatalf("RegisterExtension: %v", err)
		}
	}

	local, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("local_extension_collision.proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Host"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{
				Name:   proto.String("HOST_UNSPECIFIED"),
				Number: proto.Int32(0),
			}},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("ext_tag")}},
	}, nil)
	if err != nil {
		t.Fatalf("local protodesc.NewFile: %v", err)
	}
	reg := NewRegistry()
	if err := reg.AddFile(local); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	if got, err := reg.Types().FindExtensionByName(extensionName); err == nil {
		t.Fatalf("FindExtensionByName resolved global %v despite local wrong-kind collision", got.TypeDescriptor().FullName())
	}

	// A genuine local miss still falls back to the registered global extension.
	empty := NewRegistry().Types()
	if got, err := empty.FindExtensionByName(extensionName); err != nil || got.TypeDescriptor().Number() != extensionNum {
		t.Fatalf("global extension name fallback = %v, %v", got, err)
	}
	if got, err := empty.FindExtensionByNumber(hostName, extensionNum); err != nil || got.TypeDescriptor().FullName() != extensionName {
		t.Fatalf("global extension number fallback = %v, %v", got, err)
	}
}

// A snapshot taken before a mutation must not observe the mutation: the
// registry must swap a fresh Files rather than mutate the one it handed out.
func TestSnapshotIsImmutableAcrossMutation(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatal(err)
	}
	before := reg.Snapshot()
	beforeCount := before.NumFiles()

	if err := reg.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		t.Fatal(err)
	}
	if got := before.NumFiles(); got != beforeCount {
		t.Fatalf("earlier snapshot grew from %d to %d files; mutation must build a new snapshot", beforeCount, got)
	}
	if got := reg.Snapshot().NumFiles(); got <= beforeCount {
		t.Fatalf("current snapshot has %d files, want > %d after AddFile", got, beforeCount)
	}
	if _, err := before.FindFileByPath("grpc/health/v1/health.proto"); err == nil {
		t.Fatal("old snapshot resolves the newly added file; snapshots must be frozen")
	}
}

// A failed load must leave the served snapshot untouched (all-or-nothing,
// design §2.3) — this now covers AddProtoDir too, which previously could
// leave a half-loaded registry.
func TestFailedLoadLeavesRegistryUntouched(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatal(err)
	}
	before := reg.Snapshot()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.proto"), []byte("syntax = \"proto3\";\npackage broken;\nmessage M { this is not proto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddProtoDir(context.Background(), dir); err == nil {
		t.Fatal("AddProtoDir of a broken tree succeeded, want error")
	}
	if reg.Snapshot() != before {
		t.Fatal("failed AddProtoDir swapped the snapshot; must be all-or-nothing")
	}
}

// Descriptor identity is stable across registrations (design §2.3):
// pre-existing FileDescriptor values are reused verbatim in each candidate.
func TestDescriptorIdentityStableAcrossRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatal(err)
	}
	d1, err := reg.FindDescriptorByName("shop.v1.OrderService")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		t.Fatal(err)
	}
	d2, err := reg.FindDescriptorByName("shop.v1.OrderService")
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("descriptor identity changed across an unrelated registration")
	}
}

func loadWKTImage(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	data, err := os.ReadFile("testdata/wkt_image.binpb")
	if err != nil {
		t.Fatal(err)
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(data, set); err != nil {
		t.Fatal(err)
	}
	return set
}

// The §2.6 risk test: a buf image carrying its own copies of the well-known
// types must register as a no-op against a registry whose WKTs came from
// protocompile. buf images also carry a per-file extension (field 8042) that
// lands in unknown fields; equivalence must see through both.
func TestRegisterSetIdempotentAcrossToolchains(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "testdata/wktset"); err != nil {
		t.Fatal(err)
	}
	added, err := reg.RegisterSet(loadWKTImage(t))
	if err != nil {
		t.Fatalf("RegisterSet of an equivalent buf image: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want none (every file already registered)", added)
	}
}

func TestRegisterSetAddsAllFilesToEmptyRegistryThenNoops(t *testing.T) {
	reg := NewRegistry()
	set := loadWKTImage(t)
	added, err := reg.RegisterSet(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != len(set.File) {
		t.Fatalf("added %d files, want all %d", len(added), len(set.File))
	}
	if _, err := reg.LookupMethod("scratch.v1.ThingService/GetThing"); err != nil {
		t.Fatalf("registered method not resolvable: %v", err)
	}
	again, err := reg.RegisterSet(set)
	if err != nil {
		t.Fatalf("re-registering the identical set: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second register added %v, want none", again)
	}
}

// Same path, different content → error naming the file, registry untouched.
func TestRegisterSetConflictRollsBack(t *testing.T) {
	reg := NewRegistry()
	set := loadWKTImage(t)
	if _, err := reg.RegisterSet(set); err != nil {
		t.Fatal(err)
	}
	before := reg.Snapshot()

	conflicting := proto.Clone(set).(*descriptorpb.FileDescriptorSet)
	for _, f := range conflicting.File {
		if f.GetName() == "scratch.proto" {
			f.MessageType[0].Field[0].Number = proto.Int32(99) // id: 1 → 99
		}
	}
	_, err := reg.RegisterSet(conflicting)
	if err == nil {
		t.Fatal("conflicting set registered, want error")
	}
	if !strings.Contains(err.Error(), "scratch.proto") {
		t.Fatalf("error %q does not name the conflicting file", err)
	}
	if reg.Snapshot() != before {
		t.Fatal("failed RegisterSet swapped the snapshot; must be all-or-nothing")
	}
}

func TestRegisterSetRejectsEmptyAndNonSelfContainedSets(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.RegisterSet(&descriptorpb.FileDescriptorSet{}); err == nil {
		t.Fatal("empty set accepted, want error")
	}
	set := loadWKTImage(t)
	partial := &descriptorpb.FileDescriptorSet{}
	for _, f := range set.File {
		if f.GetName() == "scratch.proto" { // imports absent → not self-contained
			partial.File = append(partial.File, f)
		}
	}
	if _, err := reg.RegisterSet(partial); err == nil {
		t.Fatal("non-self-contained set accepted, want error")
	} else if !strings.Contains(err.Error(), "self-contained") {
		t.Fatalf("error %q should point at self-containment", err)
	}
}

// buildSet compiles nothing: it hand-builds a minimal one-file set with a
// unique package so disjoint registrations cannot conflict.
func buildSet(t *testing.T, pkg string) *descriptorpb.FileDescriptorSet {
	t.Helper()
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String(pkg + ".proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Msg"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("id"),
				Number: proto.Int32(1),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}},
		}},
	}}}
}

// The lost-update check -race cannot make (design §2.2): two registrations
// racing on the same base snapshot must both land; an atomic pointer alone
// would silently drop one.
func TestConcurrentDisjointRegistrationsBothLand(t *testing.T) {
	for round := 0; round < 50; round++ {
		reg := NewRegistry()
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = reg.RegisterSet(buildSet(t, fmt.Sprintf("race%da", i)))
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d: %v", round, i, err)
			}
		}
		for i := 0; i < 2; i++ {
			name := protoreflect.FullName(fmt.Sprintf("race%da.Msg", i))
			if _, err := reg.FindDescriptorByName(name); err != nil {
				t.Fatalf("round %d: %s missing after concurrent registration: %v", round, name, err)
			}
		}
	}
}

// All three historical escape paths running against a mutating registry.
// Run under -race: the copy-on-write snapshots must make every read safe.
func TestReadsRaceRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatal(err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	types := reg.Types()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: register a fresh disjoint set per iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := reg.RegisterSet(buildSet(t, fmt.Sprintf("w%d", i))); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Reader 1: resolver lookups (what grpc reflection does).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := reg.FindDescriptorByName("shop.v1.OrderService"); err != nil {
				t.Error(err)
				return
			}
			if _, err := reg.FindFileByPath(method.ParentFile().Path()); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Reader 2: CEL compile + eval over a snapshot (what stub compilation does).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			compiler := match.NewCompiler(reg.Snapshot())
			compiled, err := compiler.Compile(method.Input(), &match.Block{Expr: `message.order_id == "x"`}, match.Unary)
			if err != nil {
				t.Error(err)
				return
			}
			msg := dynamicpb.NewMessage(method.Input())
			compiled.Eval(match.Input{Message: msg})
		}
	}()

	// Reader 3: dynamic type resolution (what templates and Any details do).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := types.FindMessageByName(method.Input().FullName()); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}
