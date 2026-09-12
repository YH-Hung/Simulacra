package admin_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/journal"
)

func journalClient(t *testing.T, deps admin.Deps) adminv1connect.JournalServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewJournalServiceClient(ts.Client(), ts.URL)
}

// recordGetOrder records a GetOrder call carrying one request with orderID.
func recordGetOrder(t *testing.T, deps admin.Deps, orderID string) {
	t.Helper()
	req, _ := getOrderMessages(t, deps.Registry, orderID, "")
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder", Requests: []*dynamicpb.Message{req}})
}

func callSeqs(calls []*adminv1.Call) []uint64 {
	seqs := make([]uint64, len(calls))
	for i, call := range calls {
		seqs[i] = call.Seq
	}
	return seqs
}

func TestListCallsIsNewestFirstWithLimitAndMethod(t *testing.T) {
	deps := testDeps(t) // journal capacity 4: all four calls are retained
	recordGetOrder(t, deps, "o-1")
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/WatchOrder"})
	recordGetOrder(t, deps, "o-2")
	recordGetOrder(t, deps, "o-3")
	client := journalClient(t, deps)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *adminv1.ListCallsRequest
		want []uint64
	}{
		{"everything", &adminv1.ListCallsRequest{}, []uint64{4, 3, 2, 1}},
		{"limit", &adminv1.ListCallsRequest{Limit: 2}, []uint64{4, 3}},
		{"method without slash", &adminv1.ListCallsRequest{Method: "shop.v1.OrderService/GetOrder"}, []uint64{4, 3, 1}},
		{"method and limit", &adminv1.ListCallsRequest{Method: "/shop.v1.OrderService/GetOrder", Limit: 2}, []uint64{4, 3}},
	}
	for _, tc := range cases {
		resp, err := client.ListCalls(ctx, connect.NewRequest(tc.req))
		if err != nil {
			t.Fatalf("%s: ListCalls: %v", tc.name, err)
		}
		if got := callSeqs(resp.Msg.Calls); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: seqs = %v, want %v (newest first)", tc.name, got, tc.want)
		}
	}

	newest, err := client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{Limit: 1}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if orderID := jsonFields(t, newest.Msg.Calls[0].Requests[0].Json)["order_id"]; orderID != "o-3" {
		t.Errorf("newest call order_id = %v, want o-3; calls must be rendered, not just listed", orderID)
	}

	_, err = client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{Limit: -1}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("ListCalls with limit -1 = %v (code %v), want InvalidArgument", err, code)
	}
}

// One recorded call holding invalid UTF-8 must not make ListCalls fail for the
// whole journal (design §5.2).
func TestListCallsSurvivesInvalidUTF8InRecordedCalls(t *testing.T) {
	deps := testDeps(t)
	deps.Journal.Record(&journal.Call{
		Method:   "/shop.v1.OrderService/Get\xffOrder",
		Metadata: metadata.MD{"x-probe": {"ok\xffbad"}},
	})
	resp, err := journalClient(t, deps).ListCalls(context.Background(),
		connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls with an invalid UTF-8 call in the journal: %v", err)
	}
	call := resp.Msg.Calls[0]
	if call.Method != "/shop.v1.OrderService/Get�Order" || call.RequestMetadata[0].Values[0] != "ok�bad" {
		t.Errorf("rendered call = %v, want U+FFFD in the method and in the metadata value", call)
	}
}

func TestResetJournalClearsCallsAndKeepsCounting(t *testing.T) {
	deps := testDeps(t)
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
	client := journalClient(t, deps)
	ctx := context.Background()

	if _, err := client.ResetJournal(ctx, connect.NewRequest(&adminv1.ResetJournalRequest{})); err != nil {
		t.Fatalf("ResetJournal: %v", err)
	}
	cleared, err := client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(cleared.Msg.Calls) != 0 {
		t.Fatalf("after ResetJournal ListCalls = %v, want no calls", callSeqs(cleared.Msg.Calls))
	}

	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder"})
	after, err := client.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if got := callSeqs(after.Msg.Calls); !reflect.DeepEqual(got, []uint64{3}) {
		t.Errorf("seqs after reset = %v, want [3]: sequence numbers keep counting", got)
	}
}

// probeResponse renders a call as a response carrying only its seq.
func probeResponse(call *journal.Call) *adminv1.WatchCallsResponse {
	return &adminv1.WatchCallsResponse{Call: &adminv1.Call{Seq: call.Seq}}
}

