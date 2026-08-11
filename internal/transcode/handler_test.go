package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
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
	// The common test defaults live on the mapping (HandlerConfig carries
	// only Mapping, Upstream, and BodyLimits — review-z commit 4).
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	if mapping.LossPolicy.Allowed == nil {
		mapping.LossPolicy = StrictLossPolicy()
	}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}
	return NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
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

// TestTranscodeConstructorValidatesConfig verifies that
// NewTranscodeHandler validates its configuration eagerly, panicking on a
// nil round trip, an invalid mapping, invalid body limits, or an invalid
// upstream, rather than failing on the first request (review-08 additional
// 9).
func TestTranscodeConstructorValidatesConfig(t *testing.T) {
	validMapping := responsesMapping(t)
	validUpstream := mustParseURL(t, "https://upstream.example")
	validRoundTrip := func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}
	validLimits := BodyLimits{AcceptedRequestBytes: 1 << 20, SuccessfulResponseBytes: 1 << 20}

	withConfig := func(mutate func(*HandlerConfig)) HandlerConfig {
		validMapping.Auth = AuthPolicy{Mode: AuthNone}
		validMapping.ModelMap = ModelMap{AllowIdentity: true}
		validMapping.LossPolicy = StrictLossPolicy()
		cfg := HandlerConfig{
			Mapping:    validMapping,
			Upstream:   validUpstream,
			BodyLimits: validLimits,
		}
		mutate(&cfg)
		return cfg
	}

	assertPanics := func(t *testing.T, cfg HandlerConfig, roundTrip RoundTrip, want string) {
		t.Helper()
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatalf("NewTranscodeHandler: want panic containing %q, got nil", want)
			}
			if msg, ok := recovered.(string); !ok || !strings.Contains(msg, want) {
				t.Fatalf("NewTranscodeHandler panic = %v, want containing %q", recovered, want)
			}
		}()
		NewTranscodeHandler(cfg, roundTrip, nil)
	}

	t.Run("nil round trip", func(t *testing.T) {
		assertPanics(t, withConfig(func(*HandlerConfig) {}), nil, "nil round trip")
	})
	t.Run("invalid mapping", func(t *testing.T) {
		mapping := validMapping
		mapping.ClientRoute = RouteKey{Method: http.MethodGet, Path: "/v1/responses"}
		assertPanics(t, withConfig(func(cfg *HandlerConfig) { cfg.Mapping = mapping }), validRoundTrip, "invalid handler configuration")
	})
	t.Run("invalid body limits", func(t *testing.T) {
		assertPanics(t, withConfig(func(cfg *HandlerConfig) {
			cfg.BodyLimits = BodyLimits{AcceptedRequestBytes: -1}
		}), validRoundTrip, "invalid body limits")
	})
	t.Run("nil upstream", func(t *testing.T) {
		assertPanics(t, withConfig(func(cfg *HandlerConfig) { cfg.Upstream = nil }), validRoundTrip, "invalid upstream URL")
	})
	t.Run("upstream without host", func(t *testing.T) {
		u := mustParseURL(t, "https://")
		assertPanics(t, withConfig(func(cfg *HandlerConfig) { cfg.Upstream = u }), validRoundTrip, "invalid upstream URL")
	})
	t.Run("upstream port without hostname", func(t *testing.T) {
		u := mustParseURL(t, "https://:8080")
		assertPanics(t, withConfig(func(cfg *HandlerConfig) { cfg.Upstream = u }), validRoundTrip, "invalid upstream URL")
	})
	t.Run("upstream bad scheme", func(t *testing.T) {
		u := mustParseURL(t, "ftp://upstream.example")
		assertPanics(t, withConfig(func(cfg *HandlerConfig) { cfg.Upstream = u }), validRoundTrip, "invalid upstream URL")
	})
	t.Run("valid configuration does not panic", func(t *testing.T) {
		handler := NewTranscodeHandler(withConfig(func(*HandlerConfig) {}), validRoundTrip, nil)
		if handler == nil {
			t.Fatal("NewTranscodeHandler returned nil")
		}
	})
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
		Auth:             AuthPolicy{Mode: AuthNone},
	}
}

