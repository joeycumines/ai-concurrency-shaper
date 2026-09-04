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
	"fmt"
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

// TestProxy_TranscodeAuthInheritanceAndOverride verifies that:
// 1. A transcoded route with no explicit auth inherits the proxy/provider-level auth policy.
// 2. A transcoded route with an explicit auth policy overrides the provider-level auth policy.
// 3. A transcoded route with AuthNone strips client credentials and injects nothing.
// 4. Inbound client credentials are scrubbed on both transcoded and passthrough routes when upstream auth is active.
func TestProxy_TranscodeAuthInheritanceAndOverride(t *testing.T) {
	var (
		mu              sync.Mutex
		lastAuthHeaders = make(map[string]http.Header)
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastAuthHeaders[r.URL.Path] = r.Header.Clone()
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"chat response"},"finish_reason":"stop"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","created_at":1700000000,"model":"m","status":"completed","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"responses response"}]}]}`))
		default:
			fmt.Fprintf(w, `{"unmapped":true,"path":%q}`, r.URL.Path)
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	rKeyInherit, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	rKeyOverride, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	rKeyNone, err := transcode.NewRouteKey(http.MethodPost, "/v1/none")
	if err != nil {
		t.Fatal(err)
	}

	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureToolSchemaStrictness:   {},
	}}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithAuthPolicy(&auth.AuthPolicy{
			Mode:   auth.AuthBearer,
			Secret: auth.NewStaticSecretSource("provider-secret-key"),
		}),
		WithTranscodeMapping(
			// Route 1: No explicit auth (zero value) -> must inherit provider AuthBearer "provider-secret-key"
			TranscodeMapping{
				Mapping: transcode.Mapping{
					ClientRoute:      rKeyInherit,
					ClientProtocol:   transcode.ClientResponses,
					UpstreamProtocol: transcode.UpstreamChatCompletions,
					UpstreamPath:     "/v1/chat/completions",
					LossPolicy:       lossPolicy,
					ModelMap:         transcode.ModelMap{AllowIdentity: true},
					// Auth left zero to trigger inheritance
				},
			},
			// Route 2: Explicit AuthXAPIKey "route-override-key" -> must override provider policy
			TranscodeMapping{
				ClientRoute:      rKeyOverride,
				ClientProtocol:   transcode.ClientMessages,
				UpstreamProtocol: transcode.UpstreamResponses,
				UpstreamPath:     "/v1/responses",
				LossPolicy:       lossPolicy,
				ModelMap:         transcode.ModelMap{AllowIdentity: true},
				Auth: transcode.AuthPolicy{
					Mode:             transcode.AuthXAPIKey,
					Secret:           auth.NewStaticSecretSource("route-override-key"),
					AnthropicVersion: "2023-06-01",
				},
			},
			// Route 3: Explicit AuthNone -> must strip client credentials and inject nothing
			TranscodeMapping{
				ClientRoute:      rKeyNone,
				ClientProtocol:   transcode.ClientResponses,
				UpstreamProtocol: transcode.UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
				LossPolicy:       lossPolicy,
				ModelMap:         transcode.ModelMap{AllowIdentity: true},
				Auth: transcode.AuthPolicy{
					Mode: transcode.AuthNone,
				},
			},
		),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// 1. Test Inherited Route: POST /v1/responses
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-secret-should-be-scrubbed")
		req.Header.Set("X-Api-Key", "client-x-api-key-should-be-scrubbed")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/responses: status %d, body: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		hdr := lastAuthHeaders["/v1/chat/completions"]
		mu.Unlock()

		if got := hdr.Get("Authorization"); got != "Bearer provider-secret-key" {
			t.Errorf("inherited route Authorization = %q, want Bearer provider-secret-key", got)
		}
		if got := hdr.Get("X-Api-Key"); got != "" {
			t.Errorf("inherited route X-Api-Key = %q, want empty (scrubbed)", got)
		}
	}

	// 2. Test Override Route: POST /v1/messages
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-secret-should-be-scrubbed")
		req.Header.Set("Anthropic-Version", "2020-01-01")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/messages: status %d, body: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		hdr := lastAuthHeaders["/v1/responses"]
		mu.Unlock()

		if got := hdr.Get("X-Api-Key"); got != "route-override-key" {
			t.Errorf("override route X-Api-Key = %q, want route-override-key", got)
		}
		if got := hdr.Get("Authorization"); got != "" {
			t.Errorf("override route Authorization = %q, want empty (scrubbed)", got)
		}
	}

	// 3. Test AuthNone Route: POST /v1/none
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/none", strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-secret-should-be-scrubbed")
		req.Header.Set("X-Api-Key", "client-x-api-key-should-be-scrubbed")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/none: status %d, body: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		hdr := lastAuthHeaders["/v1/chat/completions"]
		mu.Unlock()

		if got := hdr.Get("Authorization"); got != "" {
			t.Errorf("AuthNone route Authorization = %q, want empty", got)
		}
		if got := hdr.Get("X-Api-Key"); got != "" {
			t.Errorf("AuthNone route X-Api-Key = %q, want empty", got)
		}
	}

	// 4. Test Passthrough Route: POST /unmapped/path
	{
		req := httptest.NewRequest(http.MethodPost, "/unmapped/path", strings.NewReader(`{"data":123}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-secret-should-be-scrubbed")
		req.Header.Set("X-Api-Key", "client-x-api-key-should-be-scrubbed")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST /unmapped/path: status %d, body: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		hdr := lastAuthHeaders["/unmapped/path"]
		mu.Unlock()

		if got := hdr.Get("Authorization"); got != "Bearer provider-secret-key" {
			t.Errorf("passthrough route Authorization = %q, want Bearer provider-secret-key", got)
		}
		if got := hdr.Get("X-Api-Key"); got != "" {
			t.Errorf("passthrough route X-Api-Key = %q, want empty", got)
		}
	}
}

