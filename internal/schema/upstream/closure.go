// Package upstream imports descriptor sets from a running gRPC server's
// reflection endpoint.
//
// It has two halves that are deliberately separable: Closure is a pure walk
// over a Fetcher, testable with no network at all, and client.go implements
// Fetcher over a real reflection stream.
package upstream

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Fetcher retrieves descriptor bytes from an upstream server. Every method
// returns serialized FileDescriptorProto messages, and a single call may
// return more than one: grpc-go answers FileContainingSymbol with the whole
// transitive closure (F4). That is an implementation's choice, not a
// guarantee, which is why Closure itself resolves whatever a Fetcher leaves
// missing.
type Fetcher interface {
	ListServices(ctx context.Context) ([]string, error)
	FileContainingSymbol(ctx context.Context, symbol string) ([][]byte, error)
	FileByFilename(ctx context.Context, name string) ([][]byte, error)
}

// skippedServices are the services Simulacra's data plane implements itself.
//
// This is a rule, not a list: all three are registered on our grpc.Server
// ahead of UnknownServiceHandler (internal/dataplane/server.go:44-61), so a
// stub for any of them is unreachable — importing them cannot enable mocking
// them, it only adds noise to every `schema list` thereafter.
//
// Health additionally breaks the import outright (F6). The data plane
// pre-registers its own compiled-in grpc/health/v1/health.proto, and
// RegisterSet rejects a same-path file whose content differs; because
// registration is all-or-nothing, one disagreeing upstream health descriptor
// fails the entire set and names a service the user never asked to import.
//
// Skipping happens at service enumeration only. A file that arrives as a
// genuine transitive dependency is kept — dropping it would produce a
// non-self-contained set, which RegisterSchemas rejects for a different
// reason (design §4, §9).
var skippedServices = []string{
	"grpc.reflection.v1.ServerReflection",
	"grpc.reflection.v1alpha.ServerReflection",
	"grpc.health.v1.Health",
}

// Closure walks the upstream and returns a FileDescriptorSet in topological
// order: every file appears after the files it imports, which is what protoc
// --include_imports and buf build -o produce, and what our own
// RegisterSchemas requires.
func Closure(ctx context.Context, f Fetcher) (*descriptorpb.FileDescriptorSet, error) {
	services, err := f.ListServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the upstream's services: %w", err)
	}

	c := &closure{
		f:        f,
		byName:   map[string]*descriptorpb.FileDescriptorProto{},
		importer: map[string]string{},
		done:     map[string]bool{},
		visiting: map[string]bool{},
	}

	skip := map[string]bool{}
	for _, s := range skippedServices {
		skip[s] = true
	}
	for _, svc := range services {
		if skip[svc] {
			continue
		}
		raw, err := f.FileContainingSymbol(ctx, svc)
		if err != nil {
			return nil, fmt.Errorf("fetching the file containing %s: %w", svc, err)
		}
		if err := c.absorb(raw); err != nil {
			return nil, err
		}
	}
	if err := c.resolve(ctx); err != nil {
		return nil, err
	}
	return c.emitAll()
}

// closure is the walk's mutable state.
//
// It is single-use: constructed inside Closure, never escapes it, and is
// discarded whether or not the walk succeeds. That is what makes it safe
// that emit does not clear visiting on its error path -- a failed closure is
// never reused, so a visiting entry left set past an error can never cause a
// false cycle report later.
type closure struct {
	f      Fetcher
	byName map[string]*descriptorpb.FileDescriptorProto
	// importer records which file first named a dependency, so a missing one
	// can say who wanted it.
	importer map[string]string
	out      []*descriptorpb.FileDescriptorProto
	done     map[string]bool
	visiting map[string]bool
}

// absorb decodes descriptors and files them by path, first one winning.
func (c *closure) absorb(raw [][]byte) error {
	for _, b := range raw {
		fd := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(b, fd); err != nil {
			return fmt.Errorf("parsing a descriptor returned by the upstream: %w", err)
		}
		name := fd.GetName()
		if prior, ok := c.byName[name]; ok {
			// One server is one source of truth. Two different definitions of
			// one path is an upstream bug, and saying so is more useful than
			// silently keeping whichever arrived first.
			if !proto.Equal(prior, fd) {
				return fmt.Errorf(
					"the upstream returned two different definitions of %q; "+
						"this is a bug in the server being imported", name)
			}
			continue
		}
		c.byName[name] = fd
		for _, dep := range fd.GetDependency() {
			if _, ok := c.importer[dep]; !ok {
				c.importer[dep] = name
			}
		}
	}
	return nil
}

// resolve fetches dependencies the upstream did not volunteer, until the set
// is closed. Against grpc-go this usually does nothing (F4).
func (c *closure) resolve(ctx context.Context) error {
	for {
		var missing []string
		for _, fd := range c.byName {
			for _, dep := range fd.GetDependency() {
				if _, ok := c.byName[dep]; !ok {
					missing = append(missing, dep)
				}
			}
		}
		if len(missing) == 0 {
			return nil
		}
		// Sorted so a failure is reported deterministically rather than
		// depending on map iteration order.
		sort.Strings(missing)
		for _, dep := range missing {
			if _, ok := c.byName[dep]; ok {
				continue // a sibling fetch already brought it in
			}
			raw, err := c.f.FileByFilename(ctx, dep)
			if err != nil {
				return fmt.Errorf("fetching %q, imported by %q: %w", dep, c.importer[dep], err)
			}
			if err := c.absorb(raw); err != nil {
				return err
			}
			if _, ok := c.byName[dep]; !ok {
				return fmt.Errorf(
					"the upstream does not have %q, imported by %q; "+
						"the descriptor set it serves is not self-contained",
					dep, c.importer[dep])
			}
		}
	}
}

// emitAll walks every absorbed file in sorted order, emitting dependencies
// first. Sorted so the output is deterministic rather than dependent on map
// iteration order.
func (c *closure) emitAll() (*descriptorpb.FileDescriptorSet, error) {
	roots := make([]string, 0, len(c.byName))
	for name := range c.byName {
		roots = append(roots, name)
	}
	sort.Strings(roots)
	for _, name := range roots {
		if err := c.emit(name, nil); err != nil {
			return nil, err
		}
	}
	return &descriptorpb.FileDescriptorSet{File: c.out}, nil
}

// emit is the post-order DFS. Proto forbids circular imports, so a cycle
// means a broken upstream; without the visiting guard this would recurse
// until the stack gave out instead of saying so.
func (c *closure) emit(name string, stack []string) error {
	if c.done[name] {
		return nil
	}
	if c.visiting[name] {
		return fmt.Errorf("the upstream's descriptors import in a cycle: %s",
			strings.Join(append(stack, name), " -> "))
	}
	fd, ok := c.byName[name]
	if !ok {
		// Unreachable after resolve: every dependency emit walks has already
		// been confirmed present in c.byName, or resolve itself would have
		// failed. Kept as a defensive check rather than a bare index.
		return fmt.Errorf("internal: %q was never absorbed", name)
	}
	c.visiting[name] = true
	for _, dep := range fd.GetDependency() {
		if err := c.emit(dep, append(stack, name)); err != nil {
			return err
		}
	}
	delete(c.visiting, name)
	c.done[name] = true
	c.out = append(c.out, fd)
	return nil
}
