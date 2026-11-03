package geminigeneratecontent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

const fullResponseJSON = `{
	"candidates": [
		{
			"content": {"role": "model", "parts": [{"text": "hello", "thought": true, "thoughtSignature": "c2ln"}]},
			"finishReason": "STOP",
			"finishMessage": "done",
			"index": 0,
			"safetyRatings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "NEGLIGIBLE", "blocked": false}],
			"citationMetadata": {"citationSources": []},
			"groundingMetadata": {"webSearchQueries": []},
			"groundingAttributions": [],
			"urlContextMetadata": {"urlMetadata": []},
			"logprobsResult": {"topCandidates": []},
			"avgLogprobs": -0.1,
			"tokenCount": 3,
			"continuationToken": "next"
		}
	],
	"promptFeedback": {"blockReason": "SAFETY", "safetyRatings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "LOW", "blocked": true}]},
	"usageMetadata": {
		"promptTokenCount": 1,
		"cachedContentTokenCount": 2,
		"candidatesTokenCount": 3,
		"toolUsePromptTokenCount": 4,
		"thoughtsTokenCount": 5,
		"totalTokenCount": 6,
		"promptTokensDetails": [{"modality": "TEXT", "tokenCount": 1}],
		"cacheTokensDetails": [{"modality": "IMAGE", "tokenCount": 2}],
		"candidatesTokensDetails": [{"modality": "AUDIO", "tokenCount": 3}],
		"toolUsePromptTokensDetails": [{"modality": "DOCUMENT", "tokenCount": 4}],
		"serviceTier": "standard"
	},
	"modelVersion": "gemini-2.5-pro",
	"responseId": "resp-1",
	"modelStatus": {"modelStage": "STABLE"},
	"error": {"code": 500, "message": "x", "status": "INTERNAL", "details": [{"@type": "type.googleapis.com/x"}]}
}`

func TestDecodeResponseFullBody(t *testing.T) {
	response, err := DecodeResponse([]byte(fullResponseJSON))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(response.Candidates) != 1 {
		t.Fatalf("candidates = %+v", response.Candidates)
	}
	candidate := response.Candidates[0]
	if candidate.Content == nil || candidate.Content.Role != "model" || len(candidate.Content.Parts) != 1 {
		t.Fatalf("content = %+v", candidate.Content)
	}
	part := candidate.Content.Parts[0]
	if part.Text == nil || *part.Text != "hello" || part.Thought == nil || !*part.Thought || part.ThoughtSignature == nil || *part.ThoughtSignature != "c2ln" {
		t.Fatalf("part = %+v", part)
	}
	if candidate.FinishReason == nil || *candidate.FinishReason != "STOP" || candidate.FinishMessage == nil || *candidate.FinishMessage != "done" {
		t.Fatalf("finish = %+v", candidate)
	}
	if candidate.Index == nil || *candidate.Index != 0 || candidate.AvgLogprobs == nil || *candidate.AvgLogprobs != -0.1 || candidate.TokenCount == nil || *candidate.TokenCount != 3 {
		t.Fatalf("candidate numerics = %+v", candidate)
	}
	if len(candidate.SafetyRatings) == 0 || len(candidate.CitationMetadata) == 0 || len(candidate.GroundingMetadata) == 0 || len(candidate.GroundingAttributions) == 0 || len(candidate.URLContextMetadata) == 0 || len(candidate.LogprobsResult) == 0 {
		t.Fatalf("candidate raw members = %+v", candidate)
	}
	if candidate.ContinuationToken == nil || *candidate.ContinuationToken != "next" {
		t.Fatalf("continuationToken = %+v", candidate.ContinuationToken)
	}
	if response.PromptFeedback == nil || response.PromptFeedback.BlockReason == nil || *response.PromptFeedback.BlockReason != "SAFETY" || len(response.PromptFeedback.SafetyRatings) == 0 {
		t.Fatalf("promptFeedback = %+v", response.PromptFeedback)
	}
	usage := response.UsageMetadata
	if usage == nil {
		t.Fatal("usageMetadata missing")
	}
	if usage.PromptTokenCount == nil || *usage.PromptTokenCount != 1 || usage.CachedContentTokenCount == nil || *usage.CachedContentTokenCount != 2 {
		t.Fatalf("usage prompt counts = %+v", usage)
	}
	if usage.CandidatesTokenCount == nil || *usage.CandidatesTokenCount != 3 || usage.ToolUsePromptTokenCount == nil || *usage.ToolUsePromptTokenCount != 4 {
		t.Fatalf("usage candidate counts = %+v", usage)
	}
	if usage.ThoughtsTokenCount == nil || *usage.ThoughtsTokenCount != 5 || usage.TotalTokenCount == nil || *usage.TotalTokenCount != 6 {
		t.Fatalf("usage totals = %+v", usage)
	}
	if len(usage.PromptTokensDetails) == 0 || len(usage.CacheTokensDetails) == 0 || len(usage.CandidatesTokensDetails) == 0 || len(usage.ToolUsePromptTokensDetails) == 0 {
		t.Fatalf("usage details = %+v", usage)
	}
	if usage.ServiceTier == nil || *usage.ServiceTier != "standard" {
		t.Fatalf("serviceTier = %+v", usage.ServiceTier)
	}
	if response.ModelVersion != "gemini-2.5-pro" || response.ResponseID != "resp-1" || response.ModelStatus == nil || response.ModelStatus.ModelStage != "STABLE" {
		t.Fatalf("envelope fields = %+v", response)
	}
	if response.Error == nil || response.Error.Code != 500 || response.Error.Message != "x" || response.Error.Status != "INTERNAL" || len(response.Error.Details) == 0 {
		t.Fatalf("error = %+v", response.Error)
	}
}

