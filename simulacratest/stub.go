package simulacratest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// StubBuilder accumulates one stub. Start one with Client.Stub and finish it
// with Respond, which sends it. Builder methods take no testing.TB: misuse is
// collected and reported once, by the call that sends.
type StubBuilder struct {
	c        *Client
	method   string
	match    matchSpec
	priority int
	times    int
	header   map[string]string
	trailer  map[string]string
	delay    string
	errs     []string
}

// stubDoc is one stub mapping in the stub file grammar. It is sent as JSON,
// which the server's YAML loader reads unchanged, as the JVM SDK does, so both
// SDKs send the same document for the same stub.
type stubDoc struct {
	Method   string         `json:"method"`
	Match    map[string]any `json:"match,omitempty"`
	Priority int            `json:"priority,omitempty"`
	Times    int            `json:"times,omitempty"`
	Respond  respondDoc     `json:"respond"`
}

type respondDoc struct {
	Literal  bool              `json:"literal"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Trailers map[string]string `json:"trailers,omitempty"`
	Delay    string            `json:"delay,omitempty"`
	Message  map[string]any    `json:"message,omitempty"`
	Status   *statusDoc        `json:"status,omitempty"`
}

type statusDoc struct {
	Code    string      `json:"code"`
	Message string      `json:"message,omitempty"`
	Details []detailDoc `json:"details,omitempty"`
}

type detailDoc struct {
	Type  string         `json:"type"`
	Value map[string]any `json:"value"`
}

// Stub starts a stub for method, given as "/pkg.Svc/M" or "pkg.Svc/M"; the
// generated _FullMethodName constants work as they are.
func (c *Client) Stub(method string) *StubBuilder { return &StubBuilder{c: c, method: method} }

// MatchField adds matchers on a request field. path is dotted for nesting;
// calls on one path merge their operators.
func (b *StubBuilder) MatchField(path string, ms ...Matcher) *StubBuilder {
	b.match.add("MatchField", path, ms)
	return b
}

// MatchMetadata adds matchers on a request metadata key.
func (b *StubBuilder) MatchMetadata(key string, ms ...Matcher) *StubBuilder {
	b.match.add("MatchMetadata", key, ms)
	return b
}

// Expr sets a CEL expression for what the structured matchers cannot say. It
// is sent verbatim, or refused if the stub grammar would alter it.
func (b *StubBuilder) Expr(cel string) *StubBuilder {
	b.match.setExpr(cel)
	return b
}

// Priority sets the stub's priority; among matching stubs the highest answers.
func (b *StubBuilder) Priority(n int) *StubBuilder {
	b.priority = n
	return b
}

// Times limits how many calls the stub answers. Zero, the default, is
// unlimited.
func (b *StubBuilder) Times(n int) *StubBuilder {
	b.times = n
	return b
}

// Header adds a response header.
func (b *StubBuilder) Header(key, value string) *StubBuilder {
	b.header = b.addMetadata("Header", b.header, key, value)
	return b
}

// Trailer adds a response trailer.
func (b *StubBuilder) Trailer(key, value string) *StubBuilder {
	b.trailer = b.addMetadata("Trailer", b.trailer, key, value)
	return b
}

// addMetadata refuses what the document would change without an error. The
// grammar holds one value per key: a repeated key would replace the first
// value, and the server lowercases keys, so two spellings of one key would
// arrive as a single key with two values. encoding/json replaces invalid
// UTF-8 with U+FFFD, which would corrupt a -bin value, and YAML folds a raw
// U+0085 into a space; a value altered either way is refused. Every other
// metadata rule (reserved keys, printable ASCII) is the server's to diagnose.
func (b *StubBuilder) addMetadata(kind string, m map[string]string, key, value string) map[string]string {
	for k := range m {
		if strings.ToLower(k) == strings.ToLower(key) {
			b.errs = append(b.errs, fmt.Sprintf("simulacratest: %s(%q): duplicate key (metadata keys are case-insensitive)", kind, key))
			return m
		}
	}
	if !utf8.ValidString(value) {
		b.errs = append(b.errs, fmt.Sprintf("simulacratest: %s(%q): value is not valid UTF-8; JSON would replace the invalid bytes", kind, key))
		return m
	}
	if !yamlSafeString(value) {
		b.errs = append(b.errs, fmt.Sprintf("simulacratest: %s(%q): value does not survive the stub grammar's JSON round trip unchanged (a raw U+0085, for example); use StubYAML for full control", kind, key))
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	return m
}

// Delay holds the response for d.
func (b *StubBuilder) Delay(d time.Duration) *StubBuilder {
	b.delay = d.String()
	return b
}

// DelayRange holds the response for a random duration from lo to hi
// inclusive.
func (b *StubBuilder) DelayRange(lo, hi time.Duration) *StubBuilder {
	b.delay = lo.String() + ".." + hi.String()
	return b
}

// Respond sends the stub with msg as its response and returns the server's
// envelope for it.
//
// msg must be the method's output type. It is sent as protobuf JSON with
// respond.literal set, so no string in it is read as a template, and it is
// refused before sending if the server would not decode exactly msg from it:
// unknown fields and a negative zero, for example, cannot survive the trip.
// StubYAML remains for what the builder cannot express.
func (b *StubBuilder) Respond(t testing.TB, msg proto.Message) *adminv1.Stub {
	t.Helper()
	if errs := slices.Concat(b.match.errors(), b.errs); len(errs) > 0 {
		t.Fatalf("%s", strings.Join(errs, "\n"))
	}
	if isNil(msg) {
		t.Fatalf("simulacratest: Respond: message is nil; pass &T{} for an empty response")
	}
	// The guard decodes into msg's own type, so it cannot see a message of
	// the wrong type whose fields happen to fit the output type: a
	// GetOrderRequest holding only order_id decodes as a GetOrderResponse
	// without complaint. A method neither side knows skips the check;
	// CreateStub reports it.
	output, known := b.c.ensure(t, b.method)
	if got := msg.ProtoReflect().Descriptor().FullName(); known && got != output {
		full, _, _, _ := normalizeMethod(b.method)
		t.Fatalf("simulacratest: Respond: got %s, %s returns %s", got, full, output)
	}
	respond, err := respondLiteral(msg, b.c.resolver())
	if err != nil {
		t.Fatalf("simulacratest: Respond: %v", err)
	}
	return b.send(t, "Respond", respond)
}

// isNil reports a nil message, typed or not. A typed nil pointer is a non-nil
// interface, and its ProtoReflect cannot be trusted to say so: a nil
// *dynamicpb.Message panics in IsValid, where a generated type's nil pointer
// reports false.
func isNil(msg proto.Message) bool {
	if msg == nil {
		return true
	}
	if rv := reflect.ValueOf(msg); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return true
	}
	return !msg.ProtoReflect().IsValid()
}

// respondLiteral is the response block for msg: its rendering, marked literal
// and checked by the round-trip guard. One resolver serves both, so the
// render and the guard see the same registered types.
func respondLiteral(msg proto.Message, r chainResolver) (respondDoc, error) {
	tree, err := renderMessage(msg, r, "respond.message")
	if err != nil {
		return respondDoc{}, err
	}
	if err := guardRoundTrip(tree, msg, r); err != nil {
		return respondDoc{}, err
	}
	return respondDoc{Literal: true, Message: tree}, nil
}

// document is the stub mapping for respond, with the builder's response
// options filled in. The match block's maps are the matchSpec's own; they are
// only ever marshaled.
func (b *StubBuilder) document(respond respondDoc) stubDoc {
	respond.Metadata, respond.Trailers, respond.Delay = b.header, b.trailer, b.delay
	return stubDoc{Method: b.method, Match: b.match.block(), Priority: b.priority, Times: b.times, Respond: respond}
}

// send creates the stub for respond. op names the calling method for a
// failure before the RPC.
func (b *StubBuilder) send(t testing.TB, op string, respond respondDoc) *adminv1.Stub {
	t.Helper()
	doc, err := json.Marshal(b.document(respond))
	if err != nil {
		t.Fatalf("simulacratest: %s: %v", op, err)
	}
	return b.c.createStub(t, b.method, string(doc))
}
