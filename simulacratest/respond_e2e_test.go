package simulacratest_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

// assertExact fails unless got is want by proto.Equal and by deterministic
// bytes: proto.Equal compares floats with ==, so it alone cannot see a lost
// sign of zero.
func assertExact(t *testing.T, got, want proto.Message) {
	t.Helper()
	det := proto.MarshalOptions{Deterministic: true}
	gotBytes, err := det.Marshal(got)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	wantBytes, err := det.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if !proto.Equal(got, want) || !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("reply = {%v}; want {%v}", prototext.Format(got), prototext.Format(want))
	}
}

func TestRespondDeliversGeneratedMessagesExactly(t *testing.T) {
	sim := simulacratest.Start(t)
	orders := shopv1.NewOrderServiceClient(sim.Dial(t))
	inner := func(id string) *anypb.Any {
		a, err := anypb.New(&shopv1.AnyInner{Id: id})
		if err != nil {
			t.Fatalf("anypb.New: %v", err)
		}
		return a
	}
	for _, tc := range []struct {
		name string
		msg  *shopv1.GetOrderResponse
	}{
		{"template text stays literal", &shopv1.GetOrderResponse{OrderId: "o-1", Note: "Hello {{ name }}"}},
		{"enum", &shopv1.GetOrderResponse{Status: shopv1.OrderStatus_ORDER_STATUS_SHIPPED}},
		{"timestamp with nanos", &shopv1.GetOrderResponse{Eta: &timestamppb.Timestamp{Seconds: 1700000000, Nanos: 123456789}}},
		{"any", &shopv1.GetOrderResponse{Extra: inner("i")}},
		{"template text inside an any", &shopv1.GetOrderResponse{Extra: inner("{{ x }}")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Times(1) spends each stub on its one call, so the next case's
			// stub answers whichever way the server breaks ties.
			sim.Stub(getOrder).Times(1).Respond(t, tc.msg)
			got, err := orders.GetOrder(t.Context(), &shopv1.GetOrderRequest{})
			if err != nil {
				t.Fatalf("GetOrder: %v", err)
			}
			assertExact(t, got, tc.msg)
		})
	}
}

func TestRespondDeliversDynamicHardCasesExactly(t *testing.T) {
	sim := simulacratest.Start(t)
	files := conformanceFiles(t)
	sim.RegisterFiles(t, files[corpusPath], files[legacyPath], files["conformance/v1/editionsmsg.proto"])
	// The fixtures are written in the text format, a codec the SDK does not
	// use, through the same instances the server and the Client were given.
	reg := new(protoregistry.Files)
	for _, fd := range files {
		if err := reg.RegisterFile(fd); err != nil {
			t.Fatalf("registering %s: %v", fd.Path(), err)
		}
	}
	types := dynamicpb.NewTypes(reg)
	conn := sim.Dial(t)

	for _, tc := range []struct {
		name    string
		method  protoreflect.FullName
		request string // text of the request, which proto2 required fields can need
		reply   string
	}{
		{"everything", "conformance.v1.CorpusService.Echo", "", `
			big: -9223372036854775808
			ubig: 18446744073709551615
			blob: "\x00\xff"
			opt_note: ""
			number: 0
			items { key: "a" value { id: "x" } }
			items { key: "b" value {} }
			when { seconds: 1700000000 nanos: 123456789 }
			span { seconds: -3 nanos: -500000000 }
			wrapped { value: "w" }
			mask { paths: "opt_note" paths: "items.id" }
			attrs { fields { key: "big" value { number_value: 1e21 } } }
			payload { [type.googleapis.com/conformance.v1.Inner] { id: "i" } }`},
		{"proto2 extension", "conformance.v1.LegacyService.Fetch", `name: "n"`, `note: "n" [conformance.v1.ext_tag]: "t"`},
		{"proto2 group", "conformance.v1.LegacyService.Make", "", `name: "n" Detail { code: "c" }`},
		{"edition 2023", "conformance.v1e.EditionService.Get", "", `name: "e"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := reg.FindDescriptorByName(tc.method)
			if err != nil {
				t.Fatalf("finding %s: %v", tc.method, err)
			}
			md := d.(protoreflect.MethodDescriptor)
			path := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
			parse := prototext.UnmarshalOptions{Resolver: types}
			req, want := dynamicpb.NewMessage(md.Input()), dynamicpb.NewMessage(md.Output())
			if err := parse.Unmarshal([]byte(tc.request), req); err != nil {
				t.Fatalf("parsing request: %v", err)
			}
			if err := parse.Unmarshal([]byte(tc.reply), want); err != nil {
				t.Fatalf("parsing reply: %v", err)
			}

			sim.Stub(path).Respond(t, want)
			reply := dynamicpb.NewMessage(md.Output())
			if err := conn.Invoke(t.Context(), path, req, reply); err != nil {
				t.Fatalf("Invoke %s: %v", path, err)
			}
			// gRPC's codec decodes through GlobalTypes, which does not hold
			// ext_tag, so it lands in the unknown fields. Decode the same
			// bytes again through the instance set to compare it as the
			// extension it is.
			wire, err := proto.Marshal(reply)
			if err != nil {
				t.Fatalf("marshal reply: %v", err)
			}
			got := dynamicpb.NewMessage(md.Output())
			if err := (proto.UnmarshalOptions{Resolver: types}).Unmarshal(wire, got); err != nil {
				t.Fatalf("decoding reply: %v", err)
			}
			assertExact(t, got, want)
		})
	}
}

func TestRespondValueRootAcceptsAnObjectValue(t *testing.T) {
	sim := simulacratest.Start(t)
	files := conformanceFiles(t)
	sim.RegisterFiles(t, files[corpusPath])
	obj, err := structpb.NewStruct(map[string]any{"s": "v", "n": 1.5, "l": []any{true, nil}, "o": map[string]any{}})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	want := structpb.NewStructValue(obj)

	sim.Stub(valueRoot).Respond(t, want)
	got := new(structpb.Value)
	if err := sim.Dial(t).Invoke(t.Context(), valueRoot, newMessage(files[corpusPath], "Everything"), got); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	assertExact(t, got, want)
}
