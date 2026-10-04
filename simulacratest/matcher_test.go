package simulacratest

import (
	"encoding/json"
	"math"
	"testing"

	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
)

func TestMatchBlockGolden(t *testing.T) {
	var m matchSpec
	m.add("MatchField", "a", []Matcher{Eq(int8(-8))})
	m.add("MatchField", "b", []Matcher{Eq(uint64(math.MaxUint64))})
	m.add("MatchField", "c", []Matcher{Eq(int64(math.MinInt64))})
	m.add("MatchField", "d", []Matcher{Eq(float32(0.1))})
	m.add("MatchField", "customer.region", []Matcher{In(shopv1.Region_EU, shopv1.Region_UK)})
	m.add("MatchField", "e", []Matcher{Eq(shopv1.Region(99))})
	m.add("MatchField", "note", []Matcher{Matches("urgent.*"), Ne("")})
	m.add("MatchField", "tags", []Matcher{Contains("x")})
	m.add("MatchMetadata", "authorization", []Matcher{Present(true)})
	m.setExpr("size(message.items) <= 10")
	got, _ := json.Marshal(m.block())
	// json.Marshal HTML-escapes "<" (the six characters backslash-u-0-0-3-c in
	// the golden); the server's YAML decoder reads that back as "<", and one
	// encoder everywhere keeps stub and verify documents byte-identical.
	want := `{"expr":"size(message.items) \u003c= 10","message":{"a":{"eq":-8},"b":{"eq":18446744073709551615},` +
		`"c":{"eq":-9223372036854775808},"customer.region":{"in":["EU","UK"]},"d":{"eq":0.10000000149011612},` +
		`"e":{"eq":99},"note":{"matches":"urgent.*","ne":""},"tags":{"contains":"x"}},` +
		`"metadata":{"authorization":{"present":true}}}`
	if string(got) != want || len(m.errors()) != 0 {
		t.Fatalf("got %s (%v), want %s", got, m.errors(), want)
	}
}

func TestMatchRefusesUnsupportedValues(t *testing.T) {
	const nonFinite = `simulacratest: MatchField("f"): NaN and ±Inf cannot be expressed in JSON; use Expr`
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{"[]byte", []byte("x"), `simulacratest: MatchField("f"): []uint8 is not supported by structured matchers; use Expr`},
		{"message", &shopv1.GetOrderRequest{}, `simulacratest: MatchField("f"): *shopv1.GetOrderRequest is not supported by structured matchers; use Expr`},
		{"map", map[string]int{}, `simulacratest: MatchField("f"): map[string]int is not supported by structured matchers; use Expr`},
		{"NaN", math.NaN(), nonFinite},
		{"float32 Inf", float32(math.Inf(1)), nonFinite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m matchSpec
			m.add("MatchField", "f", []Matcher{Eq(tc.v)})
			if got := m.errors(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("errors = %q, want [%q]", got, tc.want)
			}
			if b := m.block(); b != nil {
				t.Fatalf("block = %v, want nil: a refused value must not render", b)
			}
		})
	}

	t.Run("inside In", func(t *testing.T) {
		var m matchSpec
		m.add("MatchMetadata", "k", []Matcher{In("ok", []string{"no"})})
		want := `simulacratest: MatchMetadata("k"): []string is not supported by structured matchers; use Expr`
		if got := m.errors(); len(got) != 1 || got[0] != want {
			t.Fatalf("errors = %q, want [%q]", got, want)
		}
	})

	t.Run("operator twice", func(t *testing.T) {
		var m matchSpec
		m.add("MatchField", "f", []Matcher{Eq("a"), Eq("b")})
		want := `simulacratest: MatchField("f"): operator eq given twice`
		if got := m.errors(); len(got) != 1 || got[0] != want {
			t.Fatalf("errors = %q, want [%q]", got, want)
		}
	})
}

func TestEmptyMatchBlockIsNil(t *testing.T) {
	var m matchSpec
	if b := m.block(); b != nil {
		t.Fatalf("block = %v, want nil", b)
	}
	m.setExpr("true")
	if b := m.block(); len(b) != 1 || b["expr"] != "true" {
		t.Fatalf("block = %v, want only expr", b)
	}
}
