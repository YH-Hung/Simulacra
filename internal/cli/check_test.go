package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCheckValidStubs(t *testing.T) {
	dir := t.TempDir()
	stubs := `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { order_id: "x" }
`
	if err := os.WriteFile(filepath.Join(dir, "s.yaml"), []byte(stubs), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "check", "--proto", "../../testdata/protos", "--stubs", dir)
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 stub") {
		t.Errorf("output %q should summarize the stub count", out)
	}
}

func TestCheckReportsEveryError(t *testing.T) {
	dir := t.TempDir()
	stubs := `
- method: shop.v1.OrderService/Nope
  respond: { message: {} }
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { bad_field: 1 }
`
	if err := os.WriteFile(filepath.Join(dir, "s.yaml"), []byte(stubs), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "check", "--proto", "../../testdata/protos", "--stubs", dir)
	if err == nil {
		t.Fatal("check should fail for invalid stubs")
	}
	if !strings.Contains(out, "Nope") || !strings.Contains(out, "bad_field") {
		t.Errorf("output should mention both errors, got:\n%s", out)
	}
}

func TestCheckRequiresSchemaSource(t *testing.T) {
	if _, err := run(t, "check", "--stubs", t.TempDir()); err == nil {
		t.Fatal("check without --proto/--descriptors should fail")
	}
}
