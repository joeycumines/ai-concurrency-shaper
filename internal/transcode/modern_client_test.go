package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// TestAnthropicRequestModernClientFields verifies the newest Claude Code wire
// surface decodes: context_management, output_config, cache_control on text
// blocks and tools, inline system-role messages, and adaptive thinking. The
// two envelope controls are client-side semantics no target reproduces: under
// the strict policy the request is REJECTED naming anthropic_controls, and
// under an approving policy both drops are recorded observably (review-11
// finding 1 — never a silent drop).
func TestAnthropicRequestModernClientFields(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"output_config":{"budget_tokens":32000},
		"thinking":{"type":"adaptive","display":"omitted"},
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"t","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"system","content":"inline system"},
			{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	// The strict programmatic policy rejects: the envelope controls carry
	// semantics (an output budget, server-side context trimming) that must not
	// disappear without an explicit approval.
	if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
		t.Fatal("strict policy accepted the anthropic envelope controls; want rejection")
	} else {
		var target *UnsupportedFeatureError
		if !errors.As(err, &target) {
			t.Fatalf("error = %T: %v, want UnsupportedFeatureError", err, err)
		}
		if target.Feature != string(FeatureAnthropicControls) {
			t.Fatalf("feature = %q, want anthropic_controls", target.Feature)
		}
	}

	// An approving policy decodes cleanly and records both drops.
	result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicControls: {},
	}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Request.Thinking == nil || result.Request.Thinking.Type != "adaptive" {
		t.Fatalf("thinking = %+v", result.Request.Thinking)
	}
	// Inline system role becomes a canonical system turn.
	foundSystem := false
	for _, turn := range result.Request.Turns {
		if turn.Role == CanonicalSystem {
			foundSystem = true
		}
	}
	if !foundSystem {
		t.Fatalf("no system turn: %+v", result.Request.Turns)
	}
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "t" {
		t.Fatalf("tools = %+v", result.Request.Tools)
	}
	paths := map[string]bool{}
	for _, loss := range result.Report.Losses {
		if loss.Feature != FeatureAnthropicControls {
			t.Fatalf("unexpected loss %q (%s): %+v", loss.Feature, loss.Path, result.Report.Losses)
		}
		paths[loss.Path] = true
	}
	if !paths["context_management"] || !paths["output_config"] {
		t.Fatalf("report = %+v, want observable losses for both controls", result.Report.Losses)
	}
}

// TestAnthropicRequestControlsIndependent proves each envelope control is
// gated on its own: a request carrying only context_management records exactly
// one anthropic_controls loss, and one carrying only output_config likewise.
func TestAnthropicRequestControlsIndependent(t *testing.T) {
	cases := []struct {
		name   string
		extra  string
		report []string
	}{
		{"context_management_only", `"context_management":{"edits":[]},`, []string{"context_management"}},
		{"output_config_only", `"output_config":{"budget_tokens":32000},`, []string{"output_config"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{
				"model":"m",
				"max_tokens":100,
				` + c.extra + `
				"messages":[{"role":"user","content":"hi"}]
			}`)
			// Strict rejects.
			if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
				t.Fatal("strict policy accepted; want rejection")
			}
			// Approving decodes and records exactly the present control.
			result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
				FeatureAnthropicControls: {},
			}})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(result.Report.Losses) != len(c.report) {
				t.Fatalf("losses = %+v, want %d entries", result.Report.Losses, len(c.report))
			}
			for i, want := range c.report {
				if result.Report.Losses[i].Path != want ||
					result.Report.Losses[i].Feature != FeatureAnthropicControls {
					t.Fatalf("loss[%d] = %+v, want %s/anthropic_controls", i, result.Report.Losses[i], want)
				}
			}
		})
	}
}

// TestOpenAIChatResponseProviderExtensions verifies the non-streaming chat
// response surface mirrors the streaming surface's opaque provider extensions
// (prompt_token_ids, prompt_text): a real provider response carrying them must
// strict-decode through BOTH the wire type and the full DecodeChatResponse
// path instead of failing the exchange as corrupt upstream wire (review-11
// finding 5).
func TestOpenAIChatResponseProviderExtensions(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","prompt_token_ids":[1,2,3],"prompt_text":"hi","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","token_ids":null,"routed_experts":null}]}`)

	// The wire type itself must strict-decode the extensions.
	var response openaichat.Response
	if err := wire.Decode(raw, &response); err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	if response.ID != "chatcmpl-1" {
		t.Fatalf("id = %q", response.ID)
	}

	// The full conversion path must accept the same body.
	decoded, _, err := DecodeChatResponseWithPolicy(raw, ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("DecodeChatResponse: %v", err)
	}
	if decoded.ID != "chatcmpl-1" || decoded.Model != "m" {
		t.Fatalf("decoded = %+v", decoded)
	}

	// The extensions never leak into the client-dialect render: the canonical
	// response carries no trace of prompt_token_ids/prompt_text.
	context := testExchangeContext()
	// The test fixture carries no usage; the usage_unknown loss is unrelated
	// to the extension fields under test.
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	rendered, _, err := RenderMessagesResponse(decoded, context)
	if err != nil {
		t.Fatalf("RenderMessagesResponse: %v", err)
	}
	for _, forbidden := range []string{"prompt_token_ids", "prompt_text"} {
		if bytes.Contains(rendered, []byte(forbidden)) {
			t.Fatalf("client response leaks %s: %s", forbidden, rendered)
		}
	}
}

// TestAnthropicCacheControlNoted pins that cache_control — the Anthropic
// prompt-cache performance hint real clients attach to text blocks, tools,
// and the system prompt — is never silently dropped: the decode records an
// observable anthropic_controls note, deduped per exchange (analysis G3).
func TestAnthropicCacheControlNoted(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},
				{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}
			]}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("cache_control must decode under every policy (it is a performance hint): %v", err)
	}
	count := 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicControls && loss.Path == "cache_control" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cache_control notes = %d, want exactly one deduped note (report: %+v)", count, result.Report.Losses)
	}

	// The rendered upstream body carries no cache_control bytes.
	rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if bytes.Contains(rendered, []byte("cache_control")) {
		t.Fatalf("upstream body leaks cache_control: %s", rendered)
	}
}

