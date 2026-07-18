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
	if err := validateTemplateFields(desc, fields); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if _, err := BuildMessage(types, desc, templateProjection(fields).(map[string]any)); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
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
	program cel.Program
	source  string
	types   *schema.Types
}

func (n exprNode) render(activation map[string]any) (any, error) {
	result, _, err := n.program.Eval(activation)
	if err != nil {
		return nil, fmt.Errorf("site {{ %s }}: %w", n.source, err)
	}
	value, err := celToJSON(n.types, result)
	if err != nil {
		return nil, fmt.Errorf("site {{ %s }}: %w", n.source, err)
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

func compileTemplateMap(env *cel.Env, types *schema.Types, desc protoreflect.MessageDescriptor, value map[string]any) (templateNode, error) {
	entries := make([]mapEntry, 0, len(value))
	for _, key := range sortedKeys(value) {
		field := fieldByJSONOrProtoName(desc, key)
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
		if !field.IsMap() {
			if field.Message() == nil {
				return staticNode{value: value}, nil
			}
			return compileTemplateMap(env, types, field.Message(), value)
		}
		entries := make([]mapEntry, 0, len(value))
		for _, key := range sortedKeys(value) {
			node, err := compileTemplateNode(env, types, value[key], field.MapValue(), false)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			entries = append(entries, mapEntry{key: key, node: node})
		}
		return mapNode{entries: entries}, nil
	case []any:
		entries := make([]templateNode, len(value))
		for i, child := range value {
			node, err := compileTemplateNode(env, types, child, field, true)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			entries[i] = node
		}
		return listNode{entries: entries}, nil
	default:
		return staticNode{value: value}, nil
	}
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
	if err := validateCELResultType(cel.StringType, field, listElement); err != nil {
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
	if err := validateCELResultType(ast.OutputType(), field, listElement); err != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, err)
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("template site {{ %s }}: %w", trimmed, err)
	}
	return &exprNode{program: program, source: trimmed, types: types}, nil
}

func validateCELResultType(result *cel.Type, field protoreflect.FieldDescriptor, listElement bool) error {
	switch result.Kind() {
	case cel.ListKind, cel.MapKind:
		return fmt.Errorf("unsupported CEL result type %s; supported values are bool, string, numeric scalars, bytes, timestamp, duration, null, and protobuf messages (not lists or maps)", result)
	case cel.DynKind, cel.AnyKind, types.UnknownKind, cel.TypeParamKind:
		return nil
	case cel.NullTypeKind:
		return nil
	}
	if field == nil || celResultFitsField(result, field, listElement) {
		return nil
	}
	return fmt.Errorf("CEL result type %s is incompatible with protobuf field %s (%s)", result, field.FullName(), protobufFieldType(field))
}

func celResultFitsField(result *cel.Type, field protoreflect.FieldDescriptor, listElement bool) bool {
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
		fullName := field.Message().FullName()
		if kind, ok := protobufWrapperKind(fullName); ok {
			return celResultFitsScalarKind(result, kind)
		}
		switch fullName {
		case "google.protobuf.Timestamp":
			return result.Kind() == cel.TimestampKind || result.Kind() == cel.StringKind
		case "google.protobuf.Duration":
			return result.Kind() == cel.DurationKind || result.Kind() == cel.StringKind
		case "google.protobuf.Any":
			return result.Kind() == cel.AnyKind || result.TypeName() == string(fullName)
		case "google.protobuf.Value":
			return celResultFitsValue(result)
		case "google.protobuf.FieldMask":
			return result.Kind() == cel.StringKind || result.Kind() == cel.BytesKind ||
				(result.Kind() == cel.StructKind && result.TypeName() == string(fullName))
		default:
			return result.Kind() == cel.StructKind && result.TypeName() == string(fullName)
		}
	default:
		return false
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

// templateProjection preserves static values for load-time schema validation
// while replacing dynamic sites with JSON null, which protojson accepts for a
// singular field of any protobuf type.
func templateProjection(value any) any {
	switch value := value.(type) {
	case string:
		if hasSites(value) {
			return nil
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for _, key := range sortedKeys(value) {
			out[key] = templateProjection(value[key])
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			if text, ok := child.(string); ok && hasSites(text) {
				continue
			}
			out = append(out, templateProjection(child))
		}
		return out
	default:
		return value
	}
}

func validateTemplateFields(desc protoreflect.MessageDescriptor, fields map[string]any) error {
	for _, name := range sortedKeys(fields) {
		value := fields[name]
		field := fieldByJSONOrProtoName(desc, name)
		if field == nil {
			return fmt.Errorf("response message does not fit %s: unknown field %q", desc.FullName(), name)
		}
		if field.IsMap() {
			if childDesc := field.MapValue().Message(); childDesc != nil {
				if entries, ok := value.(map[string]any); ok {
					for _, key := range sortedKeys(entries) {
						entry := entries[key]
						if child, ok := entry.(map[string]any); ok {
							if err := validateTemplateFields(childDesc, child); err != nil {
								return err
							}
						}
					}
				}
			}
			continue
		}
		childDesc := field.Message()
		if childDesc == nil {
			continue
		}
		if field.IsList() {
			if entries, ok := value.([]any); ok {
				for _, entry := range entries {
					if child, ok := entry.(map[string]any); ok {
						if err := validateTemplateFields(childDesc, child); err != nil {
							return err
						}
					}
				}
			}
			continue
		}
		if child, ok := value.(map[string]any); ok {
			if err := validateTemplateFields(childDesc, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func fieldByJSONOrProtoName(desc protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if field := desc.Fields().ByName(protoreflect.Name(name)); field != nil {
		return field
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		if fields.Get(i).JSONName() == name {
			return fields.Get(i)
		}
	}
	return nil
}
