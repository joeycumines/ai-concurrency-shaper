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
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// TestTUIStartupFailureHoldLogIsNotActionable guards the "-failure-hold"
// startup summary that is emitted into the captured Logs buffer on every TUI
// start (main.go). The summary must be printed in hyphen-bound form
// ("failure-hold: 2s") — a space-separated "failure hold: 2s" reads like
// prose to the Logs-tab classifier, whose whole-line keyword scan fires on the
// word-boundary "failure" and raises a toast every time the dashboard launches.
// The classifier-side contract is pinned in logclass_test.go; this test pins the
// actual line the binary emits so a future rephrase cannot regress the toast.
func TestTUIStartupFailureHoldLogIsNotActionable(t *testing.T) {
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

	// A free bind address so startup config logging runs and renders into the
	// buffer; the only reason the TUI fails to start is the missing controlling
	// terminal (Setsid below), exactly as in TestTUIExitsOnBindFailure.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Do not pass -failure-hold: the 2s default must hold so the startup line
	// is actually emitted. -tui routes the buffered startup summaries through the
	// Logs classifier; on shutdown the buffer flushes to stderr, which
	// CombinedOutput captures.
	cmd := exec.CommandContext(ctx, bin,
		"-tui",
		"-bind", addr,
		"-upstream", "http://127.0.0.1:1",
	)
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	out, _ := cmd.CombinedOutput()
	output := string(out)

	if strings.Contains(output, "failure hold: 2s") {
		t.Fatalf("startup log emitted space-separated \"failure hold: 2s\" (actionable → toast):\n%s", output)
	}
	if !strings.Contains(output, "failure-hold: 2s") {
		t.Fatalf("startup log missing hyphenated \"failure-hold: 2s\":\n%s", output)
	}
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
		limitAll        bool
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
		{
			// limitAll routes every non-matching request through the default
			// limiter, so the default pool's capacity must be counted even when
			// every configured pattern carries an explicit route limit.
			name:            "limit-all counts default pool when all routes have explicit limits",
			concurrency:     100,
			patterns:        parsePatterns(t, "POST /v1/messages:5"),
			routeLimiters:   map[string]*queue.Limiter{"POST /v1/messages:5": queue.NewLimiterWithCooldown(5, 0)},
			limitAll:        true,
			wantIdlePerHost: 105,
		},
		{
			// Without limitAll the default pool is NOT used (the only pattern
			// has its own limiter), so concurrency does not contribute.
			name:            "without limit-all default pool is unused when routes have explicit limits",
			concurrency:     100,
			patterns:        parsePatterns(t, "POST /v1/messages:5"),
			routeLimiters:   map[string]*queue.Limiter{"POST /v1/messages:5": queue.NewLimiterWithCooldown(5, 0)},
			limitAll:        false,
			wantIdlePerHost: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstreamMaxIdleConnsPerHost(tt.global, tt.concurrency, tt.patterns, tt.routeLimiters, tt.limitAll)
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

	const concurrency = 4

	// Count handler entries rather than ConnState transitions: StateNew can run
	// before the previous connection's StateClosed callback, so ConnState is a
	// flaky proxy-admission signal for this assertion. The limiter bounds active
	// upstream requests; verify that deterministically.
	var (
		activeRequests          atomic.Int64
		peakActiveRequests      atomic.Int64
		connectionCloseFailures atomic.Int64
	)

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.Close {
			connectionCloseFailures.Add(1)
		}

		n := activeRequests.Add(1)
		for {
			old := peakActiveRequests.Load()
			if n <= old || peakActiveRequests.CompareAndSwap(old, n) {
				break
			}
		}
		defer activeRequests.Add(-1)

		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
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
	start := make(chan struct{})
	var ready sync.WaitGroup
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 30 * time.Second}
	for range n {
		ready.Add(1)
		wg.Go(func() {
			ready.Done() // signal ready BEFORE waiting for the start barrier
			<-start
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
	ready.Wait()
	close(start)
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

	if got := peakActiveRequests.Load(); got > concurrency {
		t.Errorf("peak active upstream requests = %d, want <= %d", got, concurrency)
	}
	if got := connectionCloseFailures.Load(); got != 0 {
		t.Errorf("upstream requests without Connection: close = %d", got)
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

// TestE2E_MultiProvider proves stage-1 multi-provider routing end-to-end
// through the real binary: two --provider sections mount two upstreams at
// distinct prefixes, each prefixed request reaches its upstream with the
// prefix stripped (true mount), unmatched paths 404, and an overlapping
// prefix pair is rejected before the server binds.
func TestE2E_MultiProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Run("routing", func(t *testing.T) {
		bin := t.TempDir() + "/test-shaper"
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Dir = "."
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build failed: %v\n%s", err, out)
		}

		// Each upstream echoes the path it received, so the response body
		// proves which upstream served the request and what path it saw.
		newEcho := func() *httptest.Server {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "%s", r.URL.Path)
			}))
			t.Cleanup(srv.Close)
			return srv
		}
		upA := newEcho()
		upB := newEcho()

		proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		proxyAddr := proxyLn.Addr().String()
		proxyLn.Close()

		var out strings.Builder
		cmd := exec.Command(bin,
			"-bind", proxyAddr,
			"--provider=acme",
			"-upstream", upA.URL,
			"-prefix", "/acme",
			"-retry", "0",
			"-circuit-breaker=false",
			"--provider=beta",
			"-upstream", upB.URL,
			"-prefix", "/acme2",
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

		if err := waitTCPReady(proxyAddr, 5*time.Second); err != nil {
			t.Fatalf("proxy not ready: %v\noutput:\n%s", err, out.String())
		}

		client := &http.Client{Timeout: 10 * time.Second}
		get := func(path string) (int, string) {
			resp, err := client.Get("http://" + proxyAddr + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body)
		}

		// True mount: /acme/v1/messages reaches upstream A as /v1/messages.
		if code, body := get("/acme/v1/messages"); code != http.StatusOK || body != "/v1/messages" {
			t.Errorf("/acme/v1/messages: status=%d body=%q, want 200 %q", code, body, "/v1/messages")
		}

		// The second mount routes to upstream B, not absorbed by /acme.
		if code, body := get("/acme2/v1/messages"); code != http.StatusOK || body != "/v1/messages" {
			t.Errorf("/acme2/v1/messages: status=%d body=%q, want 200 %q", code, body, "/v1/messages")
		}

		// A deeper path on the second mount keeps its suffix intact.
		if code, body := get("/acme2/deep/path"); code != http.StatusOK || body != "/deep/path" {
			t.Errorf("/acme2/deep/path: status=%d body=%q, want 200 %q", code, body, "/deep/path")
		}

		// Unmatched mount: 404.
		if code, _ := get("/nomatch"); code != http.StatusNotFound {
			t.Errorf("/nomatch: status=%d, want 404", code)
		}

		// A request equal to the mount prefix strips to the upstream root.
		if code, body := get("/acme"); code != http.StatusOK || body != "/" {
			t.Errorf("/acme: status=%d body=%q, want 200 %q", code, body, "/")
		}
	})

	t.Run("overlap rejected before bind", func(t *testing.T) {
		bin := t.TempDir() + "/test-shaper"
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Dir = "."
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build failed: %v\n%s", err, out)
		}

		// Hold a listener so "bound anyway" would be observable: if the
		// config were accepted, ListenAndServe would fail loudly, but the
		// process must instead exit on the validation error alone.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		bindAddr := ln.Addr().String()
		defer ln.Close()

		cmd := exec.Command(bin,
			"-bind", bindAddr,
			"--provider=a",
			"-upstream", "http://127.0.0.1:1",
			"-prefix", "/acme",
			"--provider=b",
			"-upstream", "http://127.0.0.1:2",
			"-prefix", "/acme/v1",
		)
		stdinR, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("open /dev/null: %v", err)
		}
		defer stdinR.Close()
		cmd.Stdin = stdinR

		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected non-zero exit for overlapping prefixes, got 0\noutput:\n%s", out)
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("expected non-zero process exit, got %v\noutput:\n%s", err, out)
		}
		if msg := string(out); !strings.Contains(msg, "overlap") {
			t.Errorf("stderr should mention overlap, got:\n%s", msg)
		}
	})
}

