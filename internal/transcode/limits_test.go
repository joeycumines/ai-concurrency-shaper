package transcode

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBodyLimitsValidate proves negative limits fail startup validation
// (review-j finding 14).
func TestBodyLimitsValidate(t *testing.T) {
	if err := (BodyLimits{}).Validate(); err != nil {
		t.Fatalf("zero limits must be valid: %v", err)
	}
	for name, limits := range map[string]BodyLimits{
		"accepted":            {AcceptedRequestBytes: -1},
		"decoded":             {DecodedRequestBytes: -1},
		"retry replay":        {RetryReplayBytes: -1},
		"successful":          {SuccessfulResponseBytes: -1},
		"error":               {ErrorResponseBytes: -1},
		"sse line":            {SSELineBytes: -1},
		"sse frame":           {SSEFrameBytes: -1},
		"generated response":  {GeneratedResponseBytes: -1},
		"error message":       {ErrorMessageBytes: -1},
		"generated sse frame": {GeneratedSSEFrameBytes: -1},
		"generated sse batch": {GeneratedSSEBatchBytes: -1},
	} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("%s: negative limit accepted", name)
		}
	}
}

// TestBodyLimitsWithDefaults proves every zero field selects its package
// default and explicit values are preserved (review-k finding 8): an
// all-zero BodyLimits enforces real bounds on every field, never unlimited.
func TestBodyLimitsWithDefaults(t *testing.T) {
	effective := (BodyLimits{}).WithDefaults()
	want := BodyLimits{
		AcceptedRequestBytes:    DefaultAcceptedRequestBytes,
		DecodedRequestBytes:     DefaultDecodedRequestBytes,
		RetryReplayBytes:        DefaultRetryReplayBytes,
		SuccessfulResponseBytes: DefaultSuccessfulResponseBytes,
		ErrorResponseBytes:      DefaultErrorResponseBytes,
		SSELineBytes:            DefaultSSELineBytes,
		SSEFrameBytes:           DefaultSSEFrameBytes,
		GeneratedResponseBytes:  DefaultGeneratedResponseBytes,
		ErrorMessageBytes:       DefaultErrorMessageBytes,
		GeneratedSSEFrameBytes:  DefaultGeneratedSSEFrameBytes,
		GeneratedSSEBatchBytes:  DefaultGeneratedSSEBatchBytes,
	}
	if effective != want {
		t.Fatalf("effective = %+v, want %+v", effective, want)
	}

	explicit := BodyLimits{
		AcceptedRequestBytes:    1,
		DecodedRequestBytes:     2,
		RetryReplayBytes:        3,
		SuccessfulResponseBytes: 4,
		ErrorResponseBytes:      5,
		SSELineBytes:            6,
		SSEFrameBytes:           7,
	}.WithDefaults()
	if explicit != (BodyLimits{
		AcceptedRequestBytes:    1,
		DecodedRequestBytes:     2,
		RetryReplayBytes:        3,
		SuccessfulResponseBytes: 4,
		ErrorResponseBytes:      5,
		SSELineBytes:            6,
		SSEFrameBytes:           7,
		GeneratedResponseBytes:  DefaultGeneratedResponseBytes,
		ErrorMessageBytes:       DefaultErrorMessageBytes,
		GeneratedSSEFrameBytes:  DefaultGeneratedSSEFrameBytes,
		GeneratedSSEBatchBytes:  DefaultGeneratedSSEBatchBytes,
	}) {
		t.Fatalf("explicit values changed: %+v", explicit)
	}

	// Per-field defaulting: only the zero fields are replaced.
	partial := (BodyLimits{DecodedRequestBytes: 42}).WithDefaults()
	if partial.DecodedRequestBytes != 42 ||
		partial.AcceptedRequestBytes != DefaultAcceptedRequestBytes {
		t.Fatalf("partial = %+v", partial)
	}
}