func TestDecodeResponseToleratesUnknownEnvelopeMembers(t *testing.T) {
	response, err := DecodeResponse([]byte(`{
		"futureEnvelopeMember": {"x": 1},
		"candidates": [{"futureCandidateMember": true, "content": {"role": "model", "parts": [{"text": "ok"}]}}]
	}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(response.Candidates) != 1 || response.Candidates[0].Content == nil || len(response.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("response = %+v", response)
	}
}

func TestDecodeResponseToleratesUnknownTypedMembers(t *testing.T) {
	response, err := DecodeResponse([]byte(`{
		"candidates": [{"safetyRatings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "futureRatingMember": 1}]}],
		"usageMetadata": {"promptTokensDetails": [{"modality": "TEXT", "tokenCount": 2, "futureMember": true}]},
		"modelStatus": {"modelStage": "STABLE", "futureStatusMember": "x"}
	}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	ratings := response.Candidates[0].SafetyRatings
	if len(ratings) != 1 || ratings[0].Category != "HARM_CATEGORY_HATE_SPEECH" {
		t.Fatalf("safety ratings = %+v", ratings)
	}
	details := response.UsageMetadata.PromptTokensDetails
	if len(details) != 1 || details[0].Modality != "TEXT" || details[0].TokenCount == nil || *details[0].TokenCount != 2 {
		t.Fatalf("usage details = %+v", details)
	}
	if response.ModelStatus == nil || response.ModelStatus.ModelStage != "STABLE" {
		t.Fatalf("model status = %+v", response.ModelStatus)
	}
}

func TestDecodeResponseRejectsStrictViolations(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantKind wire.DecodeErrorKind
	}{
		{
			name:     "unknown part member",
			body:     `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok","bogus":1}]}}]}`,
			wantKind: wire.DecodeUnknownField,
		},
		{
			name:     "type corrupt modeled field",
			body:     `{"usageMetadata":{"totalTokenCount":"two"}}`,
			wantKind: wire.DecodeMalformed,
		},
		{
			name:     "type corrupt safety rating",
			body:     `{"candidates":[{"safetyRatings":[{"blocked":"yes"}]}]}`,
			wantKind: wire.DecodeMalformed,
		},
		{
			name:     "duplicate key",
			body:     `{"candidates":[],"candidates":[]}`,
			wantKind: wire.DecodeDuplicateKey,
		},
		{
			name:     "trailing value",
			body:     `{"candidates":[]}{}`,
			wantKind: wire.DecodeTrailingValue,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeResponse([]byte(test.body))
			requireDecodeErrorKind(t, err, test.wantKind)
		})
	}
}

func TestDecodeResponseStreamErrorFrame(t *testing.T) {
	response, err := DecodeResponse([]byte(`{"error":{"code":500,"message":"x","status":"INTERNAL"}}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if response.Error == nil || response.Error.Code != 500 || response.Error.Message != "x" || response.Error.Status != "INTERNAL" {
		t.Fatalf("error = %+v", response.Error)
	}
}

func TestDecodeResponseRoundTripKeepsEverySetField(t *testing.T) {
	response, err := DecodeResponse([]byte(fullResponseJSON))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(fullResponseJSON), &want); err != nil {
		t.Fatalf("decode want failed: %v", err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode round trip failed: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip changed the document:\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestDecodeErrorEnvelopeRejectsBodiesWithoutErrorObject(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantKind wire.DecodeErrorKind
	}{
		{name: "html page", body: `<html><body>503 Service Unavailable</body></html>`, wantKind: wire.DecodeMalformed},
		{name: "empty body", body: ``, wantKind: wire.DecodeMalformed},
		{name: "json array", body: `[{"error":{"code":500}}]`, wantKind: wire.DecodeMalformed},
		{name: "object without error", body: `{}`, wantKind: wire.DecodeMissingRequired},
		{name: "null error", body: `{"error":null}`, wantKind: wire.DecodeMissingRequired},
		{name: "type corrupt code", body: `{"error":{"code":"x","status":"INTERNAL"}}`, wantKind: wire.DecodeMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeErrorEnvelope([]byte(test.body))
			requireDecodeErrorKind(t, err, test.wantKind)
		})
	}
}

func TestDecodeErrorEnvelope(t *testing.T) {
	envelope, err := DecodeErrorEnvelope([]byte(`{
		"error": {"code": 429, "message": "quota", "status": "RESOURCE_EXHAUSTED", "details": [{"@type": "type.googleapis.com/x"}]},
		"futureEnvelopeMember": true
	}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if envelope.Error.Code != 429 || envelope.Error.Message != "quota" || envelope.Error.Status != "RESOURCE_EXHAUSTED" || len(envelope.Error.Details) == 0 {
		t.Fatalf("envelope = %+v", envelope.Error)
	}

	missingMessage, err := DecodeErrorEnvelope([]byte(`{"error":{"code":503,"status":"UNAVAILABLE"}}`))
	if err != nil {
		t.Fatalf("decode without message failed: %v", err)
	}
	if missingMessage.Error.Code != 503 || missingMessage.Error.Message != "" || missingMessage.Error.Status != "UNAVAILABLE" {
		t.Fatalf("envelope without message = %+v", missingMessage.Error)
	}
}
