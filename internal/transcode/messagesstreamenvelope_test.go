package transcode

import (
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

type anthropicEnvelopeFailure struct {
	name   string
	frames []SSEEvent
}

// Content carried outside its event arm must not bypass block validation or
// vanish before a successful terminal, even when its own shape is valid.
func anthropicEnvelopeFailures() []anthropicEnvelopeFailure {
	var cases []anthropicEnvelopeFailure
	for name, block := range map[string]string{
		"listing": anthropicListingBlockJSON,
		"text":    `{"type":"text","text":"must not disappear"}`,
	} {
		start := ignoredAnthropicStart()
		start.Data = []byte(strings.Replace(string(start.Data), `"content":[]`, `"content":[`+block+`]`, 1))
		cases = append(cases, anthropicEnvelopeFailure{
			name: "message_start contains " + name, frames: []SSEEvent{start},
		})
		cases = append(cases, anthropicEnvelopeFailure{
			name: "ping contains " + name,
			frames: []SSEEvent{ignoredAnthropicStart(), {
				Event: "ping", Data: []byte(`{"type":"ping","content_block":` + block + `}`),
			}},
		})
	}
	for name, event := range map[string]SSEEvent{
		"foreign null payload": {Event: "ping", Data: []byte(`{"type":"ping","content_block":null}`)},
		"foreign delta":        {Event: "ping", Data: []byte(`{"type":"ping","delta":{"type":"text_delta","text":"hidden"}}`)},
		"foreign message":      {Event: "ping", Data: []byte(`{"type":"ping","message":{"content":[]}}`)},
		"foreign error":        {Event: "ping", Data: []byte(`{"type":"ping","error":{"type":"overloaded_error","message":"hidden failure"}}`)},
		"foreign usage":        {Event: "ping", Data: []byte(`{"type":"ping","usage":{"output_tokens":1}}`)},
		"foreign index":        {Event: "ping", Data: []byte(`{"type":"ping","index":0}`)},
		"block stop content":   {Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0,"content_block":` + anthropicListingBlockJSON + `}`)},
		"message stop content": {Event: "message_stop", Data: []byte(`{"type":"message_stop","content_block":` + anthropicListingBlockJSON + `}`)},
	} {
		frames := []SSEEvent{ignoredAnthropicStart()}
		switch event.Event {
		case "content_block_stop":
			frames = append(frames, ignoredAnthropicBlockStart(0, `{"type":"text","text":"answer"}`))
		case "message_stop":
			frames = append(frames, ignoredAnthropicTerminal()[0])
		}
		cases = append(cases, anthropicEnvelopeFailure{name: name, frames: append(frames, event)})
	}
	for name, field := range map[string]string{
		"text":          `"text":"hidden"`,
		"thinking":      `"thinking":"hidden"`,
		"signature":     `"signature":"hidden"`,
		"arguments":     `"partial_json":"{}"`,
		"citation null": `"citation":null`,
		"text null":     `"text":null`,
		"content type":  `"type":"text_delta"`,
	} {
		cases = append(cases, anthropicEnvelopeFailure{
			name: "message delta contains " + name,
			frames: []SSEEvent{ignoredAnthropicStart(), {
				Event: "message_delta",
				Data:  []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn",` + field + `},"usage":{"output_tokens":1}}`),
			}},
		})
	}
	return cases
}

func TestAnthropicStreamEventPayloadsCannotBypassContentLifecycle(t *testing.T) {
	for _, test := range anthropicEnvelopeFailures() {
		t.Run(test.name, func(t *testing.T) {
			frames := append(test.frames, ignoredAnthropicTerminal()...)
			c := ignoredAnthropicConverter()
			assertIgnoredAnthropicFailure(t, c, frames)
			if reportHasFeature(*c.ConversionReport(), FeatureAnthropicToolChanges) {
				t.Fatalf("a refused envelope recorded a listing omission: %+v", c.ConversionReport().Losses)
			}
		})
	}
}

