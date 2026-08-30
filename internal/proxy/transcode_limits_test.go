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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestProxy_Transcode_PropagatedRetryMaxBodyMB_Success verifies that a proxy
// with retries enabled and multiple transcoded routes having matching
// RetryReplayBytes initializes cleanly and proxies successfully (H2).
func TestProxy_Transcode_PropagatedRetryMaxBodyMB_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"chat res"},"finish_reason":"stop"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","created_at":1700000000,"model":"m","status":"completed","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"resp res"}]}]}`))
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	rKey1, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	rKey2, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
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
		WithMaxRetries(3),
		WithMaxBodyBytes(2 << 20),
		WithTranscodeMapping(
			TranscodeMapping{
				Mapping: transcode.Mapping{
					ClientRoute:      rKey1,
					ClientProtocol:   transcode.ClientResponses,
					UpstreamProtocol: transcode.UpstreamChatCompletions,
					UpstreamPath:     "/v1/chat/completions",
					LossPolicy:       lossPolicy,
					ModelMap:         transcode.ModelMap{AllowIdentity: true},
					Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
				},
				BodyLimits: transcode.BodyLimits{
					RetryReplayBytes: 2 << 20,
				},
			},
			TranscodeMapping{
				Mapping: transcode.Mapping{
					ClientRoute:      rKey2,
					ClientProtocol:   transcode.ClientMessages,
					UpstreamProtocol: transcode.UpstreamResponses,
					UpstreamPath:     "/v1/responses",
					LossPolicy:       lossPolicy,
					ModelMap:         transcode.ModelMap{AllowIdentity: true},
					Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
				},
				BodyLimits: transcode.BodyLimits{
					RetryReplayBytes: 2 << 20,
				},
			},
		),
	)
	if err != nil {
		t.Fatalf("proxy.New failed unexpectedly: %v", err)
	}

	// Make a request to each transcoded route
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/responses status %d: %s", rec.Code, rec.Body.String())
		}
	}
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/messages status %d: %s", rec.Code, rec.Body.String())
		}
	}
}

// TestProxy_Transcode_RetryReplayBytes_MismatchRejected verifies that an explicit
// per-route RetryReplayBytes that disagrees with the provider's maxBodyBytes fails
// closed at startup naming the mismatch (H2).
func TestProxy_Transcode_RetryReplayBytes_MismatchRejected(t *testing.T) {
	upstreamURL, _ := url.Parse("http://127.0.0.1:8080")
	rKey, _ := transcode.NewRouteKey(http.MethodPost, "/v1/responses")

	_, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithMaxRetries(3),
		WithMaxBodyBytes(5 << 20), // 5 MiB = 5242880
		WithTranscodeMapping(
			TranscodeMapping{
				Mapping: transcode.Mapping{
					ClientRoute:      rKey,
					ClientProtocol:   transcode.ClientResponses,
					UpstreamProtocol: transcode.UpstreamChatCompletions,
					UpstreamPath:     "/v1/chat/completions",
					ModelMap:         transcode.ModelMap{AllowIdentity: true},
					Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
				},
				BodyLimits: transcode.BodyLimits{
					RetryReplayBytes: 10 << 20, // 10 MiB != 5 MiB
				},
			},
		),
	)
	if err == nil {
		t.Fatal("expected mismatch rejection")
	}
	if !strings.Contains(err.Error(), "RetryReplayBytes=10485760 must equal the proxy retry body cap 5242880") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestProxy_Transcode_RetryReplayBytes_DisabledRetriesRejected verifies that a
// route declaring non-zero RetryReplayBytes when retries are disabled fails closed.
func TestProxy_Transcode_RetryReplayBytes_DisabledRetriesRejected(t *testing.T) {
	upstreamURL, _ := url.Parse("http://127.0.0.1:8080")
	rKey, _ := transcode.NewRouteKey(http.MethodPost, "/v1/responses")

	_, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithMaxRetries(0), // retries disabled
		WithTranscodeMapping(
			TranscodeMapping{
				Mapping: transcode.Mapping{
					ClientRoute:      rKey,
					ClientProtocol:   transcode.ClientResponses,
					UpstreamProtocol: transcode.UpstreamChatCompletions,
					UpstreamPath:     "/v1/chat/completions",
					ModelMap:         transcode.ModelMap{AllowIdentity: true},
					Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
				},
				BodyLimits: transcode.BodyLimits{
					RetryReplayBytes: 5 << 20,
				},
			},
		),
	)
	if err == nil {
		t.Fatal("expected disabled retries rejection")
	}
	if !strings.Contains(err.Error(), "declares RetryReplayBytes=5242880 but retries are disabled") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