// TestProxy_TranscodeAuthUnprotectedProvider verifies that when a provider has
// no auth policy (nil), a transcoded route with no auth policy strips client
// credentials and forwards without authentication, while passthrough routes
// forward client credentials verbatim.
func TestProxy_TranscodeAuthUnprotectedProvider(t *testing.T) {
	var (
		mu              sync.Mutex
		lastAuthHeaders = make(map[string]http.Header)
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastAuthHeaders[r.URL.Path] = r.Header.Clone()
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/chat/completions" {
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"chat response"},"finish_reason":"stop"}]}`))
			return
		}
		fmt.Fprintf(w, `{"unmapped":true,"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	rKey, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}

	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
		transcode.FeatureUsageUnknown:           {},
	}}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		// No provider auth policy (nil)
		WithTranscodeMapping(
			TranscodeMapping{
				Mapping: transcode.Mapping{
					ClientRoute:      rKey,
					ClientProtocol:   transcode.ClientResponses,
					UpstreamProtocol: transcode.UpstreamChatCompletions,
					UpstreamPath:     "/v1/chat/completions",
					LossPolicy:       lossPolicy,
					ModelMap:         transcode.ModelMap{AllowIdentity: true},
					// Zero Auth
				},
			},
		),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// 1. Transcoded route: client credentials must still be scrubbed, reaching upstream unauthenticated
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-secret")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/responses: status %d, body: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		hdr := lastAuthHeaders["/v1/chat/completions"]
		mu.Unlock()

		if got := hdr.Get("Authorization"); got != "" {
			t.Errorf("unprotected transcoded route Authorization = %q, want empty (scrubbed)", got)
		}
	}

	// 2. Passthrough route: client credentials pass through verbatim when provider auth is disabled
	{
		req := httptest.NewRequest(http.MethodPost, "/passthrough", strings.NewReader(`{"data":123}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-verbatim-key")

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("POST /passthrough: status %d, body: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		hdr := lastAuthHeaders["/passthrough"]
		mu.Unlock()

		if got := hdr.Get("Authorization"); got != "Bearer client-verbatim-key" {
			t.Errorf("unprotected passthrough route Authorization = %q, want Bearer client-verbatim-key", got)
		}
	}
}
