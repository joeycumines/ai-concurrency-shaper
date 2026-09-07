package transcode

// Autopsy 2026-09-06 M3: a Messages request with absent, null, or empty
// messages decoded and the Messages→Responses direction rendered
// "input":[] upstream, while Messages→Chat rejected the same source shape.
// The directions must agree: absent/empty conversation turns are a
// client-dialect error on every target.

import (
	"strings"
	"testing"
)

func TestMessagesEmptyConversationRejectedOnResponsesTarget(t *testing.T) {
	for name, body := range map[string]string{
		"absent messages": `{"model":"m","max_tokens":100}`,
		"null messages":   `{"model":"m","max_tokens":100,"messages":null}`,
		"empty messages":  `{"model":"m","max_tokens":100,"messages":[]}`,
	} {
		result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
		if err != nil {
			t.Fatalf("%s: decode must succeed (the empty-conversation check is render-side): %v", name, err)
		}
		context := testExchangeContext()
		context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
			FeatureToolSchemaStrictness: {},
		}}
		_, _, err = RenderResponsesRequest(result.Request, context)
		if err == nil {
			t.Fatalf("%s: Messages→Responses must reject an empty conversation (autopsy M3)", name)
		}
		if !strings.Contains(err.Error(), "no Messages-representable conversation turns") {
			t.Fatalf("%s: err = %v, want the empty-conversation rejection", name, err)
		}
	}
}

func TestMessagesSystemOnlyConversationRejectedOnResponsesTarget(t *testing.T) {
	// A system-only request has instructions but no conversation: the same
	// empty-input rejection applies (the Chat direction rejects it too).
	body := `{"model":"m","max_tokens":100,"system":"be brief"}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolSchemaStrictness: {},
	}}
	if _, _, err := RenderResponsesRequest(result.Request, context); err == nil {
		t.Fatal("system-only request must be rejected on the Responses target")
	}
}

func TestMessagesNonEmptyConversationStillRendersOnResponsesTarget(t *testing.T) {
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolSchemaStrictness: {},
	}}
	rendered, _, err := RenderResponsesRequest(result.Request, context)
	if err != nil {
		t.Fatalf("valid request must render: %v", err)
	}
	if !strings.Contains(string(rendered), `"input"`) {
		t.Fatalf("rendered request carries no input: %s", rendered)
	}
}
