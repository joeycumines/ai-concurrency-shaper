package transcode

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// TestDecodeAnthropicStreamFrameCaseAliasStopReason proves a case-variant
// duplicate key cannot launder a stop reason: {"stop_reason":"refusal",
// "STOP_REASON":"end_turn"} would let the uppercase key overwrite the modeled
// field under encoding/json's case-insensitive matching, turning a refusal
// into a success terminal. The wire layer rejects the case-variant duplicate,
// so the frame fails before the stream state ever sees it.
func TestDecodeAnthropicStreamFrameCaseAliasStopReason(t *testing.T) {
	frame := SSEEvent{
		Event: "message_delta",
		Data:  []byte(`{"type":"message_delta","delta":{"stop_reason":"refusal","STOP_REASON":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`),
	}
	_, err := decodeAnthropicStreamFrame(frame)
	if err == nil {
		t.Fatal("case-alias stop_reason decoded without error")
	}
	if !isUpstreamConversionError(err) {
		t.Fatalf("err = %T %v, want an upstream conversion error", err, err)
	}
	if !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "STOP_REASON") {
		t.Fatalf("error should name the duplicate key: %v", err)
	}

	// The same alias on the message_start envelope is rejected too.
	start := SSEEvent{
		Event: "message_start",
		Data:  []byte(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"m","stop_reason":"refusal","Stop_Reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
	}
	if _, err := decodeAnthropicStreamFrame(start); err == nil || !isUpstreamConversionError(err) {
		t.Fatalf("message_start case alias: err = %T %v, want an upstream conversion error", err, err)
	}

	// A control: the exact duplicate is rejected (pre-existing behavior),
	// and a legitimately distinct key pair decodes fine.
	if _, err := decodeAnthropicStreamFrame(SSEEvent{
		Event: "message_delta",
		Data:  []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_reason":"refusal"}}`),
	}); err == nil {
		t.Fatal("exact duplicate stop_reason decoded without error")
	}
	var ev struct {
		Type  string `json:"type"`
		Delta *struct {
			StopReason   *string `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
	}
	if err := wire.Decode([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`), &ev); err != nil {
		t.Fatalf("legitimate delta rejected: %v", err)
	}
	if ev.Delta == nil || ev.Delta.StopReason == nil || *ev.Delta.StopReason != "end_turn" {
		t.Fatalf("legitimate delta = %+v", ev)
	}
}
