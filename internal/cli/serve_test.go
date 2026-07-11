package cli

import "testing"

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