func messagesMapping(t *testing.T, upstream UpstreamProtocol) Mapping {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientMessages,
		UpstreamProtocol: upstream,
		UpstreamPath:     "/v1/" + string(upstream),
		Auth:             AuthPolicy{Mode: AuthNone},
	}
	if upstream == UpstreamResponses {
		// Messages tools carry no strictness; a messages->responses mapping
		// under the strict policy is a startup rejection (review-z commit
		// 6). These tests exercise error/stream behavior, not the
		// strictness loss, so the mapping allows it explicitly.
		mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
			FeatureToolSchemaStrictness: {},
		}}
	}
	return mapping
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes: 16,
			},
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

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

func TestHandlerContentEncodingIdentityAccepted(t *testing.T) {
	// The identity content encoding is the no-op and must be accepted
	// (review-j finding 15); only non-identity encodings are unsupported.
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
			)),
		}, nil
	})
	for _, encoding := range []string{"identity", "Identity", " IDENTITY "} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"x"}`),
		)
		req.Header.Set("Content-Encoding", encoding)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Content-Encoding %q: status = %d, want 200", encoding, rec.Code)
		}
	}
}

func TestHandlerDecodedRequestAmplification413(t *testing.T) {
	// A decoded/rendered request that amplifies beyond the decoded-request
	// body limit is a 413 in the client dialect, not the generic conversion
	// 400 (review-j finding 15).
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
				DecodedRequestBytes:     16,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		},
		nil,
	)
	// The accepted request is small; the rendered chat request is larger
	// than the 16-byte decoded limit.
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "decoded request") {
		t.Fatalf("body does not explain the decoded-request limit: %s", rec.Body.String())
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	var outcomes []Outcome
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			// Upstream returns a 200 with a VALID Chat response whose
			// finish_reason is outside the supported subset: a
			// known-but-unsupported feature is a local conversion failure,
			// never an upstream failure (review-k finding 3).
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"weird","message":{"role":"assistant","content":"x"}}]}`,
				)),
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

// TestHandlerCorruptUpstreamResponseIsUpstreamFailure proves the review-k
// finding-3 counterexample: a 200 response that is not a valid instance of
// the supported Chat subset (here: an object that is not a chat completion
// at all) is corrupt upstream wire — recorded as an upstream body failure
// with UpstreamFailure=true, never a local conversion failure.
func TestHandlerCorruptUpstreamResponseIsUpstreamFailure(t *testing.T) {
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	var outcomes []Outcome
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
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
	// The client still receives a dialect-correct error; only the recorded
	// provenance and breaker accounting differ from a local failure.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d", len(outcomes))
	}
	if outcomes[0].Provenance != ProvenanceUpstreamBodyError {
		t.Fatalf("provenance = %s, want upstream_body_error", outcomes[0].Provenance)
	}
	if !outcomes[0].UpstreamFailure {
		t.Fatal("corrupt upstream wire must be an upstream failure")
	}
}

