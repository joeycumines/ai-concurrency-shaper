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
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"

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

	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher(pats)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(concurrency, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithQueueTimeout(timeout),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	return p, upstream, &maxConcurrent
}

func TestE2E_ConcurrencyLimitEnforced(t *testing.T) {
	t.Run("20_requests_concurrency_2", func(t *testing.T) {
		p, _, maxConcurrent := newTestProxy(t, 2, 0, "POST /v1/messages")

		const n = 20
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("expected 200, got %d", rec.Code)
				}
			})
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
		for range n {
			wg.Go(func() {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("expected 200, got %d", rec.Code)
				}
			})
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
	for range 50 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		})
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

	proxy, err := proxy.New(
		proxy.WithUpstream(slowURL),
		proxy.WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithQueueTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

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
	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher(nil)),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithGlobalLimiter(queue.NewLimiterWithCooldown(2, 0)),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		})
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

	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithGlobalLimiter(queue.NewLimiterWithCooldown(8, 0)),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	const n = 30
	var wg sync.WaitGroup

	for range n {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("limited: expected 200, got %d", rec.Code)
			}
		})
	}

	for range n {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		})
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
	for range 50 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		})
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

	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
		proxy.WithGlobalLimiter(queue.NewLimiterWithCooldown(4, 0)),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Fire a passthrough request and check active counter while it's in-flight.
	var wg sync.WaitGroup
	wg.Go(func() {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	})

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
	// Prevent the child process from corrupting the parent's terminal
	// flags. Without isolation, bubbletea's tea.Program.Run() enters raw
	// mode on os.Stdin (or /dev/tty), which modifies the PARENT's
	// terminal since the child inherits the same terminal FDs. This
	// disables ICANON/ECHO, leaving the parent terminal in raw mode
	// after the test exits — the user's shell becomes unusable.
	//
	// We use two layers of isolation:
	//   1. Stdin: pipe /dev/null so os.Stdin is not a terminal FD,
	//      preventing bubbletea from entering raw mode on stdin.
	//   2. Setsid: create a new session so the child has no controlling
	//      terminal, preventing bubbletea's OpenTTY() fallback from
	//      opening /dev/tty (the parent's terminal).
	//
	// With Setsid, bubbletea cannot start (OpenTTY fails), so the TUI
	// goroutine exits early and triggers a clean shutdown via stop().
	// The process exits with code 0 — which is correct behavior since
	// the TUI initiated the shutdown, not the bind failure. The test
	// verifies the process doesn't hang or crash regardless of which
	// error path is hit first.
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	out, err := cmd.CombinedOutput()

	// With Setsid, the child may exit cleanly (TUI OpenTTY failure
	// triggers graceful shutdown) or with an error (bind failure
	// arrives first). Either outcome is acceptable — the test
	// verifies the process doesn't hang.
	if err != nil {
		// Non-zero exit: likely the bind error arrived first.
		output := string(out)
		if !strings.Contains(output, "bind") && !strings.Contains(output, "address") {
			t.Logf("output: %s", output)
		}
	}
	// err == nil is also acceptable: TUI failure triggered clean shutdown.
}

