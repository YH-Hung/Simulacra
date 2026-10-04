package simulacratest

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// typeSnapshot is an immutable pairing of the Client's local files and the
// dynamic type table over them. dynamicpb.NewTypes keeps reading the Files it
// was given and documents that changing them while resolving is unsafe, and
// parallel subtests sharing a Client resolve types while another subtest
// registers. So a published snapshot is never mutated: registration builds a
// fresh one and swaps the pointer, as the server's schema.Registry does.
type typeSnapshot struct {
	files *protoregistry.Files
	types *dynamicpb.Types
}

func newTypeSnapshot(files *protoregistry.Files) *typeSnapshot {
	return &typeSnapshot{files: files, types: dynamicpb.NewTypes(files)}
}

// chainResolver resolves through GlobalTypes first, so a generated message
// gets its generated extension and payload types, and through the local
// snapshot only for what the test binary does not link. It reports NotFound
// only when both miss.
type chainResolver struct{ local *dynamicpb.Types }

// resolver loads one snapshot, so a single decode resolves through one
// registry however many registrations land while it runs.
func (c *Client) resolver() chainResolver { return chainResolver{local: c.snap.Load().types} }

func (r chainResolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(name)
	if errors.Is(err, protoregistry.NotFound) {
		return r.local.FindMessageByName(name)
	}
	return mt, err
}

func (r chainResolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByURL(url)
	if errors.Is(err, protoregistry.NotFound) {
		return r.local.FindMessageByURL(url)
	}
	return mt, err
}

func (r chainResolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	xt, err := protoregistry.GlobalTypes.FindExtensionByName(name)
	if errors.Is(err, protoregistry.NotFound) {
		return r.local.FindExtensionByName(name)
	}
	return xt, err
}

func (r chainResolver) FindExtensionByNumber(msg protoreflect.FullName, n protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	xt, err := protoregistry.GlobalTypes.FindExtensionByNumber(msg, n)
	if errors.Is(err, protoregistry.NotFound) {
		return r.local.FindExtensionByNumber(msg, n)
	}
	return xt, err
}

// RegisterFiles registers fds and everything they import on the server, in
// one RegisterSchemas, and makes their types resolvable in the test process
// for the Client's own decoding. A file the server already holds is a no-op
// there.
//
// Extensions are matched by descriptor identity: an extension decoded through
// one instance of a file is invisible through another instance of the same
// file, with no error. So a file that declares extensions must reach this
// Client as one instance, the one the test builds its messages and extension
// values from; a different instance of it is refused. One case nothing can
// detect: an extension descriptor never passed here that extends a message
// that was. Build extension values only from extension descriptors you
// registered, or from generated code.
func (c *Client) RegisterFiles(t testing.TB, fds ...protoreflect.FileDescriptor) {
	t.Helper()
	c.registerFiles(t, "RegisterFiles", fds)
}

const (
	identityRefusal = "simulacratest: %s: %s declares extensions and is already registered with this Client as a different descriptor instance; extensions are matched by descriptor identity, so values decoded through the registered copy would be invisible through this one — register one instance and build extension values from it"
	linkedRefusal   = "simulacratest: %s: %s declares extensions and is linked into the test binary as a different descriptor instance; extensions are matched by descriptor identity, so values decoded through the linked copy would be invisible through this one — compile against the linked copy (resolve imports from protoregistry.GlobalFiles)"
)

func (c *Client) registerFiles(t testing.TB, op string, fds []protoreflect.FileDescriptor) {
	t.Helper()
	// The mutex spans check, RPC, build and swap. The atomic pointer alone
	// would let two registrations build from one snapshot and the second
	// swap lose the first's files; releasing it between check and swap would
	// let another registration slip a different instance of a checked path
	// in. Holding it across the RPC serializes registrations, which are rare,
	// and readers never take it. It is released by defer because t.Fatalf
	// leaves through runtime.Goexit, which runs deferred calls only: an
	// unlock after a failure point would never run, and the next
	// registration on this Client would deadlock.
	c.regMu.Lock()
	defer c.regMu.Unlock()
	cur := c.snap.Load()

	files, dup := closure(fds)
	if dup != "" {
		t.Fatalf(identityRefusal, op, dup)
	}
	for _, fd := range files {
		if !declaresExtensions(fd) {
			continue
		}
		// GlobalTypes wins resolution, so a linked copy that is not this
		// instance would always be the one decoding.
		if otherInstance(c.globalFiles, fd) {
			t.Fatalf(linkedRefusal, op, fd.Path())
		}
		if otherInstance(cur.files, fd) {
			t.Fatalf(identityRefusal, op, fd.Path())
		}
	}

	set := &descriptorpb.FileDescriptorSet{}
	for _, fd := range files {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("simulacratest: %s: %v", op, err)
	}
	ctx, cancel := c.rpcContext()
	defer cancel()
	if _, err := c.admin.Schema.RegisterSchemas(ctx, connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: b})); err != nil {
		c.fatalRPC(t, op+": RegisterSchemas", err)
	}

	// Locally only after the server accepted, so the Client never resolves a
	// type the server rejected. A path already linked or registered keeps
	// its first instance, matching the server's first-registration-wins.
	var all []protoreflect.FileDescriptor
	cur.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		all = append(all, fd)
		return true
	})
	for _, fd := range files {
		if !holds(c.globalFiles, fd.Path()) && !holds(cur.files, fd.Path()) {
			all = append(all, fd)
		}
	}
	next := new(protoregistry.Files)
	for _, fd := range all {
		if err := next.RegisterFile(fd); err != nil {
			t.Fatalf("simulacratest: %s: %v", op, err)
		}
	}
	c.snap.Store(newTypeSnapshot(next))
}

