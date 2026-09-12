package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// testdataDescriptorSet serializes testdata/protos as a self-contained
// FileDescriptorSet: the bytes an SDK sends to RegisterSchemas.
func testdataDescriptorSet(t *testing.T) []byte {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	reg.Snapshot().RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
		return true
	})
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return raw
}

// adminHTTPClient returns an HTTP/1.1 client for the admin plane — the Connect
// path the CLI and the Go SDK use — and the plane's base URL.
func adminHTTPClient(t *testing.T, srv *Server) (*http.Client, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	return client, "http://" + srv.AdminAddr().String()
}

// jsonField decodes one top-level field of a DecodedMessage's json. protojson
// output is not byte-stable, so tests never compare it as a string.
func jsonField(t *testing.T, text, field string) any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatalf("decoding json %q: %v", text, err)
	}
	return fields[field]
}

// metadataValue returns the first text value of key in a rendered call.
func metadataValue(call *adminv1.Call, key string) string {
	for _, entry := range call.GetRequestMetadata() {
		if entry.GetKey() == key && len(entry.GetValues()) > 0 {
			return entry.GetValues()[0]
		}
	}
	return ""
}

// rawDataPlaneCall sends one gRPC request to the data plane as hand-written
// HTTP/2 frames — HEADERS carrying path and extra, then an empty message — and
// waits for the response to end. It exists to put bytes on the wire that a
// well-behaved client library refuses to send, such as invalid UTF-8 in :path
// or in a metadata value.
func rawDataPlaneCall(t *testing.T, addr, path string, extra ...hpack.HeaderField) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial data plane: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("write preface: %v", err)
	}
	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	fields := append([]hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: path},
		{Name: ":authority", Value: "simulacra-test"},
		{Name: "content-type", Value: "application/grpc"},
		{Name: "te", Value: "trailers"},
	}, extra...)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatalf("encode %s: %v", field.Name, err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true}); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	// An empty, uncompressed gRPC message: a zero flag byte and a zero length.
	if err := framer.WriteData(1, true, make([]byte, 5)); err != nil {
		t.Fatalf("write data: %v", err)
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("the response never ended: %v", err)
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					t.Fatalf("write settings ack: %v", err)
				}
			}
		case *http2.HeadersFrame:
			if f.StreamEnded() {
				return
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("the data plane reset the stream: %v", f.ErrCode)
		case *http2.GoAwayFrame:
			t.Fatalf("the data plane sent GOAWAY: %v", f.ErrCode)
		}
	}
}

