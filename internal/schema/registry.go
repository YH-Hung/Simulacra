// Package schema builds and queries a registry of protobuf descriptors from
// runtime-compiled .proto trees and descriptor-set files. It is the single
// source of truth for "what services and message shapes does the mock know".
package schema

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type Registry struct {
	mu   sync.Mutex // serializes load→build→swap; readers never take it
	snap atomic.Pointer[snapshot]
}

// snapshot is an immutable pairing of a descriptor index and its derived
// dynamic type table. Once stored in Registry.snap it is never mutated;
// mutation builds a fresh snapshot and swaps the pointer.
type snapshot struct {
	files *protoregistry.Files
	types *dynamicpb.Types
}

func newSnapshot(files *protoregistry.Files) *snapshot {
	return &snapshot{files: files, types: dynamicpb.NewTypes(files)}
}

func NewRegistry() *Registry {
	r := &Registry{}
	r.snap.Store(newSnapshot(new(protoregistry.Files)))
	return r
}

func (r *Registry) current() *snapshot { return r.snap.Load() }

// Snapshot returns the current descriptor set. Read-only by convention:
// registering into a returned snapshot is a data race with every other
// holder. It exists for the one consumer that needs the concrete type —
// match.NewCompiler (RangeFiles + dynamicpb.NewTypes) — everything else
// should use the Registry's own resolver methods, which stay live across
// registrations.
func (r *Registry) Snapshot() *protoregistry.Files { return r.current().files }

// FindFileByPath implements protodesc.Resolver against the live snapshot.
func (r *Registry) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	return r.current().files.FindFileByPath(path)
}

// FindDescriptorByName implements protodesc.Resolver against the live snapshot.
func (r *Registry) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	return r.current().files.FindDescriptorByName(name)
}

// apply runs one mutation as load→build→swap: it copies the current snapshot
// into a fresh candidate, lets fn extend the candidate, and publishes the
// candidate only if fn succeeds. The mutex makes concurrent registrations
// serialize instead of losing updates; the failed-fn path leaves the served
// snapshot byte-for-byte untouched (all-or-nothing).
func (r *Registry) apply(fn func(candidate *protoregistry.Files) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	candidate := new(protoregistry.Files)
	var copyErr error
	r.current().files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		copyErr = candidate.RegisterFile(fd)
		return copyErr == nil
	})
	if copyErr != nil {
		return fmt.Errorf("copying registry snapshot: %w", copyErr)
	}
	if err := fn(candidate); err != nil {
		return err
	}
	r.snap.Store(newSnapshot(candidate))
	return nil
}

