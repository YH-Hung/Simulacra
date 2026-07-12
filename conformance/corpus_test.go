package conformance_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
)

const corpusMethod = "/conformance.v1.CorpusService/Echo"

type corpusCase struct {
	feature string
	stub    string
	req     string
	want    []string
	expect  string
}

func start(t *testing.T, stub string) *harness {
	t.Helper()
	return newHarness(t, stub)
}

func assertCorpusResponse(t *testing.T, h *harness, desc protoreflect.MessageDescriptor, got *dynamicpb.Message, body string) {
	t.Helper()
	want := h.jsonMessage(t, desc, body)
	if proto.Equal(got, want) {
		return
	}
	marshal := protojson.MarshalOptions{Resolver: h.reg.Types()}
	gotJSON, gotErr := marshal.Marshal(got)
	wantJSON, wantErr := marshal.Marshal(want)
	if gotErr != nil || wantErr != nil {
		t.Fatalf("response differs from expected; marshal got/want errors = %v/%v", gotErr, wantErr)
	}
	t.Fatalf("response = %s, want exactly %s", gotJSON, wantJSON)
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
	got := string(data)
	for _, want := range tc.want {
		if !strings.Contains(got, want) {
			t.Errorf("response %s missing %q", got, want)
		}
	}
	parsed := dynamicpb.NewMessage(desc.Output())
	if err := (protojson.UnmarshalOptions{Resolver: h.reg.Types()}).Unmarshal(data, parsed); err != nil {
		t.Fatalf("parse %s response: %v", tc.feature, err)
	}
	assertCorpusResponse(t, h, desc.Output(), parsed, tc.expect)
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
			req:    `{"big":"9007199254740993"}`,
			want:   []string{"9007199254740994"},
			expect: `{"big":"9007199254740994"}`,
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
			req:    `{"blob":"AAEC"}`,
			want:   []string{"AAEC"},
			expect: `{"blob":"AAEC"}`,
		},
		{
			feature: "presence.optional.set",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      opt_note: { present: true }
  respond:
    message: { text: note is set }
- method: conformance.v1.CorpusService/Echo
  priority: -1
  respond:
    message: { text: note is absent }
`,
			req:    `{"opt_note":""}`,
			want:   []string{"note is set"},
			expect: `{"text":"note is set"}`,
		},
		{
			feature: "presence.optional.unset",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      opt_note: { present: true }
  respond:
    message: { text: note is set }
- method: conformance.v1.CorpusService/Echo
  priority: -1
  respond:
    message: { text: note is absent }
`,
			req:    `{}`,
			want:   []string{"note is absent"},
			expect: `{"text":"note is absent"}`,
		},
		{
			feature: "presence.oneof",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      word: { present: true }
  respond:
    message: { text: word chosen }
`,
			req:    `{"word":""}`,
			want:   []string{"word chosen"},
			expect: `{"text":"word chosen"}`,
		},
		{
			feature: "wkt.timestamp",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: message.when == timestamp('2026-07-10T00:00:00Z')
  respond:
    message: { when: '2026-07-10T12:00:00Z' }
`,
			req:    `{"when":"2026-07-10T00:00:00Z"}`,
			want:   []string{"2026-07-10T12:00:00Z"},
			expect: `{"when":"2026-07-10T12:00:00Z"}`,
		},
		{
			feature: "wkt.duration",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: 'message.span >= duration("60s")'
  respond:
    message: { span: 120s }
`,
			req:    `{"span":"90s"}`,
			want:   []string{"120s"},
			expect: `{"span":"120s"}`,
		},
		{
			feature: "wkt.wrappers",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      wrapped.value: { eq: vip }
  respond:
    message: { wrapped: gold }
`,
			req:    `{"wrapped":"vip"}`,
			want:   []string{"gold"},
			expect: `{"wrapped":"gold"}`,
		},
		{
			feature: "wkt.struct",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: 'message.attrs["plan"] == "pro"'
  respond:
    message:
      attrs: { ok: true, tier: pro }
`,
			req:    `{"attrs":{"plan":"pro"}}`,
			want:   []string{`"ok":true`, `"tier":"pro"`},
			expect: `{"attrs":{"ok":true,"tier":"pro"}}`,
		},
		{
			feature: "wkt.fieldmask",
			stub: `
- method: conformance.v1.CorpusService/Echo
  respond:
    message: { mask: 'text,optNote' }
`,
			req:    `{}`,
			want:   []string{"optNote"},
			expect: `{"mask":"text,optNote"}`,
		},
		{
			feature: "map.message_values",
			stub: `
- method: conformance.v1.CorpusService/Echo
  match:
    expr: 'message.items["a"].id == "x"'
  respond:
    message:
      items:
        b: { id: y }
`,
			req:    `{"items":{"a":{"id":"x"}}}`,
			want:   []string{`"y"`},
			expect: `{"items":{"b":{"id":"y"}}}`,
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
			req:    `{"packed":[1,2,3]}`,
			want:   []string{"4", "5", "6"},
			expect: `{"packed":[4,5,6]}`,
		},
		{
			feature: "structural.recursive",
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
			req:    `{"tree":{"label":"L1","next":{"label":"L2"}}}`,
			want:   []string{"R5"},
			expect: `{"tree":{"label":"R1","next":{"label":"R2","next":{"label":"R3","next":{"label":"R4","next":{"label":"R5"}}}}}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.feature, func(t *testing.T) {
			runCorpus(t, tc)
		})
	}
}

func TestCorpusOneofPresenceRejectsUnsetAlternatives(t *testing.T) {
	h := start(t, `
- method: conformance.v1.CorpusService/Echo
  match:
    message:
      word: { present: true }
  respond:
    message: { text: word chosen }
- method: conformance.v1.CorpusService/Echo
  priority: -1
  respond:
    message: { text: word absent }
`)
	desc := h.method(t, corpusMethod, match.Unary)

	for _, req := range []string{`{}`, `{"number":0}`} {
		t.Run(req, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			response, err := h.invoke(t, ctx, corpusMethod, h.jsonMessage(t, desc.Input(), req))
			if err != nil {
				t.Fatalf("invoke %s: %v", corpusMethod, err)
			}
			assertCorpusResponse(t, h, desc.Output(), response, `{"text":"word absent"}`)
		})
	}
}