// The container/SDK boot contract, in process (M3 design §12), driven entirely
// through the admin API by a native gRPC client over h2c: a server started with
// no schema takes schemas, then a stub, serves a matching call, and verifies
// it.
func TestBootContractThroughTheAdminAPI(t *testing.T) {
	srv, err := Start(context.Background(), Options{
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 16,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	adminConn := adminGRPCConn(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	registered := &adminv1.RegisterSchemasResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.SchemaServiceRegisterSchemasProcedure,
		&adminv1.RegisterSchemasRequest{DescriptorSet: testdataDescriptorSet(t)}, registered); err != nil {
		t.Fatalf("RegisterSchemas: %v", err)
	}
	// The data plane registered grpc.health.v1.Health at startup, so testdata
	// brings the count to two.
	if len(registered.RegisteredFiles) != 3 || registered.ServiceCount != 2 {
		t.Fatalf("RegisterSchemas = files %q, %d service(s); want 3 files and 2 services",
			registered.RegisteredFiles, registered.ServiceCount)
	}

	created := &adminv1.CreateStubResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.StubServiceCreateStubProcedure, &adminv1.CreateStubRequest{
		Document: "method: shop.v1.OrderService/GetOrder\nmatch:\n  message:\n    order_id: { eq: o-1 }\n" +
			"respond:\n  message: { note: from-api }\n",
	}, created); err != nil {
		t.Fatalf("CreateStub: %v", err)
	}

	dataConn, err := grpc.NewClient(srv.DataAddr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = dataConn.Close() })
	method, err := srv.reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod after RegisterSchemas: %v", err)
	}
	req := dynamicpb.NewMessage(method.Input())
	if err := (protojson.UnmarshalOptions{Resolver: srv.reg.Types()}).Unmarshal([]byte(`{"order_id":"o-1"}`), req); err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp := dynamicpb.NewMessage(method.Output())
	if err := dataConn.Invoke(ctx, "/shop.v1.OrderService/GetOrder", req, resp); err != nil {
		t.Fatalf("data-plane GetOrder: %v", err)
	}
	if note := resp.Get(method.Output().Fields().ByName("note")).String(); note != "from-api" {
		t.Fatalf("response note = %q, want from-api", note)
	}

	listed := &adminv1.ListCallsResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.JournalServiceListCallsProcedure, &adminv1.ListCallsRequest{}, listed); err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(listed.Calls) != 1 {
		t.Fatalf("ListCalls = %d calls, want 1", len(listed.Calls))
	}
	call := listed.Calls[0]
	if call.MatchedStubId != created.Stub.Id || call.Status.GetCode() != 0 || len(call.Requests) != 1 || len(call.Responses) != 1 {
		t.Fatalf("recorded call = %v, want matched_stub_id %s, code 0, one request and one response", call, created.Stub.Id)
	}
	if jsonField(t, call.Requests[0].Json, "order_id") != "o-1" || jsonField(t, call.Responses[0].Json, "note") != "from-api" {
		t.Errorf("recorded messages = %s / %s, want order_id o-1 and note from-api", call.Requests[0].Json, call.Responses[0].Json)
	}

	passed := &adminv1.VerifyCallsResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.VerifyServiceVerifyCallsProcedure, &adminv1.VerifyCallsRequest{
		Method:          "shop.v1.OrderService/GetOrder",
		MatcherDocument: "message:\n  order_id: { eq: o-1 }\n",
		Times:           &adminv1.Times{Exactly: proto.Int32(1)},
	}, passed); err != nil {
		t.Fatalf("VerifyCalls (o-1): %v", err)
	}
	if !passed.Passed {
		t.Errorf("VerifyCalls o-1 exactly 1 = %q, want a pass", passed.Explanation)
	}
	failed := &adminv1.VerifyCallsResponse{}
	if err := adminConn.Invoke(ctx, adminv1connect.VerifyServiceVerifyCallsProcedure, &adminv1.VerifyCallsRequest{
		Method:          "shop.v1.OrderService/GetOrder",
		MatcherDocument: "message:\n  order_id: { eq: o-2 }\n",
		Times:           &adminv1.Times{Exactly: proto.Int32(1)},
	}, failed); err != nil {
		t.Fatalf("VerifyCalls (o-2): %v", err)
	}
	if failed.Passed || len(failed.Actual) != 1 || !strings.Contains(failed.Actual[0].NearestMiss, `expected to equal "o-2"`) {
		t.Errorf("VerifyCalls o-2 exactly 1 = %v, want a failure whose nearest miss names o-2", failed)
	}
}

// recordUntilWatched records probe calls until arrived reports that the watch
// delivered one. A client cannot observe that its subscription is registered;
// the first delivered probe is the first moment later calls are certain to
// reach it. Probe calls carry StubID "probe".
func recordUntilWatched(t *testing.T, srv *Server, arrived func() bool) {
	t.Helper()
	waitFor(t, 5*time.Second, "the watch to deliver a probe call", func() bool {
		srv.journal.Record(&journal.Call{Method: "/probe.v1.Probe/Ready", StubID: "probe"})
		return arrived()
	})
}

// arrived reports whether ch yields a value within a short wait.
func arrived(ch <-chan struct{}) func() bool {
	return func() bool {
		select {
		case <-ch:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	}
}

// stopsPromptly runs GracefulStop and fails if it takes more than a fraction of
// shutdownGrace: waiting out the whole budget is exactly the bug.
func stopsPromptly(t *testing.T, srv *Server) {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace + dataGraceFloor + 5*time.Second):
		t.Fatal("GracefulStop never returned with a WatchCalls stream open")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("GracefulStop took %v with a WatchCalls stream open, want well under shutdownGrace (%v)",
			elapsed, shutdownGrace)
	}
}

