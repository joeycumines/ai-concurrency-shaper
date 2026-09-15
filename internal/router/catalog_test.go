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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// helperMessagesToChatMapping returns a valid Messages->Chat transcode.Mapping.
func helperMessagesToChatMapping(t *testing.T) transcode.Mapping {
	t.Helper()
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	return transcode.Mapping{
		ClientRoute:      key,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		LossPolicy:       transcode.StrictLossPolicy(),
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
	}
}

// helperCatalogConfig builds a mount snapshot with one model.
func helperCatalogConfig(provider, surrogate string, servesResponses, servesMessages bool) transcode.CatalogConfig {
	return transcode.CatalogConfig{
		ProviderName:      provider,
		ServesResponses:   servesResponses,
		ServesMessages:    servesMessages,
		ParallelToolCalls: true,
		StructuredOutputs: true,
		Models: []transcode.CatalogModel{
			{Surrogate: surrogate},
		},
	}
}

// helperCatalogProxy builds a proxy serving helperCatalogConfig.
func helperCatalogProxy(t *testing.T, upstream *httptest.Server, config transcode.CatalogConfig, mappings ...transcode.Mapping) *proxy.Proxy {
	t.Helper()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := []proxy.Option{
		proxy.WithUpstream(u),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithModelCatalog(config),
	}
	for _, mapping := range mappings {
		opts = append(opts, proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: mapping}))
	}
	p, err := proxy.New(opts...)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p
}

