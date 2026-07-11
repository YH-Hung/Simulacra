package journal

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yinghanhung/simulacra/internal/match"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func intPtr(n int) *int { return &n }

func TestTimesValidate(t *testing.T) {
	tests := []struct {
		name    string
		times   Times
		wantErr string
	}{
		{name: "exactly", times: Times{Exactly: intPtr(2)}},
		{name: "at least", times: Times{AtLeast: intPtr(2)}},
		{name: "at most", times: Times{AtMost: intPtr(2)}},
		{name: "range", times: Times{AtLeast: intPtr(1), AtMost: intPtr(3)}},
		{name: "never", times: Times{Never: true}},
		{name: "missing", times: Times{}, wantErr: "one times assertion is required"},
		{name: "never with exactly", times: Times{Never: true, Exactly: intPtr(0)}, wantErr: "never cannot be combined"},
		{name: "exactly with range", times: Times{Exactly: intPtr(1), AtLeast: intPtr(1)}, wantErr: "exactly cannot be combined"},
		{name: "negative exactly", times: Times{Exactly: intPtr(-1)}, wantErr: "exactly must not be negative"},
		{name: "negative at least", times: Times{AtLeast: intPtr(-1)}, wantErr: "at_least must not be negative"},
		{name: "negative at most", times: Times{AtMost: intPtr(-1)}, wantErr: "at_most must not be negative"},
		{name: "reversed range", times: Times{AtLeast: intPtr(3), AtMost: intPtr(2)}, wantErr: "at_least must not exceed at_most"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.times.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestTimesOKAndString(t *testing.T) {
	tests := []struct {
		name       string
		times      Times
		count      int
		wantOK     bool
		wantString string
	}{
		{name: "exactly passes", times: Times{Exactly: intPtr(2)}, count: 2, wantOK: true, wantString: "exactly 2"},
		{name: "exactly fails", times: Times{Exactly: intPtr(2)}, count: 1, wantString: "exactly 2"},
		{name: "at least", times: Times{AtLeast: intPtr(2)}, count: 3, wantOK: true, wantString: "at least 2"},
		{name: "at most", times: Times{AtMost: intPtr(2)}, count: 3, wantString: "at most 2"},
		{name: "range", times: Times{AtLeast: intPtr(1), AtMost: intPtr(3)}, count: 2, wantOK: true, wantString: "at least 1 and at most 3"},
		{name: "never", times: Times{Never: true}, count: 0, wantOK: true, wantString: "never"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.times.ok(tt.count); got != tt.wantOK {
				t.Errorf("ok(%d) = %v, want %v", tt.count, got, tt.wantOK)
			}
			if got := tt.times.String(); got != tt.wantString {
				t.Errorf("String = %q, want %q", got, tt.wantString)
			}
		})
	}
}

func TestVerifyCountsMatchingCalls(t *testing.T) {
	j, orderOne, orderNine := verificationFixture(t)
	tests := []struct {
		name    string
		matcher *match.Compiled
		times   Times
		pass    bool
		matched int
		want    string
	}{
		{name: "exactly", matcher: orderOne, times: Times{Exactly: intPtr(2)}, pass: true, matched: 2, want: "exactly 2"},
		{name: "at least", matcher: orderOne, times: Times{AtLeast: intPtr(2)}, pass: true, matched: 2, want: "at least 2"},
		{name: "at most", matcher: orderOne, times: Times{AtMost: intPtr(1)}, matched: 2, want: "at most 1"},
		{name: "range", matcher: orderOne, times: Times{AtLeast: intPtr(1), AtMost: intPtr(2)}, pass: true, matched: 2, want: "at least 1 and at most 2"},
		{name: "never pass", matcher: orderNine, times: Times{Never: true}, pass: true, matched: 0, want: "never"},
		{name: "never fail", matcher: orderOne, times: Times{Never: true}, matched: 2, want: "never"},
		{name: "nil matcher counts all", matcher: nil, times: Times{Exactly: intPtr(3)}, pass: true, matched: 3, want: "exactly 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := Verify(j, "shop.v1.OrderService/GetOrder", tt.matcher, tt.times)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if report.Pass != tt.pass || report.Matched != tt.matched || report.Considered != 3 || report.Want != tt.want {
				t.Errorf("report = %+v", report)
			}
			if report.Pass && report.Misses != nil {
				t.Errorf("passing report Misses = %#v, want nil", report.Misses)
			}
		})
	}
}

