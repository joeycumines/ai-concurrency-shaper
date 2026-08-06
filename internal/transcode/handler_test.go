package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// testHandler builds a TranscodeHandler with a scripted round trip.
func testHandler(t *testing.T, mapping Mapping, roundTrip RoundTrip) *TranscodeHandler {
	t.Helper()
	return NewTranscodeHandler(
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
		roundTrip,
		nil,
	)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func responsesMapping(t *testing.T) Mapping {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	return Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientResponses,
		UpstreamProtocol: UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
	}
}

func messagesMapping(t *testing.T, upstream UpstreamProtocol) Mapping {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	return Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientMessages,
		UpstreamProtocol: upstream,
		UpstreamPath:     "/v1/" + string(upstream),
	}
}

func TestHandlerResponsesToChatJSON(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		// Verify the upstream request: path, auth, headers.
		if req.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatalf("no auth configured, got %q", req.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(req.Body)
		var chat ChatRequest
		if err := strictDecode(body, &chat); err != nil {
			t.Fatalf("upstream request: %v\n%s", err, body)
		}
		if chat.N == nil || *chat.N != 1 {
			t.Fatalf("n = %v", chat.N)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(
				testcorpus.ChatCompletionsResponseJSON(),
			)),
		}, nil
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		bytes.NewReader(testcorpus.ResponsesRequestJSON()),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope ResponseEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Object != "response" || envelope.Status != "completed" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if len(envelope.Output) != 1 {
		t.Fatalf("output = %d", len(envelope.Output))
	}
}

func TestHandlerStringInputJSON(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var chat ChatRequest
		if err := strictDecode(body, &chat); err != nil {
			t.Fatalf("upstream request: %v\n%s", err, body)
		}
		if len(chat.Messages) != 1 || chat.Messages[0].Role != ChatMessageRoleUser {
			t.Fatalf("messages = %+v", chat.Messages)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(
				testcorpus.ChatCompletionsResponseJSON(),
			)),
		}, nil
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"gpt-4.1","input":"Hello"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerRequestConversionErrorDialect400(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called on conversion error")
		return nil, nil
	})

	// Unknown field -> strict decode error -> client-dialect 400.
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","bogus":1}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// The error is rendered in the client dialect (OpenAI error envelope).
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Message == "" || envelope.Error.Type == "" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestHandlerBodyLimit413(t *testing.T) {
	mapping := responsesMapping(t)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes: 16,
			},
			ModelMap:   ModelMap{AllowIdentity: true},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
		},
		func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"`+strings.Repeat("x", 64)+`"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestHandlerRetryReplayLimitSeparateFromInbound proves the retry-replay cap
// does not gate inbound payloads: a request larger than RetryReplayBytes but
// under AcceptedRequestBytes is accepted and forwarded (the retry transport's
// own MaxBodyBytes bounds what can be replayed, never what is admitted).
func TestHandlerRetryReplayLimitSeparateFromInbound(t *testing.T) {
	mapping := responsesMapping(t)
	const (
		replayCap = 1 << 20 // 1 MiB — what retries may replay
		bodySize  = replayCap * 6
	)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    bodySize * 2, // inbound cap is larger than the payload
				RetryReplayBytes:        replayCap,    // payload is far above what retries may replay
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap:           ModelMap{AllowIdentity: true},
			LossPolicy:         StrictLossPolicy(),
			AuthPolicy:         AuthPolicy{Mode: AuthNone},
			ChatCapabilities:   ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
			AllowedClientQuery: map[string]struct{}{},
		},
		func(req *http.Request) (*http.Response, error) {
			upstreamBody, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read upstream body: %v", err)
			}
			// The full client payload must have been forwarded: the rendered
			// upstream body carries the 6 MiB input, so it far exceeds the
			// 1 MiB retry-replay cap. The replay cap never gates admission.
			if len(upstreamBody) <= int(replayCap) {
				t.Fatalf("upstream body = %d bytes, want > %d (full payload forwarded)", len(upstreamBody), replayCap)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(
					testcorpus.ChatCompletionsResponseJSON(),
				)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"`+strings.Repeat("x", bodySize)+`"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retry-replay cap must not gate inbound)", rec.Code)
	}
}

func TestHandlerContentEncoding415(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandlerUpstreamErrorDialect(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"X-Request-Id": []string{"req_1"},
			},
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") != "req_1" {
		t.Fatalf("request id = %q", rec.Header().Get("X-Request-Id"))
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Type != "rate_limit_error" || envelope.Error.Message != "slow down" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestHandlerUpstreamErrorMessagesDialect(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"message":"bad gateway","type":"api_error"}}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Message != "bad gateway" {
		t.Fatalf("body = %+v", body)
	}
}

