package schema

import (
	"context"
	"testing"
)

const testProtoDir = "../../testdata/protos"

func TestAddProtoDirAndLookupMethod(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}

	// Both slash forms must resolve (grpc-go hands us "/pkg.Svc/Method").
	for _, name := range []string{"/shop.v1.OrderService/GetOrder", "shop.v1.OrderService/GetOrder"} {
		m, err := reg.LookupMethod(name)
		if err != nil {
			t.Fatalf("LookupMethod(%q): %v", name, err)
		}
		if got := string(m.Input().FullName()); got != "shop.v1.GetOrderRequest" {
			t.Errorf("input type = %s, want shop.v1.GetOrderRequest", got)
		}
		if got := string(m.Output().FullName()); got != "shop.v1.GetOrderResponse" {
			t.Errorf("output type = %s, want shop.v1.GetOrderResponse", got)
		}
	}

	if _, err := reg.LookupMethod("/shop.v1.OrderService/NoSuchMethod"); err == nil {
		t.Error("expected error for unknown method")
	}
	if _, err := reg.LookupMethod("/no.such.Service/GetOrder"); err == nil {
		t.Error("expected error for unknown service")
	}
	if _, err := reg.LookupMethod("garbage"); err == nil {
		t.Error("expected error for malformed method name")
	}

	svcs := reg.Services()
	found := false
	for _, s := range svcs {
		if string(s.FullName()) == "shop.v1.OrderService" {
			found = true
		}
	}
	if !found {
		t.Errorf("Services() = %v, want to contain shop.v1.OrderService", svcs)
	}
}

func TestAddProtoDirEmpty(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), t.TempDir()); err == nil {
		t.Error("expected error for directory with no .proto files")
	}
}
