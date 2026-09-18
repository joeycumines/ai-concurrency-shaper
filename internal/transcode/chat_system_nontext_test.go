package transcode

// Tests for the chat system/developer non-text content decision: a system
// message cannot carry an image or document, so the part follows the same
// loss/reject decision the Responses target applies under
// system_non_text_content — dropped observably when approved, rejected with a
// stable feature name otherwise. Never silently dropped, never an internal Go
// type name in a client-dialect error.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// chatSystemNonTextRequest builds a request whose system turn carries a
// non-text part followed by a user turn (the system turn must not be the
// only turn, or the request is trivially empty).
func chatSystemNonTextRequest(part CanonicalPart) CanonicalRequest {
	return CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalSystem, Parts: []CanonicalPart{part}},
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
		},
	}
}

// TestChatSystemImageLossApproved proves an approved
// system_non_text_content drop is observable (the loss is recorded) and the
// system message stays a valid content union.
func TestChatSystemImageLossApproved(t *testing.T) {
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	rendered, report, err := RenderChatRequest(
		chatSystemNonTextRequest(CanonicalImage{
			MediaType: "image/png",
			URL:       "https://example.test/x.png",
		}),
		context,
		ChatCapabilities{},
	)
	if err != nil {
		t.Fatalf("approved system image drop rejected: %v", err)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report lacks system_non_text_content: %+v", report)
	}
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rendered, &request); err != nil {
		t.Fatalf("rendered request is not JSON: %v", err)
	}
	if len(request.Messages) == 0 || request.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v, want a leading system message", request.Messages)
	}
	// The system content must remain a valid text union (never an empty or
	// image-bearing block array).
	if !strings.Contains(string(request.Messages[0].Content), `"type":"text"`) {
		t.Fatalf("system content = %s, want a text block", request.Messages[0].Content)
	}
	if strings.Contains(string(rendered), "image_url") {
		t.Fatalf("dropped system image leaked into the request: %s", rendered)
	}
}

// TestChatSystemDocumentLossApproved proves the document shape takes the same
// approved-loss path.
func TestChatSystemDocumentLossApproved(t *testing.T) {
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	rendered, report, err := RenderChatRequest(
		chatSystemNonTextRequest(CanonicalDocument{
			MediaType: "application/pdf",
			URL:       "https://example.test/d.pdf",
		}),
		context,
		ChatCapabilities{},
	)
	if err != nil {
		t.Fatalf("approved system document drop rejected: %v", err)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report lacks system_non_text_content: %+v", report)
	}
	// The dropped document must not survive into the rendered request, and
	// the system message must remain a valid text union.
	if strings.Contains(string(rendered), "d.pdf") {
		t.Fatalf("dropped document leaked into the request: %s", rendered)
	}
	if !strings.Contains(string(rendered), `"type":"text"`) {
		t.Fatalf("system content is not a text block: %s", rendered)
	}
}

// TestChatSystemNonTextStrictRejected proves the strict (default programmatic)
// policy still rejects, with a stable feature name in the error — never a
// leaked Go type.
func TestChatSystemNonTextStrictRejected(t *testing.T) {
	_, _, err := RenderChatRequest(
		chatSystemNonTextRequest(CanonicalImage{
			MediaType: "image/png",
			URL:       "https://example.test/x.png",
		}),
		testExchangeContext(),
		ChatCapabilities{},
	)
	if err == nil {
		t.Fatal("strict policy accepted non-text system content")
	}
	if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
		t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
	}
	if !strings.Contains(err.Error(), "system_non_text_content") {
		t.Fatalf("error = %q, want the stable system_non_text_content feature name", err.Error())
	}
	// The client-visible text must not leak the internal Go type.
	if strings.Contains(err.Error(), "CanonicalImage") {
		t.Fatalf("error leaks the internal Go type: %q", err.Error())
	}
}

// TestChatSystemTextUnchanged proves an ordinary text system turn records no
// system_non_text_content loss and renders a text block (the change is inert
// for the normal path).
func TestChatSystemTextUnchanged(t *testing.T) {
	rendered, report, err := RenderChatRequest(
		CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{
				{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "be terse"}}},
				{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			},
		},
		testExchangeContext(),
		ChatCapabilities{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("text system turn recorded system_non_text_content: %+v", report)
	}
	if !strings.Contains(string(rendered), "be terse") {
		t.Fatalf("system text missing: %s", rendered)
	}
}

// TestChatSystemMixedTextAndImageDrop proves a mixed system turn keeps its
// text while the image drops observably under the approved loss.
func TestChatSystemMixedTextAndImageDrop(t *testing.T) {
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	rendered, report, err := RenderChatRequest(
		CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{
				{Role: CanonicalSystem, Parts: []CanonicalPart{
					CanonicalText{Text: "be terse"},
					CanonicalImage{MediaType: "image/png", URL: "https://example.test/x.png"},
				}},
				{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			},
		},
		context,
		ChatCapabilities{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report lacks system_non_text_content: %+v", report)
	}
	if !strings.Contains(string(rendered), "be terse") {
		t.Fatalf("system text lost with the image: %s", rendered)
	}
	if strings.Contains(string(rendered), "image_url") {
		t.Fatalf("dropped image leaked into the request: %s", rendered)
	}
}

// TestChatDeveloperSystemNonTextDecision proves the developer turn routed to
// the system role (developer-role capability off) takes the same decision.
func TestChatDeveloperSystemNonTextDecision(t *testing.T) {
	context := testExchangeContext()
	// With developer-role off the developer turn routes to the system role,
	// which needs the developer_role loss approved as well as the non-text
	// drop — two independent decisions.
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
		FeatureDeveloperRole:        {},
	}}
	_, report, err := RenderChatRequest(
		CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{
				{Role: CanonicalDeveloper, Parts: []CanonicalPart{
					CanonicalImage{MediaType: "image/png", URL: "https://example.test/x.png"},
				}},
				{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			},
		},
		context,
		ChatCapabilities{},
	)
	if err != nil {
		t.Fatalf("approved developer image drop rejected: %v", err)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report lacks system_non_text_content: %+v", report)
	}
	// The role-distinction loss and the content loss are independent: both
	// must be recorded.
	if !reportHasFeature(report, FeatureDeveloperRole) {
		t.Fatalf("report lacks developer_role: %+v", report)
	}
}
