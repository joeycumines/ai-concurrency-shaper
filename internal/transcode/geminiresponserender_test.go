package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func geminiRenderContext(policy LossPolicy, includeThoughts bool) *ExchangeContext {
	return &ExchangeContext{
		RequestedClientModel:  "gemini-2.5-pro",
		IDs:                   NewExchangeIDs(),
		LossPolicy:            policy,
		GeminiIncludeThoughts: includeThoughts,
	}
}

func geminiCompletedResponse(items []CanonicalResponseItem, stop CanonicalStopReason, usage CanonicalUsage) CanonicalResponse {
	return CanonicalResponse{
		ID:     "upstream-1",
		Model:  "upstream-model",
		Status: CanonicalResponseCompleted,
		Stop:   CanonicalStop{Reason: stop},
		Items:  items,
		Usage:  usage,
	}
}

func geminiRenderedDoc(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rendered response is not JSON: %v\n%s", err, raw)
	}
	return doc
}

func geminiFirstCandidate(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	candidates, ok := doc["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("candidates = %v, want exactly one", doc["candidates"])
	}
	return candidates[0].(map[string]any)
}

func TestRenderGeminiResponseTextCompletion(t *testing.T) {
	items := []CanonicalResponseItem{&CanonicalMessageItem{
		Role:  CanonicalAssistant,
		Parts: []CanonicalPart{CanonicalText{Text: "hello"}},
	}}
	usage := CanonicalUsage{
		InputTokens: 12, InputKnown: true,
		OutputTokens: 9, OutputKnown: true,
		ReasoningTokens: 4, ReasoningKnown: true,
		CacheReadTokens: 3, CacheReadKnown: true,
		CacheWriteKnown: true,
		TotalTokens:     21, TotalKnown: true,
	}
	raw, report, err := RenderGeminiResponse(
		geminiCompletedResponse(items, CanonicalStopEndTurn, usage),
		geminiRenderContext(StrictLossPolicy(), false),
	)
	if err != nil {
		t.Fatalf("RenderGeminiResponse error: %v", err)
	}
	if len(report.Losses) != 0 {
		t.Fatalf("unexpected report entries: %+v", report.Losses)
	}
	doc := geminiRenderedDoc(t, raw)
	candidate := geminiFirstCandidate(t, doc)
	if candidate["finishReason"] != "STOP" {
		t.Fatalf("finishReason = %v", candidate["finishReason"])
	}
	content := candidate["content"].(map[string]any)
	if content["role"] != "model" {
		t.Fatalf("role = %v", content["role"])
	}
	parts := content["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("parts = %v", parts)
	}
	usageDoc := doc["usageMetadata"].(map[string]any)
	want := map[string]float64{
		"promptTokenCount":        12,
		"candidatesTokenCount":    5,
		"thoughtsTokenCount":      4,
		"cachedContentTokenCount": 3,
		"totalTokenCount":         21,
	}
	for key, value := range want {
		if usageDoc[key] != value {
			t.Fatalf("usageMetadata.%s = %v, want %v", key, usageDoc[key], value)
		}
	}
	if doc["modelVersion"] != "gemini-2.5-pro" {
		t.Fatalf("modelVersion = %v, want the client-facing alias", doc["modelVersion"])
	}
	if id, _ := doc["responseId"].(string); !strings.HasPrefix(id, "resp_") {
		t.Fatalf("responseId = %v", doc["responseId"])
	}
}

func TestRenderGeminiResponseFailedStatusIsNeverSuccess(t *testing.T) {
	response := CanonicalResponse{
		ID:           "upstream-1",
		Model:        "m",
		Status:       CanonicalResponseFailed,
		Stop:         CanonicalStop{Reason: CanonicalStopEndTurn},
		ErrorMessage: "overloaded",
	}
	raw, _, err := RenderGeminiResponse(response, geminiRenderContext(StrictLossPolicy(), false))
	if raw != nil {
		t.Fatalf("failed exchange rendered a body: %s", raw)
	}
	var semantic *UpstreamSemanticFailureError
	if !errors.As(err, &semantic) || semantic.Message != "overloaded" {
		t.Fatalf("error = %v, want an upstream semantic failure", err)
	}
}

