// Package match compiles the structured `match:` block of a stub against a
// message descriptor and evaluates decoded requests against it. Compilation
// validates every field path, operator, and literal, so bad stubs fail at
// load time — never silently at request time.
package match

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Block is the YAML shape of a stub's `match:` section (M1 structured subset).
type Block struct {
	Metadata map[string]Rules `yaml:"metadata"`
	Message  map[string]Rules `yaml:"message"`
}

// Rules maps an operator name (eq, ne, in, matches, present, contains) to its literal.
type Rules map[string]any

type Compiled struct {
	metadata []mdRule
	message  []msgRule
}

type mdRule struct {
	key  string
	op   string
	str  string
	list []string
	re   *regexp.Regexp
	want bool
}

type msgRule struct {
	path []protoreflect.FieldDescriptor
	op   string
	lit  literal
	list []literal
	re   *regexp.Regexp
	want bool
}

// literal is a stub-file literal pre-converted to the leaf field's kind.
type literal struct {
	kind protoreflect.Kind
	str  string
	b    bool
	i    int64
	u    uint64
	f    float64
	enum protoreflect.EnumNumber
}

// Compile validates the block against the request descriptor. A nil block
// compiles to a matcher that accepts everything.
func Compile(input protoreflect.MessageDescriptor, b *Block) (*Compiled, error) {
	c := &Compiled{}
	if b == nil {
		return c, nil
	}
	for key, rules := range b.Metadata {
		for op, raw := range rules {
			r, err := compileMDRule(strings.ToLower(key), op, raw)
			if err != nil {
				return nil, fmt.Errorf("metadata %q: %w", key, err)
			}
			c.metadata = append(c.metadata, r)
		}
	}
	for path, rules := range b.Message {
		fds, err := resolvePath(input, path)
		if err != nil {
			return nil, err
		}
		for op, raw := range rules {
			r, err := compileMsgRule(fds, op, raw)
			if err != nil {
				return nil, fmt.Errorf("message field %q: %w", path, err)
			}
			c.message = append(c.message, r)
		}
	}
	return c, nil
}

func (c *Compiled) Eval(msg protoreflect.Message, md metadata.MD) bool {
	for _, r := range c.metadata {
		if !r.eval(md) {
			return false
		}
	}
	for _, r := range c.message {
		if !r.eval(msg) {
			return false
		}
	}
	return true
}

// --- metadata rules ---

func compileMDRule(key, op string, raw any) (mdRule, error) {
	r := mdRule{key: key, op: op}
	switch op {
	case "eq", "ne", "matches":
		s, ok := raw.(string)
		if !ok {
			return r, fmt.Errorf("%s wants a string, got %T", op, raw)
		}
		if op == "matches" {
			re, err := regexp.Compile(s)
			if err != nil {
				return r, fmt.Errorf("invalid regex: %w", err)
			}
			r.re = re
		} else {
			r.str = s
		}
	case "in":
		items, ok := raw.([]any)
		if !ok {
			return r, fmt.Errorf("in wants a list, got %T", raw)
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return r, fmt.Errorf("in wants strings, got %T", it)
			}
			r.list = append(r.list, s)
		}
	case "present":
		want, ok := raw.(bool)
		if !ok {
			return r, fmt.Errorf("present wants true/false, got %T", raw)
		}
		r.want = want
	default:
		return r, fmt.Errorf("unknown metadata operator %q (want eq, ne, in, matches, present)", op)
	}
	return r, nil
}

func (r mdRule) eval(md metadata.MD) bool {
	vals := md.Get(r.key)
	switch r.op {
	case "present":
		return (len(vals) > 0) == r.want
	case "eq":
		return slices.Contains(vals, r.str)
	case "ne":
		return !slices.Contains(vals, r.str)
	case "in":
		for _, v := range vals {
			if slices.Contains(r.list, v) {
				return true
			}
		}
		return false
	case "matches":
		for _, v := range vals {
			if r.re.MatchString(v) {
				return true
			}
		}
		return false
	}
	return false
}

// --- message rules ---

func resolvePath(md protoreflect.MessageDescriptor, path string) ([]protoreflect.FieldDescriptor, error) {
	parts := strings.Split(path, ".")
	out := make([]protoreflect.FieldDescriptor, 0, len(parts))
	cur := md
	for i, p := range parts {
		fd := cur.Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return nil, fmt.Errorf("field %q not found in %s (path %q)", p, cur.FullName(), path)
		}
		out = append(out, fd)
		if i < len(parts)-1 {
			if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
				return nil, fmt.Errorf("cannot descend into %q in path %q: not a singular message field", p, path)
			}
			cur = fd.Message()
		}
	}
	return out, nil
}

