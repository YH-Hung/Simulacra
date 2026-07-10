package stub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirs(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "orders.yaml", `
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: "o-123" }
  respond:
    message: { order_id: "o-123", status: ORDER_STATUS_SHIPPED }
- method: shop.v1.OrderService/GetOrder
  priority: -1
  respond:
    message: { note: "fallback" }
`)
	stubs, errs := LoadDirs(reg, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("LoadDirs errors: %v", errs)
	}
	if len(stubs) != 2 {
		t.Fatalf("got %d stubs, want 2", len(stubs))
	}
	if !strings.Contains(stubs[0].Source, "orders.yaml#0") {
		t.Errorf("Source = %q, want to contain orders.yaml#0", stubs[0].Source)
	}
}

func TestLoadDirsReportsAllErrors(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
- method: shop.v1.OrderService/Nope
  respond: { message: {} }
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { no_such_field: 1 }
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want 2 (one per bad stub): %v", len(errs), errs)
	}
}

func TestLoadDirsRejectsUnknownKeys(t *testing.T) {
	// Strict parsing: M2 syntax (e.g. `delay:`) must fail loudly in M1.
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "future.yaml", `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
    delay: 50ms
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) == 0 {
		t.Fatal("expected an error for unknown key 'delay'")
	}
	if !strings.Contains(errs[0].Error(), "delay") {
		t.Errorf("error %q should mention the unknown key", errs[0])
	}
}

func TestLoadDirsMultiDocument(t *testing.T) {
	// A file may hold several `---` documents; all of them must load.
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "multi.yaml", `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: "doc one" }
---
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { note: "doc two" }
`)
	stubs, errs := LoadDirs(reg, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("LoadDirs errors: %v", errs)
	}
	if len(stubs) != 2 {
		t.Fatalf("got %d stubs, want 2 (one per document)", len(stubs))
	}
}

func TestLoadDirsMultiDocumentStrictInLaterDocs(t *testing.T) {
	// Unknown keys must fail loudly even in the second document.
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "multi.yaml", `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
---
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
    delay: 50ms
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) == 0 {
		t.Fatal("expected an error for unknown key in second document")
	}
	if !strings.Contains(errs[0].Error(), "delay") {
		t.Errorf("error %q should mention the unknown key", errs[0])
	}
}

func TestLoadDirsRejectsInvalidYAML(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "broken.yaml", "{{{ not yaml")
	if _, errs := LoadDirs(reg, []string{dir}); len(errs) == 0 {
		t.Fatal("expected a parse error")
	}
}
