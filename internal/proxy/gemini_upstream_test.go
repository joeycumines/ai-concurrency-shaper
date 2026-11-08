package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// geminiUpstreamStub is a fake Gemini GenerateContent upstream that records
// what it received and answers from a queue of scripted responses.
type geminiUpstreamStub struct {
	server *httptest.Server
	mu     chan struct{}
	paths  []string
	heads  []http.Header
	bodies []string
	// responder builds the response for request n.
	responder func(n int, r *http.Request, body []byte) (int, string, http.Header)
}

func newGeminiUpstreamStub(t *testing.T, responder func(n int, r *http.Request, body []byte) (int, string, http.Header)) *geminiUpstreamStub {
	t.Helper()
	stub := &geminiUpstreamStub{
		mu:        make(chan struct{}, 1),
		responder: responder,
	}
	stub.mu <- struct{}{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		<-stub.mu
		n := len(stub.paths)
		stub.paths = append(stub.paths, r.URL.Path+"?"+r.URL.RawQuery)
		stub.heads = append(stub.heads, r.Header.Clone())
		stub.bodies = append(stub.bodies, string(body))
		stub.mu <- struct{}{}
		status, respBody, header := responder(n, r, body)
		for name, values := range header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *geminiUpstreamStub) url(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (s *geminiUpstreamStub) last() (string, http.Header, string) {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	n := len(s.paths) - 1
	return s.paths[n], s.heads[n], s.bodies[n]
}

// geminiOKResponse is a minimal successful GenerateContent response.
const geminiOKResponse = `{
	"candidates": [{"content": {"role": "model", "parts": [{"text": "hello"}]}, "finishReason": "STOP", "index": 0}],
	"usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5},
	"modelVersion": "gemini-2.5-flash",
	"responseId": "resp-1"
}`

// newGeminiUpstreamProxy builds a proxy whose /v1/messages and /v1/responses
// routes transcode to a Gemini upstream with the given mappings, using the
// goog-api-key auth mode and a shared signature store.
// geminiUpstreamDefaultLosses are the permissive-policy losses the helper
// grants unless strict is set.
var geminiUpstreamDefaultLosses = []transcode.Feature{
	transcode.FeatureMetadata,
	transcode.FeatureParallelToolCalls,
	transcode.FeatureGeminiClientQuery,
	transcode.FeatureGeminiThoughtSignaturePlaceholder,
	transcode.FeatureGeminiSafetyMetadata,
	transcode.FeatureGeminiGroundingMetadata,
	transcode.FeatureGeminiUsageBreakdown,
	transcode.FeatureGeminiResponseThoughtSignature,
	transcode.FeatureGeminiModelStatus,
	transcode.FeatureGeminiToolStrictness,
	transcode.FeatureGeminiImageDetail,
	transcode.FeatureUsageUnknown,
	transcode.FeatureUsageCacheReadUnknown,
	transcode.FeatureUsageCacheWriteUnknown,
	transcode.FeatureUsageReasoningUnknown,
	transcode.FeatureResponseServiceTier,
}

func newGeminiUpstreamProxy(t *testing.T, upstream *url.URL, mappings ...TranscodeMapping) *Proxy {
	t.Helper()
	return newGeminiUpstreamProxyStrict(t, upstream, false, mappings...)
}

func newGeminiUpstreamProxyStrict(t *testing.T, upstream *url.URL, strict bool, mappings ...TranscodeMapping) *Proxy {
	t.Helper()
	store := transcode.NewGeminiSignatureStore(transcode.GeminiSignatureConfig{})
	for i := range mappings {
		mappings[i].GeminiSignatures = store
		mappings[i].GeminiSignatureScope = "testprov"
		if mappings[i].Mapping.LossPolicy.Allowed == nil {
			mappings[i].Mapping.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{}}
		}
		if !strict {
			for _, f := range geminiUpstreamDefaultLosses {
				mappings[i].Mapping.LossPolicy.Allowed[f] = struct{}{}
			}
		}
		if mappings[i].Mapping.Auth.IsZero() {
			mappings[i].Mapping.Auth = transcode.AuthPolicy{
				Mode:   transcode.AuthGoogAPIKey,
				Secret: auth.NewStaticSecretSource("gemini-secret"),
			}
		}
	}
	p, err := New(
		WithUpstream(upstream),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(mappings...),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	return p
}

func messagesGeminiMapping() TranscodeMapping {
	return TranscodeMapping{
		ClientRoute:        transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:     transcode.ClientMessages,
		UpstreamProtocol:   transcode.UpstreamGemini,
		UpstreamPath:       "/v1beta",
		ModelMap:           transcode.ModelMap{AllowIdentity: true},
		AllowedClientQuery: map[string]struct{}{"beta": {}},
	}
}

func responsesGeminiMapping() TranscodeMapping {
	return TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamGemini,
		UpstreamPath:     "/v1beta",
		ModelMap:         transcode.ModelMap{AllowIdentity: true}}
}

// TestGeminiUpstreamMessagesNonStreaming proves an M2G exchange: the upstream
// sees POST /v1beta/models/<model>:generateContent with the goog-api-key
// credential and no client Authorization, and the client gets a Messages
// response.
func TestGeminiUpstreamMessagesNonStreaming(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, geminiOKResponse, nil
	})
	p := newGeminiUpstreamProxy(t, stub.url(t), messagesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u-1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, respBody)
	}
	var doc map[string]any
	if err := json.Unmarshal(respBody, &doc); err != nil {
		t.Fatalf("client response not JSON: %v\n%s", err, respBody)
	}
	if doc["type"] != "message" || doc["stop_reason"] != "end_turn" {
		t.Fatalf("client response = %s", respBody)
	}

	path, header, body := stub.last()
	if !strings.HasPrefix(path, "/v1beta/models/gemini-2.5-flash:generateContent") {
		t.Fatalf("upstream path = %q", path)
	}
	if strings.Contains(path, "beta=true") {
		t.Fatalf("client query forwarded to Gemini: %q", path)
	}
	if got := header.Get("X-Goog-Api-Key"); got != "gemini-secret" {
		t.Fatalf("upstream X-Goog-Api-Key = %q, want the provider secret", got)
	}
	if got := header.Get("Authorization"); got != "" {
		t.Fatalf("client Authorization reached the Gemini upstream: %q", got)
	}
	if got := header.Get("X-Api-Key"); got != "" {
		t.Fatalf("client X-Api-Key reached the Gemini upstream: %q", got)
	}
	if !strings.Contains(body, `"contents"`) || !strings.Contains(body, `"role":"user"`) {
		t.Fatalf("upstream body = %s", body)
	}
	if strings.Contains(body, "user_id") {
		t.Fatalf("client metadata leaked upstream: %s", body)
	}
}

