package transcode

// All four request renderers tolerate a nil ExchangeContext:
// ExchangeContext.lossPolicy reports the strictest policy for it, toolNames
// reports a nil map, and the Gemini signature store is explicitly nil-guarded.
// The renderers therefore treat "no context" as the strictest ordinary render,
// never as a programming error.
//
// The per-exchange namespace resolver is built before any turn is inspected, so
// dereferencing the context there would turn a render that used to work into an
// unconditional panic. These tests pin the tolerance, and pin that an
// unresolvable qualifier is still adjudicated (strictly refused) rather than
// panicking on the nil context.

import (
	"strings"
	"testing"
)

// nilContextTools is a request with no namespaced call, so the only decision
// the renderer has to make is the ordinary one.
func nilContextRequest() CanonicalRequest {
	return CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		},
	}
}

func TestRequestRenderersTolerateNilExchangeContext(t *testing.T) {
	t.Run("gemini", func(t *testing.T) {
		if _, _, err := RenderGeminiRequest(nilContextRequest(), nil); err != nil {
			t.Fatalf("nil context must render, got %v", err)
		}
	})

	t.Run("messages", func(t *testing.T) {
		if _, _, err := RenderMessagesRequest(nilContextRequest(), nil); err != nil {
			t.Fatalf("nil context must render, got %v", err)
		}
	})

	t.Run("chat", func(t *testing.T) {
		// The Chat renderer reads the Responses request echo and the resolved
		// reasoning tier, so a nil context must be guarded rather than
		// dereferenced: every other renderer in this package treats nil as the
		// ordinary strict render.
		if _, _, err := RenderChatRequest(nilContextRequest(), nil, ChatCapabilities{}); err != nil {
			t.Fatalf("nil context must render, got %v", err)
		}
	})

	t.Run("responses", func(t *testing.T) {
		if _, _, err := RenderResponsesRequest(nilContextRequest(), nil); err != nil {
			t.Fatalf("nil context must render, got %v", err)
		}
	})

	// An unresolvable qualifier is adjudicated against the nil context's
	// strict default policy rather than panicking on it. The bare name is
	// owned by nothing here, so the outcome is the named refusal.
	namespaced := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalFunctionCall{CallID: "call-1", Name: "unowned", Namespace: "ns", Arguments: []byte(`{}`)},
			}},
		},
	}
	for name, render := range map[string]func() error{
		"gemini": func() error {
			_, _, err := RenderGeminiRequest(namespaced, nil)
			return err
		},
		"messages": func() error {
			_, _, err := RenderMessagesRequest(namespaced, nil)
			return err
		},
	} {
		t.Run(name+" adjudicates without panicking", func(t *testing.T) {
			err := render()
			if err == nil || !strings.Contains(err.Error(), string(FeatureNamespaceReplayUndeclared)) {
				t.Fatalf("err = %v, want the strict refusal for an unresolvable qualifier", err)
			}
		})
	}
}