// Bytes the data plane accepts but a proto3 string cannot carry must not break
// any response that renders them (design §5.2). Removing validUTF8 from the
// metadata value or from the method makes the RPCs below fail to serialize.
func TestInvalidUTF8FromIngressRendersInEveryCallResponse(t *testing.T) {
	srv := startWithAdmin(t, Options{})
	httpClient, base := adminHTTPClient(t, srv)
	journals := adminv1connect.NewJournalServiceClient(httpClient, base)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stream, err := journals.WatchCalls(ctx, connect.NewRequest(&adminv1.WatchCallsRequest{}))
	if err != nil {
		t.Fatalf("WatchCalls: %v", err)
	}
	defer stream.Close()
	watched := make(chan *adminv1.Call)
	go func() {
		defer close(watched)
		for stream.Receive() {
			watched <- stream.Msg().GetCall()
		}
	}()
	recordUntilWatched(t, srv, func() bool {
		select {
		case <-watched:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	})

	rawDataPlaneCall(t, srv.DataAddr().String(), "/shop.v1.OrderService/GetOrder",
		hpack.HeaderField{Name: "x-probe", Value: "ok\xffbad"})
	rawDataPlaneCall(t, srv.DataAddr().String(), "/shop.v1.OrderService/Get\xffOrder")

	const wantValue, wantMethod = "ok�bad", "/shop.v1.OrderService/Get�Order"
	var sawValue, sawMethod bool
	for !sawValue || !sawMethod {
		select {
		case call, ok := <-watched:
			if !ok {
				t.Fatalf("WatchCalls ended before delivering both calls: %v", stream.Err())
			}
			sawValue = sawValue || metadataValue(call, "x-probe") == wantValue
			sawMethod = sawMethod || call.Method == wantMethod
		case <-ctx.Done():
			t.Fatalf("WatchCalls delivered the metadata call: %v, the :path call: %v; want both", sawValue, sawMethod)
		}
	}

	listed, err := journals.ListCalls(ctx, connect.NewRequest(&adminv1.ListCallsRequest{}))
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	var listedValue, listedMethod bool
	for _, call := range listed.Msg.Calls {
		listedValue = listedValue || metadataValue(call, "x-probe") == wantValue
		listedMethod = listedMethod || call.Method == wantMethod
	}
	if !listedValue || !listedMethod {
		t.Errorf("ListCalls rendered the metadata call: %v, the :path call: %v; want both", listedValue, listedMethod)
	}

	verdict, err := adminv1connect.NewVerifyServiceClient(httpClient, base).VerifyCalls(ctx,
		connect.NewRequest(&adminv1.VerifyCallsRequest{
			Method:          "/shop.v1.OrderService/GetOrder",
			MatcherDocument: "metadata:\n  x-probe: { eq: nope }\n",
			Times:           &adminv1.Times{Exactly: proto.Int32(1)},
		}))
	if err != nil {
		t.Fatalf("VerifyCalls: %v", err)
	}
	if verdict.Msg.Passed || len(verdict.Msg.Actual) != 1 || metadataValue(verdict.Msg.Actual[0].Call, "x-probe") != wantValue {
		t.Errorf("VerifyCalls = %v, want a failure whose actual call carries x-probe %q", verdict.Msg, wantValue)
	}
}

// A tail must not hold teardown for the grace period: WatchCalls ends its
// stream with UNAVAILABLE as soon as Stopping closes (design §6). Without that,
// GracefulStop waits out shutdownGrace, which stopsPromptly catches.
func TestWatchCallsEndsWithUnavailableWhenTheServerStops(t *testing.T) {
	t.Run("connect over HTTP/1.1", func(t *testing.T) {
		srv := startWithAdmin(t, Options{})
		httpClient, base := adminHTTPClient(t, srv)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := adminv1connect.NewJournalServiceClient(httpClient, base).WatchCalls(ctx,
			connect.NewRequest(&adminv1.WatchCallsRequest{}))
		if err != nil {
			t.Fatalf("WatchCalls: %v", err)
		}
		defer stream.Close()
		delivered := make(chan struct{}, 1)
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			for stream.Receive() {
				select {
				case delivered <- struct{}{}:
				default:
				}
			}
		}()
		recordUntilWatched(t, srv, arrived(delivered))

		stopsPromptly(t, srv)
		<-ended
		if err := stream.Err(); connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "server shutting down") {
			t.Fatalf("stream ended with %v, want unavailable: server shutting down", err)
		}
	})

	t.Run("grpc over h2c", func(t *testing.T) {
		srv := startWithAdmin(t, Options{})
		conn := adminGRPCConn(t, srv)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true},
			adminv1connect.JournalServiceWatchCallsProcedure)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := stream.SendMsg(&adminv1.WatchCallsRequest{}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		delivered := make(chan struct{}, 1)
		final := make(chan error, 1)
		go func() {
			for {
				if err := stream.RecvMsg(&adminv1.WatchCallsResponse{}); err != nil {
					final <- err
					return
				}
				select {
				case delivered <- struct{}{}:
				default:
				}
			}
		}()
		recordUntilWatched(t, srv, arrived(delivered))

		stopsPromptly(t, srv)
		if err := <-final; status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "server shutting down") {
			t.Fatalf("stream ended with %v, want Unavailable: server shutting down", err)
		}
	})
}