func TestHandlerLocalConversion502NotUpstreamFailure(t *testing.T) {
	mapping := responsesMapping(t)
	var outcomes []Outcome
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap:   ModelMap{AllowIdentity: true},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
		},
		func(req *http.Request) (*http.Response, error) {
			// Upstream returns a 200 with malformed JSON for the client
			// dialect.
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"not":"chat"}`)),
			}, nil
		},
		func(o Outcome) { outcomes = append(outcomes, o) },
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	// The local 502 must be recorded as a local conversion error, not an
	// upstream failure.
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d", len(outcomes))
	}
	if outcomes[0].Provenance != ProvenanceLocalResponseConversionError {
		t.Fatalf("provenance = %s", outcomes[0].Provenance)
	}
	if outcomes[0].UpstreamFailure {
		t.Fatal("local conversion must not be an upstream failure")
	}
}

func TestHandlerMessagesToResponsesJSON(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap: ModelMap{AllowIdentity: true},
			LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
				FeatureTopK:              {},
				FeatureReasoningSummary:  {},
				FeatureConversationState: {},
			}},
			AuthPolicy:         AuthPolicy{Mode: AuthNone},
			AllowedClientQuery: map[string]struct{}{},
		},
		func(req *http.Request) (*http.Response, error) {
			// The upstream request is a Responses request with instructions.
			body, _ := io.ReadAll(req.Body)
			var envelope responsesRequestEnvelope
			if err := strictDecode(body, &envelope); err != nil {
				t.Fatalf("upstream request: %v\n%s", err, body)
			}
			if envelope.Instructions == nil {
				t.Fatal("instructions missing")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(testcorpus.ResponsesResponseJSON())),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		bytes.NewReader(testcorpus.AnthropicMessagesRequestJSON()),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var message AnthropicMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "message" || message.Role != "assistant" {
		t.Fatalf("message = %+v", message)
	}
}

func TestHandlerStreamingResponsesToChat(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body: io.NopCloser(bytes.NewReader(
				testcorpus.ChatCompletionsStreamSSE(),
			)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	if conn := rec.Header().Get("Connection"); conn != "" {
		t.Fatalf("Connection header must not be set, got %q", conn)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.created") {
		t.Fatalf("missing created event: %q", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("missing completed event: %q", body)
	}
}

func TestHandlerStreamingMessagesToResponses(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body: io.NopCloser(bytes.NewReader(
				testcorpus.ResponsesStreamSSE(),
			)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_start") {
		t.Fatalf("missing message_start: %q", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("missing message_stop: %q", body)
	}
}

func TestHandlerTruncatedStreamErrorEvent(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		// The upstream stream truncates without a terminal.
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
			)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The stream must not silently end: an error event terminates it.
	if !strings.Contains(body, "event: error") {
		t.Fatalf("missing error event: %q", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("truncated stream must not report success: %q", body)
	}
}

func TestHandlerStreamIntentMismatch(t *testing.T) {
	// A JSON response for a streaming request is an upstream protocol
	// mismatch: the client requested SSE and the upstream returned JSON.
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// The handler decodes the JSON response regardless; the stream intent is
	// validated at admission in the proxy. The JSON path succeeds.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerUpgradeRequestRejected(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandlerModelMapping(t *testing.T) {
	mapping := responsesMapping(t)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap: ModelMap{Exact: map[string]ModelMapping{
				"client-model": {
					ClientModel:         "client-model",
					UpstreamModel:       "upstream-model",
					ClientResponseModel: "client-model",
				},
			}},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
		},
		func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			var chat ChatRequest
			if err := strictDecode(body, &chat); err != nil {
				t.Fatalf("upstream request: %v\n%s", err, body)
			}
			if chat.Model != "upstream-model" {
				t.Fatalf("upstream model = %q", chat.Model)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"client-model","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope ResponseEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Model != "client-model" {
		t.Fatalf("client response model = %q (must be the client alias)", envelope.Model)
	}
}

func TestHandlerModelMappingMissing(t *testing.T) {
	mapping := responsesMapping(t)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes: 1 << 20,
			},
			ModelMap:   ModelMap{RequireExplicitMap: true},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
		},
		func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"unknown","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandlerAuthApplied(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap: ModelMap{AllowIdentity: true},
			LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
				FeatureReasoningSummary:  {},
				FeatureConversationState: {},
			}},
			AuthPolicy: AuthPolicy{
				Mode:             AuthBearer,
				Inbound:          true,
				AnthropicVersion: "2023-06-01",
			},
			AllowedClientQuery: map[string]struct{}{},
		},
		func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer client-key" {
				t.Fatalf("Authorization = %q", got)
			}
			// The inbound Anthropic headers must be stripped.
			if got := req.Header.Get("X-Api-Key"); got != "" {
				t.Fatalf("X-Api-Key = %q", got)
			}
			if got := req.Header.Get("Anthropic-Version"); got != "" {
				t.Fatalf("Anthropic-Version = %q (non-Messages target)", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(testcorpus.ResponsesResponseJSON())),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("X-Api-Key", "client-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerQueryParametersAsConfig(t *testing.T) {
	mapping := responsesMapping(t)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap:   ModelMap{AllowIdentity: true},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
			AllowedClientQuery: map[string]struct{}{
				"api-version": {},
			},
		},
		func(req *http.Request) (*http.Response, error) {
			if got := req.URL.Query().Get("api-version"); got != "2024-01-01" {
				t.Fatalf("api-version = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
			}, nil
		},
		nil,
	)
	// An allowed client query parameter is preserved.
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses?api-version=2024-01-01",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerUnknownQueryRejected(t *testing.T) {
	mapping := responsesMapping(t)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes: 1 << 20,
			},
			ModelMap:   ModelMap{AllowIdentity: true},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
			AllowedClientQuery: map[string]struct{}{
				"api-version": {},
			},
		},
		func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		},
		nil,
	)
	// An unknown client query parameter is rejected on transcoded routes.
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses?evil=1",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandlerRoundTripErrorClosesBody(t *testing.T) {
	mapping := responsesMapping(t)
	closed := false
	body := &closeTrackingBody{closed: &closed}
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 0, Body: body}, errors.New("boom")
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !closed {
		t.Fatal("response body not closed on round trip error")
	}
	// The upstream transport error is a dialect 502.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
}

type closeTrackingBody struct {
	closed *bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (b *closeTrackingBody) Close() error {
	*b.closed = true
	return nil
}

func TestHandlerClientAbortReturns(t *testing.T) {
	mapping := responsesMapping(t)
	var (
		gotOutcome Outcome
		outcomeMu  sync.Mutex
	)
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
			return nil, context.Canceled
		},
		func(outcome Outcome) {
			outcomeMu.Lock()
			gotOutcome = outcome
			outcomeMu.Unlock()
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	outcomeMu.Lock()
	defer outcomeMu.Unlock()
	if gotOutcome.Provenance != ProvenanceClientAbort {
		t.Fatalf("outcome provenance = %v, want client abort", gotOutcome.Provenance)
	}
}

func TestHandlerUpstream100SwitchProtocolsRejected(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandlerNoLateOpsAfterReturn(t *testing.T) {
	// Streaming with a guard writer: after ServeHTTP returns, no
	// ResponseWriter operation may occur.
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE())),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	writer := newLateOpGuardWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, req)
		writer.markReturned()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	time.Sleep(20 * time.Millisecond)
	if writer.lateOps != 0 {
		t.Fatalf("late ops = %d", writer.lateOps)
	}
	if !strings.Contains(writer.body.String(), "response.completed") {
		t.Fatalf("missing terminal: %q", writer.body.String())
	}
}

func TestHandlerClientCancelMidStreamReleases(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
			)),
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	).WithContext(ctx)
	writer := newLateOpGuardWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, req)
		writer.markReturned()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for writer.flushCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after cancel")
	}
	time.Sleep(20 * time.Millisecond)
	if writer.lateOps != 0 {
		t.Fatalf("late ops = %d", writer.lateOps)
	}
}

// lateOpGuardWriter is a synchronized ResponseWriter that records operations
// after a return marker, mirroring the fuzz harness's returnGuardWriter.
type lateOpGuardWriter struct {
	mu       chan struct{}
	header   http.Header
	status   int
	body     bytes.Buffer
	returned bool
	lateOps  int
	flushes  int
}

func newLateOpGuardWriter() *lateOpGuardWriter {
	return &lateOpGuardWriter{mu: make(chan struct{}, 1)}
}

func (w *lateOpGuardWriter) lock() {
	w.mu <- struct{}{}
}

func (w *lateOpGuardWriter) unlock() {
	<-w.mu
}

func (w *lateOpGuardWriter) Header() http.Header {
	w.lock()
	defer w.unlock()
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *lateOpGuardWriter) WriteHeader(status int) {
	w.lock()
	defer w.unlock()
	if w.returned {
		w.lateOps++
		return
	}
	if w.status == 0 {
		w.status = status
	}
}

func (w *lateOpGuardWriter) Write(p []byte) (int, error) {
	w.lock()
	defer w.unlock()
	if w.returned {
		w.lateOps++
		return 0, io.ErrClosedPipe
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *lateOpGuardWriter) Flush() {
	w.lock()
	defer w.unlock()
	if w.returned {
		w.lateOps++
		return
	}
	w.flushes++
}

func (w *lateOpGuardWriter) markReturned() {
	w.lock()
	w.returned = true
	w.unlock()
}

func (w *lateOpGuardWriter) flushCount() int {
	w.lock()
	defer w.unlock()
	return w.flushes
}
