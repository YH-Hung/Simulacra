package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yinghanhung/simulacra/internal/match"
)

const corpusMethod = "/conformance.v1.CorpusService/Echo"

type corpusCase struct {
	feature string
	stub    string
	req     string
	want    string
}

func start(t *testing.T, stub string) *harness {
	t.Helper()
	return newHarness(t, stub)
}

func runCorpus(t *testing.T, tc corpusCase) {
	t.Helper()
	h := start(t, tc.stub)
	desc := h.method(t, corpusMethod, match.Unary)
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	response, err := h.invoke(t, ctx, corpusMethod, h.jsonMessage(t, desc.Input(), tc.req))
	if err != nil {
		t.Fatalf("invoke %s: %v", tc.feature, err)
	}
	data, err := (protojson.MarshalOptions{Resolver: h.reg.Types()}).Marshal(response)
	if err != nil {
		t.Fatalf("marshal %s response: %v", tc.feature, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatalf("compact %s response: %v", tc.feature, err)
	}
	if got := compact.String(); !strings.Contains(got, tc.want) {
		t.Fatalf("response %s missing %q", got, tc.want)
	}
}

func TestCorpusHardCasesI(t *testing.T) {
	cases := []corpusCase{
		{
			feature: "int64.precision",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      big: { eq: 9007199254740993 }
  respond:
    message:
      big: '{{ message.big + 1 }}'
`,
			req:  `{"big":"9007199254740993"}`,
			want: `"big":"9007199254740994"`,
		},
		{
			feature: "bytes.roundtrip",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.blob == b'\x00\x01\x02'
  respond:
    message: { blob: AAEC }
`,
			req:  `{"blob":"AAEC"}`,
			want: `"blob":"AAEC"`,
		},
		{
			feature: "optional.set",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      opt_note: { present: true }
  respond:
    message: { text: note is set }
- method: conformance.v1.CorpusService/Echo
  respond:
    message: { text: fallback }
`,
			req:  `{"optNote":""}`,
			want: `"text":"note is set"`,
		},
		{
			feature: "optional.unset",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      opt_note: { present: true }
  respond:
    message: { text: note is set }
- method: conformance.v1.CorpusService/Echo
  respond:
    message: { text: note absent }
`,
			req:  `{}`,
			want: `"text":"note absent"`,
		},
		{
			feature: "oneof.word.empty",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      word: { present: true }
  respond:
    message: { text: word chosen }
`,
			req:  `{"word":""}`,
			want: `"text":"word chosen"`,
		},
		{
			feature: "timestamp.cel",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.when == timestamp('2026-07-10T00:00:00Z')
  respond:
    message: { when: '2026-07-10T12:00:00Z' }
`,
			req:  `{"when":"2026-07-10T00:00:00Z"}`,
			want: `"when":"2026-07-10T12:00:00Z"`,
		},
		{
			feature: "duration.cel",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.span >= duration('60s')
  respond:
    message: { span: 120s }
`,
			req:  `{"span":"90s"}`,
			want: `"span":"120s"`,
		},
		{
			feature: "wrapper.nested",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      wrapped.value: { eq: vip }
  respond:
    message: { wrapped: gold }
`,
			req:  `{"wrapped":"vip"}`,
			want: `"wrapped":"gold"`,
		},
		{
			feature: "struct.cel",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.attrs['plan'] == 'pro'
  respond:
    message:
      attrs: { ok: true, tier: pro }
`,
			req:  `{"attrs":{"plan":"pro"}}`,
			want: `"attrs":{"ok":true,"tier":"pro"}`,
		},
		{
			feature: "fieldmask.response",
			stub: `
- method: conformance.v1.CorpusService/Echo
  respond:
    message: { mask: 'text,optNote' }
`,
			req:  `{}`,
			want: `"mask":"text,optNote"`,
		},
		{
			feature: "map.message.cel",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.items['a'].id == 'x'
  respond:
    message:
      items:
        b: { id: y }
`,
			req:  `{"items":{"a":{"id":"x"}}}`,
			want: `"items":{"b":{"id":"y"}}`,
		},
		{
			feature: "repeated.packed",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.packed.exists(n, n == 2)
  respond:
    message: { packed: [4, 5, 6] }
`,
			req:  `{"packed":[1,2,3]}`,
			want: `"packed":[4,5,6]`,
		},
		{
			feature: "recursive.structured",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      tree.next.label: { eq: L2 }
  respond:
    message:
      tree:
        label: R1
        next:
          label: R2
          next:
            label: R3
            next:
              label: R4
              next: { label: R5 }
`,
			req:  `{"tree":{"label":"L1","next":{"label":"L2"}}}`,
			want: `"label":"R5"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.feature, func(t *testing.T) {
			runCorpus(t, tc)
		})
	}
}
