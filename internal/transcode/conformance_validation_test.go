package transcode

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// TestIsJSONApplicationTreeRestriction proves the structured-syntax suffix
// "+json" matches ONLY within the application tree: text/example+json is NOT
// JSON per RFC 6839 (review-08 additional 7).
func TestIsJSONApplicationTreeRestriction(t *testing.T) {
	tests := []struct {
		contentType string
		want        bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/vnd.api+json", true},
		{"application/problem+json", true},
		{"text/example+json", false},
		{"text/json", false},
		{"application/notjson", false},
		{"application/xml", false},
		{"", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			resp.Header.Set("Content-Type", tt.contentType)
			if got := isJSON(resp); got != tt.want {
				t.Fatalf("isJSON(%q) = %v, want %v", tt.contentType, got, tt.want)
			}
		})
	}
}

// TestValidateCanonicalRequestNegativeMatrix exercises the new IR validation
// invariants: role, part presence, tool-call identity, arguments shape
// (review-08 additional 10).
func TestValidateCanonicalRequestNegativeMatrix(t *testing.T) {
	validCall := CanonicalFunctionCall{
		CallID:    "fc_1",
		Name:      "lookup",
		Arguments: []byte(`{"q":"hello"}`),
	}

	t.Run("invalid role", func(t *testing.T) {
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{{
				Role:  CanonicalRole("robot"),
				Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
			}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("invalid role accepted")
		}
	})

	t.Run("empty turn", func(t *testing.T) {
		req := CanonicalRequest{
			ClientModel: "m",
			Turns:       []CanonicalTurn{{Role: CanonicalUser}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("empty turn accepted")
		}
	})

	t.Run("nil part", func(t *testing.T) {
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{{
				Role:  CanonicalUser,
				Parts: []CanonicalPart{nil},
			}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("nil part accepted")
		}
	})

	t.Run("function call empty call id", func(t *testing.T) {
		call := validCall
		call.CallID = ""
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{{
				Role:  CanonicalAssistant,
				Parts: []CanonicalPart{call},
			}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("empty call id accepted")
		}
	})

	t.Run("function call empty name", func(t *testing.T) {
		call := validCall
		call.Name = ""
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{{
				Role:  CanonicalAssistant,
				Parts: []CanonicalPart{call},
			}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("empty name accepted")
		}
	})

	t.Run("function call non-object arguments", func(t *testing.T) {
		call := validCall
		call.Arguments = []byte(`[1,2,3]`)
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{{
				Role:  CanonicalAssistant,
				Parts: []CanonicalPart{call},
			}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("non-object arguments accepted")
		}
	})

	t.Run("function result empty call id", func(t *testing.T) {
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{{
				Role: CanonicalUser,
				Parts: []CanonicalPart{CanonicalFunctionResult{
					CallID: "",
					Parts:  []CanonicalPart{CanonicalText{Text: "ok"}},
				}},
			}},
		}
		if err := ValidateCanonicalRequest(req); err == nil {
			t.Fatal("function result empty call id accepted")
		}
	})

	t.Run("valid request", func(t *testing.T) {
		req := CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{
				{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
				{Role: CanonicalAssistant, Parts: []CanonicalPart{validCall}},
				{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalFunctionResult{
					CallID: "fc_1",
					Parts:  []CanonicalPart{CanonicalText{Text: "ok"}},
				}}},
			},
		}
		if err := ValidateCanonicalRequest(req); err != nil {
			t.Fatalf("valid request rejected: %v", err)
		}
	})
}

