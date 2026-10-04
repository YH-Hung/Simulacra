package simulacratest_test

import (
	"testing"

	// Stands in for your own generated package.
	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

func ExampleStart() {
	var t *testing.T // provided by your test

	sim := simulacratest.Start(t)
	sim.Stub(shopv1.OrderService_GetOrder_FullMethodName).
		MatchField("order_id", simulacratest.Eq("o-123")).
		Respond(t, &shopv1.GetOrderResponse{OrderId: "o-123", Status: shopv1.OrderStatus_ORDER_STATUS_SHIPPED})

	// The code under test calls the data plane.
	orders := shopv1.NewOrderServiceClient(sim.Dial(t))
	if _, err := orders.GetOrder(t.Context(), &shopv1.GetOrderRequest{OrderId: "o-123"}); err != nil {
		t.Fatal(err)
	}

	sim.Verify(shopv1.OrderService_GetOrder_FullMethodName).
		MatchField("order_id", simulacratest.Eq("o-123")).
		Exactly(t, 1)

	req := &shopv1.GetOrderRequest{}
	sim.Calls(t, shopv1.OrderService_GetOrder_FullMethodName)[0].Request(t, req)
	if req.GetOrderId() != "o-123" {
		t.Errorf("order_id = %q", req.GetOrderId())
	}
}
