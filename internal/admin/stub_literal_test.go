package admin_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func TestCreateStubKeepsLiteralInTheEnvelopeDocument(t *testing.T) {
	const document = `{"method":"shop.v1.OrderService/GetOrder","respond":{"literal":true,"message":{"note":"{{ x }}"}}}`
	resp, err := stubClient(t, testDeps(t)).CreateStub(context.Background(),
		connect.NewRequest(&adminv1.CreateStubRequest{Document: document}))
	if err != nil {
		t.Fatalf("CreateStub: %v", err)
	}
	got := resp.Msg.Stub.Document
	for _, want := range []string{"literal: true", "{{ x }}"} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope document missing %q:\n%s", want, got)
		}
	}
}
