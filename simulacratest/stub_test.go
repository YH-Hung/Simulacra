package simulacratest_test

import (
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

const (
	corpusPath = "conformance/v1/corpus.proto"
	getOrder   = shopv1.OrderService_GetOrder_FullMethodName
	echo       = "/conformance.v1.CorpusService/Echo"
	valueRoot  = "/conformance.v1.CorpusService/ValueRoot"
)

// guardRefusal is the round-trip guard's refusal for a message of type name.
func guardRefusal(name string) string {
	return "simulacratest: Respond: " + name + " does not survive the stub grammar's JSON round trip unchanged (unknown fields are dropped and negative zero loses its sign, for example); use StubYAML for full control"
}

func TestStubDocumentGolden(t *testing.T) {
	sim := simulacratest.Start(t)
	b := sim.Stub(getOrder).
		MatchField("order_id", simulacratest.Eq("o-123")).
		MatchField("customer.region", simulacratest.In(shopv1.Region_EU, shopv1.Region_UK)).
		MatchMetadata("x-tenant", simulacratest.Eq("acme")).
		Expr(`size(message.items) <= 10`).
		Priority(10).Times(1).
		Header("x-mock", "simulacra").Trailer("x-served-by", "go-sdk").
		DelayRange(50*time.Millisecond, 200*time.Millisecond)

	got, err := simulacratest.StubDocument(b, &shopv1.GetOrderResponse{OrderId: "o-123", Status: shopv1.OrderStatus_ORDER_STATUS_SHIPPED})

	// json.Marshal HTML-escapes "<" as the six characters backslash-u-0-0-3-c;
	// the server's YAML decoder reads them back as "<" (Ruling R1).
	want := `{"method":"/shop.v1.OrderService/GetOrder",` +
		`"match":{"expr":"size(message.items) \u003c= 10","message":{"customer.region":{"in":["EU","UK"]},"order_id":{"eq":"o-123"}},"metadata":{"x-tenant":{"eq":"acme"}}},` +
		`"priority":10,"times":1,` +
		`"respond":{"literal":true,"metadata":{"x-mock":"simulacra"},"trailers":{"x-served-by":"go-sdk"},"delay":"50ms..200ms","message":{"order_id":"o-123","status":"ORDER_STATUS_SHIPPED"}}}`
	if err != nil || got != want {
		t.Fatalf("StubDocument = %s, %v;\nwant %s", got, err, want)
	}
}

func TestHeaderKeysAreCaseInsensitive(t *testing.T) {
	sim := simulacratest.Start(t)
	for _, tc := range []struct {
		name string
		b    *simulacratest.StubBuilder
		want string
	}{
		{"Header", sim.Stub(getOrder).Header("X-A", "1").Header("x-a", "2"),
			`simulacratest: Header("x-a"): duplicate key (metadata keys are case-insensitive)`},
		{"Trailer", sim.Stub(getOrder).Trailer("x-a", "1").Trailer("X-A", "2"),
			`simulacratest: Trailer("X-A"): duplicate key (metadata keys are case-insensitive)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runRecorded(t, func(tb testing.TB) { tc.b.Respond(tb, &shopv1.GetOrderResponse{}) })
			if rec.FatalMsg() != tc.want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), tc.want)
			}
		})
	}
}

func TestHeaderValuesMustBeUTF8(t *testing.T) {
	sim := simulacratest.Start(t)
	rec := runRecorded(t, func(tb testing.TB) {
		sim.Stub(getOrder).Header("x-bin-bin", "\xff").Respond(tb, &shopv1.GetOrderResponse{})
	})
	want := `simulacratest: Header("x-bin-bin"): value is not valid UTF-8; JSON would replace the invalid bytes`
	if rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestRespondRejectsNilMessage(t *testing.T) {
	sim := simulacratest.Start(t)
	const want = "simulacratest: Respond: message is nil; pass &T{} for an empty response"
	for name, msg := range map[string]proto.Message{"nil interface": nil, "typed nil": (*shopv1.GetOrderResponse)(nil)} {
		t.Run(name, func(t *testing.T) {
			rec := runRecorded(t, func(tb testing.TB) { sim.Stub(getOrder).Respond(tb, msg) })
			if rec.FatalMsg() != want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
			}
		})
	}
}

func TestRespondRefusesUnknownFields(t *testing.T) {
	sim := simulacratest.Start(t)
	msg := &shopv1.GetOrderResponse{OrderId: "o"}
	msg.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))

	rec := runRecorded(t, func(tb testing.TB) { sim.Stub(getOrder).Respond(tb, msg) })

	if want := guardRefusal("shop.v1.GetOrderResponse"); rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

// A Struct number's sign of zero is invisible to proto.Equal, so only the
// guard's byte comparison can refuse it (F6).
func TestRespondRefusesNegativeZero(t *testing.T) {
	sim := simulacratest.Start(t)
	files := conformanceFiles(t)
	sim.RegisterFiles(t, files[corpusPath])
	attrs, err := structpb.NewStruct(map[string]any{"z": math.Copysign(0, -1)})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	if !math.Signbit(attrs.GetFields()["z"].GetNumberValue()) {
		t.Fatalf("the fixture lost its negative zero before the test ran")
	}
	msg := newMessage(files[corpusPath], "Everything")
	msg.Set(msg.Descriptor().Fields().ByName("attrs"), protoreflect.ValueOfMessage(attrs.ProtoReflect()))

	rec := runRecorded(t, func(tb testing.TB) { sim.Stub(echo).Respond(tb, msg) })

	if want := guardRefusal("conformance.v1.Everything"); rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestRespondRefusesNonObjectJSON(t *testing.T) {
	sim := simulacratest.Start(t)
	sim.RegisterFiles(t, conformanceFiles(t)[corpusPath])
	list, err := structpb.NewList([]any{1})
	if err != nil {
		t.Fatalf("NewList: %v", err)
	}
	for _, tc := range []struct {
		kind string
		v    *structpb.Value
	}{
		{"string", structpb.NewStringValue("s")},
		{"number", structpb.NewNumberValue(1)},
		{"bool", structpb.NewBoolValue(true)},
		{"array", structpb.NewListValue(list)},
		{"null", structpb.NewNullValue()},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			rec := runRecorded(t, func(tb testing.TB) { sim.Stub(valueRoot).Respond(tb, tc.v) })
			want := "simulacratest: Respond: google.protobuf.Value renders as a JSON " + tc.kind + ", not an object; respond.message must be a mapping, so this value cannot be expressed in the stub grammar"
			if rec.FatalMsg() != want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
			}
		})
	}

	obj, err := structpb.NewStruct(map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	sim.Stub(valueRoot).Respond(t, structpb.NewStructValue(obj))
}

func TestRespondChecksTheTypeAgainstTheRPC(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []simulacratest.Option
	}{
		{"linked by the test binary", nil},
		{"listed by the server", []simulacratest.Option{simulacratest.WithProtoDir("../testdata/protos")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim := simulacratest.Start(t, tc.opts...)
			// GetOrderRequest and GetOrderResponse both declare string
			// order_id = 1, so only the type check can refuse this one. The
			// method is given without its leading slash; the message names
			// the normalized form.
			rec := runRecorded(t, func(tb testing.TB) {
				sim.Stub("shop.v1.OrderService/GetOrder").Respond(tb, &shopv1.GetOrderRequest{OrderId: "o-1"})
			})
			want := "simulacratest: Respond: got shop.v1.GetOrderRequest, /shop.v1.OrderService/GetOrder returns shop.v1.GetOrderResponse"
			if rec.FatalMsg() != want {
				t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
			}
		})
	}
}

func TestRespondRefusesAMessageFromADifferentDescriptorInstance(t *testing.T) {
	sim := simulacratest.Start(t)
	a, b := conformanceFiles(t), conformanceFiles(t)
	sim.RegisterFiles(t, a[legacyPath])
	msg := newMessage(b[legacyPath], "LegacyReply")
	msg.Set(extension(b[legacyPath], "ext_tag"), protoreflect.ValueOfString("t"))

	rec := runRecorded(t, func(tb testing.TB) { sim.Stub("/conformance.v1.LegacyService/Fetch").Respond(tb, msg) })

	if want := "simulacratest: Respond: " + instanceMismatch(legacyPath); rec.FatalMsg() != want {
		t.Fatalf("FatalMsg() = %q; want %q", rec.FatalMsg(), want)
	}
}
