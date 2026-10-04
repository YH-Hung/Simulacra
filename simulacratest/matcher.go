package simulacratest

import (
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Matcher is one operator applied to a field or a metadata key in a stub's or
// a verification's match block. Build one with Eq, Ne, In, Matches, Present or
// Contains.
//
// A Matcher holds the caller's value untouched. Rendering it to the JSON the
// server's literal parser wants happens in matchSpec.add, where the field path
// is known, so an unsupported value can name the path it was given for.
type Matcher struct {
	op    string
	value any
}

// Eq matches a field equal to v.
func Eq(v any) Matcher { return Matcher{"eq", v} }

// Ne matches a field not equal to v.
func Ne(v any) Matcher { return Matcher{"ne", v} }

// In matches a field equal to any of vs.
func In(vs ...any) Matcher { return Matcher{"in", vs} }

// Matches matches a string field against the regular expression re.
func Matches(re string) Matcher { return Matcher{"matches", re} }

// Present matches on whether a field or metadata key is set (want true) or
// unset (want false).
func Present(want bool) Matcher { return Matcher{"present", want} }

// Contains matches a repeated field holding v.
func Contains(v any) Matcher { return Matcher{"contains", v} }

// render is the operator's JSON operand: the single value, or the list for In.
func (m Matcher) render() (any, error) {
	if m.op != "in" {
		return matcherValue(m.value)
	}
	vs := m.value.([]any)
	out := make([]any, len(vs))
	for i, v := range vs {
		r, err := matcherValue(v)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// matchSpec accumulates the `match:` block of a stub or a verification. The
// StubBuilder and the VerifyBuilder both embed it, so a matcher cannot mean one
// thing in a stub and another in a verify. The zero value is ready to use.
type matchSpec struct {
	message  map[string]map[string]any // field path -> operator -> operand
	metadata map[string]map[string]any // metadata key -> operator -> operand
	expr     string
	errs     []string
}

// add renders ms under key and records, rather than panics on, a value the
// server's parser cannot take: the builder reports every problem at once when
// the test sets the stub or runs the verification. kind is "MatchField" or
// "MatchMetadata" and names the method in the error.
func (m *matchSpec) add(kind, key string, ms []Matcher) {
	dst, noun := &m.message, "path"
	if kind == "MatchMetadata" {
		dst, noun = &m.metadata, "key"
	}
	if !yamlSafeString(key) {
		m.errs = append(m.errs, fmt.Sprintf("simulacratest: %s(%q): the %s"+notYAMLSafe, kind, key, noun))
		return
	}
	for _, mt := range ms {
		val, err := mt.render()
		if err != nil {
			m.errs = append(m.errs, fmt.Sprintf("simulacratest: %s(%q): %v", kind, key, err))
			continue
		}
		if _, dup := (*dst)[key][mt.op]; dup {
			m.errs = append(m.errs, fmt.Sprintf("simulacratest: %s(%q): operator %s given twice", kind, key, mt.op))
			continue
		}
		if *dst == nil {
			*dst = map[string]map[string]any{}
		}
		if (*dst)[key] == nil {
			(*dst)[key] = map[string]any{}
		}
		(*dst)[key][mt.op] = val
	}
}

// setExpr sets the CEL expression for what the structured matchers cannot say,
// or records it as refused when the document would alter it.
func (m *matchSpec) setExpr(cel string) {
	if !yamlSafeString(cel) {
		m.errs = append(m.errs, "simulacratest: Expr: the expression"+notYAMLSafe)
		return
	}
	m.expr = cel
}

// block is the `match:` document, or nil when nothing was set so the builder
// can omit the key entirely.
func (m *matchSpec) block() map[string]any {
	b := map[string]any{}
	if len(m.message) > 0 {
		b["message"] = m.message
	}
	if len(m.metadata) > 0 {
		b["metadata"] = m.metadata
	}
	if m.expr != "" {
		b["expr"] = m.expr
	}
	if len(b) == 0 {
		return nil
	}
	return b
}

// errors lists the values add refused, in the order they were given.
func (m *matchSpec) errors() []string { return m.errs }

var errNonFinite = errors.New("NaN and ±Inf cannot be expressed in JSON; use Expr")

// notYAMLSafe ends the refusal of a string the match block would carry
// altered: the server would match against, or parse, something else.
const notYAMLSafe = " does not survive the stub grammar's JSON round trip unchanged (invalid UTF-8 or a raw U+0085, for example); use MatchYAML or StubYAML for full control"

var errNotYAMLSafe = errors.New("value" + notYAMLSafe)

// matcherValue renders v as the JSON literal the server's matcher parser reads
// (internal/match literalFor). The Go kind decides the JSON type, because the
// parser is strict about it:
//   - Integers stay numbers, never strings: an integer field rejects a quoted
//     "123", and only int64/uint64 keep the extremes exact through json.Marshal.
//   - float32 widens to float64, so float32(0.1) renders 0.10000000149011612,
//     which the server narrows back with float64(float32(literal)) and finds
//     equal to what a client sent.
//   - An enum renders as its value name, readable in a failing verification;
//     a number with no name (an open proto3 enum) renders as that number,
//     which the parser accepts when in int32 range.
//
// Everything else — bytes, messages, maps, slices — has no literal form, and
// NaN and the infinities have no JSON form, so both are refused in favour of
// Expr rather than silently stringified. A string the document would alter
// (invalid UTF-8, a raw U+0085) is refused too, as add refuses such a path.
func matcherValue(v any) (any, error) {
	switch x := v.(type) {
	case string:
		if !yamlSafeString(x) {
			return nil, errNotYAMLSafe
		}
		return x, nil
	case bool:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return uint64(x), nil
	case uint8:
		return uint64(x), nil
	case uint16:
		return uint64(x), nil
	case uint32:
		return uint64(x), nil
	case uint64:
		return x, nil
	case float32:
		return finite(float64(x))
	case float64:
		return finite(x)
	case protoreflect.Enum:
		if ev := x.Descriptor().Values().ByNumber(x.Number()); ev != nil {
			return string(ev.Name()), nil
		}
		return int64(x.Number()), nil
	}
	return nil, fmt.Errorf("%T is not supported by structured matchers; use Expr", v)
}

func finite(f float64) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errNonFinite
	}
	return f, nil
}
