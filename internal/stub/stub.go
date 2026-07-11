// Package stub defines the YAML stub format (M1 subset), compiles stubs
// against the schema registry, and selects them at request time.
package stub

import (
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
	Metadata map[string]string `yaml:"metadata"`
	Trailers map[string]string `yaml:"trailers"`
	Delay    string            `yaml:"delay"`
	Message  map[string]any    `yaml:"message"`
	Status   *StatusSpec       `yaml:"status"`
	Stream   []StepSpec        `yaml:"stream"`
	OnOpen   []StepSpec        `yaml:"on_open"`
	Rules    []RuleSpec        `yaml:"rules"`
	OnClose  *CloseSpec        `yaml:"on_close"`
}

type StepSpec struct {
	Delay   string         `yaml:"delay"`
	Message map[string]any `yaml:"message"`
	Status  *StatusSpec    `yaml:"status"`
}

type RuleSpec struct {
	Match *match.Block `yaml:"match"`
	Send  []StepSpec   `yaml:"send"`
}

type CloseSpec struct {
	Status *StatusSpec `yaml:"status"`
}

type Plan struct {
	Header  metadata.MD
	Trailer metadata.MD
	Delay   *Delay
	Message *Template
	Status  *status.Status
	Stream  []Step
	OnOpen  []Step
	Rules   []Rule
	OnClose *status.Status
}

type Step struct {
	Delay   *Delay
	Message *Template
	Status  *status.Status
}

type Rule struct {
	Matcher *match.Compiled
	Send    []Step
}

// Compiled is a stub validated against the schema. Source identifies where it
// came from ("path/to/file.yaml#index") for error messages.
type Compiled struct {
	Method   string // normalized "/pkg.Service/Method"
	Shape    match.Shape
	Priority int
	Times    int
	Source   string
	matcher  *match.Compiled
	plan     *Plan
}

func (c *Compiled) Matches(in match.Input) bool {
	return c.matcher.Eval(in)
}

// Explain returns every matcher clause that rejects the input.
func (c *Compiled) Explain(in match.Input) []string {
	return c.matcher.Explain(in)
}

func (c *Compiled) Plan() *Plan { return c.plan }

type Compiler struct {
	reg     *schema.Registry
	matcher *match.Compiler
	types   *schema.Types
}

func NewCompiler(reg *schema.Registry) *Compiler {
	return &Compiler{reg: reg, matcher: match.NewCompiler(reg.Files()), types: reg.Types()}
}

func Compile(reg *schema.Registry, s Stub, source string) (*Compiled, error) {
	return NewCompiler(reg).Compile(s, source)
}

func (c *Compiler) Compile(s Stub, source string) (*Compiled, error) {
	if s.Times < 0 {
		return nil, fmt.Errorf("%s: times must not be negative (got %d); omit it or use 0 for unlimited", source, s.Times)
	}
	m, err := c.reg.LookupMethod(s.Method)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	shape := match.ShapeOf(m)
	cm, err := c.matcher.Compile(m.Input(), s.Match, shape)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	plan, err := c.compilePlan(m, shape, s.Respond, source)
	if err != nil {
		return nil, err
	}
	return &Compiled{
		Method:   fmt.Sprintf("/%s/%s", m.Parent().FullName(), m.Name()),
		Shape:    shape,
		Priority: s.Priority,
		Times:    s.Times,
		Source:   source,
		matcher:  cm,
		plan:     plan,
	}, nil
}

