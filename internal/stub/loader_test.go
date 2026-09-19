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
	// Strict parsing: unknown response syntax must fail loudly.
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "future.yaml", `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
    fault: unavailable
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) == 0 {
		t.Fatal("expected an error for unknown key 'fault'")
	}
	if !strings.Contains(errs[0].Error(), "fault") {
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
    fault: unavailable
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) == 0 {
		t.Fatal("expected an error for unknown key in second document")
	}
	if !strings.Contains(errs[0].Error(), "fault") {
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

// Every file-loaded stub carries a normalized document that re-parses to the
// same stub — the file/API single-grammar guarantee (design §3.1).
func TestLoadDirsStampsRoundTrippableDocuments(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "s.yaml", `
# file comment
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: o-1 }
  respond:
    message: {}
- method: shop.v1.OrderService/GetOrder
  priority: 5
  respond:
    message:
      order_id: o-2
`)
	stubs, errs := LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(stubs) != 2 {
		t.Fatalf("loaded %d stubs, want 2", len(stubs))
	}
	for i, c := range stubs {
		if c.ID != c.Source || c.ID == "" {
			t.Fatalf("stub %d: ID = %q, Source = %q; file stubs carry Source as ID", i, c.ID, c.Source)
		}
		if c.Document == "" {
			t.Fatalf("stub %d: empty Document", i)
		}
		s, normalized, err := ParseDocument([]byte(c.Document))
		if err != nil {
			t.Fatalf("stub %d document does not re-parse: %v\n%s", i, err, c.Document)
		}
		if normalized != c.Document {
			t.Fatalf("stub %d document is not a normalization fixed point", i)
		}
		re, err := Compile(reg, s, c.Source)
		if err != nil {
			t.Fatalf("stub %d document does not re-compile: %v", i, err)
		}
		if re.Method != c.Method || re.Priority != c.Priority || re.Times != c.Times || re.Shape != c.Shape {
			t.Fatalf("stub %d round trip changed compiled fields: %+v vs %+v", i, re, c)
		}
	}
}

// SplitDocuments is the file-grammar splitter the CLI's `stub add` reuses, so
// that files and the admin API are split by one implementation (design §7).
func TestSplitDocumentsIsTheExportedFileSplitter(t *testing.T) {
	t.Run("multi-document file splits in order", func(t *testing.T) {
		file := []byte("- method: shop.v1.OrderService/GetOrder\n" +
			"  respond:\n    message: { note: first }\n" +
			"---\n" +
			"- method: shop.v1.OrderService/WatchOrder\n" +
			"  respond:\n    stream: [{ message: { note: second } }]\n")
		docs, err := SplitDocuments(file)
		if err != nil {
			t.Fatalf("SplitDocuments: %v", err)
		}
		if len(docs) != 2 {
			t.Fatalf("got %d documents, want 2: %q", len(docs), docs)
		}
		if !strings.Contains(docs[0], "note: first") || strings.Contains(docs[0], "second") {
			t.Errorf("document 0 = %q, want only the first stub", docs[0])
		}
		if !strings.Contains(docs[1], "note: second") {
			t.Errorf("document 1 = %q, want the second stub", docs[1])
		}
		// Each document must stand alone as one mapping, not a list item.
		if strings.HasPrefix(docs[0], "- ") {
			t.Errorf("document 0 = %q, want a bare mapping", docs[0])
		}
	})

	t.Run("empty sequence yields no documents", func(t *testing.T) {
		docs, err := SplitDocuments([]byte("[]\n"))
		if err != nil {
			t.Fatalf("SplitDocuments([]): %v", err)
		}
		if len(docs) != 0 {
			t.Fatalf("got %d documents, want 0: %q", len(docs), docs)
		}
	})

	t.Run("a bare mapping is rejected as not a stub file", func(t *testing.T) {
		_, err := SplitDocuments([]byte("method: shop.v1.OrderService/GetOrder\n"))
		if err == nil {
			t.Fatal("SplitDocuments accepted a bare mapping; stub files hold a list")
		}
	})

	t.Run("aliases are expanded so each document stands alone", func(t *testing.T) {
		file := []byte("- &base\n  method: shop.v1.OrderService/GetOrder\n" +
			"  respond:\n    message: { note: shared }\n" +
			"- *base\n")
		docs, err := SplitDocuments(file)
		if err != nil {
			t.Fatalf("SplitDocuments: %v", err)
		}
		if len(docs) != 2 {
			t.Fatalf("got %d documents, want 2", len(docs))
		}
		if strings.Contains(docs[1], "*base") || !strings.Contains(docs[1], "note: shared") {
			t.Errorf("document 1 = %q, want the alias expanded", docs[1])
		}
	})
}