// TestGeminiUpstreamResponsesNonStreaming proves an R2G exchange returns a
// Responses-shaped document.
func TestGeminiUpstreamResponsesNonStreaming(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, geminiOKResponse, nil
	})
	p := newGeminiUpstreamProxy(t, stub.url(t), responsesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
		strings.NewReader(`{"model":"gemini-2.5-flash","input":"hi","parallel_tool_calls":false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, respBody)
	}
	var doc map[string]any
	if err := json.Unmarshal(respBody, &doc); err != nil {
		t.Fatalf("client response not JSON: %v\n%s", err, respBody)
	}
	if doc["object"] != "response" || doc["status"] != "completed" {
		t.Fatalf("client response = %s", respBody)
	}
	_, header, _ := stub.last()
	if got := header.Get("X-Goog-Api-Key"); got != "gemini-secret" {
		t.Fatalf("upstream X-Goog-Api-Key = %q", got)
	}
	if got := header.Get("Authorization"); got != "" {
		t.Fatalf("client Authorization reached the Gemini upstream: %q", got)
	}
}

// TestGeminiUpstream429IsUpstreamFailure proves a Gemini 429 with Retry-After
// surfaces to the client with the same status/header and counts as a breaker
// upstream failure.
func TestGeminiUpstream429IsUpstreamFailure(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusTooManyRequests,
			`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`,
			http.Header{"Retry-After": []string{"7"}}
	})
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	mapping := messagesGeminiMapping()
	mapping.GeminiSignatures = transcode.NewGeminiSignatureStore(transcode.GeminiSignatureConfig{})
	mapping.GeminiSignatureScope = "testprov"
	mapping.Mapping.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{}}
	mapping.Mapping.Auth = transcode.AuthPolicy{
		Mode:   transcode.AuthGoogAPIKey,
		Secret: auth.NewStaticSecretSource("gemini-secret"),
	}
	p, err := New(
		WithUpstream(stub.url(t)),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
		WithTranscodeMapping(mapping),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
		strings.NewReader(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429; body %s", resp.StatusCode, respBody)
	}
	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	stats := waitForOutcome(t, breaker, 1)
	if stats.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1 (429 is an upstream failure)", stats.ConsecutiveFailures)
	}
}

// TestGeminiUpstreamUnrepresentableFinishIsBreakerNeutral proves a 2xx
// response whose finishReason the transcoder cannot represent is a local,
// breaker-neutral error.
func TestGeminiUpstreamUnrepresentableFinishIsBreakerNeutral(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, `{
			"candidates": [{"content": {"role": "model", "parts": [{"text": "x"}]}, "finishReason": "MISSING_THOUGHT_SIGNATURE"}],
			"modelVersion": "m"
		}`, nil
	})
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	mapping := messagesGeminiMapping()
	mapping.GeminiSignatures = transcode.NewGeminiSignatureStore(transcode.GeminiSignatureConfig{})
	mapping.GeminiSignatureScope = "testprov"
	mapping.Mapping.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{}}
	mapping.Mapping.Auth = transcode.AuthPolicy{
		Mode:   transcode.AuthGoogAPIKey,
		Secret: auth.NewStaticSecretSource("gemini-secret"),
	}
	p, err := New(
		WithUpstream(stub.url(t)),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
		WithTranscodeMapping(mapping),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	post := func() string {
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
			strings.NewReader(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	body := post()
	if !strings.Contains(body, "MISSING_THOUGHT_SIGNATURE") {
		t.Fatalf("client body = %s, want the unrepresentable finish reason named", body)
	}
	// Two exchanges: neither may move the breaker.
	_ = post()
	stats := breaker.Stats()
	if stats.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0 (unrepresentable finish is breaker-neutral)", stats.ConsecutiveFailures)
	}
}

// TestGeminiUpstreamSignatureReplay proves a two-turn tool conversation sends
// the first turn's thought signature back on the replayed function call.
func TestGeminiUpstreamSignatureReplay(t *testing.T) {
	var calls atomic.Int32
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		if calls.Add(1) == 1 {
			return http.StatusOK, `{
				"candidates": [{"content": {"role": "model", "parts": [
					{"functionCall": {"id": "call_1", "name": "get_weather", "args": {"city": "Paris"}}, "thoughtSignature": "sig-xyz"}
				]}, "finishReason": "STOP"}],
				"usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5},
				"modelVersion": "gemini-2.5-flash", "responseId": "resp-1"
			}`, nil
		}
		return http.StatusOK, geminiOKResponse, nil
	})
	p := newGeminiUpstreamProxy(t, stub.url(t), messagesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	post := func(payload string) string {
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
		return string(body)
	}

	first := post(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"weather?"}]}`)
	if !strings.Contains(first, "tool_use") || !strings.Contains(first, "call_1") {
		t.Fatalf("first response = %s", first)
	}
	second := post(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}
	]}`)
	_ = second

	<-stub.mu
	secondBody := stub.bodies[1]
	stub.mu <- struct{}{}
	if !strings.Contains(secondBody, `"thoughtSignature":"sig-xyz"`) {
		t.Fatalf("second request does not replay the stored signature: %s", secondBody)
	}
	if !strings.Contains(secondBody, `"functionResponse"`) {
		t.Fatalf("second request lacks the functionResponse part: %s", secondBody)
	}
}

// TestGeminiUpstreamMultiTargetDispatch proves a /v1/messages route carrying
// both a chat and a gemini mapping dispatches via=gemini models to Gemini and
// via-less models to chat, in either registration order.
func TestGeminiUpstreamMultiTargetDispatch(t *testing.T) {
	geminiStub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, geminiOKResponse, nil
	})
	chatBody := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"chat-model","choices":[{"index":0,"message":{"role":"assistant","content":"from chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	chatStub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, chatBody, nil
	})

	chatMapping := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth: transcode.AuthPolicy{
			Mode:   transcode.AuthBearer,
			Secret: auth.NewStaticSecretSource("chat-secret"),
		},
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
			transcode.FeatureMetadata:               {},
			transcode.FeatureParallelToolCalls:      {},
		}}}

	for _, order := range []string{"chat-first", "gemini-first"} {
		t.Run(order, func(t *testing.T) {
			gemMapping := messagesGeminiMapping()
			gemMapping.Mapping.ModelMap = transcode.ModelMap{
				AllowIdentity: false,
				Exact: map[string]transcode.ModelMapping{
					"fake-gemini": {UpstreamModel: "gemini-2.5-flash", Via: transcode.NativeGemini},
				},
			}
			// The chat mapping must also resolve the gemini model (identity
			// fallback is off on the gemini side, so a via-less request falls
			// to chat).
			chatMapping.Mapping.ModelMap = transcode.ModelMap{AllowIdentity: true}

			var mappings []TranscodeMapping
			if order == "chat-first" {
				mappings = []TranscodeMapping{chatMapping, gemMapping}
			} else {
				mappings = []TranscodeMapping{gemMapping, chatMapping}
			}

			// Two proxies, one per upstream: the dispatcher picks the mapping,
			// but the upstream URL is per-proxy, so run the gemini model
			// against the gemini-backed proxy and the chat model against the
			// chat-backed proxy, both with identical mapping sets.
			gemProxy := newGeminiUpstreamProxy(t, geminiStub.url(t), mappings...)
			chatProxy, err := New(
				WithUpstream(chatStub.url(t)),
				WithMatcher(route.NewMatcher(route.DefaultPatterns())),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(metrics.NewCollector()),
				WithTranscodeMapping(mappings...),
			)
			if err != nil {
				t.Fatalf("New chat proxy: %v", err)
			}
			gemFront := httptest.NewServer(gemProxy)
			t.Cleanup(gemFront.Close)
			chatFront := httptest.NewServer(chatProxy)
			t.Cleanup(chatFront.Close)

			post := func(front, model string) string {
				req, err := http.NewRequest(http.MethodPost, front+"/v1/messages",
					strings.NewReader(`{"model":"`+model+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("model %q: status %d, body %s", model, resp.StatusCode, body)
				}
				return string(body)
			}

			// via=gemini model goes to the gemini-backed proxy's gemini mapping.
			if got := post(gemFront.URL, "fake-gemini"); !strings.Contains(got, "hello") {
				t.Fatalf("via=gemini model did not reach the gemini upstream: %s", got)
			}
			// A via-less model goes to chat on the chat-backed proxy.
			if got := post(chatFront.URL, "anything"); !strings.Contains(got, "from chat") {
				t.Fatalf("via-less model did not reach the chat upstream: %s", got)
			}
		})
	}
}

