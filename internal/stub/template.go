package stub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

var templateSiteRE = regexp.MustCompile(`(?s)\{\{(.*?)\}\}`)

// Template is a compiled response body. Static bodies keep one pre-built
// message; dynamic bodies keep a tree of CEL-backed rendering nodes.
type Template struct {
	desc   protoreflect.MessageDescriptor
	types  *schema.Types
	static *dynamicpb.Message
	root   templateNode
	source string
}

func newTemplate(env *cel.Env, types *schema.Types, desc protoreflect.MessageDescriptor, fields map[string]any, source string) (*Template, error) {
	t := &Template{desc: desc, types: types, source: source}
	if !hasSites(fields) {
		msg, err := BuildMessage(types, desc, fields)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		t.static = msg
		return t, nil
	}
	if env == nil {
		return nil, fmt.Errorf("%s: response templates require a CEL environment", source)
	}
	projection, err := projectTemplateMessage(types, desc, fields)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	projected, err := buildMessage(types, desc, projection, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	seedDynamicRequiredFields(projected.ProtoReflect(), fields)
	if err := proto.CheckInitialized(projected); err != nil {
		return nil, fmt.Errorf("%s: response message does not fit %s: %w", source, desc.FullName(), err)
	}
	root, err := compileTemplateMap(env, types, desc, fields)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	t.root = root
	return t, nil
}

// Render evaluates a response template against one request. Static templates
// return their load-time message pointer unchanged.
func (t *Template) Render(in match.Input) (*dynamicpb.Message, error) {
	if t.static != nil {
		return t.static, nil
	}
	value, err := t.root.render(match.Activation(in))
	if err != nil {
		return nil, fmt.Errorf("%s: rendering response template: %w", t.source, err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding rendered response as JSON: %w", t.source, err)
	}
	msg := dynamicpb.NewMessage(t.desc)
	if err := (protojson.UnmarshalOptions{Resolver: t.types}).Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("%s: rendered response message does not fit %s: %w", t.source, t.desc.FullName(), err)
	}
	return msg, nil
}

func hasSites(value any) bool {
	switch value := value.(type) {
	case string:
		return templateSiteRE.MatchString(value)
	case map[string]any:
		for _, key := range sortedKeys(value) {
			if hasSites(value[key]) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if hasSites(child) {
				return true
			}
		}
	}
	return false
}

type templateNode interface {
	render(map[string]any) (any, error)
}

type staticNode struct{ value any }

func (n staticNode) render(map[string]any) (any, error) { return n.value, nil }

type exprNode struct {
	program     cel.Program
	source      string
	types       *schema.Types
	field       protoreflect.FieldDescriptor
	messageDesc protoreflect.MessageDescriptor
	listElement bool
	packAny     bool
}

func (n exprNode) render(activation map[string]any) (any, error) {
	result, _, err := n.program.Eval(activation)
	if err != nil {
		return nil, fmt.Errorf("site {{ %s }}: %w", n.source, err)
	}
	var value any
	if n.packAny {
		value, err = celToAnyJSON(n.types, result)
	} else {
		value, err = celToJSON(n.types, result)
	}
	if err != nil {
		return nil, fmt.Errorf("site {{ %s }}: %w", n.source, err)
	}
	if n.field != nil {
		if err := validateRenderedFieldSite(n.types, n.field, n.listElement, value); err != nil {
			return nil, fmt.Errorf("site {{ %s }}: %w", n.source, err)
		}
	}
	if n.messageDesc != nil {
		if err := validateRenderedMessageSite(n.types, n.messageDesc, value); err != nil {
			return nil, fmt.Errorf("site {{ %s }}: %w", n.source, err)
		}
	}
	return value, nil
}

type interpPart struct {
	literal string
	expr    *exprNode
}

type interpNode struct{ parts []interpPart }

func (n interpNode) render(activation map[string]any) (any, error) {
	var out strings.Builder
	for _, part := range n.parts {
		if part.expr == nil {
			out.WriteString(part.literal)
			continue
		}
		value, err := part.expr.render(activation)
		if err != nil {
			return nil, err
		}
		switch value := value.(type) {
		case string:
			out.WriteString(value)
		case json.RawMessage:
			out.Write(value)
		case nil:
			out.WriteString("null")
		default:
			fmt.Fprint(&out, value)
		}
	}
	return out.String(), nil
}

type mapEntry struct {
	key  string
	node templateNode
}

type mapNode struct{ entries []mapEntry }

func (n mapNode) render(activation map[string]any) (any, error) {
	out := make(map[string]any, len(n.entries))
	for _, entry := range n.entries {
		value, err := entry.node.render(activation)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", entry.key, err)
		}
		out[entry.key] = value
	}
	return out, nil
}

