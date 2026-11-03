// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// nativeGeminiUpstream is a recording fake Gemini upstream: it captures the
// raw request (path, query, auth header, body bytes) and answers with a
// scripted JSON or SSE body.
type nativeGeminiUpstream struct {
	mu       sync.Mutex
	path     string
	rawQuery string
	apiKey   string
	body     []byte
	jsonBody string
	sseBody  string
}

func (u *nativeGeminiUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.path = r.URL.Path
	u.rawQuery = r.URL.RawQuery
	u.apiKey = r.Header.Get("X-Goog-Api-Key")
	u.body = body
	u.mu.Unlock()
	if u.sseBody != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(u.sseBody))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(u.jsonBody))
}

func (u *nativeGeminiUpstream) got() (path, rawQuery, apiKey string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.rawQuery, u.apiKey, u.body
}

// nativeGeminiModelMap maps the client model fake-gemini to the wire model
// wire-gemini with via=gemini, plus a chat-native model for the fallthrough
// test.
func nativeGeminiModelMap() transcode.ModelMap {
	return transcode.ModelMap{
		Exact: map[string]transcode.ModelMapping{
			"fake-gemini": {
				ClientModel:         "fake-gemini",
				UpstreamModel:       "wire-gemini",
				ClientResponseModel: "fake-gemini",
				Via:                 transcode.NativeGemini,
			},
			"chat-model": {
				ClientModel:         "chat-model",
				UpstreamModel:       "wire-chat",
				ClientResponseModel: "chat-model",
				Via:                 transcode.NativeChat,
			},
		},
		AllowIdentity:      false,
		RequireExplicitMap: true,
	}
}

// geminiNativeRouteKey is the canonical template route key for the test
// native Gemini route.
func geminiNativeRouteKey(t *testing.T) transcode.RouteKey {
	t.Helper()
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1beta/models/{model}:generateContent")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// newNativeGeminiProxy builds a proxy with a native Gemini route (and any
// extra transcode mappings) against the fake upstream, with goog-api-key
// provider auth and a journal.
func newNativeGeminiProxy(t *testing.T, up *nativeGeminiUpstream, j *journal.Journal, mappings ...TranscodeMapping) (*Proxy, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	opts := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithAuthPolicy(&auth.AuthPolicy{
			Mode:   auth.AuthGoogAPIKey,
			Secret: auth.NewStaticSecretSource("gemini-secret"),
		}),
		WithNativeRoutes(NativeRoute{
			RouteKey: geminiNativeRouteKey(t),
			Protocol: transcode.NativeGemini,
			Provider: "testprov",
			ModelMap: nativeGeminiModelMap(),
		}),
	}
	if j != nil {
		opts = append(opts, WithJournal(j))
	}
	if len(mappings) > 0 {
		opts = append(opts, WithTranscodeMapping(mappings...))
	}
	p, err := New(opts...)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, srv
}

// postGemini issues a POST against the proxy and returns the recorder.
func postGemini(t *testing.T, p *Proxy, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", "client-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestNativeGeminiPassthroughJSON proves a natively served Gemini
// generateContent request reaches the upstream with only the model segment
// of the path rewritten: the query, headers, and body (minus an optional
// body model member) are byte-identical, auth is adapted to goog-api-key,
// and the JSON response — including modelVersion — is byte-identical.
func TestNativeGeminiPassthroughJSON(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}],"modelVersion":"wire-gemini","responseId":"r1"}`}
	j := journal.New(512, 1<<20)
	p, _ := newNativeGeminiProxy(t, up, j)

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"unknownProviderField":{"x":1},"generationConfig":{"temperature":0.2}}`
	rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent?alt=json&beta=true", reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	path, rawQuery, apiKey, body := up.got()
	if path != "/v1beta/models/wire-gemini:generateContent" {
		t.Fatalf("upstream path = %q, want /v1beta/models/wire-gemini:generateContent", path)
	}
	if rawQuery != "alt=json&beta=true" {
		t.Fatalf("upstream query = %q, want alt=json&beta=true", rawQuery)
	}
	if apiKey != "gemini-secret" {
		t.Fatalf("X-Goog-Api-Key = %q, want the provider secret gemini-secret (client key stripped)", apiKey)
	}
	if string(body) != reqBody {
		t.Fatalf("body = %s, want byte-identical %s", body, reqBody)
	}
	// The response is byte-identical: modelVersion is never rewritten.
	if rec.Body.String() != up.jsonBody {
		t.Fatalf("response = %s, want byte-identical %s", rec.Body.String(), up.jsonBody)
	}
	// The journal keeps the client-facing alias path.
	entries := j.Entries()
	if len(entries) == 0 {
		t.Fatal("no journal entries")
	}
	if got := entries[len(entries)-1].URL.Path; got != "/v1beta/models/fake-gemini:generateContent" {
		t.Fatalf("journal path = %q, want /v1beta/models/fake-gemini:generateContent", got)
	}
}

