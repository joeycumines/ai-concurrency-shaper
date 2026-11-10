package transcode

import (
	"errors"
	"strings"
	"testing"
)

// decodeGemini runs DecodeGeminiResponse under the given policy.
func decodeGemini(body string, policy LossPolicy) (CanonicalResponse, ConversionReport, error) {
	return DecodeGeminiResponse([]byte(body), ChatCapabilities{}, policy, NewExchangeIDs(), nil)
}

const geminiTextResponse = `{
	"candidates": [{
		"content": {"role": "model", "parts": [{"text": "hello "}, {"text": "world"}]},
		"finishReason": "STOP",
		"index": 0
	}],
	"usageMetadata": {
		"promptTokenCount": 10,
		"candidatesTokenCount": 5,
		"thoughtsTokenCount": 2,
		"totalTokenCount": 17,
		"cachedContentTokenCount": 3
	},
	"modelVersion": "gemini-2.5-flash",
	"responseId": "resp-1"
}`

func TestDecodeGeminiResponseText(t *testing.T) {
	resp, report, err := decodeGemini(geminiTextResponse, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "resp-1" || resp.Model != "gemini-2.5-flash" {
		t.Fatalf("id/model = %q/%q", resp.ID, resp.Model)
	}
	if resp.Status != CanonicalResponseCompleted || resp.Stop.Reason != CanonicalStopEndTurn {
		t.Fatalf("status/stop = %q/%q", resp.Status, resp.Stop.Reason)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %v", resp.Items)
	}
	msg, ok := resp.Items[0].(*CanonicalMessageItem)
	if !ok {
		t.Fatalf("item type %T", resp.Items[0])
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("parts = %v, want the two text parts concatenated", msg.Parts)
	}
	if text, ok := msg.Parts[0].(CanonicalText); !ok || text.Text != "hello world" {
		t.Fatalf("part = %#v", msg.Parts[0])
	}
	// Usage: input 10, cache read 3, output 5+2, reasoning 2, total 17.
	u := resp.Usage
	if u.InputTokens != 10 || u.CacheReadTokens != 3 || u.OutputTokens != 7 ||
		u.ReasoningTokens != 2 || u.TotalTokens != 17 {
		t.Fatalf("usage = %+v", u)
	}
	if !u.InputKnown || !u.CacheReadKnown || !u.OutputKnown || !u.ReasoningKnown || !u.TotalKnown {
		t.Fatalf("usage known flags = %+v", u)
	}
	if u.CacheWriteKnown {
		t.Fatal("cache write must be unknown for Gemini")
	}
	if len(report.Losses) != 0 {
		t.Fatalf("unexpected losses: %+v", report.Losses)
	}
}

func TestDecodeGeminiResponsePromptBlocked(t *testing.T) {
	body := `{
		"promptFeedback": {"blockReason": "SAFETY", "safetyRatings": [{"category": "HARM_CATEGORY_HARASSMENT", "probability": "HIGH"}]},
		"modelVersion": "m"
	}`
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != CanonicalResponseIncomplete || resp.Stop.Reason != CanonicalStopRefusal ||
		resp.IncompleteReason != "content_filter" {
		t.Fatalf("status/stop/incomplete = %q/%q/%q, want incomplete content_filter refusal", resp.Status, resp.Stop.Reason, resp.IncompleteReason)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %v", resp.Items)
	}
	msg := resp.Items[0].(*CanonicalMessageItem)
	if len(msg.Parts) != 1 {
		t.Fatalf("parts = %v", msg.Parts)
	}
	if _, ok := msg.Parts[0].(CanonicalRefusal); !ok {
		t.Fatalf("part = %T, want refusal", msg.Parts[0])
	}
	if !reportHasFeature(report, FeatureGeminiPromptBlocked) {
		t.Fatalf("report lacks prompt-blocked note: %+v", report.Losses)
	}
	if !reportHasFeature(report, FeatureGeminiSafetyMetadata) {
		t.Fatalf("report lacks safety-metadata loss: %+v", report.Losses)
	}
}

func TestDecodeGeminiResponseCandidateCollapse(t *testing.T) {
	body := `{
		"candidates": [
			{"content": {"role": "model", "parts": [{"text": "b"}]}, "finishReason": "STOP", "index": 1},
			{"content": {"role": "model", "parts": [{"text": "a"}]}, "finishReason": "STOP", "index": 0}
		],
		"modelVersion": "m"
	}`
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasFeature(report, FeatureGeminiCandidateCollapse) {
		t.Fatalf("report lacks candidate-collapse loss: %+v", report.Losses)
	}
	msg := resp.Items[0].(*CanonicalMessageItem)
	if text := msg.Parts[0].(CanonicalText); text.Text != "a" {
		t.Fatalf("kept candidate text = %q, want the lowest index (a)", text.Text)
	}
	// Strict rejects the collapse.
	if _, _, err := decodeGemini(body, StrictLossPolicy()); err == nil {
		t.Fatal("strict policy accepted candidate collapse")
	}
}

func TestDecodeGeminiResponseDuplicateCandidateIndexWireError(t *testing.T) {
	body := `{
		"candidates": [
			{"content": {"role": "model", "parts": [{"text": "a"}]}, "finishReason": "STOP", "index": 0},
			{"content": {"role": "model", "parts": [{"text": "b"}]}, "finishReason": "STOP", "index": 0}
		],
		"modelVersion": "m"
	}`
	_, _, err := decodeGemini(body, permissivePolicy())
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %v (%T), want *UpstreamWireError", err, err)
	}
}

