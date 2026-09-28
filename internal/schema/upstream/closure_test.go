package upstream

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fakeFetcher is an in-memory upstream. files maps a filename to the
// descriptor served for it; symbols maps a service name to the filenames
// returned for FileContainingSymbol, mimicking F4 (a server may return the
// closure, or just the one file).
type fakeFetcher struct {
	services []string
	symbols  map[string][]string
	files    map[string]*descriptorpb.FileDescriptorProto
	// calls records every FileByFilename lookup, so a test can prove a
	// diamond dependency was fetched once rather than twice.
	calls []string
}

func (f *fakeFetcher) ListServices(context.Context) ([]string, error) {
	return f.services, nil
}

func (f *fakeFetcher) FileContainingSymbol(_ context.Context, sym string) ([][]byte, error) {
	return f.marshal(f.symbols[sym])
}

func (f *fakeFetcher) FileByFilename(_ context.Context, name string) ([][]byte, error) {
	f.calls = append(f.calls, name)
	return f.marshal([]string{name})
}

func (f *fakeFetcher) marshal(names []string) ([][]byte, error) {
	var out [][]byte
	for _, n := range names {
		fd, ok := f.files[n]
		if !ok {
			continue // an upstream that does not have the file returns nothing
		}
		raw, err := proto.Marshal(fd)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// file is a terse FileDescriptorProto builder for the tests.
func file(name string, deps ...string) *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:       proto.String(name),
		Dependency: deps,
	}
}

// names renders a set as its filenames, in emitted order.
func names(set *descriptorpb.FileDescriptorSet) []string {
	var out []string
	for _, f := range set.GetFile() {
		out = append(out, f.GetName())
	}
	return out
}

