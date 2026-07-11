package journal

import (
	"fmt"
	"strings"

	"github.com/yinghanhung/simulacra/internal/match"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Times describes the allowed number of matching calls. Exactly is mutually
// exclusive with the range bounds, and Never is mutually exclusive with all
// numeric assertions.
type Times struct {
	Exactly *int
	AtLeast *int
	AtMost  *int
	Never   bool
}

// Validate rejects ambiguous or impossible count assertions.
func (t Times) Validate() error {
	if t.Never {
		if t.Exactly != nil || t.AtLeast != nil || t.AtMost != nil {
			return fmt.Errorf("never cannot be combined with another times assertion")
		}
		return nil
	}
	if t.Exactly != nil {
		if t.AtLeast != nil || t.AtMost != nil {
			return fmt.Errorf("exactly cannot be combined with at_least or at_most")
		}
		if *t.Exactly < 0 {
			return fmt.Errorf("exactly must not be negative")
		}
		return nil
	}
	if t.AtLeast == nil && t.AtMost == nil {
		return fmt.Errorf("one times assertion is required")
	}
	if t.AtLeast != nil && *t.AtLeast < 0 {
		return fmt.Errorf("at_least must not be negative")
	}
	if t.AtMost != nil && *t.AtMost < 0 {
		return fmt.Errorf("at_most must not be negative")
	}
	if t.AtLeast != nil && t.AtMost != nil && *t.AtLeast > *t.AtMost {
		return fmt.Errorf("at_least must not exceed at_most")
	}
	return nil
}

func (t Times) ok(count int) bool {
	switch {
	case t.Never:
		return count == 0
	case t.Exactly != nil:
		return count == *t.Exactly
	case t.AtLeast != nil && count < *t.AtLeast:
		return false
	case t.AtMost != nil && count > *t.AtMost:
		return false
	default:
		return t.AtLeast != nil || t.AtMost != nil
	}
}

// String returns a human-readable description of the assertion.
func (t Times) String() string {
	switch {
	case t.Never:
		return "never"
	case t.Exactly != nil:
		return fmt.Sprintf("exactly %d", *t.Exactly)
	case t.AtLeast != nil && t.AtMost != nil:
		return fmt.Sprintf("at least %d and at most %d", *t.AtLeast, *t.AtMost)
	case t.AtLeast != nil:
		return fmt.Sprintf("at least %d", *t.AtLeast)
	case t.AtMost != nil:
		return fmt.Sprintf("at most %d", *t.AtMost)
	default:
		return "invalid times assertion"
	}
}

// Miss explains why a considered call did not satisfy the matcher.
type Miss struct {
	Seq     uint64
	Reasons []string
}

// Report summarizes a journal verification.
type Report struct {
	Pass       bool
	Matched    int
	Considered int
	Want       string
	Misses     []Miss
}

// Verify checks calls for method against matcher and the requested count.
// The journal's List snapshot keeps evaluation isolated from concurrent writes
// and from mutation of retained messages.
func Verify(j *Journal, method string, matcher *match.Compiled, times Times) (Report, error) {
	if err := times.Validate(); err != nil {
		return Report{}, fmt.Errorf("invalid times: %w", err)
	}
	if j == nil {
		return Report{}, fmt.Errorf("journal is nil")
	}

	wantMethod := normalizeMethod(method)
	report := Report{Want: times.String()}
	type mismatch struct {
		seq   uint64
		input match.Input
	}
	var mismatches []mismatch
	for _, call := range j.List() {
		if call == nil || (wantMethod != "" && call.Method != wantMethod) {
			continue
		}
		report.Considered++
		input := callInput(call)
		if matcher == nil || matcher.Eval(input) {
			report.Matched++
			continue
		}
		mismatches = append(mismatches, mismatch{seq: call.Seq, input: input})
	}
	report.Pass = times.ok(report.Matched)
	if report.Pass {
		return report, nil
	}
	report.Misses = make([]Miss, 0, len(mismatches))
	for _, missed := range mismatches {
		report.Misses = append(report.Misses, Miss{
			Seq:     missed.seq,
			Reasons: matcher.Explain(missed.input),
		})
	}
	return report, nil
}

func callInput(call *Call) match.Input {
	md := metadata.MD{}
	for key, values := range call.Metadata {
		key = strings.ToLower(key)
		md[key] = append(md[key], values...)
	}
	messages := make([]protoreflect.Message, 0, len(call.Requests))
	for _, request := range call.Requests {
		if request != nil {
			messages = append(messages, request.ProtoReflect())
		}
	}
	var message protoreflect.Message
	if len(call.Requests) > 0 && call.Requests[0] != nil {
		message = call.Requests[0].ProtoReflect()
	}
	return match.Input{
		Method:   strings.TrimPrefix(normalizeMethod(call.Method), "/"),
		Metadata: md,
		Message:  message,
		Messages: messages,
		Now:      call.Start,
	}
}
