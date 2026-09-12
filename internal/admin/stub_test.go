package admin_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/admin"
	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func stubClient(t *testing.T, deps admin.Deps) adminv1connect.StubServiceClient {
	t.Helper()
	ts := installed(t, deps)
	return adminv1connect.NewStubServiceClient(ts.Client(), ts.URL)
}

// getOrderStub is one API stub document for GetOrder, answering with note.
func getOrderStub(note string, priority int) string {
	return fmt.Sprintf("method: shop.v1.OrderService/GetOrder\npriority: %d\nrespond:\n  message: { note: %s }\n",
		priority, note)
}

// apiStubIDs lists the ids of the API-origin stubs in the served store, in
// List order.
func apiStubIDs(deps admin.Deps) []string {
	api := stub.OriginAPI
	var ids []string
	for _, info := range deps.Store.List(stub.ListFilter{Origin: &api}) {
		ids = append(ids, info.ID)
	}
	return ids
}

func TestCreateStubInstallsAnAPIStubFromYAMLOrJSON(t *testing.T) {
	deps := testDeps(t)
	client := stubClient(t, deps)
	cases := []struct {
		document string
		want     *adminv1.Stub
	}{
		{
			document: "method: shop.v1.OrderService/GetOrder\nrespond:\n  message: { note: from-yaml }\n",
			want: &adminv1.Stub{Id: "api-1", Method: "/shop.v1.OrderService/GetOrder",
				Shape: adminv1.StubShape_STUB_SHAPE_UNARY, Origin: adminv1.StubOrigin_STUB_ORIGIN_API, Source: "api"},
		},
		{
			document: `{"method": "shop.v1.OrderService/WatchOrder", "priority": 3, "times": 2,` +
				` "respond": {"stream": [{"message": {"note": "from-json"}}]}}`,
			want: &adminv1.Stub{Id: "api-2", Method: "/shop.v1.OrderService/WatchOrder",
				Shape: adminv1.StubShape_STUB_SHAPE_SERVER_STREAM, Priority: 3, Times: 2,
				Origin: adminv1.StubOrigin_STUB_ORIGIN_API, Source: "api"},
		},
	}
	for _, tc := range cases {
		resp, err := client.CreateStub(context.Background(),
			connect.NewRequest(&adminv1.CreateStubRequest{Document: tc.document}))
		if err != nil {
			t.Fatalf("CreateStub(%s): %v", tc.document, err)
		}
		_, normalized, err := stub.ParseDocument([]byte(tc.document))
		if err != nil {
			t.Fatalf("ParseDocument: %v", err)
		}
		tc.want.Document = normalized
		if !proto.Equal(resp.Msg.Stub, tc.want) {
			t.Errorf("CreateStub envelope = %v, want %v", resp.Msg.Stub, tc.want)
		}
	}
	if ids := apiStubIDs(deps); !reflect.DeepEqual(ids, []string{"api-1", "api-2"}) {
		t.Errorf("API stubs in the served store = %q, want [api-1 api-2]", ids)
	}
}

