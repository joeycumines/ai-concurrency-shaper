package transcode

import (
	"errors"
	"reflect"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

func TestResponsesToolIdentityAliasesCannotChangeClassification(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureBuiltinTools: {}}}
	for _, tool := range []string{
		`{"type":"function","TYPE":"web_search","name":"f","parameters":{"type":"object"},"strict":true}`,
		`{"TYPE":"web_search","type":"function","name":"f","parameters":{"type":"object"},"strict":true}`,
		`{"TYPE":"web_search"}`,
	} {
		t.Run(tool, func(t *testing.T) {
			body := []byte(`{"model":"m","input":"hi","tools":[` + tool + `]}`)
			result, _, err := DecodeResponsesRequest(body, policy)
			var malformed *wire.DecodeError
			if !errors.As(err, &malformed) || (malformed.Kind != wire.DecodeDuplicateKey && malformed.Kind != wire.DecodeUnknownField && malformed.Kind != wire.DecodeMissingRequired) {
				t.Fatalf("aliased tool identity must be malformed, not reclassified or dropped: err=%v tools=%+v losses=%+v", err, result.Request.Tools, result.Report.Losses)
			}
		})
	}
}

func TestResponsesBuiltinToolOpaqueFieldsRemainTolerant(t *testing.T) {
	const body = `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":true},{"type":"web_search","external_web_access":true,"extension":1,"Extension":2,"parameters":{"ID":1,"id":2}}]}`
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureBuiltinTools: {}}}
	result, _, err := DecodeResponsesRequest([]byte(body), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "f" {
		t.Fatalf("the client function must survive while only the built-in is dropped: %+v", result.Request.Tools)
	}
	losses := 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureBuiltinTools && loss.Kind == LossRecord {
			losses++
		}
	}
	if losses != 1 {
		t.Fatalf("built-in tool must take exactly one approved loss: %+v", result.Report.Losses)
	}
}

func TestWireEmbeddedFieldsDoNotBindAliases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		alias string
		new   func() any
	}{
		{
			name:  "chat assistant fields",
			body:  `{"role":"assistant","content":"hi","refusal":"denied"`,
			alias: `,"REFUSAL":"allowed"`,
			new:   func() any { return new(openaichat.Message) },
		},
		{
			name:  "chat tool fields",
			body:  `{"role":"tool","content":"hi","tool_call_id":"call_a"`,
			alias: `,"TOOL_CALL_ID":"call_b"`,
			new:   func() any { return new(openaichat.Message) },
		},
		{
			name:  "responses event base",
			body:  `{"type":"response.output_text.delta","sequence_number":0,"item_id":"i","output_index":0,"content_index":0,"delta":"x"`,
			alias: `,"SEQUENCE_NUMBER":1`,
			new:   func() any { return new(openairesponses.TextDeltaEvent) },
		},
	} {
		for name, decode := range map[string]func([]byte, any) error{"strict": wire.Decode, "tolerant": wire.DecodeTolerant} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				canonical := tc.new()
				if err := decode([]byte(tc.body+`}`), canonical); err != nil {
					t.Fatalf("canonical embedded fields rejected: %v", err)
				}
				out := tc.new()
				err := decode([]byte(tc.body+tc.alias+`}`), out)
				if name == "strict" {
					var malformed *wire.DecodeError
					if !errors.As(err, &malformed) || malformed.Kind != wire.DecodeDuplicateKey {
						t.Fatalf("embedded field alias accepted: %v", err)
					}
				} else if err != nil || !reflect.DeepEqual(out, canonical) {
					t.Fatalf("unknown alias changed embedded field: got %+v, want %+v, error %v", out, canonical, err)
				}
			})
		}
	}
}