// TestHandlerReviewKChatCounterexampleIsUpstreamFailure proves the exact
// review-k finding-4 counterexample end to end: a single choice with index
// zero, a user-role message, and no finish_reason is rejected with a
// client-dialect error and recorded as an upstream failure — it can never
// become a successful assistant response (review-k findings 3 and 4).
func TestHandlerReviewKChatCounterexampleIsUpstreamFailure(t *testing.T) {
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	var outcomes []Outcome
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"user","content":"x"}}]}`,
				)),
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
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d", len(outcomes))
	}
	if outcomes[0].Provenance != ProvenanceUpstreamBodyError {
		t.Fatalf("provenance = %s, want upstream_body_error", outcomes[0].Provenance)
	}
	if !outcomes[0].UpstreamFailure {
		t.Fatal("the review-k counterexample must record an upstream failure")
	}
}

func TestHandlerMessagesToResponsesJSON(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureTopK:                   {},
		FeatureReasoningSummary:       {},
		FeatureOutputItemBoundaries:   {},
		FeaturePreviousResponseID:     {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
		FeatureToolSchemaStrictness:   {},
	}}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			// The upstream request is a Responses request with instructions.
			body, _ := io.ReadAll(req.Body)
			var envelope openairesponses.Request
			if err := strictDecode(body, &envelope); err != nil {
				t.Fatalf("upstream request: %v\n%s", err, body)
			}
			if envelope.Instructions == nil {
				t.Fatal("instructions missing")
			}
			// A non-streaming exchange must not demand SSE from the
			// upstream: the rendered request carries no stream key
			// (review run 1 finding F1).
			if envelope.Stream.Present {
				t.Fatalf("non-streaming upstream request carries stream: %s", body)
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

// TestHandlerMessagesToChatJSON verifies the non-stream response dialect for
// a messages client with a chat upstream: the chat response must render as an
// Anthropic message envelope, not as a Responses envelope.
func TestHandlerMessagesToChatJSON(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureTopK:                   {},
		FeatureReasoningSummary:       {},
		FeatureOutputItemBoundaries:   {},
		FeaturePreviousResponseID:     {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			// The upstream request is a Chat request.
			body, _ := io.ReadAll(req.Body)
			var chat ChatRequest
			if err := strictDecode(body, &chat); err != nil {
				t.Fatalf("upstream request: %v\n%s", err, body)
			}
			if len(chat.Messages) == 0 {
				t.Fatal("chat messages missing")
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
	var text string
	for _, block := range message.Content {
		if block.Type == AnthropicContentBlockTypeText && block.Text != nil {
			text += *block.Text
		}
	}
	if !strings.Contains(text, "weather") {
		t.Fatalf("message text %q does not contain the upstream content", text)
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

// TestHandlerStreamingResponsesToChatAcceptOnly proves the Accept-derived
// stream intent is written back into the rendered upstream request: a client
// that signals streaming ONLY via Accept: text/event-stream must produce an
// upstream request with stream:true + stream_options.include_usage:true, and
// the SSE response must be accepted — never the JSON-vs-SSE 502 mismatch
// (review-j finding 6).
func TestHandlerStreamingResponsesToChatAcceptOnly(t *testing.T) {
	var (
		mu      sync.Mutex
		gotReq  *http.Request
		gotBody []byte
	)
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		gotReq = req.Clone(req.Context())
		gotBody = body
		mu.Unlock()
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
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("missing terminal: %q", rec.Body.String())
	}
	var probe struct {
		Stream        *bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	mu.Lock()
	defer mu.Unlock()
	if got := gotReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("outbound Accept = %q, want text/event-stream", got)
	}
	if err := json.Unmarshal(gotBody, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Stream == nil || !*probe.Stream {
		t.Fatalf("upstream request stream = %v, want true: %s", probe.Stream, gotBody)
	}
	if probe.StreamOptions == nil || probe.StreamOptions.IncludeUsage == nil || !*probe.StreamOptions.IncludeUsage {
		t.Fatalf("upstream request include_usage = %v, want true: %s", probe.StreamOptions, gotBody)
	}
}

// TestExplicitStreamFalseIsNotOverridden proves the request body's stream
// field is authoritative over the Accept header (review-08 blocker 1): an
// explicit stream:false must never become an upstream SSE request or an SSE
// response, regardless of the client's Accept header — the outbound request
// stays non-streaming, its Accept header is rewritten to application/json,
// and a JSON upstream response is accepted.
func TestExplicitStreamFalseIsNotOverridden(t *testing.T) {
	assertNonStreaming := func(t *testing.T, clientProtocol string, policy LossPolicy, body string) {
		t.Helper()
		var (
			mu      sync.Mutex
			gotReq  *http.Request
			gotBody []byte
		)
		var mapping Mapping
		if clientProtocol == string(ClientResponses) {
			mapping = responsesMapping(t)
			mapping.ModelMap = ModelMap{AllowIdentity: true}
			mapping.LossPolicy = policy
			mapping.Auth = AuthPolicy{Mode: AuthNone}
			mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
			mapping.AllowedClientQuery = map[string]struct{}{}
		} else {
			mapping = messagesMapping(t, UpstreamChatCompletions)
			mapping.ModelMap = ModelMap{AllowIdentity: true}
			mapping.LossPolicy = policy
			mapping.Auth = AuthPolicy{Mode: AuthNone}
			mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
			mapping.AllowedClientQuery = map[string]struct{}{}

		}
		handler := NewTranscodeHandler(
			HandlerConfig{
				Mapping:  mapping,
				Upstream: mustParseURL(t, "https://upstream.example"),
				BodyLimits: BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					SuccessfulResponseBytes: 1 << 20,
				},
			},
			func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				mu.Lock()
				gotReq = req.Clone(req.Context())
				gotBody = body
				mu.Unlock()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
				}, nil
			},
			nil,
		)
		req := httptest.NewRequest(http.MethodPost, mapping.ClientRoute.Path, strings.NewReader(body))
		req.Header.Set("Accept", "text/event-stream")
		// A client Connection token nominating Accept for removal at its hop
		// must not strip the proxy's rewritten outbound Accept: the client's
		// hop-by-hop directives govern the client-to-proxy hop only, and the
		// proxy generates a fresh Accept for its own hop upstream.
		req.Header.Set("Connection", "accept")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		if got := gotReq.Header.Get("Accept"); got != "application/json" {
			t.Errorf("outbound Accept = %q, want application/json", got)
		}
		var probe struct {
			Stream *bool `json:"stream"`
		}
		if err := json.Unmarshal(gotBody, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Stream == nil || *probe.Stream {
			t.Errorf("upstream request stream = %v, want explicit false: %s", probe.Stream, gotBody)
		}
		// The client receives a JSON response, never an SSE stream.
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("response Content-Type = %q, want application/json", ct)
		}
		if strings.Contains(rec.Body.String(), "event: response.created") {
			t.Errorf("response carries SSE events for a non-streaming exchange: %q", rec.Body.String())
		}
	}
	t.Run("responses-client", func(t *testing.T) {
		assertNonStreaming(t, string(ClientResponses), StrictLossPolicy(),
			`{"model":"m","input":"x","stream":false}`)
	})
	t.Run("messages-client", func(t *testing.T) {
		// The Chat fixture's usage_timing breakdown is not portable to
		// Messages; the permissive policy approves that response-side loss.
		assertNonStreaming(t, string(ClientMessages), j6PermissivePolicy(),
			`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	})
}