// TestRouter_CatalogScopedPerMount proves each mount lists only its own
// catalog and never touches another mount's upstream.
func TestRouter_CatalogScopedPerMount(t *testing.T) {
	var countA, countB atomic.Int64
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countA.Add(1)
	}))
	t.Cleanup(upstreamA.Close)
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countB.Add(1)
	}))
	t.Cleanup(upstreamB.Close)

	proxyA := helperCatalogProxy(t, upstreamA, helperCatalogConfig("dialagram", "qwen-3.8-max", true, false), helperResponsesToChatMapping(t))
	proxyB := helperCatalogProxy(t, upstreamB, helperCatalogConfig("verboo", "glm-5.3-flash", false, true), helperMessagesToChatMapping(t))

	r, err := router.New([]router.Provider{
		{Name: "dialagram", Prefix: "/dialagram", Proxy: proxyA},
		{Name: "verboo", Prefix: "/verboo", Proxy: proxyB},
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	get := func(path string, headers map[string]string) (int, string) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	status, body := get("/dialagram/v1/models", map[string]string{"Anthropic-Version": "2023-06-01"})
	if status != http.StatusOK {
		t.Fatalf("dialagram catalog status = %d", status)
	}
	if !strings.Contains(body, "qwen-3.8-max") || strings.Contains(body, "glm-5.3-flash") {
		t.Fatalf("dialagram catalog leaked or missed models: %s", body)
	}
	if countA.Load() != 0 || countB.Load() != 0 {
		t.Fatalf("catalog contacts an upstream: A=%d B=%d", countA.Load(), countB.Load())
	}

	status, body = get("/verboo/v1/models", nil)
	if status != http.StatusOK {
		t.Fatalf("verboo catalog status = %d", status)
	}
	if !strings.Contains(body, "glm-5.3-flash") || strings.Contains(body, "qwen-3.8-max") {
		t.Fatalf("verboo catalog leaked or missed models: %s", body)
	}
	if countA.Load() != 0 || countB.Load() != 0 {
		t.Fatalf("catalog contacts an upstream: A=%d B=%d", countA.Load(), countB.Load())
	}

	// A bare-root single provider serves its catalog at the root path.
	bareUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(bareUpstream.Close)
	bare := helperCatalogProxy(t, bareUpstream, helperCatalogConfig("anthropic", "opus", false, true), helperMessagesToChatMapping(t))
	bareRouter, err := router.New([]router.Provider{{Name: "anthropic", Proxy: bare}})
	if err != nil {
		t.Fatalf("router.New bare: %v", err)
	}
	bareSrv := httptest.NewServer(bareRouter)
	t.Cleanup(bareSrv.Close)
	resp, err := http.Get(bareSrv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(bodyBytes), "opus") {
		t.Fatalf("bare-root catalog = %d %s", resp.StatusCode, bodyBytes)
	}
}

// TestRouter_CatalogShapeSelection pins the precedence and its 400s.
func TestRouter_CatalogShapeSelection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)

	both := helperCatalogProxy(t, upstream, helperCatalogConfig("both", "alpha", true, true),
		helperResponsesToChatMapping(t), helperMessagesToChatMapping(t))
	responsesOnly := helperCatalogProxy(t, upstream, helperCatalogConfig("dialagram", "alpha", true, false),
		helperResponsesToChatMapping(t))

	bothRouter, err := router.New([]router.Provider{{Name: "both", Prefix: "/both", Proxy: both}})
	if err != nil {
		t.Fatal(err)
	}
	bothSrv := httptest.NewServer(bothRouter)
	t.Cleanup(bothSrv.Close)
	responsesRouter, err := router.New([]router.Provider{{Name: "dialagram", Prefix: "/dialagram", Proxy: responsesOnly}})
	if err != nil {
		t.Fatal(err)
	}
	responsesSrv := httptest.NewServer(responsesRouter)
	t.Cleanup(responsesSrv.Close)

	fetch := func(base, path string, headers map[string]string) (int, string) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	cases := []struct {
		name        string
		base        string
		path        string
		headers     map[string]string
		wantStatus  int
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "both-dialect default is lean",
			base: bothSrv.URL, path: "/both/v1/models", wantStatus: http.StatusOK,
			wantPresent: []string{`"object":"list"`, `"data":[`}, wantAbsent: []string{`"models":`},
		},
		{
			name: "client_version selects codex on any mount",
			base: responsesSrv.URL, path: "/dialagram/v1/models?client_version=0.154.0", wantStatus: http.StatusOK,
			wantPresent: []string{`"models":[`}, wantAbsent: []string{`"object":"list"`},
		},
		{
			name: "anthropic header selects the messages list",
			base: responsesSrv.URL, path: "/dialagram/v1/models", headers: map[string]string{"Anthropic-Version": "2023-06-01"},
			wantStatus:  http.StatusOK,
			wantPresent: []string{`"data":[`, `"first_id"`, `"has_more"`}, wantAbsent: []string{`"object":"list"`, `"models":`},
		},
		{
			name: "format codex overrides the both-default",
			base: bothSrv.URL, path: "/both/v1/models?format=codex", wantStatus: http.StatusOK,
			wantPresent: []string{`"models":[`}, wantAbsent: []string{`"object":"list"`},
		},
		{
			name: "format openai overrides the responses default",
			base: responsesSrv.URL, path: "/dialagram/v1/models?format=openai", wantStatus: http.StatusOK,
			wantPresent: []string{`"object":"list"`}, wantAbsent: []string{`"models":`},
		},
		{
			name: "unknown format is a dialect 400, never lean",
			base: bothSrv.URL, path: "/both/v1/models?format=BOGUS", wantStatus: http.StatusBadRequest,
			wantPresent: []string{"unknown catalog format"}, wantAbsent: []string{`"object":"list"`, `"models":[`},
		},
		{
			name: "beta is accepted and ignored",
			base: bothSrv.URL, path: "/both/v1/models?beta=true", wantStatus: http.StatusOK,
			wantPresent: []string{`"object":"list"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := fetch(tc.base, tc.path, tc.headers)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", status, tc.wantStatus, body)
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q: %s", want, body)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body carries %q: %s", absent, body)
				}
			}
		})
	}
}

// TestCatalogLeanNeverForCodex pins both negative directions: the lean shape is
// never substituted where Codex is required, and Codex never masquerades as
// lean.
func TestCatalogLeanNeverForCodex(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)
	p := helperCatalogProxy(t, upstream, helperCatalogConfig("dialagram", "alpha", true, false),
		helperResponsesToChatMapping(t))
	r, err := router.New([]router.Provider{{Name: "dialagram", Prefix: "/dialagram", Proxy: p}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dialagram/v1/models?client_version=0.154.0")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var codex map[string]any
	if err := json.Unmarshal(body, &codex); err != nil {
		t.Fatal(err)
	}
	if _, ok := codex["models"]; !ok {
		t.Fatalf("codex probe did not return the native shape: %s", body)
	}
	if _, ok := codex["object"]; ok {
		t.Fatalf("codex probe returned an object envelope: %s", body)
	}
	if _, ok := codex["data"]; ok {
		t.Fatalf("codex probe returned a data envelope: %s", body)
	}

	resp, err = http.Get(srv.URL + "/dialagram/v1/models?format=openai")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var lean map[string]any
	if err := json.Unmarshal(body, &lean); err != nil {
		t.Fatal(err)
	}
	if lean["object"] != "list" {
		t.Fatalf("lean shape missing object:list: %s", body)
	}
	if _, ok := lean["models"]; ok {
		t.Fatalf("lean shape carries models: %s", body)
	}
}
