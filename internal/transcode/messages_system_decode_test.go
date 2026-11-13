package transcode

// The Messages decoder synthesizes an empty text part when every block of a
// messages[] turn was scrubbed or moved to artifacts, precisely so the turn
// survives validation with its boundary intact. The top-level system field
// needs the same treatment: a system prompt made only of marker-signature
// thinking (or of authenticated thinking, or of server-tool blocks) decodes to
// zero parts, and ValidateCanonicalRequest would refuse the whole request with
// "turn 0 has no content parts" before any renderer — including the artifact
// gate — could run.

import (
	"strings"
	"testing"
)

// TestMessagesSystemOfScrubbedThinkingRendersNoSystemPrompt proves the shape a
// client can actually send: a system prompt whose blocks were all
// proxy-synthesized thinking is scrubbed, and the render proceeds with no
// system prompt rather than failing validation.
func TestMessagesSystemOfScrubbedThinkingRendersNoSystemPrompt(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"system":[` +
		`{"type":"thinking","thinking":"proxy thought","signature":"` + SyntheticThinkingSignature + `"}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Request.Turns) == 0 {
		t.Fatal("the system field produced no turn")
	}
	for i, turn := range result.Request.Turns {
		if len(turn.Parts) == 0 {
			t.Fatalf("turn %d has no parts", i)
		}
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("a scrubbed system prompt failed validation: %v", err)
	}
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			rendered, _, err := render(result.Request, testExchangeContext())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			for _, empty := range []string{`"system":""`, `"instructions":""`, `"systemInstruction":{}`} {
				if strings.Contains(string(rendered), empty) {
					t.Fatalf("an empty system field was invented (%s): %s", empty, rendered)
				}
			}
			if strings.Contains(string(rendered), "proxy thought") {
				t.Fatalf("a scrubbed system thought reached the wire: %s", rendered)
			}
		})
	}
}

// TestMessagesSystemOfAuthenticatedThinkingTakesTheArtifactDecision proves the
// authenticated-thinking gate is what refuses or drops the block, not a turn
// shape error: the operator's policy must be the thing that decides.
func TestMessagesSystemOfAuthenticatedThinkingTakesTheArtifactDecision(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"system":[` +
		`{"type":"thinking","thinking":"real","signature":"provider-sig"}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Strict: the artifact gate refuses, and it names the feature.
	_, _, err = RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err == nil {
		t.Fatal("a system thinking block passed the strict artifact gate")
	}
	if !strings.Contains(err.Error(), string(FeatureAuthenticatedThinking)) {
		t.Fatalf("err = %q, want the authenticated_thinking decision", err)
	}
	if strings.Contains(err.Error(), "no content parts") {
		t.Fatalf("err = %q, want the artifact decision rather than a turn-shape error", err)
	}

	// Approved: the block drops observably and the render succeeds.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureAuthenticatedThinking: {}}}
	rendered, report, err := RenderChatRequest(result.Request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("approved render: %v", err)
	}
	if !reportHasFeature(report, FeatureAuthenticatedThinking) {
		t.Fatalf("report = %+v, want the authenticated_thinking loss", report.Losses)
	}
	if strings.Contains(string(rendered), "real") {
		t.Fatalf("an authenticated system thought reached the wire: %s", rendered)
	}
}

// TestMessagesSystemOfServerToolsTakesTheServerToolDecision proves the same for
// a system prompt made only of server-executed blocks: the server-tool decision
// is the thing that decides, not a turn-shape error. The loss is recorded at
// decode (a server tool has no target and the decoder is the stage that reads
// it), so the approved case is the one that reaches validation.
func TestMessagesSystemOfServerToolsTakesTheServerToolDecision(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"system":[` +
		`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"q":"x"}}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)

	// Strict: the decoder refuses and names the server-tool key.
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("a system server-tool block passed the strict server-tool gate")
	}
	if !strings.Contains(err.Error(), string(FeatureAnthropicServerTools)) {
		t.Fatalf("err = %q, want the anthropic_server_tools decision", err)
	}

	// Approved: the block drops observably, the turn keeps an empty text part
	// so the boundary survives, and no renderer sees a contentless turn.
	result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicServerTools: {},
	}})
	if err != nil {
		t.Fatalf("approved decode: %v", err)
	}
	if !reportHasFeature(result.Report, FeatureAnthropicServerTools) {
		t.Fatalf("report = %+v, want the anthropic_server_tools loss", result.Report.Losses)
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("the surviving system turn failed validation: %v", err)
	}
}

// TestMessagesSystemOfOneSurvivingBlockKeepsOnlyThatBlock proves the synthesis
// only fires when nothing survives: a system prompt with one real text block
// beside a scrubbed thinking block keeps exactly the real text.
func TestMessagesSystemOfOneSurvivingBlockKeepsOnlyThatBlock(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"system":[` +
		`{"type":"thinking","thinking":"proxy thought","signature":"` + SyntheticThinkingSignature + `"},` +
		`{"type":"text","text":"be brief"}` +
		`],"messages":[{"role":"user","content":"hi"}]}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	parts := result.Request.Turns[0].Parts
	if len(parts) != 1 {
		t.Fatalf("system parts = %d, want only the surviving text: %+v", len(parts), parts)
	}
	if text, ok := parts[0].(CanonicalText); !ok || text.Text != "be brief" {
		t.Fatalf("system part = %+v, want the real text", parts[0])
	}
}