func TestCreateStubErrorCodesLeaveTheStoreUntouched(t *testing.T) {
	cases := []struct {
		name     string
		document string
		code     connect.Code
		want     string
	}{
		{"empty", "", connect.CodeInvalidArgument, "document is empty"},
		{"sequence", "- method: shop.v1.OrderService/GetOrder\n", connect.CodeInvalidArgument, "document is a sequence"},
		{"unknown field", "method: shop.v1.OrderService/GetOrder\nbogus: 1\n", connect.CodeInvalidArgument, "bogus"},
		{"malformed method", "method: garbage\n", connect.CodeInvalidArgument, `invalid method name "garbage"`},
		{"unregistered method", "method: shop.v1.OrderService/Nope\n", connect.CodeNotFound,
			`method "Nope" not found on service "shop.v1.OrderService"`},
		{"times beyond int32", "method: shop.v1.OrderService/GetOrder\ntimes: 2147483648\n",
			connect.CodeInvalidArgument, "times must be at most 2147483647"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			_, err := stubClient(t, deps).CreateStub(context.Background(),
				connect.NewRequest(&adminv1.CreateStubRequest{Document: tc.document}))
			if code := connect.CodeOf(err); code != tc.code {
				t.Fatalf("CreateStub = %v (code %v), want %v", err, code, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if n := deps.Store.Len(); n != 0 {
				t.Errorf("store holds %d stub(s) after a rejected CreateStub, want 0", n)
			}
		})
	}
}

func TestListStubsReportsEnvelopesAndFilters(t *testing.T) {
	deps := testDeps(t)
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  priority: 2147483647
  times: 2147483647
  respond:
    message: { note: file-origin }
`)
	client := stubClient(t, deps)
	ctx := context.Background()
	if _, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{
		Document: "method: shop.v1.OrderService/WatchOrder\nrespond:\n  stream:\n  - message: { note: api-origin }\n",
	})); err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	// Spend one use of the file stub so hits is observable.
	if deps.Store.Select("/shop.v1.OrderService/GetOrder", match.Input{}) == nil {
		t.Fatal("precondition: the file stub did not select")
	}

	all, err := client.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	if len(all.Msg.Stubs) != 2 {
		t.Fatalf("ListStubs = %d stubs, want 2", len(all.Msg.Stubs))
	}
	file, api := all.Msg.Stubs[0], all.Msg.Stubs[1]
	if file.Origin != adminv1.StubOrigin_STUB_ORIGIN_FILE || !strings.HasSuffix(file.Source, "stubs.yaml#0") || file.Id != file.Source {
		t.Errorf("file stub envelope = %v, want FILE origin with id = source = <dir>/stubs.yaml#0", file)
	}
	// The int32 boundary values convert exactly (design §3.3).
	if file.Priority != 2147483647 || file.Times != 2147483647 || file.Hits != 1 {
		t.Errorf("file stub priority/times/hits = %d/%d/%d, want 2147483647/2147483647/1",
			file.Priority, file.Times, file.Hits)
	}
	if api.Id != "api-1" || api.Origin != adminv1.StubOrigin_STUB_ORIGIN_API || api.Shape != adminv1.StubShape_STUB_SHAPE_SERVER_STREAM {
		t.Errorf("api stub envelope = %v, want api-1, API origin, server-streaming", api)
	}

	filters := []struct {
		name string
		req  *adminv1.ListStubsRequest
		want []string
	}{
		{"API origin", &adminv1.ListStubsRequest{Origin: adminv1.StubOrigin_STUB_ORIGIN_API}, []string{"api-1"}},
		{"file origin", &adminv1.ListStubsRequest{Origin: adminv1.StubOrigin_STUB_ORIGIN_FILE}, []string{file.Id}},
		{"method without slash", &adminv1.ListStubsRequest{Method: "shop.v1.OrderService/WatchOrder"}, []string{"api-1"}},
		{"method with slash", &adminv1.ListStubsRequest{Method: "/shop.v1.OrderService/GetOrder"}, []string{file.Id}},
	}
	for _, f := range filters {
		resp, err := client.ListStubs(ctx, connect.NewRequest(f.req))
		if err != nil {
			t.Fatalf("%s: ListStubs: %v", f.name, err)
		}
		var ids []string
		for _, s := range resp.Msg.Stubs {
			ids = append(ids, s.Id)
		}
		if !reflect.DeepEqual(ids, f.want) {
			t.Errorf("%s: ids = %q, want %q", f.name, ids, f.want)
		}
	}

	_, err = client.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{Origin: adminv1.StubOrigin(7)}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("ListStubs with origin 7 = %v (code %v), want InvalidArgument", err, code)
	}
}

func TestDeleteStubRemovesAPIStubsOnly(t *testing.T) {
	deps := testDeps(t)
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: file-origin }
`)
	client := stubClient(t, deps)
	ctx := context.Background()
	created, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: getOrderStub("api", 0)}))
	if err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	fileOrigin := stub.OriginFile
	fileID := deps.Store.List(stub.ListFilter{Origin: &fileOrigin})[0].ID

	cases := []struct {
		name string
		id   string
		code connect.Code
		want string
	}{
		{"empty id", "", connect.CodeInvalidArgument, "id is required"},
		{"file-origin stub", fileID, connect.CodeFailedPrecondition, "edit or remove the file"},
		{"unknown id", "api-99", connect.CodeNotFound, `no stub with this id: "api-99"`},
	}
	for _, tc := range cases {
		_, err := client.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: tc.id}))
		if code := connect.CodeOf(err); code != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: DeleteStub = %v (code %v), want %v containing %q", tc.name, err, code, tc.code, tc.want)
		}
	}

	if _, err := client.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: created.Msg.Stub.Id})); err != nil {
		t.Fatalf("DeleteStub(%s): %v", created.Msg.Stub.Id, err)
	}
	if ids := apiStubIDs(deps); len(ids) != 0 || deps.Store.Len() != 1 {
		t.Fatalf("after DeleteStub: API ids %q, store size %d; want none and the file stub kept", ids, deps.Store.Len())
	}
	_, err = client.DeleteStub(ctx, connect.NewRequest(&adminv1.DeleteStubRequest{Id: created.Msg.Stub.Id}))
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("deleting %s twice = %v (code %v), want NotFound", created.Msg.Stub.Id, err, code)
	}
}

