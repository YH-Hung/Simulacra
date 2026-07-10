// Package schema builds and queries a registry of protobuf descriptors from
// runtime-compiled .proto trees and descriptor-set files. It is the single
// source of truth for "what services and message shapes does the mock know".
package schema

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type Registry struct {
	files *protoregistry.Files
}

func NewRegistry() *Registry {
	return &Registry{files: new(protoregistry.Files)}
}

// Files exposes the registry as a protodesc.Resolver (used by grpc reflection).
func (r *Registry) Files() *protoregistry.Files { return r.files }

// AddProtoDir compiles every .proto file found under root, treating root as
// the single import path (imports inside the files are resolved relative to
// root; well-known types are provided automatically).
func (r *Registry) AddProtoDir(ctx context.Context, root string) error {
	var names []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".proto") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanning %s: %w", root, err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no .proto files found under %s", root)
	}

	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(
			&protocompile.SourceResolver{ImportPaths: []string{root}},
		),
	}
	compiled, err := compiler.Compile(ctx, names...)
	if err != nil {
		return fmt.Errorf("compiling protos under %s: %w", root, err)
	}
	for _, fd := range compiled {
		if err := r.AddFile(fd); err != nil {
			return err
		}
	}
	return nil
}

// AddFile registers a file descriptor and, first, all of its imports.
// A path that is already registered is skipped (first registration wins).
func (r *Registry) AddFile(fd protoreflect.FileDescriptor) error {
	if _, err := r.files.FindFileByPath(fd.Path()); err == nil {
		return nil
	}
	imps := fd.Imports()
	for i := 0; i < imps.Len(); i++ {
		if err := r.AddFile(imps.Get(i).FileDescriptor); err != nil {
			return err
		}
	}
	return r.files.RegisterFile(fd)
}

// LookupMethod resolves "pkg.Service/Method" or "/pkg.Service/Method".
func (r *Registry) LookupMethod(fullMethod string) (protoreflect.MethodDescriptor, error) {
	name := strings.TrimPrefix(fullMethod, "/")
	idx := strings.LastIndex(name, "/")
	if idx <= 0 || idx == len(name)-1 {
		return nil, fmt.Errorf("invalid method name %q (want package.Service/Method)", fullMethod)
	}
	svcName, methodName := name[:idx], name[idx+1:]
	d, err := r.files.FindDescriptorByName(protoreflect.FullName(svcName))
	if err != nil {
		return nil, fmt.Errorf("service %q is not registered (no schema source declares it)", svcName)
	}
	svc, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a service", svcName)
	}
	m := svc.Methods().ByName(protoreflect.Name(methodName))
	if m == nil {
		return nil, fmt.Errorf("method %q not found on service %q", methodName, svcName)
	}
	return m, nil
}

// Services returns every service descriptor across all registered files.
func (r *Registry) Services() []protoreflect.ServiceDescriptor {
	var out []protoreflect.ServiceDescriptor
	r.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			out = append(out, svcs.Get(i))
		}
		return true
	})
	return out
}
