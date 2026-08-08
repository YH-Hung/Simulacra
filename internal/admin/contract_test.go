package admin_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

// The connect constructors are part of the contract Phase 4 builds on. These
// references fail the build if a service is renamed or dropped from the protos.
var (
	_ = adminv1connect.NewSchemaServiceClient
	_ = adminv1connect.NewStubServiceClient
	_ = adminv1connect.NewJournalServiceClient
	_ = adminv1connect.NewVerifyServiceClient
	_ = adminv1connect.NewControlServiceClient

	_ = adminv1connect.NewSchemaServiceHandler
	_ = adminv1connect.NewStubServiceHandler
	_ = adminv1connect.NewJournalServiceHandler
	_ = adminv1connect.NewVerifyServiceHandler
	_ = adminv1connect.NewControlServiceHandler
)

type methodSpec struct {
	Name            string
	ClientStreaming bool
	ServerStreaming bool
	// Input and Output are the fully-qualified request/response message
	// names, e.g. "simulacra.admin.v1.ListStubsRequest". Asserting them
	// catches a type swap (an RPC kept its name but started taking or
	// returning the wrong message) that name-and-streaming-flags alone would
	// miss.
	Input  string
	Output string
}

// wantSurface is the admin contract as designed. Adding an RPC is a deliberate
// one-line edit here — the right amount of friction for a public API.
var wantSurface = map[string][]methodSpec{
	"simulacra.admin.v1.SchemaService": {
		{Name: "RegisterSchemas", Input: "simulacra.admin.v1.RegisterSchemasRequest", Output: "simulacra.admin.v1.RegisterSchemasResponse"},
		{Name: "ListServices", Input: "simulacra.admin.v1.ListServicesRequest", Output: "simulacra.admin.v1.ListServicesResponse"},
	},
	"simulacra.admin.v1.StubService": {
		{Name: "CreateStub", Input: "simulacra.admin.v1.CreateStubRequest", Output: "simulacra.admin.v1.CreateStubResponse"},
		{Name: "ListStubs", Input: "simulacra.admin.v1.ListStubsRequest", Output: "simulacra.admin.v1.ListStubsResponse"},
		{Name: "DeleteStub", Input: "simulacra.admin.v1.DeleteStubRequest", Output: "simulacra.admin.v1.DeleteStubResponse"},
		{Name: "ReplaceAllStubs", Input: "simulacra.admin.v1.ReplaceAllStubsRequest", Output: "simulacra.admin.v1.ReplaceAllStubsResponse"},
		{Name: "ExportStubs", Input: "simulacra.admin.v1.ExportStubsRequest", Output: "simulacra.admin.v1.ExportStubsResponse"},
	},
	"simulacra.admin.v1.JournalService": {
		{Name: "ListCalls", Input: "simulacra.admin.v1.ListCallsRequest", Output: "simulacra.admin.v1.ListCallsResponse"},
		{Name: "WatchCalls", ServerStreaming: true, Input: "simulacra.admin.v1.WatchCallsRequest", Output: "simulacra.admin.v1.WatchCallsResponse"},
		{Name: "ResetJournal", Input: "simulacra.admin.v1.ResetJournalRequest", Output: "simulacra.admin.v1.ResetJournalResponse"},
	},
	"simulacra.admin.v1.VerifyService": {
		{Name: "VerifyCalls", Input: "simulacra.admin.v1.VerifyCallsRequest", Output: "simulacra.admin.v1.VerifyCallsResponse"},
	},
	"simulacra.admin.v1.ControlService": {
		{Name: "GetServerInfo", Input: "simulacra.admin.v1.GetServerInfoRequest", Output: "simulacra.admin.v1.GetServerInfoResponse"},
		{Name: "Reset", Input: "simulacra.admin.v1.ResetRequest", Output: "simulacra.admin.v1.ResetResponse"},
		{Name: "Shutdown", Input: "simulacra.admin.v1.ShutdownRequest", Output: "simulacra.admin.v1.ShutdownResponse"},
	},
}

func TestGeneratedSurfaceMatchesTheAdminContract(t *testing.T) {
	files := []protoreflect.FileDescriptor{
		adminv1.File_simulacra_admin_v1_schema_proto,
		adminv1.File_simulacra_admin_v1_stub_proto,
		adminv1.File_simulacra_admin_v1_journal_proto,
		adminv1.File_simulacra_admin_v1_verify_proto,
		adminv1.File_simulacra_admin_v1_control_proto,
	}

	got := make(map[string][]methodSpec)
	for _, fd := range files {
		services := fd.Services()
		for i := range services.Len() {
			svc := services.Get(i)
			methods := svc.Methods()
			specs := make([]methodSpec, 0, methods.Len())
			for j := range methods.Len() {
				m := methods.Get(j)
				specs = append(specs, methodSpec{
					Name:            string(m.Name()),
					ClientStreaming: m.IsStreamingClient(),
					ServerStreaming: m.IsStreamingServer(),
					Input:           string(m.Input().FullName()),
					Output:          string(m.Output().FullName()),
				})
			}
			got[string(svc.FullName())] = specs
		}
	}

	for name, want := range wantSurface {
		have, ok := got[name]
		if !ok {
			t.Errorf("service %s is missing from the generated descriptors", name)
			continue
		}
		if !slices.Equal(have, want) {
			t.Errorf("service %s methods = %+v, want %+v", name, have, want)
		}
	}
	for name := range got {
		if _, ok := wantSurface[name]; !ok {
			t.Errorf("unexpected service %s in the generated descriptors", name)
		}
	}
}

// TestTimesFieldsArePresenceTracked guards the correction that made Times a
// plain message instead of a oneof: at_least and at_most must be expressible
// together as a range, and an unset bound must be distinguishable from zero.
func TestTimesFieldsArePresenceTracked(t *testing.T) {
	rng := &adminv1.Times{AtLeast: proto.Int32(1), AtMost: proto.Int32(3)}
	if rng.AtLeast == nil || rng.AtMost == nil {
		t.Fatal("at_least and at_most must be settable together; a oneof would forbid it")
	}
	if rng.Exactly != nil {
		t.Errorf("Exactly = %v, want absent", rng.Exactly)
	}

	zero := &adminv1.Times{AtMost: proto.Int32(0)}
	if zero.AtMost == nil || zero.GetAtMost() != 0 {
		t.Errorf("at_most = 0 must be distinguishable from unset, got %v", zero.AtMost)
	}
	if zero.AtLeast != nil {
		t.Errorf("AtLeast = %v, want absent", zero.AtLeast)
	}
}

// TestResetRequestDistinguishesOmittedFromFalse guards ControlService.Reset's
// documented semantics: an omitted field means "reset it", explicit false skips.
func TestResetRequestDistinguishesOmittedFromFalse(t *testing.T) {
	omitted := &adminv1.ResetRequest{}
	if omitted.Stubs != nil || omitted.Journal != nil {
		t.Fatalf("empty ResetRequest must leave both fields absent, got stubs=%v journal=%v",
			omitted.Stubs, omitted.Journal)
	}

	explicit := &adminv1.ResetRequest{Stubs: proto.Bool(false)}
	if explicit.Stubs == nil {
		t.Fatal("explicit false must be present, not absent")
	}
	if explicit.GetStubs() {
		t.Error("GetStubs() = true, want false")
	}
	if explicit.Journal != nil {
		t.Error("Journal must remain absent when only Stubs is set")
	}
}
