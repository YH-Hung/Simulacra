package schema_test

import (
	"context"
	"testing"

	"github.com/yinghanhung/simulacra/internal/schema"
	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// closureSet returns fd and everything it imports transitively as a
// FileDescriptorSet, dependencies first, each path once.
func closureSet(fd protoreflect.FileDescriptor) *descriptorpb.FileDescriptorSet {
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var walk func(protoreflect.FileDescriptor)
	walk = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		imports := f.Imports()
		for i := 0; i < imports.Len(); i++ {
			walk(imports.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
	}
	walk(fd)
	return set
}

func TestGeneratedShopDescriptorsAreEquivalentToProtoDir(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	set := closureSet(shopv1.File_shop_v1_order_proto) // deps first, deduplicated by path
	added, err := reg.RegisterSet(set)
	if err != nil || len(added) != 0 {
		t.Fatalf("RegisterSet(generated closure) = %v, %v; want no new files and no error (F3)", added, err)
	}
}