func compileMsgRule(path []protoreflect.FieldDescriptor, op string, raw any) (msgRule, error) {
	leaf := path[len(path)-1]
	r := msgRule{path: path, op: op}
	switch op {
	case "present":
		want, ok := raw.(bool)
		if !ok {
			return r, fmt.Errorf("present wants true/false, got %T", raw)
		}
		r.want = want
		return r, nil
	case "matches":
		if leaf.Kind() != protoreflect.StringKind || leaf.IsList() || leaf.IsMap() {
			return r, fmt.Errorf("matches requires a singular string field, %q is %s", leaf.Name(), leaf.Kind())
		}
		s, ok := raw.(string)
		if !ok {
			return r, fmt.Errorf("matches wants a string regex, got %T", raw)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return r, fmt.Errorf("invalid regex: %w", err)
		}
		r.re = re
		return r, nil
	case "contains":
		if !leaf.IsList() {
			return r, fmt.Errorf("contains requires a repeated field, %q is singular", leaf.Name())
		}
		lit, err := literalFor(leaf, raw)
		if err != nil {
			return r, err
		}
		r.lit = lit
		return r, nil
	case "eq", "ne":
		if leaf.IsList() || leaf.IsMap() {
			return r, fmt.Errorf("%s requires a singular field, %q is repeated/map (use contains)", op, leaf.Name())
		}
		lit, err := literalFor(leaf, raw)
		if err != nil {
			return r, err
		}
		r.lit = lit
		return r, nil
	case "in":
		if leaf.IsList() || leaf.IsMap() {
			return r, fmt.Errorf("in requires a singular field, %q is repeated/map", leaf.Name())
		}
		items, ok := raw.([]any)
		if !ok {
			return r, fmt.Errorf("in wants a list, got %T", raw)
		}
		for _, it := range items {
			lit, err := literalFor(leaf, it)
			if err != nil {
				return r, err
			}
			r.list = append(r.list, lit)
		}
		return r, nil
	default:
		return r, fmt.Errorf("unknown operator %q (want eq, ne, in, matches, present, contains)", op)
	}
}

func literalFor(fd protoreflect.FieldDescriptor, raw any) (literal, error) {
	k := fd.Kind()
	l := literal{kind: k}
	switch k {
	case protoreflect.StringKind:
		s, ok := raw.(string)
		if !ok {
			return l, fmt.Errorf("field %q is a string, got %T literal", fd.Name(), raw)
		}
		l.str = s
	case protoreflect.BoolKind:
		b, ok := raw.(bool)
		if !ok {
			return l, fmt.Errorf("field %q is a bool, got %T literal", fd.Name(), raw)
		}
		l.b = b
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		i, ok := toInt64(raw)
		if !ok {
			return l, fmt.Errorf("field %q is an integer, got %T literal", fd.Name(), raw)
		}
		l.i = i
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		i, ok := toInt64(raw)
		if !ok || i < 0 {
			return l, fmt.Errorf("field %q is an unsigned integer, got %v", fd.Name(), raw)
		}
		l.u = uint64(i)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		switch v := raw.(type) {
		case float64:
			l.f = v
		case int:
			l.f = float64(v)
		default:
			return l, fmt.Errorf("field %q is a float, got %T literal", fd.Name(), raw)
		}
	case protoreflect.EnumKind:
		switch v := raw.(type) {
		case string:
			ev := fd.Enum().Values().ByName(protoreflect.Name(v))
			if ev == nil {
				return l, fmt.Errorf("enum %s has no value named %q", fd.Enum().FullName(), v)
			}
			l.enum = ev.Number()
		case int:
			l.enum = protoreflect.EnumNumber(v)
		default:
			return l, fmt.Errorf("field %q is an enum, got %T literal", fd.Name(), raw)
		}
	default:
		return l, fmt.Errorf("field %q has kind %s, which structured matchers do not support yet (bytes/message/map matching arrives with CEL in M2)", fd.Name(), k)
	}
	return l, nil
}

func toInt64(raw any) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

func (l literal) equal(v protoreflect.Value) bool {
	switch l.kind {
	case protoreflect.StringKind:
		return v.String() == l.str
	case protoreflect.BoolKind:
		return v.Bool() == l.b
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int() == l.i
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return v.Uint() == l.u
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float() == l.f
	case protoreflect.EnumKind:
		return v.Enum() == l.enum
	}
	return false
}

func (r msgRule) eval(root protoreflect.Message) bool {
	if r.op == "present" {
		return r.hasPath(root) == r.want
	}
	// Walk intermediates; Get on an unset message field yields an empty
	// read-only message, so unset leaves evaluate as proto defaults.
	msg := root
	for _, fd := range r.path[:len(r.path)-1] {
		msg = msg.Get(fd).Message()
	}
	leaf := r.path[len(r.path)-1]
	val := msg.Get(leaf)
	switch r.op {
	case "eq":
		return r.lit.equal(val)
	case "ne":
		return !r.lit.equal(val)
	case "in":
		for _, l := range r.list {
			if l.equal(val) {
				return true
			}
		}
		return false
	case "matches":
		return r.re.MatchString(val.String())
	case "contains":
		list := val.List()
		for i := 0; i < list.Len(); i++ {
			if r.lit.equal(list.Get(i)) {
				return true
			}
		}
		return false
	}
	return false
}

func (r msgRule) hasPath(root protoreflect.Message) bool {
	msg := root
	for _, fd := range r.path[:len(r.path)-1] {
		if !msg.Has(fd) {
			return false
		}
		msg = msg.Get(fd).Message()
	}
	return msg.Has(r.path[len(r.path)-1])
}