func TestClosureEmitsDependenciesBeforeDependents(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"shop.v1.OrderService"},
		symbols:  map[string][]string{"shop.v1.OrderService": {"order.proto", "ts.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"order.proto": file("order.proto", "ts.proto"),
			"ts.proto":    file("ts.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	got := names(set)
	want := []string{"ts.proto", "order.proto"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (dependencies must precede dependents)", got, want)
		}
	}
}

func TestClosureSkipsServicesTheDataPlaneImplements(t *testing.T) {
	f := &fakeFetcher{
		services: []string{
			"shop.v1.OrderService",
			"grpc.reflection.v1.ServerReflection",
			"grpc.reflection.v1alpha.ServerReflection",
			"grpc.health.v1.Health",
		},
		symbols: map[string][]string{
			"shop.v1.OrderService":                     {"order.proto"},
			"grpc.reflection.v1.ServerReflection":      {"reflection.proto"},
			"grpc.reflection.v1alpha.ServerReflection": {"reflection_alpha.proto"},
			"grpc.health.v1.Health":                    {"health.proto"},
		},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"order.proto":            file("order.proto"),
			"reflection.proto":       file("reflection.proto"),
			"reflection_alpha.proto": file("reflection_alpha.proto"),
			"health.proto":           file("health.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	got := names(set)
	if len(got) != 1 || got[0] != "order.proto" {
		t.Fatalf("got %v, want only [order.proto]; reflection and health are "+
			"implemented by the data plane and cannot be mocked (design §4)", got)
	}
}

// An upstream that returns only the named file, never its dependencies —
// the opposite of grpc-go's behavior (F4), and the reason the recursion exists.
func TestClosureFetchesDependenciesTheUpstreamWithheld(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"shop.v1.OrderService"},
		symbols:  map[string][]string{"shop.v1.OrderService": {"order.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"order.proto": file("order.proto", "ts.proto"),
			"ts.proto":    file("ts.proto", "base.proto"),
			"base.proto":  file("base.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	got := names(set)
	want := []string{"base.proto", "ts.proto", "order.proto"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A diamond: two files importing one shared dependency. It must be fetched
// once and emitted once.
func TestClosureFetchesADiamondDependencyOnce(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"a.A", "b.B"},
		symbols: map[string][]string{
			"a.A": {"a.proto"},
			"b.B": {"b.proto"},
		},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"a.proto":      file("a.proto", "shared.proto"),
			"b.proto":      file("b.proto", "shared.proto"),
			"shared.proto": file("shared.proto"),
		},
	}
	set, err := Closure(context.Background(), f)
	if err != nil {
		t.Fatalf("Closure: %v", err)
	}
	var shared int
	for _, n := range names(set) {
		if n == "shared.proto" {
			shared++
		}
	}
	if shared != 1 {
		t.Fatalf("shared.proto emitted %d times, want 1; got %v", shared, names(set))
	}
	var fetches int
	for _, c := range f.calls {
		if c == "shared.proto" {
			fetches++
		}
	}
	if fetches != 1 {
		t.Fatalf("shared.proto fetched %d times, want exactly 1 (calls: %v)", fetches, f.calls)
	}
}

// An upstream that does not have a file something imports. The error must
// name both the missing file and who wanted it.
func TestClosureMissingDependencyNamesTheImporter(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"a.A"},
		symbols:  map[string][]string{"a.A": {"a.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"a.proto": file("a.proto", "gone.proto"),
		},
	}
	_, err := Closure(context.Background(), f)
	if err == nil {
		t.Fatal("Closure succeeded, want an error naming the missing dependency")
	}
	for _, want := range []string{"gone.proto", "a.proto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// missingFetcher mimics what a real reflection server does for a missing
// file: FileByFilename itself fails with a NotFound status, rather than
// succeeding with an empty result the way fakeFetcher's FileByFilename
// does. This is the path resolve's first error branch actually takes in
// practice; fakeFetcher can only exercise its second branch (the RPC
// succeeds emptily and the file is simply absent from byName afterward).
type missingFetcher struct {
	services []string
	symbols  map[string][]string
	files    map[string]*descriptorpb.FileDescriptorProto
}

func (f *missingFetcher) ListServices(context.Context) ([]string, error) { return f.services, nil }

func (f *missingFetcher) FileContainingSymbol(_ context.Context, sym string) ([][]byte, error) {
	var out [][]byte
	for _, n := range f.symbols[sym] {
		raw, err := proto.Marshal(f.files[n])
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func (f *missingFetcher) FileByFilename(_ context.Context, name string) ([][]byte, error) {
	fd, ok := f.files[name]
	if !ok {
		return nil, status.Error(codes.NotFound, "File not found.")
	}
	raw, err := proto.Marshal(fd)
	if err != nil {
		return nil, err
	}
	return [][]byte{raw}, nil
}

// The first failure branch in resolve: FileByFilename itself errors (real
// reflection servers return NotFound rather than an empty success). Design
// §4 requires the error to name the missing file and its importer either
// way; only the second branch was covered before this test.
//
// Named mutation check: reverting the fix (dropping ", imported by %q" from
// the fmt.Errorf in resolve's first branch, back to "fetching %q: %w") makes
// this fail on the missing "a.proto" substring below.
func TestClosureMissingDependencyFetchErrorNamesTheImporter(t *testing.T) {
	f := &missingFetcher{
		services: []string{"a.A"},
		symbols:  map[string][]string{"a.A": {"a.proto"}},
		files:    map[string]*descriptorpb.FileDescriptorProto{"a.proto": file("a.proto", "gone.proto")},
	}
	_, err := Closure(context.Background(), f)
	if err == nil {
		t.Fatal("Closure succeeded, want an error naming the missing dependency and its importer")
	}
	for _, want := range []string{"gone.proto", "a.proto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	// The upstream's own diagnostic must survive verbatim, not be reworded.
	if !strings.Contains(err.Error(), "File not found.") {
		t.Fatalf("error %q does not preserve the upstream's verbatim diagnostic", err)
	}
}

// conflictFetcher answers two services with two different definitions of one
// filename — something fakeFetcher's single files map cannot express.
type conflictFetcher struct {
	first, second *descriptorpb.FileDescriptorProto
}

func (c *conflictFetcher) ListServices(context.Context) ([]string, error) {
	return []string{"a.A", "b.B"}, nil
}

func (c *conflictFetcher) FileContainingSymbol(_ context.Context, sym string) ([][]byte, error) {
	fd := c.first
	if sym == "b.B" {
		fd = c.second
	}
	raw, err := proto.Marshal(fd)
	if err != nil {
		return nil, err
	}
	return [][]byte{raw}, nil
}

func (c *conflictFetcher) FileByFilename(context.Context, string) ([][]byte, error) {
	return nil, nil
}

// Two different definitions of one path from one server.
func TestClosureConflictingDefinitionsAreAnError(t *testing.T) {
	conflicting := file("dup.proto")
	conflicting.Package = proto.String("second")
	_, err := Closure(context.Background(), &conflictFetcher{
		first:  file("dup.proto"),
		second: conflicting,
	})
	if err == nil {
		t.Fatal("Closure succeeded, want an error naming the conflicting file")
	}
	if !strings.Contains(err.Error(), "dup.proto") {
		t.Fatalf("error %q does not name the conflicting file", err)
	}
}

// Proto forbids circular imports, so this can only come from a broken
// upstream — but it must be reported rather than recursed into forever.
func TestClosureDetectsAnImportCycle(t *testing.T) {
	f := &fakeFetcher{
		services: []string{"a.A"},
		symbols:  map[string][]string{"a.A": {"a.proto", "b.proto"}},
		files: map[string]*descriptorpb.FileDescriptorProto{
			"a.proto": file("a.proto", "b.proto"),
			"b.proto": file("b.proto", "a.proto"),
		},
	}
	_, err := Closure(context.Background(), f)
	if err == nil {
		t.Fatal("Closure succeeded on a cyclic import graph, want an error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not report a cycle", err)
	}
}
