package transcode

import (
	"errors"
	"fmt"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Provider envelope names are exact: uppercase and Unicode case-fold aliases
// are unknown metadata, so they must not overwrite or supply a stop reason.
// Exact duplicate keys remain malformed independently of envelope tolerance.
func TestDecodeAnthropicStreamFrameCaseAliasStopReason(t *testing.T) {
	for kind, envelope := range map[string]string{
		"message_delta": `{"type":"message_delta","delta":{%s},"usage":{"output_tokens":2}}`,
		"message_start": `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"m",%s,"usage":{"input_tokens":1,"output_tokens":0}}}`,
	} {
		t.Run(kind, func(t *testing.T) {
			for _, fields := range []string{
				`"stop_reason":"refusal","STOP_REASON":"end_turn"`,
				`"STOP_REASON":"end_turn","stop_reason":"refusal"`,
				`"stop_reason":"refusal","Stop_Reason":"end_turn"`,
				`"Stop_Reason":"end_turn","stop_reason":"refusal"`,
				`"stop_reason":"refusal","ſtop_reason":"end_turn"`,
				`"ſtop_reason":"end_turn","stop_reason":"refusal"`,
			} {
				t.Run(fields, func(t *testing.T) {
					event, err := decodeAnthropicStreamFrame(SSEEvent{
						Event: kind,
						Data:  []byte(fmt.Sprintf(envelope, fields+`,"stop_sequence":null`)),
					})
					if err != nil {
						t.Fatalf("unknown provider metadata rejected: %v", err)
					}
					if kind == "message_delta" {
						if event.Delta == nil || event.Delta.StopReason == nil || *event.Delta.StopReason != "refusal" {
							t.Fatalf("canonical refusal changed: %+v", event.Delta)
						}
					} else if event.Message == nil || event.Message.StopReason == nil || *event.Message.StopReason != "refusal" {
						t.Fatalf("canonical refusal changed: %+v", event.Message)
					}
				})
			}

			// An exact duplicate remains an upstream wire error.
			_, err := decodeAnthropicStreamFrame(SSEEvent{
				Event: kind,
				Data:  []byte(fmt.Sprintf(envelope, `"stop_reason":"end_turn","stop_reason":"refusal","stop_sequence":null`)),
			})
			var malformed *wire.DecodeError
			if !isUpstreamConversionError(err) || !errors.As(err, &malformed) || malformed.Kind != wire.DecodeDuplicateKey {
				t.Fatalf("exact duplicate error = %T: %v, want an upstream duplicate-key failure", err, err)
			}

			// stop_reason is nullable at decode. Its noncanonical spelling
			// leaves it absent; the stream state requires the canonical value
			// before authorizing a terminal completion.
			event, err := decodeAnthropicStreamFrame(SSEEvent{
				Event: kind,
				Data:  []byte(fmt.Sprintf(envelope, `"STOP_REASON":"end_turn","stop_sequence":null`)),
			})
			if err != nil {
				t.Fatalf("unknown stop metadata rejected: %v", err)
			}
			if kind == "message_delta" {
				if event.Delta == nil || event.Delta.StopReason != nil {
					t.Fatalf("unknown spelling supplied stop_reason: %+v", event.Delta)
				}
			} else if event.Message == nil || event.Message.StopReason != nil {
				t.Fatalf("unknown spelling supplied stop_reason: %+v", event.Message)
			}
		})
	}

	// The event discriminator itself must be supplied canonically.
	if _, err := decodeAnthropicStreamFrame(SSEEvent{
		Event: "ping", Data: []byte(`{"TYPE":"ping"}`),
	}); err == nil || !isUpstreamConversionError(err) {
		t.Fatalf("missing canonical event type: err = %T %v, want upstream wire error", err, err)
	}
}