// TestExplicitStreamTrueOverridesJsonAccept proves the body-authoritative
// precedence in the streaming direction (review-08 blocker 1): an explicit
// body stream:true with Accept: application/json still produces an upstream
// SSE request whose outbound Accept is rewritten to text/event-stream, and
// the SSE response is accepted — the mirror of
// TestExplicitStreamFalseIsNotOverridden, pinning the documented rule that
// the outbound Accept always matches the converted request body's stream
// value.
func TestExplicitStreamTrueOverridesJsonAccept(t *testing.T) {
	var (
		mu      sync.Mutex
		gotReq  *http.Request
		gotBody []byte
	)
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		gotReq = req.Clone(req.Context())
		gotBody = body
		mu.Unlock()
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
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := gotReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("outbound Accept = %q, want text/event-stream", got)
	}
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(gotBody, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Stream == nil || !*probe.Stream {
		t.Errorf("upstream request stream = %v, want explicit true: %s", probe.Stream, gotBody)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("response Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "response.completed") {
		t.Errorf("missing SSE terminal: %q", rec.Body.String())
	}
}

func TestHandlerStreamingMessagesToResponses(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = j6PermissivePolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

	// The fixture carries a reasoning item and no early usage; the
	// response-side losses are approved for this conversion.
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(bytes.NewReader(
					testcorpus.ResponsesStreamSSE(),
				)),
			}, nil
		},
		nil,
	)
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
	// Merge gate 17 requires the response media type to agree with the
	// stream intent; the exchange is rejected with a dialect-correct error.
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
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// The reverse mismatch: an SSE response for a non-streaming request.
	handler2 := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE())),
		}, nil
	})
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec2 := httptest.NewRecorder()
	handler2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", rec2.Code, rec2.Body.String())
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
	mapping.ModelMap = ModelMap{Exact: map[string]ModelMapping{
		"client-model": {
			ClientModel:         "client-model",
			UpstreamModel:       "upstream-model",
			ClientResponseModel: "client-model",
		},
	}}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
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
	mapping.ModelMap = ModelMap{
		// An explicit-map policy must actually map something: with entries,
		// an UNKNOWN model is rejected at request time (400); a policy with
		// zero entries is a startup rejection (see
		// TestMappingMissingModelPolicyRejected).
		RequireExplicitMap: true,
		Exact: map[string]ModelMapping{
			"known": {ClientModel: "known", UpstreamModel: "upstream-known"},
		},
	}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes: 1 << 20,
			},
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary:       {},
		FeatureOutputItemBoundaries:   {},
		FeaturePreviousResponseID:     {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	mapping.Auth = AuthPolicy{
		Mode:             AuthBearer,
		Inbound:          true,
		AnthropicVersion: "2023-06-01",
	}
	mapping.AllowedClientQuery = map[string]struct{}{}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{
		"api-version": {},
	}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{
		"api-version": {},
	}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes: 1 << 20,
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
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

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

// TestHandlerMessagesFailedUpstreamNotSuccess verifies merge gate 10 for the
// non-streaming path: a 2xx Responses body with status "failed" must surface
// as a client-dialect error, never as a successful Messages completion.
func TestHandlerMessagesFailedUpstreamNotSuccess(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureTopK:                 {},
		FeatureReasoningSummary:     {},
		FeatureOutputItemBoundaries: {},
		FeaturePreviousResponseID:   {},
		FeatureToolSchemaStrictness: {},
	}}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"resp_f","object":"response","created_at":1710000000,"status":"failed","model":"gpt-4.1","error":{"code":"api_error","message":"upstream exploded"},"output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}`,
				)),
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
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var message AnthropicMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err == nil && message.Type == "message" {
		t.Fatalf("failed upstream rendered as a successful message: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream exploded") {
		t.Fatalf("error body does not carry the upstream failure: %s", rec.Body.String())
	}
}