type listNode struct{ entries []templateNode }

func (n listNode) render(activation map[string]any) (any, error) {
	out := make([]any, len(n.entries))
	for i, child := range n.entries {
		value, err := child.render(activation)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out[i] = value
	}
	return out, nil
}

// anyEnvelopeNode restores the protobuf-JSON Any envelope around a payload
// whose fields may contain template sites. Payload WKTs use Any's canonical
// `value` member; ordinary payload messages inline their JSON fields.
type anyEnvelopeNode struct {
	typeURL    string
	payload    templateNode
	valueField bool
}

func (n anyEnvelopeNode) render(activation map[string]any) (any, error) {
	payload, err := n.payload.render(activation)
	if err != nil {
		return nil, err
	}
	if n.valueField {
		return map[string]any{"@type": n.typeURL, "value": payload}, nil
	}
	fields, ok := payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Any payload %q rendered as %T, want JSON object", n.typeURL, payload)
	}
	out := make(map[string]any, len(fields)+1)
	out["@type"] = n.typeURL
	for key, value := range fields {
		out[key] = value
	}
	return out, nil
}

func compileTemplateMap(env *cel.Env, types *schema.Types, desc protoreflect.MessageDescriptor, value map[string]any) (templateNode, error) {
	entries := make([]mapEntry, 0, len(value))
	for _, key := range sortedKeys(value) {
		field, err := templateField(types, desc, key)
		if err != nil {
			return nil, err
		}
		if field == nil {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		node, err := compileTemplateNode(env, types, value[key], field, false)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		entries = append(entries, mapEntry{key: key, node: node})
	}
	return mapNode{entries: entries}, nil
}

func compileTemplateNode(env *cel.Env, types *schema.Types, value any, field protoreflect.FieldDescriptor, listElement bool) (templateNode, error) {
	switch value := value.(type) {
	case string:
		return compileTemplateString(env, types, value, field, listElement)
	case map[string]any:
		if field.IsMap() && !listElement {
			entries := make([]mapEntry, 0, len(value))
			for _, key := range sortedKeys(value) {
				node, err := compileTemplateNode(env, types, value[key], field.MapValue(), false)
				if err != nil {
					return nil, fmt.Errorf("field %q: %w", key, err)
				}
				entries = append(entries, mapEntry{key: key, node: node})
			}
			return mapNode{entries: entries}, nil
		}
		if field.Message() != nil {
			return compileTemplateMessageValue(env, types, field.Message(), value)
		}
		return staticNode{value: value}, nil
	case []any:
		if field.IsList() && !listElement {
			entries := make([]templateNode, len(value))
			for i, child := range value {
				node, err := compileTemplateNode(env, types, child, field, true)
				if err != nil {
					return nil, fmt.Errorf("element %d: %w", i, err)
				}
				entries[i] = node
			}
			return listNode{entries: entries}, nil
		}
		if field.Message() != nil {
			return compileTemplateMessageValue(env, types, field.Message(), value)
		}
		return staticNode{value: value}, nil
	default:
		return staticNode{value: value}, nil
	}
}

func compileTemplateMessageValue(env *cel.Env, types *schema.Types, desc protoreflect.MessageDescriptor, value any) (templateNode, error) {
	if text, ok := value.(string); ok {
		return compileTemplateMessageString(env, types, desc, text)
	}
	switch desc.FullName() {
	case "google.protobuf.Any":
		envelope, ok := value.(map[string]any)
		if !ok {
			return staticNode{value: value}, nil
		}
		return compileAnyEnvelope(env, types, envelope)
	case "google.protobuf.Struct":
		object, ok := value.(map[string]any)
		if !ok {
			return staticNode{value: value}, nil
		}
		return compileNaturalJSONMap(env, types, object)
	case "google.protobuf.Value":
		return compileNaturalJSONNode(env, types, value)
	case "google.protobuf.ListValue":
		list, ok := value.([]any)
		if !ok {
			return staticNode{value: value}, nil
		}
		return compileNaturalJSONList(env, types, list)
	default:
		object, ok := value.(map[string]any)
		if !ok {
			return staticNode{value: value}, nil
		}
		return compileTemplateMap(env, types, desc, object)
	}
}

func compileNaturalJSONNode(env *cel.Env, types *schema.Types, value any) (templateNode, error) {
	switch value := value.(type) {
	case string:
		return compileTemplateString(env, types, value, nil, false)
	case map[string]any:
		return compileNaturalJSONMap(env, types, value)
	case []any:
		return compileNaturalJSONList(env, types, value)
	default:
		return staticNode{value: value}, nil
	}
}

func compileNaturalJSONMap(env *cel.Env, types *schema.Types, value map[string]any) (templateNode, error) {
	entries := make([]mapEntry, 0, len(value))
	for _, key := range sortedKeys(value) {
		node, err := compileNaturalJSONNode(env, types, value[key])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		entries = append(entries, mapEntry{key: key, node: node})
	}
	return mapNode{entries: entries}, nil
}

func compileNaturalJSONList(env *cel.Env, types *schema.Types, value []any) (templateNode, error) {
	entries := make([]templateNode, len(value))
	for i, child := range value {
		node, err := compileNaturalJSONNode(env, types, child)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		entries[i] = node
	}
	return listNode{entries: entries}, nil
}

func compileAnyEnvelope(env *cel.Env, types *schema.Types, envelope map[string]any) (templateNode, error) {
	typeURL, payloadDesc, err := resolveAnyTemplateType(types, envelope)
	if err != nil {
		return nil, err
	}
	if anyPayloadUsesValueField(payloadDesc) {
		for _, key := range sortedKeys(envelope) {
			if key != "@type" && key != "value" {
				return nil, fmt.Errorf("Any payload %q uses protobuf JSON field %q, not %q", typeURL, "value", key)
			}
		}
		value, ok := envelope["value"]
		if !ok {
			return nil, fmt.Errorf("Any payload %q requires protobuf JSON field %q", typeURL, "value")
		}
		payload, err := compileTemplateMessageValue(env, types, payloadDesc, value)
		if err != nil {
			return nil, fmt.Errorf("Any payload %q: %w", typeURL, err)
		}
		return anyEnvelopeNode{typeURL: typeURL, payload: payload, valueField: true}, nil
	}
	payloadFields := make(map[string]any, len(envelope)-1)
	for key, value := range envelope {
		if key != "@type" {
			payloadFields[key] = value
		}
	}
	payload, err := compileTemplateMap(env, types, payloadDesc, payloadFields)
	if err != nil {
		return nil, fmt.Errorf("Any payload %q: %w", typeURL, err)
	}
	return anyEnvelopeNode{typeURL: typeURL, payload: payload}, nil
}

func compileTemplateMessageString(env *cel.Env, types *schema.Types, desc protoreflect.MessageDescriptor, value string) (templateNode, error) {
	trimmed := strings.TrimSpace(value)
	matches := templateSiteRE.FindAllStringSubmatchIndex(trimmed, -1)
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(trimmed) {
		return compileMessageExpr(env, types, trimmed[matches[0][2]:matches[0][3]], desc)
	}

	matches = templateSiteRE.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return staticNode{value: value}, nil
	}
	parts := make([]interpPart, 0, len(matches)*2+1)
	last := 0
	for _, match := range matches {
		if match[0] > last {
			parts = append(parts, interpPart{literal: value[last:match[0]]})
		}
		expr, err := compileExpr(env, types, value[match[2]:match[3]], nil, false)
		if err != nil {
			return nil, err
		}
		parts = append(parts, interpPart{expr: expr})
		last = match[1]
	}
	if last < len(value) {
		parts = append(parts, interpPart{literal: value[last:]})
	}
	if !celResultFitsMessage(types, cel.StringType, desc) {
		return nil, fmt.Errorf("interpolated template string is incompatible with protobuf message %s", desc.FullName())
	}
	return interpNode{parts: parts}, nil
}

