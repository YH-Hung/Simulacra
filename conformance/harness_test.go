package conformance_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"gopkg.in/yaml.v3"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

const rpcTimeout = 5 * time.Second

type harness struct {
	reg     *schema.Registry
	conn    *grpc.ClientConn
	journal *journal.Journal
}

type healthChecker interface {
	Check(context.Context, *healthpb.HealthCheckRequest, ...grpc.CallOption) (*healthpb.HealthCheckResponse, error)
}

func awaitServing(ctx context.Context, client healthChecker) error {
	response, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("health Check: %w", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("health Check status = %s, want SERVING", response.GetStatus())
	}
	return nil
}

func waitForServe(serveDone <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-serveDone:
		if err == nil || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case <-timer.C:
		return fmt.Errorf("gRPC Serve did not return after Stop")
	}
}

func waitForJournalCall(calls *journal.Journal, method string, timeout time.Duration) (*journal.Call, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		if recorded := calls.Filter(journal.Filter{Method: method, Limit: 1}); len(recorded) == 1 {
			return recorded[0], nil
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			return nil, fmt.Errorf("journal did not record %s within %s", method, timeout)
		}
	}
}

func newHarness(t *testing.T, stubsYAML string) *harness {
	t.Helper()

	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}

	dir := t.TempDir()
	stubPath := filepath.Join(dir, "stubs.yaml")
	if err := os.WriteFile(stubPath, []byte(stubsYAML), 0o600); err != nil {
		t.Fatalf("write stubs: %v", err)
	}
	compiled, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatalf("LoadDirs(%s): %v", stubPath, errs)
	}
	if len(compiled) == 0 {
		t.Fatal("LoadDirs compiled no stubs")
	}

	calls := journal.New(256)
	server, err := dataplane.New(reg, stub.NewStore(compiled), calls)
	if err != nil {
		t.Fatalf("dataplane.New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
		if err := waitForServe(serveDone, time.Second); err != nil {
			t.Errorf("gRPC Serve shutdown: %v", err)
		}
	})
	healthCtx, cancelHealth := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelHealth()
	if err := awaitServing(healthCtx, healthpb.NewHealthClient(conn)); err != nil {
		t.Fatalf("gRPC server readiness: %v", err)
	}
	return &harness{reg: reg, conn: conn, journal: calls}
}

func (h *harness) method(t *testing.T, method string, want match.Shape) protoreflect.MethodDescriptor {
	t.Helper()
	desc, err := h.reg.LookupMethod(method)
	if err != nil {
		t.Fatalf("LookupMethod(%q): %v", method, err)
	}
	if got := match.ShapeOf(desc); got != want {
		t.Fatalf("%s shape = %s, want %s", method, got, want)
	}
	return desc
}

func (h *harness) message(t *testing.T, desc protoreflect.MessageDescriptor, value proto.Message) *dynamicpb.Message {
	t.Helper()
	data, err := protojson.MarshalOptions{Resolver: h.reg.Types()}.Marshal(value)
	if err != nil {
		t.Fatalf("registry-aware marshal %s: %v", desc.FullName(), err)
	}
	message := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{Resolver: h.reg.Types()}).Unmarshal(data, message); err != nil {
		t.Fatalf("registry-aware unmarshal %s: %v", desc.FullName(), err)
	}
	return message
}

func (h *harness) jsonMessage(t *testing.T, desc protoreflect.MessageDescriptor, body string) *dynamicpb.Message {
	t.Helper()
	message := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{Resolver: h.reg.Types()}).Unmarshal([]byte(body), message); err != nil {
		t.Fatalf("unmarshal %s: %v", desc.FullName(), err)
	}
	return h.message(t, desc, message)
}

func (h *harness) marshalWire(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("proto.Marshal %s: %v", message.ProtoReflect().Descriptor().FullName(), err)
	}
	return data
}

func (h *harness) unmarshalWire(t *testing.T, data []byte, message proto.Message) {
	t.Helper()
	if err := (proto.UnmarshalOptions{Resolver: h.reg.Types()}).Unmarshal(data, message); err != nil {
		t.Fatalf("proto.Unmarshal %s: %v", message.ProtoReflect().Descriptor().FullName(), err)
	}
}