func TestVerifyFailureExplainsNonMatchingCall(t *testing.T) {
	j, orderOne, _ := verificationFixture(t)
	report, err := Verify(j, "/shop.v1.OrderService/GetOrder", orderOne, Times{Exactly: intPtr(3)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantMisses := []Miss{{Seq: 3, Reasons: []string{`message order_id: expected to equal "o-1"; actual "o-2"`}}}
	if report.Pass || report.Want != "exactly 3" || !reflect.DeepEqual(report.Misses, wantMisses) {
		t.Fatalf("report = %#v, want failure with %#v", report, wantMisses)
	}
}

func TestVerifyRejectsInvalidTimes(t *testing.T) {
	j, _, _ := verificationFixture(t)
	if _, err := Verify(j, "/shop.v1.OrderService/GetOrder", nil, Times{}); err == nil {
		t.Fatal("Verify with empty Times succeeded, want error")
	}
}

func TestCallInputEmptyAndNilRequestsArePanicSafe(t *testing.T) {
	method := "shop.v1.OrderService/UploadOrders"
	in := callInput(&Call{Method: method})
	if in.Method != method || in.Message != nil || in.Messages == nil || len(in.Messages) != 0 {
		t.Fatalf("callInput(empty) = %+v, want normalized method and non-nil empty messages", in)
	}

	desc, files := orderDescriptor(t)
	compiled, err := match.NewCompiler(files).Compile(desc, &match.Block{Expr: "size(messages) == 0"}, match.ClientStream)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	j := New(2)
	j.Record(&Call{Method: method})
	report, err := Verify(j, method, compiled, Times{Exactly: intPtr(1)})
	if err != nil || !report.Pass {
		t.Fatalf("Verify empty stream = %+v, %v", report, err)
	}

	nilInput := callInput(&Call{Method: method, Requests: []*dynamicpb.Message{nil}})
	if nilInput.Message != nil || nilInput.Messages == nil || len(nilInput.Messages) != 0 {
		t.Fatalf("callInput(nil request) = %+v, want nil skipped safely", nilInput)
	}
	j.Record(&Call{Method: method, Requests: []*dynamicpb.Message{nil}})
	report, err = Verify(j, method, compiled, Times{Exactly: intPtr(2)})
	if err != nil || !report.Pass {
		t.Fatalf("Verify nil request = %+v, %v; want safe empty-stream match", report, err)
	}
}

func TestCallInputMatchesRuntimeMethodAndMetadataNormalization(t *testing.T) {
	desc, files := orderDescriptor(t)
	compiled, err := match.NewCompiler(files).Compile(desc, &match.Block{
		Metadata: map[string]match.Rules{"x-tenant": {"eq": "acme"}},
		Expr:     `method == "shop.v1.OrderService/GetOrder"`,
	}, match.Unary)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	message := dynamicpb.NewMessage(desc)
	in := callInput(&Call{
		Method:   "/shop.v1.OrderService/GetOrder",
		Metadata: metadata.MD{"X-Tenant": {"acme"}},
		Requests: []*dynamicpb.Message{message},
	})
	if !compiled.Eval(in) {
		t.Fatalf("normalized input did not match: %+v; reasons: %v", in, compiled.Explain(in))
	}
}

func verificationFixture(t *testing.T) (*Journal, *match.Compiled, *match.Compiled) {
	t.Helper()
	desc, files := orderDescriptor(t)
	compiler := match.NewCompiler(files)
	compile := func(orderID string) *match.Compiled {
		t.Helper()
		compiled, err := compiler.Compile(desc, &match.Block{Message: map[string]match.Rules{"order_id": {"eq": orderID}}}, match.Unary)
		if err != nil {
			t.Fatalf("Compile order_id %q: %v", orderID, err)
		}
		return compiled
	}
	j := New(10)
	for _, orderID := range []string{"o-1", "o-1", "o-2"} {
		message := dynamicpb.NewMessage(desc)
		message.Set(desc.Fields().ByName("order_id"), protoreflect.ValueOfString(orderID))
		j.Record(&Call{Method: "shop.v1.OrderService/GetOrder", Requests: []*dynamicpb.Message{message}})
	}
	j.Record(&Call{Method: "shop.v1.OrderService/Other", Requests: []*dynamicpb.Message{dynamicpb.NewMessage(desc)}})
	return j, compile("o-1"), compile("o-9")
}

func orderDescriptor(t *testing.T) (protoreflect.MessageDescriptor, *protoregistry.Files) {
	t.Helper()
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    strPtr("verify_order.proto"),
		Package: strPtr("shop.v1"),
		Syntax:  strPtr("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: strPtr("GetOrderRequest"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: strPtr("order_id"), Number: int32Ptr(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	files := new(protoregistry.Files)
	if err := files.RegisterFile(file); err != nil {
		t.Fatalf("register descriptor: %v", err)
	}
	return file.Messages().ByName("GetOrderRequest"), files
}

func strPtr(s string) *string { return &s }
func int32Ptr(n int32) *int32 { return &n }