// TestRenderedResponseBoundEndToEnd proves the complete rendered JSON
// response is bounded AFTER conversion and BEFORE any header commit: an
// oversized render fails the exchange with the configured limit and an
// UpstreamBodyError outcome, never a partial response (review-z commit 5,
// review-z commit 3).
func TestRenderedResponseBoundEndToEnd(t *testing.T) {
	roundTrip := func(req *http.Request) (*http.Response, error) {
		// The upstream response MUST carry the JSON content type: without it
		// the handler rejects the representation earlier and the rendered
		// bound check never runs — this test pins the bound mechanism
		// itself (review-z commit 5).
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"` + strings.Repeat("x", 4096) + `"}}]}`)),
		}, nil
	}
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
				// The minimum legal rendered-response bound (review-z
				// commit 6); the 4096-char content still overflows it.
				GeneratedResponseBytes: MinGeneratedResponseBytes,
			},
		},
		roundTrip,
		nil,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an oversized rendered response", rec.Code)
	}
	// Pin the MECHANISM: the failure must be the generated-response bound,
	// not an earlier rejection (content type, conversion, etc.).
	if !strings.Contains(rec.Body.String(), "generated response exceeds the configured limit") {
		t.Fatalf("error body = %q, want the generated-response bound error", rec.Body.String())
	}
	if len(rec.Body.Bytes()) > int(MinGeneratedResponseBytes) {
		t.Fatalf("rendered error body = %d bytes, want <= the bound", len(rec.Body.Bytes()))
	}
}

// TestErrorMessageBoundEndToEnd proves every client-visible error message is
// truncated to the configured ErrorMessageBytes bound (review-z commit 5).
func TestErrorMessageBoundEndToEnd(t *testing.T) {
	roundTrip := func(req *http.Request) (*http.Response, error) {
		return nil, errors.New(strings.Repeat("x", 8192))
	}
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
				ErrorMessageBytes:       256,
			},
		},
		roundTrip,
		nil,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	// The bound is on the client-visible MESSAGE TEXT: the dialect envelope
	// around it is bounded by the rendered-body limits, but the message
	// itself must never exceed ErrorMessageBytes.
	// Responses dialect nests the error under error.message.
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error body is not client-dialect JSON: %v", err)
	}
	if len(envelope.Error.Message) > 256 {
		t.Fatalf("client-visible message = %d bytes, want <= the 256 bound", len(envelope.Error.Message))
	}
	if len(envelope.Error.Message) == 0 {
		t.Fatal("client-visible message is empty")
	}
}

// TestMinimumOutputBoundsFit PROVES the MinGenerated* constants: the actual
// smallest legal terminal batches (both client dialects), the smallest SSE
// error frame, and the smallest rendered JSON response all fit within the
// constants, so a limit below them could never carry a legal completion
// (review-z commit 6).
func TestMinimumOutputBoundsFit(t *testing.T) {
	// Messages-client terminal batch: message_delta + message_stop. The
	// real state machine always carries the usage object on the delta when
	// the source provided usage (the required-wire shape), so BOTH the
	// bare and usage-carrying variants are measured and the larger must
	// fit.
	deltaBare, err := json.Marshal(AnthropicStreamEvent{
		Type: AnthropicStreamEventTypeMessageDelta,
		Delta: &AnthropicStreamDelta{
			StopReason: new(AnthropicStopReason("end_turn")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	deltaWithUsage, err := json.Marshal(AnthropicStreamEvent{
		Type: AnthropicStreamEventTypeMessageDelta,
		Delta: &AnthropicStreamDelta{
			StopReason: new(AnthropicStopReason("end_turn")),
		},
		Usage: &AnthropicUsage{
			InputTokens:              0,
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     0,
			OutputTokens:             0,
			OutputTokensDetails:      &AnthropicOutputTokensDetails{ThinkingTokens: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop, err := json.Marshal(AnthropicStreamEvent{Type: AnthropicStreamEventTypeMessageStop})
	if err != nil {
		t.Fatal(err)
	}
	// SSE framing: "event: <type>\ndata: <json>\n\n" per frame.
	messagesBatch := len(deltaWithUsage) + len(stop) + 128
	if messagesBatch > MinGeneratedBatchBytes {
		t.Fatalf("largest smallest messages terminal batch = %d, want <= %d", messagesBatch, MinGeneratedBatchBytes)
	}
	if len(deltaBare) > len(deltaWithUsage) {
		t.Fatal("bare delta larger than the usage-carrying delta; measure the wrong variant")
	}

	// Responses-client terminal frame: response.completed with the minimal
	// envelope (all required fields, empty output).
	completed, err := json.Marshal(ResponseEnvelope{
		ID: "resp_1234567890abcdef", Object: "response", CreatedAt: 1, Status: "completed",
		Model: "m", Output: []ResponsesOutputItem{}, Usage: &ResponsesUsage{
			InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	completedFrame := len(completed) + 64
	if completedFrame > MinGeneratedFrameBytes {
		t.Fatalf("smallest responses terminal frame = %d, want <= %d", completedFrame, MinGeneratedFrameBytes)
	}

	// Smallest SSE error event.
	errorEvent, err := json.Marshal(AnthropicStreamEvent{
		Type: AnthropicStreamEventTypeError,
		Error: &AnthropicStreamError{
			Type:    "api_error",
			Message: "",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(errorEvent)+64 > MinGeneratedFrameBytes {
		t.Fatalf("smallest error frame = %d, want <= %d", len(errorEvent)+64, MinGeneratedFrameBytes)
	}

	// Smallest rendered non-stream JSON response.
	ctx := testStreamContext()
	response := CanonicalResponse{
		ID: "resp_1", Model: "m", CreatedAt: 1, Status: CanonicalResponseCompleted,
		Stop:  CanonicalStop{Reason: CanonicalStopEndTurn},
		Items: []CanonicalResponseItem{},
		Usage: CanonicalUsage{},
	}
	rendered, _, err := RenderResponsesResponse(response, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rendered)) > MinGeneratedResponseBytes {
		t.Fatalf("smallest rendered response = %d, want <= %d", len(rendered), MinGeneratedResponseBytes)
	}
}

// TestBodyLimitsMinimumOutputRejected proves output limits below the
// minimum legal terminal or error frame fail validation while zero values
// (default-selected) and the minimum itself pass (review-z commit 6).
func TestBodyLimitsMinimumOutputRejected(t *testing.T) {
	base := BodyLimits{AcceptedRequestBytes: 1 << 20, SuccessfulResponseBytes: 1 << 20}
	if err := base.Validate(); err != nil {
		t.Fatalf("zero generated limits must be valid (defaults): %v", err)
	}
	tooSmall := base
	tooSmall.GeneratedSSEFrameBytes = MinGeneratedFrameBytes - 1
	if err := tooSmall.Validate(); err == nil {
		t.Fatal("GeneratedSSEFrameBytes below the minimum accepted")
	}
	tooSmall = base
	tooSmall.GeneratedSSEBatchBytes = MinGeneratedBatchBytes - 1
	if err := tooSmall.Validate(); err == nil {
		t.Fatal("GeneratedSSEBatchBytes below the minimum accepted")
	}
	tooSmall = base
	tooSmall.GeneratedResponseBytes = MinGeneratedResponseBytes - 1
	if err := tooSmall.Validate(); err == nil {
		t.Fatal("GeneratedResponseBytes below the minimum accepted")
	}
	atMin := base
	atMin.GeneratedSSEFrameBytes = MinGeneratedFrameBytes
	atMin.GeneratedSSEBatchBytes = MinGeneratedBatchBytes
	atMin.GeneratedResponseBytes = MinGeneratedResponseBytes
	if err := atMin.Validate(); err != nil {
		t.Fatalf("the minimum values must be valid: %v", err)
	}
}
