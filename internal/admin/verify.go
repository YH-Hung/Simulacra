package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// verifyService implements simulacra.admin.v1.VerifyService.
//
// It deliberately does not embed UnimplementedVerifyServiceHandler: an RPC
// added to the contract must fail the build here, not return Unimplemented at
// runtime.
type verifyService struct {
	deps Deps
}

// VerifyCalls asserts how many recorded calls to a method matched a matcher
// (design §4.5).
func (v *verifyService) VerifyCalls(
	_ context.Context,
	req *connect.Request[adminv1.VerifyCallsRequest],
) (*connect.Response[adminv1.VerifyCallsResponse], error) {
	method := normalizeMethod(req.Msg.GetMethod())
	if method == "" {
		return nil, connectError(invalidArgument(errors.New("method is required")))
	}
	times := journalTimes(req.Msg.GetTimes())
	if err := times.Validate(); err != nil {
		return nil, connectError(invalidArgument(fmt.Errorf("times: %w", err)))
	}
	block, err := stub.ParseMatchDocument([]byte(req.Msg.GetMatcherDocument()))
	if err != nil {
		return nil, connectError(invalidArgument(err))
	}
	// CompileMatch resolves the method even for a nil block: a mistyped method
	// asserted with never must be NOT_FOUND, not a silent pass.
	matcher, err := stub.NewCompiler(v.deps.Registry).CompileMatch(method, block)
	if err != nil {
		return nil, connectError(invalidArgument(err))
	}
	report, err := journal.Verify(v.deps.Journal, method, matcher, times)
	if err != nil {
		return nil, connectError(err)
	}

	types := v.deps.Registry.Types()
	resp := &adminv1.VerifyCallsResponse{
		Passed:      report.Pass,
		Matched:     int32(report.Matched),
		Explanation: validUTF8(explainVerdict(method, report)),
	}
	// Too few matches: the calls that missed, each with its reasons. Too many:
	// the calls that matched, with nothing to explain.
	for _, miss := range report.Misses {
		resp.Actual = append(resp.Actual, &adminv1.CallExplanation{
			Call:        renderCall(miss.Call, types),
			NearestMiss: validUTF8(strings.Join(miss.Reasons, "; ")),
		})
	}
	for _, call := range report.UnexpectedMatches {
		resp.Actual = append(resp.Actual, &adminv1.CallExplanation{Call: renderCall(call, types)})
	}
	return connect.NewResponse(resp), nil
}

// journalTimes converts the wire assertion. An absent Times converts to the
// zero value, which Validate rejects: there is no implied default.
func journalTimes(t *adminv1.Times) journal.Times {
	var times journal.Times
	if t == nil {
		return times
	}
	times.Never = t.GetNever()
	if t.Exactly != nil {
		n := int(*t.Exactly)
		times.Exactly = &n
	}
	if t.AtLeast != nil {
		n := int(*t.AtLeast)
		times.AtLeast = &n
	}
	if t.AtMost != nil {
		n := int(*t.AtMost)
		times.AtMost = &n
	}
	return times
}

// explainVerdict is VerifyCalls' one-line verdict. Its wording is not a
// contract.
func explainVerdict(method string, report journal.Report) string {
	verdict := "failed"
	if report.Pass {
		verdict = "passed"
	}
	return fmt.Sprintf("%s: %s matched %d of %d call(s) considered; want %s",
		verdict, method, report.Matched, report.Considered, report.Want)
}
