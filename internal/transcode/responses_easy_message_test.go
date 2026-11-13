package transcode

// A Responses input turn is a sequence of items: the content message, then the
// function_call / function_call_output items. Flushing the buffered content
// around a call or a result therefore has three sites, and all three have to
// agree about a buffer that scrubbed to nothing: emit no contentless union, and
// refuse only a turn that produced no item at all.

import (
	"encoding/json"
	"strings"
	"testing"
)

// responsesInputItems decodes the rendered Responses body's input item list.
func responsesInputItems(t *testing.T, rendered []byte) []map[string]any {
	t.Helper()
	var raw struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rendered, &raw); err != nil {
		t.Fatalf("render decode: %v\n%s", err, rendered)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw.Input, &items); err != nil {
		t.Fatalf("input decode: %v (input = %s)", err, raw.Input)
	}
	return items
}

// TestResponsesThinkingBeforeFunctionCallRendersTheCall proves the flush before
// a function_call does not emit an easy message whose content union selects no
// arm: the marshaller rejects that union with "no selected variant", which
// describes a Go bug rather than the request the operator sent.
func TestResponsesThinkingBeforeFunctionCallRendersTheCall(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "go"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
				CanonicalFunctionCall{CallID: "call_1", Name: "f", Arguments: []byte(`{}`)},
			}},
		},
	}
	rendered, _, err := RenderResponsesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("a scrubbed thinking part before a call failed the render: %v", err)
	}
	items := responsesInputItems(t, rendered)
	var calls int
	for _, item := range items {
		if item["type"] == "function_call" {
			calls++
		}
		if content, ok := item["content"].([]any); ok && len(content) == 0 {
			t.Fatalf("an empty content array was emitted: %s", rendered)
		}
		if _, isMessage := item["role"]; isMessage {
			if _, hasContent := item["content"]; !hasContent {
				t.Fatalf("a contentless easy message was emitted: %s", rendered)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("function_call items = %d, want 1: %s", calls, rendered)
	}
}

// TestResponsesThinkingAfterFunctionCallRendersTheCall proves the trailing
// flush does not refuse a turn that already emitted a valid item.
func TestResponsesThinkingAfterFunctionCallRendersTheCall(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "go"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalFunctionCall{CallID: "call_1", Name: "f", Arguments: []byte(`{}`)},
				CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
			}},
		},
	}
	rendered, _, err := RenderResponsesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("a scrubbed thinking part after a call failed the render: %v", err)
	}
	if !strings.Contains(string(rendered), `"function_call"`) {
		t.Fatalf("the function call is missing: %s", rendered)
	}
}

// TestResponsesThinkingOnlyTurnIsRefusedAtTheCanonicalPath proves a turn that
// produced NOTHING is still refused, locally and by canonical path, rather than
// reaching the wire marshaller.
func TestResponsesThinkingOnlyTurnIsRefusedAtTheCanonicalPath(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "go"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
			}},
		},
	}
	_, _, err := RenderResponsesRequest(request, testExchangeContext())
	if err == nil {
		t.Fatal("a turn with no renderable content was accepted")
	}
	if !strings.Contains(err.Error(), "turns[1]") {
		t.Fatalf("err = %q, want it to name turns[1]", err)
	}
	if strings.Contains(err.Error(), "no selected variant") {
		t.Fatalf("err = %q, want a local refusal rather than a marshal error", err)
	}
}

// TestResponsesThinkingBeforeFunctionResultRendersTheResult proves the same for
// the flush before a function_call_output.
func TestResponsesThinkingBeforeFunctionResultRendersTheResult(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "go"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalFunctionCall{CallID: "call_1", Name: "f", Arguments: []byte(`{}`)},
			}},
			{Role: CanonicalUser, Parts: []CanonicalPart{
				CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalText{Text: "done"},
				}},
			}},
		},
	}
	rendered, _, err := RenderResponsesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("a scrubbed thinking part before a result failed the render: %v", err)
	}
	if !strings.Contains(string(rendered), `"function_call_output"`) {
		t.Fatalf("the function output is missing: %s", rendered)
	}
}
