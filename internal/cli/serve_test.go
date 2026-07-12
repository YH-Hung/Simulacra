package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func TestServeJournalSizeFlagDefaultsTo1024(t *testing.T) {
	cmd := newServeCmd()
	flag := cmd.Flags().Lookup("journal-size")
	if flag == nil {
		t.Fatal("--journal-size flag is missing")
	}
	if flag.DefValue != "1024" {
		t.Fatalf("--journal-size default = %q, want 1024", flag.DefValue)
	}
}

func TestServeWatchFlagDefaultsTrue(t *testing.T) {
	cmd := newServeCmd()
	flag := cmd.Flags().Lookup("watch")
	if flag == nil {
		t.Fatal("--watch flag is missing")
	}
	if flag.DefValue != "true" {
		t.Fatalf("--watch default = %q, want true", flag.DefValue)
	}
	if err := cmd.Flags().Set("watch", "false"); err != nil {
		t.Fatalf("set --watch=false: %v", err)
	}
}

func TestReloadStubDirsKeepsInvalidStoreAndResetsValidBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	valid := `
- method: shop.v1.OrderService/GetOrder
  times: 1
  respond: { message: { note: fresh } }
`
	write(valid)
	src := &sources{protoDirs: []string{"../../testdata/protos"}}
	reg, err := src.buildRegistry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initial, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	store := stub.NewStore(initial)
	const method = "/shop.v1.OrderService/GetOrder"
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("initial Select = nil, want limited stub")
	}

	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	write("{{{ invalid yaml")
	reloadStubDirs(cmd, reg, store, []string{dir})
	if got := store.Select(method, match.Input{}); got != nil {
		t.Fatalf("Select after invalid reload = %v, want old exhausted store", got)
	}
	if !strings.Contains(output.String(), "stub error:") {
		t.Fatalf("invalid reload output = %q, want stub error", output.String())
	}

	output.Reset()
	write(valid)
	reloadStubDirs(cmd, reg, store, []string{dir})
	if got := store.Select(method, match.Input{}); got == nil {
		t.Fatal("Select after valid reload = nil, want reset times budget")
	}
	if !strings.Contains(output.String(), "1 stub(s) reloaded") {
		t.Fatalf("valid reload output = %q, want count", output.String())
	}
}

func TestServeRejectsNonPositiveJournalSize(t *testing.T) {
	for _, size := range []string{"0", "-1"} {
		t.Run(size, func(t *testing.T) {
			cmd := newServeCmd()
			cmd.SetArgs([]string{"--journal-size", size})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "journal-size must be greater than zero") {
				t.Fatalf("Execute error = %v, want journal-size validation", err)
			}
		})
	}
}
