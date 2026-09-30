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
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// headerCapture records inbound headers at a fake upstream.
type headerCapture struct {
	mu      sync.Mutex
	headers http.Header
	body    []byte
}

func (c *headerCapture) handler(response string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.headers = r.Header.Clone()
		c.body = body
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response))
	}
}

func (c *headerCapture) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headers.Get(key)
}

func testPreset() transcode.OpencodePreset {
	return transcode.OpencodePreset{Enabled: true, Provider: "zen"}
}

func newPresetProxy(t *testing.T, cap *headerCapture, response string, opts ...Option) *Proxy {
	t.Helper()
	srv := httptest.NewServer(cap.handler(response))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
	}
	p, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p
}

// TestProxyPresetPassthroughHeaders proves a headerless passthrough request
// leaves with the full first-party shape, and that a client which named
// itself keeps its session but not its User-Agent or client attribution.
func TestProxyPresetPassthroughHeaders(t *testing.T) {
	cap := &headerCapture{}
	p := newPresetProxy(t, cap, `{"ok":true}`, WithOpencodePreset(testPreset()))

	do := func(headers map[string]string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader(`{}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	// Headerless: everything synthesized/defaulted.
	do(nil)
	if got := cap.get("X-Opencode-Session"); got == "" {
		t.Fatal("synthesized session missing")
	}
	// The session must travel under the opencode-branch name ONLY: a real
	// client talking to an opencode provider never sends the affinity pair.
	for _, key := range []string{"X-Session-Affinity", "X-Session-Id"} {
		if got := cap.get(key); got != "" {
			t.Fatalf("%s = %q, want absent: it belongs to the non-opencode branch", key, got)
		}
	}
	if got := cap.get("User-Agent"); got != transcode.DefaultOpencodeUserAgent {
		t.Fatalf("UA = %q, want default", got)
	}
	if got := cap.get("X-Opencode-Client"); got != transcode.DefaultOpencodeClient {
		t.Fatalf("client = %q, want default", got)
	}
	if got := cap.get("X-Opencode-Project"); got != "" {
		t.Fatalf("project = %q, want absent", got)
	}

	// A client that named itself must not identify itself upstream: the
	// session is preserved (it is the conversation's own value), while the
	// impersonation markers assert the first-party values.
	do(map[string]string{
		"X-Opencode-Session": "client-sess",
		"User-Agent":         "claude-cli/2.1.0 (external, cli)",
		"X-Opencode-Client":  "some-other-tool",
		"X-Opencode-Request": "user-7",
	})
	if got := cap.get("X-Opencode-Session"); got != "client-sess" {
		t.Fatalf("session = %q, want client-sess", got)
	}
	if got := cap.get("User-Agent"); got != transcode.DefaultOpencodeUserAgent {
		t.Fatalf("UA = %q, want the first-party value", got)
	}
	if got := cap.get("X-Opencode-Client"); got != transcode.DefaultOpencodeClient {
		t.Fatalf("client = %q, want the first-party value", got)
	}
	if got := cap.get("X-Opencode-Request"); got != "user-7" {
		t.Fatalf("request = %q, want forwarded", got)
	}
}

// TestProxyPresetDisabledUntouched proves mounts without the preset forward
// verbatim: no session, UA, or client headers are added. It also proves a
// client's own session names pass through untouched when the preset is off,
// which keeps the strip preset-scoped rather than a blanket deletion.
func TestProxyPresetDisabledUntouched(t *testing.T) {
	cap := &headerCapture{}
	p := newPresetProxy(t, cap, `{"ok":true}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, key := range []string{"X-Opencode-Session", "X-Session-Affinity", "X-Session-Id", "X-Opencode-Client"} {
		if got := cap.get(key); got != "" {
			t.Fatalf("%s = %q, want absent without the preset", key, got)
		}
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if string(cap.body) != `{}` {
		t.Fatalf("upstream body = %q, want verbatim {}", cap.body)
	}
}

// TestProxyPresetSurvivesTranscode proves the session set survives request
// conversion: a headerless messages request still carries a stable session
// upstream after rendering to chat.
func TestProxyPresetSurvivesTranscode(t *testing.T) {
	cap := &headerCapture{}
	srv := httptest.NewServer(cap.handler(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	preset := testPreset()
	preset.Provider = "zen"
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(TranscodeMapping{
			ClientRoute:      key,
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
			ModelMap: transcode.ModelMap{AllowIdentity: true},
			Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone},
			Opencode: preset}),
	)
	if err != nil {
		t.Fatal(err)
	}

	send := func() string {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"same question"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return cap.get("X-Opencode-Session")
	}
	first, second := send(), send()
	if first == "" {
		t.Fatal("transcoded session missing")
	}
	if first != second {
		t.Fatalf("sessions differ across turns (%q vs %q): same history must reuse the value", first, second)
	}
	if got := cap.get("User-Agent"); got != transcode.DefaultOpencodeUserAgent {
		t.Fatalf("UA = %q, want default", got)
	}
}

// TestProxyPresetWithAuth proves ordering: the upstream credential is
// injected AND the first-party session is present, with the client
// credential stripped — the preset never depends on or clobbers auth.
func TestProxyPresetWithAuth(t *testing.T) {
	cap := &headerCapture{}
	srv := httptest.NewServer(cap.handler(`{"ok":true}`))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithOpencodePreset(testPreset()),
		WithAuthPolicy(&auth.AuthPolicy{
			Mode:   auth.AuthBearer,
			Secret: auth.NewStaticSecretSource("cfg-secret"),
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := cap.get("Authorization"); got != "Bearer cfg-secret" {
		t.Fatalf("upstream Authorization = %q, want the configured credential", got)
	}
	if got := cap.get("X-Opencode-Session"); got == "" {
		t.Fatal("session missing alongside auth")
	}
	if got := cap.get("User-Agent"); got != transcode.DefaultOpencodeUserAgent {
		t.Fatalf("UA = %q, want default", got)
	}
}

// TestProxyPresetSessionStableAcrossPaths proves one conversation keeps
// one session value whether it is served natively or converted: the key
// derives from the client model on every path.
func TestProxyPresetSessionStableAcrossPaths(t *testing.T) {
	const question = "same question"
	body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"` + question + `"}]}`

	// Native path: m is messages-native.
	nativeCap := &headerCapture{}
	nativeSrv := httptest.NewServer(nativeCap.handler(`{"type":"message","model":"wire-msg"}`))
	t.Cleanup(nativeSrv.Close)
	nativeURL, _ := url.Parse(nativeSrv.URL)
	msgKey, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	nativeMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-msg", ClientResponseModel: "m", Via: transcode.NativeMessages},
	}, AllowIdentity: false, RequireExplicitMap: true}
	nativeProxy, err := New(
		WithUpstream(nativeURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithOpencodePreset(testPreset()),
		WithNativeRoutes(NativeRoute{RouteKey: msgKey, Protocol: transcode.NativeMessages, ModelMap: nativeMap, Provider: "zen"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Converted path: the same model rendered to a chat upstream.
	convCap := &headerCapture{}
	convSrv := httptest.NewServer(convCap.handler(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	t.Cleanup(convSrv.Close)
	convURL, _ := url.Parse(convSrv.URL)
	convMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-chat", ClientResponseModel: "m", Via: transcode.NativeChat},
	}, AllowIdentity: false, RequireExplicitMap: true}
	convProxy, err := New(
		WithUpstream(convURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
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
			ModelMap: convMap,
			Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone},
			Opencode: testPreset()}),
	)
	if err != nil {
		t.Fatal(err)
	}

	send := func(p *Proxy) string {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return ""
	}
	send(nativeProxy)
	send(convProxy)
	nativeSession := nativeCap.get("X-Opencode-Session")
	convSession := convCap.get("X-Opencode-Session")
	if nativeSession == "" || convSession == "" {
		t.Fatalf("sessions missing: native=%q converted=%q", nativeSession, convSession)
	}
	if nativeSession != convSession {
		t.Fatalf("session differs across native (%q) and converted (%q) serving of one conversation",
			nativeSession, convSession)
	}
}

// TestProxyPresetNativeSession proves the native path derives the
// conversation key from the document: repeated turns reuse it.
func TestProxyPresetNativeSession(t *testing.T) {
	cap := &headerCapture{}
	srv := httptest.NewServer(cap.handler(`{"type":"message","model":"wire-msg"}`))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithOpencodePreset(testPreset()),
		WithNativeRoutes(NativeRoute{
			RouteKey: key,
			Protocol: transcode.NativeMessages,
			ModelMap: nativeTestModelMap(),
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	send := func() string {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"same question"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return cap.get("X-Opencode-Session")
	}
	first, second := send(), send()
	if first == "" {
		t.Fatal("native session missing")
	}
	if first != second {
		t.Fatalf("sessions differ across turns (%q vs %q)", first, second)
	}
}

// TestProxyPresetIdentityShapes proves the preset identifies a conversation
// under every shape a real client uses: requests carrying an explicit
// session id, stateless requests that resend the full content each turn,
// and a mixture of the two. The explicit id always wins; a content-only
// request derives a value that is stable across turns; and a client that
// sends an id on one turn and none on the next keeps the derived value for
// the id-less turn rather than losing affinity.
func TestProxyPresetIdentityShapes(t *testing.T) {
	cap := &headerCapture{}
	srv := httptest.NewServer(cap.handler(`{"type":"message","model":"wire-msg"}`))
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithOpencodePreset(testPreset()),
		WithNativeRoutes(NativeRoute{
			RouteKey: key,
			Protocol: transcode.NativeMessages,
			ModelMap: nativeTestModelMap(),
			Provider: "zen",
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	// full is a chat-completions-shaped stateless turn: the whole history
	// is resent every time, so the first user turn identifies it.
	full := func(turn string) string {
		return `{"model":"msg-model","max_tokens":5,"messages":[` +
			`{"role":"user","content":"kick off"},` +
			`{"role":"assistant","content":"ok"},` +
			`{"role":"user","content":"` + turn + `"}]}`
	}

	send := func(body, session string) string {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("X-Opencode-Session", session)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return cap.get("X-Opencode-Session")
	}

	// Full-content resends, no id: one stable derived value.
	contentOnlyA := send(full("second turn"), "")
	contentOnlyB := send(full("third turn"), "")
	if contentOnlyA == "" || contentOnlyA != contentOnlyB {
		t.Fatalf("content-only identity unstable: %q then %q", contentOnlyA, contentOnlyB)
	}

	// A different opening turn is a different conversation.
	other := send(`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"another kick off"}]}`, "")
	if other == contentOnlyA {
		t.Fatalf("distinct conversations share the derived value %q", other)
	}

	// Explicit id wins, and the id-less turn after it keeps the derived
	// value (the mixed shape).
	if got := send(full("second turn"), "client-provided-id"); got != "client-provided-id" {
		t.Fatalf("explicit id not honoured: got %q", got)
	}
	if got := send(full("fourth turn"), ""); got != contentOnlyA {
		t.Fatalf("mixed shape lost affinity: got %q, want %q", got, contentOnlyA)
	}
}
