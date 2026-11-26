package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Schemas retain their raw bytes on the wire, but encoding/json substitutes
// U+FFFD for unpaired surrogates and invalid UTF-8 when decoding into strings.
// Equality of that substituted view must not bypass a schema-change loss.
func TestMessagesToolSchemaUnicodeReplacementRequiresLoss(t *testing.T) {
	stringSchema := func(value string) string {
		return `{"type":"object","properties":{"value":{"const":` + value + `}}}`
	}
	propertySchema := func(name string) string {
		return `{"type":"object","properties":{` + name + `:{"type":"string"}},"required":[` + name + `]}`
	}
	for _, tc := range []struct {
		name, previous, latest string
	}{
		{"distinct high surrogates", stringSchema(`"\ud800"`), stringSchema(`"\ud801"`)},
		{"distinct low surrogates", stringSchema(`"\udc00"`), stringSchema(`"\udc01"`)},
		{"surrogate versus replacement character", stringSchema(`"\ud800"`), stringSchema(`"�"`)},
		{"surrogate followed by another escape", stringSchema(`"\ud800\u0061"`), stringSchema(`"\ud801a"`)},
		{"distinct property names", propertySchema(`"\ud800"`), propertySchema(`"\ud801"`)},
		{"invalid UTF-8", stringSchema("\"" + string([]byte{0xff}) + "\""), stringSchema("\"" + string([]byte{0xfe}) + "\"")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := messagesSchemaReplacementRequest(tc.previous, tc.latest)
			result, err := DecodeMessagesRequest(body, StrictLossPolicy())
			if err == nil {
				t.Fatalf("different raw schema accepted under strict policy: tools=%+v report=%+v", result.Request.Tools, result.Report.Losses)
			}
			var unsupported *UnsupportedFeatureError
			if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureAnthropicToolChanges) ||
				unsupported.Path != "messages[1].content[0].tool.definition" {
				t.Fatalf("schema change error = %T: %v, want anthropic_tool_changes at its source definition", err, err)
			}

			result, err = DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}})
			if err != nil {
				t.Fatalf("approved schema replacement: %v", err)
			}
			if len(result.Request.Tools) != 1 || !bytes.Equal(result.Request.Tools[0].JSONSchema, []byte(tc.latest)) {
				t.Fatalf("new schema was normalized or lost: %+v", result.Request.Tools)
			}
			losses, notes := 0, 0
			for _, entry := range result.Report.Losses {
				if entry.Feature != FeatureAnthropicToolChanges {
					continue
				}
				switch entry.Kind {
				case LossRecord:
					if entry.Path != "messages[1].content[0].tool.definition" {
						t.Fatalf("wrong schema replacement loss path: %+v", entry)
					}
					losses++
				case NoteRecord:
					if entry.Path != "messages[1].content[0]" || !strings.Contains(entry.Detail, "redefined") {
						t.Fatalf("schema replacement not explained by its note: %+v", entry)
					}
					notes++
				}
			}
			if losses != 1 || notes != 1 {
				t.Fatalf("schema replacement produced %d losses and %d notes, want one each: %+v", losses, notes, result.Report.Losses)
			}
			// Reading the rendered parameters as RawMessage avoids repeating the
			// very string normalization that masked the source schema change.
			rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
			if err != nil {
				t.Fatalf("render approved schema: %v", err)
			}
			var out struct {
				Tools []struct {
					Function struct {
						Parameters json.RawMessage `json:"parameters"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(rendered, &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Tools) != 1 || !bytes.Equal(out.Tools[0].Function.Parameters, []byte(tc.latest)) {
				t.Fatalf("rendered schema changed after approval: %s", rendered)
			}
		})
	}
}

func TestMessagesToolSchemaLosslessUnicodeEquality(t *testing.T) {
	for _, tc := range []struct {
		name, previous, latest string
	}{
		{"surrogate pair and UTF-8 value", `{"type":"object","properties":{"value":{"const":"\ud83d\ude00"}}}`, `{"properties":{"value":{"const":"😀"}},"type":"object"}`},
		{"surrogate pair and UTF-8 property", `{"type":"object","properties":{"\uD83D\uDE00":{"type":"string"}}}`, `{"properties":{"😀":{"type":"string"}},"type":"object"}`},
		{"literal backslash-u text", `{"type":"object","properties":{"value":{"const":"\\ud800"}}}`, `{"properties":{"value":{"const":"\u005cud800"}},"type":"object"}`},
		{"literal replacement character", `{"type":"object","properties":{"value":{"const":"\ufffd"}}}`, `{"properties":{"value":{"const":"�"}},"type":"object"}`},
		{"identical unpaired surrogate", `{"type":"object","properties":{"value":{"const":"\ud800"}}}`, `{"type":"object","properties":{"value":{"const":"\ud800"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := DecodeMessagesRequest(messagesSchemaReplacementRequest(tc.previous, tc.latest), StrictLossPolicy())
			if err != nil {
				t.Fatalf("unchanged schema required loss approval: %v", err)
			}
			if len(result.Request.Tools) != 1 || !bytes.Equal(result.Request.Tools[0].JSONSchema, []byte(tc.latest)) {
				t.Fatalf("repeated definition lost the latest schema spelling: %+v", result.Request.Tools)
			}
			if len(result.Report.Losses) != 1 || result.Report.Losses[0].Feature != FeatureAnthropicToolChanges ||
				result.Report.Losses[0].Kind != NoteRecord || !strings.Contains(result.Report.Losses[0].Detail, "repeats") {
				t.Fatalf("equal schema was reported as a replacement: %+v", result.Report.Losses)
			}
		})
	}
}

func messagesSchemaReplacementRequest(previous, latest string) []byte {
	return []byte(`{"model":"m","max_tokens":16,"tools":[{"name":"db","input_schema":` + previous + `}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":` + latest + `}}}]}]}`)
}
