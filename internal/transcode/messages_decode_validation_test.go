package transcode

// A decoder is a client-dialect boundary. top_k is now carried rather than
// destroyed, and the Anthropic display enum has no empty member, so a value
// outside the enum is malformed client input — rejected at the boundary that can
// name it, exactly as the Responses decoder rejects a negative top_logprobs or an
// unknown truncation. The enum is summarized|omitted|updates: the last member is
// beta-gated upstream by thinking-display-updates-2026-08-18, which claude-cli
// 2.x sends, so a client selecting it is legal traffic rather than a typo.

import (
	"strings"
	"testing"
)

// messagesThinkingBody builds a minimal Messages request carrying the thinking
// object.
func messagesThinkingBody(thinking string) []byte {
	return []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":` + thinking + `}`)
}

// TestMessagesDecodeRejectsNegativeTopK proves a negative sampling cutoff is
// client-dialect invalid rather than forwarded.
func TestMessagesDecodeRejectsNegativeTopK(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"top_k":-1}`)
	if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
		t.Fatal("a negative top_k was accepted")
	} else if !strings.Contains(err.Error(), "top_k") {
		t.Fatalf("err = %q, want it to name top_k", err)
	}
	// Zero is the documented "no cutoff" value and passes through.
	zero := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"top_k":0}`)
	result, err := DecodeMessagesRequest(zero, StrictLossPolicy())
	if err != nil {
		t.Fatalf("top_k 0 was refused: %v", err)
	}
	if result.Request.TopK == nil || *result.Request.TopK != 0 {
		t.Fatalf("TopK = %v, want the explicit zero carried", result.Request.TopK)
	}
}

// TestMessagesDecodeRejectsAnUnknownThinkingDisplay proves the display enum is
// validated at the boundary: the wire has no empty member and no value outside
// the documented enum is accepted, so a bogus one is malformed rather than
// silently elided.
func TestMessagesDecodeRejectsAnUnknownThinkingDisplay(t *testing.T) {
	for _, display := range []string{`"bogus"`, `""`, `"full"`} {
		body := messagesThinkingBody(`{"type":"adaptive","display":` + display + `}`)
		if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
			t.Fatalf("display %s was accepted", display)
		} else if !strings.Contains(err.Error(), "display") {
			t.Fatalf("display %s: err = %q, want it to name display", display, err)
		}
	}
	// Every documented value decodes and reaches the Messages wire verbatim, on
	// both arms that carry a display (the contract documents it on enabled too).
	for _, display := range []string{"summarized", "omitted", "updates"} {
		for _, thinking := range []string{
			`{"type":"adaptive","display":"` + display + `"}`,
			`{"type":"enabled","budget_tokens":4096,"display":"` + display + `"}`,
		} {
			body := messagesThinkingBody(thinking)
			result, err := DecodeMessagesRequest(body, StrictLossPolicy())
			if err != nil {
				t.Fatalf("%s refused: %v", thinking, err)
			}
			if result.Request.Thinking == nil || result.Request.Thinking.Display != display {
				t.Fatalf("Thinking = %+v, want display %q carried", result.Request.Thinking, display)
			}
			rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(string(rendered), `"display":"`+display+`"`) {
				t.Fatalf("display %q is missing from the Messages wire: %s", display, rendered)
			}
		}
	}
}

// TestMessagesDecodeDoesNotInventAnAbsentDisplay proves absence stays absent: no
// display is synthesized on decode or on the wire, and an explicit JSON null —
// which the contract types as legal (`"summarized" or "omitted" or null`) —
// decodes as ABSENT rather than as the illegal empty string, on every arm that
// carries a display.
func TestMessagesDecodeDoesNotInventAnAbsentDisplay(t *testing.T) {
	for _, thinking := range []string{
		`{"type":"adaptive"}`,
		`{"type":"adaptive","display":null}`,
		`{"type":"enabled","budget_tokens":4096,"display":null}`,
		`{"type":"disabled","display":null}`,
		`{"type":"between_tools","display":null}`,
	} {
		body := messagesThinkingBody(thinking)
		result, err := DecodeMessagesRequest(body, StrictLossPolicy())
		if err != nil {
			t.Fatalf("%s: decode: %v", thinking, err)
		}
		if result.Request.Thinking == nil || result.Request.Thinking.Display != "" {
			t.Fatalf("%s: Thinking = %+v, want no display invented", thinking, result.Request.Thinking)
		}
		rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
		if err != nil {
			t.Fatalf("%s: render: %v", thinking, err)
		}
		if strings.Contains(string(rendered), `"display"`) {
			t.Fatalf("%s: an absent display was invented on the wire: %s", thinking, rendered)
		}
	}
}