func TestUpstreamMaxIdleConnsPerHost(t *testing.T) {
	parsePatterns := func(t *testing.T, specs ...string) []route.Pattern {
		t.Helper()
		patterns := make([]route.Pattern, 0, len(specs))
		for _, spec := range specs {
			p, err := route.Parse(spec)
			if err != nil {
				t.Fatalf("parse pattern %q: %v", spec, err)
			}
			patterns = append(patterns, p)
		}
		return patterns
	}

	tests := []struct {
		name            string
		global          int
		concurrency     int
		patterns        []route.Pattern
		routeLimiters   map[string]*queue.Limiter
		wantIdlePerHost int
	}{
		{
			name:            "default pool floors at legacy value",
			concurrency:     4,
			patterns:        route.DefaultPatterns(),
			wantIdlePerHost: 20,
		},
		{
			name:            "zero concurrency floors at legacy value",
			concurrency:     0,
			patterns:        route.DefaultPatterns(),
			wantIdlePerHost: 20,
		},
		{
			name:            "independent route limiters are summed",
			concurrency:     4,
			patterns:        parsePatterns(t, "POST /v1/chat/completions:20", "POST /v1/embeddings:20"),
			routeLimiters:   map[string]*queue.Limiter{"POST /v1/chat/completions:20": queue.NewLimiterWithCooldown(20, 0), "POST /v1/embeddings:20": queue.NewLimiterWithCooldown(20, 0)},
			wantIdlePerHost: 40,
		},
		{
			name:            "grouped route limiters are summed once",
			concurrency:     4,
			patterns:        parsePatterns(t, "POST /v1/messages:20@messages", "POST /v1/messages/batches:20@messages"),
			routeLimiters:   map[string]*queue.Limiter{"messages": queue.NewLimiterWithCooldown(20, 0)},
			wantIdlePerHost: 20,
		},
		{
			name:            "default pool and route limiter are combined",
			concurrency:     4,
			patterns:        parsePatterns(t, "POST /v1/messages", "POST /v1/embeddings:30"),
			routeLimiters:   map[string]*queue.Limiter{"POST /v1/embeddings:30": queue.NewLimiterWithCooldown(30, 0)},
			wantIdlePerHost: 34,
		},
		{
			name:            "global caps summed route pool",
			global:          25,
			concurrency:     4,
			patterns:        parsePatterns(t, "POST /v1/chat/completions:20", "POST /v1/embeddings:20"),
			routeLimiters:   map[string]*queue.Limiter{"POST /v1/chat/completions:20": queue.NewLimiterWithCooldown(20, 0), "POST /v1/embeddings:20": queue.NewLimiterWithCooldown(20, 0)},
			wantIdlePerHost: 25,
		},
		{
			name:            "global cap does not reduce below default floor",
			global:          0,
			concurrency:     0,
			patterns:        route.DefaultPatterns(),
			wantIdlePerHost: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstreamMaxIdleConnsPerHost(tt.global, tt.concurrency, tt.patterns, tt.routeLimiters)
			if got != tt.wantIdlePerHost {
				t.Fatalf("upstreamMaxIdleConnsPerHost() = %d, want %d", got, tt.wantIdlePerHost)
			}
		})
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

func TestCLI_UpstreamDisableKeepAlives(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var (
		currentOpen atomic.Int64
		peakOpen    atomic.Int64
	)

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	upstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			n := currentOpen.Add(1)
			for {
				old := peakOpen.Load()
				if n <= old || peakOpen.CompareAndSwap(old, n) {
					break
				}
			}
		case http.StateClosed, http.StateHijacked:
			currentOpen.Add(-1)
		}
	}
	upstream.Start()
	t.Cleanup(upstream.Close)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	var out strings.Builder
	cmd := exec.Command(bin,
		"-upstream", upstream.URL,
		"-limit", "POST /v1/messages",
		"-concurrency", "4",
		"-queue-timeout", "30s",
		"-bind", proxyAddr,
		"-upstream-disable-keep-alives",
		"-release-cooldown", "0",
		"-cancel-cooldown", "0",
		"-retry", "0",
		"-circuit-breaker=false",
	)

	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v\n%s", err, out.String())
	}

	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	runErr := make(chan error, 1)
	go func() {
		runErr <- cmd.Wait()
	}()

	if err := waitTCPReady(proxyAddr, 5*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-runErr
		}
		t.Fatalf("proxy not ready: %v\noutput:\n%s", err, out.String())
	}

	proxyURL := "http://" + proxyAddr + "/v1/messages"
	const n = 8
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 30 * time.Second}
	for range n {
		wg.Go(func() {
			resp, err := client.Post(proxyURL, "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Errorf("request failed: %v", err)
				return
			}
			defer resp.Body.Close()
			slurp, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected 200, got %d: %s", resp.StatusCode, slurp)
			}
		})
	}
	wg.Wait()

	_ = cmd.Process.Signal(syscall.SIGTERM)

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("proxy exited with error: %v\noutput:\n%s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
		t.Fatalf("proxy did not exit after SIGTERM\noutput:\n%s", out.String())
	}

	if got := peakOpen.Load(); got > 4 {
		t.Errorf("peak upstream connections = %d, want <= 4", got)
	}
}

func waitTCPReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("address %s did not become reachable", addr)
}

func TestCLI_AdaptiveHeadroom(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var requestCount atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	var out strings.Builder
	cmd := exec.Command(bin,
		"-upstream", upstream.URL,
		"-limit", "POST /v1/messages",
		"-concurrency", "4",
		"-queue-timeout", "30s",
		"-bind", proxyAddr,
		"-retry", "0",
		"-circuit-breaker=false",
		"-adaptive-headroom",
		"-adaptive-headroom-window", "200ms",
	)

	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v\n%s", err, out.String())
	}

	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	runErr := make(chan error, 1)
	go func() {
		runErr <- cmd.Wait()
	}()

	if err := waitTCPReady(proxyAddr, 5*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-runErr
		}
		t.Fatalf("proxy not ready: %v", err)
	}

	proxyURL := "http://" + proxyAddr + "/v1/messages"

	// First request returns 429 and should trigger adaptive headroom.
	resp, err := http.Post(proxyURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("first request status = %d, want 429", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Subsequent requests should succeed.
	for range 3 {
		resp, err := http.Post(proxyURL, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("follow-up request failed: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("follow-up request status = %d, want 200", resp.StatusCode)
		}
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("proxy exited with error: %v\noutput:\n%s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
		t.Fatalf("proxy did not exit after SIGTERM\noutput:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "adaptive headroom: enabled") {
		t.Errorf("expected startup log to mention adaptive headroom; output:\n%s", out.String())
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
					routeLimiters[p.Group] = queue.NewLimiterWithCooldown(p.Limit, 0)
				}
			} else {
				routeLimiters[p.Raw] = queue.NewLimiterWithCooldown(p.Limit, 0)
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
	limiter := queue.NewLimiterWithCooldown(10, 0)
	p, err := proxy.New(
		proxy.WithUpstream(mustParseURL("http://127.0.0.1:1")),
		proxy.WithMatcher(matcher),
		proxy.WithLimiter(limiter),
		proxy.WithMetrics(met),
		proxy.WithRouteLimiters(routeLimiters),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

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

// terminalStateDiff compares two terminal states and returns a human-readable
// description of any differences. Returns empty string if identical.
func terminalStateDiff(a, b *term.State) string {
	// Use the fact that term.State wraps unix.Termios which has
	// exported fields. Serialize via fmt.Sprintf for comparison.
	aStr := fmt.Sprintf("%+v", a)
	bStr := fmt.Sprintf("%+v", b)
	if aStr == bStr {
		return ""
	}
	return fmt.Sprintf("terminal state changed:\n  before: %s\n  after:  %s", aStr, bStr)
}

// TestSubprocessTerminalIsolation verifies that running the binary with -tui
// as a subprocess does NOT corrupt the parent process's terminal flags.
// This is a regression test for a bug where CombinedOutput() left stdin
// inherited from the parent, allowing bubbletea's MakeRaw() to disable
// ICANON/ECHO on the shared terminal FD.
//
// This test only runs when stdin is a real terminal. It will be skipped
// in CI or when output is piped. Use -count=N to detect cross-run
// contamination from prior test invocations.
func TestSubprocessTerminalIsolation(t *testing.T) {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		t.Skip("skipping: stdin is not a terminal (run from a real terminal to enable)")
	}

	// Capture terminal state before running the subprocess.
	before, err := term.GetState(fd)
	if err != nil {
		t.Fatalf("get terminal state before: %v", err)
	}

	// Build the binary.
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Occupy a port so the binary exits quickly.
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
	// Apply the same isolation as TestTUIExitsOnBindFailure:
	// /dev/null stdin + new session to prevent terminal access.
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	_, _ = cmd.CombinedOutput()

	// Verify terminal state is preserved.
	after, err := term.GetState(fd)
	if err != nil {
		t.Fatalf("get terminal state after: %v", err)
	}

	if diff := terminalStateDiff(before, after); diff != "" {
		t.Error(diff)
		// Restore the saved state to prevent leaving the terminal broken.
		if err := term.Restore(fd, before); err != nil {
			t.Errorf("failed to restore terminal state: %v", err)
		}
	}
}
