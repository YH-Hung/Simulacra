package match

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/common/types"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Explain evaluates every matcher clause and returns one human-readable
// reason for each failure. It is intentionally the slow diagnostic path;
// request selection should continue to use Eval.
func (c *Compiled) Explain(in Input) []string {
	var reasons []string
	for _, r := range c.metadata {
		if !r.eval(in.Metadata) {
			reasons = append(reasons, r.describe(in.Metadata))
		}
	}
	for _, r := range c.message {
		if in.Message == nil {
			reasons = append(reasons, fmt.Sprintf("message %s: no request message", joinPath(r.path)))
			continue
		}
		if !r.eval(in.Message) {
			reasons = append(reasons, r.describe(in.Message))
		}
	}
	if c.expr != nil {
		out, _, err := c.expr.prg.Eval(Activation(in))
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("expr errored: %v (%s)", err, c.expr.src))
		case out != types.True:
			reasons = append(reasons, "expr is false: "+c.expr.src)
		}
	}
	return reasons
}

func (r mdRule) describe(md metadata.MD) string {
	vals := md.Get(r.key)
	actual := "key absent"
	if len(vals) > 0 {
		actual = "actual " + formatStrings(vals)
	}

	var want string
	switch r.op {
	case "present":
		if r.want {
			want = "to be present"
		} else {
			want = "to be absent"
		}
	case "eq":
		want = fmt.Sprintf("to contain %q", r.str)
	case "ne":
		want = fmt.Sprintf("not to contain %q", r.str)
	case "in":
		want = "to contain one of " + formatStrings(r.list)
	case "matches":
		want = fmt.Sprintf("to contain a value matching %q", r.re.String())
	}
	return fmt.Sprintf("metadata %s: expected %s; %s", r.key, want, actual)
}

func formatStrings(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func (r msgRule) wantDesc() string {
	switch r.op {
	case "present":
		if r.want {
			return "to be set"
		}
		return "to be unset"
	case "eq":
		return "to equal " + formatLiteral(r.lit)
	case "ne":
		return "not to equal " + formatLiteral(r.lit)
	case "in":
		items := make([]string, len(r.list))
		for i, lit := range r.list {
			items[i] = formatLiteral(lit)
		}
		return "to be one of [" + strings.Join(items, ", ") + "]"
	case "matches":
		return fmt.Sprintf("to match %q", r.re.String())
	case "contains":
		return "to contain " + formatLiteral(r.lit)
	default:
		return r.op
	}
}

func (r msgRule) describe(root protoreflect.Message) string {
	path := joinPath(r.path)
	if r.op == "present" {
		state := "unset"
		if r.hasPath(root) {
			state = "set"
		}
		return fmt.Sprintf("message %s: expected %s; actual %s", path, r.wantDesc(), state)
	}

	msg := root
	for _, fd := range r.path[:len(r.path)-1] {
		msg = msg.Get(fd).Message()
	}
	leaf := r.path[len(r.path)-1]
	return fmt.Sprintf("message %s: expected %s; actual %s", path, r.wantDesc(), formatValue(leaf, msg.Get(leaf)))
}

func joinPath(path []protoreflect.FieldDescriptor) string {
	parts := make([]string, len(path))
	for i, fd := range path {
		parts[i] = string(fd.Name())
	}
	return strings.Join(parts, ".")
}

func formatLiteral(lit literal) string {
	switch lit.kind {
	case protoreflect.StringKind:
		return fmt.Sprintf("%q", lit.src)
	default:
		return lit.src
	}
}

func formatValue(fd protoreflect.FieldDescriptor, value protoreflect.Value) string {
	if fd.IsList() {
		list := value.List()
		limit := list.Len()
		if limit > 5 {
			limit = 5
		}
		items := make([]string, 0, limit+1)
		for i := 0; i < limit; i++ {
			items = append(items, formatScalar(fd, list.Get(i)))
		}
		if list.Len() > limit {
			items = append(items, "…")
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	return formatScalar(fd, value)
}

func formatScalar(fd protoreflect.FieldDescriptor, value protoreflect.Value) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return fmt.Sprintf("%q", value.String())
	case protoreflect.BoolKind:
		return fmt.Sprintf("%t", value.Bool())
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return fmt.Sprintf("%d", value.Int())
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return fmt.Sprintf("%d", value.Uint())
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return fmt.Sprintf("%v", value.Float())
	case protoreflect.EnumKind:
		number := value.Enum()
		if named := fd.Enum().Values().ByNumber(number); named != nil {
			return string(named.Name())
		}
		return fmt.Sprintf("%d", number)
	case protoreflect.BytesKind:
		return fmt.Sprintf("%d bytes", len(value.Bytes()))
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "message"
	default:
		return fmt.Sprintf("%v", value.Interface())
	}
}
