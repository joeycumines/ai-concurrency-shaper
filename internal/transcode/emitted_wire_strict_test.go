package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// strictDecodeUnknown uses encoding/json with DisallowUnknownFields to prove
// the given wire carries ONLY fields in the target schema. This is the
// emitted-wire conformance contract: the transcoder's output must be
// pin-conformant, not merely decodable (review-08 task 12b).
func strictDecodeUnknown(t *testing.T, wire []byte, target any, label string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(wire)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		t.Fatalf("strict-decode %s failed: %v\nwire: %s", label, err, wire)
	}
}

// TestEmittedWireResponsesEnvelopeStrictConformance proves the non-streaming
// Chat→Responses render emits a ResponseEnvelope that strict-decodes against
// the package schema (no unknown fields, review-08 task 12b).
func TestEmittedWireResponsesEnvelopeStrictConformance(t *testing.T) {
	resp := CanonicalResponse{
		ID:         "resp_1",
		Model:      "m",
		CreatedAt:  1,
		Status:     CanonicalResponseCompleted,
		StopReason: CanonicalStopEndTurn,
		Turns: []CanonicalTurn{{
			Role:  CanonicalAssistant,
			Parts: []CanonicalPart{CanonicalText{Text: "hello"}},
		}},
		Usage: CanonicalUsage{
			InputTokens: 5, InputKnown: true,
			OutputTokens: 2, OutputKnown: true,
			TotalTokens: 7, TotalKnown: true,
		},
	}
	ctx := &ExchangeContext{
		RequestedClientModel: "m",
		UpstreamModel:        "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageTiming: {},
		}},
	}
	wire, _, err := RenderResponsesResponse(resp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var envelope ResponseEnvelope
	strictDecodeUnknown(t, wire, &envelope, "ResponseEnvelope")
}

// TestEmittedWireChatRequestStrictConformance proves the Messages→Chat request
// render emits a ChatRequest that strict-decodes.
func TestEmittedWireChatRequestStrictConformance(t *testing.T) {
	req := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hello"}},
		}},
	}
	ctx := &ExchangeContext{
		RequestedClientModel: "m",
		UpstreamModel:        "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy:           StrictLossPolicy(),
	}
	wire, _, err := RenderChatRequest(req, ctx, ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	var chatReq ChatRequest
	strictDecodeUnknown(t, wire, &chatReq, "ChatRequest")
}

// TestEmittedWireAnthropicStreamEventsStrictConformance proves the
// Responses→Anthropic stream direction emits events that strict-decode
// against the AnthropicStreamEvent schema.
func TestEmittedWireAnthropicStreamEventsStrictConformance(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), "resp_1", "m", 1,
	)
	input := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
		`{"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello","logprobs":[]}`,
		`{"type":"response.output_text.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"text":"hello","logprobs":[]}`,
		`{"type":"response.content_part.done","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello","annotations":[]}}`,
		`{"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}}`,
		`{"type":"response.completed","sequence_number":7,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}}`,
	}
	for _, raw := range input {
		event, err := decodeResponsesSSEEvent([]byte(raw))
		if err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		anthropicEvents, err := state.Convert(event)
		if err != nil {
			t.Fatalf("convert %q: %v", raw, err)
		}
		for _, ae := range anthropicEvents {
			wire, err := json.Marshal(ae)
			if err != nil {
				t.Fatalf("marshal anthropic event: %v", err)
			}
			var decoded AnthropicStreamEvent
			strictDecodeUnknown(t, wire, &decoded, "AnthropicStreamEvent")
		}
	}
}

// TestEmittedWireResponsesStreamEventsStrictConformance proves the
// Chat→Responses stream direction emits Responses SSE events whose JSON
// strict-decodes against the package event types.
func TestEmittedWireResponsesStreamEventsStrictConformance(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(),
		ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
		"resp_1", "m", 1, nil,
	)
	chunks := []string{
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
	}
	convertAndStrictDecode := func(events []ResponsesSSEEvent) {
		t.Helper()
		for _, ev := range events {
			wire, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal responses event: %v", err)
			}
			probe := struct {
				Type string `json:"type"`
			}{}
			if err := json.Unmarshal(wire, &probe); err != nil {
				t.Fatalf("unmarshal type: %v", err)
			}
			if probe.Type == "" {
				continue
			}
			target, ok := strictDecodeTargetForResponseType(probe.Type)
			if !ok {
				t.Fatalf("no strict-decode target for emitted event type %q", probe.Type)
			}
			strictDecodeUnknown(t, wire, target, "ResponsesSSEEvent("+probe.Type+")")
		}
	}
	for _, raw := range chunks {
		var chunk ChatStreamResponse
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		events, err := state.Convert(chunk)
		if err != nil {
			t.Fatalf("convert chunk: %v", err)
		}
		convertAndStrictDecode(events)
	}
	// Release the held terminal to strict-decode the done/completed events
	// (the [DONE] sentinel is the release signal — review-08 blocker 2).
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("releaseTerminal returned false")
	}
	convertAndStrictDecode(held)
}

// strictDecodeTargetForResponseType returns a fresh zero-value pointer for the
// concrete Responses SSE event type matching the JSON type tag, so the event
// wire can be strict-decoded against its own schema (proving no unknown fields
// leak). Events not in the package's emitted surface are skipped.
func strictDecodeTargetForResponseType(typeTag string) (any, bool) {
	switch typeTag {
	case "response.created", "response.in_progress",
		"response.completed", "response.incomplete", "response.failed":
		return &struct {
			Type           string           `json:"type"`
			SequenceNumber int              `json:"sequence_number"`
			Response       ResponseEnvelope `json:"response"`
		}{}, true
	case "response.output_item.added", "response.output_item.done":
		return &struct {
			Type           string           `json:"type"`
			SequenceNumber int              `json:"sequence_number"`
			OutputIndex    int              `json:"output_index"`
			Item           json.RawMessage  `json:"item"`
		}{}, true
	case "response.content_part.added", "response.content_part.done":
		return &struct {
			Type           string           `json:"type"`
			SequenceNumber int              `json:"sequence_number"`
			ItemID         string           `json:"item_id"`
			OutputIndex    int              `json:"output_index"`
			ContentIndex   int              `json:"content_index"`
			Part           json.RawMessage  `json:"part"`
		}{}, true
	case "response.output_text.delta":
		return &struct {
			Type           string           `json:"type"`
			SequenceNumber int              `json:"sequence_number"`
			ItemID         string           `json:"item_id"`
			OutputIndex    int              `json:"output_index"`
			ContentIndex   int              `json:"content_index"`
			Delta          string           `json:"delta"`
			Logprobs       json.RawMessage  `json:"logprobs"`
		}{}, true
	case "response.output_text.done":
		return &struct {
			Type           string           `json:"type"`
			SequenceNumber int              `json:"sequence_number"`
			ItemID         string           `json:"item_id"`
			OutputIndex    int              `json:"output_index"`
			ContentIndex   int              `json:"content_index"`
			Text           string           `json:"text"`
			Logprobs       json.RawMessage  `json:"logprobs"`
		}{}, true
	}
	return nil, false
}
