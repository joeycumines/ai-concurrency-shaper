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
				GeneratedResponseBytes:  512,
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
	if len(rec.Body.Bytes()) > 512 {
		t.Fatalf("rendered error body = %d bytes, want <= the 512 bound", len(rec.Body.Bytes()))
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