func compileMessageExpr(env *cel.Env, resolver *schema.Types, source string, desc protoreflect.MessageDescriptor) (*exprNode, error) {
	trimmed := strings.TrimSpace(source)
	ast, issues := env.Compile(trimmed)
	if issues.Err() != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, issues.Err())
	}
	result := ast.OutputType()
	switch result.Kind() {
	case cel.ListKind, cel.MapKind:
		return nil, fmt.Errorf("template site {{ %s }}: unsupported CEL result type %s; supported values are bool, string, numeric scalars, bytes, timestamp, duration, null, and protobuf messages (not lists or maps)", trimmed, result)
	case cel.DynKind, cel.AnyKind, types.UnknownKind, cel.TypeParamKind, cel.NullTypeKind:
	default:
		if !celResultFitsMessage(resolver, result, desc) {
			return nil, fmt.Errorf("template site {{ %s }}: CEL result type %s is incompatible with protobuf message %s", trimmed, result, desc.FullName())
		}
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, err)
	}
	return &exprNode{program: program, source: trimmed, types: resolver, messageDesc: desc, packAny: desc.FullName() == "google.protobuf.Any"}, nil
}

func compileTemplateString(env *cel.Env, types *schema.Types, value string, field protoreflect.FieldDescriptor, listElement bool) (templateNode, error) {
	trimmed := strings.TrimSpace(value)
	matches := templateSiteRE.FindAllStringSubmatchIndex(trimmed, -1)
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(trimmed) {
		return compileExpr(env, types, trimmed[matches[0][2]:matches[0][3]], field, listElement)
	}

	matches = templateSiteRE.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return staticNode{value: value}, nil
	}
	parts := make([]interpPart, 0, len(matches)*2+1)
	last := 0
	for _, match := range matches {
		if match[0] > last {
			parts = append(parts, interpPart{literal: value[last:match[0]]})
		}
		expr, err := compileExpr(env, types, value[match[2]:match[3]], nil, false)
		if err != nil {
			return nil, err
		}
		parts = append(parts, interpPart{expr: expr})
		last = match[1]
	}
	if last < len(value) {
		parts = append(parts, interpPart{literal: value[last:]})
	}
	if err := validateCELResultType(types, cel.StringType, field, listElement); err != nil {
		return nil, fmt.Errorf("interpolated template string: %w", err)
	}
	return interpNode{parts: parts}, nil
}

