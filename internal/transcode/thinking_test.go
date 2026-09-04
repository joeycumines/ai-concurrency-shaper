package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// decodeThinking renders a Messages request with the given thinking config
// through decode + chat render and returns the rendered body and report.
func decodeThinking(t *testing.T, thinking string, capabilities ChatCapabilities) ([]byte, ConversionReport, error) {
	t.Helper()
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"thinking":` + thinking + `
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		return nil, ConversionReport{}, err
	}
	rendered, report, err := RenderChatRequest(result.Request, testExchangeContext(), capabilities)
	return rendered, report, err
}

// TestThinkingBudgetToEffort pins the documented deterministic threshold
// mapping used for an explicit Anthropic thinking budget.
func TestThinkingBudgetToEffort(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, "minimal"},
		{1023, "minimal"},
		{1024, "low"},
		{4095, "low"},
		{4096, "medium"},
		{16383, "medium"},
		{16384, "high"},
		{131072, "high"},
	}
	for _, c := range cases {
		if got := thinkingBudgetToEffort(c.budget); got != c.want {
			t.Errorf("thinkingBudgetToEffort(%d) = %q, want %q", c.budget, got, c.want)
		}
	}
}

// TestDecodeMessagesRequestThinkingConfig verifies the thinking request field
// decodes into the canonical IR, rejecting an enabled type without a budget
// and an unknown type.
func TestDecodeMessagesRequestThinkingConfig(t *testing.T) {
	enabled, _, err := decodeThinking(t, `{"type":"enabled","budget_tokens":4096}`, ChatCapabilities{ReasoningEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(enabled, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.ReasoningEffort == nil || *probe.ReasoningEffort != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium", probe.ReasoningEffort)
	}

	adaptive, report, err := decodeThinking(t, `{"type":"adaptive","display":"omitted"}`, ChatCapabilities{ReasoningEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	var probe2 struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(adaptive, &probe2); err != nil {
		t.Fatal(err)
	}
	if probe2.ReasoningEffort != nil {
		t.Fatalf("adaptive reasoning_effort = %v, want omitted", *probe2.ReasoningEffort)
	}
	if len(report.Losses) != 1 || report.Losses[0].Feature != FeatureRequestReasoning {
		t.Fatalf("adaptive report = %+v, want one request_reasoning note", report.Losses)
	}

	disabled, _, err := decodeThinking(t, `{"type":"disabled"}`, ChatCapabilities{ReasoningEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	var probe3 struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(disabled, &probe3); err != nil {
		t.Fatal(err)
	}
	if probe3.ReasoningEffort != nil {
		t.Fatalf("disabled reasoning_effort = %v, want omitted", *probe3.ReasoningEffort)
	}
}

// TestDecodeMessagesRequestThinkingRejections verifies the invalid shapes:
// enabled without a budget, an unknown type, and cross-type members
// (budget_tokens on disabled/adaptive, display on enabled — the official
// contract is enabled={type,budget_tokens}, disabled={type},
// adaptive={type,display}) all fail the decode (review-12 R12-L1).
func TestDecodeMessagesRequestThinkingRejections(t *testing.T) {
	for _, thinking := range []string{
		`{"type":"enabled"}`,
		`{"type":"enabled","budget_tokens":0}`,
		`{"type":"bogus"}`,
		`{"type":"enabled","budget_tokens":4096,"display":"omitted"}`,
		`{"type":"disabled","budget_tokens":4096}`,
		`{"type":"adaptive","budget_tokens":4096,"display":"omitted"}`,
	} {
		if _, _, err := decodeThinking(t, thinking, ChatCapabilities{ReasoningEffort: true}); err == nil {
			t.Errorf("thinking %s: want decode error", thinking)
		}
	}
}

// TestMessagesThinkingDisabledNoted pins that `thinking: disabled` — an
// explicit client-asserted behavior, not an omission — is observably reported
// at render, exactly like adaptive (analysis doc 05 §4 / G8).
func TestMessagesThinkingDisabledNoted(t *testing.T) {
	_, report, err := decodeThinking(t, `{"type":"disabled"}`, ChatCapabilities{ReasoningEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature == FeatureRequestReasoning &&
			loss.Path == "thinking" &&
			strings.Contains(loss.Detail, "disabled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want a request_reasoning note for disabled thinking", report.Losses)
	}
}

// TestDecodeMessagesRequestThinkingNoCapability verifies an explicit enabled
// budget under a chat provider without the reasoning_effort capability is a
// loss/reject decision (request_reasoning), never a silent drop.
func TestDecodeMessagesRequestThinkingNoCapability(t *testing.T) {
	_, _, err := decodeThinking(t, `{"type":"enabled","budget_tokens":4096}`, ChatCapabilities{})
	if err == nil {
		t.Fatal("expected rejection without the reasoning_effort capability")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("error = %T: %v", err, err)
	}
	if target.Feature != string(FeatureRequestReasoning) {
		t.Fatalf("feature = %q, want request_reasoning", target.Feature)
	}

	// With the loss approved, the budget is dropped and reported.
	ctx := testExchangeContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning: {},
	}}
	body := []byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rendered, report, err := RenderChatRequest(result.Request, ctx, ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "reasoning_effort") {
		t.Fatalf("rendered %s, want no reasoning_effort", rendered)
	}
	if len(report.Losses) != 1 || report.Losses[0].Feature != FeatureRequestReasoning {
		t.Fatalf("report = %+v, want one request_reasoning loss", report.Losses)
	}
}

// TestMessagesThinkingRejectedOnResponsesTarget proves an explicit thinking
// budget is never silently dropped when the target cannot express it: the
// messages->responses render rejects under the strict policy and records the
// loss under a permissive policy (review finding: the decode alone must not
// change a hard error into a silent drop).
func TestMessagesThinkingRejectedOnResponsesTarget(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, renderErr := RenderResponsesRequest(result.Request, testExchangeContext()); renderErr == nil {
		t.Fatal("expected the messages->responses render to reject the thinking budget")
	} else {
		var target *UnsupportedFeatureError
		if !errors.As(renderErr, &target) || target.Feature != string(FeatureRequestReasoning) {
			t.Fatalf("error = %T: %v, want request_reasoning", renderErr, renderErr)
		}
	}

	// Adaptive and disabled carry no budget; the target's own default is the
	// exact semantic, so they are not gated.
	for _, thinking := range []string{
		`{"type":"adaptive","display":"omitted"}`,
		`{"type":"disabled"}`,
	} {
		b := []byte(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":false,"thinking":` + thinking + `}`)
		r, err := DecodeMessagesRequest(b, StrictLossPolicy())
		if err != nil {
			t.Fatalf("decode %s: %v", thinking, err)
		}
		if _, _, err := RenderResponsesRequest(r.Request, testExchangeContext()); err != nil {
			t.Fatalf("render %s: %v", thinking, err)
		}
	}

	// Under a permissive policy the enabled budget is an approved loss.
	ctx := testExchangeContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning: {},
		FeatureTopK:             {},
	}}
	_, report, err := RenderResponsesRequest(result.Request, ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature == FeatureRequestReasoning && strings.Contains(loss.Path, "thinking") {
			found = true
		}
	}
	if !found {
		t.Fatalf("thinking loss not recorded: %+v", report.Losses)
	}
}

// TestMessagesThinkingBudgetMapsToResponsesEffort is the task 21 reproduction
// for the high finding request_reasoning-default-native-path: an explicit
// Anthropic thinking budget must map to Responses reasoning.effort when the
// exchange grants the ReasoningEffort capability, instead of being consumed as
// an approved request_reasoning loss that never appears on the upstream
// request. Before the mapping existed this test failed with an
// UnsupportedFeatureError even under the strict policy because
// RequirePortableArtifacts gated the budget; the previous renderer never
// emitted reasoning.effort for a Messages-sourced request.
func TestMessagesThinkingBudgetMapsToResponsesEffort(t *testing.T) {
	cases := []struct {
		thinking string
		want     string
	}{
		{`{"type":"enabled","budget_tokens":1023}`, "minimal"},
		{`{"type":"enabled","budget_tokens":4096}`, "medium"},
		{`{"type":"enabled","budget_tokens":16384}`, "high"},
	}
	for _, c := range cases {
		body := []byte(`{
			"model":"m","max_tokens":100,
			"messages":[{"role":"user","content":"hi"}],
			"stream":false,
			"thinking":` + c.thinking + `
		}`)
		result, err := DecodeMessagesRequest(body, StrictLossPolicy())
		if err != nil {
			t.Fatalf("decode %s: %v", c.thinking, err)
		}
		ctx := testExchangeContext()
		ctx.Capabilities = ChatCapabilities{ReasoningEffort: true}
		rendered, report, err := RenderResponsesRequest(result.Request, ctx)
		if err != nil {
			t.Fatalf("render %s with ReasoningEffort: %v", c.thinking, err)
		}
		var envelope struct {
			Reasoning *ResponsesEnvelopeReasoning `json:"reasoning"`
		}
		if err := json.Unmarshal(rendered, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Reasoning == nil ||
			envelope.Reasoning.Effort == nil ||
			*envelope.Reasoning.Effort != c.want {
			t.Fatalf("thinking %s: reasoning = %+v, want effort %q", c.thinking, envelope.Reasoning, c.want)
		}
		mapped := false
		for _, loss := range report.Losses {
			if loss.Feature == FeatureRequestReasoning &&
				strings.Contains(loss.Detail, "mapped to Responses reasoning.effort") {
				mapped = true
			}
		}
		if !mapped {
			t.Fatalf("thinking %s: mapping note not recorded: %+v", c.thinking, report.Losses)
		}
	}

	// Without the capability the same request stays a policy-gated loss, never
	// a silently rendered budget (this is the existing capability-off contract,
	// pinned here so the two capability arms cannot diverge).
	body := []byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RenderResponsesRequest(result.Request, testExchangeContext()); err == nil {
		t.Fatal("expected the messages->responses render to reject the thinking budget without the capability")
	}
}

// TestMessagesThinkingEnabledWithoutBudgetNoted pins the task-21 never-silent
// invariant for the render path: an "enabled" thinking config rendered with no
// budget — a shape decode rejects today but a hand-built CanonicalRequest or a
// future decode relaxation can reach — is never silently passed. Both renderers
// record exactly one request_reasoning note naming the upstream default effort
// and still render a body; neither hard-errors and neither is silent.
func TestMessagesThinkingEnabledWithoutBudgetNoted(t *testing.T) {
	result, err := DecodeMessagesRequest([]byte(`{
		"model":"m",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the decoded shape deliberately to reach the hand-built /
	// relaxed-decode surface the invariant protects.
	result.Request.Thinking.BudgetTokens = nil

	assertSingleNote := func(rendered []byte, report ConversionReport, wantDetail string) {
		t.Helper()
		if len(rendered) == 0 {
			t.Fatal("empty rendered body")
		}
		if !json.Valid(rendered) {
			t.Fatalf("rendered body is not valid JSON: %s", rendered)
		}
		count := 0
		for _, loss := range report.Losses {
			if loss.Feature != FeatureRequestReasoning {
				continue
			}
			count++
			if loss.Detail != wantDetail {
				t.Fatalf("reasoning detail = %q, want %q", loss.Detail, wantDetail)
			}
		}
		if count != 1 {
			t.Fatalf("request_reasoning notes = %d (%+v), want exactly one", count, report.Losses)
		}
	}

	// Native Responses renderer.
	rendered, report, err := RenderResponsesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatalf("responses render: %v", err)
	}
	assertSingleNote(
		rendered,
		report,
		"thinking enabled without a budget maps to the upstream provider's default reasoning effort",
	)

	// Chat renderer, capability granted: the budget-less shape is still
	// unreproducible and must not be silent.
	rendered, report, err = RenderChatRequest(
		result.Request,
		testExchangeContext(),
		ChatCapabilities{ReasoningEffort: true},
	)
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	assertSingleNote(
		rendered,
		report,
		"thinking enabled without a budget maps to the chat provider's default reasoning effort",
	)
}
