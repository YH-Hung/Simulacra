package stub

import (
	"os"
	"strings"
	"testing"

	"github.com/yinghanhung/simulacra/internal/match"
)

func TestParseDocumentAcceptsYAMLMapping(t *testing.T) {
	s, doc, err := ParseDocument([]byte("# a comment\nmethod: a.B/C # inline\nrespond:\n  message: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Method != "a.B/C" {
		t.Fatalf("method = %q", s.Method)
	}
	want := "method: a.B/C\nrespond:\n    message: {}\n"
	if doc != want {
		t.Fatalf("normalized document:\n%q\nwant:\n%q", doc, want)
	}
}

func TestParseDocumentAcceptsJSONAndNormalizesToYAML(t *testing.T) {
	s, doc, err := ParseDocument([]byte(`{"method": "a.B/C", "times": 3, "respond": {"message": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Method != "a.B/C" || s.Times != 3 {
		t.Fatalf("stub = %+v", s)
	}
	if strings.Contains(doc, "{\"") {
		t.Fatalf("document kept JSON flow style:\n%s", doc)
	}
	// Normalization is a fixed point: re-parsing the document re-renders it.
	s2, doc2, err := ParseDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if doc2 != doc {
		t.Fatalf("not a fixed point:\n%q\nvs\n%q", doc, doc2)
	}
	if s2.Method != s.Method || s2.Times != s.Times {
		t.Fatalf("round trip changed the stub: %+v vs %+v", s, s2)
	}
}

func TestParseDocumentRejectsSequence(t *testing.T) {
	_, _, err := ParseDocument([]byte("- method: a.B/C\n"))
	if err == nil || !strings.Contains(err.Error(), "files take lists") {
		t.Fatalf("err = %v, want sequence rejection pointing at the file grammar", err)
	}
}

func TestParseDocumentRejectsMultipleDocuments(t *testing.T) {
	_, _, err := ParseDocument([]byte("method: a.B/C\n---\nmethod: a.B/D\n"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("err = %v, want exactly-one rejection", err)
	}
}

func TestParseDocumentRejectsEmptyInput(t *testing.T) {
	for _, in := range []string{"", "   \n", "\n\n"} {
		if _, _, err := ParseDocument([]byte(in)); err == nil {
			t.Fatalf("empty input %q accepted, want error", in)
		}
	}
}

func TestParseDocumentRejectsUnknownFieldWithPosition(t *testing.T) {
	_, _, err := ParseDocument([]byte("method: a.B/C\nbogus: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want unknown-field error with the input's own position", err)
	}
}

func TestParseDocumentRejectsScalar(t *testing.T) {
	if _, _, err := ParseDocument([]byte("just a string\n")); err == nil {
		t.Fatal("scalar document accepted, want mapping-required error")
	}
}

func TestRenderSequenceRoundTripsThroughFileGrammar(t *testing.T) {
	// Canonical per-stub documents come from ParseDocument itself, so this
	// test never hardcodes yaml.v3's indentation choices.
	var docs []string
	for _, in := range []string{
		"method: a.B/C\nrespond:\n  message: {}\n",
		"method: a.B/D\ntimes: 2\nrespond:\n  stream:\n  - message:\n      x: 1\n  - status:\n      code: 5\n",
	} {
		_, doc, err := ParseDocument([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
	}
	out, err := RenderSequence(docs)
	if err != nil {
		t.Fatal(err)
	}
	// The output must be one file-grammar sequence parseFile accepts.
	dir := t.TempDir()
	path := dir + "/export.yaml"
	if err := writeTestFile(t, path, out); err != nil {
		t.Fatal(err)
	}
	stubs, perStub, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile of rendered export: %v", err)
	}
	if len(stubs) != 2 {
		t.Fatalf("parsed %d stubs, want 2", len(stubs))
	}
	if stubs[0].Method != "a.B/C" || stubs[1].Method != "a.B/D" || stubs[1].Times != 2 {
		t.Fatalf("round-tripped stubs differ: %+v", stubs)
	}
	if len(perStub) != 2 || perStub[0] != docs[0] || perStub[1] != docs[1] {
		t.Fatalf("per-stub documents did not round trip:\n%q\nwant\n%q", perStub, docs)
	}
}

func TestRenderSequenceOfNothingIsEmptyList(t *testing.T) {
	out, err := RenderSequence(nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "[]\n" {
		t.Fatalf("empty export = %q, want %q", out, "[]\n")
	}
}

func writeTestFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestParseMatchDocumentEmptyMeansMatchAll(t *testing.T) {
	for _, in := range []string{"", "   ", "\n"} {
		b, err := ParseMatchDocument([]byte(in))
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		if b != nil {
			t.Fatalf("input %q: block = %+v, want nil (match-all)", in, b)
		}
	}
}

func TestParseMatchDocumentDecodesStrictly(t *testing.T) {
	b, err := ParseMatchDocument([]byte("message:\n  order_id: { eq: o-1 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Message["order_id"]["eq"] != "o-1" {
		t.Fatalf("block = %+v", b)
	}
	if _, err := ParseMatchDocument([]byte("bogus: 1\n")); err == nil {
		t.Fatal("unknown matcher field accepted, want error")
	}
	if _, err := ParseMatchDocument([]byte("- message\n")); err == nil {
		t.Fatal("sequence matcher accepted, want error")
	}
}

func TestCompileMatchCompilesAgainstTheMethod(t *testing.T) {
	reg := testRegistry(t)
	c := NewCompiler(reg)
	compiled, err := c.CompileMatch("shop.v1.OrderService/GetOrder", &match.Block{
		Message: map[string]match.Rules{"order_id": {"eq": "o-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled == nil {
		t.Fatal("nil compiled matcher")
	}
	if _, err := c.CompileMatch("no.Such/Method", nil); err == nil {
		t.Fatal("unknown method accepted, want error")
	}
	if _, err := c.CompileMatch("shop.v1.OrderService/GetOrder", &match.Block{
		Message: map[string]match.Rules{"no_such_field": {"eq": "x"}},
	}); err == nil {
		t.Fatal("bad field path accepted, want compile error")
	}
}