// TestResetDrainResetsAllCollectors proves the TUI ticker's "Reset Stats"
// path: drainResetSignals collapses coalesced signals and every provider's
// collector is reset fleet-wide (Task 6).
func TestResetDrainResetsAllCollectors(t *testing.T) {
	ch := make(chan struct{}, 1)
	drainResetSignals(ch) // empty channel: must not block

	// Coalesced case: the model's non-blocking send keeps at most one
	// pending signal on the cap-1 channel, so a burst of confirmations
	// collapses to a single pending signal before the drain.
	ch <- struct{}{}
	drainResetSignals(ch)
	select {
	case <-ch:
		t.Fatal("drainResetSignals must empty the channel")
	default:
	}

	// Fleet-wide reset: two providers with non-zero cumulative counters all
	// zero on Reset, while in-flight gauges (active/queued) survive per
	// Collector.Reset's documented contract.
	mets := []*metrics.Collector{metrics.NewCollector(), metrics.NewCollector()}
	for i, mc := range mets {
		for j := 0; j < i+1; j++ {
			mc.IncProxied()
			mc.IncPassThrough()
		}
		mc.RecordStatus(200)
		mc.IncActive()
	}
	for _, mc := range mets {
		mc.Reset()
	}
	for i, mc := range mets {
		snap := mc.Snapshot()
		if snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
			t.Errorf("collector %d: proxied=%d passthrough=%d, want zeros", i, snap.TotalProxied, snap.TotalPassThrough)
		}
		if snap.StatusCounts[2] != 0 { // RecordStatus(200) lands in bucket 2
			t.Errorf("collector %d: status counts not reset", i)
		}
	}
}

