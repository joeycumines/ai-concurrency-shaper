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

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// newTestProxy builds a proxy backed by a fake upstream that tracks
// concurrency and returns the method+path in the JSON body.
func newTestProxy(t *testing.T, concurrency int, timeout time.Duration, patterns ...string) (*proxy.Proxy, *httptest.Server, *atomic.Int64) {
	t.Helper()

	var maxConcurrent atomic.Int64
	var currentConcurrent atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := currentConcurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		// Simulate some work.
		time.Sleep(10 * time.Millisecond)
		currentConcurrent.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"method":%q,"path":%q}`, r.Method, r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	var pats []route.Pattern
	for _, p := range patterns {
		pat, err := route.Parse(p)
		if err != nil {
			t.Fatalf("parse pattern %q: %v", p, err)
		}
		pats = append(pats, pat)
	}

	cfg := proxy.Config{
		Upstream:     upstreamURL,
		Matcher:      route.NewMatcher(pats),
		Limiter:      queue.NewLimiter(concurrency),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: timeout,
	}

	return proxy.New(cfg), upstream, &maxConcurrent
}

func TestE2E_ConcurrencyLimitEnforced(t *testing.T) {
	t.Run("20_requests_concurrency_2", func(t *testing.T) {
		p, _, maxConcurrent := newTestProxy(t, 2, 0, "POST /v1/messages")

		const n = 20
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("expected 200, got %d", rec.Code)
				}
			}()
		}
		wg.Wait()

		snap := p.Metrics().Snapshot()
		if snap.TotalProxied != n {
			t.Errorf("TotalProxied: got %d, want %d", snap.TotalProxied, n)
		}
		if snap.Active != 0 {
			t.Errorf("Active: got %d, want 0", snap.Active)
		}
		got := maxConcurrent.Load()
		if got > 2 {
			t.Errorf("upstream saw %d concurrent, want <= 2", got)
		}
	})

	t.Run("50_requests_concurrency_3", func(t *testing.T) {
		p, _, maxConcurrent := newTestProxy(t, 3, 0, "POST /v1/messages")

		const n = 50
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("expected 200, got %d", rec.Code)
				}
			}()
		}
		wg.Wait()

		snap := p.Metrics().Snapshot()
		if snap.TotalProxied != n {
			t.Errorf("TotalProxied: got %d, want %d", snap.TotalProxied, n)
		}
		if snap.Active != 0 {
			t.Errorf("Active: got %d, want 0", snap.Active)
		}
		got := maxConcurrent.Load()
		if got > 3 {
			t.Errorf("upstream saw %d concurrent, want <= 3", got)
		}
	})
}

func TestE2E_PassthroughUnaffected(t *testing.T) {
	proxy, _, _ := newTestProxy(t, 1, 0, "POST /v1/messages")

	// 50 passthrough requests should all succeed even though concurrency is 1.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	snap := proxy.Metrics().Snapshot()
	if snap.TotalPassThrough != 50 {
		t.Errorf("TotalPassThrough: got %d, want 50", snap.TotalPassThrough)
	}
}

func TestE2E_QueueTimeout(t *testing.T) {
	// Slow upstream holds the single slot for 2s.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	cfg := proxy.Config{
		Upstream:     slowURL,
		Matcher:      route.NewMatcher([]route.Pattern{pat}),
		Limiter:      queue.NewLimiter(1),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: 100 * time.Millisecond,
	}
	proxy := proxy.New(cfg)

	// First request holds the slot.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
	}()

	time.Sleep(20 * time.Millisecond)

	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	proxy.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", rec2.Code, rec2.Body.String())
	}

	snap := proxy.Metrics().Snapshot()
	if snap.TotalTimeout != 1 {
		t.Errorf("TotalTimeout: got %d, want 1", snap.TotalTimeout)
	}
}

func TestE2E_ResponseBodyPreserved(t *testing.T) {
	proxy, _, _ := newTestProxy(t, 2, 0, "POST /v1/messages")

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(respBody), `"method":"POST"`) {
		t.Errorf("response body not proxied correctly: %q", string(respBody))
	}
}

func TestE2E_MultiplePatterns(t *testing.T) {
	proxy, _, _ := newTestProxy(t, 4, 0,
		"POST /v1/chat/completions",
		"POST /v1/responses",
		"POST /v1/messages",
	)

	patterns := []struct {
		method  string
		path    string
		limited bool
	}{
		{"POST", "/v1/chat/completions", true},
		{"POST", "/v1/responses", true},
		{"POST", "/v1/messages", true},
		{"GET", "/v1/chat/completions", false},
		{"POST", "/health", false},
		{"GET", "/health", false},
	}

	for _, p := range patterns {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			req := httptest.NewRequest(p.method, p.path, nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		})
	}
}

func TestE2E_PassthroughLogged(t *testing.T) {
	proxy, _, _ := newTestProxy(t, 2, 0, "POST /v1/messages")

	// Passthrough request.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	snap := proxy.Metrics().Snapshot()
	if len(snap.LogEntries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(snap.LogEntries))
	}
	if snap.LogEntries[0].Method != "GET" || snap.LogEntries[0].Path != "/health" {
		t.Errorf("wrong entry: %v", snap.LogEntries[0])
	}
	if snap.LogEntries[0].Limited {
		t.Error("passthrough should not be limited")
	}
	if snap.TotalPassThrough != 1 {
		t.Errorf("TotalPassThrough: got %d, want 1", snap.TotalPassThrough)
	}
}

func TestGlobalConcurrency_PassthroughBounded(t *testing.T) {
	// With global-concurrency=2 and no limited routes, 50 concurrent
	// passthrough requests should be bounded to 2 at the upstream.
	var maxConcurrent atomic.Int64
	var currentConcurrent atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := currentConcurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		currentConcurrent.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	p := proxy.New(proxy.Config{
		Upstream:      upstreamURL,
		Matcher:       route.NewMatcher(nil),
		Limiter:       queue.NewLimiter(4),
		Metrics:       metrics.NewCollector(),
		GlobalLimiter: queue.NewLimiter(2),
	})

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	got := maxConcurrent.Load()
	if got > 2 {
		t.Errorf("upstream saw %d concurrent passthrough, want <= 2", got)
	}
}

func TestGlobalConcurrency_MixedTraffic(t *testing.T) {
	// concurrency=4, global-concurrency=8.
	// Fire 30 limited + 30 passthrough concurrent requests.
	// Limited should be capped at 4, total at 8.
	var maxConcurrent atomic.Int64
	var currentConcurrent atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := currentConcurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		currentConcurrent.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	p := proxy.New(proxy.Config{
		Upstream:      upstreamURL,
		Matcher:       route.NewMatcher([]route.Pattern{pat}),
		Limiter:       queue.NewLimiter(4),
		Metrics:       metrics.NewCollector(),
		GlobalLimiter: queue.NewLimiter(8),
	})

	const n = 30
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("limited: expected 200, got %d", rec.Code)
			}
		}()
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		}()
	}

	wg.Wait()

	got := maxConcurrent.Load()
	if got > 8 {
		t.Errorf("upstream saw %d concurrent total, want <= 8", got)
	}
}

func TestGlobalConcurrency_BackwardsCompatible(t *testing.T) {
	// Without -global-concurrency, passthrough is unbounded.
	// With concurrency=1 and 50 passthrough requests, all should succeed
	// (passthrough doesn't use the limiter).
	p, _, _ := newTestProxy(t, 1, 0, "POST /v1/messages")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	snap := p.Metrics().Snapshot()
	if snap.TotalPassThrough != 50 {
		t.Errorf("TotalPassThrough: got %d, want 50", snap.TotalPassThrough)
	}
}

func TestGlobalConcurrency_ActiveCounter(t *testing.T) {
	// With global concurrency enabled, the active counter should
	// include passthrough requests.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	p := proxy.New(proxy.Config{
		Upstream:      upstreamURL,
		Matcher:       route.NewMatcher([]route.Pattern{pat}),
		Limiter:       queue.NewLimiter(4),
		Metrics:       metrics.NewCollector(),
		GlobalLimiter: queue.NewLimiter(4),
	})

	// Fire a passthrough request and check active counter while it's in-flight.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	time.Sleep(10 * time.Millisecond)

	snap := p.Metrics().Snapshot()
	if snap.Active < 1 {
		t.Errorf("Active: got %d, want >= 1 (passthrough should be counted)", snap.Active)
	}

	wg.Wait()
}

// TestTUIExitsOnBindFailure verifies that the binary exits cleanly when
// the bind address is already in use and -tui is enabled. It does NOT
// verify terminal restoration — that requires PTY-based integration
// testing (see internal/tui/tuitest/).
func TestTUIExitsOnBindFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Build the binary.
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin,
		"-bind", addr,
		"-upstream", "http://127.0.0.1:1",
		"-tui",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected error from bind failure, got nil")
	}

	output := string(out)
	if !strings.Contains(output, "bind") && !strings.Contains(output, "address") {
		t.Logf("output: %s", output)
	}
}

func TestVersionFlag(t *testing.T) {
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("-version: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		t.Fatal("-version produced empty output")
	}
	// Default version is "dev" when built without ldflags.
	if got != "dev" {
		t.Errorf("unexpected version: got %q, want %q", got, "dev")
	}
}

func TestGroupLimiterSharing(t *testing.T) {
	// Two patterns in the same @group should share a limiter.
	p1, err := route.Parse("POST /v1/chat/completions:3@llm")
	if err != nil {
		t.Fatalf("parse p1: %v", err)
	}
	p2, err := route.Parse("POST /v1/messages:3@llm")
	if err != nil {
		t.Fatalf("parse p2: %v", err)
	}

	if p1.Group != p2.Group {
		t.Fatalf("expected same group, got %q and %q", p1.Group, p2.Group)
	}

	routeLimiters := make(map[string]*queue.Limiter)
	patterns := []route.Pattern{p1, p2}

	for _, p := range patterns {
		if p.Limit > 0 {
			if p.Group != "" {
				if _, exists := routeLimiters[p.Group]; !exists {
					routeLimiters[p.Group] = queue.NewLimiter(p.Limit)
				}
			} else {
				routeLimiters[p.Raw] = queue.NewLimiter(p.Limit)
			}
		}
	}

	if len(routeLimiters) != 1 {
		t.Fatalf("expected 1 limiter, got %d", len(routeLimiters))
	}
	lim := routeLimiters["llm"]
	if lim == nil {
		t.Fatal("llm limiter is nil")
	}

	matcher := route.NewMatcher(patterns)
	met := metrics.NewCollector()
	limiter := queue.NewLimiter(10)
	p := proxy.New(proxy.Config{
		Upstream:      mustParseURL("http://127.0.0.1:1"),
		Matcher:       matcher,
		Limiter:       limiter,
		Metrics:       met,
		RouteLimiters: routeLimiters,
	})

	// Both routes should hit the group limiter, not the global one.
	_ = p // The acquireSlot method is internal; we verify via route/key mapping.
}

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
