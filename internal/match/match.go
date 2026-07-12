// Package match compiles the structured `match:` block of a stub against a
// message descriptor and evaluates decoded requests against it. Compilation
// validates every field path, operator, and literal, so bad stubs fail at
// load time — never silently at request time.
package match

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type Shape int

const (
	Unary Shape = iota
	ServerStream
	ClientStream
	Bidi
	BidiRule
)

func (s Shape) String() string {
	switch s {
	case Unary:
		return "unary"
	case ServerStream:
		return "server-streaming"
	case ClientStream:
		return "client-streaming"
	case Bidi:
		return "bidirectional"
	case BidiRule:
		return "bidirectional rule"
	}
	return "unknown"
}

func ShapeOf(m protoreflect.MethodDescriptor) Shape {
	switch {
	case m.IsStreamingClient() && m.IsStreamingServer():
		return Bidi
	case m.IsStreamingClient():
		return ClientStream
	case m.IsStreamingServer():
		return ServerStream
	default:
		return Unary
	}
}

type Input struct {
	Method   string
	Metadata metadata.MD
	Message  protoreflect.Message
	Messages []protoreflect.Message
	Now      time.Time
}

type Compiler struct {
	files *protoregistry.Files
	mu    sync.Mutex
	envs  map[envKey]*cel.Env
}

type envKey struct {
	msg   protoreflect.FullName
	shape Shape
}

func NewCompiler(files *protoregistry.Files) *Compiler { return &Compiler{files: files} }

// Block is the YAML shape of a stub's `match:` section (M1 structured subset).
type Block struct {
	Metadata map[string]Rules `yaml:"metadata"`
	Message  map[string]Rules `yaml:"message"`
	Expr     string           `yaml:"expr"`
}

// Rules maps an operator name (eq, ne, in, matches, present, contains) to its literal.
type Rules map[string]any

type Compiled struct {
	metadata []mdRule
	message  []msgRule
	expr     *exprRule
}

type exprRule struct {
	prg     cel.Program
	src     string
	adapter types.Adapter
}

