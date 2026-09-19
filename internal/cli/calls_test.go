package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func TestCallsListText(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("calls list: %v", err)
	}
	for _, want := range []string{"SEQ", "METHOD", "CODE", "DURATION", "STUB",
		"/shop.v1.OrderService/GetOrder"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestCallsListEmptyReportsToStderr(t *testing.T) {
	srv := startCommandServer(t)
	stdout, stderr, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("calls list: %v", err)
	}
	if !strings.Contains(stderr, "no calls") {
		t.Errorf("stderr = %q, want a no-calls note", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
}

func TestCallsListNewestFirst(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")
	recordDataPlaneCall(t, srv, "o-2")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("calls list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	calls, _ := fields["calls"].([]any)
	if len(calls) < 2 {
		t.Fatalf("got %d calls, want at least 2: %s", len(calls), stdout)
	}
	first, _ := calls[0].(map[string]any)
	second, _ := calls[1].(map[string]any)
	// seq is a uint64 and therefore a JSON string; newest first means
	// descending.
	if first["seq"].(string) <= second["seq"].(string) {
		t.Errorf("calls are not newest-first: %v then %v", first["seq"], second["seq"])
	}
}

func TestCallsListLimit(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")
	recordDataPlaneCall(t, srv, "o-2")
	recordDataPlaneCall(t, srv, "o-3")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv),
		"--limit", "1", "--output", "json")
	if err != nil {
		t.Fatalf("calls list --limit: %v", err)
	}
	fields := decodeJSON(t, stdout)
	calls, _ := fields["calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %s", len(calls), stdout)
	}
}

func TestCallsListMethodFilter(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, stderr, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/WatchOrder")
	if err != nil {
		t.Fatalf("calls list --method: %v", err)
	}
	if strings.Contains(stdout, "GetOrder") {
		t.Errorf("a GetOrder call survived a WatchOrder filter:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no calls") {
		t.Errorf("stderr = %q, want a no-calls note", stderr)
	}
}

// The decoded request payload is reachable in JSON as an escaped string, which
// is the contract shape (design §4).
func TestCallsListJSONCarriesTheDecodedRequest(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-42")

	stdout, _, err := runCmd(t, newCallsCmd(), "list", "--addr", adminAddr(srv), "--output", "json")
	if err != nil {
		t.Fatalf("calls list --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	calls, _ := fields["calls"].([]any)
	first, _ := calls[0].(map[string]any)
	requests, _ := first["requests"].([]any)
	if len(requests) == 0 {
		t.Fatalf("no requests recorded: %s", stdout)
	}
	req, _ := requests[0].(map[string]any)
	body, _ := req["json"].(string)
	if !strings.Contains(body, "o-42") {
		t.Fatalf("decoded request = %q, want the order id", body)
	}
	// Proto field names, not camelCase.
	if _, ok := req["type_name"]; !ok {
		t.Errorf("type_name absent (camelCase leaked?): %s", stdout)
	}
}

// fakeCallStream replays a scripted stream: a run of calls, then an ending.
type fakeCallStream struct {
	calls []*adminv1.Call
	idx   int
	err   error
	msg   *adminv1.WatchCallsResponse
}

func (f *fakeCallStream) Receive() bool {
	if f.idx >= len(f.calls) {
		return false
	}
	f.msg = &adminv1.WatchCallsResponse{Call: f.calls[f.idx]}
	f.idx++
	return true
}
func (f *fakeCallStream) Msg() *adminv1.WatchCallsResponse { return f.msg }
func (f *fakeCallStream) Err() error                       { return f.err }
func (f *fakeCallStream) Close() error                     { return nil }

func call(seq uint64) *adminv1.Call {
	return &adminv1.Call{Seq: seq, Method: "/shop.v1.OrderService/GetOrder"}
}

// Eviction is the one ending that resumes: the CLI warns, reconnects, and
// says plainly that the calls in the gap are lost (design §6.2).
func TestTailLoopResumesAfterEviction(t *testing.T) {
	streams := []*fakeCallStream{
		{calls: []*adminv1.Call{call(1)}, err: connect.NewError(connect.CodeResourceExhausted, errors.New("slow consumer"))},
		{calls: []*adminv1.Call{call(2)}, err: connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))},
	}
	opened := 0
	open := func(context.Context) (callStream, error) {
		s := streams[opened]
		opened++
		return s, nil
	}
	var got []uint64
	var warnings []string
	err := tailLoop(context.Background(), open,
		func(c *adminv1.Call) error { got = append(got, c.GetSeq()); return nil },
		func(msg string) { warnings = append(warnings, msg) })

	if opened != 2 {
		t.Fatalf("opened %d streams, want 2 — eviction must reconnect", opened)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("delivered %v, want [1 2]", got)
	}
	if len(warnings) == 0 {
		t.Fatal("eviction produced no warning")
	}
	if !strings.Contains(strings.Join(warnings, " "), "lost") {
		t.Errorf("warning %q does not say calls in the gap are lost", warnings)
	}
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("final error = %v, want the unavailable ending to surface", err)
	}
}

