package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func TestOutputFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		format string
		wantOK bool
	}{
		{"text", true},
		{"json", true},
		{"yaml", false},
		{"JSON", false},
		{"", false},
	} {
		t.Run(tc.format, func(t *testing.T) {
			f := &outputFlag{format: tc.format}
			err := f.validate()
			if tc.wantOK && err != nil {
				t.Fatalf("validate(%q) = %v, want nil", tc.format, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("validate(%q) = nil, want an error", tc.format)
			}
		})
	}
}

// -o must not be bound to --output: it is stub export's --out (design §4).
func TestOutputFlagHasNoShorthand(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	(&outputFlag{}).register(cmd)
	flag := cmd.Flags().Lookup("output")
	if flag == nil {
		t.Fatal("--output was not registered")
	}
	if flag.Shorthand != "" {
		t.Fatalf("--output has shorthand %q; -o belongs to stub export's --out", flag.Shorthand)
	}
	if flag.DefValue != "text" {
		t.Fatalf("--output default = %q, want text", flag.DefValue)
	}
}

// Scalar defaults are emitted so a script never reads null for a false, and
// field names are proto names, matching matcher paths and the stub grammar.
func TestJSONRendersDefaultsAndProtoNames(t *testing.T) {
	var buf bytes.Buffer
	resp := &adminv1.VerifyCallsResponse{Passed: false, Matched: 0}
	if err := writeJSON(&buf, resp); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	fields := decodeJSON(t, buf.String())
	passed, ok := fields["passed"]
	if !ok {
		t.Fatalf("passed is absent from %s; a script would read null", buf.String())
	}
	if passed != false {
		t.Fatalf("passed = %v, want false", passed)
	}
	if _, ok := fields["matched"]; !ok {
		t.Fatalf("matched is absent from %s", buf.String())
	}
}

func TestJSONUsesProtoFieldNamesAndEnumNames(t *testing.T) {
	var buf bytes.Buffer
	resp := &adminv1.ListStubsResponse{Stubs: []*adminv1.Stub{{
		Id:     "api-1",
		Method: "/shop.v1.OrderService/GetOrder",
		Shape:  adminv1.StubShape_STUB_SHAPE_UNARY,
		Origin: adminv1.StubOrigin_STUB_ORIGIN_API,
	}}}
	if err := writeJSON(&buf, resp); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	text := buf.String()
	if !strings.Contains(text, `"STUB_SHAPE_UNARY"`) {
		t.Errorf("enums are not rendered by name: %s", text)
	}
	// matched_stub_id would be matchedStubId under camelCase; assert a
	// snake_case key is present on this message instead.
	fields := decodeJSON(t, text)
	stubs, _ := fields["stubs"].([]any)
	if len(stubs) != 1 {
		t.Fatalf("stubs = %v, want one entry", fields["stubs"])
	}
	first, _ := stubs[0].(map[string]any)
	if _, ok := first["id"]; !ok {
		t.Errorf("id is absent: %s", text)
	}
}

// A stream has no enclosing document, so tail emits one compact object per
// line (design §4).
func TestWriteJSONLineIsSingleLineAndNewlineTerminated(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 2; i++ {
		msg := &adminv1.WatchCallsResponse{Call: &adminv1.Call{Seq: uint64(i + 1), Method: "/shop.v1.OrderService/GetOrder"}}
		if err := writeJSONLine(&buf, msg); err != nil {
			t.Fatalf("writeJSONLine: %v", err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		fields := decodeJSON(t, line)
		call, _ := fields["call"].(map[string]any)
		if call == nil {
			t.Fatalf("line %d has no call: %q", i, line)
		}
		// int64/uint64 render as JSON strings; that is protojson's rule and
		// scripts need `| tonumber`.
		if _, ok := call["seq"].(string); !ok {
			t.Errorf("line %d seq = %#v, want a JSON string", i, call["seq"])
		}
	}
}

func TestNewTableAlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	w := newTable(&buf)
	if _, err := w.Write([]byte("ID\tMETHOD\nlong-id-value\t/a/B\nx\t/c/D\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if strings.Index(lines[1], "/a/B") != strings.Index(lines[2], "/c/D") {
		t.Errorf("columns are not aligned:\n%s", buf.String())
	}
}
