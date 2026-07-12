package match

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/yinghanhung/simulacra/internal/schema"
)

func requestDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	m, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	return m.Input()
}

func msg(t *testing.T, desc protoreflect.MessageDescriptor, jsonBody string) protoreflect.Message {
	t.Helper()
	m := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(jsonBody), m); err != nil {
		t.Fatalf("building test message from %s: %v", jsonBody, err)
	}
	return m
}

func TestEval(t *testing.T) {
	desc := requestDesc(t)
	req := `{
		"order_id": "o-123",
		"customer": {"id": "c-9", "region": "EU"},
		"items": [{"sku": "A1", "qty": "3"}, {"sku": "B2", "qty": "1"}],
		"tags": ["prio", "gift"]
	}`

	cases := []struct {
		name  string
		block *Block
		body  string
		md    metadata.MD
		want  bool
	}{
		{"nil block matches all", nil, req, nil, true},
		{"eq string", &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}}}, req, nil, true},
		{"eq string miss", &Block{Message: map[string]Rules{"order_id": {"eq": "other"}}}, req, nil, false},
		{"ne", &Block{Message: map[string]Rules{"order_id": {"ne": "other"}}}, req, nil, true},
		{"nested path", &Block{Message: map[string]Rules{"customer.id": {"eq": "c-9"}}}, req, nil, true},
		{"enum by name", &Block{Message: map[string]Rules{"customer.region": {"eq": "EU"}}}, req, nil, true},
		{"enum in list", &Block{Message: map[string]Rules{"customer.region": {"in": []any{"EU", "UK"}}}}, req, nil, true},
		{"enum in list miss", &Block{Message: map[string]Rules{"customer.region": {"in": []any{"UK", "US"}}}}, req, nil, false},
		{"regex", &Block{Message: map[string]Rules{"order_id": {"matches": "^o-\\d+$"}}}, req, nil, true},
		{"present true", &Block{Message: map[string]Rules{"customer": {"present": true}}}, req, nil, true},
		{"present false on unset", &Block{Message: map[string]Rules{"customer": {"present": true}}}, `{"order_id":"x"}`, nil, false},
		{"present on repeated means non-empty", &Block{Message: map[string]Rules{"items": {"present": true}}}, `{"order_id":"x"}`, nil, false},
		{"contains on repeated string", &Block{Message: map[string]Rules{"tags": {"contains": "gift"}}}, req, nil, true},
		{"contains miss", &Block{Message: map[string]Rules{"tags": {"contains": "bulk"}}}, req, nil, false},
		{"unset leaf reads as default", &Block{Message: map[string]Rules{"customer.id": {"eq": ""}}}, `{"order_id":"x"}`, nil, true},
		{"two rules AND", &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}, "customer.id": {"eq": "nope"}}}, req, nil, false},
		{"metadata eq", &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}, req, metadata.Pairs("x-tenant", "acme"), true},
		{"metadata eq missing key", &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}, req, nil, false},
		{"metadata present", &Block{Metadata: map[string]Rules{"authorization": {"present": true}}}, req, metadata.Pairs("authorization", "Bearer x"), true},
		{"metadata any-value semantics", &Block{Metadata: map[string]Rules{"x-tag": {"eq": "b"}}}, req, metadata.Pairs("x-tag", "a", "x-tag", "b"), true},
		{"metadata regex", &Block{Metadata: map[string]Rules{"x-tenant": {"matches": "^ac"}}}, req, metadata.Pairs("x-tenant", "acme"), true},
		// Numeric correctness: yaml.v3 hands big integers to us as uint64,
		// and float32 fields must compare after float32 normalization.
		{"uint64 full range", &Block{Message: map[string]Rules{"big": {"eq": uint64(18446744073709551615)}}}, `{"big":"18446744073709551615"}`, nil, true},
		{"uint64 full range miss", &Block{Message: map[string]Rules{"big": {"eq": uint64(18446744073709551615)}}}, `{"big":"1"}`, nil, false},
		{"uint64 small literal as int", &Block{Message: map[string]Rules{"big": {"eq": 7}}}, `{"big":"7"}`, nil, true},
		{"float32 literal normalized", &Block{Message: map[string]Rules{"ratio": {"eq": 0.1}}}, `{"ratio":0.1}`, nil, true},
		{"float32 int literal", &Block{Message: map[string]Rules{"ratio": {"eq": 2}}}, `{"ratio":2}`, nil, true},
		{"double unaffected", &Block{Message: map[string]Rules{"precise": {"eq": 0.1}}}, `{"precise":0.1}`, nil, true},
		{"int32 in range", &Block{Message: map[string]Rules{"small": {"eq": -42}}}, `{"small":-42}`, nil, true},
		{"enum by number", &Block{Message: map[string]Rules{"customer.region": {"eq": 1}}}, req, nil, true},
		// Proto3 enums are open: an in-range number with no declared name is
		// legal on the wire and must be matchable.
		{"enum by undeclared number", &Block{Message: map[string]Rules{"customer.region": {"eq": 42}}}, `{"customer":{"region":42}}`, nil, true},
		// Explicit infinity is a legitimate float value a client can send.
		{"explicit inf on float32", &Block{Message: map[string]Rules{"ratio": {"eq": math.Inf(1)}}}, `{"ratio":"Infinity"}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCompiler(nil).Compile(desc, tc.block, Unary)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := c.Eval(Input{Message: msg(t, desc, tc.body), Metadata: tc.md}); got != tc.want {
				t.Errorf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	desc := requestDesc(t)
	cases := []struct {
		name  string
		block *Block
	}{
		{"unknown field", &Block{Message: map[string]Rules{"no_such_field": {"eq": "x"}}}},
		{"unknown nested field", &Block{Message: map[string]Rules{"customer.nope": {"eq": "x"}}}},
		{"descend through scalar", &Block{Message: map[string]Rules{"order_id.sub": {"eq": "x"}}}},
		{"descend through repeated", &Block{Message: map[string]Rules{"items.sku": {"eq": "x"}}}},
		{"unknown op", &Block{Message: map[string]Rules{"order_id": {"equalz": "x"}}}},
		{"regex on non-string", &Block{Message: map[string]Rules{"customer.region": {"matches": "E.*"}}}},
		{"bad regex", &Block{Message: map[string]Rules{"order_id": {"matches": "("}}}},
		{"contains on singular", &Block{Message: map[string]Rules{"order_id": {"contains": "x"}}}},
		{"eq on repeated", &Block{Message: map[string]Rules{"tags": {"eq": "x"}}}},
		{"unknown enum name", &Block{Message: map[string]Rules{"customer.region": {"eq": "MARS"}}}},
		{"type mismatch", &Block{Message: map[string]Rules{"order_id": {"eq": 42}}}},
		{"metadata unknown op", &Block{Metadata: map[string]Rules{"k": {"has": true}}}},
		{"int32 literal out of range", &Block{Message: map[string]Rules{"small": {"eq": int64(3000000000)}}}},
		{"int32 literal below range", &Block{Message: map[string]Rules{"small": {"eq": int64(-3000000000)}}}},
		{"negative literal on uint64", &Block{Message: map[string]Rules{"big": {"eq": -1}}}},
		{"huge uint64 literal on signed field", &Block{Message: map[string]Rules{"small": {"eq": uint64(9999999999999999999)}}}},
		// yaml.v3 hands 4294967296 over as int (fits in 64-bit int), which
		// previously wrapped through the int32-backed EnumNumber to 0.
		{"enum number wraps int32", &Block{Message: map[string]Rules{"customer.region": {"eq": 4294967296}}}},
		{"enum number negative overflow", &Block{Message: map[string]Rules{"customer.region": {"eq": -4294967296}}}},
		{"enum number huge uint64", &Block{Message: map[string]Rules{"customer.region": {"eq": uint64(9999999999999999999)}}}},
		{"finite float literal overflows float32", &Block{Message: map[string]Rules{"ratio": {"eq": 1e100}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCompiler(nil).Compile(desc, tc.block, Unary); err == nil {
				t.Error("expected compile error, got nil")
			}
		})
	}
}

func TestShapeValidation(t *testing.T) {
	desc := requestDesc(t)
	withMsg := &Block{Message: map[string]Rules{"order_id": {"eq": "x"}}}
	for _, shape := range []Shape{Unary, ServerStream, BidiRule} {
		if _, err := NewCompiler(nil).Compile(desc, withMsg, shape); err != nil {
			t.Errorf("Compile(%v) with message rules: %v, want ok", shape, err)
		}
	}
	for _, shape := range []Shape{ClientStream, Bidi} {
		if _, err := NewCompiler(nil).Compile(desc, withMsg, shape); err == nil {
			t.Errorf("Compile(%v) with message rules: want error", shape)
		}
	}
	withMD := &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}
	for _, shape := range []Shape{Unary, ServerStream, ClientStream, Bidi, BidiRule} {
		if _, err := NewCompiler(nil).Compile(desc, withMD, shape); err != nil {
			t.Errorf("Compile(%v) with metadata rules: %v, want ok", shape, err)
		}
	}
}

func TestEvalNilMessageFailsMessageRules(t *testing.T) {
	desc := requestDesc(t)
	c, err := NewCompiler(nil).Compile(desc, &Block{Message: map[string]Rules{"order_id": {"eq": "x"}}}, Unary)
	if err != nil {
		t.Fatal(err)
	}
	if c.Eval(Input{}) {
		t.Error("message rule matched an input with no message")
	}
}

func testFiles(t *testing.T) *protoregistry.Files {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	return reg.Files()
}

func TestExprMatching(t *testing.T) {
	desc := requestDesc(t)
	mc := NewCompiler(testFiles(t))
	req := `{
		"order_id": "o-123",
		"customer": {"id": "c-9", "region": "EU"},
		"items": [{"sku": "A1", "qty": "3"}, {"sku": "B2", "qty": "1"}],
		"tags": ["prio", "gift"]
	}`
	cases := []struct {
		name string
		expr string
		md   metadata.MD
		want bool
	}{
		{"compound predicate", `message.items.exists(i, i.sku == "A1") && size(message.items) <= 10`, nil, true},
		{"compound predicate miss", `message.items.exists(i, i.sku == "ZZ")`, nil, false},
		{"int64 arithmetic", `message.items[0].qty * 2 == 6`, nil, true},
		{"enum comparison", `message.customer.region == shop.v1.Region.EU`, nil, true},
		{"metadata multi-value", `'acme' in metadata['x-tenant']`, metadata.Pairs("x-tenant", "other", "x-tenant", "acme"), true},
		{"metadata guard", `'x-tenant' in metadata && 'acme' in metadata['x-tenant']`, nil, false},
		{"method variable", `method == "shop.v1.OrderService/GetOrder"`, nil, true},
		{"eval error is a non-match", `metadata['absent-key'][0] == "x"`, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := mc.Compile(desc, &Block{Expr: tc.expr}, Unary)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			in := Input{Method: "shop.v1.OrderService/GetOrder", Metadata: tc.md, Message: msg(t, desc, req)}
			if got := c.Eval(in); got != tc.want {
				t.Errorf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExprRegisteredDynamicAny(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	request := dynamicpb.NewMessage(method.Input())
	if err := (protojson.UnmarshalOptions{Resolver: reg.Types()}).Unmarshal([]byte(`{
  "payload": {"@type":"type.googleapis.com/shop.v1.AnyInner", "id":"in-1"}
}`), request); err != nil {
		t.Fatalf("build registered Any request: %v", err)
	}
	compiled, err := NewCompiler(reg.Files()).Compile(method.Input(), &Block{
		Expr: `message.payload.id == "in-1"`,
	}, Unary)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !compiled.Eval(Input{Message: request.ProtoReflect()}) {
		t.Fatal("registered dynamic Any expression did not match")
	}
}

func TestExprProcessGlobalAny(t *testing.T) {
	reg, method := anyExprRegistry(t)
	envelope, err := anypb.New(&errdetails.ErrorInfo{Reason: "global"})
	if err != nil {
		t.Fatalf("pack ErrorInfo: %v", err)
	}
	compiled := compileAnyExpr(t, reg, method.Input(), `message.payload.reason == "global"`)
	if !compiled.Eval(Input{Message: requestWithAny(t, method.Input(), envelope)}) {
		t.Fatal("process-global registered Any expression did not match")
	}
}

func TestExprNestedProcessGlobalAny(t *testing.T) {
	reg, method := anyExprRegistry(t)
	detail, err := anypb.New(&errdetails.ErrorInfo{Reason: "nested-global"})
	if err != nil {
		t.Fatalf("pack ErrorInfo: %v", err)
	}
	envelope, err := anypb.New(&statuspb.Status{Details: []*anypb.Any{detail}})
	if err != nil {
		t.Fatalf("pack Status: %v", err)
	}
	compiled := compileAnyExpr(t, reg, method.Input(), `message.payload.details[0].reason == "nested-global"`)
	in := Input{Message: requestWithAny(t, method.Input(), envelope)}
	if !compiled.Eval(in) {
		t.Fatalf("nested process-global Any expression did not match: %v", compiled.Explain(in))
	}
}

func TestExprSchemaAnyWinsOverProcessGlobal(t *testing.T) {
	reg, method := anyExprRegistry(t)
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("schema_collision.proto"),
		Package: proto.String("google.rpc"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("ErrorInfo"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("schema_value"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("build collision descriptor: %v", err)
	}
	if err := reg.AddFile(file); err != nil {
		t.Fatalf("register collision descriptor: %v", err)
	}
	desc, err := reg.LookupMessage("google.rpc.ErrorInfo")
	if err != nil {
		t.Fatalf("LookupMessage schema ErrorInfo: %v", err)
	}
	schemaMessage := dynamicpb.NewMessage(desc)
	schemaMessage.Set(desc.Fields().ByName("schema_value"), protoreflect.ValueOfString("schema"))
	envelope := &anypb.Any{
		TypeUrl: "type.googleapis.com/google.rpc.ErrorInfo",
		Value:   mustMarshal(t, schemaMessage),
	}
	compiled := compileAnyExpr(t, reg, method.Input(), `message.payload.schema_value == "schema"`)
	if !compiled.Eval(Input{Message: requestWithAny(t, method.Input(), envelope)}) {
		t.Fatal("schema-local Any did not win over process-global type with the same name")
	}
	global, err := protoregistry.GlobalTypes.FindMessageByName("google.rpc.ErrorInfo")
	if err != nil {
		t.Fatalf("find process-global ErrorInfo: %v", err)
	}
	if global.Descriptor().Fields().ByName("schema_value") != nil {
		t.Fatal("schema-local collision mutated the process-global type registry")
	}
}

func TestExprSchemaWrongKindDoesNotFallBackToProcessGlobalAny(t *testing.T) {
	reg, method := anyExprRegistry(t)
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("schema_wrong_kind.proto"),
		Package: proto.String("google.rpc"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("ErrorInfo"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{
				Name:   proto.String("ERROR_INFO_UNSPECIFIED"),
				Number: proto.Int32(0),
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("build wrong-kind collision descriptor: %v", err)
	}
	if err := reg.AddFile(file); err != nil {
		t.Fatalf("register wrong-kind collision descriptor: %v", err)
	}
	envelope, err := anypb.New(&errdetails.ErrorInfo{Reason: "global"})
	if err != nil {
		t.Fatalf("pack process-global ErrorInfo: %v", err)
	}
	compiled := compileAnyExpr(t, reg, method.Input(), `message.payload.reason == "global"`)
	in := Input{Message: requestWithAny(t, method.Input(), envelope)}
	if compiled.Eval(in) {
		t.Fatal("schema-local enum collision silently fell back to process-global message")
	}
	diagnostic := strings.Join(compiled.Explain(in), "\n")
	if !strings.Contains(diagnostic, "wrong type") || !strings.Contains(diagnostic, "want message") {
		t.Fatalf("wrong-kind Any diagnostic = %q, want schema type error", diagnostic)
	}
}

func TestRegistryFirstTypesFallbackOnlyOnNotFound(t *testing.T) {
	files := new(protoregistry.Files)
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("resolver_wrong_kinds.proto"),
		Package: proto.String("google.rpc"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("ErrorInfo"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{
				Name:   proto.String("ERROR_INFO_UNSPECIFIED"),
				Number: proto.Int32(0),
			}},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("DebugInfo")}},
	}, nil)
	if err != nil {
		t.Fatalf("build resolver descriptors: %v", err)
	}
	if err := files.RegisterFile(file); err != nil {
		t.Fatalf("register resolver descriptors: %v", err)
	}
	resolver := &registryFirstTypes{dynamic: dynamicpb.NewTypes(files)}
	if _, err := resolver.FindMessageByName("google.rpc.ErrorInfo"); err == nil || errors.Is(err, protoregistry.NotFound) {
		t.Fatalf("FindMessageByName wrong-kind error = %v, want non-NotFound error", err)
	}
	if _, err := resolver.FindMessageByURL("type.googleapis.com/google.rpc.ErrorInfo"); err == nil || errors.Is(err, protoregistry.NotFound) {
		t.Fatalf("FindMessageByURL wrong-kind error = %v, want non-NotFound error", err)
	}
	if _, err := resolver.FindExtensionByName("google.rpc.DebugInfo"); err == nil || errors.Is(err, protoregistry.NotFound) {
		t.Fatalf("FindExtensionByName wrong-kind error = %v, want non-NotFound error", err)
	}
	if _, err := resolver.FindMessageByName("google.rpc.DebugInfo"); err != nil {
		t.Fatalf("FindMessageByName schema message: %v", err)
	}
	if _, err := resolver.FindMessageByName("google.rpc.Status"); err != nil {
		t.Fatalf("FindMessageByName global fallback: %v", err)
	}
}

func TestExprDynamicProto2Extension(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../conformance/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("conformance.v1.LegacyService/Make")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	extensionType, err := reg.Types().FindExtensionByName("conformance.v1.ext_tag")
	if err != nil {
		t.Fatalf("FindExtensionByName: %v", err)
	}
	extension := extensionType.TypeDescriptor()

	t.Run("set", func(t *testing.T) {
		message := dynamicpb.NewMessage(method.Input())
		message.Set(extension, protoreflect.ValueOfString("tagged"))
		compiled := compileAnyExpr(t, reg, method.Input(), "message.`conformance.v1.ext_tag` == 'tagged' && has(message.`conformance.v1.ext_tag`)")
		if !compiled.Eval(Input{Message: message}) {
			t.Fatalf("set extension expression did not match: %v", compiled.Explain(Input{Message: message}))
		}
	})

	t.Run("unset", func(t *testing.T) {
		message := dynamicpb.NewMessage(method.Input())
		compiled := compileAnyExpr(t, reg, method.Input(), "message.`conformance.v1.ext_tag` == '' && !has(message.`conformance.v1.ext_tag`)")
		if !compiled.Eval(Input{Message: message}) {
			t.Fatalf("unset extension expression did not match: %v", compiled.Explain(Input{Message: message}))
		}
	})

	t.Run("runtime descriptor copy", func(t *testing.T) {
		fileProto := protodesc.ToFileDescriptorProto(method.Input().ParentFile())
		fileCopy, err := protodesc.NewFile(fileProto, nil)
		if err != nil {
			t.Fatalf("clone legacy descriptor: %v", err)
		}
		messageDesc := fileCopy.Messages().ByName("LegacyReply")
		runtimeExtension := dynamicpb.NewExtensionType(fileCopy.Extensions().ByName("ext_tag")).TypeDescriptor()
		message := dynamicpb.NewMessage(messageDesc)
		message.Set(runtimeExtension, protoreflect.ValueOfString("copied"))
		compiled := compileAnyExpr(t, reg, method.Input(), "message.`conformance.v1.ext_tag` == 'copied' && has(message.`conformance.v1.ext_tag`)")
		if !compiled.Eval(Input{Message: message}) {
			t.Fatalf("copied-descriptor extension expression did not match: %v", compiled.Explain(Input{Message: message}))
		}
	})

	t.Run("inside registered Any", func(t *testing.T) {
		if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
			t.Fatalf("AddProtoDir test schema: %v", err)
		}
		orderMethod, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
		if err != nil {
			t.Fatalf("LookupMethod OrderService/GetOrder: %v", err)
		}
		legacyReply := dynamicpb.NewMessage(method.Input())
		legacyReply.Set(extension, protoreflect.ValueOfString("in-any"))
		envelope := &anypb.Any{
			TypeUrl: "type.googleapis.com/conformance.v1.LegacyReply",
			Value:   mustMarshal(t, legacyReply),
		}
		compiled := compileAnyExpr(t, reg, orderMethod.Input(), "message.payload.`conformance.v1.ext_tag` == 'in-any' && has(message.payload.`conformance.v1.ext_tag`)")
		in := Input{Message: requestWithAny(t, orderMethod.Input(), envelope)}
		if !compiled.Eval(in) {
			t.Fatalf("registered Any extension expression did not match: %v", compiled.Explain(in))
		}
	})
}

func TestExprUnknownAnyIsNonMatchWithDiagnostic(t *testing.T) {
	reg, method := anyExprRegistry(t)
	compiled := compileAnyExpr(t, reg, method.Input(), `message.payload.id == "x"`)
	in := Input{Message: requestWithAny(t, method.Input(), &anypb.Any{
		TypeUrl: "type.googleapis.com/acme.Unknown",
		Value:   []byte{1, 2, 3},
	})}
	if compiled.Eval(in) {
		t.Fatal("truly unknown Any unexpectedly matched")
	}
	diagnostic := strings.Join(compiled.Explain(in), "\n")
	if !strings.Contains(diagnostic, "acme.Unknown") || !strings.Contains(diagnostic, "resolving") {
		t.Fatalf("unknown Any diagnostic = %q, want type name and resolver failure", diagnostic)
	}
}

func anyExprRegistry(t *testing.T) (*schema.Registry, protoreflect.MethodDescriptor) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	method, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	return reg, method
}

func compileAnyExpr(t *testing.T, reg *schema.Registry, input protoreflect.MessageDescriptor, expression string) *Compiled {
	t.Helper()
	compiled, err := NewCompiler(reg.Files()).Compile(input, &Block{Expr: expression}, Unary)
	if err != nil {
		t.Fatalf("Compile(%s): %v", expression, err)
	}
	return compiled
}

func requestWithAny(t *testing.T, input protoreflect.MessageDescriptor, envelope *anypb.Any) protoreflect.Message {
	t.Helper()
	request := dynamicpb.NewMessage(input)
	payloadField := input.Fields().ByName("payload")
	payload := request.Mutable(payloadField).Message()
	payload.Set(payload.Descriptor().Fields().ByName("type_url"), protoreflect.ValueOfString(envelope.TypeUrl))
	payload.Set(payload.Descriptor().Fields().ByName("value"), protoreflect.ValueOfBytes(envelope.Value))
	return request.ProtoReflect()
}

func mustMarshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("proto.Marshal %s: %v", message.ProtoReflect().Descriptor().FullName(), err)
	}
	return data
}

func TestExprMessagesForClientStream(t *testing.T) {
	desc := requestDesc(t)
	mc := NewCompiler(testFiles(t))
	c, err := mc.Compile(desc, &Block{Expr: `size(messages) == 2 && messages.exists(m, m.order_id == "o-1")`}, ClientStream)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	in := Input{Messages: []protoreflect.Message{
		msg(t, desc, `{"order_id":"o-1"}`),
		msg(t, desc, `{"order_id":"o-2"}`),
	}}
	if !c.Eval(in) {
		t.Error("Eval = false, want true")
	}
	if c.Eval(Input{Messages: in.Messages[:1]}) {
		t.Error("Eval with one message = true, want false")
	}
}

func TestExprCompileErrors(t *testing.T) {
	desc := requestDesc(t)
	mc := NewCompiler(testFiles(t))
	cases := []struct {
		name  string
		expr  string
		shape Shape
	}{
		{"not a bool", `size(message.items)`, Unary},
		{"unknown field", `message.no_such_field == 1`, Unary},
		{"syntax error", `message.order_id ==`, Unary},
		{"message var absent for client-streaming", `message.order_id == "x"`, ClientStream},
		{"messages var absent for unary", `size(messages) > 0`, Unary},
		{"message var absent at bidi open", `message.order_id == "x"`, Bidi},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mc.Compile(desc, &Block{Expr: tc.expr}, tc.shape); err == nil {
				t.Error("expected compile error, got nil")
			}
		})
	}
}

func TestExprWithoutFilesFails(t *testing.T) {
	desc := requestDesc(t)
	if _, err := NewCompiler(nil).Compile(desc, &Block{Expr: `true`}, Unary); err == nil {
		t.Error("expected error compiling expr without descriptor files")
	}
}
