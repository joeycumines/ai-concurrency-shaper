package transcode

import (
	"encoding/json"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// decodeAnthropicStartBlock models the partial streaming thinking arm. The
// ordinary ContentBlock decoder still requires a signature in complete messages
// and replayed client requests. No placeholder signature is synthesized here.
// https://platform.claude.com/docs/en/build-with-claude/streaming
func decodeAnthropicStartBlock(data []byte) (anthropicmessages.ContentBlock, error) {
	var probe struct {
		Type anthropicmessages.ContentBlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return anthropicmessages.ContentBlock{}, err
	}
	if probe.Type != anthropicmessages.ContentBlockTypeThinking {
		var b anthropicmessages.ContentBlock
		err := wire.Decode(data, &b)
		return b, err
	}
	var partial struct {
		Type      anthropicmessages.ContentBlockType `json:"type"`
		Thinking  wire.Field[string]                 `json:"thinking"`
		Signature wire.Field[string]                 `json:"signature"`
	}
	if err := wire.Decode(data, &partial); err != nil {
		return anthropicmessages.ContentBlock{}, err
	}
	if !partial.Thinking.Present {
		return anthropicmessages.ContentBlock{}, &wire.DecodeError{Kind: wire.DecodeMissingRequired, Path: "content_block.thinking"}
	}
	if partial.Thinking.Null || partial.Signature.Null {
		return anthropicmessages.ContentBlock{}, &wire.DecodeError{Kind: wire.DecodeIllegalNull, Path: "content_block"}
	}
	b := anthropicmessages.ContentBlock{Type: partial.Type, Thinking: &partial.Thinking.Value}
	if partial.Signature.Present {
		b.Signature = &partial.Signature.Value
	}
	return b, nil
}