// bufferedCalls returns a channel already holding one call per seq.
func bufferedCalls(seqs ...uint64) chan *journal.Call {
	calls := make(chan *journal.Call, len(seqs))
	for _, seq := range seqs {
		calls <- &journal.Call{Seq: seq}
	}
	return calls
}

func noSubscriptionErr() error { return nil }

// With calls buffered and Stopping already closed, nothing may be sent. A loop
// that relies on its select alone picks between the two ready cases at random,
// so it passes any single run with probability one half; 100 runs leave luck no
// room.
func TestStreamCallsSendsNothingOnceStoppingIsClosed(t *testing.T) {
	for run := 0; run < 100; run++ {
		stopping := make(chan struct{})
		close(stopping)
		sent := 0
		err := admin.StreamCalls(context.Background(), stopping, bufferedCalls(1, 2, 3), noSubscriptionErr,
			probeResponse, func(*adminv1.WatchCallsResponse) error { sent++; return nil })
		if sent != 0 {
			t.Fatalf("run %d: sent %d call(s) after Stopping closed, want 0", run, sent)
		}
		if code := connect.CodeOf(err); code != connect.CodeUnavailable {
			t.Fatalf("run %d: err = %v (code %v), want Unavailable", run, err, code)
		}
	}
}

// Stopping closes while a received call is being rendered: that call must not
// be sent. This fails a shutdown check placed only at the top of the loop, or
// before rendering.
func TestStreamCallsChecksStoppingAfterRendering(t *testing.T) {
	stopping := make(chan struct{})
	sent := 0
	err := admin.StreamCalls(context.Background(), stopping, bufferedCalls(1), noSubscriptionErr,
		func(call *journal.Call) *adminv1.WatchCallsResponse {
			close(stopping)
			return probeResponse(call)
		},
		func(*adminv1.WatchCallsResponse) error { sent++; return nil })
	if sent != 0 {
		t.Fatalf("sent %d call(s) whose rendering overlapped Stopping closing, want 0", sent)
	}
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("err = %v (code %v), want Unavailable", err, code)
	}
}

// An evicted subscription's channel still holds the calls buffered before the
// eviction: they are delivered, in order, before the eviction is reported.
func TestStreamCallsDeliversBufferedCallsBeforeReportingEviction(t *testing.T) {
	calls := bufferedCalls(1, 2, 3)
	close(calls)
	var sent []uint64
	err := admin.StreamCalls(context.Background(), make(chan struct{}), calls,
		func() error { return journal.ErrSlowConsumer }, probeResponse,
		func(resp *adminv1.WatchCallsResponse) error { sent = append(sent, resp.Call.Seq); return nil })
	if !reflect.DeepEqual(sent, []uint64{1, 2, 3}) {
		t.Errorf("sent %v, want [1 2 3] before the eviction is reported", sent)
	}
	if !errors.Is(err, journal.ErrSlowConsumer) {
		t.Errorf("err = %v, want journal.ErrSlowConsumer", err)
	}
}

func TestStreamCallsEndsWithTheContextOrTheSendError(t *testing.T) {
	closed := func() chan *journal.Call {
		calls := make(chan *journal.Call)
		close(calls)
		return calls
	}
	neverSend := func(*adminv1.WatchCallsResponse) error {
		t.Error("send called with no call to send")
		return nil
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := admin.StreamCalls(canceled, make(chan struct{}), closed(), noSubscriptionErr, probeResponse, neverSend); !errors.Is(err, context.Canceled) {
		t.Errorf("subscription ended by a canceled context: err = %v, want context.Canceled", err)
	}
	if err := admin.StreamCalls(context.Background(), make(chan struct{}), closed(), noSubscriptionErr, probeResponse, neverSend); err != nil {
		t.Errorf("subscription closed under a live context: err = %v, want nil", err)
	}
	gone := errors.New("connection gone")
	err := admin.StreamCalls(context.Background(), make(chan struct{}), bufferedCalls(1), noSubscriptionErr,
		probeResponse, func(*adminv1.WatchCallsResponse) error { return gone })
	if !errors.Is(err, gone) {
		t.Errorf("send failure: err = %v, want the send error", err)
	}
}

// watchedCalls receives a WatchCalls stream on its own goroutine, so a test can
// record calls while waiting for them. The channel is unbuffered: a test that
// stops reading it stops the client from draining the connection. It closes
// when the stream ends, after which stream.Err is safe to read.
func watchedCalls(stream *connect.ServerStreamForClient[adminv1.WatchCallsResponse]) <-chan *adminv1.Call {
	calls := make(chan *adminv1.Call)
	go func() {
		defer close(calls)
		for stream.Receive() {
			calls <- stream.Msg().GetCall()
		}
	}()
	return calls
}

// probeStubID marks the calls awaitWatching records, so nextCall can skip them.
const probeStubID = "probe"

// awaitWatching records probe calls on method until the stream delivers one. A
// client cannot observe that its subscription is registered; the first
// delivered probe is the first moment a later Record is certain to reach it.
func awaitWatching(t *testing.T, j *journal.Journal, method string, calls <-chan *adminv1.Call) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		j.Record(&journal.Call{Method: method, StubID: probeStubID})
		select {
		case _, ok := <-calls:
			if !ok {
				t.Fatal("the stream ended before its subscription was observed")
			}
			return
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("the watch never delivered a probe call")
		}
	}
}

