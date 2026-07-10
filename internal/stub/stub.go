// Package stub defines the YAML stub format (M1 subset), compiles stubs
// against the schema registry, and selects them at request time.
package stub

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// Stub is one entry in a stub YAML file (files hold a list of these).
type Stub struct {
	Method   string       `yaml:"method"`
	Match    *match.Block `yaml:"match"`
	Priority int          `yaml:"priority"`
	Times    int          `yaml:"times"` // 0 means unlimited
	Respond  Respond      `yaml:"respond"`
}

type Respond struct {
	Message map[string]any `yaml:"message"`
}

// Compiled is a stub validated against the schema: matcher compiled,
// response message pre-built. Source identifies where it came from
// ("path/to/file.yaml#index") for error messages.
type Compiled struct {
	Method   string // normalized "/pkg.Service/Method"
	Priority int
	Times    int
	Source   string
	matcher  *match.Compiled
	response *dynamicpb.Message
}

func (c *Compiled) Matches(in match.Input) bool {
	return c.matcher.Eval(in)
}

// Response returns the pre-built response message. It is shared across
// calls and must be treated as read-only.
func (c *Compiled) Response() *dynamicpb.Message { return c.response }

func Compile(reg *schema.Registry, s Stub, source string) (*Compiled, error) {
	if s.Times < 0 {
		return nil, fmt.Errorf("%s: times must not be negative (got %d); omit it or use 0 for unlimited", source, s.Times)
	}
	m, err := reg.LookupMethod(s.Method)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if m.IsStreamingClient() || m.IsStreamingServer() {
		return nil, fmt.Errorf("%s: %s is a streaming method; this build supports unary methods only", source, s.Method)
	}
	cm, err := match.NewCompiler(reg.Files()).Compile(m.Input(), s.Match, match.ShapeOf(m))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	resp, err := BuildMessage(reg.Types(), m.Output(), s.Respond.Message)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &Compiled{
		Method:   fmt.Sprintf("/%s/%s", m.Parent().FullName(), m.Name()),
		Priority: s.Priority,
		Times:    s.Times,
		Source:   source,
		matcher:  cm,
		response: resp,
	}, nil
}

// BuildMessage turns a stub's YAML message body into a dynamic protobuf
// message via the canonical protobuf-JSON mapping (protojson), so enum
// names, nested/repeated/map fields, 64-bit ints, and well-known types
// (e.g. Timestamp as RFC 3339 strings) all behave per spec.
func BuildMessage(types *schema.Types, desc protoreflect.MessageDescriptor, fields map[string]any) (*dynamicpb.Message, error) {
	if fields == nil {
		fields = map[string]any{}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encoding response message as JSON: %w", err)
	}
	msg := dynamicpb.NewMessage(desc)
	if err := (protojson.UnmarshalOptions{Resolver: types}).Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("response message does not fit %s: %w", desc.FullName(), err)
	}
	return msg, nil
}