func (h *harness) invoke(t *testing.T, ctx context.Context, method string, req *dynamicpb.Message, opts ...grpc.CallOption) (*dynamicpb.Message, error) {
	t.Helper()
	desc := h.method(t, method, match.Unary)
	response := dynamicpb.NewMessage(desc.Output())
	err := h.conn.Invoke(ctx, method, req, response, opts...)
	if err == nil {
		// gRPC's default protobuf codec resolves extensions only through the
		// process-global registry. Re-decode successful dynamic responses with
		// the schema registry used by this real-client harness so local
		// extensions participate in semantic protobuf equality and reflection.
		resolved := dynamicpb.NewMessage(desc.Output())
		h.unmarshalWire(t, h.marshalWire(t, response), resolved)
		response = resolved
	}
	return response, err
}

func (h *harness) openStream(t *testing.T, ctx context.Context, method string, want match.Shape) (grpc.ClientStream, protoreflect.MethodDescriptor) {
	t.Helper()
	desc := h.method(t, method, want)
	stream, err := h.conn.NewStream(ctx, &grpc.StreamDesc{
		ClientStreams: desc.IsStreamingClient(),
		ServerStreams: desc.IsStreamingServer(),
	}, method)
	if err != nil {
		t.Fatalf("NewStream(%s): %v", method, err)
	}
	return stream, desc
}

type verificationYAML struct {
	Method string       `yaml:"method"`
	Match  *match.Block `yaml:"match"`
	Times  struct {
		Exactly *int `yaml:"exactly"`
		AtLeast *int `yaml:"at_least"`
		AtMost  *int `yaml:"at_most"`
		Never   bool `yaml:"never"`
	} `yaml:"times"`
}

func (h *harness) verify(t *testing.T, body string, wantShape match.Shape) journal.Report {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewBufferString(body))
	dec.KnownFields(true)
	var spec verificationYAML
	if err := dec.Decode(&spec); err != nil {
		t.Fatalf("parse verification YAML: %v", err)
	}
	desc := h.method(t, spec.Method, wantShape)
	matcher, err := match.NewCompiler(h.reg.Files()).Compile(desc.Input(), spec.Match, wantShape)
	if err != nil {
		t.Fatalf("compile verification match: %v", err)
	}
	report, err := journal.Verify(h.journal, spec.Method, matcher, journal.Times{
		Exactly: spec.Times.Exactly,
		AtLeast: spec.Times.AtLeast,
		AtMost:  spec.Times.AtMost,
		Never:   spec.Times.Never,
	})
	if err != nil {
		t.Fatalf("journal.Verify: %v", err)
	}
	if !report.Pass {
		t.Fatalf("verification failed: matched %d of %d, want %s; misses=%v unexpected=%v",
			report.Matched, report.Considered, report.Want, report.Misses, report.UnexpectedMatches)
	}
	return report
}

func fieldString(t *testing.T, message *dynamicpb.Message, name protoreflect.Name) string {
	t.Helper()
	field := message.Descriptor().Fields().ByName(name)
	if field == nil {
		t.Fatalf("%s has no field %s", message.Descriptor().FullName(), name)
	}
	return message.Get(field).String()
}

func richRequest(t *testing.T, h *harness, desc protoreflect.MessageDescriptor, text string) *dynamicpb.Message {
	t.Helper()
	return h.jsonMessage(t, desc, fmt.Sprintf(`{
  "big": "9223372036854775807",
  "ubig": "18446744073709551615",
  "blob": "AQID",
  "optNote": "present",
  "word": "chosen",
  "when": "2026-07-12T01:02:03Z",
  "span": "1.500s",
  "wrapped": "wrapped value",
  "attrs": {"enabled": true, "count": 3},
  "val": "free form",
  "mask": "text,optNote",
  "payload": {"@type": "type.googleapis.com/conformance.v1.Inner", "id": "inside-any"},
  "items": {"first": {"id": "mapped"}},
  "packed": [1, 2, 3],
  "tree": {"label": "root", "next": {"label": "leaf"}},
  "text": %q
}`, text))
}