func TestReplaceAllStubsIsAllOrNothing(t *testing.T) {
	deps := testDeps(t)
	// Priority 10 makes the file stub, not an API stub, the one Select spends.
	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  priority: 10
  respond:
    message: { note: file-origin }
`)
	client := stubClient(t, deps)
	ctx := context.Background()
	for _, note := range []string{"first", "second"} {
		if _, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: getOrderStub(note, 0)})); err != nil {
			t.Fatalf("CreateStub: %v", err)
		}
	}
	if selected := deps.Store.Select("/shop.v1.OrderService/GetOrder", match.Input{}); selected == nil || selected.Origin != stub.OriginFile {
		t.Fatalf("precondition: selected %+v, want the file stub", selected)
	}
	before := apiStubIDs(deps)

	_, err := client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{Documents: []string{
		getOrderStub("third", 0),
		getOrderStub("fourth", 0),
		"method: shop.v1.OrderService/GetOrder\nbogus: 1\n",
	}}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "documents[2]: ") {
		t.Fatalf("ReplaceAllStubs with a bad third document = %v (code %v), want InvalidArgument naming documents[2]", err, code)
	}
	if after := apiStubIDs(deps); !reflect.DeepEqual(after, before) {
		t.Fatalf("API stubs after a rejected ReplaceAllStubs = %q, want the previous %q", after, before)
	}

	_, err = client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{Documents: []string{
		getOrderStub("third", 0),
		"method: shop.v1.OrderService/Nope\n",
	}}))
	if code := connect.CodeOf(err); code != connect.CodeNotFound || !strings.Contains(err.Error(), "documents[1]: ") {
		t.Fatalf("ReplaceAllStubs naming an unregistered method = %v (code %v), want NotFound naming documents[1]", err, code)
	}

	replaced, err := client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{Documents: []string{
		getOrderStub("third", 0),
		getOrderStub("fourth", 0),
	}}))
	if err != nil {
		t.Fatalf("ReplaceAllStubs: %v", err)
	}
	var ids []string
	for _, s := range replaced.Msg.Stubs {
		ids = append(ids, s.Id)
	}
	if want := []string{"api-3", "api-4"}; !reflect.DeepEqual(ids, want) || !reflect.DeepEqual(apiStubIDs(deps), want) {
		t.Fatalf("replaced ids = %q, store API ids = %q; want %q in both", ids, apiStubIDs(deps), want)
	}
	fileOrigin := stub.OriginFile
	if files := deps.Store.List(stub.ListFilter{Origin: &fileOrigin}); len(files) != 1 || files[0].Hits != 1 {
		t.Fatalf("file stubs after ReplaceAllStubs = %+v, want the one file stub with its spent use kept", files)
	}

	if _, err := client.ReplaceAllStubs(ctx, connect.NewRequest(&adminv1.ReplaceAllStubsRequest{})); err != nil {
		t.Fatalf("ReplaceAllStubs(no documents): %v", err)
	}
	if ids := apiStubIDs(deps); len(ids) != 0 || deps.Store.Len() != 1 {
		t.Fatalf("after ReplaceAllStubs(no documents): API ids %q, store size %d; want none and the file stub kept",
			ids, deps.Store.Len())
	}
}

func TestExportStubsRendersAPIStubsAsOneLoadableFile(t *testing.T) {
	deps := testDeps(t)
	client := stubClient(t, deps)
	ctx := context.Background()

	empty, err := client.ExportStubs(ctx, connect.NewRequest(&adminv1.ExportStubsRequest{}))
	if err != nil {
		t.Fatalf("ExportStubs(no stubs): %v", err)
	}
	if empty.Msg.Document != "[]\n" || empty.Msg.StubCount != 0 {
		t.Errorf("empty export = %q, %d; want %q, 0", empty.Msg.Document, empty.Msg.StubCount, "[]\n")
	}

	loadStubsInto(t, deps, `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: file-origin }
