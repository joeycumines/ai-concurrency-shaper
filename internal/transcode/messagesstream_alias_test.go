package transcode

import (
	"strings"
	"testing"
)

// The tolerant provider envelope uses exact JSON field names. An unknown
// extension must not overwrite the canonical stop reason through Go's
// case-insensitive struct-field matching, in either encounter order.
func TestGeminiStreamMessagesCanonicalStopReasonIgnoresExtensions(t *testing.T) {
	for _, test := range []struct {
		name, delta, finish string
	}{
		{"refusal before long-s extension", `"stop_reason":"refusal","ſtop_reason":"end_turn"`, "SAFETY"},
		{"refusal after long-s extension", `"ſtop_reason":"end_turn","stop_reason":"refusal"`, "SAFETY"},
		{"completion before long-s extension", `"stop_reason":"end_turn","ſtop_reason":"refusal"`, "STOP"},
		{"completion after long-s extension", `"ſtop_reason":"refusal","stop_reason":"end_turn"`, "STOP"},
		{"refusal before uppercase extension", `"stop_reason":"refusal","STOP_REASON":"end_turn"`, "SAFETY"},
		{"refusal after uppercase extension", `"STOP_REASON":"end_turn","stop_reason":"refusal"`, "SAFETY"},
		{"null extension does not erase refusal", `"stop_reason":"refusal","ſtop_reason":null`, "SAFETY"},
		{"extension type does not constrain canonical field", `"stop_reason":"refusal","ſtop_reason":{"opaque":1}`, "SAFETY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sse := anthropicStartSSE + anthropicTextBlockSSE +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{` + test.delta + `,"stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
			assertMessagesEnvelopeStream(t, sse, test.finish)
		})
	}
}