// TestValidateCanonicalResponseNegativeMatrix exercises the response-side IR
// validation: model, status, stop reason, turn structure, usage arithmetic
// (review-08 additional 10).
func TestValidateCanonicalResponseNegativeMatrix(t *testing.T) {
	base := func() CanonicalResponse {
		return CanonicalResponse{
			ID:     "resp_1",
			Model:  "m",
			Status: CanonicalResponseCompleted,
			Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
			Items: []CanonicalResponseItem{&CanonicalMessageItem{
				Role:  CanonicalAssistant,
				Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
			}},
			Usage: CanonicalUsage{
				InputTokens: 5, InputKnown: true,
				OutputTokens: 2, OutputKnown: true,
				TotalTokens: 7, TotalKnown: true,
			},
		}
	}

	t.Run("empty model", func(t *testing.T) {
		r := base()
		r.Model = ""
		if err := ValidateCanonicalResponse(r); err == nil {
			t.Fatal("empty model accepted")
		}
	})

	t.Run("invalid status", func(t *testing.T) {
		r := base()
		r.Status = CanonicalResponseStatus("pending")
		if err := ValidateCanonicalResponse(r); err == nil {
			t.Fatal("invalid status accepted")
		}
	})

	t.Run("empty status", func(t *testing.T) {
		r := base()
		r.Status = ""
		if err := ValidateCanonicalResponse(r); err == nil {
			t.Fatal("empty status accepted")
		}
	})

	t.Run("invalid stop reason", func(t *testing.T) {
		r := base()
		r.Stop.Reason = CanonicalStopReason("because")
		if err := ValidateCanonicalResponse(r); err == nil {
			t.Fatal("invalid stop reason accepted")
		}
	})

	t.Run("negative usage", func(t *testing.T) {
		r := base()
		r.Usage.OutputTokens = -1
		if err := ValidateCanonicalResponse(r); err == nil {
			t.Fatal("negative usage accepted")
		}
	})

	t.Run("inconsistent total (overflow)", func(t *testing.T) {
		r := base()
		r.Usage.TotalTokens = 3 // 5+2 > 3
		if err := ValidateCanonicalResponse(r); err == nil {
			t.Fatal("inconsistent total accepted")
		}
	})

	t.Run("valid response", func(t *testing.T) {
		if err := ValidateCanonicalResponse(base()); err != nil {
			t.Fatalf("valid response rejected: %v", err)
		}
	})
}

// TestUsageStreamingNegativeRejection proves negative and inconsistent token
// counts are rejected at the usage conversion boundaries (review-08
// additional 10/11).
func TestUsageStreamingNegativeRejection(t *testing.T) {
	t.Run("chat negative output", func(t *testing.T) {
		_, err := chatUsageToResponsesUsage(&ChatLLMUsage{
			PromptTokens:     10,
			CompletionTokens: -1,
			TotalTokens:      9,
		})
		if err == nil {
			t.Fatal("negative output accepted")
		}
	})

	t.Run("chat negative reasoning detail", func(t *testing.T) {
		_, err := chatUsageToResponsesUsage(&ChatLLMUsage{
			PromptTokens:     10,
			CompletionTokens: 2,
			TotalTokens:      12,
			CompletionTokensDetails: &ChatCompletionTokensDetails{
				ReasoningTokens: -1,
			},
		})
		if err == nil {
			t.Fatal("negative reasoning accepted")
		}
	})

	t.Run("chat inconsistent total", func(t *testing.T) {
		_, err := chatUsageToResponsesUsage(&ChatLLMUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      12, // 10+5 > 12 is false; 10+5=15 > 12 is true
		})
		if err == nil {
			t.Fatal("inconsistent total accepted")
		}
	})

	t.Run("chat nil usage returns nil", func(t *testing.T) {
		got, err := chatUsageToResponsesUsage(nil)
		if err != nil || got != nil {
			t.Fatalf("nil usage = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("responses negative output", func(t *testing.T) {
		_, err := responsesUsageToAnthropicUsage(&ResponsesUsage{
			InputTokens:  10,
			OutputTokens: -1,
		})
		if err == nil {
			t.Fatal("negative output accepted")
		}
	})

	t.Run("responses negative reasoning detail", func(t *testing.T) {
		_, err := responsesUsageToAnthropicUsage(&ResponsesUsage{
			InputTokens:  10,
			OutputTokens: 2,
			OutputTokensDetails: &UsageOutputTokensDetails{
				ReasoningTokens: -1,
			},
		})
		if err == nil {
			t.Fatal("negative reasoning accepted")
		}
	})
}

// TestSSEReadLineCROnly proves readSSELine handles CR-only line endings
// (legal per the HTML SSE spec), not just LF and CRLF (review-08 additional
// 12).
func TestSSEReadLineCROnly(t *testing.T) {
	input := "event: message_stop\rdata: {\"type\":\"message_stop\"}\r\r"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "message_stop" {
		t.Fatalf("events = %+v", events)
	}
}

// TestResponsesStreamEmptyEventNameRejected proves a Responses stream frame
// without an event name is rejected by the Responses→Anthropic adapter: the
// package's own rule requires event: to be present and equal the JSON type
// tag (review-08 additional 12).
func TestResponsesStreamEmptyEventNameRejected(t *testing.T) {
	ctx := testStreamContext()
	state := newAnthropicResponsesStreamState(ctx, StrictLossPolicy(), "resp_1", "m", 1)
	converter := &responsesToAnthropicConverter{state: state}

	// Empty event name + valid data → rejected by the adapter guard.
	_, err := converter.Convert(SSEEvent{
		Event: "",
		Data:  []byte(`{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"in_progress"}}`),
	})
	if err == nil {
		t.Fatal("empty event name accepted by the Responses adapter")
	}
}

// TestSSEEmptyDataFrameHandledPerSpec proves a frame with an event name but
// no data line is handled per spec: the data is empty, and the frame does not
// produce a spurious event (review-08 additional 12).
func TestSSEEmptyDataFrameHandledPerSpec(t *testing.T) {
	// "event: ping\n\n" — event name but no data. Per the existing spec
	// behavior, this is discarded (no data payload).
	input := "event: ping\n\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("empty-data frame produced events = %d", len(events))
	}
}