func TestDecodeGeminiResponseFunctionCallAndSignature(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"functionCall": {"id": "call_1", "name": "get_weather", "args": {"city": "Paris"}}, "thoughtSignature": "sig-abc"}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	resp, _, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stop.Reason != CanonicalStopToolUse {
		t.Fatalf("stop = %q, want tool_use", resp.Stop.Reason)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %v", resp.Items)
	}
	call, ok := resp.Items[0].(*CanonicalFunctionCallItem)
	if !ok {
		t.Fatalf("item type %T", resp.Items[0])
	}
	if call.CallID != "call_1" || call.Name != "get_weather" {
		t.Fatalf("call = %+v", call)
	}
	if string(call.Arguments.Object) != `{"city":"Paris"}` || !call.Arguments.IsObject {
		t.Fatalf("args = %+v", call.Arguments)
	}
	if resp.Source.GeminiCallSignatures["call_1"] != "sig-abc" {
		t.Fatalf("signatures = %v", resp.Source.GeminiCallSignatures)
	}
}

func TestDecodeGeminiResponseFunctionCallNoID(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"functionCall": {"name": "f", "args": {}}}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	resp, _, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	call := resp.Items[0].(*CanonicalFunctionCallItem)
	if !strings.HasPrefix(call.CallID, "gcall_") {
		t.Fatalf("call id = %q, want a gcall_ mint", call.CallID)
	}
}

// TestDecodeGeminiResponseDroppedThoughtKeepsTextRun proves a thought part
// dropped under the no-capability arm does not split adjacent text parts:
// [text a, thought b, text c] renders ONE CanonicalText "ac" in one message.
func TestDecodeGeminiResponseDroppedThoughtKeepsTextRun(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "a"},
				{"text": "b", "thought": true},
				{"text": "c"}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasFeature(report, FeatureProviderReasoningText) {
		t.Fatalf("report lacks the dropped-thought loss: %+v", report.Losses)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %v, want one message item", resp.Items)
	}
	msg, ok := resp.Items[0].(*CanonicalMessageItem)
	if !ok {
		t.Fatalf("item type %T", resp.Items[0])
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("parts = %v, want the un-split text run", msg.Parts)
	}
	if text, ok := msg.Parts[0].(CanonicalText); !ok || text.Text != "ac" {
		t.Fatalf("part = %#v, want one CanonicalText \"ac\"", msg.Parts[0])
	}
}

// TestDecodeGeminiResponseFunctionResponseRejected proves a functionResponse
// part in a model response is refused as unrepresentable by the non-streaming
// decode, exactly as the streaming convertPart refuses it.
func TestDecodeGeminiResponseFunctionResponseRejected(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"functionResponse": {"name": "f", "response": {"ok": true}}}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	_, _, err := decodeGemini(body, permissivePolicy())
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("err = %T %v, want *UnrepresentableError", err, err)
	}
	if !strings.Contains(err.Error(), "functionResponse") {
		t.Fatalf("error = %q, want the functionResponse violation", err.Error())
	}
}