// TestHandlerHopByHopHeadersStrippedJSON verifies Connection-nominated
// hop-by-hop headers do not leak on the non-streaming success path.
func TestHandlerHopByHopHeadersStrippedJSON(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":      []string{"application/json"},
				"Connection":        []string{"X-Internal-Secret"},
				"X-Internal-Secret": []string{"s3cr3t"},
				"X-Keep":            []string{"visible"},
				"X-Request-Id":      []string{"req_1"},
			},
			Body: io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Internal-Secret"); got != "" {
		t.Fatalf("Connection-nominated header leaked: %q", got)
	}
	// The response allowlist is stricter than hop-by-hop removal: any
	// non-listed upstream header, including an ordinary entity header, is
	// stripped on a transcoded route (review-08 blocker 10).
	if got := rec.Header().Get("X-Keep"); got != "" {
		t.Fatalf("non-allowed entity header leaked: %q", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "req_1" {
		t.Fatalf("request-id header dropped: %q", got)
	}
}

// TestHandlerConversationStateRejectedChat verifies that Responses request
// fields not portable to a Chat upstream are rejected (strict policy)
// rather than silently dropped while echoed as honored.
func TestHandlerConversationStateRejectedChat(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})
	for _, body := range []string{
		`{"model":"m","input":"x","previous_response_id":"resp_prev"}`,
		`{"model":"m","input":"x","truncation":"auto"}`,
		`{"model":"m","input":"x","top_logprobs":3}`,
		`{"model":"m","input":"x","service_tier":"auto"}`,
	} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(body),
		)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestHandlerStreamingMessagesToResponsesStrictRejectsEarlyUsage pins the
// strict-policy behavior: a Messages stream whose source cannot provide the
// required early message_start usage is rejected with a client-dialect error
// event (review-j finding 9: zeros would fabricate facts; the FeatureUsageUnknown
// decision is explicit).
func TestHandlerStreamingMessagesToResponsesStrictRejectsEarlyUsage(t *testing.T) {
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
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("strict policy must reject with an error event: %q", body)
	}
	if !strings.Contains(body, "usage_unknown") {
		t.Fatalf("error event must name the usage_unknown feature: %q", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("rejected stream must not terminate cleanly: %q", body)
	}
}

// captureSigner records the request it signed.
type captureSigner struct {
	req *http.Request
}

func (s *captureSigner) Sign(_ context.Context, req *http.Request) error {
	s.req = req
	return nil
}

