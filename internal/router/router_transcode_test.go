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

package router_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// helperResponsesToChatMapping returns a valid Responses->Chat transcode.Mapping.
func helperResponsesToChatMapping(t *testing.T) transcode.Mapping {
	t.Helper()
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	return transcode.Mapping{
		ClientRoute:      key,
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
			transcode.FeatureUsageUnknown:           {},
		}},
		ModelMap: transcode.ModelMap{AllowIdentity: true},
		Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone},
	}
}

// TestRouter_MultiProviderTranscodeHandlerIsolation proves that two providers
// with overlapping client routes (identical POST /v1/responses) register and
// serve correctly in the Router->Proxy->TranscodeHandler composition:
// 1. Each mount delegates to its own provider's Proxy and TranscodeHandler.
// 2. A request to one mount never reaches the other provider's upstream.
// 3. Non-POST methods on a mapped path pass through transparently without conversion.
func TestRouter_MultiProviderTranscodeHandlerIsolation(t *testing.T) {
	var countA, countB atomic.Int64
	var lastPathA, lastPathB atomic.Value

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countA.Add(1)
		lastPathA.Store(r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-a","object":"chat.completion","created":1700000000,"model":"m-a","choices":[{"index":0,"message":{"role":"assistant","content":"hello from openai upstream"},"finish_reason":"stop"}]}`))
			return
		}
		// Passthrough path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"passthrough_a":true}`))
	}))
	t.Cleanup(upstreamA.Close)

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countB.Add(1)
		lastPathB.Store(r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-b","object":"chat.completion","created":1700000000,"model":"m-b","choices":[{"index":0,"message":{"role":"assistant","content":"hello from anthropic upstream"},"finish_reason":"stop"}]}`))
			return
		}
		// Passthrough path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"passthrough_b":true}`))
	}))
	t.Cleanup(upstreamB.Close)

	uA, err := url.Parse(upstreamA.URL)
	if err != nil {
		t.Fatal(err)
	}
	uB, err := url.Parse(upstreamB.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxyA, err := proxy.New(
		proxy.WithUpstream(uA),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: helperResponsesToChatMapping(t)}),
	)
	if err != nil {
		t.Fatalf("proxyA New: %v", err)
	}

	proxyB, err := proxy.New(
		proxy.WithUpstream(uB),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: helperResponsesToChatMapping(t)}),
	)
	if err != nil {
		t.Fatalf("proxyB New: %v", err)
	}

	r, err := router.New([]router.Provider{
		{Name: "openai", Prefix: "/openai", Proxy: proxyA},
		{Name: "anthropic", Prefix: "/anthropic", Proxy: proxyB},
	})
	if err != nil {
		t.Fatalf("router New: %v", err)
	}

	client := &http.Client{}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	// 1. Send POST /openai/v1/responses -> should route to proxyA, transcode to /v1/chat/completions on upstreamA
	{
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			srv.URL+"/openai/v1/responses",
			strings.NewReader(`{"model":"m-a","input":"hello"}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /openai/v1/responses: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /openai/v1/responses status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "hello from openai upstream") || !strings.Contains(bodyStr, `"object":"response"`) {
			t.Fatalf("POST /openai/v1/responses body unexpected: %s", bodyStr)
		}
		if countA.Load() != 1 {
			t.Errorf("upstreamA request count = %d, want 1", countA.Load())
		}
		if countB.Load() != 0 {
			t.Errorf("upstreamB request count = %d, want 0", countB.Load())
		}
		if p, ok := lastPathA.Load().(string); !ok || p != "/v1/chat/completions" {
			t.Errorf("upstreamA received path %q, want /v1/chat/completions", p)
		}
	}

	// 2. Send POST /anthropic/v1/responses -> should route to proxyB, transcode to /v1/chat/completions on upstreamB
	{
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			srv.URL+"/anthropic/v1/responses",
			strings.NewReader(`{"model":"m-b","input":"hello"}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /anthropic/v1/responses: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /anthropic/v1/responses status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "hello from anthropic upstream") || !strings.Contains(bodyStr, `"object":"response"`) {
			t.Fatalf("POST /anthropic/v1/responses body unexpected: %s", bodyStr)
		}
		if countA.Load() != 1 {
			t.Errorf("upstreamA request count = %d, want 1", countA.Load())
		}
		if countB.Load() != 1 {
			t.Errorf("upstreamB request count = %d, want 1", countB.Load())
		}
		if p, ok := lastPathB.Load().(string); !ok || p != "/v1/chat/completions" {
			t.Errorf("upstreamB received path %q, want /v1/chat/completions", p)
		}
	}

	// 3. Send GET /openai/v1/responses -> method-scoped dispatch: non-POST passes through transparently
	{
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			srv.URL+"/openai/v1/responses",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /openai/v1/responses: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /openai/v1/responses status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"passthrough_a":true`) {
			t.Fatalf("GET /openai/v1/responses body unexpected: %s", bodyStr)
		}
		if p, ok := lastPathA.Load().(string); !ok || p != "/v1/responses" {
			t.Errorf("upstreamA received path %q, want /v1/responses (untranscoded passthrough)", p)
		}
	}
}

// TestRouter_BareVsPrefixedTrailingSlashParity pins the F-6/H5 class behavior:
//   - Under a bare mount (Prefix=""), the router delegates r.URL.Path unnormalized
//     so POST /v1/responses matches the RouteKey, while POST /v1/responses/ does not.
//   - Under a prefixed mount (Prefix="/p"), joinSegments normalizes /p/v1/responses/
//     to /v1/responses, so it matches the RouteKey.
func TestRouter_BareVsPrefixedTrailingSlashParity(t *testing.T) {
	var bareUpstreamPath, prefixedUpstreamPath atomic.Value

	bareUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bareUpstreamPath.Store(r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"bare"},"finish_reason":"stop"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"passthrough":true}`))
	}))
	t.Cleanup(bareUpstream.Close)

	prefixedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefixedUpstreamPath.Store(r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"prefixed"},"finish_reason":"stop"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"passthrough":true}`))
	}))
	t.Cleanup(prefixedUpstream.Close)

	uBare, _ := url.Parse(bareUpstream.URL)
	uPrefixed, _ := url.Parse(prefixedUpstream.URL)

	proxyBare, err := proxy.New(
		proxy.WithUpstream(uBare),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: helperResponsesToChatMapping(t)}),
	)
	if err != nil {
		t.Fatal(err)
	}

	proxyPrefixed, err := proxy.New(
		proxy.WithUpstream(uPrefixed),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: helperResponsesToChatMapping(t)}),
	)
	if err != nil {
		t.Fatal(err)
	}

	rBare, err := router.New([]router.Provider{{Name: "bare", Prefix: "", Proxy: proxyBare}})
	if err != nil {
		t.Fatal(err)
	}

	rPrefixed, err := router.New([]router.Provider{{Name: "prefixed", Prefix: "/p", Proxy: proxyPrefixed}})
	if err != nil {
		t.Fatal(err)
	}

	srvBare := httptest.NewServer(rBare)
	t.Cleanup(srvBare.Close)

	srvPrefixed := httptest.NewServer(rPrefixed)
	t.Cleanup(srvPrefixed.Close)

	client := &http.Client{}

	// Bare mount with exact path -> transcoded
	{
		resp, err := client.Post(srvBare.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"object":"response"`) {
			t.Fatalf("bare exact path not transcoded: %s", body)
		}
		if bareUpstreamPath.Load().(string) != "/v1/chat/completions" {
			t.Errorf("bare exact upstream path = %v, want /v1/chat/completions", bareUpstreamPath.Load())
		}
	}

	// Bare mount with trailing slash /v1/responses/ -> normalized in router to /v1/responses -> transcoded (F-6/H5 parity)
	{
		resp, err := client.Post(srvBare.URL+"/v1/responses/", "application/json", strings.NewReader(`{"model":"m","input":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"object":"response"`) {
			t.Fatalf("bare trailing slash path not transcoded: %s", body)
		}
		if bareUpstreamPath.Load().(string) != "/v1/chat/completions" {
			t.Errorf("bare trailing slash upstream path = %v, want /v1/chat/completions", bareUpstreamPath.Load())
		}
	}

	// Prefixed mount with trailing slash /p/v1/responses/ -> joinSegments normalizes to /v1/responses -> transcoded
	{
		resp, err := client.Post(srvPrefixed.URL+"/p/v1/responses/", "application/json", strings.NewReader(`{"model":"m","input":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"object":"response"`) {
			t.Fatalf("prefixed trailing slash not transcoded: %s", body)
		}
		if prefixedUpstreamPath.Load().(string) != "/v1/chat/completions" {
			t.Errorf("prefixed trailing slash upstream path = %v, want /v1/chat/completions", prefixedUpstreamPath.Load())
		}
	}

	// Unmapped path on bare mount -> transparent passthrough
	{
		resp, err := client.Post(srvBare.URL+"/v1/other/", "application/json", strings.NewReader(`{"other":true}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"passthrough":true`) {
			t.Fatalf("bare unmapped path did not passthrough: %s", body)
		}
	}

	// Unmapped path on prefixed mount -> transparent passthrough
	{
		resp, err := client.Post(srvPrefixed.URL+"/p/v1/other/", "application/json", strings.NewReader(`{"other":true}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"passthrough":true`) {
			t.Fatalf("prefixed unmapped path did not passthrough: %s", body)
		}
	}
}

// TestRouter_Transcode_HeaderAllowlisting_PerRoute proves that header allowlisting
// is per-route and never forwards client credentials or arbitrary headers across
// providers (G5).
func TestRouter_Transcode_HeaderAllowlisting_PerRoute(t *testing.T) {
	var upstreamHeader atomic.Value

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeader.Store(r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	mapping := helperResponsesToChatMapping(t)
	mapping.Auth = transcode.AuthPolicy{
		Mode:   transcode.AuthBearer,
		Secret: auth.NewStaticSecretSource("upstream-injected-token"),
	}

	p, err := proxy.New(
		proxy.WithUpstream(u),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: mapping}),
	)
	if err != nil {
		t.Fatalf("proxy New: %v", err)
	}

	r, err := router.New([]router.Provider{{Name: "p1", Prefix: "/p1", Proxy: p}})
	if err != nil {
		t.Fatalf("router New: %v", err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		srv.URL+"/p1/v1/responses",
		strings.NewReader(`{"model":"m","input":"hello"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-leak-token")
	req.Header.Set("Cookie", "session=client-leak-cookie")
	req.Header.Set("X-Custom-Header", "client-leak-custom")
	req.Header.Set("X-Test", "test-value")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	hdr, ok := upstreamHeader.Load().(http.Header)
	if !ok || hdr == nil {
		t.Fatal("upstream never received header")
	}

	// 1. Injected upstream auth is present
	if got := hdr.Get("Authorization"); got != "Bearer upstream-injected-token" {
		t.Errorf("Authorization = %q, want Bearer upstream-injected-token", got)
	}

	// 2. Client headers are completely scrubbed
	for _, leaked := range []string{"Cookie", "X-Custom-Header", "X-Test"} {
		if got := hdr.Get(leaked); got != "" {
			t.Errorf("header %s leaked to upstream: %q", leaked, got)
		}
	}

	// 3. Expected allowlisted headers are present
	if got := hdr.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