// closure walks fds and their imports depth-first, dependencies before
// importers, one file per path. dup names an extension-declaring path reached
// as two different instances: only one of them could be registered, so the
// identity rule refuses it as it would an already registered copy.
func closure(fds []protoreflect.FileDescriptor) (files []protoreflect.FileDescriptor, dup string) {
	seen := map[string]protoreflect.FileDescriptor{}
	var walk func(fd protoreflect.FileDescriptor)
	walk = func(fd protoreflect.FileDescriptor) {
		if prev, ok := seen[fd.Path()]; ok {
			if prev != fd && dup == "" && declaresExtensions(fd) {
				dup = fd.Path()
			}
			return
		}
		seen[fd.Path()] = fd
		imports := fd.Imports()
		for i := range imports.Len() {
			walk(imports.Get(i).FileDescriptor)
		}
		files = append(files, fd)
	}
	for _, fd := range fds {
		walk(fd)
	}
	return files, dup
}

func holds(files *protoregistry.Files, path string) bool {
	_, err := files.FindFileByPath(path)
	return err == nil
}

// otherInstance reports whether files holds fd's path as a different
// descriptor instance.
func otherInstance(files *protoregistry.Files, fd protoreflect.FileDescriptor) bool {
	got, err := files.FindFileByPath(fd.Path())
	return err == nil && got != fd
}

// declaresExtensions reports whether fd declares an extension at file scope
// or nested in any message.
func declaresExtensions(fd protoreflect.FileDescriptor) bool {
	return fd.Extensions().Len() > 0 || messagesDeclareExtensions(fd.Messages())
}

func messagesDeclareExtensions(mds protoreflect.MessageDescriptors) bool {
	for i := range mds.Len() {
		md := mds.Get(i)
		if md.Extensions().Len() > 0 || messagesDeclareExtensions(md.Messages()) {
			return true
		}
	}
	return false
}

// checkExtensionInstances is the decode-side half of the identity rule. A
// message built from a different instance of a registered file decodes its
// extensions without error, but stores them under the registered instance's
// descriptors, where they are invisible through the caller's own: Has is
// false and Get returns the default. Range yields the stored descriptor, so
// requiring every populated extension, at any depth, to extend the very
// instance of the message holding it catches that. It cannot catch an
// extension instance that shares the registered extendee; RegisterFiles
// refuses those up front.
func checkExtensionInstances(m protoreflect.Message) error {
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.IsExtension() && fd.ContainingMessage() != m.Descriptor() {
			err = fmt.Errorf("message built from a different descriptor instance of %s than the one registered with this Client; build messages from the FileDescriptor passed to RegisterFiles", m.Descriptor().ParentFile().Path())
			return false
		}
		err = checkValueInstances(fd, v)
		return err == nil
	})
	return err
}

// checkValueInstances recurses into the messages a field value holds.
func checkValueInstances(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch {
	case fd.IsMap():
		if fd.MapValue().Message() == nil {
			return nil
		}
		var err error
		v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
			err = checkExtensionInstances(mv.Message())
			return err == nil
		})
		return err
	case fd.IsList():
		if fd.Message() == nil {
			return nil
		}
		list := v.List()
		for i := range list.Len() {
			if err := checkExtensionInstances(list.Get(i).Message()); err != nil {
				return err
			}
		}
		return nil
	case fd.Message() != nil:
		return checkExtensionInstances(v.Message())
	}
	return nil
}