func TestRenderGeminiResponseThinking(t *testing.T) {
	items := []CanonicalResponseItem{&CanonicalMessageItem{
		Role: CanonicalAssistant,
		Parts: []CanonicalPart{
			CanonicalThinkingPart{Text: "let me think"},
			CanonicalText{Text: "answer"},
		},
	}}
	usage := knownGeminiUsage()

	t.Run("thoughts requested render as thought parts with a note", func(t *testing.T) {
		raw, report, err := RenderGeminiResponse(
			geminiCompletedResponse(items, CanonicalStopEndTurn, usage),
			geminiRenderContext(StrictLossPolicy(), true),
		)
		if err != nil {
			t.Fatalf("RenderGeminiResponse error: %v", err)
		}
		parts := geminiFirstCandidate(t, geminiRenderedDoc(t, raw))["content"].(map[string]any)["parts"].([]any)
		first := parts[0].(map[string]any)
		if first["text"] != "let me think" || first["thought"] != true {
			t.Fatalf("thought part = %v", first)
		}
		if !geminiReportHas(report, FeatureGeminiThoughtText, NoteRecord) {
			t.Fatalf("thought rendering not noted")
		}
	})

	t.Run("thoughts not requested are an approved loss", func(t *testing.T) {
		raw, report, err := RenderGeminiResponse(
			geminiCompletedResponse(items, CanonicalStopEndTurn, usage),
			geminiRenderContext(geminiAllowAll(), false),
		)
		if err != nil {
			t.Fatalf("RenderGeminiResponse error: %v", err)
		}
		parts := geminiFirstCandidate(t, geminiRenderedDoc(t, raw))["content"].(map[string]any)["parts"].([]any)
		if len(parts) != 1 {
			t.Fatalf("parts = %v, want only the answer", parts)
		}
		if !geminiReportHas(report, FeatureProviderReasoningText, LossRecord) {
			t.Fatalf("reasoning text loss not recorded")
		}
	})

	t.Run("thoughts not requested under strict policy are refused", func(t *testing.T) {
		_, _, err := RenderGeminiResponse(
			geminiCompletedResponse(items, CanonicalStopEndTurn, usage),
			geminiRenderContext(StrictLossPolicy(), false),
		)
		if err == nil {
			t.Fatalf("strict policy rendered a response that drops reasoning")
		}
	})
}

func TestRenderGeminiResponseFunctionCall(t *testing.T) {
	items := []CanonicalResponseItem{&CanonicalFunctionCallItem{
		CallID:    "call-7",
		Name:      "lookup",
		Arguments: ParseToolArguments(`{"q":"x"}`),
	}}
	raw, _, err := RenderGeminiResponse(
		geminiCompletedResponse(items, CanonicalStopToolUse, knownGeminiUsage()),
		geminiRenderContext(StrictLossPolicy(), false),
	)
	if err != nil {
		t.Fatalf("RenderGeminiResponse error: %v", err)
	}
	candidate := geminiFirstCandidate(t, geminiRenderedDoc(t, raw))
	if candidate["finishReason"] != "STOP" {
		t.Fatalf("tool use finishReason = %v, want STOP", candidate["finishReason"])
	}
	call := candidate["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if call["id"] != "call-7" || call["name"] != "lookup" {
		t.Fatalf("functionCall = %v", call)
	}
	if args := call["args"].(map[string]any); args["q"] != "x" {
		t.Fatalf("args = %v", args)
	}
}

func TestRenderGeminiResponseNonObjectArgumentsAreUnrepresentable(t *testing.T) {
	items := []CanonicalResponseItem{&CanonicalFunctionCallItem{
		CallID:    "call-1",
		Name:      "f",
		Arguments: ParseToolArguments(`[1,2]`),
	}}
	_, _, err := RenderGeminiResponse(
		geminiCompletedResponse(items, CanonicalStopToolUse, knownGeminiUsage()),
		geminiRenderContext(StrictLossPolicy(), false),
	)
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("error = %v, want UnrepresentableError", err)
	}
}

