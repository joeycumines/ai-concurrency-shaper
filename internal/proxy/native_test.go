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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// nativeUpstream is a recording fake upstream for native-route tests.
type nativeUpstream struct {
	mu       sync.Mutex
	path     string
	model    string
	session  string
	response string
	sse      string
}

func (u *nativeUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	u.mu.Lock()
	u.path = r.URL.Path
	u.model = probe.Model
	u.session = r.Header.Get("x-opencode-session")
	u.mu.Unlock()
	if u.sse != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(u.sse))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(u.response))
}

func (u *nativeUpstream) got() (path, model, session string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.model, u.session
}

func nativeTestModelMap() transcode.ModelMap {
	return transcode.ModelMap{
		Exact: map[string]transcode.ModelMapping{
			"msg-model": {
				ClientModel:         "msg-model",
				UpstreamModel:       "wire-msg",
				ClientResponseModel: "msg-model",
				Via:                 transcode.NativeMessages,
			},
			"resp-model": {
				ClientModel:         "resp-model",
				UpstreamModel:       "wire-resp",
				ClientResponseModel: "resp-model",
				Via:                 transcode.NativeResponses,
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

func newNativeProxy(t *testing.T, up *nativeUpstream, routes ...NativeRoute) (*Proxy, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	for i := range routes {
		if routes[i].ModelMap.Exact == nil {
			routes[i].ModelMap = nativeTestModelMap()
		}
	}
	opts := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(routes...),
	}
	p, err := New(opts...)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, srv
}

func nativeRoute(t *testing.T, protocol transcode.NativeProtocol, path string) NativeRoute {
	t.Helper()
	key, err := transcode.NewRouteKey(http.MethodPost, path)
	if err != nil {
		t.Fatal(err)
	}
	return NativeRoute{RouteKey: key, Protocol: protocol, ModelMap: nativeTestModelMap()}
}

func postNative(t *testing.T, p *Proxy, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestProxyNativeMessagesRewrite proves a messages-native model is
// forwarded on its native path with the wire model, and the client-facing
// alias is restored on the non-streaming response.
func TestProxyNativeMessagesRewrite(t *testing.T) {
	up := &nativeUpstream{response: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"wire-msg","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	rec := postNative(t, p, "/v1/messages",
		`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, model, _ := up.got(); path != "/v1/messages" || model != "wire-msg" {
		t.Fatalf("upstream path=%q model=%q, want /v1/messages wire-msg", path, model)
	}
	var downstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &downstream); err != nil {
		t.Fatalf("downstream body: %v", err)
	}
	if downstream.Model != "msg-model" {
		t.Fatalf("downstream model = %q, want msg-model", downstream.Model)
	}
}

// TestNativeRouteValidateRejectsUnknownViaDialect proves the defence-in-depth
// guard on the mapping dialects a route decides against. The configured paths
// cannot produce one, so only a library caller can; catching it at startup
// beats leaving every request for that model to fail closed at request time.
func TestNativeRouteValidateRejectsUnknownViaDialect(t *testing.T) {
	route := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		Protocol: transcode.NativeMessages,
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"weird": {
				ClientModel:         "weird",
				UpstreamModel:       "wire-weird",
				ClientResponseModel: "weird",
				Via:                 transcode.NativeProtocol("bogus"),
			},
		}},
	}
	if err := route.Validate(); err == nil {
		t.Fatal("Validate accepted a mapping with an unknown Via dialect")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error = %v, want it to name the offending dialect", err)
	}

	// The empty Via stays legal: it is what marks a dialect unknown, and
	// those requests must fall through rather than fail startup.
	route.ModelMap.Exact["weird"] = transcode.ModelMapping{
		ClientModel:         "weird",
		UpstreamModel:       "wire-weird",
		ClientResponseModel: "weird",
	}
	if err := route.Validate(); err != nil {
		t.Fatalf("Validate rejected an empty (unknown) Via: %v", err)
	}
}

// TestProxyNativeResponsesRewrite proves the same contract on the
// responses dialect.
func TestProxyNativeResponsesRewrite(t *testing.T) {
	up := &nativeUpstream{response: `{"id":"resp_1","object":"response","model":"wire-resp","status":"completed","output":[]}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeResponses, "/v1/responses"))

	rec := postNative(t, p, "/v1/responses", `{"model":"resp-model","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, model, _ := up.got(); path != "/v1/responses" || model != "wire-resp" {
		t.Fatalf("upstream path=%q model=%q, want /v1/responses wire-resp", path, model)
	}
	var downstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &downstream); err != nil {
		t.Fatalf("downstream body: %v", err)
	}
	if downstream.Model != "resp-model" {
		t.Fatalf("downstream model = %q, want resp-model", downstream.Model)
	}
}

// TestProxyNativeChatRewrite proves chat-native models route on
// /v1/chat/completions with only the model identifier rewritten.
func TestProxyNativeChatRewrite(t *testing.T) {
	up := &nativeUpstream{response: `{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeChat, "/v1/chat/completions"))

	rec := postNative(t, p, "/v1/chat/completions",
		`{"model":"chat-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, model, _ := up.got(); path != "/v1/chat/completions" || model != "wire-chat" {
		t.Fatalf("upstream path=%q model=%q, want /v1/chat/completions wire-chat", path, model)
	}
	var downstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &downstream); err != nil {
		t.Fatalf("downstream body: %v", err)
	}
	if downstream.Model != "chat-model" {
		t.Fatalf("downstream model = %q, want chat-model", downstream.Model)
	}
}

// TestProxyNativeKnownViaMismatchIsLocal404 proves a known model sent to
// the wrong native dialect fails closed locally naming its native dialect,
// instead of reaching the upstream with a surrogate it does not know.
func TestProxyNativeKnownViaMismatchIsLocal404(t *testing.T) {
	up := &nativeUpstream{response: `{}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	rec := postNative(t, p, "/v1/messages",
		`{"model":"chat-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "natively served as chat") {
		t.Fatalf("body = %s, want it to name the chat native dialect as the model's own", rec.Body.String())
	}
	if path, _, _ := up.got(); path != "" {
		t.Fatalf("upstream reached at %q, want no upstream contact", path)
	}
}

// TestProxyNativeUnknownViaFallsThrough proves identity-resolved models
// (unknown dialect) keep legacy transparent behavior: forwarded verbatim.
func TestProxyNativeUnknownViaFallsThrough(t *testing.T) {
	up := &nativeUpstream{response: `{"type":"message","model":"any-model"}`}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(NativeRoute{
			RouteKey: key,
			Protocol: transcode.NativeMessages,
			ModelMap: transcode.ModelMap{AllowIdentity: true},
		}),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	rec := postNative(t, p, "/v1/messages",
		`{"model":"any-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, model, _ := up.got(); path != "/v1/messages" || model != "any-model" {
		t.Fatalf("upstream path=%q model=%q, want verbatim /v1/messages any-model", path, model)
	}
}

// TestProxyNativeRouteValidation proves malformed native declarations fail
// at construction, while a native route may SHARE its client route with a
// transcode mapping (native-first dispatch, conversion on dialect
// mismatch).
func TestProxyNativeRouteValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	base := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
	}

	msgKey, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	native := NativeRoute{RouteKey: msgKey, Protocol: transcode.NativeMessages, ModelMap: nativeTestModelMap()}
	transcodeMapping := TranscodeMapping{
		ClientRoute:      msgKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone}}

	if _, err := New(append(base, WithNativeRoutes(native), WithTranscodeMapping(transcodeMapping))...); err != nil {
		t.Fatalf("native route sharing a client route with a transcode mapping: %v", err)
	}
	if _, err := New(append(base, WithNativeRoutes(native, native))...); err == nil {
		t.Fatal("duplicate native route: want construction error, got nil")
	}
	getKey, _ := transcode.NewRouteKey(http.MethodGet, "/v1/messages")
	if _, err := New(append(base, WithNativeRoutes(NativeRoute{
		RouteKey: getKey, Protocol: transcode.NativeMessages, ModelMap: nativeTestModelMap(),
	}))...); err == nil {
		t.Fatal("non-POST native route: want construction error, got nil")
	}
	if _, err := New(append(base, WithNativeRoutes(NativeRoute{
		RouteKey: msgKey, Protocol: "bogus", ModelMap: nativeTestModelMap(),
	}))...); err == nil {
		t.Fatal("unknown native protocol: want construction error, got nil")
	}
}

// TestProxyNativeFirstThenTranscode proves one path serves both: a model
// whose dialect matches the native route is forwarded natively, while a
// different-dialect model on the same path falls through to the transcode
// mapping.
func TestProxyNativeFirstThenTranscode(t *testing.T) {
	up := &nativeUpstream{response: `{"type":"message","model":"wire-msg"}`}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)

	// messages-native and chat-native models in one map; the native route
	// covers /v1/messages, the mapping converts the chat-native model.
	shared := nativeTestModelMap()
	msgKey, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(NativeRoute{
			RouteKey: msgKey, Protocol: transcode.NativeMessages, ModelMap: shared,
		}),
		WithTranscodeMapping(TranscodeMapping{
			ClientRoute:      msgKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
				transcode.FeatureUsageCacheReadUnknown:  {},
				transcode.FeatureUsageCacheWriteUnknown: {},
				transcode.FeatureUsageReasoningUnknown:  {},
				transcode.FeatureUsageUnknown:           {},
				transcode.FeatureRequestReasoning:       {},
				transcode.FeatureDeveloperRole:          {},
			}},
			ModelMap: shared,
			Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone}}),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Native: messages-native model.
	rec := postNative(t, p, "/v1/messages",
		`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("native status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, model, _ := up.got(); path != "/v1/messages" || model != "wire-msg" {
		t.Fatalf("native path=%q model=%q, want /v1/messages wire-msg", path, model)
	}

	// Converted: chat-native model on the same path falls through.
	up.mu.Lock()
	up.response = `{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	up.mu.Unlock()
	rec = postNative(t, p, "/v1/messages",
		`{"model":"chat-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("converted status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if path, model, _ := up.got(); path != "/v1/chat/completions" || model != "wire-chat" {
		t.Fatalf("converted path=%q model=%q, want /v1/chat/completions wire-chat", path, model)
	}
}

// TestProxyNativeForwardsServableControls proves in-dialect controls that
// the conversion loss policy would drop (top_k, background) pass through
// to the native upstream that serves them.
func TestProxyNativeForwardsServableControls(t *testing.T) {
	var mu sync.Mutex
	var gotTopK *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			TopK *int `json:"top_k"`
		}
		_ = json.Unmarshal(body, &probe)
		mu.Lock()
		gotTopK = probe.TopK
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","model":"wire-msg"}`))
	}))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(nativeRoute(t, transcode.NativeMessages, "/v1/messages")),
	)
	if err != nil {
		t.Fatal(err)
	}

	rec := postNative(t, p, "/v1/messages",
		`{"model":"msg-model","max_tokens":5,"top_k":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotTopK == nil || *gotTopK != 5 {
		t.Fatalf("upstream top_k = %v, want 5", gotTopK)
	}

	up2srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Background *bool  `json:"background"`
			Model      string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Background == nil || !*probe.Background {
			t.Errorf("upstream background = %v, want true", probe.Background)
		}
		if probe.Model != "wire-resp" {
			t.Errorf("upstream model = %q, want wire-resp", probe.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"response","model":"wire-resp"}`))
	}))
	t.Cleanup(up2srv.Close)
	up2URL, _ := url.Parse(up2srv.URL)
	p2, err := New(
		WithUpstream(up2URL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(nativeRoute(t, transcode.NativeResponses, "/v1/responses")),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec = postNative(t, p2, "/v1/responses", `{"model":"resp-model","input":"hi","background":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("responses status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestProxyNativeForwardsDocumentedOptionalFields proves the native path
// does not import the conversion contract's field coverage: documented
// optional in-dialect fields the pinned shadows do not model, and a
// Responses function tool without an explicit strict, still reach the
// upstream.
func TestProxyNativeForwardsDocumentedOptionalFields(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","model":"wire-msg"}`))
	}))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(nativeRoute(t, transcode.NativeMessages, "/v1/messages")),
	)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"msg-model","max_tokens":5,"service_tier":"auto","container":null,` +
		`"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"hi"}]}`
	rec := postNative(t, p, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := string(gotBody)
	mu.Unlock()
	for _, want := range []string{`"service_tier":"auto"`, `"cache_control":{"type":"ephemeral"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("upstream body lost %s: %s", want, got)
		}
	}
	if want := `"model":"wire-msg"`; !strings.Contains(got, want) {
		t.Fatalf("upstream body missing the wire model: %s", got)
	}
	// Every byte outside the model value is preserved exactly.
	if want := strings.Replace(body, `"model":"msg-model"`, `"model":"wire-msg"`, 1); got != want {
		t.Fatalf("body not byte-preserving:\n got %s\nwant %s", got, want)
	}

	// Responses function tool without an explicit strict.
	up2 := &nativeUpstream{response: `{"object":"response","model":"wire-resp"}`}
	p2, _ := newNativeProxy(t, up2, nativeRoute(t, transcode.NativeResponses, "/v1/responses"))
	rec = postNative(t, p2, "/v1/responses",
		`{"model":"resp-model","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("responses status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestProxyNativeRejectsStructuralCorruption proves the always-reject set
// still holds on the native path: duplicate keys, trailing values, and
// malformed syntax are local 400s that never reach the upstream.
func TestProxyNativeRejectsStructuralCorruption(t *testing.T) {
	up := &nativeUpstream{response: `{"type":"message","model":"wire-msg"}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	cases := []struct {
		name string
		body string
	}{
		{"duplicate top-level key", `{"model":"msg-model","max_tokens":5,"max_tokens":6,"messages":[]}`},
		{"duplicate nested key", `{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi","content":"ho"}]}`},
		{"trailing value", `{"model":"msg-model","max_tokens":5,"messages":[]}{}`},
		{"malformed", `{"model":`},
		{"non-object", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postNative(t, p, "/v1/messages", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if path, _, _ := up.got(); path != "" {
				t.Fatalf("upstream reached at %q, want no contact", path)
			}
		})
	}
}

// TestProxyNativeForwardsQueryAndHeaders proves the native path keeps the
// transparent contract: client query and non-credential headers reach the
// upstream verbatim.
func TestProxyNativeForwardsQueryAndHeaders(t *testing.T) {
	var mu sync.Mutex
	var gotBeta, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBeta = r.URL.Query().Get("beta")
		gotVersion = r.Header.Get("anthropic-version")
		mu.Unlock()
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Model != "wire-msg" {
			t.Errorf("upstream model = %q, want wire-msg", probe.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","model":"wire-msg"}`))
	}))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(nativeRoute(t, transcode.NativeMessages, "/v1/messages")),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true",
		bytes.NewBufferString(`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotBeta != "true" || gotVersion != "2023-06-01" {
		t.Fatalf("upstream beta=%q version=%q, want true/2023-06-01", gotBeta, gotVersion)
	}
}
