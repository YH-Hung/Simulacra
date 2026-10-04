package simulacratest_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/simulacratest"
)

// recordingTB stands in for the *testing.T an SDK helper is given, so a test
// can assert on what the helper reported instead of failing itself. It
// overrides exactly the three methods the SDK reports through. Fatalf must
// stop the goroutine like the real one, or a helper would run on past its own
// failure.
type recordingTB struct {
	testing.TB

	mu     sync.Mutex
	errors []string
	fatal  string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fatal = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

// Errors returns every Errorf message, in order.
func (r *recordingTB) Errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

// FatalMsg returns the Fatalf message, or "" if the helper never failed
// fatally. It cannot be named Fatal: testing.TB already has Fatal(...any), and
// a different signature would stop recordingTB from being a testing.TB.
func (r *recordingTB) FatalMsg() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatal
}

// runRecorded runs f against a recordingTB on its own goroutine, because
// Fatalf ends the goroutine it is called on. The deferred close runs on
// Goexit too, so the join sees a fatal exit as well as a normal return.
func runRecorded(t *testing.T, f func(tb testing.TB)) *recordingTB {
	t.Helper()
	rec := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(rec)
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatalf("runRecorded: f did not return within 10s")
	}
	return rec
}

// hangingListener returns the address of a listener that accepts connections
// and never responds, so an RPC stays in flight until its deadline.
func hangingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	return ln.Addr().String()
}

// closeCountingListener tracks how many accepted connections are still open
// and signals on each close, so a test can wait for the client side to let go
// without sleeping.
type closeCountingListener struct {
	net.Listener
	closed chan struct{}

	mu   sync.Mutex
	open int
}

func newCloseCountingListener(ln net.Listener) *closeCountingListener {
	// One slot suffices: a signal that finds the slot full leaves one pending,
	// and waitAllClosed re-reads the count after every wakeup.
	return &closeCountingListener{Listener: ln, closed: make(chan struct{}, 1)}
}

func (l *closeCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.open++
	l.mu.Unlock()
	return &countedConn{Conn: c, lis: l}, nil
}

type countedConn struct {
	net.Conn
	lis  *closeCountingListener
	once sync.Once
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.lis.mu.Lock()
		c.lis.open--
		c.lis.mu.Unlock()
		select {
		case c.lis.closed <- struct{}{}:
		default:
		}
	})
	return err
}

// waitAllClosed fails the test if an accepted connection is still open after
// within.
func (l *closeCountingListener) waitAllClosed(t *testing.T, within time.Duration) {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		l.mu.Lock()
		n := l.open
		l.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-l.closed:
		case <-timer.C:
			t.Fatalf("%d accepted connection(s) still open after %v", n, within)
		}
	}
}

// countingSchemaClient counts RegisterSchemas calls and keeps the last
// response, so a test can assert that a refusal sent nothing and see what the
// server added.
type countingSchemaClient struct {
	adminv1connect.SchemaServiceClient
	registers int
	last      *adminv1.RegisterSchemasResponse
}

func (c *countingSchemaClient) RegisterSchemas(ctx context.Context, req *connect.Request[adminv1.RegisterSchemasRequest]) (*connect.Response[adminv1.RegisterSchemasResponse], error) {
	c.registers++
	resp, err := c.SchemaServiceClient.RegisterSchemas(ctx, req)
	if err == nil {
		c.last = resp.Msg
	}
	return resp, err
}

// countSchemaRPCs installs a countingSchemaClient on c.
func countSchemaRPCs(c *simulacratest.Client) *countingSchemaClient {
	counter := &countingSchemaClient{}
	simulacratest.WrapAdmin(c, func(a simulacratest.Admin) simulacratest.Admin {
		counter.SchemaServiceClient = a.Schema
		a.Schema = counter
		return a
	})
	return counter
}

// conformanceFiles compiles every file under conformance/v1, keyed by path.
// Each call compiles afresh, so two calls yield independent descriptor
// instances of the same files.
func conformanceFiles(t *testing.T) map[string]protoreflect.FileDescriptor {
	t.Helper()
	const root = "../conformance/protos"
	matches, err := filepath.Glob(filepath.Join(root, "conformance/v1/*.proto"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("finding conformance protos: %v (%d found)", err, len(matches))
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatalf("relative path of %s: %v", m, err)
		}
		names[i] = filepath.ToSlash(rel)
	}
	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{root}}),
	}
	compiled, err := compiler.Compile(context.Background(), names...)
	if err != nil {
		t.Fatalf("compiling conformance protos: %v", err)
	}
	files := make(map[string]protoreflect.FileDescriptor, len(compiled))
	for _, f := range compiled {
		files[f.Path()] = f
	}
	return files
}

// buildFile builds a file from a FileDescriptorProto in text format, resolving
// its imports against deps. Every call is a new descriptor instance.
func buildFile(t *testing.T, text string, deps ...protoreflect.FileDescriptor) protoreflect.FileDescriptor {
	t.Helper()
	fdp := new(descriptorpb.FileDescriptorProto)
	if err := prototext.Unmarshal([]byte(text), fdp); err != nil {
		t.Fatalf("parsing descriptor text: %v", err)
	}
	resolver := new(protoregistry.Files)
	for _, d := range deps {
		if err := resolver.RegisterFile(d); err != nil {
			t.Fatalf("registering dependency %s: %v", d.Path(), err)
		}
	}
	fd, err := protodesc.NewFile(fdp, resolver)
	if err != nil {
		t.Fatalf("building %s: %v", fdp.GetName(), err)
	}
	return fd
}

// sharedExtendee builds probe/ext.proto twice against one instance of
// probe/base.proto: the case where the extendee is shared, so only the
// extension descriptors differ and nothing in a decoded message reveals it.
func sharedExtendee(t *testing.T) (base, e1, e2 protoreflect.FileDescriptor) {
	t.Helper()
	const baseText = `name: "probe/base.proto" package: "probe" syntax: "proto2"
message_type { name: "Base" extension_range { start: 100 end: 200 } }`
	const extText = `name: "probe/ext.proto" package: "probe" syntax: "proto2"
dependency: "probe/base.proto"
extension { name: "tag" number: 100 label: LABEL_OPTIONAL type: TYPE_STRING extendee: ".probe.Base" }`
	base = buildFile(t, baseText)
	return base, buildFile(t, extText, base), buildFile(t, extText, base)
}

// syntheticFile is syn/file_<i>.proto, package syn<i>, with one message M.
func syntheticFile(t *testing.T, i int) protoreflect.FileDescriptor {
	t.Helper()
	return buildFile(t, fmt.Sprintf(`name: "syn/file_%d.proto" package: "syn%d" syntax: "proto3" message_type { name: "M" }`, i, i))
}

// nestedExtFile declares an extendee, a message holding it in a repeated
// field, and an extension of it, all in one file. Each call is a new instance.
func nestedExtFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	return buildFile(t, `name: "nested/x.proto" package: "nested" syntax: "proto2"
message_type { name: "Base" extension_range { start: 100 end: 200 } }
message_type { name: "Outer" field { name: "items" number: 1 label: LABEL_REPEATED type: TYPE_MESSAGE type_name: ".nested.Base" } }
extension { name: "tag" number: 100 label: LABEL_OPTIONAL type: TYPE_STRING extendee: ".nested.Base" }`)
}
