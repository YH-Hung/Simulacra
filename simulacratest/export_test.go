package simulacratest

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// SetRPCTimeout overrides the per-RPC deadline so a test can bound a hanging
// server in milliseconds instead of waiting out defaultRPCTimeout.
func SetRPCTimeout(c *Client, d time.Duration) { c.timeout = d }

// WrapAdmin replaces the Client's admin clients with wrap's result, so a test
// can inject a failure or a counter a healthy server cannot produce.
func WrapAdmin(c *Client, wrap func(Admin) Admin) { c.admin = wrap(c.admin) }

// SetGlobalFiles replaces the linked registry the extension-identity rule
// consults, so a test can stand in a copy of a file the binary does not link.
func SetGlobalFiles(c *Client, f *protoregistry.Files) { c.globalFiles = f }

// LocalPaths lists the paths in the Client's local snapshot, sorted.
func LocalPaths(c *Client) []string {
	var paths []string
	c.snap.Load().files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		paths = append(paths, fd.Path())
		return true
	})
	slices.Sort(paths)
	return paths
}

func CheckExtensionInstances(m protoreflect.Message) error { return checkExtensionInstances(m) }

func ResolveExtension(c *Client, msg protoreflect.FullName, n protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	return c.resolver().FindExtensionByNumber(msg, n)
}

// DecodeWithClientTypes decodes the way the SDK's own decodes do: extensions
// resolve through the Client's types.
func DecodeWithClientTypes(c *Client, b []byte, into proto.Message) error {
	return proto.UnmarshalOptions{Resolver: c.resolver()}.Unmarshal(b, into)
}

// OutputType runs the Client's automatic registration for method and returns
// what it learned.
func OutputType(t testing.TB, c *Client, method string) (protoreflect.FullName, bool) {
	t.Helper()
	return c.ensure(t, method)
}

// StubDocument is the document Respond would send for msg, without ensure or
// the output type check, so a golden needs no server state.
func StubDocument(b *StubBuilder, msg proto.Message) (string, error) {
	respond, err := respondLiteral(msg, b.c.resolver())
	if err != nil {
		return "", err
	}
	doc, err := json.Marshal(b.document(respond))
	return string(doc), err
}