func compileExpr(env *cel.Env, types *schema.Types, source string, field protoreflect.FieldDescriptor, listElement bool) (*exprNode, error) {
	trimmed := strings.TrimSpace(source)
	ast, issues := env.Compile(trimmed)
	if issues.Err() != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, issues.Err())
	}
	if err := validateCELResultType(types, ast.OutputType(), field, listElement); err != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, err)
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, err)
	}
	return &exprNode{program: program, source: trimmed, types: types, field: field, listElement: listElement, packAny: isAnyDestination(field)}, nil
}

func validateCELResultType(resolver *schema.Types, result *cel.Type, field protoreflect.FieldDescriptor, listElement bool) error {
	switch result.Kind() {
	case cel.ListKind, cel.MapKind:
		return fmt.Errorf("unsupported CEL result type %s; supported values are bool, string, numeric scalars, bytes, timestamp, duration, null, and protobuf messages (not lists or maps)", result)
	case cel.DynKind, cel.AnyKind, types.UnknownKind, cel.TypeParamKind:
		return nil
	case cel.NullTypeKind:
		return nil
	}
	if field == nil || celResultFitsField(resolver, result, field, listElement) {
		return nil
	}
	return fmt.Errorf("CEL result type %s is incompatible with protobuf field %s (%s)", result, field.FullName(), protobufFieldType(field))
}

func celResultFitsField(resolver *schema.Types, result *cel.Type, field protoreflect.FieldDescriptor, listElement bool) bool {
	if !listElement && (field.IsList() || field.IsMap()) {
		return false
	}
	// Compare the JSON shape produced by celToJSON with the shapes protojson
	// accepts. Content-dependent constraints (ranges, enum names, base64, and
	// timestamp text) remain request-time validation concerns.
	switch field.Kind() {
	case protoreflect.BoolKind, protoreflect.StringKind, protoreflect.BytesKind,
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return celResultFitsScalarKind(result, field.Kind())
	case protoreflect.EnumKind:
		return result.Kind() == cel.IntKind || result.Kind() == cel.UintKind || result.Kind() == cel.DoubleKind || result.Kind() == cel.StringKind
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return celResultFitsMessage(resolver, result, field.Message())
	default:
		return false
	}
}