// TestGeminiUpstreamResponsesOwnDialect404 proves that a /v1/responses route
// carrying both a chat and a gemini mapping returns the own-dialect 404 for a
// via=responses model, with byte-identical bodies in both registration orders.
func TestGeminiUpstreamResponsesOwnDialect404(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, geminiOKResponse, nil
	})
	responsesMapping := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth: transcode.AuthPolicy{
			Mode:   transcode.AuthBearer,
			Secret: auth.NewStaticSecretSource("chat-secret"),
		},
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
			transcode.FeatureParallelToolCalls:      {},
		}}}
	gemMapping := responsesGeminiMapping()
	gemMapping.Mapping.ModelMap = transcode.ModelMap{
		AllowIdentity: false,
		Exact: map[string]transcode.ModelMapping{
			"fake-gemini": {UpstreamModel: "gemini-2.5-flash", Via: transcode.NativeGemini},
			"fake-native": {UpstreamModel: "native-model", Via: transcode.NativeResponses},
		},
	}

	bodies := map[string]string{}
	for _, order := range []string{"chat-first", "gemini-first"} {
		var mappings []TranscodeMapping
		if order == "chat-first" {
			mappings = []TranscodeMapping{responsesMapping, gemMapping}
		} else {
			mappings = []TranscodeMapping{gemMapping, responsesMapping}
		}
		p := newGeminiUpstreamProxy(t, stub.url(t), mappings...)
		front := httptest.NewServer(p)
		t.Cleanup(front.Close)

		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
			strings.NewReader(`{"model":"fake-native","input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("order %s: status = %d, want 404; body %s", order, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "served natively as responses") {
			t.Fatalf("order %s: body = %s, want the own-dialect refusal", order, body)
		}
		if prev, ok := bodies["fake-native"]; ok && prev != string(body) {
			t.Fatalf("refusal bytes differ across registration orders:\n%s\nvs\n%s", prev, body)
		}
		bodies["fake-native"] = string(body)

		// The via=gemini model still reaches the gemini mapping in both orders.
		req2, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
			strings.NewReader(`{"model":"fake-gemini","input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		req2.Header.Set("Content-Type", "application/json")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("order %s: via=gemini model status = %d, body %s", order, resp2.StatusCode, body2)
		}
	}
}