// TestOpenAIChatResponseMessageLevelExtensions pins that a provider body
// carrying the opaque extensions at MESSAGE level (inside
// choices[].message, not on the choice or envelope) decodes end-to-end:
// before the fix the wire Message type rejected them as unknown fields
// while chatMessageShadow modeled them, so the fields were dead surface and
// the exchange failed as corrupt upstream wire (gate run 2 F1). Value
// fixtures, not explicit nulls — presence is what the wire decode rejects.
func TestOpenAIChatResponseMessageLevelExtensions(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-ml","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok","token_ids":[7,8,9],"routed_experts":["expert-Alpha-9"],"stop_reason":"end_turn"},"finish_reason":"stop"}]}`)

	var response openaichat.Response
	if err := wire.Decode(raw, &response); err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	msg := response.Choices[0].Message
	if msg == nil || msg.ChatAssistantMessage == nil {
		t.Fatalf("message = %+v", msg)
	}
	if len(msg.ChatAssistantMessage.TokenIDs.([]any)) != 3 {
		t.Fatalf("token_ids = %+v", msg.ChatAssistantMessage.TokenIDs)
	}
	if msg.ChatAssistantMessage.RoutedExperts == nil {
		t.Fatalf("routed_experts = %+v", msg.ChatAssistantMessage.RoutedExperts)
	}
	if msg.ChatAssistantMessage.StopReason == nil || *msg.ChatAssistantMessage.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %+v", msg.ChatAssistantMessage.StopReason)
	}

	decoded, _, err := DecodeChatResponseWithPolicy(raw, ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("DecodeChatResponse: %v", err)
	}
	if decoded.ID != "chatcmpl-ml" {
		t.Fatalf("id = %q", decoded.ID)
	}

	// The extensions never leak into the client-dialect render. The
	// Messages dialect has its own envelope stop_reason (end_turn), so the
	// leak check targets the extension VALUES and the names the dialect
	// does not share.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	rendered, _, err := RenderMessagesResponse(decoded, context)
	if err != nil {
		t.Fatalf("RenderMessagesResponse: %v", err)
	}
	for _, forbidden := range []string{"token_ids", "routed_experts", "expert-Alpha-9"} {
		if bytes.Contains(rendered, []byte(forbidden)) {
			t.Fatalf("client response leaks %s: %s", forbidden, rendered)
		}
	}
}