func celResultFitsMessage(resolver *schema.Types, result *cel.Type, desc protoreflect.MessageDescriptor) bool {
	fullName := desc.FullName()
	if kind, ok := protobufWrapperKind(fullName); ok {
		return celResultFitsScalarKind(result, kind)
	}
	switch fullName {
	case "google.protobuf.Timestamp":
		return result.Kind() == cel.TimestampKind || result.Kind() == cel.StringKind
	case "google.protobuf.Duration":
		return result.Kind() == cel.DurationKind || result.Kind() == cel.StringKind
	case "google.protobuf.Any":
		if result.Kind() == cel.AnyKind || result.TypeName() == string(fullName) {
			return true
		}
		if result.Kind() != cel.StructKind || result.TypeName() == "" || resolver == nil {
			return false
		}
		_, err := resolver.FindMessageByName(protoreflect.FullName(result.TypeName()))
		return err == nil
	case "google.protobuf.Value":
		return celResultFitsValue(result)
	case "google.protobuf.FieldMask":
		return result.Kind() == cel.StringKind || result.Kind() == cel.BytesKind ||
			(result.Kind() == cel.StructKind && result.TypeName() == string(fullName))
	default:
		return result.Kind() == cel.StructKind && result.TypeName() == string(fullName)
	}
}

func protobufWrapperKind(name protoreflect.FullName) (protoreflect.Kind, bool) {
	switch name {
	case "google.protobuf.BoolValue":
		return protoreflect.BoolKind, true
	case "google.protobuf.BytesValue":
		return protoreflect.BytesKind, true
	case "google.protobuf.DoubleValue", "google.protobuf.FloatValue":
		return protoreflect.DoubleKind, true
	case "google.protobuf.Int32Value", "google.protobuf.Int64Value":
		return protoreflect.Int64Kind, true
	case "google.protobuf.UInt32Value", "google.protobuf.UInt64Value":
		return protoreflect.Uint64Kind, true
	case "google.protobuf.StringValue":
		return protoreflect.StringKind, true
	default:
		return 0, false
	}
}

func celResultFitsScalarKind(result *cel.Type, kind protoreflect.Kind) bool {
	switch kind {
	case protoreflect.BoolKind:
		return result.Kind() == cel.BoolKind
	case protoreflect.StringKind:
		return celResultRendersString(result)
	case protoreflect.BytesKind:
		return result.Kind() == cel.BytesKind || result.Kind() == cel.StringKind
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return result.Kind() == cel.IntKind || result.Kind() == cel.UintKind || result.Kind() == cel.DoubleKind || result.Kind() == cel.StringKind
	default:
		return false
	}
}

func celResultFitsValue(result *cel.Type) bool {
	if celResultRendersString(result) {
		return true
	}
	switch result.Kind() {
	case cel.BoolKind, cel.DoubleKind, cel.IntKind, cel.UintKind, cel.StructKind:
		return true
	default:
		return false
	}
}

func celResultRendersString(result *cel.Type) bool {
	switch result.Kind() {
	case cel.StringKind, cel.BytesKind, cel.TimestampKind, cel.DurationKind:
		return true
	case cel.StructKind:
		return result.TypeName() == "google.protobuf.FieldMask"
	default:
		return false
	}
}

func protobufFieldType(field protoreflect.FieldDescriptor) string {
	if message := field.Message(); message != nil {
		return string(message.FullName())
	}
	if enum := field.Enum(); enum != nil {
		return string(enum.FullName())
	}
	return field.Kind().String()
}

func isAnyDestination(field protoreflect.FieldDescriptor) bool {
	return field != nil && field.Message() != nil && field.Message().FullName() == "google.protobuf.Any"
}

func validateRenderedFieldSite(types *schema.Types, field protoreflect.FieldDescriptor, listElement bool, value any) error {
	rendered := value
	if field.IsList() && listElement {
		rendered = []any{value}
	}
	name := field.JSONName()
	if field.IsExtension() {
		name = "[" + string(field.FullName()) + "]"
	}
	data, err := json.Marshal(map[string]any{name: rendered})
	if err != nil {
		return fmt.Errorf("encoding rendered value for protobuf field %s: %w", field.FullName(), err)
	}
	message := dynamicpb.NewMessage(field.ContainingMessage())
	if err := (protojson.UnmarshalOptions{Resolver: types, AllowPartial: true}).Unmarshal(data, message); err != nil {
		return fmt.Errorf("rendered value does not fit protobuf field %s: %w", field.FullName(), err)
	}
	if field.Cardinality() == protoreflect.Required && !message.Has(field) {
		return fmt.Errorf("rendered value leaves required protobuf field %s unset", field.FullName())
	}
	return nil
}