// TestGeminiUpstreamContinuityReplay proves that with a continuity store, a
// follow-up R2G request carrying previous_response_id replays the stored turns
// to the Gemini upstream.
func TestGeminiUpstreamContinuityReplay(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, geminiOKResponse, nil
	})
	continuity := transcode.NewContinuityStore(transcode.ContinuityConfig{})
	mapping := responsesGeminiMapping()
	mapping.Continuity = continuity
	mapping.ContinuityKey = "testprov"
	p := newGeminiUpstreamProxy(t, stub.url(t), mapping)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	post := func(payload string) map[string]any {
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("response not JSON: %v\n%s", err, body)
		}
		return doc
	}

	first := post(`{"model":"gemini-2.5-flash","input":"first question"}`)
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("first response has no id: %v", first)
	}
	second := post(`{"model":"gemini-2.5-flash","input":"second question","previous_response_id":"` + id + `"}`)
	if second["status"] != "completed" {
		t.Fatalf("second response = %v", second)
	}

	<-stub.mu
	secondBody := stub.bodies[len(stub.bodies)-1]
	stub.mu <- struct{}{}
	// The replayed history must contain the first question as a user content
	// ahead of the second question.
	if !strings.Contains(secondBody, "first question") || !strings.Contains(secondBody, "second question") {
		t.Fatalf("second request does not replay the stored turns: %s", secondBody)
	}
	if strings.Index(secondBody, "first question") > strings.Index(secondBody, "second question") {
		t.Fatalf("replayed turn is out of order: %s", secondBody)
	}
}

