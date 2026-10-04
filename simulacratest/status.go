package simulacratest

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// codeNames are the canonical names, indexed by code. The server reads a code
// with codes.Code.UnmarshalJSON, which accepts exactly these and rejects
// codes.Code.String()'s CamelCase ("FailedPrecondition").
var codeNames = [17]string{"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND",
	"ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED",
	"OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED"}

// RespondStatus sends the stub with st as its response and returns the
// server's envelope for it.
//
// The status message is sent as written, never as a template. Each detail is
// unpacked through the Client's types and rendered like a Respond message,
// with the same round-trip guard, so the client receives details equal to
// st's; a detail whose type neither the test binary links nor the test
// registered is refused. An OK status is sent too, and the server's own
// diagnostic for it is what the test sees.
func (b *StubBuilder) RespondStatus(t testing.TB, st *status.Status) *adminv1.Stub {
	t.Helper()
	if errs := slices.Concat(b.match.errors(), b.errs); len(errs) > 0 {
		t.Fatalf("%s", strings.Join(errs, "\n"))
	}
	if st == nil {
		t.Fatalf("simulacratest: RespondStatus: status is nil")
	}
	if st.Code() >= codes.Code(len(codeNames)) {
		t.Fatalf("simulacratest: RespondStatus: code %d has no canonical name", st.Code())
	}
	// ensure registers the method's linked schemas, which is where its
	// details' types may come from, and lets CreateStub hint at an unknown
	// service. The output type is not checked: no message is sent.
	b.c.ensure(t, b.method)
	r := b.c.resolver()
	doc := statusDoc{Code: codeNames[st.Code()], Message: st.Message()}
	for i, a := range st.Proto().GetDetails() {
		msg, err := anypb.UnmarshalNew(a, proto.UnmarshalOptions{Resolver: r})
		if err != nil {
			t.Fatalf("simulacratest: RespondStatus: detail %d: cannot resolve %s", i, a.GetTypeUrl())
		}
		tree, err := renderMessage(msg, r)
		if err == nil {
			err = guardRoundTrip(tree, msg, r)
		}
		if err != nil {
			t.Fatalf("simulacratest: RespondStatus: detail %d: %v", i, err)
		}
		doc.Details = append(doc.Details, detailDoc{Type: string(msg.ProtoReflect().Descriptor().FullName()), Value: tree})
	}
	return b.send(t, "RespondStatus", respondDoc{Literal: true, Status: &doc})
}