func validateRenderedMessageSite(types *schema.Types, desc protoreflect.MessageDescriptor, value any) error {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding rendered value for protobuf message %s: %w", desc.FullName(), err)
	}
	message := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{Resolver: types}).Unmarshal(data, message); err != nil {
		return fmt.Errorf("rendered value does not fit protobuf message %s: %w", desc.FullName(), err)
	}
	return nil
}

// celToJSON converts CEL's native scalar and protobuf values into values the
// standard JSON encoder can pass to protojson. Composite CEL list/map values
// are intentionally rejected: response structure belongs in YAML.
func celToJSON(resolver *schema.Types, value any) (any, error) {
	if celValue, ok := value.(ref.Val); ok {
		if types.IsError(celValue) {
			return nil, fmt.Errorf("CEL evaluation failed: %v", celValue)
		}
		if celValue == types.NullValue {
			return nil, nil
		}
		value = celValue.Value()
	}
	switch value := value.(type) {
	case nil:
		return nil, nil
	case bool, string, int64, uint64, float64:
		return value, nil
	case []byte:
		return base64.StdEncoding.EncodeToString(value), nil
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano), nil
	case time.Duration:
		return durationJSON(value)
	case proto.Message:
		data, err := (protojson.MarshalOptions{Resolver: resolver}).Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("marshaling protobuf value as JSON: %w", err)
		}
		return json.RawMessage(data), nil
	default:
		return nil, fmt.Errorf("unsupported CEL result type %T; supported values are bool, string, numeric scalars, bytes, timestamp, duration, null, and protobuf messages (not lists or maps)", value)
	}
}

// celToAnyJSON preserves Any's envelope when CEL exposes the registered
// payload message directly. The adapter deliberately unwraps Any for field
// access, so rendering into an Any destination must explicitly repack it.
func celToAnyJSON(resolver *schema.Types, value any) (any, error) {
	if celValue, ok := value.(ref.Val); ok {
		if types.IsError(celValue) {
			return nil, fmt.Errorf("CEL evaluation failed: %v", celValue)
		}
		if celValue == types.NullValue {
			return nil, nil
		}
		value = celValue.Value()
	}
	if reflected, ok := value.(protoreflect.Message); ok {
		value = reflected.Interface()
	}
	message, ok := value.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("Any destination requires a registered protobuf message, got %T", value)
	}
	fullName := message.ProtoReflect().Descriptor().FullName()
	if fullName != "google.protobuf.Any" {
		if resolver == nil {
			return nil, fmt.Errorf("Any destination cannot resolve protobuf message %s without a type registry", fullName)
		}
		if _, err := resolver.FindMessageByName(fullName); err != nil {
			return nil, fmt.Errorf("Any destination protobuf message %s is not registered: %w", fullName, err)
		}
		packed, err := anypb.New(message)
		if err != nil {
			return nil, fmt.Errorf("packing protobuf message %s into Any: %w", fullName, err)
		}
		message = packed
	}
	data, err := (protojson.MarshalOptions{Resolver: resolver}).Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("marshaling Any protobuf JSON: %w", err)
	}
	return json.RawMessage(data), nil
}

func durationJSON(value time.Duration) (string, error) {
	data, err := protojson.Marshal(durationpb.New(value))
	if err != nil {
		return "", fmt.Errorf("marshaling duration as protobuf JSON: %w", err)
	}
	var out string
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decoding protobuf duration JSON: %w", err)
	}
	return out, nil
}

// projectTemplateMessage preserves static values for load-time protojson
// validation while replacing dynamic sites with JSON null. Unlike a generic
// tree walk, it follows the destination descriptor so protobuf JSON's natural
// WKT forms, Any envelopes, and extension names are interpreted correctly.
func projectTemplateMessage(types *schema.Types, desc protoreflect.MessageDescriptor, fields map[string]any) (map[string]any, error) {
	switch desc.FullName() {
	case "google.protobuf.Struct", "google.protobuf.Value":
		projected, _ := projectNaturalJSON(fields).(map[string]any)
		return projected, nil
	case "google.protobuf.Any":
		return projectAnyEnvelope(types, fields)
	default:
		return projectMessageMap(types, desc, fields)
	}
}

