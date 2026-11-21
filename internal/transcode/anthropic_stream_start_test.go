package transcode

import (
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

func TestThinkingSignatureMayArriveAfterStartOnlyOnStream(t *testing.T) {
	data := []byte(`{"type":"thinking","thinking":""}`)
	b, err := decodeAnthropicStartBlock(data)
	if err != nil || b.Signature != nil || b.Thinking == nil {
		t.Fatalf("partial start %v %+v", err, b)
	}
	var full anthropicmessages.ContentBlock
	if err = wire.Decode(data, &full); err == nil {
		t.Fatal("complete/replayed unsigned thinking became accepted")
	}
	for _, bad := range []string{`{"type":"thinking"}`, `{"type":"thinking","thinking":null}`, `{"type":"thinking","thinking":"","signature":null}`, `{"type":"thinking","thinking":"","signature":7}`, `{"type":"thinking","thinking":"","text":"wrong union"}`, `{"type":"thinking","thinking":"","unknown_field":1}`, `{"type":"thinking","thinking":"","thinking":"duplicate"}`} {
		if _, err = decodeAnthropicStartBlock([]byte(bad)); err == nil {
			t.Fatalf("stream strictness relaxed: %s", bad)
		}
	}
}
