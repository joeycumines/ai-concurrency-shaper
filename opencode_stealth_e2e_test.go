package main

// Opencode stealth end-to-end: a suite wired like an opencode.ai mount
// (three native routes plus the first-party preset) serves
// claude-code-shaped discovery and inference with zen-faithful behavior.

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
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

type stealthUpstream struct {
	mu       sync.Mutex
	path     string
	model    string
	session  string
	ua       string
	beta     string
	version  string
	response map[string]string
}

func (u *stealthUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	u.mu.Lock()
	u.path = r.URL.Path
	u.model = probe.Model
	u.session = r.Header.Get("X-Opencode-Session")
	u.ua = r.Header.Get("User-Agent")
	u.beta = r.URL.Query().Get("beta")
	u.version = r.Header.Get("anthropic-version")
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(u.response[r.URL.Path]))
}

func (u *stealthUpstream) got() (path, model, session, ua, beta, version string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.model, u.session, u.ua, u.beta, u.version
}

// TestOpencodeStealthSuiteEndToEnd drives the user's failing shape through
// the wired stack: suite discovery plus claude-code-shaped inference per
// native family, asserting upstream path, wire model, session presence,
// first-party User-Agent, query/header tolerance, and alias restoration.
func TestOpencodeStealthSuiteEndToEnd(t *testing.T) {
	up := &stealthUpstream{response: map[string]string{
		"/v1/messages":         `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"wire-msg","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		"/v1/responses":        `{"id":"resp_1","object":"response","model":"wire-resp","status":"completed","output":[]}`,
		"/v1/chat/completions": `{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	modelMap := transcode.ModelMap{
		Exact: map[string]transcode.ModelMapping{
			"claude-alias": {
				ClientModel: "claude-alias", UpstreamModel: "wire-msg",
				ClientResponseModel: "claude-alias", Via: transcode.NativeMessages,
			},
			"gpt-alias": {
				ClientModel: "gpt-alias", UpstreamModel: "wire-resp",
				ClientResponseModel: "gpt-alias", Via: transcode.NativeResponses,
			},
			"chat-alias": {
				ClientModel: "chat-alias", UpstreamModel: "wire-chat",
				ClientResponseModel: "chat-alias", Via: transcode.NativeChat,
			},
		},
		AllowIdentity:      false,
		RequireExplicitMap: true,
	}
	native := func(protocol transcode.NativeProtocol, path string) proxy.NativeRoute {
		key, err := transcode.NewRouteKey(http.MethodPost, path)
		if err != nil {
			t.Fatal(err)
		}
		return proxy.NativeRoute{RouteKey: key, Protocol: protocol, ModelMap: modelMap, Provider: "zen"}
	}
	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(8, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithNativeRoutes(
			native(transcode.NativeMessages, "/v1/messages"),
			native(transcode.NativeResponses, "/v1/responses"),
			native(transcode.NativeChat, "/v1/chat/completions"),
		),
		proxy.WithOpencodePreset(transcode.OpencodePreset{Enabled: true, Provider: "zen"}),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	catHandler, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite",
		Models: []transcode.CatalogModel{
			{Surrogate: "claude-alias", Provider: "zen"},
			{Surrogate: "gpt-alias", Provider: "zen"},
			{Surrogate: "chat-alias", Provider: "zen"},
		},
		DefaultShape: transcode.CatalogShapeAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	allRoutes := map[transcode.RouteKey]struct{}{
		{Method: http.MethodPost, Path: "/v1/messages"}:         {},
		{Method: http.MethodPost, Path: "/v1/responses"}:        {},
		{Method: http.MethodPost, Path: "/v1/chat/completions"}: {},
	}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite",
		Prefix:         "/suite",
		CatalogHandler: catHandler,
		DefaultShape:   transcode.CatalogShapeAnthropic,
		ModelRoutes: []router.ModelRoute{
			{Model: "claude-alias", Provider: "zen", Handler: p, SupportedRoutes: allRoutes},
			{Model: "gpt-alias", Provider: "zen", Handler: p, SupportedRoutes: allRoutes},
			{Model: "chat-alias", Provider: "zen", Handler: p, SupportedRoutes: allRoutes},
		},
		Strict: true,
	})
	rtr, err := router.New([]router.Provider{
		{Name: "zen", Prefix: "/zen", Proxy: p},
		{Name: "suite", Prefix: "/suite", Proxy: suite},
	})
	if err != nil {
		t.Fatal(err)
	}

	serve := func(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = bytes.NewBufferString(body)
		}
		req := httptest.NewRequest(method, target, reader)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		return rec
	}

	// Discovery: GET /suite/v1/models?limit=1000 lists the surrogates.
	rec := serve(http.MethodGet, "/suite/v1/models?limit=1000", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery status = %d: %s", rec.Code, rec.Body.String())
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data) != 3 {
		t.Fatalf("discovery models = %d, want 3: %s", len(catalog.Data), rec.Body.String())
	}

	// Inference per family: headerless claude-code shape with ?beta=true.
	cases := []struct {
		name         string
		target       string
		body         string
		upstreamPath string
		wire         string
		surrogate    string
	}{
		{"messages", "/suite/v1/messages?beta=true",
			`{"model":"claude-alias","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			"/v1/messages", "wire-msg", "claude-alias"},
		{"responses", "/suite/v1/responses",
			`{"model":"gpt-alias","input":"hi"}`,
			"/v1/responses", "wire-resp", "gpt-alias"},
		{"chat", "/suite/v1/chat/completions",
			`{"model":"chat-alias","messages":[{"role":"user","content":"hi"}]}`,
			"/v1/chat/completions", "wire-chat", "chat-alias"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(http.MethodPost, tc.target, tc.body, map[string]string{
				"Content-Type":      "application/json",
				"anthropic-version": "2023-06-01",
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			path, model, session, ua, beta, version := up.got()
			if path != tc.upstreamPath || model != tc.wire {
				t.Fatalf("upstream path=%q model=%q, want %q %q", path, model, tc.upstreamPath, tc.wire)
			}
			if session == "" {
				t.Fatal("upstream session missing: headerless clients must still carry one")
			}
			if !strings.HasPrefix(ua, "opencode/") {
				t.Fatalf("upstream UA = %q, want the first-party shape", ua)
			}
			if tc.name == "messages" && (beta != "true" || version != "2023-06-01") {
				t.Fatalf("beta=%q version=%q, want tolerated true/2023-06-01", beta, version)
			}
			var downstream struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &downstream); err != nil {
				t.Fatalf("downstream body: %v", err)
			}
			if downstream.Model != tc.surrogate {
				t.Fatalf("downstream model = %q, want alias %q", downstream.Model, tc.surrogate)
			}
		})
	}

	// Unknown model: 404 in the client dialect, no upstream contact.
	before, _, _, _, _, _ := up.got()
	rec = serve(http.MethodPost, "/suite/v1/messages",
		`{"model":"nope","max_tokens":1,"messages":[]}`,
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404", rec.Code)
	}
	if path, _, _, _, _, _ := up.got(); path != before {
		t.Fatal("unknown model reached the upstream")
	}
}

// TestOpencodeStealthStreamByteIdentical proves a streaming exchange
// through suite plus native plus preset stays byte-identical downstream
// while carrying the wire model and session upstream.
func TestOpencodeStealthStreamByteIdentical(t *testing.T) {
	const sse = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"wire-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	var (
		mu      sync.Mutex
		gotPath string
		gotSess string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotSess = r.Header.Get("X-Opencode-Session")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/chat/completions")
	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(8, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithNativeRoutes(proxy.NativeRoute{
			RouteKey: key,
			Protocol: transcode.NativeChat,
			ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
				"chat-alias": {
					ClientModel: "chat-alias", UpstreamModel: "wire-chat",
					ClientResponseModel: "chat-alias", Via: transcode.NativeChat,
				},
			}, AllowIdentity: false, RequireExplicitMap: true},
			Provider: "zen",
		}),
		proxy.WithOpencodePreset(transcode.OpencodePreset{Enabled: true, Provider: "zen"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"chat-alias","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != sse {
		t.Fatalf("stream = %q, want byte-identical", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/chat/completions" || gotSess == "" {
		t.Fatalf("upstream path=%q session=%q, want native path with a session", gotPath, gotSess)
	}
}