// representationHeaders returns the inbound representation-metadata headers
// used by the sanitizer tests.
func representationHeaders() http.Header {
	return http.Header{
		"Content-Digest":      {"sha-256=:abc:"},
		"Digest":              {"sha-256=:abc:"},
		"Repr-Digest":         {"sha-256=:abc:"},
		"Content-Md5":         {"abc"},
		"Signature":           {"sig1=:abc:"},
		"Signature-Input":     {`sig1=("content-digest" "@method" "@path");created=1`},
		"Content-Range":       {"bytes 0-10/100"},
		"Content-Length":      {"999"},
		"Content-Encoding":    {"identity"},
		"Etag":                {`"abc"`},
		"Last-Modified":       {"Wed, 21 Oct 2015 07:28:00 GMT"},
		"If-Match":            {`"abc"`},
		"If-None-Match":       {`"abc"`},
		"If-Modified-Since":   {"Wed, 21 Oct 2015 07:28:00 GMT"},
		"If-Unmodified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT"},
		"If-Range":            {`"abc"`},
	}
}

// TestHandlerTransformedRequestSanitized proves inbound integrity digests,
// message signatures, content metadata, and validators never reach the
// upstream, and Content-Length is recomputed from the converted body
// (review-j finding 12).
func TestHandlerTransformedRequestSanitized(t *testing.T) {
	var got *http.Request
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	req.Header = representationHeaders()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got == nil {
		t.Fatal("upstream request not captured")
	}
	for name := range representationHeaders() {
		if got.Header.Get(name) != "" {
			t.Fatalf("upstream request carries stale %s: %q", name, got.Header.Get(name))
		}
	}
	body, _ := io.ReadAll(got.Body)
	if got.ContentLength != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d (recomputed)", got.ContentLength, len(body))
	}
}

// TestHandlerExternalSignerAttachedToContext proves the external-signer mode
// attaches the signer to the request context instead of invoking it inline:
// the signing transport inside the retry chain signs every actual attempt
// after body reconstruction (review-z commit 4).
func TestHandlerExternalSignerAttachedToContext(t *testing.T) {
	signer := &captureSigner{}
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthExternalSigner, Signer: signer}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

	var observed *http.Request
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			// The signer travels in the request context; the transport
			// layer (SigningTransport) invokes it per attempt.
			if RequestSignerFromContext(req.Context()) != signer {
				t.Fatal("signer not attached to the upstream request context")
			}
			observed = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
				)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	req.Header = representationHeaders()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if observed == nil {
		t.Fatal("round trip was not called")
	}
	for name := range representationHeaders() {
		if observed.Header.Get(name) != "" {
			t.Fatalf("upstream request carries stale %s: %q", name, observed.Header.Get(name))
		}
	}
	// The signer itself is exercised end-to-end by the proxy-level signing
	// tests (SigningTransport signs the exact attempt).
}

// leakingPathSecret is a SecretSource whose error message contains a file
// path.
type leakingPathSecret struct{}

func (leakingPathSecret) Secret(context.Context) (string, error) {
	return "", errors.New("read /etc/secrets/upstream-key: no such file")
}

// TestHandlerInternalErrorSanitized proves internal construction errors
// (secret file paths) never leak into the client message (review-j finding
// 14); the detail is logged instead.
func TestHandlerInternalErrorSanitized(t *testing.T) {
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{
		Mode:   AuthBearer,
		Secret: leakingPathSecret{},
	}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
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
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/etc/secrets") {
		t.Fatalf("client message leaks the secret file path: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("client message should be generic: %q", rec.Body.String())
	}
}

// TestHandlerResponsesRequestMissingToolStrictRejected proves the pinned
// strict requirement end to end: a Responses client request whose tool
// entries omit strict is rejected client-dialect (400) BEFORE any upstream
// request is made (review-z commit 1).
func TestHandlerResponsesRequestMissingToolStrictRejected(t *testing.T) {
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	upstreamCalls := 0
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			upstreamCalls++
			t.Fatal("upstream must never be reached for a malformed request")
			return nil, nil
		},
		func(Outcome) {},
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{
			"model":"m",
			"input":"hi",
			"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]
		}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
	if !strings.Contains(rec.Body.String(), "strict") {
		t.Fatalf("error body does not name strict: %s", rec.Body.String())
	}
}