// TestNativeGeminiPassthroughBodyModel proves a body model naming the path
// model (with the models/ prefix) is rewritten to the wire model in the same
// spelling form, and nothing else in the body changes.
func TestNativeGeminiPassthroughBodyModel(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{"candidates":[],"modelVersion":"wire-gemini"}`}
	p, _ := newNativeGeminiProxy(t, up, nil)

	reqBody := `{"model":"models/fake-gemini","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent", reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	_, _, _, body := up.got()
	want := `{"model":"models/wire-gemini","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	if string(body) != want {
		t.Fatalf("body = %s, want %s", body, want)
	}

	// A conflicting body model is a 400.
	rec = postGemini(t, p, "/v1beta/models/fake-gemini:generateContent", `{"model":"other-model"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("conflicting body model status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestNativeGeminiPassthroughSSE proves the streaming operation is served
// natively with alt=sse preserved and the SSE body unbuffered/byte-identical.
func TestNativeGeminiPassthroughSSE(t *testing.T) {
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"index\":0}]}\n\n" +
		"data: {\"candidates\":[{\"finishReason\":\"STOP\",\"index\":0}],\"modelVersion\":\"wire-gemini\"}\n\n"
	up := &nativeGeminiUpstream{sseBody: sse}
	p, _ := newNativeGeminiProxy(t, up, nil)

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rec := postGemini(t, p, "/v1beta/models/fake-gemini:streamGenerateContent?alt=sse", reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want SSE", ct)
	}
	if rec.Body.String() != sse {
		t.Fatalf("SSE body = %q, want byte-identical %q", rec.Body.String(), sse)
	}
	path, rawQuery, _, body := up.got()
	if path != "/v1beta/models/wire-gemini:streamGenerateContent" {
		t.Fatalf("upstream path = %q, want the stream action with the wire model", path)
	}
	if rawQuery != "alt=sse" {
		t.Fatalf("upstream query = %q, want alt=sse", rawQuery)
	}
	if string(body) != reqBody {
		t.Fatalf("body = %s, want byte-identical %s", body, reqBody)
	}
}

// TestNativeGeminiQueryCredentialRefused proves a credential in the query
// string is a 400 before the body is read.
func TestNativeGeminiQueryCredentialRefused(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{}`}
	p, _ := newNativeGeminiProxy(t, up, nil)

	// The upstream must not be touched at all. The scan runs over the raw
	// query, so malformed percent-encoding (which url.ParseQuery would drop)
	// is refused too. (';' is not a pair separator in a query per RFC 3986,
	// so x=y;key=secret is a single parameter named "x" — it is not a
	// credential.)
	for _, q := range []string{
		"key=secret", "access_token=secret", "oauth_token=secret", "KEY=secret",
		"key=secret%zz", "key%3D=secret",
	} {
		rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent?"+q, `{"contents":[]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400: %s", q, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "query string") {
			t.Fatalf("query %q: body = %s, want a query-credential refusal", q, rec.Body.String())
		}
	}
	if path, _, _, _ := up.got(); path != "" {
		t.Fatalf("upstream was touched (path %q) despite a query credential", path)
	}
}

// TestNativeGeminiConflictErrorBounded proves the body/path model-conflict
// 400 is bounded by ErrorMessageBytes even when the conflicting body model
// is huge.
func TestNativeGeminiConflictErrorBounded(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{}`}
	p, _ := newNativeGeminiProxy(t, up, nil)

	big := strings.Repeat("A", 64*1024)
	rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent",
		`{"model":"`+big+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %.200s", rec.Code, rec.Body.String())
	}
	// The message is bounded at ErrorMessageBytes: the 64KiB client model is
	// truncated with the ellipsis marker, never reflected whole.
	if !strings.Contains(rec.Body.String(), "…") {
		t.Fatalf("error body lacks the truncation marker, len = %d: %.200s", len(rec.Body.String()), rec.Body.String())
	}
	if len(rec.Body.String()) > 8*1024 {
		t.Fatalf("error body = %d bytes, want it bounded well under the 64KiB model", len(rec.Body.String()))
	}
}

// TestNativeGeminiEmptyBodyModelIsAbsent proves a present-but-empty body
// model is treated as absent (matching the transcoded Gemini path): the
// request is served natively and the empty model member is forwarded
// untouched.
func TestNativeGeminiEmptyBodyModelIsAbsent(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{"candidates":[],"modelVersion":"wire-gemini"}`}
	p, _ := newNativeGeminiProxy(t, up, nil)

	reqBody := `{"model":"","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent", reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, _, _, body := up.got(); string(body) != reqBody {
		t.Fatalf("body = %s, want byte-identical (empty model left as-is) %s", body, reqBody)
	}
}

// TestNativeGeminiDecodedBodyBound proves the decoded-body limit applies to
// the final forwarded body even when the body carries no model member, with
// AcceptedRequestBytes larger than DecodedRequestBytes.
func TestNativeGeminiDecodedBodyBound(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{}`}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	limits := transcode.BodyLimits{
		AcceptedRequestBytes: 1 << 20, // 1 MiB accepted
		DecodedRequestBytes:  1 << 10, // 1 KiB decoded
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(NativeRoute{
			RouteKey:   geminiNativeRouteKey(t),
			Protocol:   transcode.NativeGemini,
			Provider:   "testprov",
			ModelMap:   nativeGeminiModelMap(),
			BodyLimits: limits,
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 2 KiB, no model member: within the accepted bound, over the decoded bound.
	body := `{"contents":[{"role":"user","parts":[{"text":"` + strings.Repeat("x", 2048) + `"}]}]}`
	rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if path, _, _, _ := up.got(); path != "" {
		t.Fatalf("upstream was touched (path %q) despite the decoded-bound refusal", path)
	}
}

// TestNativeGeminiRouteRejectsUnsafeUpstreamModel proves a via=gemini
// mapping whose upstream model is not a bare model id fails route
// validation at startup.
func TestNativeGeminiRouteRejectsUnsafeUpstreamModel(t *testing.T) {
	for _, wire := range []string{"models/wire-gemini", "foo/bar", "foo:bar", "foo?bar", "foo#bar", ""} {
		mm := nativeGeminiModelMap()
		mm.Exact["fake-gemini"] = transcode.ModelMapping{
			ClientModel:   "fake-gemini",
			UpstreamModel: wire,
			Via:           transcode.NativeGemini,
		}
		nr := NativeRoute{
			RouteKey: geminiNativeRouteKey(t),
			Protocol: transcode.NativeGemini,
			ModelMap: mm,
		}
		if err := nr.Validate(); err == nil {
			t.Fatalf("upstream model %q accepted, want rejection", wire)
		}
	}
}

// TestNativeGeminiViaChatFallsThroughToConversion proves a model whose
// via dialect is chat on a native Gemini route that also carries a G2C
// mapping falls through to the conversion instead of serving natively.
func TestNativeGeminiViaChatFallsThroughToConversion(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	g2c := TranscodeMapping{
		ClientRoute:      geminiNativeRouteKey(t),
		ClientProtocol:   transcode.ClientGemini,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureMetadata:                          {},
			transcode.FeatureParallelToolCalls:                 {},
			transcode.FeatureGeminiClientQuery:                 {},
			transcode.FeatureGeminiThoughtSignaturePlaceholder: {},
			transcode.FeatureGeminiGenerationControls:          {},
			transcode.FeatureGeminiSafetySettings:              {},
			transcode.FeatureGeminiRequestControls:             {},
			transcode.FeatureGeminiThoughtSignature:            {},
			transcode.FeatureGeminiPartMetadata:                {},
			transcode.FeatureGeminiToolStrictness:              {},
			transcode.FeatureGeminiImageDetail:                 {},
			transcode.FeatureUsageUnknown:                      {},
			transcode.FeatureUsageCacheReadUnknown:             {},
			transcode.FeatureUsageCacheWriteUnknown:            {},
			transcode.FeatureUsageReasoningUnknown:             {},
		}},
		ModelMap: nativeGeminiModelMap(),
		Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone},
	}
	p, _ := newNativeGeminiProxy(t, up, nil, g2c)

	rec := postGemini(t, p, "/v1beta/models/chat-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (conversion): %s", rec.Code, rec.Body.String())
	}
	path, _, _, _ := up.got()
	if path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want the chat conversion path /v1/chat/completions", path)
	}
}

// TestNativeGeminiMultiMappingBothOrders proves a native Gemini route plus
// G2C and G2R mappings dispatches correctly in both registration orders:
// via=gemini serves natively, via=chat converts to chat, via=responses
// converts to responses, and the refusal bodies are registration-order
// independent.
func TestNativeGeminiMultiMappingBothOrders(t *testing.T) {
	modelMap := nativeGeminiModelMap()
	modelMap.Exact["resp-model"] = transcode.ModelMapping{
		ClientModel:         "resp-model",
		UpstreamModel:       "wire-resp",
		ClientResponseModel: "resp-model",
		Via:                 transcode.NativeResponses,
	}

	losses := func() transcode.LossPolicy {
		return transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureToolSchemaStrictness:              {},
			transcode.FeatureMetadata:                          {},
			transcode.FeatureParallelToolCalls:                 {},
			transcode.FeatureGeminiClientQuery:                 {},
			transcode.FeatureGeminiThoughtSignaturePlaceholder: {},
			transcode.FeatureGeminiGenerationControls:          {},
			transcode.FeatureGeminiSafetySettings:              {},
			transcode.FeatureGeminiRequestControls:             {},
			transcode.FeatureGeminiThoughtSignature:            {},
			transcode.FeatureGeminiPartMetadata:                {},
			transcode.FeatureGeminiToolStrictness:              {},
			transcode.FeatureGeminiImageDetail:                 {},
			transcode.FeatureUsageUnknown:                      {},
			transcode.FeatureUsageCacheReadUnknown:             {},
			transcode.FeatureUsageCacheWriteUnknown:            {},
			transcode.FeatureUsageReasoningUnknown:             {},
		}}
	}
	g2c := func() TranscodeMapping {
		return TranscodeMapping{
			ClientRoute:      geminiNativeRouteKey(t),
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			LossPolicy:       losses(),
			ModelMap:         modelMap,
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}
	}
	g2r := func() TranscodeMapping {
		return TranscodeMapping{
			ClientRoute:      geminiNativeRouteKey(t),
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			LossPolicy:       losses(),
			ModelMap:         modelMap,
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}
	}

	run := func(t *testing.T, mappings []TranscodeMapping) (nativeOK, chatOK, respOK bool, nativeBody string) {
		up := &nativeGeminiUpstream{jsonBody: `{"candidates":[{"content":{"parts":[{"text":"n"}]},"index":0}],"modelVersion":"wire-gemini"}`}
		p, err := New(
			WithUpstream(mustParseURL(t, httptestServerURL(t, up))),
			WithMatcher(route.NewMatcher(route.DefaultPatterns())),
			WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
			WithMetrics(metrics.NewCollector()),
			WithNativeRoutes(NativeRoute{
				RouteKey: geminiNativeRouteKey(t),
				Protocol: transcode.NativeGemini,
				Provider: "testprov",
				ModelMap: modelMap,
			}),
			WithTranscodeMapping(mappings...),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// via=gemini: native passthrough.
		rec := postGemini(t, p, "/v1beta/models/fake-gemini:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		nativeOK = rec.Code == http.StatusOK
		nativeBody = rec.Body.String()
		path, _, _, _ := up.got()
		if path != "/v1beta/models/wire-gemini:generateContent" {
			t.Fatalf("native dispatch went to %q", path)
		}
		// via=chat: conversion to chat.
		up.jsonBody = `{"id":"c1","object":"chat.completion","created":1,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"c"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		rec = postGemini(t, p, "/v1beta/models/chat-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		chatOK = rec.Code == http.StatusOK
		if path, _, _, _ := up.got(); chatOK && path != "/v1/chat/completions" {
			t.Fatalf("chat dispatch went to %q", path)
		}
		// via=responses: conversion to responses.
		up.jsonBody = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"wire-resp","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"r","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		rec = postGemini(t, p, "/v1beta/models/resp-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		respOK = rec.Code == http.StatusOK
		if path, _, _, _ := up.got(); respOK && path != "/v1/responses" {
			t.Fatalf("responses dispatch went to %q", path)
		}
		return nativeOK, chatOK, respOK, nativeBody
	}

	n1, c1, r1, b1 := run(t, []TranscodeMapping{g2c(), g2r()})
	n2, c2, r2, b2 := run(t, []TranscodeMapping{g2r(), g2c()})
	if !n1 || !c1 || !r1 {
		t.Fatalf("order [g2c g2r]: native=%v chat=%v responses=%v", n1, c1, r1)
	}
	if !n2 || !c2 || !r2 {
		t.Fatalf("order [g2r g2c]: native=%v chat=%v responses=%v", n2, c2, r2)
	}
	if b1 != b2 {
		t.Fatalf("native response differs by registration order:\n%s\n%s", b1, b2)
	}
}

// TestNativeGeminiNonPostPassesThrough proves a non-POST method on a Gemini
// operation path is transparent passthrough, not native serving.
func TestNativeGeminiNonPostPassesThrough(t *testing.T) {
	up := &nativeGeminiUpstream{jsonBody: `{"models":[{"name":"models/fake-gemini"}]}`}
	p, _ := newNativeGeminiProxy(t, up, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1beta/models/fake-gemini:generateContent", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	path, _, apiKey, _ := up.got()
	// The client model is forwarded verbatim on a transparent passthrough.
	if path != "/v1beta/models/fake-gemini:generateContent" {
		t.Fatalf("path = %q, want the client path unrewritten", path)
	}
	_ = apiKey
}

// TestNativeGeminiRouteValidation proves Validate accepts the canonical
// Gemini template and rejects malformed ones.
func TestNativeGeminiRouteValidation(t *testing.T) {
	good := NativeRoute{
		RouteKey: geminiNativeRouteKey(t),
		Protocol: transcode.NativeGemini,
		ModelMap: transcode.ModelMap{AllowIdentity: true},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("canonical gemini route rejected: %v", err)
	}
	badPaths := []string{
		"/v1beta/models/{model}:streamGenerateContent",
		"/v1beta/{model}:generateContent",
		"/v1beta/models/{model}:generateContent/extra",
		"/v1beta/models/{model}",
		"/models/{model}:generateContent:extra",
	}
	for _, path := range badPaths {
		key, err := transcode.NewRouteKey(http.MethodPost, path)
		if err != nil {
			continue // NewRouteKey itself rejects; fine.
		}
		nr := NativeRoute{RouteKey: key, Protocol: transcode.NativeGemini, ModelMap: transcode.ModelMap{AllowIdentity: true}}
		if err := nr.Validate(); err == nil {
			t.Fatalf("gemini route path %q accepted, want rejection", path)
		}
	}
}

// httptestServerURL is a tiny helper so the both-orders test can build a
// fresh upstream per sub-run. mustParseURL already exists in proxy_test.go.
func httptestServerURL(t *testing.T, up *nativeGeminiUpstream) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	return srv.URL
}