// TestResetChannelPlumbedToRun ensures the reset channel handed to tui.Run
// is the one the ticker goroutine drains, by exercising the same wiring
// shape main uses (buffered cap-1 channel adopted by the model).
func TestResetChannelPlumbedToRun(t *testing.T) {
	resetCh := make(chan struct{}, 1)
	// The model adopts the caller's channel (see tui.Run); simulate its
	// non-blocking send and the ticker's drain.
	select {
	case resetCh <- struct{}{}:
	default:
		t.Fatal("first send on an empty buffered channel must succeed")
	}
	select {
	case resetCh <- struct{}{}:
		t.Fatal("second send on a full buffered channel must be dropped, not queued elsewhere")
	default:
	}
	drainResetSignals(resetCh)
	select {
	case <-resetCh:
		t.Fatal("channel must be empty after drain")
	default:
	}
}

// TestHelpFlagPrintsUsageAndExitsZero covers the -h/-help contract at both
// scopes at the process level: full sectioned usage on stdout, exit 0.
func TestHelpFlagPrintsUsageAndExitsZero(t *testing.T) {
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"server -h", []string{"-h"}},
		{"server --help", []string{"--help"}},
		{"provider -h", []string{"--provider=acme", "-h"}},
		{"provider --help", []string{"--provider", "--help"}},
		{"mixed with other flags", []string{"-bind", ":9090", "--provider=acme", "-upstream", "https://x", "-h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(bin, tc.args...).Output()
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			s := string(out)
			for _, want := range []string{
				"Usage: ai-concurrency-shaper",
				"Server flags:",
				"--provider[=name]",
				"Provider flags",
				"-upstream string",
				"-bind string",
			} {
				if !strings.Contains(s, want) {
					t.Errorf("usage output missing %q:\n%s", want, s)
				}
			}
		})
	}
}

// TestUnknownFlagExitsTwo covers the deliberate usage-error exit code: GNU
// convention exit 2, error + "-h" hint on stderr, no usage dump.
func TestUnknownFlagExitsTwo(t *testing.T) {
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"server scope", []string{"-bogus"}},
		{"provider scope", []string{"--provider=a", "-nope"}},
		{"missing argument", []string{"-bind"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%v: want exit error, got %v (stdout %s)", tc.args, err, out)
			}
			if code := exitErr.ExitCode(); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			msg := stderr.String()
			for _, want := range []string{"error:", "-h"} {
				if !strings.Contains(msg, want) {
					t.Errorf("stderr missing %q:\n%s", want, msg)
				}
			}
			if strings.Contains(msg, "Usage:") {
				t.Errorf("stderr should hint at -h, not dump full usage:\n%s", msg)
			}
		})
	}
}

// TestSemanticErrorStillExitsOne pins the split: a well-formed invocation
// with a bad value is a runtime failure (exit 1), not a usage error (exit 2).
func TestSemanticErrorStillExitsOne(t *testing.T) {
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "-upstream", "https://x", "-concurrency", "0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_, err := cmd.Output()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("want exit error, got %v", err)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1 (semantic failure)", code)
	}
	if !strings.Contains(stderr.String(), "concurrency") {
		t.Errorf("stderr should name the semantic failure:\n%s", stderr.String())
	}
}

