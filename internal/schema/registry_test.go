package schema

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
