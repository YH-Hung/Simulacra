package journal

import (
	"fmt"
	"strings"
	"time"

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

// snapshotTimes makes verification independent of subsequent caller changes
// to threshold values. Concurrent mutation while this copy is being made is
// still a caller data race and must be synchronized by the caller.
func snapshotTimes(times Times) Times {
	snapshot := Times{Never: times.Never}
	if times.Exactly != nil {
		value := *times.Exactly
		snapshot.Exactly = &value
	}
	if times.AtLeast != nil {
		value := *times.AtLeast
		snapshot.AtLeast = &value
	}
	if times.AtMost != nil {
		value := *times.AtMost
		snapshot.AtMost = &value
	}
	return snapshot
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

func (t Times) under(count int) bool {
	switch {
	case t.Exactly != nil:
		return count < *t.Exactly
	case t.AtLeast != nil:
		return count < *t.AtLeast
	default:
		return false
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
	Pass              bool
	Matched           int
	Considered        int
	Want              string
	Misses            []Miss
	UnexpectedMatches []uint64
}

// Verify checks calls for method against matcher and the requested count.
// The journal's List snapshot keeps evaluation isolated from concurrent writes
// and from mutation of retained messages.
func Verify(j *Journal, method string, matcher *match.Compiled, times Times) (Report, error) {
	times = snapshotTimes(times)
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
	var matchedSeqs []uint64
	for _, call := range j.List() {
		if call == nil || (wantMethod != "" && call.Method != wantMethod) {
			continue
		}
		report.Considered++
		input, err := callInput(call)
		if err != nil {
			return Report{}, err
		}
		if matcher == nil || matcher.Eval(input) {
			report.Matched++
			matchedSeqs = append(matchedSeqs, call.Seq)
			continue
		}
		mismatches = append(mismatches, mismatch{seq: call.Seq, input: input})
	}
	report.Pass = times.ok(report.Matched)
	if report.Pass {
		return report, nil
	}
	if !times.under(report.Matched) {
		report.UnexpectedMatches = matchedSeqs
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

func callInput(call *Call) (match.Input, error) {
	if call == nil {
		return match.Input{}, fmt.Errorf("call is nil")
	}
	md := metadata.MD{}
	for key, values := range call.Metadata {
		key = strings.ToLower(key)
		md[key] = append(md[key], values...)
	}
	messages := make([]protoreflect.Message, 0, len(call.Requests))
	for index, request := range call.Requests {
		if request == nil {
			return match.Input{}, fmt.Errorf("call %d request %d is nil", call.Seq, index)
		}
		messages = append(messages, request.ProtoReflect())
	}
	var message protoreflect.Message
	if len(call.Requests) > 0 {
		message = call.Requests[0].ProtoReflect()
	}
	now := call.Start
	if now.IsZero() {
		now = time.Now()
	}
	return match.Input{
		Method:   strings.TrimPrefix(normalizeMethod(call.Method), "/"),
		Metadata: md,
		Message:  message,
		Messages: messages,
		Now:      now,
	}, nil
}