// addNew registers fd and, first, all of its imports into files. A path that
// is already present is skipped (first registration wins). Newly registered
// paths are appended to added when it is non-nil.
func addNew(files *protoregistry.Files, fd protoreflect.FileDescriptor, added *[]string) error {
	if _, err := files.FindFileByPath(fd.Path()); err == nil {
		return nil
	}
	imps := fd.Imports()
	for i := 0; i < imps.Len(); i++ {
		if err := addNew(files, imps.Get(i).FileDescriptor, added); err != nil {
			return err
		}
	}
	if err := files.RegisterFile(fd); err != nil {
		return err
	}
	if added != nil {
		*added = append(*added, fd.Path())
	}
	return nil
}

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
	return r.apply(func(candidate *protoregistry.Files) error {
		for _, fd := range compiled {
			if err := addNew(candidate, fd, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddFile registers a file descriptor and its imports as one atomic swap.
// A path that is already registered is skipped (first registration wins).
func (r *Registry) AddFile(fd protoreflect.FileDescriptor) error {
	return r.apply(func(candidate *protoregistry.Files) error {
		return addNew(candidate, fd, nil)
	})
}

// AddDescriptorSetFile loads a serialized FileDescriptorSet (e.g. a buf image
// or `protoc --descriptor_set_out --include_imports` output). The set must be
// self-contained: every import must be included in the set.
func (r *Registry) AddDescriptorSetFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading descriptor set: %w", err)
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(data, set); err != nil {
		return fmt.Errorf("%s is not a valid FileDescriptorSet: %w", path, err)
	}
	if len(set.File) == 0 {
		return fmt.Errorf("%s is not a valid FileDescriptorSet: contains no files", path)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return fmt.Errorf("loading %s (descriptor sets must be self-contained; build with `buf build -o` or `protoc --include_imports`): %w", path, err)
	}
	return r.apply(func(candidate *protoregistry.Files) error {
		var regErr error
		files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			regErr = addNew(candidate, fd, nil)
			return regErr == nil
		})
		return regErr
	})
}

// ErrUnknownMethod reports a well-formed method name that no registered schema
// declares: the service is absent, or the method is absent on a present
// service. The admin plane answers it with NOT_FOUND.
var ErrUnknownMethod = errors.New("unknown method")

// unknownMethodError keeps LookupMethod's established message text, which
// loader and check diagnostics print, while matching ErrUnknownMethod.
type unknownMethodError struct{ msg string }

func (e *unknownMethodError) Error() string        { return e.msg }
func (e *unknownMethodError) Is(target error) bool { return target == ErrUnknownMethod }

// LookupMethod resolves "pkg.Service/Method" or "/pkg.Service/Method".
func (r *Registry) LookupMethod(fullMethod string) (protoreflect.MethodDescriptor, error) {
	name := strings.TrimPrefix(fullMethod, "/")
	idx := strings.LastIndex(name, "/")
	if idx <= 0 || idx == len(name)-1 {
		return nil, fmt.Errorf("invalid method name %q (want package.Service/Method)", fullMethod)
	}
	svcName, methodName := name[:idx], name[idx+1:]
	// A name that cannot be a protobuf identifier is malformed, not absent, and
	// must stay untyped: ErrUnknownMethod becomes NOT_FOUND at the admin
	// surface, which tells a client to register its schemas — and no schema can
	// declare a service called "bad service" or a method called "Get Order".
	// This also rejects extra separators, which the split above leaves inside
	// the service name.
	if !protoreflect.FullName(svcName).IsValid() || !protoreflect.Name(methodName).IsValid() {
		return nil, fmt.Errorf("invalid method name %q (want package.Service/Method)", fullMethod)
	}
	d, err := r.current().files.FindDescriptorByName(protoreflect.FullName(svcName))
	if err != nil {
		return nil, &unknownMethodError{msg: fmt.Sprintf("service %q is not registered (no schema source declares it)", svcName)}
	}
	svc, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a service", svcName)
	}
	m := svc.Methods().ByName(protoreflect.Name(methodName))
	if m == nil {
		return nil, &unknownMethodError{msg: fmt.Sprintf("method %q not found on service %q", methodName, svcName)}
	}
	return m, nil
}

// LookupMessage resolves a fully-qualified message name against the
// registry's files, falling back to the process-global registry.
func (r *Registry) LookupMessage(name string) (protoreflect.MessageDescriptor, error) {
	full := protoreflect.FullName(name)
	d, err := r.current().files.FindDescriptorByName(full)
	if errors.Is(err, protoregistry.NotFound) {
		d, err = protoregistry.GlobalFiles.FindDescriptorByName(full)
	}
	if err != nil {
		if !errors.Is(err, protoregistry.NotFound) {
			return nil, fmt.Errorf("resolving message type %q from registered schemas: %w", name, err)
		}
		return nil, fmt.Errorf("message type %q is not registered (add its .proto to a schema source)", name)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a message type", name)
	}
	return md, nil
}

func (r *Registry) Types() *Types { return &Types{reg: r} }

// Types is a registry-first, global-fallback protobuf type resolver. It reads
// the registry's current snapshot on every call, so a message type registered
// after Types was constructed still resolves.
type Types struct {
	reg *Registry
}

func (t *Types) FindMessageByName(n protoreflect.FullName) (protoreflect.MessageType, error) {
	mt, err := t.reg.current().types.FindMessageByName(n)
	if err == nil {
		return mt, nil
	}
	if !errors.Is(err, protoregistry.NotFound) {
		return nil, err
	}
	return protoregistry.GlobalTypes.FindMessageByName(n)
}

func (t *Types) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	mt, err := t.reg.current().types.FindMessageByURL(url)
	if err == nil {
		return mt, nil
	}
	if !errors.Is(err, protoregistry.NotFound) {
		return nil, err
	}
	return protoregistry.GlobalTypes.FindMessageByURL(url)
}

func (t *Types) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error) {
	et, err := t.reg.current().types.FindExtensionByName(field)
	if err == nil {
		return et, nil
	}
	if !errors.Is(err, protoregistry.NotFound) {
		return nil, err
	}
	return protoregistry.GlobalTypes.FindExtensionByName(field)
}