func projectMessageMap(types *schema.Types, desc protoreflect.MessageDescriptor, fields map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for _, name := range sortedKeys(fields) {
		field, err := templateField(types, desc, name)
		if err != nil {
			return nil, fmt.Errorf("response message does not fit %s: %w", desc.FullName(), err)
		}
		if field == nil {
			return nil, fmt.Errorf("response message does not fit %s: unknown field %q", desc.FullName(), name)
		}
		projected, err := projectFieldValue(types, field, fields[name], false)
		if err != nil {
			return nil, fmt.Errorf("response message does not fit %s field %q: %w", desc.FullName(), name, err)
		}
		out[name] = projected
	}
	return out, nil
}

func projectFieldValue(types *schema.Types, field protoreflect.FieldDescriptor, value any, listElement bool) (any, error) {
	if text, ok := value.(string); ok {
		if hasSites(text) {
			return nil, nil
		}
		return text, nil
	}
	switch value := value.(type) {
	case map[string]any:
		if field.IsMap() && !listElement {
			out := make(map[string]any, len(value))
			for _, key := range sortedKeys(value) {
				projected, err := projectFieldValue(types, field.MapValue(), value[key], false)
				if err != nil {
					return nil, fmt.Errorf("map key %q: %w", key, err)
				}
				out[key] = projected
			}
			return out, nil
		}
		if field.Message() != nil {
			return projectMessageValue(types, field.Message(), value)
		}
		return value, nil
	case []any:
		if field.IsList() && !listElement {
			out := make([]any, 0, len(value))
			for _, child := range value {
				if text, ok := child.(string); ok && hasSites(text) {
					continue
				}
				projected, err := projectFieldValue(types, field, child, true)
				if err != nil {
					return nil, err
				}
				out = append(out, projected)
			}
			return out, nil
		}
		if field.Message() != nil {
			return projectMessageValue(types, field.Message(), value)
		}
		return value, nil
	default:
		return value, nil
	}
}

func projectMessageValue(types *schema.Types, desc protoreflect.MessageDescriptor, value any) (any, error) {
	if text, ok := value.(string); ok {
		if hasSites(text) {
			return nil, nil
		}
		return text, nil
	}
	switch desc.FullName() {
	case "google.protobuf.Any":
		envelope, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		return projectAnyEnvelope(types, envelope)
	case "google.protobuf.Struct", "google.protobuf.Value":
		return projectNaturalJSON(value), nil
	case "google.protobuf.ListValue":
		return projectNaturalJSON(value), nil
	default:
		object, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		return projectMessageMap(types, desc, object)
	}
}

