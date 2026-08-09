package admin_test

import (
	"bytes"
	"maps"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

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
	// Idempotency is asserted because RPC_SAME_IDEMPOTENCY_LEVEL is breaking
	// under the FILE category buf.yaml selects: a level dropped by accident
	// can only be restored with a breaking exception. The zero value is
	// IDEMPOTENCY_UNKNOWN, so only the pure-read RPCs name it below.
	Idempotency descriptorpb.MethodOptions_IdempotencyLevel
}

const noSideEffects = descriptorpb.MethodOptions_NO_SIDE_EFFECTS

// wantSurface is the admin contract as designed. Adding an RPC is a deliberate
// one-line edit here — the right amount of friction for a public API.
var wantSurface = map[string][]methodSpec{
	"simulacra.admin.v1.SchemaService": {
		{Name: "RegisterSchemas", Input: "simulacra.admin.v1.RegisterSchemasRequest", Output: "simulacra.admin.v1.RegisterSchemasResponse"},
		{Name: "ListServices", Input: "simulacra.admin.v1.ListServicesRequest", Output: "simulacra.admin.v1.ListServicesResponse", Idempotency: noSideEffects},
	},
	"simulacra.admin.v1.StubService": {
		{Name: "CreateStub", Input: "simulacra.admin.v1.CreateStubRequest", Output: "simulacra.admin.v1.CreateStubResponse"},
		{Name: "ListStubs", Input: "simulacra.admin.v1.ListStubsRequest", Output: "simulacra.admin.v1.ListStubsResponse", Idempotency: noSideEffects},
		{Name: "DeleteStub", Input: "simulacra.admin.v1.DeleteStubRequest", Output: "simulacra.admin.v1.DeleteStubResponse"},
		{Name: "ReplaceAllStubs", Input: "simulacra.admin.v1.ReplaceAllStubsRequest", Output: "simulacra.admin.v1.ReplaceAllStubsResponse"},
		{Name: "ExportStubs", Input: "simulacra.admin.v1.ExportStubsRequest", Output: "simulacra.admin.v1.ExportStubsResponse", Idempotency: noSideEffects},
	},
	"simulacra.admin.v1.JournalService": {
		{Name: "ListCalls", Input: "simulacra.admin.v1.ListCallsRequest", Output: "simulacra.admin.v1.ListCallsResponse", Idempotency: noSideEffects},
		{Name: "WatchCalls", ServerStreaming: true, Input: "simulacra.admin.v1.WatchCallsRequest", Output: "simulacra.admin.v1.WatchCallsResponse", Idempotency: noSideEffects},
		{Name: "ResetJournal", Input: "simulacra.admin.v1.ResetJournalRequest", Output: "simulacra.admin.v1.ResetJournalResponse"},
	},
	"simulacra.admin.v1.VerifyService": {
		{Name: "VerifyCalls", Input: "simulacra.admin.v1.VerifyCallsRequest", Output: "simulacra.admin.v1.VerifyCallsResponse", Idempotency: noSideEffects},
	},
	"simulacra.admin.v1.ControlService": {
		{Name: "GetServerInfo", Input: "simulacra.admin.v1.GetServerInfoRequest", Output: "simulacra.admin.v1.GetServerInfoResponse", Idempotency: noSideEffects},
		{Name: "Reset", Input: "simulacra.admin.v1.ResetRequest", Output: "simulacra.admin.v1.ResetResponse"},
		{Name: "Shutdown", Input: "simulacra.admin.v1.ShutdownRequest", Output: "simulacra.admin.v1.ShutdownResponse"},
	},
}

// contractFiles is every file in the admin module. Naming them explicitly,
// rather than ranging over a registry, is what makes a dropped file fail.
func contractFiles() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		adminv1.File_simulacra_admin_v1_schema_proto,
		adminv1.File_simulacra_admin_v1_stub_proto,
		adminv1.File_simulacra_admin_v1_journal_proto,
		adminv1.File_simulacra_admin_v1_verify_proto,
		adminv1.File_simulacra_admin_v1_control_proto,
	}
}

