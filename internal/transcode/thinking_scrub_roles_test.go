package transcode

// Thinking parts reach the canonical IR in exactly one supported form: a
// proxy-synthesized block carrying SyntheticThinkingSignature, which the request
// path must scrub before any upstream rendering. Two guarantees are tested here.
//
//  1. A thinking part WITHOUT the marker is refused centrally, by
//     ValidateCanonicalRequest. The artifact gate covers request.Artifacts only,
//     so a turn part was never gated by it; refusing is the only outcome that
//     keeps AGENTS.md's "preserve byte-for-byte or reject/explicitly lose them"
//     honest without inventing positions the IR does not carry.
//  2. The marker scrub happens in EVERY turn role on EVERY renderer. A scrub
//     that works for an assistant turn but errors in a system turn, or charges
//     the client a loss for content it never wrote, is the same silent
//     inconsistency in a different place.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// thinkingTurn builds a one-part thinking turn with the given signature.
func thinkingTurn(role CanonicalRole, signature string) CanonicalTurn {
	return CanonicalTurn{
		Role:  role,
		Parts: []CanonicalPart{CanonicalThinkingPart{Text: "reasoning", Signature: signature}},
	}
}

// TestNonMarkerThinkingPartIsRefusedEverywhere proves a turn part that is not
// proxy-synthesized is a typed refusal at validation, so no renderer can drop it
// silently — the artifact gate never saw it, and there is no position to restore
// it to.
func TestNonMarkerThinkingPartIsRefusedEverywhere(t *testing.T) {
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			request := positionRequest(0,
				textTurn(CanonicalUser, "hi"),
				thinkingTurn(CanonicalAssistant, "a-real-provider-signature"),
			)
			// Every policy approves every loss: nothing but the missing marker
			// signature can refuse this request.
			context := testExchangeContext()
			context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{}}
			for key := range allLossKeys() {
				context.LossPolicy.Allowed[key] = struct{}{}
			}
			_, _, err := render(request, context)
			if err == nil {
				t.Fatal("a non-marker thinking part was accepted and would be scrubbed")
			}
			if !strings.Contains(err.Error(), string(FeatureAuthenticatedThinking)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureAuthenticatedThinking)
			}
			if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
				t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
			}
		})
	}
}

// TestMarkerThinkingIsScrubbedInEveryTurnRole proves the scrub is positional
// rather than assistant-only: a marker-signature part dropped out of a user or
// system turn leaves the rest of that turn intact on every renderer, with no
// error and no policy decision.
func TestMarkerThinkingIsScrubbedInEveryTurnRole(t *testing.T) {
	cases := []struct {
		name  string
		turns []CanonicalTurn
	}{
		{
			name: "assistant",
			turns: []CanonicalTurn{
				textTurn(CanonicalUser, "hi"),
				{Role: CanonicalAssistant, Parts: []CanonicalPart{
					CanonicalThinkingPart{Text: "reasoning", Signature: SyntheticThinkingSignature},
					CanonicalText{Text: "answer"},
				}},
			},
		},
		{
			name: "system",
			turns: []CanonicalTurn{
				{Role: CanonicalSystem, Parts: []CanonicalPart{
					CanonicalThinkingPart{Text: "reasoning", Signature: SyntheticThinkingSignature},
					CanonicalText{Text: "be brief"},
				}},
				textTurn(CanonicalUser, "hi"),
			},
		},
		{
			name: "user",
			turns: []CanonicalTurn{
				{Role: CanonicalUser, Parts: []CanonicalPart{
					CanonicalThinkingPart{Text: "reasoning", Signature: SyntheticThinkingSignature},
					CanonicalText{Text: "hi"},
				}},
			},
		},
	}
	for name, render := range allRequestRenderers() {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				request := positionRequest(0, tc.turns...)
				rendered, report, err := render(request, testExchangeContext())
				if err != nil {
					t.Fatalf("a marker-signature thinking part in a %s turn must be scrubbed: %v", tc.name, err)
				}
				if strings.Contains(string(rendered), "reasoning") {
					t.Fatalf("marker thinking reached the wire: %s", rendered)
				}
				if reportHasFeature(report, FeatureAuthenticatedThinking) {
					t.Fatalf("a proxy-synthesized part was charged the authenticated-thinking key: %+v", report.Losses)
				}
			})
		}
	}
}

// TestThinkingOnlyTurnIsNotSilentlyDropped proves a thinking-only turn cannot
// vanish into a contentless wire item. The expected verdict differs per target
// and is enumerated so a future silent drop has to change this table: Chat would
// otherwise emit a bodiless assistant message, Messages and Gemini refuse a
// message with no blocks, and Responses would fail wire validation. None of them
// may emit an empty content array or an assistant message with no content.
func TestThinkingOnlyTurnIsNotSilentlyDropped(t *testing.T) {
	// Each renderer's honest outcome for a turn whose only part is scrubbed.
	verdicts := map[string]string{
		"chat":      "refuse or omit the empty message",
		"messages":  "refuse",
		"responses": "refuse",
		"gemini":    "refuse",
	}
	renderers := allRequestRenderers()
	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			expect, ok := verdicts[name]
			if !ok {
				t.Fatalf("no expected verdict for renderer %q: add it to the table", name)
			}
			request := positionRequest(0,
				textTurn(CanonicalUser, "hi"),
				thinkingTurn(CanonicalAssistant, SyntheticThinkingSignature),
			)
			rendered, _, err := render(request, testExchangeContext())
			if err != nil {
				// A refusal is honest: the turn would have had no content. It
				// must, however, be a LOCAL, path-named refusal — never a wire
				// marshal failure such as "message content has no selected
				// variant", which describes a marshaller bug to the operator.
				if strings.Contains(err.Error(), "no selected variant") ||
					strings.Contains(err.Error(), "MarshalJSON") {
					t.Fatalf("a thinking-only turn surfaced a wire-marshal failure (%s): %v", expect, err)
				}
				return
			}
			body := string(rendered)
			if strings.Contains(body, `"content":[]`) || strings.Contains(body, `"parts":[]`) {
				t.Fatalf("an empty content array was emitted for a thinking-only turn (%s): %s", expect, body)
			}
			// A rendered body must not carry a contentless assistant message:
			// one with no content, no tool calls and no refusal is a bodiless
			// turn the upstream would have to interpret.
			var messages []struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
				Refusal   json.RawMessage `json:"refusal"`
			}
			if err := json.Unmarshal(rendered, &struct {
				Messages *[]struct {
					Role      string          `json:"role"`
					Content   json.RawMessage `json:"content"`
					ToolCalls json.RawMessage `json:"tool_calls"`
					Refusal   json.RawMessage `json:"refusal"`
				} `json:"messages"`
			}{Messages: &messages}); err == nil {
				for _, message := range messages {
					if message.Role != "assistant" {
						continue
					}
					if len(message.Content) == 0 && len(message.ToolCalls) == 0 && len(message.Refusal) == 0 {
						t.Fatalf("a bodiless assistant message was emitted for a thinking-only turn: %s", body)
					}
				}
			}
		})
	}
}