`)
	for _, doc := range []string{getOrderStub("low", 0), getOrderStub("high", 5)} {
		if _, err := client.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: doc})); err != nil {
			t.Fatalf("CreateStub: %v", err)
		}
	}
	listed, err := client.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{Origin: adminv1.StubOrigin_STUB_ORIGIN_API}))
	if err != nil {
		t.Fatalf("ListStubs: %v", err)
	}
	exported, err := client.ExportStubs(ctx, connect.NewRequest(&adminv1.ExportStubsRequest{}))
	if err != nil {
		t.Fatalf("ExportStubs: %v", err)
	}
	if exported.Msg.StubCount != 2 {
		t.Fatalf("stub_count = %d, want 2 (file-origin stubs are not exported)", exported.Msg.StubCount)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exported.yaml"), []byte(exported.Msg.Document), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, errs := stub.LoadDirs(deps.Registry, []string{dir})
	if len(errs) != 0 || len(loaded) != 2 {
		t.Fatalf("loading the export = %d stub(s), errors %v; want 2 and none", len(loaded), errs)
	}
	for i, c := range loaded {
		want := listed.Msg.Stubs[i]
		if c.Document != want.Document || int32(c.Priority) != want.Priority {
			t.Errorf("loaded stub %d = priority %d; want ListStubs order and documents (%v)", i, c.Priority, want)
		}
	}
}

// File stub ids and sources are paths, which filesystems such as ext4 allow to
// hold invalid UTF-8. No development filesystem here can produce one, so the
// conversion is tested directly.
func TestStubEnvelopeSanitizesPathDerivedStrings(t *testing.T) {
	got := admin.StubEnvelope(stub.Info{
		ID:     "stubs/\xff.yaml#0",
		Source: "stubs/\xff.yaml#0",
		Method: "/shop.v1.OrderService/GetOrder",
		Origin: stub.OriginFile,
	})
	if got.Id != "stubs/�.yaml#0" || got.Source != got.Id {
		t.Errorf("id/source = %q/%q, want U+FFFD in place of the invalid byte", got.Id, got.Source)
	}
	if _, err := proto.Marshal(got); err != nil {
		t.Fatalf("the envelope does not marshal: %v", err)
	}
}

// CreateStub reaches the same method lookup through Compile, so a malformed
// identifier in a document must be INVALID_ARGUMENT there too.
func TestCreateStubRejectsMalformedMethodIdentifiers(t *testing.T) {
	for _, method := range malformedMethods {
		deps := testDeps(t)
		document := fmt.Sprintf("method: %s\nrespond:\n  message: {}\n", method)
		_, err := stubClient(t, deps).CreateStub(context.Background(),
			connect.NewRequest(&adminv1.CreateStubRequest{Document: document}))
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
			t.Errorf("CreateStub(%q) = %v (code %v), want InvalidArgument", method, err, code)
		}
		if n := deps.Store.Len(); n != 0 {
			t.Errorf("CreateStub(%q) left %d stub(s) in the store", method, n)
		}
	}
}