// TestStreamOutcomeReflectsClassification proves Outcome.StreamOutcome is
// truthfully assigned from classifyStreamObservation for every streaming
// classification bucket (review-08 additional 8).
func TestStreamOutcomeReflectsClassification(t *testing.T) {
	mapping := responsesMapping(t)

	t.Run("success", func(t *testing.T) {
		var captured Outcome
		handler := NewTranscodeHandler(
			HandlerConfig{
				Mapping:  mapping,
				Upstream: mustParseURL(t, "https://upstream.example"),
				BodyLimits: BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					SuccessfulResponseBytes: 1 << 20,
				},
				ModelMap:           ModelMap{AllowIdentity: true},
				LossPolicy:         StrictLossPolicy(),
				AuthPolicy:         AuthPolicy{Mode: AuthNone},
				ChatCapabilities:   ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
				AllowedClientQuery: map[string]struct{}{},
			},
			func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(bytes.NewReader(
						testcorpus.ChatCompletionsStreamSSE(),
					)),
				}, nil
			},
			func(o Outcome) { captured = o },
		)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m","input":"x","stream":true}`))
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if captured.StreamOutcome != streamOutcomeSuccess {
			t.Fatalf("StreamOutcome = %s, want %s",
				captured.StreamOutcome, streamOutcomeSuccess)
		}
	})

	t.Run("upstream failure", func(t *testing.T) {
		var captured Outcome
		handler := NewTranscodeHandler(
			HandlerConfig{
				Mapping:  mapping,
				Upstream: mustParseURL(t, "https://upstream.example"),
				BodyLimits: BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					SuccessfulResponseBytes: 1 << 20,
				},
				ModelMap:           ModelMap{AllowIdentity: true},
				LossPolicy:         StrictLossPolicy(),
				AuthPolicy:         AuthPolicy{Mode: AuthNone},
				ChatCapabilities:   ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
				AllowedClientQuery: map[string]struct{}{},
			},
			func(req *http.Request) (*http.Response, error) {
				// A malformed SSE stream that truncates before a terminal.
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(
						"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n",
					)),
				}, nil
			},
			func(o Outcome) { captured = o },
		)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m","input":"x","stream":true}`))
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if captured.StreamOutcome != streamOutcomeUpstreamFailure {
			t.Fatalf("StreamOutcome = %s, want %s",
				captured.StreamOutcome, streamOutcomeUpstreamFailure)
		}
	})

	t.Run("client abort", func(t *testing.T) {
		var captured Outcome
		handler := NewTranscodeHandler(
			HandlerConfig{
				Mapping:  mapping,
				Upstream: mustParseURL(t, "https://upstream.example"),
				BodyLimits: BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					SuccessfulResponseBytes: 1 << 20,
				},
				ModelMap:           ModelMap{AllowIdentity: true},
				LossPolicy:         StrictLossPolicy(),
				AuthPolicy:         AuthPolicy{Mode: AuthNone},
				ChatCapabilities:   ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
				AllowedClientQuery: map[string]struct{}{},
			},
			func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(
						"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n",
					)),
				}, nil
			},
			func(o Outcome) { captured = o },
		)
		ctx, cancel := contextWithCancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m","input":"x","stream":true}`)).WithContext(ctx)
		cancel()
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if captured.StreamOutcome != streamOutcomeClientAbort {
			t.Fatalf("StreamOutcome = %s, want %s",
				captured.StreamOutcome, streamOutcomeClientAbort)
		}
	})
}