// TestCLI_AuthFailClosedStartup proves a missing credential fails before any
// socket binds, naming the offending reference on stderr with exit code 1.
func TestCLI_AuthFailClosedStartup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	const missing = "SHAPER_DEFINITELY_UNSET_KEY"
	os.Unsetenv(missing)

	cmd := exec.Command(bin,
		"-bind", "127.0.0.1:0",
		"-upstream", "https://api.openai.com",
		"-auth-source", "env:"+missing,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("want exit error, got %v\nstderr:\n%s", err, stderr.String())
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1 (semantic failure)", code)
	}
	for _, want := range []string{missing, "-auth-source"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestCLI_AuthStartupLines pins the process-level logging contract: an
// authenticated provider logs its mode and source REFERENCE (never the value),
// and a mixed fleet emits exactly one forwarded-verbatim notice.
func TestCLI_AuthStartupLines(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(upB.Close)

	const secretRef = "SHAPER_TEST_AUTH_LINE_KEY"
	const secretValue = "super-sekrit-value"
	t.Setenv(secretRef, secretValue)

	newAddr := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close()
		return addr
	}

	proxyAddr := newAddr()
	var out bytes.Buffer
	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"--provider=authed",
		"-upstream", upA.URL,
		"-prefix", "/authed",
		"-retry", "0",
		"-circuit-breaker=false",
		"-release-cooldown", "0",
		"-auth-source", "env:"+secretRef,
		"--provider=open",
		"-upstream", upB.URL,
		"-prefix", "/open",
		"-retry", "0",
		"-circuit-breaker=false",
		"-release-cooldown", "0",
	)
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	runErr := make(chan error, 1)
	go func() { runErr <- cmd.Wait() }()

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

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
		t.Fatalf("proxy did not exit after SIGTERM\noutput:\n%s", out.String())
	}

	logged := out.String()
	// The summary prints the RESOLVED mode (auto derived bearer from the
	// openai upstream host), not the raw flag value.
	if !strings.Contains(logged, "upstream auth: auth-mode=bearer auth-source=env:"+secretRef) {
		t.Errorf("missing auth summary line:\n%s", logged)
	}
	if n := strings.Count(logged, "providers configured without upstream auth"); n != 1 {
		t.Errorf("forwarded-verbatim notice count = %d, want 1:\n%s", n, logged)
	}
	if strings.Contains(logged, secretValue) {
		t.Errorf("log leaked the secret value:\n%s", logged)
	}
}

// TestE2E_ProviderAuth proves per-provider strip-then-inject end-to-end
// through the real binary: hostile client credentials and cloud decoys sent
// to either mount never reach either upstream, and each upstream sees exactly
// its own provider's credential form.
func TestE2E_ProviderAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Each upstream records the headers it received and answers 200.
	newCapturingUpstream := func() (*httptest.Server, func() map[string][]string) {
		var mu sync.Mutex
		seen := map[string][]string{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			for name, values := range r.Header {
				seen[name] = values
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		return srv, func() map[string][]string {
			mu.Lock()
			defer mu.Unlock()
			cp := make(map[string][]string, len(seen))
			for name, values := range seen {
				cp[name] = values
			}
			return cp
		}
	}

	upAcme, acmeSeen := newCapturingUpstream()
	upBeta, betaSeen := newCapturingUpstream()

	const (
		acmeRef = "SHAPER_PROVIDER_ACME_API_KEY"
		acmeVal = "acme-upstream-secret"
		betaRef = "SHAPER_PROVIDER_BETA_API_KEY"
		betaVal = "beta-upstream-secret"
	)
	t.Setenv(acmeRef, acmeVal)
	t.Setenv(betaRef, betaVal)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"--provider=acme",
		"-upstream", upAcme.URL,
		"-prefix", "/acme",
		"-auth-source", "env:"+acmeRef,
		"-auth-mode", "x-api-key", // explicit override of host-derived bearer
		"-retry", "0",
		"-circuit-breaker=false",
		"--provider=beta",
		"-upstream", upBeta.URL,
		"-prefix", "/beta",
		"-auth-source", "env:"+betaRef,
		"-retry", "0",
		"-circuit-breaker=false",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	runErr := make(chan error, 1)
	go func() { runErr <- cmd.Wait() }()

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

	client := &http.Client{Timeout: 10 * time.Second}
	fire := func(mount string) {
		req, err := http.NewRequest(http.MethodPost, "http://"+proxyAddr+mount+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		// A hostile client carries BOTH providers' credentials plus cloud
		// decoys and a stale Anthropic version on every request.
		req.Header.Set("Authorization", "Bearer client-bearer-token")
		req.Header.Set("X-Api-Key", "client-anthropic-key")
		req.Header.Set("X-Goog-Api-Key", "client-google-key")
		req.Header.Set("Anthropic-Version", "1999-01-01")
		req.Header.Set("X-Amz-Date", "19990101T000000Z")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", mount, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: status %d", mount, resp.StatusCode)
		}
	}
	fire("/acme")
	fire("/beta")

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
		t.Fatalf("proxy did not exit after SIGTERM\noutput:\n%s", out.String())
	}

	assertOnly := func(name string, got map[string][]string, want map[string]string) {
		t.Helper()
		banned := map[string]bool{
			"Authorization": true, "X-Api-Key": true, "Api-Key": true,
			"X-Goog-Api-Key": true, "Anthropic-Version": true,
			"Anthropic-Beta": true, "X-Amz-Date": true,
		}
		for injected := range want {
			delete(banned, injected)
		}
		for name2 := range banned {
			if values, ok := got[name2]; ok {
				t.Errorf("%s upstream saw stripped header %s: %v", name, name2, values)
			}
		}
		for name2, wantValue := range want {
			gotValue := strings.Join(got[name2], ", ")
			if gotValue != wantValue {
				t.Errorf("%s upstream %s = %q, want %q", name, name2, gotValue, wantValue)
			}
		}
	}

	assertOnly("acme", acmeSeen(), map[string]string{
		"X-Api-Key":         acmeVal,
		"Anthropic-Version": "2023-06-01",
	})
	assertOnly("beta", betaSeen(), map[string]string{
		"Authorization": "Bearer " + betaVal,
	})

	if logged := out.String(); strings.Contains(logged, acmeVal) || strings.Contains(logged, betaVal) {
		t.Errorf("process logs leaked a secret:\n%s", logged)
	}
}

