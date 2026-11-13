package transcode

// ValidateCanonicalRequest is the single gate every renderer passes, so every
// rule it enforces must be unreachable from the DECODERS: a rule a decoder can
// trip would reject a request the client legitimately sent. The function-result
// part restriction and the positive-budget rule are the two most recent, so this
// test drives every decoder that produces a CanonicalFunctionResult and asserts
// its output validates.

import (
	"strings"
	"testing"
)

// TestDecodersProduceValidatingFunctionResults proves no decoder can build a
// function result the central validator refuses: each producer is driven through
// its public entry point with the widest content it accepts.
func TestDecodersProduceValidatingFunctionResults(t *testing.T) {
	cases := []struct {
		name   string
		body   []byte
		decode func(*testing.T, []byte) CanonicalRequest
	}{
		{
			name: "messages tool_result with text, image and document",
			body: []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"text","text":"done"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},` +
				`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"BBBB"}}` +
				`]}]}]}`),
			decode: func(t *testing.T, body []byte) CanonicalRequest {
				result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
					FeatureAnthropicServerTools: {},
				}})
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				return result.Request
			},
		},
		{
			name: "responses function_call_output with text and image",
			body: []byte(`{"model":"m","input":[` +
				`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"c1","output":[` +
				`{"type":"input_text","text":"done"},` +
				`{"type":"input_image","image_url":"https://example.test/i.png"}` +
				`]}]}`),
			decode: func(t *testing.T, body []byte) CanonicalRequest {
				result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				return result.Request
			},
		},
		{
			name: "gemini functionResponse",
			body: []byte(`{"contents":[` +
				`{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},` +
				`{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"done"}}}]}` +
				`]}`),
			decode: func(t *testing.T, body []byte) CanonicalRequest {
				result, _, err := DecodeGeminiRequest(body, StrictLossPolicy(), UpstreamChatCompletions)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				return result.Request
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.decode(t, tc.body)
			// At least one function result must exist, or the test proves
			// nothing about the new restriction.
			found := 0
			for _, turn := range request.Turns {
				for _, part := range turn.Parts {
					if _, ok := part.(CanonicalFunctionResult); ok {
						found++
					}
				}
			}
			if found == 0 {
				t.Fatalf("the probe produced no function result: %+v", request.Turns)
			}
			if err := ValidateCanonicalRequest(request); err != nil {
				t.Fatalf("a decoder's function result failed the central validator: %v", err)
			}
		})
	}
}

// TestMessagesDecoderPositiveBudgetValidates proves the budget-sign rule cannot
// be tripped by the decoder that owns the union: it rejects a non-positive value
// itself, so a decoder-produced request always passes.
func TestMessagesDecoderPositiveBudgetValidates(t *testing.T) {
	for _, budget := range []string{"1", "1024", "32000"} {
		body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
			`"thinking":{"type":"enabled","budget_tokens":` + budget + `}}`)
		result, err := DecodeMessagesRequest(body, StrictLossPolicy())
		if err != nil {
			t.Fatalf("budget %s refused by its own decoder: %v", budget, err)
		}
		if err := ValidateCanonicalRequest(result.Request); err != nil {
			t.Fatalf("budget %s produced an invalid request: %v", budget, err)
		}
	}
	// A non-positive budget never reaches the IR at all.
	for _, budget := range []string{"0", "-1"} {
		body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
			`"thinking":{"type":"enabled","budget_tokens":` + budget + `}}`)
		if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
			t.Fatalf("budget %s was accepted by the decoder", budget)
		}
	}
}

// TestNestedToolResultIsReachableAndRenderable proves the central restriction
// does not out-reach the wire. An Anthropic tool_result's content is the same
// Content union, so the Messages decoder legitimately nests a tool_result (and a
// tool_use) inside one; both must validate and render, because refusing them
// would reject a request the client sent in its own dialect.
func TestNestedToolResultIsReachableAndRenderable(t *testing.T) {
	cases := map[string]string{
		"nested tool_result": `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` +
			`{"type":"tool_result","tool_use_id":"t2","content":"x"}]}]}]}`,
		"nested tool_use": `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` +
			`{"type":"tool_use","id":"t9","name":"g","input":{}}]}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := ValidateCanonicalRequest(result.Request); err != nil {
				t.Fatalf("a decoder-produced nested result failed the central validator: %v", err)
			}
			for target, render := range allRequestRenderers() {
				if _, _, err := render(result.Request, testExchangeContext()); err != nil {
					// The target may legitimately lack a form for the nested
					// shape; what it must not do is fail the CENTRAL gate.
					if strings.Contains(err.Error(), "is not content a tool result can carry") {
						t.Fatalf("%s: the central restriction rejected a decoder shape: %v", target, err)
					}
				}
			}
		})
	}
}

// TestFunctionResultStillRejectsAPlacedNowhereThinkingPart keeps the restriction
// meaningful: a thinking part genuinely has no place inside a tool result (the IR
// records no position for it), so that shape stays refused.
func TestFunctionResultStillRejectsAPlacedNowhereThinkingPart(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalFunctionResult{CallID: "c1", Parts: []CanonicalPart{
					CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
				}},
			},
		}},
	}
	if err := ValidateCanonicalRequest(request); err == nil {
		t.Fatal("a thinking part inside a tool result passed validation")
	}
}