// TestDecodeGeminiResponseSynthesizedCallIDNoted proves the non-streaming
// decode records the synthesized-call-id note when a functionCall carries no
// id and a proxy-minted gcall_ id is emitted.
func TestDecodeGeminiResponseSynthesizedCallIDNoted(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"functionCall": {"name": "f", "args": {}}}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	call, ok := resp.Items[0].(*CanonicalFunctionCallItem)
	if !ok || !strings.HasPrefix(call.CallID, "gcall_") {
		t.Fatalf("call = %#v, want a gcall_ mint", resp.Items[0])
	}
	if n := countReportFeature(report, FeatureGeminiCallIDSynthesized); n != 1 {
		t.Fatalf("synthesized-call-id notes = %d, want 1: %+v", n, report.Losses)
	}
}

// TestDecodeGeminiResponseNullArgsMatchStream proves an explicit JSON null
// functionCall.args (which the wire model treats as absent) decodes to an
// empty-object call, exactly as the stream renders it, with the synthesized
// call-id note recorded once — never a refusal that leaves the note behind.
func TestDecodeGeminiResponseNullArgsMatchStream(t *testing.T) {
	part := `{"functionCall": {"name": "f", "args": null}}`
	body := `{"candidates": [{"content": {"role": "model", "parts": [` + part + `]}, "finishReason": "STOP"}], "modelVersion": "m"}`
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("null args: %v (report %+v)", err, report.Losses)
	}
	call, ok := resp.Items[0].(*CanonicalFunctionCallItem)
	if !ok || call.Arguments.Raw != "{}" {
		t.Fatalf("call = %#v, want empty-object arguments", resp.Items[0])
	}
	if n := countReportFeature(report, FeatureGeminiCallIDSynthesized); n != 1 {
		t.Fatalf("call-id notes = %d, want 1: %+v", n, report.Losses)
	}

	ctx := geminiStreamTestContext()
	s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
	events, err := s.Convert(geminiChunk(t, `{"candidates":[{"content":{"parts":[`+part+`]},"index":0}]}`))
	if err != nil {
		t.Fatalf("stream null args: %v", err)
	}
	found := false
	for _, event := range events {
		if done, ok := event.(ResponseFunctionCallArgumentsDoneEvent); ok {
			found = true
			if done.Arguments != "{}" {
				t.Fatalf("stream arguments = %q, want {}", done.Arguments)
			}
		}
	}
	if !found {
		t.Fatalf("stream emitted no arguments done event: %v", geminiStreamEventTypes(events))
	}
}

// TestDecodeGeminiResponseDroppedThoughtSignatureParity pins the
// non-streaming decode against the stream for a dropped thought carrying a
// signature: both the reasoning loss and the signature loss are recorded, and
// a policy refusing the signature refuses the part.
func TestDecodeGeminiResponseDroppedThoughtSignatureParity(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "hmm", "thought": true, "thoughtSignature": "sig"}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	_, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := countReportFeature(report, FeatureProviderReasoningText); n != 1 {
		t.Fatalf("reasoning loss recorded %d times, want 1: %+v", n, report.Losses)
	}
	if n := countReportFeature(report, FeatureGeminiResponseThoughtSignature); n != 1 {
		t.Fatalf("signature loss recorded %d times, want 1: %+v", n, report.Losses)
	}
	_, _, err = decodeGemini(body, LossPolicy{Allowed: map[Feature]struct{}{FeatureProviderReasoningText: {}}})
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureGeminiResponseThoughtSignature) {
		t.Fatalf("refused signature: err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureGeminiResponseThoughtSignature)
	}
}

// TestDecodeGeminiResponseReasoningTextJoinsTextRun pins that reasoning
// mapped to text under provider_reasoning_text joins the surrounding text
// run, as the stream concatenates it into the open text part.
func TestDecodeGeminiResponseReasoningTextJoinsTextRun(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "a"},
				{"text": "b", "thought": true},
				{"text": "c"}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	resp, _, err := DecodeGeminiResponse([]byte(body), ChatCapabilities{ProviderReasoningText: true}, permissivePolicy(), NewExchangeIDs(), nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg, ok := resp.Items[0].(*CanonicalMessageItem)
	if !ok || len(msg.Parts) != 1 {
		t.Fatalf("items = %#v, want one message item with one part", resp.Items)
	}
	if text, ok := msg.Parts[0].(CanonicalText); !ok || text.Text != "abc" {
		t.Fatalf("part = %#v, want one CanonicalText \"abc\"", msg.Parts[0])
	}
}