func (t *Types) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	et, err := t.reg.current().types.FindExtensionByNumber(message, field)
	if err == nil {
		return et, nil
	}
	if !errors.Is(err, protoregistry.NotFound) {
		return nil, err
	}
	return protoregistry.GlobalTypes.FindExtensionByNumber(message, field)
}

// Services returns every service descriptor across all registered files.
func (r *Registry) Services() []protoreflect.ServiceDescriptor {
	var out []protoreflect.ServiceDescriptor
	r.current().files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			out = append(out, svcs.Get(i))
		}
		return true
	})
	return out
}

// RegisterSet registers every file of a serialized-set image all-or-nothing:
// on any error the served registry is untouched. The set must be
// self-contained (every import present). Idempotent: a path already
// registered with equivalent content is skipped; the same path with
// different content fails, naming the file. Returns the paths newly added
// (empty when every file was already present).
func (r *Registry) RegisterSet(set *descriptorpb.FileDescriptorSet) ([]string, error) {
	if len(set.GetFile()) == 0 {
		return nil, errors.New("descriptor set contains no files")
	}
	incoming, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("loading descriptor set (descriptor sets must be self-contained; build with `buf build -o` or `protoc --include_imports`): %w", err)
	}
	var added []string
	err = r.apply(func(candidate *protoregistry.Files) error {
		var rangeErr error
		incoming.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			existing, findErr := candidate.FindFileByPath(fd.Path())
			if findErr == nil {
				if !descriptorsEquivalent(existing, fd) {
					rangeErr = fmt.Errorf("file %q is already registered with different content", fd.Path())
				}
				return rangeErr == nil
			}
			// A file registered from the incoming set keeps descriptor-internal
			// references to the incoming copies of its imports even when the
			// candidate already indexed equivalent copies — safe precisely
			// because equivalence was just verified, and lookups by name always
			// resolve the candidate's copy. RangeFiles order is unspecified,
			// which is fine: addNew registers imports before importers, and a
			// path skipped at recursion time is still equivalence-checked when
			// the outer range reaches its own visit.
			rangeErr = addNew(candidate, fd, &added)
			return rangeErr == nil
		})
		return rangeErr
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// descriptorsEquivalent compares two copies of the same proto file for
// semantic equality, seeing through representation-only differences.
func descriptorsEquivalent(a, b protoreflect.FileDescriptor) bool {
	return proto.Equal(
		normalizeFileProto(protodesc.ToFileDescriptorProto(a)),
		normalizeFileProto(protodesc.ToFileDescriptorProto(b)),
	)
}

// bufImageField is the per-file extension buf stamps onto each descriptor in
// an image (buf.alpha.image.v1.ImageFileExtension). Parsing an image as a
// plain FileDescriptorSet keeps it as unknown bytes on the file itself.
const bufImageField = 8042

// normalizeFileProto strips the two representation-only differences observed
// between toolchains (design §2.6, verified empirically): source code info,
// and buf's image extension.
//
// It is deliberately surgical. An earlier version round-tripped the whole
// message with proto.UnmarshalOptions{DiscardUnknown: true}, which also
// discarded unknown fields *nested* inside option messages — where custom
// options live whenever their extension is not linked into this binary. Two
// same-path files differing only in a custom option then compared equal and
// were silently accepted, which is exactly the conflict the admin contract
// requires us to reject. Descriptor meaning may not be touched here:
// loosening equivalence must never mask a real conflict.
func normalizeFileProto(fdp *descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto {
	clone := proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	clone.SourceCodeInfo = nil
	if unknown := clone.ProtoReflect().GetUnknown(); len(unknown) > 0 {
		clone.ProtoReflect().SetUnknown(dropUnknownField(unknown, bufImageField))
	}
	return clone
}

// dropUnknownField removes every occurrence of one field number from a raw
// unknown-fields buffer, preserving every other unknown field byte-for-byte.
// A malformed buffer is returned untouched: refusing to interpret it is
// safer than truncating it into something that might compare equal.
func dropUnknownField(raw protoreflect.RawFields, drop protowire.Number) protoreflect.RawFields {
	var out protoreflect.RawFields
	rest := raw
	for len(rest) > 0 {
		num, typ, tagLen := protowire.ConsumeTag(rest)
		if tagLen < 0 {
			return raw
		}
		valLen := protowire.ConsumeFieldValue(num, typ, rest[tagLen:])
		if valLen < 0 {
			return raw
		}
		total := tagLen + valLen
		if num != drop {
			out = append(out, rest[:total]...)
		}
		rest = rest[total:]
	}
	return out
}