// TestOpenAIChatStreamProviderExtensions verifies provider-extension fields
// on real chat streams (yolo gateway) decode: prompt_token_ids and
// prompt_text on the chunk, reasoning deltas, and the
// created_cache_tokens/multimodal_tokens usage details — each asserted, not
// just mentioned (review-12 R12-L3).
func TestOpenAIChatStreamProviderExtensions(t *testing.T) {
	raw := []byte(`{"id":"chunk1","object":"chat.completion.chunk","created":1,"model":"m","prompt_token_ids":null,"prompt_text":null,"choices":[{"index":0,"delta":{"role":"assistant","reasoning":"hmm"},"finish_reason":null,"token_ids":null,"routed_experts":null}]}`)
	var chunk openaichat.StreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("chunk decode: %v", err)
	}
	if chunk.Choices[0].Delta == nil || chunk.Choices[0].Delta.Reasoning == nil || *chunk.Choices[0].Delta.Reasoning != "hmm" {
		t.Fatalf("delta reasoning = %+v", chunk.Choices[0].Delta)
	}
	// The extension shadow is opaque (any-typed): explicit null and absent
	// both decode to nil by design, so the honest assertions are the value
	// fixtures below.
	withValues := []byte(`{"id":"chunk2","object":"chat.completion.chunk","created":1,"model":"m","prompt_token_ids":[1,2,3],"prompt_text":"hi","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null,"token_ids":null,"routed_experts":null}]}`)
	var chunk2 openaichat.StreamChunk
	if err := json.Unmarshal(withValues, &chunk2); err != nil {
		t.Fatalf("chunk2 decode: %v", err)
	}
	ids, ok := chunk2.PromptTokenIDs.([]any)
	if !ok || len(ids) != 3 || ids[2] != float64(3) {
		t.Fatalf("prompt_token_ids = %#v, want [1 2 3]", chunk2.PromptTokenIDs)
	}
	if chunk2.PromptText == nil || *chunk2.PromptText != "hi" {
		t.Fatalf("prompt_text = %v, want hi", chunk2.PromptText)
	}

	usage := []byte(`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3,"created_cache_tokens":2,"multimodal_tokens":1}}`)
	var u openaichat.LLMUsage
	if err := json.Unmarshal(usage, &u); err != nil {
		t.Fatalf("usage decode: %v", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 3 {
		t.Fatalf("prompt details = %+v", u.PromptTokensDetails)
	}
	if u.PromptTokensDetails.CreatedCacheTokens == nil || *u.PromptTokensDetails.CreatedCacheTokens != 2 {
		t.Fatalf("created_cache_tokens = %v, want 2", u.PromptTokensDetails.CreatedCacheTokens)
	}
	if u.PromptTokensDetails.MultimodalTokens == nil || *u.PromptTokensDetails.MultimodalTokens != 1 {
		t.Fatalf("multimodal_tokens = %v, want 1", u.PromptTokensDetails.MultimodalTokens)
	}
}

// TestAnthropicMessagesThinkingWireType pins the thinking config wire shape.
// systemTurnTexts extracts the text of every system turn in wire order.
func systemTurnTexts(turns []CanonicalTurn) []string {
	var texts []string
	for _, turn := range turns {
		if turn.Role != CanonicalSystem {
			continue
		}
		for _, part := range turn.Parts {
			if text, ok := part.(CanonicalText); ok {
				texts = append(texts, text.Text)
			}
		}
	}
	return texts
}

// requireTurnSequence asserts the decoded turn roles equal want exactly.
func requireTurnSequence(t *testing.T, turns []CanonicalTurn, want ...CanonicalRole) {
	t.Helper()
	if len(turns) != len(want) {
		t.Fatalf("turns = %+v, want roles %v", turns, want)
	}
	for i, role := range want {
		if turns[i].Role != role {
			t.Fatalf("turn[%d].role = %v, want %v (turns: %+v)", i, turns[i].Role, role, turns)
		}
	}
}

// TestMessagesSystemCoexistence pins how the two Anthropic system surfaces
// combine: the top-level envelope.system field becomes a system turn FIRST
// (appended before the messages loop), then inline system-role messages map
// to system turns in wire order (analysis G7/G11). Under strict policy the
// Responses render rejects the two system turns (multiple_system_turns);
// under the permission both coexist observably.
func TestMessagesSystemCoexistence(t *testing.T) {
	// Top-level system, then one inline system message interleaved with a
	// user turn: the decode order must be top-level first, inline in wire
	// order.
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"system":[{"type":"text","text":"top"}],
		"messages":[
			{"role":"user","content":"first"},
			{"role":"system","content":"inline"},
			{"role":"user","content":"second"}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode must not depend on the system-turn count: %v", err)
	}
	requireTurnSequence(t,
		result.Request.Turns,
		CanonicalSystem, CanonicalUser, CanonicalSystem, CanonicalUser,
	)
	if got := systemTurnTexts(result.Request.Turns); len(got) != 2 || got[0] != "top" || got[1] != "inline" {
		t.Fatalf("system texts = %v, want [top inline] (top-level first)", got)
	}

	// A string system field behaves the same as the block form.
	plain, err := DecodeMessagesRequest([]byte(`{
		"model":"m",
		"max_tokens":100,
		"system":"top",
		"messages":[
			{"role":"system","content":"inline"},
			{"role":"user","content":"hi"}
		]
	}`), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	requireTurnSequence(t, plain.Request.Turns, CanonicalSystem, CanonicalSystem, CanonicalUser)
	if got := systemTurnTexts(plain.Request.Turns); len(got) != 2 || got[0] != "top" || got[1] != "inline" {
		t.Fatalf("system texts = %v, want [top inline]", got)
	}

	// INTENDED SEMANTIC CHANGE (autopsy 02, task 14): the Chat render no
	// longer carries multiple system turns positionally — open-weights
	// chat templates (Qwen/Llama/DeepSeek Jinja) reject any role:system
	// message after index 0, including a second leading one, which killed
	// 100% of real Claude Code multi-turn traffic in the field. This
	// fixture's inline system turn follows dialog turns, so the strict
	// render now rejects the position loss; under the
	// mid_conversation_system permission the system-channel turns
	// consolidate into one leading system message carrying both texts in
	// order.
	if _, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{}); err == nil {
		t.Fatal("strict chat render accepted a mid-conversation system turn; want rejection")
	}
	permissiveChat := testExchangeContext()
	permissiveChat.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem: {},
	}}
	rendered, report, err := RenderChatRequest(result.Request, permissiveChat, ChatCapabilities{})
	if err != nil {
		t.Fatalf("permissive chat render: %v", err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	systemCount := 0
	var systemContents []string
	for i, message := range chat.Messages {
		if message.Role != ChatMessageRoleSystem {
			continue
		}
		systemCount++
		if i != 0 {
			t.Fatalf("system message at index %d violates the single-leading-system property: %s", i, rendered)
		}
		// canonicalTextTurnToChatMessage emits text blocks; consolidation
		// joins them into one block.
		if message.Content == nil {
			t.Fatalf("system message has no content: %+v", message)
		}
		for _, block := range message.Content.ContentBlocks {
			if block.Type != ChatContentBlockTypeText || block.Text == nil {
				t.Fatalf("system message block = %+v, want a text block", block)
			}
			systemContents = append(systemContents, *block.Text)
		}
	}
	if systemCount != 1 {
		t.Fatalf("chat system messages = %d, want exactly 1: %s", systemCount, rendered)
	}
	if len(systemContents) != 1 || systemContents[0] != "top\n\ninline" {
		t.Fatalf("consolidated chat system content = %v, want [\"top\\n\\ninline\"]", systemContents)
	}
	found := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("mid_conversation_system entries = %d, want exactly 1: %+v", found, report.Losses)
	}

	// The Responses render cannot express two system turns as one
	// instructions string: strict rejects, and the permission records the
	// loss and emits NO instructions — the multi-turn system prompt is the
	// shape the loss drops (the single-turn builder only runs for exactly
	// one system turn; review-j finding 13), never an illegal items array.
	if _, _, err := RenderResponsesRequest(result.Request, testExchangeContext()); err == nil {
		t.Fatal("strict responses render accepted multiple system turns; want rejection")
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMultipleSystemTurns: {},
	}}
	responsesRendered, report, err := RenderResponsesRequest(result.Request, permissive)
	if err != nil {
		t.Fatalf("permissive responses render: %v", err)
	}
	if !reportHasFeature(report, FeatureMultipleSystemTurns) {
		t.Fatalf("report lacks the multiple-system-turns loss: %+v", report)
	}
	var envelope openairesponses.Request
	if err := strictDecode(responsesRendered, &envelope); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, responsesRendered)
	}
	if envelope.Instructions != nil {
		t.Fatalf(
			"instructions = %q, want nil (the approved loss drops the whole multi-turn system prompt)",
			*envelope.Instructions,
		)
	}
	if strings.Contains(string(responsesRendered), `"items"`) {
		t.Fatalf("multi-turn instructions rendered as items: %s", responsesRendered)
	}
}

func TestAnthropicMessagesThinkingWireType(t *testing.T) {
	var r anthropicmessages.Request
	if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":1,"messages":[],"thinking":{"type":"enabled","budget_tokens":4096}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Thinking == nil || r.Thinking.Type != "enabled" || r.Thinking.BudgetTokens == nil || *r.Thinking.BudgetTokens != 4096 {
		t.Fatalf("thinking = %+v", r.Thinking)
	}
}