func TestDecodeGeminiResponseThoughtText(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "thinking", "thought": true},
				{"text": "answer"}
			]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	// No provider-reasoning capability: thought text is an approved loss.
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasFeature(report, FeatureProviderReasoningText) {
		t.Fatalf("report lacks provider_reasoning_text: %+v", report.Losses)
	}
	msg := resp.Items[0].(*CanonicalMessageItem)
	if len(msg.Parts) != 1 || msg.Parts[0].(CanonicalText).Text != "answer" {
		t.Fatalf("parts = %v, want only the answer text", msg.Parts)
	}
	// With the thinking capability: a synthetic thinking block.
	resp, report, err = DecodeGeminiResponse([]byte(body), ChatCapabilities{ProviderReasoningThinking: true}, permissivePolicy(), NewExchangeIDs(), nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg = resp.Items[0].(*CanonicalMessageItem)
	if len(msg.Parts) != 2 {
		t.Fatalf("parts = %v", msg.Parts)
	}
	thinking, ok := msg.Parts[0].(CanonicalThinkingPart)
	if !ok || thinking.Text != "thinking" || thinking.Signature != SyntheticThinkingSignature {
		t.Fatalf("part 0 = %#v", msg.Parts[0])
	}
	if !reportHasFeature(report, FeatureProviderReasoningText) {
		t.Fatalf("report lacks the note: %+v", report.Losses)
	}
}

func TestDecodeGeminiResponseRefusalFinishReason(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "partial"}]},
			"finishReason": "SAFETY",
			"finishMessage": "blocked by safety"
		}],
		"modelVersion": "m"
	}`
	resp, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != CanonicalResponseIncomplete || resp.Stop.Reason != CanonicalStopRefusal ||
		resp.IncompleteReason != "content_filter" {
		t.Fatalf("status/stop/incomplete = %q/%q/%q", resp.Status, resp.Stop.Reason, resp.IncompleteReason)
	}
	msg := resp.Items[0].(*CanonicalMessageItem)
	if len(msg.Parts) != 2 {
		t.Fatalf("parts = %v, want text + refusal appended", msg.Parts)
	}
	if r, ok := msg.Parts[1].(CanonicalRefusal); !ok || r.Text != "blocked by safety" {
		t.Fatalf("part 1 = %#v", msg.Parts[1])
	}
	if !reportHasFeature(report, FeatureGeminiFinishRefusal) {
		t.Fatalf("report lacks finish-refusal note: %+v", report.Losses)
	}
}

func TestDecodeGeminiResponseMaxTokens(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "..."}]},
			"finishReason": "MAX_TOKENS"
		}],
		"modelVersion": "m"
	}`
	resp, _, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != CanonicalResponseIncomplete || resp.Stop.Reason != CanonicalStopMaxTokens ||
		resp.IncompleteReason != "max_output_tokens" {
		t.Fatalf("status/stop/incomplete = %q/%q/%q", resp.Status, resp.Stop.Reason, resp.IncompleteReason)
	}
}

func TestDecodeGeminiResponseUnknownFinishReasonUnrepresentable(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "x"}]},
			"finishReason": "MALFORMED_FUNCTION_CALL"
		}],
		"modelVersion": "m"
	}`
	_, _, err := decodeGemini(body, permissivePolicy())
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("err = %v (%T), want *UnrepresentableError", err, err)
	}
}

func TestDecodeGeminiResponseNoFinishReasonWireError(t *testing.T) {
	body := `{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "x"}]}}],
		"modelVersion": "m"
	}`
	_, _, err := decodeGemini(body, permissivePolicy())
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %v (%T), want *UpstreamWireError", err, err)
	}
}

func TestDecodeGeminiResponseUnrepresentablePart(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"inlineData": {"mimeType": "image/png", "data": "aGk="}}]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	_, _, err := decodeGemini(body, permissivePolicy())
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("err = %v (%T), want *UnrepresentableError", err, err)
	}
}

func TestDecodeGeminiResponseSignatureOnTextPartLoss(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "hi", "thoughtSignature": "sig"}]},
			"finishReason": "STOP"
		}],
		"modelVersion": "m"
	}`
	_, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasFeature(report, FeatureGeminiResponseThoughtSignature) {
		t.Fatalf("report lacks response thought-signature loss: %+v", report.Losses)
	}
	if _, _, err := decodeGemini(body, StrictLossPolicy()); err == nil {
		t.Fatal("strict policy accepted a signature on a text part")
	}
}