func (c *Compiler) Env(input protoreflect.MessageDescriptor, shape Shape) (*cel.Env, error) {
	if c.files == nil {
		return nil, fmt.Errorf("CEL is unavailable: match compiler was built without descriptor files")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.envs == nil {
		c.envs = map[envKey]*cel.Env{}
	}
	key := envKey{input.FullName(), shape}
	if env, ok := c.envs[key]; ok {
		return env, nil
	}
	provider, err := newCELProvider(c.files)
	if err != nil {
		return nil, fmt.Errorf("building CEL type provider for %s: %w", input.FullName(), err)
	}
	adapter := newProtoAdapter(provider, c.files)
	obj := cel.ObjectType(string(input.FullName()))
	opts := []cel.EnvOption{
		cel.CustomTypeProvider(provider),
		cel.CustomTypeAdapter(adapter),
		cel.Variable("metadata", cel.MapType(cel.StringType, cel.ListType(cel.StringType))),
		cel.Variable("method", cel.StringType),
		cel.Variable("now", cel.TimestampType),
	}
	switch shape {
	case Unary, ServerStream:
		opts = append(opts, cel.Variable("message", obj))
	case ClientStream:
		opts = append(opts, cel.Variable("messages", cel.ListType(obj)))
	case BidiRule:
		opts = append(opts, cel.Variable("message", obj), cel.Variable("messages", cel.ListType(obj)))
	case Bidi:
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("building CEL environment for %s: %w", input.FullName(), err)
	}
	c.envs[key] = env
	return env, nil
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
	src  string
	str  string
	b    bool
	i    int64
	u    uint64
	f    float64
	enum protoreflect.EnumNumber
}

// Compile validates the block against the request descriptor. A nil block
// compiles to a matcher that accepts everything.
func (c *Compiler) Compile(input protoreflect.MessageDescriptor, b *Block, shape Shape) (*Compiled, error) {
	cmp := &Compiled{}
	if b == nil {
		return cmp, nil
	}
	if len(b.Message) > 0 && (shape == ClientStream || shape == Bidi) {
		return nil, fmt.Errorf("message field matchers are not valid for %s stubs (there is no single request message); use expr over `messages` instead", shape)
	}
	for _, key := range sortedKeys(b.Metadata) {
		rules := b.Metadata[key]
		for _, op := range sortedKeys(rules) {
			raw := rules[op]
			r, err := compileMDRule(strings.ToLower(key), op, raw)
			if err != nil {
				return nil, fmt.Errorf("metadata %q: %w", key, err)
			}
			cmp.metadata = append(cmp.metadata, r)
		}
	}
	for _, path := range sortedKeys(b.Message) {
		rules := b.Message[path]
		fds, err := resolvePath(input, path)
		if err != nil {
			return nil, err
		}
		for _, op := range sortedKeys(rules) {
			raw := rules[op]
			r, err := compileMsgRule(fds, op, raw)
			if err != nil {
				return nil, fmt.Errorf("message field %q: %w", path, err)
			}
			cmp.message = append(cmp.message, r)
		}
	}
	if b.Expr != "" {
		env, err := c.Env(input, shape)
		if err != nil {
			return nil, err
		}
		ast, iss := env.Compile(b.Expr)
		if iss.Err() != nil {
			return nil, fmt.Errorf("expr: %w", iss.Err())
		}
		if !ast.OutputType().IsExactType(cel.BoolType) {
			return nil, fmt.Errorf("expr must evaluate to a bool, got %s: %s", ast.OutputType(), b.Expr)
		}
		prg, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("expr: %w", err)
		}
		cmp.expr = &exprRule{prg: prg, src: b.Expr, adapter: env.CELTypeAdapter()}
	}
	return cmp, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (c *Compiled) Eval(in Input) bool {
	for _, r := range c.metadata {
		if !r.eval(in.Metadata) {
			return false
		}
	}
	if len(c.message) > 0 && in.Message == nil {
		return false
	}
	for _, r := range c.message {
		if !r.eval(in.Message) {
			return false
		}
	}
	if c.expr != nil {
		out, _, err := c.expr.prg.Eval(activation(in, c.expr.adapter))
		if err != nil || out != types.True {
			return false
		}
	}
	return true
}

func Activation(in Input) map[string]any {
	return activation(in, nil)
}

func activation(in Input, adapter types.Adapter) map[string]any {
	md := map[string][]string{}
	for k, v := range in.Metadata {
		md[k] = v
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	act := map[string]any{"metadata": md, "method": in.Method, "now": now}
	if in.Message != nil {
		message := any(in.Message.Interface())
		if adapter != nil {
			message = adapter.NativeToValue(message)
		}
		act["message"] = message
	}
	if in.Messages != nil {
		msgs := make([]any, len(in.Messages))
		for i, m := range in.Messages {
			msgs[i] = m.Interface()
			if adapter != nil {
				msgs[i] = adapter.NativeToValue(msgs[i])
			}
		}
		act["messages"] = msgs
	}
	return act
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
	l := literal{kind: k, src: fmt.Sprintf("%v", raw)}
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
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		i, err := intLiteral(fd, raw, math.MinInt32, math.MaxInt32)
		if err != nil {
			return l, err
		}
		l.i = i
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		i, err := intLiteral(fd, raw, math.MinInt64, math.MaxInt64)
		if err != nil {
			return l, err
		}
		l.i = i
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		u, err := uintLiteral(fd, raw, math.MaxUint32)
		if err != nil {
			return l, err
		}
		l.u = u
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		u, err := uintLiteral(fd, raw, math.MaxUint64)
		if err != nil {
			return l, err
		}
		l.u = u
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		var f float64
		switch v := raw.(type) {
		case float64:
			f = v
		case int:
			f = float64(v)
		case int64:
			f = float64(v)
		case uint64:
			f = float64(v)
		default:
			return l, fmt.Errorf("field %q is a float, got %T literal", fd.Name(), raw)
		}
		if k == protoreflect.FloatKind {
			// The wire value is a float32; protoreflect widens it back to
			// float64. Normalize the literal the same way or 0.1 would
			// never equal the float32 0.1 a client actually sent.
			f32 := float64(float32(f))
			if math.IsInf(f32, 0) && !math.IsInf(f, 0) {
				return l, fmt.Errorf("literal %v overflows float (float32) field %q; use .inf to match infinity", f, fd.Name())
			}
			f = f32
		}
		l.f = f
	case protoreflect.EnumKind:
		switch v := raw.(type) {
		case string:
			ev := fd.Enum().Values().ByName(protoreflect.Name(v))
			if ev == nil {
				return l, fmt.Errorf("enum %s has no value named %q", fd.Enum().FullName(), v)
			}
			l.enum = ev.Number()
		case int, int64, uint64:
			// EnumNumber is int32-backed; range-check like any int32 so
			// out-of-range numbers fail instead of wrapping. Proto3 enums
			// are open, so any in-range number stays matchable.
			n, err := intLiteral(fd, v, math.MinInt32, math.MaxInt32)
			if err != nil {
				return l, err
			}
			l.enum = protoreflect.EnumNumber(n)
		default:
			return l, fmt.Errorf("field %q is an enum, got %T literal", fd.Name(), raw)
		}
	default:
		return l, fmt.Errorf("field %q has kind %s, which structured matchers do not support yet (bytes/message/map matching arrives with CEL in M2)", fd.Name(), k)
	}
	return l, nil
}

// intLiteral converts a YAML numeric literal for a signed integer field,
// rejecting values outside [min, max]. yaml.v3 yields int for most values
// and uint64 for values above MaxInt64.
func intLiteral(fd protoreflect.FieldDescriptor, raw any, min, max int64) (int64, error) {
	var i int64
	switch v := raw.(type) {
	case int:
		i = int64(v)
	case int64:
		i = v
	case uint64:
		if v > math.MaxInt64 {
			return 0, fmt.Errorf("literal %d overflows %s field %q", v, fd.Kind(), fd.Name())
		}
		i = int64(v)
	default:
		return 0, fmt.Errorf("field %q is an integer, got %T literal", fd.Name(), raw)
	}
	if i < min || i > max {
		return 0, fmt.Errorf("literal %d is out of range for %s field %q [%d, %d]", i, fd.Kind(), fd.Name(), min, max)
	}
	return i, nil
}

// uintLiteral converts a YAML numeric literal for an unsigned integer field,
// rejecting negatives and values above max.
func uintLiteral(fd protoreflect.FieldDescriptor, raw any, max uint64) (uint64, error) {
	var u uint64
	switch v := raw.(type) {
	case int:
		if v < 0 {
			return 0, fmt.Errorf("field %q is unsigned, got negative literal %d", fd.Name(), v)
		}
		u = uint64(v)
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("field %q is unsigned, got negative literal %d", fd.Name(), v)
		}
		u = uint64(v)
	case uint64:
		u = v
	default:
		return 0, fmt.Errorf("field %q is an unsigned integer, got %T literal", fd.Name(), raw)
	}
	if u > max {
		return 0, fmt.Errorf("literal %d is out of range for %s field %q (max %d)", u, fd.Kind(), fd.Name(), max)
	}
	return u, nil
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