// UNAVAILABLE is terminal and is NOT distinguished by message text: a dial
// failure produces the same code, and sniffing the server's wording would
// couple the CLI to it (design §6.2).
func TestTailLoopTreatsUnavailableAsTerminal(t *testing.T) {
	opened := 0
	open := func(context.Context) (callStream, error) {
		opened++
		return &fakeCallStream{err: connect.NewError(connect.CodeUnavailable, errors.New("server shutting down"))}, nil
	}
	err := tailLoop(context.Background(), open, func(*adminv1.Call) error { return nil }, func(string) {})
	if opened != 1 {
		t.Fatalf("opened %d streams, want 1 — unavailable must not reconnect", opened)
	}
	if err == nil {
		t.Fatal("unavailable did not surface as an error")
	}
}

func TestTailLoopStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opened := 0
	open := func(context.Context) (callStream, error) {
		opened++
		return &fakeCallStream{}, nil
	}
	if err := tailLoop(ctx, open, func(*adminv1.Call) error { return nil }, func(string) {}); err != nil {
		t.Fatalf("tailLoop on a cancelled context = %v, want nil", err)
	}
	if opened != 0 {
		t.Fatalf("opened %d streams on a cancelled context, want 0", opened)
	}
}

func TestTailLoopSurfacesAnOpenFailure(t *testing.T) {
	open := func(context.Context) (callStream, error) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	}
	if err := tailLoop(context.Background(), open, func(*adminv1.Call) error { return nil }, func(string) {}); err == nil {
		t.Fatal("an open failure did not surface")
	}
}

// End to end against a real server: a recorded call reaches the tail.
func TestCallsTailDeliversARecordedCall(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newCallsCmd()
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"tail", "--addr", adminAddr(srv), "--output", "json"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	// Record until the tail has picked one up, then stop it.
	deadline := time.After(20 * time.Second)
	for !strings.Contains(stdout.String(), "GetOrder") {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("tail delivered nothing; stdout=%q stderr=%q", stdout.String(), stderr.String())
		default:
		}
		recordDataPlaneCall(t, srv, "o-tail")
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	line := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)[0]
	fields := decodeJSON(t, line)
	if _, ok := fields["call"]; !ok {
		t.Fatalf("first line is not a WatchCallsResponse: %q", line)
	}
}

// Teardown ends the stream with UNAVAILABLE, which is terminal.
func TestCallsTailEndsWhenTheServerStops(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newCallsCmd()
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"tail", "--addr", adminAddr(srv)})

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	srv.Stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("tail returned nil when the server tore down; teardown is not a clean end")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("tail did not return after the server stopped")
	}
}

func TestCallsTailHasNoTimeoutFlag(t *testing.T) {
	cmd := newCallsCmd()
	for _, sub := range cmd.Commands() {
		if sub.Name() != "tail" {
			continue
		}
		if sub.Flags().Lookup("timeout") != nil {
			t.Fatal("calls tail registered --timeout; its stream is unbounded by design")
		}
		if sub.Flags().Lookup("addr") == nil {
			t.Fatal("calls tail is missing --addr")
		}
		return
	}
	t.Fatal("calls tail is not registered")
}
