package schema

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
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