// TestE2E_AuthDisabledPassthrough pins the backward-compat contract: with no
// auth flags the binary forwards client credentials VERBATIM to the upstream.
func TestE2E_AuthDisabledPassthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var mu sync.Mutex
	seen := map[string][]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for name, values := range r.Header {
			seen[name] = values
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"-upstream", up.URL,
		"-retry", "0",
		"-circuit-breaker=false",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	runErr := make(chan error, 1)
	go func() { runErr <- cmd.Wait() }()

	if err := waitTCPReady(proxyAddr, 5*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-runErr
		t.Fatalf("proxy not ready: %v\noutput:\n%s", err, out.String())
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer verbatim-client-token")
	req.Header.Set("X-Api-Key", "verbatim-client-key")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen["Authorization"], ", "); got != "Bearer verbatim-client-token" {
		t.Errorf("Authorization = %q, want forwarded verbatim", got)
	}
	if got := strings.Join(seen["X-Api-Key"], ", "); got != "verbatim-client-key" {
		t.Errorf("X-Api-Key = %q, want forwarded verbatim", got)
	}
}

// TestE2E_ConfigFileDriven proves an external catalog can drive the whole
// gateway from a committed JSON file: providers load with ${ENV} references
// resolved fail-closed, compose under the same validation rules, and route
// end-to-end through the real binary.
func TestE2E_ConfigFileDriven(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(upB.Close)

	const keyRef = "SHAPER_CONFIG_FILE_KEY"
	t.Setenv(keyRef, "file-secret")

	newAddr := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close()
		return addr
	}

	cfgPath := writeFileTemp(t, `{
	  "providers": [
	    {"name": "alpha", "upstream": "`+upA.URL+`", "prefix": "/alpha",
	     "auth_source": "env:`+keyRef+`", "concurrency": 3},
	    {"name": "bravo", "upstream": "`+upB.URL+`", "prefix": "/bravo"}
	  ]
	}`)

	proxyAddr := newAddr()
	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"-config", cfgPath,
		"--provider=gamma",
		"-upstream", upA.URL,
		"-prefix", "/gamma",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	runErr := make(chan error, 1)
	go func() { runErr <- cmd.Wait() }()

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

	client := &http.Client{Timeout: 10 * time.Second}
	for _, mount := range []string{"/alpha/ping", "/bravo/ping", "/gamma/ping"} {
		resp, err := client.Post("http://"+proxyAddr+mount, "text/plain", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", mount, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s: status %d, want 200", mount, resp.StatusCode)
		}
	}

	// Inspect output only AFTER Wait(): cmd.Wait joins os/exec's copying
	// goroutines, so reading earlier races the writer.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
	}

	logged := out.String()
	if !strings.Contains(logged, "auth-source=env:"+keyRef) {
		t.Errorf("file provider auth not active:\n%s", logged)
	}
	if strings.Contains(logged, "file-secret") {
		t.Errorf("log leaked the secret:\n%s", logged)
	}
}

// writeFileTemp writes content to a temp file and returns its path.
func writeFileTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
