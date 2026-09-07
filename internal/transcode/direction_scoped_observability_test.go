package transcode

// Autopsy 2026-09-06 M4: direction-scoped loss observability.
//
// (1) A Responses upstream response's service_tier was silently dropped on
// the Responses→Messages render while the chat source's tier entered the
// loss/reject decision — the same fact must be observable from both
// sources.
//
// (2) A Chat→Responses source that provides no usage at all omitted the
// usage object silently; the omission is now recorded as a Note so the
// usage provenance of every exchange is observable.

import (
	"testing"
)

func TestResponsesSourceServiceTierObservedOnMessagesRender(t *testing.T) {
	response, err := DecodeResponsesResponse([]byte(
		`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","service_tier":"flex","output":[],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if response.Source.ResponsesServiceTier != "flex" {
		t.Fatalf("source tier = %q, want flex", response.Source.ResponsesServiceTier)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureResponseServiceTier:    {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	_, report, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if !reportHasFeature(report, FeatureResponseServiceTier) {
		t.Fatalf("report lacks the service-tier loss (autopsy M4): %+v", report)
	}
}

func TestResponsesSourceServiceTierRejectedUnderStrictPolicy(t *testing.T) {
	response, err := DecodeResponsesResponse([]byte(
		`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","service_tier":"flex","output":[],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = RenderMessagesResponse(response, testExchangeContext())
	if err == nil {
		t.Fatal("strict policy must reject the service-tier drop")
	}
}

func TestChatSourceWithoutUsageRecordsOmissionNoteOnResponsesRender(t *testing.T) {
	response, _, err := DecodeChatResponseWithPolicy([]byte(
		`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
	), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !response.Usage.Unknown() {
		t.Fatal("fixture usage must be fully unknown")
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	_, report, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatalf("a usage-less source must render (the omission is a Note, not a policy-gated loss): %v", err)
	}
	if !reportHasFeature(report, FeatureUsageUnknown) {
		t.Fatalf("report lacks the usage-omission note (autopsy M4): %+v", report)
	}
	for _, entry := range report.Losses {
		if entry.Feature == FeatureUsageUnknown && entry.Kind != NoteRecord {
			t.Fatalf("usage-omission entry kind = %v, want NoteRecord", entry.Kind)
		}
	}
}
