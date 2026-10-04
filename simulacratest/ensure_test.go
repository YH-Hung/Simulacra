package simulacratest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

const getOrderResponse = "shop.v1.GetOrderResponse"

// createStubError is the error the server itself gives for doc, for a test to
// hold the SDK's report against.
func createStubError(t *testing.T, c *simulacratest.Client, doc string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Admin().Stub.CreateStub(ctx, connect.NewRequest(&adminv1.CreateStubRequest{Document: doc}))
	if err == nil {
		t.Fatalf("CreateStub(%q) succeeded; want the server to refuse it", doc)
	}
	return err.Error()
}

func listedServices(t *testing.T, c *simulacratest.Client) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Admin().Schema.ListServices(ctx, connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	var names []string
	for _, s := range resp.Msg.GetServices() {
		names = append(names, s.GetName())
	}
	return names
}

func TestStubYAMLAutoRegistersALinkedService(t *testing.T) {
	sim := simulacratest.Start(t)
	if got := listedServices(t, sim.Client); slices.Contains(got, "shop.v1.OrderService") {
		t.Fatalf("a schemaless server already lists %v", got)
	}

	stub := sim.StubYAML(t, getOrderDoc)

	if !strings.HasPrefix(stub.GetId(), "api-") {
		t.Errorf("StubYAML id = %q; want an api- id", stub.GetId())
	}
	if got := listedServices(t, sim.Client); !slices.Contains(got, "shop.v1.OrderService") {
		t.Errorf("ListServices after StubYAML = %v; want it to list shop.v1.OrderService", got)
	}
}

func TestEnsureSkipsRegistrationWhenTheServerListsTheService(t *testing.T) {
	sim := simulacratest.Start(t, simulacratest.WithProtoDir("../testdata/protos"))
	counter := countSchemaRPCs(sim.Client)

	sim.StubYAML(t, getOrderDoc)

	if counter.registers != 0 {
		t.Errorf("RegisterSchemas calls = %d; want 0 when the server already lists the service", counter.registers)
	}
}

func TestEnsureUnknownServiceShowsTheServerErrorAndOneHint(t *testing.T) {
	sim := simulacratest.Start(t)
	const doc = "method: /nope.v1.Missing/Get\nrespond:\n  message:\n    note: x\n"
	want := createStubError(t, sim.Client, doc)

	rec := runRecorded(t, func(tb testing.TB) { sim.StubYAML(tb, doc) })

	wantMsg := "simulacratest: CreateStub: " + want + "\n" +
		"simulacratest: hint: the test binary does not link generated code for nope.v1.Missing; register its schema with RegisterFiles or WithProtoDir"
	if rec.FatalMsg() != wantMsg {
		t.Errorf("FatalMsg() = %q\nwant %q", rec.FatalMsg(), wantMsg)
	}
}

func TestEnsureMethodNameForms(t *testing.T) {
	sim := simulacratest.Start(t)
	counter := countSchemaRPCs(sim.Client)

	for _, method := range []string{"/shop.v1.OrderService/GetOrder", "shop.v1.OrderService/GetOrder"} {
		out, known := simulacratest.OutputType(t, sim.Client, method)
		if out != getOrderResponse || !known {
			t.Errorf("OutputType(%q) = %q, %v; want %q, true", method, out, known, getOrderResponse)
		}
	}
	if counter.registers != 1 || counter.lists != 1 {
		t.Errorf("RegisterSchemas/ListServices calls = %d/%d; want 1/1: both spellings are one service", counter.registers, counter.lists)
	}

	// Malformed names never reach the server.
	for _, bad := range []string{"", "bad", "/bad", "a/b/c", "/shop.v1.OrderService/", "//GetOrder", "shop.v1.OrderService/Get Order"} {
		if out, known := simulacratest.OutputType(t, sim.Client, bad); out != "" || known {
			t.Errorf("OutputType(%q) = %q, %v; want \"\", false", bad, out, known)
		}
	}
	if counter.registers != 1 || counter.lists != 1 {
		t.Errorf("RegisterSchemas/ListServices calls after malformed names = %d/%d; want 1/1", counter.registers, counter.lists)
	}

	// The SDK adds nothing to a malformed method's failure: the server's text
	// is the whole message.
	const doc = "method: bad\nrespond:\n  message:\n    note: x\n"
	want := createStubError(t, sim.Client, doc)
	rec := runRecorded(t, func(tb testing.TB) { sim.StubYAML(tb, doc) })
	if wantMsg := "simulacratest: CreateStub: " + want; rec.FatalMsg() != wantMsg {
		t.Errorf("FatalMsg() = %q; want %q", rec.FatalMsg(), wantMsg)
	}
}

func TestEnsureOutputTypeFromBothSources(t *testing.T) {
	const method = "shop.v1.OrderService/GetOrder"
	for _, tc := range []struct {
		name          string
		opts          []simulacratest.Option
		wantRegisters int
	}{
		{"linked generated code", nil, 1},
		{"server listing", []simulacratest.Option{simulacratest.WithProtoDir("../testdata/protos")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim := simulacratest.Start(t, tc.opts...)
			counter := countSchemaRPCs(sim.Client)

			out, known := simulacratest.OutputType(t, sim.Client, method)

			if out != getOrderResponse || !known {
				t.Errorf("OutputType = %q, %v; want %q, true", out, known, getOrderResponse)
			}
			if counter.registers != tc.wantRegisters {
				t.Errorf("RegisterSchemas calls = %d; want %d", counter.registers, tc.wantRegisters)
			}
		})
	}
}

func TestEnsureIsSafeForParallelSubtests(t *testing.T) {
	sim := simulacratest.Start(t)
	counter := countSchemaRPCs(sim.Client)

	t.Run("group", func(t *testing.T) {
		for i := range 8 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				sim.StubYAML(t, getOrderDoc)
			})
		}
	})

	if counter.registers != 1 {
		t.Errorf("RegisterSchemas calls = %d; want exactly 1 across 8 parallel subtests", counter.registers)
	}
}

// A hint blaming the missing generated code would misdirect a user whose
// service is registered and whose method name is merely mistyped.
func TestEnsureMistypedMethodOnAKnownServiceGetsNoHint(t *testing.T) {
	const typo = "method: /shop.v1.OrderService/GetOrdr\nrespond:\n  message:\n    note: x\n"
	for _, tc := range []struct {
		name  string
		opts  []simulacratest.Option
		setup func(*testing.T, *simulacratest.Server)
	}{
		{"auto-registered from the test binary", nil, func(t *testing.T, sim *simulacratest.Server) { sim.StubYAML(t, getOrderDoc) }},
		{"listed by the server", []simulacratest.Option{simulacratest.WithProtoDir("../testdata/protos")}, func(*testing.T, *simulacratest.Server) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim := simulacratest.Start(t, tc.opts...)
			tc.setup(t, sim)
			want := createStubError(t, sim.Client, typo)
			if !strings.Contains(want, "not_found") {
				t.Fatalf("server error %q is not NOT_FOUND; the test would not exercise the hint gate", want)
			}

			rec := runRecorded(t, func(tb testing.TB) { sim.StubYAML(tb, typo) })

			if wantMsg := "simulacratest: CreateStub: " + want; rec.FatalMsg() != wantMsg {
				t.Errorf("FatalMsg() = %q; want %q, with no hint: the service is known, only the method is mistyped", rec.FatalMsg(), wantMsg)
			}
		})
	}
}
