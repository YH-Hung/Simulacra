package simulacratest

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// Call is one call the server recorded, as Calls returns it.
type Call struct {
	c *Client
	p *adminv1.Call
}

// Calls returns the calls the server recorded for method, given as
// "/pkg.Svc/M" or "pkg.Svc/M", oldest first, so calls[0] is the first call;
// "" returns every method's. Once the journal has wrapped, only the calls it
// still retains are returned.
func (c *Client) Calls(t testing.TB, method string) []Call {
	t.Helper()
	// No ensure: ListCalls filters by name and never reads the registry, so
	// registering here would be a side effect with no purpose.
	ctx, cancel := c.rpcContext()
	defer cancel()
	resp, err := c.admin.Journal.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{Method: method}))
	if err != nil {
		c.fatalRPC(t, "ListCalls", err)
	}
	// The server lists newest first.
	listed := resp.Msg.GetCalls()
	calls := make([]Call, len(listed))
	for i, p := range listed {
		calls[len(listed)-1-i] = Call{c: c, p: p}
	}
	return calls
}

// Request decodes the call's request into into, which must be of the type the
// server recorded. A call with other than one request (a client or
// bidirectional stream) is refused, as is a request of another type: decoding
// one type's bytes as another usually succeeds, with the data in unknown
// fields. Extensions resolve through the Client's types, so a registered
// extension lands in its field; into must be built from the descriptors passed
// to RegisterFiles (see the package doc).
func (k Call) Request(t testing.TB, into proto.Message) {
	t.Helper()
	k.capture(t, "Request", "requests", k.p.GetRequests(), into, "")
}

// Response decodes the call's response into into, checked like Request. A
// call that failed has no response, and the refusal names its status.
func (k Call) Response(t testing.TB, into proto.Message) {
	t.Helper()
	st := k.p.GetStatus()
	name := fmt.Sprint(st.GetCode())
	if code := st.GetCode(); code >= 0 && int(code) < len(codeNames) {
		name = codeNames[code]
	}
	k.capture(t, "Response", "responses", k.p.GetResponses(), into, fmt.Sprintf(" (status %s: %s)", name, st.GetMessage()))
}

// capture decodes the one message in msgs into into. ifNone follows the count
// when msgs is empty.
func (k Call) capture(t testing.TB, op, noun string, msgs []*adminv1.DecodedMessage, into proto.Message, ifNone string) {
	t.Helper()
	// A typed nil pointer is a non-nil interface whose reflection is invalid;
	// decoding into it would panic.
	if into == nil || !into.ProtoReflect().IsValid() {
		t.Fatalf("simulacratest: %s: target message is nil", op)
	}
	seq := k.p.GetSeq()
	if len(msgs) != 1 {
		suffix := ""
		if len(msgs) == 0 {
			suffix = ifNone
		}
		t.Fatalf("simulacratest: %s: call seq=%d has %d %s%s; use Proto() for streaming calls", op, seq, len(msgs), noun, suffix)
	}
	want := into.ProtoReflect().Descriptor().FullName()
	if got := msgs[0].GetTypeName(); got != string(want) {
		t.Fatalf("simulacratest: %s: call seq=%d carries %s, not %s", op, seq, got, want)
	}
	if err := (proto.UnmarshalOptions{Resolver: k.c.resolver()}).Unmarshal(msgs[0].GetWireBytes(), into); err != nil {
		t.Fatalf("simulacratest: %s: call seq=%d: %v", op, seq, err)
	}
	// An extension decoded through the registered instance of a file is
	// invisible through another instance's descriptors, with no error.
	if err := checkExtensionInstances(into.ProtoReflect()); err != nil {
		t.Fatalf("simulacratest: %s: %v", op, err)
	}
}

// Metadata returns the request metadata the server received. Each -bin key's
// values are the decoded bytes, as grpc-go presents them.
func (k Call) Metadata() metadata.MD {
	md := metadata.MD{}
	for _, e := range k.p.GetRequestMetadata() {
		md[e.GetKey()] = append(md[e.GetKey()], e.GetValues()...)
		for _, b := range e.GetBinaryValues() {
			md[e.GetKey()] = append(md[e.GetKey()], string(b))
		}
	}
	return md
}

// Status returns the status the call ended with, OK for a call that
// succeeded. Each detail carries the bytes the server sent under its type's
// URL, so Details resolves it as grpc-go's client does.
func (k Call) Status() *status.Status {
	st := k.p.GetStatus()
	p := &spb.Status{Code: st.GetCode(), Message: st.GetMessage()}
	for _, d := range st.GetDetails() {
		p.Details = append(p.Details, &anypb.Any{TypeUrl: "type.googleapis.com/" + d.GetTypeName(), Value: d.GetWireBytes()})
	}
	return status.FromProto(p)
}

// StubID is the id of the stub that answered the call, or "" when none did.
func (k Call) StubID() string { return k.p.GetMatchedStubId() }

// Proto returns the call as the server reported it, for what the typed
// accessors do not cover: streams, timing, the server's JSON rendering.
func (k Call) Proto() *adminv1.Call { return k.p }
