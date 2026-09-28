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
// leaves with the full first-party shape, while a client-supplied session
// and User-Agent survive verbatim.
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
	first := cap.get("X-Opencode-Session")
	if got := cap.get("X-Session-Affinity"); got != first {
		t.Fatalf("affinity = %q, want the session value %q", got, first)
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

	// Client-supplied identity preserved verbatim.
	do(map[string]string{
		"X-Opencode-Session": "client-sess",
		"User-Agent":         "opencode/prod/9.9.9/opencode",
		"X-Opencode-Request": "user-7",
	})
	if got := cap.get("X-Opencode-Session"); got != "client-sess" {
		t.Fatalf("session = %q, want client-sess", got)
	}
	if got := cap.get("User-Agent"); got != "opencode/prod/9.9.9/opencode" {
		t.Fatalf("UA = %q, want inbound preserved", got)
	}
	if got := cap.get("X-Opencode-Request"); got != "user-7" {
		t.Fatalf("request = %q, want forwarded", got)
	}
}

// TestProxyPresetDisabledUntouched proves mounts without the preset forward
// verbatim: no session, UA, or client headers are added.
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
		WithTranscodeMapping(TranscodeMapping{Mapping: transcode.Mapping{
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
			Opencode: preset,
		}}),
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
