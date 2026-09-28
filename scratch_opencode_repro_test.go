package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestScratch_OpencodeRepro demonstrates the reported zen/go failure shape:
// claude-code-shaped POST /suite/v1/messages?beta=true with a messages-native
// model is forced through messages->chat and the session header is dropped.
func TestScratch_OpencodeRepro(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotSession, gotModel, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		mu.Lock()
		gotPath = r.URL.Path
		gotSession = r.Header.Get("x-opencode-session")
		gotModel = probe.Model
		gotBeta = r.URL.Query().Get("beta")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"qwen3.7-max","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	t.Cleanup(upstream.Close)

	u, _ := url.Parse(upstream.URL)
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	p, err := proxy.New(
		proxy.WithUpstream(u),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: transcode.Mapping{
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
			ModelMap:             transcode.ModelMap{AllowIdentity: true},
			Auth:                 transcode.AuthPolicy{Mode: transcode.AuthNone},
			AllowedClientQuery:   map[string]struct{}{"beta": {}},
			ChatCapabilities:     transcode.ChatCapabilities{ParallelToolCalls: true},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}

	cat, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite",
		Models:       []transcode.CatalogModel{{Surrogate: "qwen3.7-max", Provider: "zen"}},
		DefaultShape: transcode.CatalogShapeAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite",
		Prefix:         "/suite",
		CatalogHandler: cat,
		DefaultShape:   transcode.CatalogShapeAnthropic,
		ModelRoutes: []router.ModelRoute{{
			Model:     "qwen3.7-max",
			Provider:  "zen",
			Handler:   p,
			SupportedRoutes: map[transcode.RouteKey]struct{}{
				{Method: http.MethodPost, Path: "/v1/messages"}: {},
			},
		}},
		Strict: true,
	})
	rtr, err := router.New([]router.Provider{
		{Name: "zen", Prefix: "/zen", Proxy: p},
		{Name: "suite", Prefix: "/suite", Proxy: suite},
	})
	if err != nil {
		t.Fatal(err)
	}

	do := func(target, session string) (int, string) {
		body := `{"model":"qwen3.7-max","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("x-api-key", "test-key")
		if session != "" {
			req.Header.Set("x-opencode-session", session)
		}
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		resp := rec.Result()
		b, _ := io.ReadAll(resp.Body)
		mu.Lock()
		defer mu.Unlock()
		t.Logf("client=%s session=%q -> downstream=%d upstream_path=%q upstream_model=%q upstream_session=%q upstream_beta=%q body=%.200s",
			target, session, rec.Code, gotPath, gotModel, gotSession, gotBeta, string(b))
		return rec.Code, string(b)
	}

	do("/suite/v1/messages?beta=true", "sess-123")
	do("/suite/v1/messages?beta=true", "")
	code, _ := do("/suite/v1/chat/completions", "sess-123")
	t.Logf("chat-completions-via-suite downstream=%d (want 404 under current strict suite)", code)
}