// TestGeminiUpstreamStrictDefaultsRefuseInjectedLosses proves that under a
// strict loss policy (no metadata/parallel_tool_calls approval) an M2G request
// carrying metadata.user_id and an R2G request carrying parallel_tool_calls
// are refused with 400, while the default policy serves both.
func TestGeminiUpstreamStrictDefaultsRefuseInjectedLosses(t *testing.T) {
	stub := newGeminiUpstreamStub(t, func(n int, r *http.Request, body []byte) (int, string, http.Header) {
		return http.StatusOK, geminiOKResponse, nil
	})

	permissiveMapping := func() TranscodeMapping {
		m := messagesGeminiMapping()
		// newGeminiUpstreamProxy adds the defaults; emulate the default
		// (non-strict) behavior by allowing metadata and parallel_tool_calls.
		return m
	}
	strictMapping := func(route transcode.RouteKey, client transcode.ClientProtocol) TranscodeMapping {
		m := TranscodeMapping{
			ClientRoute:        route,
			ClientProtocol:     client,
			UpstreamProtocol:   transcode.UpstreamGemini,
			UpstreamPath:       "/v1beta",
			ModelMap:           transcode.ModelMap{AllowIdentity: true},
			AllowedClientQuery: map[string]struct{}{"beta": {}},
			// Strict: metadata and parallel_tool_calls are NOT approved.
			LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
				transcode.FeatureUsageCacheReadUnknown:  {},
				transcode.FeatureUsageCacheWriteUnknown: {},
				transcode.FeatureUsageReasoningUnknown:  {},
				transcode.FeatureUsageUnknown:           {},
				transcode.FeatureGeminiClientQuery:      {},
				transcode.FeatureResponseServiceTier:    {},
			}},
			Auth: transcode.AuthPolicy{
				Mode:   transcode.AuthGoogAPIKey,
				Secret: auth.NewStaticSecretSource("gemini-secret"),
			}}
		return m
	}

	post := func(t *testing.T, p *Proxy, path, payload string) (int, string) {
		t.Helper()
		front := httptest.NewServer(p)
		t.Cleanup(front.Close)
		req, err := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Default (non-strict) policy: both requests succeed.
	pDefault := newGeminiUpstreamProxy(t, stub.url(t), permissiveMapping(), responsesGeminiMapping())
	if code, body := post(t, pDefault, "/v1/messages",
		`{"model":"gemini-2.5-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u-1"}}`); code != http.StatusOK {
		t.Fatalf("default policy M2G+metadata: status %d, body %s", code, body)
	}
	if code, body := post(t, pDefault, "/v1/responses",
		`{"model":"gemini-2.5-flash","input":"hi","parallel_tool_calls":false}`); code != http.StatusOK {
		t.Fatalf("default policy R2G+parallel_tool_calls: status %d, body %s", code, body)
	}

	// Strict policy: both are 400s naming the refused feature.
	pStrict := newGeminiUpstreamProxyStrict(t, stub.url(t), true,
		strictMapping(transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}, transcode.ClientMessages),
		strictMapping(transcode.RouteKey{Method: http.MethodPost, Path: "/v1/responses"}, transcode.ClientResponses),
	)
	if code, body := post(t, pStrict, "/v1/messages",
		`{"model":"gemini-2.5-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u-1"}}`); code != http.StatusBadRequest || !strings.Contains(body, "metadata") {
		t.Fatalf("strict policy M2G+metadata: status %d, body %s", code, body)
	}
	if code, body := post(t, pStrict, "/v1/responses",
		`{"model":"gemini-2.5-flash","input":"hi","parallel_tool_calls":false}`); code != http.StatusBadRequest || !strings.Contains(body, "parallel_tool_calls") {
		t.Fatalf("strict policy R2G+parallel_tool_calls: status %d, body %s", code, body)
	}
}
