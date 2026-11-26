package transcode

import (
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestMessagesResponsePresentUsageRequiresTotals(t *testing.T) {
	for _, tc := range []struct{ usage, missing string }{
		{`{}`, "input_tokens"},
		{`{"output_tokens":0}`, "input_tokens"},
		{`{"input_tokens":0}`, "output_tokens"},
		{`{"unrelated_metadata":99,"output_tokens":0}`, "input_tokens"},
		{`{"INPUT_TOKENS":99,"output_tokens":0}`, "input_tokens"},
		{`{"input_tokens":0,"OUTPUT_TOKENS":99}`, "output_tokens"},
	} {
		t.Run(tc.usage, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":` + tc.usage + `}`
			_, _, err := DecodeMessagesResponse([]byte(body), permissiveLossPolicy())
			requireMessagesMissingUsageField(t, err, "usage."+tc.missing)
		})
	}
	for _, tc := range []struct {
		name, field string
		known       bool
	}{
		{"explicit zero totals", `,"usage":{"input_tokens":0,"output_tokens":0}`, true},
		{"zero totals with extensions", `,"usage":{"input_tokens":0,"output_tokens":0,"provider_metadata":99,"INPUT_TOKENS":99}`, true},
		{"absent object", "", false},
		{"null object", `,"usage":null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null` + tc.field + `}`
			response, _, err := DecodeMessagesResponse([]byte(body), StrictLossPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if response.Usage.InputKnown != tc.known || response.Usage.OutputKnown != tc.known || response.Usage.InputTokens != 0 || response.Usage.OutputTokens != 0 {
				t.Fatalf("explicit zero and absence were conflated: %+v", response.Usage)
			}
		})
	}
}

func TestMessagesStreamPresentUsageRequiresPerEventTotals(t *testing.T) {
	for _, tc := range []struct {
		name, kind, data, missing string
	}{
		{"start input omitted", "message_start", `"message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"output_tokens":0}}`, "message.usage.input_tokens"},
		{"start output omitted", "message_start", `"message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":0}}`, "message.usage.output_tokens"},
		{"start input alias", "message_start", `"message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"INPUT_TOKENS":99,"output_tokens":0}}`, "message.usage.input_tokens"},
		{"delta output omitted", "message_delta", `"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3}`, "usage.output_tokens"},
		{"delta empty usage", "message_delta", `"delta":{"stop_reason":"end_turn"},"usage":{}`, "usage.output_tokens"},
		{"delta output alias", "message_delta", `"delta":{"stop_reason":"end_turn"},"usage":{"OUTPUT_TOKENS":99}`, "usage.output_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeAnthropicStreamFrame(SSEEvent{Event: tc.kind, Data: []byte(`{"type":"` + tc.kind + `",` + tc.data + `}`)})
			requireMessagesMissingUsageField(t, err, tc.missing)
		})
	}
	for _, tc := range []struct{ name, kind, data string }{
		{"start zero totals", "message_start", `"message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":0,"output_tokens":0}}`},
		{"delta zero output without optional input", "message_delta", `"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}`},
		{"delta no usage object", "message_delta", `"delta":{"stop_reason":"end_turn"}`},
		{"delta null usage object", "message_delta", `"delta":{"stop_reason":"end_turn"},"usage":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeAnthropicStreamFrame(SSEEvent{Event: tc.kind, Data: []byte(`{"type":"` + tc.kind + `",` + tc.data + `}`)}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func requireMessagesMissingUsageField(t *testing.T, err error, path string) {
	t.Helper()
	var upstream *UpstreamWireError
	var malformed *wire.DecodeError
	if !errors.As(err, &upstream) || !errors.As(err, &malformed) || malformed.Kind != wire.DecodeMissingRequired || malformed.Path != path {
		t.Fatalf("missing usage count error = %T: %v, want an upstream missing-required error at %s", err, err, path)
	}
}

func TestGeminiStreamMessagesPartialUsageNeverCompletes(t *testing.T) {
	for _, tc := range []struct{ name, start, stop string }{
		{"missing initial input", strings.Replace(anthropicStartSSE, `"input_tokens":3`, `"unrelated_metadata":99`, 1), anthropicStopSSE("end_turn")},
		{"missing final output", anthropicStartSSE, strings.Replace(anthropicStopSSE("end_turn"), `"output_tokens":2`, `"unrelated_metadata":99`, 1)},
		{"alias cannot supply final output", anthropicStartSSE, strings.Replace(anthropicStopSSE("end_turn"), `"output_tokens":2`, `"OUTPUT_TOKENS":99`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveGeminiStreamBody(t, geminiMessagesStreamHandler(t, tc.start+anthropicTextBlockSSE+tc.stop, nil), geminiMessagesStreamTarget, geminiMessagesBody)
			frames := geminiStreamFrames(t, rec.Body.String())
			if len(frames) == 0 {
				t.Fatalf("no error frame: %s", rec.Body.String())
			}
			for _, frame := range frames {
				if finish := geminiFrameFinish(frame); finish != "" {
					t.Fatalf("incomplete usage produced successful completion %q: %s", finish, rec.Body.String())
				}
			}
			if _, ok := frames[len(frames)-1]["error"]; !ok {
				t.Fatalf("malformed usage did not end in an error: %s", rec.Body.String())
			}
		})
	}
}