func TestGeneratedSurfaceMatchesTheAdminContract(t *testing.T) {
	got := make(map[string][]methodSpec)
	for _, fd := range contractFiles() {
		services := fd.Services()
		for i := range services.Len() {
			svc := services.Get(i)
			methods := svc.Methods()
			specs := make([]methodSpec, 0, methods.Len())
			for j := range methods.Len() {
				m := methods.Get(j)
				opts, _ := m.Options().(*descriptorpb.MethodOptions)
				specs = append(specs, methodSpec{
					Name:            string(m.Name()),
					ClientStreaming: m.IsStreamingClient(),
					ServerStreaming: m.IsStreamingServer(),
					Input:           string(m.Input().FullName()),
					Output:          string(m.Output().FullName()),
					Idempotency:     opts.GetIdempotencyLevel(),
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

// TestEnumValuesAreStable pins both admin enums by name and number. Enum
// values are the other half of the surface that renumbering silently breaks:
// a client that already serialized STUB_ORIGIN_API as 2 keeps sending 2.
func TestEnumValuesAreStable(t *testing.T) {
	wantEnums := map[string]map[string]protoreflect.EnumNumber{
		"simulacra.admin.v1.StubOrigin": {
			"STUB_ORIGIN_UNSPECIFIED": 0,
			"STUB_ORIGIN_FILE":        1,
			"STUB_ORIGIN_API":         2,
		},
		"simulacra.admin.v1.StubShape": {
			"STUB_SHAPE_UNSPECIFIED":   0,
			"STUB_SHAPE_UNARY":         1,
			"STUB_SHAPE_SERVER_STREAM": 2,
			"STUB_SHAPE_CLIENT_STREAM": 3,
			"STUB_SHAPE_BIDI_STREAM":   4,
		},
	}

	got := make(map[string]map[string]protoreflect.EnumNumber)
	for _, fd := range contractFiles() {
		enums := fd.Enums()
		for i := range enums.Len() {
			e := enums.Get(i)
			values := make(map[string]protoreflect.EnumNumber)
			for j := range e.Values().Len() {
				v := e.Values().Get(j)
				values[string(v.Name())] = v.Number()
			}
			got[string(e.FullName())] = values
		}
	}

	if len(got) != len(wantEnums) {
		t.Errorf("enum set = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(wantEnums)))
	}
	for name, want := range wantEnums {
		have, ok := got[name]
		if !ok {
			t.Errorf("enum %s is missing from the generated descriptors", name)
			continue
		}
		if !maps.Equal(have, want) {
			t.Errorf("enum %s values = %v, want %v", name, have, want)
		}
	}
}

// TestJournalCarriesUntypedPayloadsWithoutAny guards the two journal shapes
// that a "natural" definition gets wrong, both of which fail only at runtime.
//
// MetadataEntry.binary_values must stay bytes: -bin metadata values are
// arbitrary bytes, a proto3 string must be valid UTF-8, and proto.Marshal
// rejects one that is not — so a repeated string field cannot carry them.
//
// CallStatus.details must not be google.protobuf.Any: details are built from
// dynamically registered types, and Connect's default JSON codec resolves Any
// against protoregistry.GlobalTypes, where those types do not exist.
func TestJournalCarriesUntypedPayloadsWithoutAny(t *testing.T) {
	binValues := (&adminv1.MetadataEntry{}).ProtoReflect().Descriptor().Fields().ByName("binary_values")
	if binValues == nil {
		t.Fatal("MetadataEntry has no binary_values field")
	}
	if got := binValues.Kind(); got != protoreflect.BytesKind {
		t.Errorf("MetadataEntry.binary_values kind = %v, want %v", got, protoreflect.BytesKind)
	}

	raw := []byte{0x00, 0xff, 0xfe, 0x80} // invalid UTF-8, legal -bin metadata
	entry := &adminv1.MetadataEntry{Key: "trace-bin", BinaryValues: [][]byte{raw}}
	encoded, err := proto.Marshal(entry)
	if err != nil {
		t.Fatalf("marshaling raw -bin metadata: %v", err)
	}
	var decoded adminv1.MetadataEntry
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshaling raw -bin metadata: %v", err)
	}
	if len(decoded.BinaryValues) != 1 || !bytes.Equal(decoded.BinaryValues[0], raw) {
		t.Errorf("binary_values round trip = %v, want [%v]", decoded.BinaryValues, raw)
	}

	details := (&adminv1.CallStatus{}).ProtoReflect().Descriptor().Fields().ByName("details")
	if details == nil {
		t.Fatal("CallStatus has no details field")
	}
	if got := string(details.Message().FullName()); got != "simulacra.admin.v1.DecodedMessage" {
		t.Errorf("CallStatus.details message = %s, want simulacra.admin.v1.DecodedMessage", got)
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