func TestRenderGeminiResponseFinishReasons(t *testing.T) {
	refusal := []CanonicalResponseItem{&CanonicalMessageItem{
		Role:  CanonicalAssistant,
		Parts: []CanonicalPart{CanonicalRefusal{Text: "I can't help with that."}},
	}}
	raw, report, err := RenderGeminiResponse(
		geminiCompletedResponse(refusal, CanonicalStopRefusal, knownGeminiUsage()),
		geminiRenderContext(StrictLossPolicy(), false),
	)
	if err != nil {
		t.Fatalf("refusal render error: %v", err)
	}
	candidate := geminiFirstCandidate(t, geminiRenderedDoc(t, raw))
	if candidate["finishReason"] != "SAFETY" {
		t.Fatalf("refusal finishReason = %v, want SAFETY", candidate["finishReason"])
	}
	if !geminiReportHas(report, FeatureGeminiRefusalText, NoteRecord) || !geminiReportHas(report, FeatureGeminiFinishRefusal, NoteRecord) {
		t.Fatalf("refusal notes missing: %+v", report.Losses)
	}

	text := []CanonicalResponseItem{&CanonicalMessageItem{
		Role:  CanonicalAssistant,
		Parts: []CanonicalPart{CanonicalText{Text: "partial"}},
	}}
	raw, _, err = RenderGeminiResponse(
		geminiCompletedResponse(text, CanonicalStopMaxTokens, knownGeminiUsage()),
		geminiRenderContext(StrictLossPolicy(), false),
	)
	if err != nil {
		t.Fatalf("max tokens render error: %v", err)
	}
	if got := geminiFirstCandidate(t, geminiRenderedDoc(t, raw))["finishReason"]; got != "MAX_TOKENS" {
		t.Fatalf("max tokens finishReason = %v", got)
	}
}

func TestRenderGeminiResponseEmptyContentGetsEmptyText(t *testing.T) {
	raw, report, err := RenderGeminiResponse(
		geminiCompletedResponse(nil, CanonicalStopEndTurn, knownGeminiUsage()),
		geminiRenderContext(StrictLossPolicy(), false),
	)
	if err != nil {
		t.Fatalf("RenderGeminiResponse error: %v", err)
	}
	parts := geminiFirstCandidate(t, geminiRenderedDoc(t, raw))["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "" {
		t.Fatalf("parts = %v, want one empty text part", parts)
	}
	if !geminiReportHas(report, FeatureGeminiEmptyContent, NoteRecord) {
		t.Fatalf("empty content not noted")
	}
}

func TestRenderGeminiResponseUnknownUsage(t *testing.T) {
	items := []CanonicalResponseItem{&CanonicalMessageItem{
		Role:  CanonicalAssistant,
		Parts: []CanonicalPart{CanonicalText{Text: "ok"}},
	}}
	response := geminiCompletedResponse(items, CanonicalStopEndTurn, CanonicalUsage{})

	if _, _, err := RenderGeminiResponse(response, geminiRenderContext(StrictLossPolicy(), false)); err == nil {
		t.Fatalf("strict policy rendered a response with unknown usage")
	}

	raw, report, err := RenderGeminiResponse(response, geminiRenderContext(geminiAllowAll(), false))
	if err != nil {
		t.Fatalf("RenderGeminiResponse error: %v", err)
	}
	if _, present := geminiRenderedDoc(t, raw)["usageMetadata"]; present {
		t.Fatalf("usageMetadata rendered for unknown usage")
	}
	if !geminiReportHas(report, FeatureUsageUnknown, LossRecord) {
		t.Fatalf("unknown usage loss not recorded")
	}
}

func TestRenderGeminiResponseDerivesMissingTotal(t *testing.T) {
	items := []CanonicalResponseItem{&CanonicalMessageItem{
		Role:  CanonicalAssistant,
		Parts: []CanonicalPart{CanonicalText{Text: "ok"}},
	}}
	usage := CanonicalUsage{
		InputTokens: 5, InputKnown: true,
		OutputTokens: 7, OutputKnown: true,
		ReasoningKnown: true,
	}
	raw, report, err := RenderGeminiResponse(
		geminiCompletedResponse(items, CanonicalStopEndTurn, usage),
		geminiRenderContext(geminiAllowAll(), false),
	)
	if err != nil {
		t.Fatalf("RenderGeminiResponse error: %v", err)
	}
	usageDoc := geminiRenderedDoc(t, raw)["usageMetadata"].(map[string]any)
	if usageDoc["totalTokenCount"] != float64(12) {
		t.Fatalf("totalTokenCount = %v, want 12", usageDoc["totalTokenCount"])
	}
	if !geminiReportHas(report, FeatureUsageTotalDerived, NoteRecord) {
		t.Fatalf("derived total not noted")
	}
}

func knownGeminiUsage() CanonicalUsage {
	return CanonicalUsage{
		InputTokens: 3, InputKnown: true,
		OutputTokens: 2, OutputKnown: true,
		ReasoningKnown:  true,
		CacheReadKnown:  true,
		CacheWriteKnown: true,
		TotalTokens:     5, TotalKnown: true,
	}
}
