package transcode

// A decoder is a client-dialect boundary. top_k is now carried rather than
// destroyed, and the Anthropic display enum has no empty member, so a value
// outside the enum is malformed client input — rejected at the boundary that can
// name it, exactly as the Responses decoder rejects a negative top_logprobs or an
// unknown truncation.

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
// validated at the boundary: the wire has no empty member and no other value is
// documented, so a bogus one is malformed rather than silently elided.
func TestMessagesDecodeRejectsAnUnknownThinkingDisplay(t *testing.T) {
	for _, display := range []string{`"bogus"`, `""`, `"full"`} {
		body := messagesThinkingBody(`{"type":"adaptive","display":` + display + `}`)
		if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
			t.Fatalf("display %s was accepted", display)
		} else if !strings.Contains(err.Error(), "display") {
			t.Fatalf("display %s: err = %q, want it to name display", display, err)
		}
	}
	// The two documented values decode and reach the Messages wire verbatim.
	for _, display := range []string{"summarized", "omitted"} {
		body := messagesThinkingBody(`{"type":"adaptive","display":"` + display + `"}`)
		result, err := DecodeMessagesRequest(body, StrictLossPolicy())
		if err != nil {
			t.Fatalf("display %q refused: %v", display, err)
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

// TestMessagesDecodeDoesNotInventAnAbsentDisplay proves absence stays absent: no
// display is synthesized on decode or on the wire.
func TestMessagesDecodeDoesNotInventAnAbsentDisplay(t *testing.T) {
	body := messagesThinkingBody(`{"type":"adaptive"}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Request.Thinking == nil || result.Request.Thinking.Display != "" {
		t.Fatalf("Thinking = %+v, want no display invented", result.Request.Thinking)
	}
	rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(rendered), `"display"`) {
		t.Fatalf("an absent display was invented on the wire: %s", rendered)
	}
}
