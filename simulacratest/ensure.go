package simulacratest

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// ensure makes the method's service known to the server where it can, and
// returns the method's output type and whether the server or the test binary
// knows the method. It does at most one lookup per service per Client: the
// server's own listing first, so automatic registration only fills gaps and
// never collides with explicit provisioning, then the test binary's linked
// generated code. A service neither knows is not cached, so a later
// registration is picked up.
func (c *Client) ensure(t testing.TB, method string) (output protoreflect.FullName, known bool) {
	t.Helper()
	_, service, name, ok := normalizeMethod(method)
	if !ok {
		return "", false
	}
	// Released by defer: registerFiles can t.Fatalf, which leaves through
	// runtime.Goexit, and an explicit unlock after it would never run. Lock
	// order is ensureMu then regMu; nothing takes them the other way round.
	c.ensureMu.Lock()
	defer c.ensureMu.Unlock()
	methods, cached := c.services[service]
	if !cached {
		if methods = c.discover(t, service); methods == nil {
			return "", false
		}
		if c.services == nil {
			c.services = map[protoreflect.FullName]map[protoreflect.Name]protoreflect.FullName{}
		}
		c.services[service] = methods
	}
	output, known = methods[name]
	return output, known
}

// discover returns a service's methods and their output types, or nil when
// neither the server nor the test binary knows the service. The caller holds
// ensureMu.
func (c *Client) discover(t testing.TB, service protoreflect.FullName) map[protoreflect.Name]protoreflect.FullName {
	t.Helper()
	ctx, cancel := c.rpcContext()
	defer cancel()
	resp, err := c.admin.Schema.ListServices(ctx, connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		c.fatalRPC(t, "ListServices", err)
	}
	for _, s := range resp.Msg.GetServices() {
		if protoreflect.FullName(s.GetName()) != service {
			continue
		}
		methods := make(map[protoreflect.Name]protoreflect.FullName, len(s.GetMethods()))
		for _, m := range s.GetMethods() {
			methods[protoreflect.Name(m.GetName())] = protoreflect.FullName(m.GetOutputType())
		}
		return methods
	}

	d, err := c.globalFiles.FindDescriptorByName(service)
	if err != nil {
		return nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	c.registerFiles(t, fmt.Sprintf("auto-registering %s from the test binary", service), []protoreflect.FileDescriptor{sd.ParentFile()})
	mds := sd.Methods()
	methods := make(map[protoreflect.Name]protoreflect.FullName, mds.Len())
	for i := range mds.Len() {
		md := mds.Get(i)
		methods[md.Name()] = md.Output().FullName()
	}
	return methods
}

// notFoundHint is the one SDK line appended to a NOT_FOUND from a call whose
// method ensure could not find.
func notFoundHint(service string) string {
	return fmt.Sprintf("simulacratest: hint: the test binary does not link generated code for %s; register its schema with RegisterFiles or WithProtoDir", service)
}

// notFoundHintFor is the hint line, if any, to append to a NOT_FOUND for
// method. The hint blames missing generated code, which is true only when the
// service is unknown to both the server and the test binary: ensure caches every
// service it finds and never an unknown one. A known service with a mistyped
// method gets the server's message alone.
func (c *Client) notFoundHintFor(method string) []string {
	_, service, _, ok := normalizeMethod(method)
	if !ok {
		return nil
	}
	c.ensureMu.Lock()
	defer c.ensureMu.Unlock()
	if _, known := c.services[service]; known {
		return nil
	}
	return []string{notFoundHint(string(service))}
}

// normalizeMethod accepts "/pkg.Svc/M" or "pkg.Svc/M" and returns the first
// form, the service's full name, and the method's name. ok is false when the
// method is not exactly one service and one method name.
func normalizeMethod(method string) (full string, service protoreflect.FullName, name protoreflect.Name, ok bool) {
	svc, m, found := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	service, name = protoreflect.FullName(svc), protoreflect.Name(m)
	if !found || !service.IsValid() || !name.IsValid() {
		return "", "", "", false
	}
	return "/" + svc + "/" + m, service, name, true
}