func TestDecodeGeminiResponseUsageBreakdownAndServiceTierLosses(t *testing.T) {
	body := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "hi"}]},
			"finishReason": "STOP",
			"tokenCount": 5,
			"safetyRatings": [{"category": "X", "probability": "LOW"}],
			"citationMetadata": {"citations": []}
		}],
		"usageMetadata": {
			"promptTokenCount": 3,
			"candidatesTokenCount": 2,
			"totalTokenCount": 5,
			"promptTokensDetails": [{"modality": "TEXT", "tokenCount": 3}],
			"serviceTier": "PRIORITY"
		},
		"modelStatus": {"lifecycleStage": "ACTIVE"},
		"modelVersion": "m"
	}`
	_, report, err := decodeGemini(body, permissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, feature := range []Feature{
		FeatureGeminiUsageBreakdown,
		FeatureGeminiSafetyMetadata,
		FeatureGeminiGroundingMetadata,
		FeatureResponseServiceTier,
		FeatureGeminiModelStatus,
	} {
		if !reportHasFeature(report, feature) {
			t.Fatalf("report lacks %q: %+v", feature, report.Losses)
		}
	}
}

func TestDecodeGeminiResponseCorruptWire(t *testing.T) {
	_, _, err := decodeGemini(`{"candidates": [`, permissivePolicy())
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %v (%T), want *UpstreamWireError", err, err)
	}
}

func TestDecodeGeminiResponseBlockReasonWithCandidateIsCorrupt(t *testing.T) {
	// The stream path rejects a chunk carrying both promptFeedback.blockReason
	// and a candidate as corrupt wire; the non-streaming decode must too,
	// otherwise the finish-reason switch overwrites the blocked-prompt
	// incomplete status while IncompleteReason stays content_filter.
	for _, finish := range []string{"STOP", "SAFETY", "MAX_TOKENS"} {
		body := `{
			"promptFeedback": {"blockReason": "SAFETY"},
			"candidates": [{"content": {"role": "model", "parts": [{"text": "hi"}]}, "finishReason": "` + finish + `", "index": 0}],
			"modelVersion": "m"
		}`
		_, _, err := decodeGemini(body, permissivePolicy())
		if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
			t.Fatalf("finishReason %s: err = %v (%T), want *UpstreamWireError", finish, err, err)
		}
	}
}

func TestDecodeGeminiResponseEmptyThoughtPartRecordsNothing(t *testing.T) {
	// An empty thought part renders nothing, so it records no reasoning note,
	// matching the streaming empty-delta short circuit.
	response, report, err := DecodeGeminiResponse(
		[]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"","thought":true}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3-pro-preview"}`),
		ChatCapabilities{ProviderReasoningThinking: true},
		StrictLossPolicy(),
		NewExchangeIDs(), nil,
	)
	if err != nil {
		t.Fatalf("empty thought part: %v", err)
	}
	for _, item := range response.Items {
		if message, ok := item.(*CanonicalMessageItem); ok && len(message.Parts) != 0 {
			t.Fatalf("empty thought part produced %d parts", len(message.Parts))
		}
	}
	if reportHasFeature(report, FeatureProviderReasoningText) {
		t.Fatalf("empty thought part recorded a reasoning entry: %+v", report.Losses)
	}

	// A signature attached to the empty part is still an observable loss.
	_, _, err = DecodeGeminiResponse(
		[]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"","thought":true,"thoughtSignature":"sig"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3-pro-preview"}`),
		ChatCapabilities{ProviderReasoningThinking: true},
		StrictLossPolicy(),
		NewExchangeIDs(), nil,
	)
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureGeminiResponseThoughtSignature) {
		t.Fatalf("empty thought part signature: err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureGeminiResponseThoughtSignature)
	}
}