func projectNaturalJSON(value any) any {
	switch value := value.(type) {
	case string:
		if hasSites(value) {
			return nil
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for _, key := range sortedKeys(value) {
			out[key] = projectNaturalJSON(value[key])
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = projectNaturalJSON(child)
		}
		return out
	default:
		return value
	}
}

func projectAnyEnvelope(types *schema.Types, envelope map[string]any) (map[string]any, error) {
	typeURL, payloadDesc, err := resolveAnyTemplateType(types, envelope)
	if err != nil {
		return nil, err
	}
	if anyPayloadUsesValueField(payloadDesc) {
		for _, key := range sortedKeys(envelope) {
			if key != "@type" && key != "value" {
				return nil, fmt.Errorf("Any payload %q uses protobuf JSON field %q, not %q", typeURL, "value", key)
			}
		}
		value, ok := envelope["value"]
		if !ok {
			return nil, fmt.Errorf("Any payload %q requires protobuf JSON field %q", typeURL, "value")
		}
		projected, err := projectMessageValue(types, payloadDesc, value)
		if err != nil {
			return nil, fmt.Errorf("Any payload %q: %w", typeURL, err)
		}
		return map[string]any{"@type": typeURL, "value": projected}, nil
	}
	payloadFields := make(map[string]any, len(envelope)-1)
	for key, value := range envelope {
		if key != "@type" {
			payloadFields[key] = value
		}
	}
	projected, err := projectMessageMap(types, payloadDesc, payloadFields)
	if err != nil {
		return nil, fmt.Errorf("Any payload %q: %w", typeURL, err)
	}
	projected["@type"] = typeURL
	return projected, nil
}

func resolveAnyTemplateType(types *schema.Types, envelope map[string]any) (string, protoreflect.MessageDescriptor, error) {
	raw, ok := envelope["@type"]
	if !ok {
		return "", nil, fmt.Errorf("Any protobuf JSON object requires a static %q field", "@type")
	}
	typeURL, ok := raw.(string)
	if !ok || typeURL == "" || hasSites(typeURL) {
		return "", nil, fmt.Errorf("Any protobuf JSON field %q must be a non-empty static string", "@type")
	}
	messageType, err := types.FindMessageByURL(typeURL)
	if err != nil {
		return "", nil, fmt.Errorf("Any type %q is not registered: %w", typeURL, err)
	}
	return typeURL, messageType.Descriptor(), nil
}

func anyPayloadUsesValueField(desc protoreflect.MessageDescriptor) bool {
	if _, ok := protobufWrapperKind(desc.FullName()); ok {
		return true
	}
	switch desc.FullName() {
	case "google.protobuf.Any", "google.protobuf.Duration", "google.protobuf.FieldMask",
		"google.protobuf.ListValue", "google.protobuf.Struct", "google.protobuf.Timestamp",
		"google.protobuf.Value":
		return true
	default:
		return false
	}
}

// seedDynamicRequiredFields makes only the load-time projection initialized.
// A compatible dynamic site is known to provide the required field at render
// time, but JSON null deliberately leaves it absent in the projected message.
// The real rendered message is still unmarshaled without AllowPartial.
func seedDynamicRequiredFields(message protoreflect.Message, source map[string]any) {
	desc := message.Descriptor()
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		raw, present := sourceTemplateField(source, field)
		if field.Cardinality() == protoreflect.Required && !message.Has(field) && present && hasSites(raw) {
			seedRequiredField(message, field)
			continue
		}
		if !present || field.Message() == nil || !message.Has(field) {
			continue
		}
		switch {
		case field.IsMap() && field.MapValue().Message() != nil:
			entries, ok := raw.(map[string]any)
			if !ok || field.MapKey().Kind() != protoreflect.StringKind {
				continue
			}
			values := message.Get(field).Map()
			for key, entry := range entries {
				child, ok := entry.(map[string]any)
				mapKey := protoreflect.ValueOfString(key).MapKey()
				if ok && values.Has(mapKey) {
					seedDynamicRequiredFields(values.Get(mapKey).Message(), child)
				}
			}
		case field.IsList():
			entries, ok := raw.([]any)
			if !ok {
				continue
			}
			values := message.Get(field).List()
			projectedIndex := 0
			for _, entry := range entries {
				if text, ok := entry.(string); ok && hasSites(text) {
					continue
				}
				if child, ok := entry.(map[string]any); ok && projectedIndex < values.Len() {
					seedDynamicRequiredFields(values.Get(projectedIndex).Message(), child)
				}
				projectedIndex++
			}
		default:
			if child, ok := raw.(map[string]any); ok {
				seedDynamicRequiredFields(message.Get(field).Message(), child)
			}
		}
	}
}

func sourceTemplateField(source map[string]any, field protoreflect.FieldDescriptor) (any, bool) {
	if value, ok := source[string(field.Name())]; ok {
		return value, true
	}
	value, ok := source[field.JSONName()]
	return value, ok
}

func seedRequiredField(message protoreflect.Message, field protoreflect.FieldDescriptor) {
	if field.Message() == nil {
		message.Set(field, field.Default())
		return
	}
	child := message.Mutable(field).Message()
	seedAllRequiredFields(child)
}

func seedAllRequiredFields(message protoreflect.Message) {
	fields := message.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.Cardinality() == protoreflect.Required && !message.Has(field) {
			seedRequiredField(message, field)
		}
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func templateField(types *schema.Types, desc protoreflect.MessageDescriptor, name string) (protoreflect.FieldDescriptor, error) {
	if strings.HasPrefix(name, "[") || strings.HasSuffix(name, "]") {
		if len(name) < 3 || name[0] != '[' || name[len(name)-1] != ']' {
			return nil, fmt.Errorf("malformed extension JSON name %q", name)
		}
		extensionName := protoreflect.FullName(name[1 : len(name)-1])
		extensionType, err := types.FindExtensionByName(extensionName)
		if err != nil {
			return nil, fmt.Errorf("unknown extension JSON name %q: %w", name, err)
		}
		extension := extensionType.TypeDescriptor()
		if extension.ContainingMessage().FullName() != desc.FullName() {
			return nil, fmt.Errorf("extension %q extends %s, not %s", extensionName, extension.ContainingMessage().FullName(), desc.FullName())
		}
		return extension, nil
	}
	if field := desc.Fields().ByName(protoreflect.Name(name)); field != nil {
		return field, nil
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		if fields.Get(i).JSONName() == name {
			return fields.Get(i), nil
		}
	}
	return nil, nil
}
