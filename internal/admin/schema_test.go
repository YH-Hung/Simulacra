package admin_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/schema"
)

func schemaClient(t *testing.T, deps admin.Deps) adminv1connect.SchemaServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewSchemaServiceClient(ts.Client(), ts.URL)
}

// testdataFiles returns testdata/protos as descriptor protos: a self-contained
// set of google/protobuf/any.proto, google/protobuf/timestamp.proto, and
// shop/v1/order.proto.
func testdataFiles(t *testing.T) []*descriptorpb.FileDescriptorProto {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	var files []*descriptorpb.FileDescriptorProto
	reg.Snapshot().RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		files = append(files, protodesc.ToFileDescriptorProto(fd))
		return true
	})
	return files
}

func marshalSet(t *testing.T, files ...*descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	raw, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: files})
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return raw
}

// emptyRegistryDeps is testDeps with nothing registered, so registration has
// something to add.
func emptyRegistryDeps(t *testing.T) admin.Deps {
	t.Helper()
	deps := testDeps(t)
	deps.Registry = schema.NewRegistry()
	return deps
}

func TestRegisterSchemasAddsFilesToTheServedRegistryIdempotently(t *testing.T) {
	deps := emptyRegistryDeps(t)
	client := schemaClient(t, deps)
	req := &adminv1.RegisterSchemasRequest{DescriptorSet: marshalSet(t, testdataFiles(t)...)}

	first, err := client.RegisterSchemas(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("RegisterSchemas: %v", err)
	}
	wantFiles := []string{"google/protobuf/any.proto", "google/protobuf/timestamp.proto", "shop/v1/order.proto"}
	if !reflect.DeepEqual(first.Msg.RegisteredFiles, wantFiles) {
		t.Errorf("registered_files = %q, want %q (sorted)", first.Msg.RegisteredFiles, wantFiles)
	}
	if first.Msg.ServiceCount != 1 {
		t.Errorf("service_count = %d, want 1", first.Msg.ServiceCount)
	}
	// Registered into the served registry, not a copy of it.
	if _, err := deps.Registry.LookupMethod("shop.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("after RegisterSchemas, deps.Registry cannot resolve GetOrder: %v", err)
	}

	again, err := client.RegisterSchemas(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("second RegisterSchemas: %v", err)
	}
	if len(again.Msg.RegisteredFiles) != 0 || again.Msg.ServiceCount != 1 {
		t.Errorf("re-registering = files %q, %d service(s); want no files and still 1 service",
			again.Msg.RegisteredFiles, again.Msg.ServiceCount)
	}
}

func TestRegisterSchemasRejectsBadSetsAndLeavesTheRegistryUntouched(t *testing.T) {
	files := testdataFiles(t)
	var order *descriptorpb.FileDescriptorProto
	var imports []*descriptorpb.FileDescriptorProto
	for _, file := range files {
		if file.GetName() == "shop/v1/order.proto" {
			order = file
		} else {
			imports = append(imports, file)
		}
	}
	conflicting := proto.Clone(order).(*descriptorpb.FileDescriptorProto)
	conflicting.MessageType = append(conflicting.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("Extra")})

	cases := []struct {
		name string
		set  []byte
		want string
	}{
		{"undecodable bytes", []byte{0x0a, 0x05}, "descriptor_set is not a valid FileDescriptorSet"},
		{"empty set", nil, "descriptor set contains no files"},
		{"missing imports", marshalSet(t, order), "descriptor sets must be self-contained"},
		{"same path, different content", marshalSet(t, append(imports, conflicting)...),
			`file "shop/v1/order.proto" is already registered with different content`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t) // testdata/protos is already registered
			_, err := schemaClient(t, deps).RegisterSchemas(context.Background(),
				connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: tc.set}))
			if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
				t.Fatalf("RegisterSchemas = %v (code %v), want InvalidArgument", err, code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if _, err := deps.Registry.LookupMessage("shop.v1.Extra"); err == nil {
				t.Error("a rejected set changed the served registry")
			}
		})
	}
}

// RegisterSet accepts a descriptor path that is not valid UTF-8, so the
// response and every later ListServices must still serialize (design §5.2).
// Without validUTF8 the registration succeeds and its own response fails.
func TestSchemaResponsesSurviveANonUTF8DescriptorPath(t *testing.T) {
	deps := emptyRegistryDeps(t)
	client := schemaClient(t, deps)
	bad := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("bad\xffsvc.proto"),
		Package:     proto.String("probe.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("ProbeService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("Do"),
				InputType:  proto.String(".probe.v1.Empty"),
				OutputType: proto.String(".probe.v1.Empty"),
			}},
		}},
	}
	registered, err := client.RegisterSchemas(context.Background(),
		connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: marshalSet(t, bad)}))
	if err != nil {
		t.Fatalf("RegisterSchemas: %v", err)
	}
	if want := []string{"bad�svc.proto"}; !reflect.DeepEqual(registered.Msg.RegisteredFiles, want) {
		t.Errorf("registered_files = %q, want %q", registered.Msg.RegisteredFiles, want)
	}
	listed, err := client.ListServices(context.Background(), connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		t.Fatalf("ListServices after registering a non-UTF-8 path: %v", err)
	}
	if len(listed.Msg.Services) != 1 || listed.Msg.Services[0].File != "bad�svc.proto" {
		t.Errorf("services = %v, want probe.v1.ProbeService from bad�svc.proto", listed.Msg.Services)
	}
}

func TestListServicesDescribesEveryServiceSortedByName(t *testing.T) {
	deps := testDeps(t)
	if err := deps.Registry.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		t.Fatalf("AddFile(health): %v", err)
	}
	resp, err := schemaClient(t, deps).ListServices(context.Background(),
		connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	var names []string
	for _, svc := range resp.Msg.Services {
		names = append(names, svc.Name)
	}
	if want := []string{"grpc.health.v1.Health", "shop.v1.OrderService"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service names = %q, want %q", names, want)
	}
	order := resp.Msg.Services[1]
	if order.File != "shop/v1/order.proto" {
		t.Errorf("file = %q, want shop/v1/order.proto", order.File)
	}
	want := []*adminv1.MethodInfo{
		{Name: "GetOrder", InputType: "shop.v1.GetOrderRequest", OutputType: "shop.v1.GetOrderResponse"},
		{Name: "WatchOrder", InputType: "shop.v1.GetOrderRequest", OutputType: "shop.v1.GetOrderResponse",
			ServerStreaming: true},
		{Name: "UploadOrders", InputType: "shop.v1.GetOrderRequest", OutputType: "shop.v1.GetOrderResponse",
			ClientStreaming: true},
		{Name: "Chat", InputType: "shop.v1.ChatMessage", OutputType: "shop.v1.ChatMessage",
			ClientStreaming: true, ServerStreaming: true},
	}
	if len(order.Methods) != len(want) {
		t.Fatalf("methods = %d, want %d", len(order.Methods), len(want))
	}
	for i := range want {
		if !proto.Equal(order.Methods[i], want[i]) {
			t.Errorf("method %d = %v, want %v (declaration order)", i, order.Methods[i], want[i])
		}
	}
}
