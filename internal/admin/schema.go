package admin

import (
	"context"
	"fmt"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// schemaService implements simulacra.admin.v1.SchemaService.
//
// It deliberately does not embed UnimplementedSchemaServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type schemaService struct {
	deps Deps
}

// RegisterSchemas applies a descriptor set to the served registry,
// all-or-nothing (design §4.2).
func (s *schemaService) RegisterSchemas(
	_ context.Context,
	req *connect.Request[adminv1.RegisterSchemasRequest],
) (*connect.Response[adminv1.RegisterSchemasResponse], error) {
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(req.Msg.GetDescriptorSet(), set); err != nil {
		return nil, connectError(invalidArgument(fmt.Errorf("descriptor_set is not a valid FileDescriptorSet: %w", err)))
	}
	added, err := s.deps.Registry.RegisterSet(set)
	if err != nil {
		return nil, connectError(invalidArgument(err))
	}
	sort.Strings(added)
	files := make([]string, len(added))
	for i, path := range added {
		files[i] = validUTF8(path)
	}
	return connect.NewResponse(&adminv1.RegisterSchemasResponse{
		RegisteredFiles: files,
		ServiceCount:    int32(len(s.deps.Registry.Services())),
	}), nil
}

// ListServices lists every registered service, sorted by fully-qualified name,
// with its methods in declaration order (design §4.2).
func (s *schemaService) ListServices(
	_ context.Context,
	_ *connect.Request[adminv1.ListServicesRequest],
) (*connect.Response[adminv1.ListServicesResponse], error) {
	services := s.deps.Registry.Services()
	sort.Slice(services, func(i, j int) bool { return services[i].FullName() < services[j].FullName() })
	infos := make([]*adminv1.ServiceInfo, len(services))
	for i, svc := range services {
		infos[i] = serviceInfo(svc)
	}
	return connect.NewResponse(&adminv1.ListServicesResponse{Services: infos}), nil
}

func serviceInfo(svc protoreflect.ServiceDescriptor) *adminv1.ServiceInfo {
	info := &adminv1.ServiceInfo{
		Name: validUTF8(string(svc.FullName())),
		File: validUTF8(svc.ParentFile().Path()),
	}
	methods := svc.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		info.Methods = append(info.Methods, &adminv1.MethodInfo{
			Name:            validUTF8(string(m.Name())),
			InputType:       validUTF8(string(m.Input().FullName())),
			OutputType:      validUTF8(string(m.Output().FullName())),
			ClientStreaming: m.IsStreamingClient(),
			ServerStreaming: m.IsStreamingServer(),
		})
	}
	return info
}