// These spellings are unknown on the extensible envelope. They must be
// skipped before decoding can bind them to modeled fields or validate them
// against another event arm. Canonical spellings remain subject to the
// event-arm checks in TestGeminiStreamMessagesRejectsContentOutsideEventArms.
func TestGeminiStreamMessagesToleratesNoncanonicalEnvelopeNames(t *testing.T) {
	for _, test := range []struct {
		name, kind, fields string
	}{
		{"null text on metadata delta", "message_delta", `"delta":{"stop_reason":"end_turn","TEXT":null}`},
		{"text on metadata delta", "message_delta", `"delta":{"stop_reason":"end_turn","TEXT":"opaque extension"}`},
		{"null type on metadata delta", "message_delta", `"delta":{"stop_reason":"end_turn","TYPE":null}`},
		{"type on metadata delta", "message_delta", `"delta":{"stop_reason":"end_turn","TYPE":"text_delta"}`},
		{"null content on ping", "ping", `"CONTENT_BLOCK":null`},
		{"content on ping", "ping", `"CONTENT_BLOCK":{"type":"text","text":"opaque extension"}`},
		{"null error on ping", "ping", `"ERROR":null`},
		{"error on ping", "ping", `"ERROR":{"type":"overloaded_error","message":"opaque extension"}`},
		{"delta on ping", "ping", `"DELTA":{"type":"text_delta","text":"opaque extension"}`},
		{"index on ping", "ping", `"INDEX":0`},
		{"usage on ping", "ping", `"USAGE":{"output_tokens":999}`},
		{"event type extension", "ping", `"TYPE":"error"`},
		{"delta extension does not erase canonical metadata", "message_delta", `"delta":{"stop_reason":"end_turn"},"DELTA":null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			sse := anthropicStartSSE + anthropicTextBlockSSE +
				"event: " + test.kind + "\n" +
				`data: {"type":"` + test.kind + `",` + test.fields + `}` + "\n\n" +
				anthropicStopSSE("end_turn")
			assertMessagesEnvelopeStream(t, sse, "STOP")
		})
	}
}

// Ignoring an unknown extension must not let it satisfy a missing canonical
// member. A stop reason, event discriminator, block index or block payload
// supplied only under another spelling cannot authorize stream completion.
func TestGeminiStreamMessagesRequiresCanonicalMemberNames(t *testing.T) {
	for _, test := range []struct {
		name, suffix string
	}{
		{
			name: "stop reason only has a long-s spelling",
			suffix: "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"ſtop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
		},
		{
			name: "event type only has an uppercase spelling",
			suffix: "event: ping\n" + `data: {"TYPE":"ping"}` + "\n\n" +
				anthropicStopSSE("end_turn"),
		},
		{
			name: "block index only has an uppercase spelling",
			suffix: "event: content_block_start\n" +
				`data: {"type":"content_block_start","INDEX":1,"content_block":{"type":"text","text":"hidden"}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n" +
				anthropicStopSSE("end_turn"),
		},
		{
			name: "block payload only has an uppercase spelling",
			suffix: "event: content_block_start\n" +
				`data: {"type":"content_block_start","index":1,"CONTENT_BLOCK":{"type":"text","text":"hidden"}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n" +
				anthropicStopSSE("end_turn"),
		},
		{
			name: "canonical stop reason remains type checked",
			suffix: "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":{},"STOP_REASON":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertMessagesEnvelopeStream(t, anthropicStartSSE+anthropicTextBlockSSE+test.suffix, "")
		})
	}
}

// Content blocks and their deltas remain strict unions even inside tolerant
// event envelopes. Unknown spellings on these content-bearing surfaces are
// errors, including an otherwise inert null.
func TestGeminiStreamMessagesContentUnionsRejectNoncanonicalNames(t *testing.T) {
	for _, test := range []struct {
		name, suffix string
	}{
		{
			name: "content block field",
			suffix: "event: content_block_start\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"","TEXT":null}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n",
		},
		{
			name: "content block discriminator",
			suffix: "event: content_block_start\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"TYPE":"text","text":""}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n",
		},
		{
			name: "content delta field",
			suffix: "event: content_block_start\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"","TEXT":null}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertMessagesEnvelopeStream(t, anthropicStartSSE+anthropicTextBlockSSE+test.suffix+anthropicStopSSE("end_turn"), "")
		})
	}
}

// Unknown provider metadata is discarded. Its arbitrary object keys do not
// acquire the case-insensitive matching rules of modeled Go struct fields.
func TestGeminiStreamMessagesToleratesCaseDistinctProviderMetadata(t *testing.T) {
	sse := anthropicStartSSE + anthropicTextBlockSSE +
		"event: ping\n" +
		`data: {"type":"ping","provider_metric":1,"PROVIDER_METRIC":2,"provider_metadata":{"Input":1,"input":2,"stop_reason":"opaque","ſtop_reason":"opaque"}}` + "\n\n" +
		anthropicStopSSE("end_turn")
	assertMessagesEnvelopeStream(t, sse, "STOP")
}

// A nonempty finish requires that exact terminal; empty requires an error.
// All cases place the tested event after a complete hello text block.
func assertMessagesEnvelopeStream(t *testing.T, sse, finish string) {
	t.Helper()
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, sse, nil),
		geminiMessagesStreamTarget, geminiMessagesBody,
	)
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if got := geminiFrameText(frames); got != "hello" {
		t.Fatalf("text = %q, want hello: %s", got, rec.Body.String())
	}
	if got := geminiFrameFinish(frames[len(frames)-1]); got != finish {
		t.Fatalf("finishReason = %q, want %q: %s", got, finish, rec.Body.String())
	}
	if _, isError := frames[len(frames)-1]["error"]; isError != (finish == "") {
		t.Fatalf("error terminal = %v, want %v: %s", isError, finish == "", rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "provider_") || strings.Contains(rec.Body.String(), "opaque extension") {
		t.Fatalf("provider metadata leaked to the client: %s", rec.Body.String())
	}
}