// nextCall returns the next call from the stream that is not a probe.
func nextCall(t *testing.T, calls <-chan *adminv1.Call) *adminv1.Call {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case call, ok := <-calls:
			if !ok {
				t.Fatal("the stream ended while a call was expected")
			}
			if call.MatchedStubId != probeStubID {
				return call
			}
		case <-timeout:
			t.Fatal("no call arrived")
		}
	}
}

func TestWatchCallsStreamsRenderedCallsForItsMethod(t *testing.T) {
	deps := testDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := journalClient(t, deps).WatchCalls(ctx,
		connect.NewRequest(&adminv1.WatchCallsRequest{Method: "shop.v1.OrderService/GetOrder"}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	calls := watchedCalls(stream)
	awaitWatching(t, deps.Journal, "/shop.v1.OrderService/GetOrder", calls)

	deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/WatchOrder"})
	recordGetOrder(t, deps, "o-7")

	call := nextCall(t, calls)
	if call.Method != "/shop.v1.OrderService/GetOrder" {
		t.Fatalf("delivered a %q call, want only GetOrder calls", call.Method)
	}
	if orderID := jsonFields(t, call.Requests[0].Json)["order_id"]; orderID != "o-7" {
		t.Errorf("delivered call json = %s, want order_id o-7", call.Requests[0].Json)
	}
}

func TestWatchCallsEndsWithUnavailableWhenStoppingCloses(t *testing.T) {
	deps := testDeps(t)
	stopping := make(chan struct{})
	deps.Stopping = stopping
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := journalClient(t, deps).WatchCalls(ctx, connect.NewRequest(&adminv1.WatchCallsRequest{}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	calls := watchedCalls(stream)
	awaitWatching(t, deps.Journal, "/shop.v1.OrderService/GetOrder", calls)

	close(stopping)
	for range calls {
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeUnavailable {
		t.Fatalf("stream ended with %v (code %v), want Unavailable", stream.Err(), code)
	}
	if !strings.Contains(stream.Err().Error(), "server shutting down") {
		t.Errorf("stream error = %q, want it to say the server is shutting down", stream.Err())
	}
}

// A client that stops reading is evicted by the journal: the stream still
// delivers what was buffered, then ends with RESOURCE_EXHAUSTED. Each call
// carries 64 KiB of metadata, so the connection's buffers fill after a handful
// of calls; once the handler blocks in Send, its 64-call subscription buffer
// overflows. If a platform ever buffered all 64 MiB, the stream would never be
// evicted and the test would fail on its context deadline rather than pass.
func TestWatchCallsEndsWithResourceExhaustedWhenTheClientFallsBehind(t *testing.T) {
	deps := testDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := journalClient(t, deps).WatchCalls(ctx, connect.NewRequest(&adminv1.WatchCallsRequest{}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	calls := watchedCalls(stream)
	awaitWatching(t, deps.Journal, "/shop.v1.OrderService/GetOrder", calls)

	// Nothing reads calls while recording, so the client stops draining.
	bulk := metadata.MD{"x-bulk": {strings.Repeat("x", 64<<10)}}
	const recorded = 1000
	for i := 0; i < recorded; i++ {
		deps.Journal.Record(&journal.Call{Method: "/shop.v1.OrderService/GetOrder", Metadata: bulk})
	}

	delivered := 0
	for range calls {
		delivered++
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeResourceExhausted {
		t.Fatalf("stream ended with %v (code %v) after %d of %d calls, want ResourceExhausted",
			stream.Err(), code, delivered, recorded)
	}
	if delivered >= recorded {
		t.Errorf("delivered %d of %d calls, want some dropped by the eviction", delivered, recorded)
	}
}