func TestAnthropicStreamDecodedEventsKeepPayloadBoundaries(t *testing.T) {
	text := "hidden"
	for name, event := range map[string]anthropicmessages.StreamEvent{
		"ping with content": {
			Type: anthropicmessages.StreamEventTypePing,
			ContentBlock: &anthropicmessages.ContentBlock{
				Type: anthropicmessages.ContentBlockTypeText, Text: &text,
			},
		},
		"message delta with text": {
			Type: anthropicmessages.StreamEventTypeMessageDelta,
			Delta: &anthropicmessages.StreamDelta{
				Text: &text,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
			if _, err := s.Convert(anthropicmessages.StreamEvent{
				Type: anthropicmessages.StreamEventTypeMessageStart, Message: &anthropicmessages.Response{},
			}); err != nil {
				t.Fatalf("valid message start: %v", err)
			}
			_, err := s.Convert(event)
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("expected typed event-arm rejection, got %v", err)
			}
			if events, after := s.Convert(anthropicmessages.StreamEvent{Type: anthropicmessages.StreamEventTypePing}); after != err || len(events) != 0 {
				t.Fatalf("failed direct event did not latch: events=%v, err=%v", events, after)
			}
		})
	}
}

func TestAnthropicStreamEventPayloadsStillTolerateProviderMetadata(t *testing.T) {
	c := ignoredAnthropicConverter()
	start := ignoredAnthropicStart()
	start.Data = []byte(strings.Replace(string(start.Data), `"type":"message_start"`, `"type":"message_start","provider_extension":{"data":1}`, 1))
	frames := []SSEEvent{
		start,
		{Event: "ping", Data: []byte(`{"type":"ping","provider_extension":null}`)},
		ignoredAnthropicBlockStart(0, `{"type":"text","text":"answer"}`),
		ignoredAnthropicBlockStop(0),
		{Event: "message_delta", Data: []byte(`{"type":"message_delta","provider_extension":true,"delta":{"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"provider_metadata":{"opaque":1}},"usage":{"output_tokens":1,"service_tier":"standard"}}`)},
		{Event: "message_stop", Data: []byte(`{"type":"message_stop","provider_extension":"ok"}`)},
	}
	var output strings.Builder
	terminals := 0
	for _, frame := range frames {
		batch, err := c.Convert(frame)
		if err != nil {
			t.Fatalf("provider metadata refused on %s: %v", frame.Event, err)
		}
		for _, event := range batch.Events {
			output.Write(event.Data)
		}
		if batch.Terminal {
			terminals++
		}
	}
	if terminals != 1 || !strings.Contains(output.String(), `"text":"answer"`) {
		t.Fatalf("provider metadata lost the answer or terminal: count=%d, body=%s", terminals, output.String())
	}
	if strings.Contains(output.String(), "provider_") || strings.Contains(output.String(), "stop_details") {
		t.Fatalf("unknown provider metadata leaked to the client: %s", output.String())
	}
	if batch, err := c.FinalizeEOF(); err != nil || !batch.Terminal {
		t.Fatalf("valid metadata stream did not end cleanly: batch=%+v, err=%v", batch, err)
	}
}

func TestGeminiStreamMessagesRejectsContentOutsideEventArms(t *testing.T) {
	for _, test := range anthropicEnvelopeFailures() {
		t.Run(test.name, func(t *testing.T) {
			var stream strings.Builder
			for _, frame := range append(test.frames, ignoredAnthropicTerminal()...) {
				stream.WriteString("event: " + frame.Event + "\ndata: " + string(frame.Data) + "\n\n")
			}
			rec := serveGeminiStreamBody(t,
				geminiMessagesStreamHandler(t, stream.String(), nil),
				geminiMessagesStreamTarget, geminiMessagesBody,
			)
			frames := geminiStreamFrames(t, rec.Body.String())
			if len(frames) == 0 {
				t.Fatalf("malformed stream has no error frame: HTTP %d, %s", rec.Code, rec.Body.String())
			}
			for _, frame := range frames {
				if geminiFrameFinish(frame) != "" {
					t.Fatalf("malformed envelope reported success: HTTP %d, %s", rec.Code, rec.Body.String())
				}
			}
			if _, ok := frames[len(frames)-1]["error"]; !ok {
				t.Fatalf("malformed stream has no error terminal: %s", rec.Body.String())
			}
		})
	}
}