func (c *Compiler) compilePlan(method protoreflect.MethodDescriptor, shape match.Shape, spec Respond, source string) (*Plan, error) {
	plan := &Plan{Header: metadata.New(spec.Metadata), Trailer: metadata.New(spec.Trailers)}
	var err error
	if spec.Delay != "" {
		plan.Delay, err = ParseDelay(spec.Delay)
		if err != nil {
			return nil, fmt.Errorf("%s: respond.delay: %w", source, err)
		}
	}

	switch shape {
	case match.Unary, match.ClientStream:
		if spec.Stream != nil || spec.OnOpen != nil || spec.Rules != nil || spec.OnClose != nil {
			return nil, fmt.Errorf("%s: respond: stream, on_open, rules, and on_close are not valid for %s methods", source, shape)
		}
		if spec.Message != nil && spec.Status != nil {
			return nil, fmt.Errorf("%s: respond: message and status are mutually exclusive", source)
		}
		if spec.Status != nil {
			plan.Status, err = compileStatus(c.reg, spec.Status, false)
			if err != nil {
				return nil, fmt.Errorf("%s: respond.status: %w", source, err)
			}
			return plan, nil
		}
		plan.Message, err = c.compileTemplate(method, shape, spec.Message, source+": respond.message")
		if err != nil {
			return nil, err
		}
		return plan, nil

	case match.ServerStream:
		if spec.Message != nil || spec.Status != nil || spec.Delay != "" || spec.OnOpen != nil || spec.Rules != nil || spec.OnClose != nil {
			return nil, fmt.Errorf("%s: respond: message, status, delay, on_open, rules, and on_close are not valid for server-streaming methods; use stream", source)
		}
		if len(spec.Stream) == 0 {
			return nil, fmt.Errorf("%s: respond.stream: server-streaming methods require a non-empty stream script", source)
		}
		plan.Stream, err = c.compileSteps(method, shape, spec.Stream, source+": respond.stream")
		if err != nil {
			return nil, err
		}
		return plan, nil

	case match.Bidi:
		if spec.Message != nil || spec.Status != nil || spec.Stream != nil || spec.Delay != "" {
			return nil, fmt.Errorf("%s: respond: top-level message, status, stream, and delay are not valid for bidirectional methods", source)
		}
		if len(spec.OnOpen) == 0 && len(spec.Rules) == 0 {
			return nil, fmt.Errorf("%s: respond: bidirectional methods require rules and/or on_open", source)
		}
		if len(spec.OnOpen) > 0 {
			plan.OnOpen, err = c.compileSteps(method, match.Bidi, spec.OnOpen, source+": respond.on_open")
			if err != nil {
				return nil, err
			}
		}
		for i, ruleSpec := range spec.Rules {
			where := fmt.Sprintf("%s: respond.rules[%d]", source, i)
			if len(ruleSpec.Send) == 0 {
				return nil, fmt.Errorf("%s.send: must contain at least one step", where)
			}
			matcher, compileErr := c.matcher.Compile(method.Input(), ruleSpec.Match, match.BidiRule)
			if compileErr != nil {
				return nil, fmt.Errorf("%s.match: %w", where, compileErr)
			}
			send, compileErr := c.compileSteps(method, match.BidiRule, ruleSpec.Send, where+".send")
			if compileErr != nil {
				return nil, compileErr
			}
			plan.Rules = append(plan.Rules, Rule{Matcher: matcher, Send: send})
		}
		if spec.OnClose == nil {
			plan.OnClose = status.New(codes.OK, "")
		} else {
			if spec.OnClose.Status == nil {
				return nil, fmt.Errorf("%s: respond.on_close.status is required", source)
			}
			plan.OnClose, err = compileStatus(c.reg, spec.OnClose.Status, true)
			if err != nil {
				return nil, fmt.Errorf("%s: respond.on_close.status: %w", source, err)
			}
		}
		return plan, nil
	}
	return nil, fmt.Errorf("%s: unsupported method shape %v", source, shape)
}

func (c *Compiler) compileSteps(method protoreflect.MethodDescriptor, shape match.Shape, specs []StepSpec, where string) ([]Step, error) {
	steps := make([]Step, len(specs))
	for i, spec := range specs {
		stepWhere := fmt.Sprintf("%s[%d]", where, i)
		if (spec.Message == nil) == (spec.Status == nil) {
			return nil, fmt.Errorf("%s: step must specify exactly one of message or status", stepWhere)
		}
		if spec.Delay != "" {
			delay, err := ParseDelay(spec.Delay)
			if err != nil {
				return nil, fmt.Errorf("%s.delay: %w", stepWhere, err)
			}
			steps[i].Delay = delay
		}
		if spec.Status != nil {
			if i != len(specs)-1 {
				return nil, fmt.Errorf("%s.status: terminal status must be the last step", stepWhere)
			}
			compiled, err := compileStatus(c.reg, spec.Status, false)
			if err != nil {
				return nil, fmt.Errorf("%s.status: %w", stepWhere, err)
			}
			steps[i].Status = compiled
			continue
		}
		tmpl, err := c.compileTemplate(method, shape, spec.Message, stepWhere+".message")
		if err != nil {
			return nil, err
		}
		steps[i].Message = tmpl
	}
	return steps, nil
}

func (c *Compiler) compileTemplate(method protoreflect.MethodDescriptor, shape match.Shape, fields map[string]any, source string) (*Template, error) {
	if !hasSites(fields) {
		return newTemplate(nil, c.types, method.Output(), fields, source)
	}
	celEnv, err := c.matcher.Env(method.Input(), shape)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return newTemplate(celEnv, c.types, method.Output(), fields, source)
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
