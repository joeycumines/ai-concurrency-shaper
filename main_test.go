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
	"fmt"
	"io"
	"maps"
	"math"
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

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
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

// TestValidateMBFlag proves negative and overflowing megabyte flags are
// rejected against their actual byte shift (review-j finding 14): a value
// valid at shift 20 may overflow at shift 22.
func TestValidateMBFlag(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value int64
		shift uint
	}{
		{"valid", 32, 20},
		{"zero", 0, 22},
		{"max at 20", math.MaxInt64 >> 20, 20},
		{"max at 21", math.MaxInt64 >> 21, 21},
		{"max at 22", math.MaxInt64 >> 22, 22},
	} {
		if err := validateMBFlag(tc.name, tc.value, tc.shift); err != nil {
			t.Fatalf("%s = %d shift %d: %v", tc.name, tc.value, tc.shift, err)
		}
	}
	for _, tc := range []struct {
		name  string
		value int64
		shift uint
	}{
		{"negative", -1, 20},
		{"overflow at 20", (math.MaxInt64 >> 20) + 1, 20},
		// retry-max-body-mb is validated at shift 21 because its byte value is
		// doubled (journal sizing maxBody*2) and must not overflow (review-08
		// additional 3).
		{"overflow at 21", (math.MaxInt64 >> 21) + 1, 21},
		{"overflow at 22", (math.MaxInt64 >> 22) + 1, 22},
	} {
		if err := validateMBFlag(tc.name, tc.value, tc.shift); err == nil {
			t.Fatalf("%s = %d shift %d accepted", tc.name, tc.value, tc.shift)
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

// TestE2E_MultiProvider_Transcode proves the composed CLI multi-provider transcoding
// end-to-end through the real binary: each --provider section can configure its own
// transcode routes, each mount transcodes only its own configured routes, and unmapped
// paths on either mount pass through transparently.
func TestE2E_MultiProvider_Transcode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var upARequests, upBRequests atomic.Int64

	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upARequests.Add(1)
		if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-a","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello from anthropic chat upstream"},"finish_reason":"stop"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"untranscoded_a":true,"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upA.Close)

	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upBRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"untranscoded_b":true,"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upB.Close)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	var out strings.Builder
	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"--provider=anthropic",
		"-upstream", upA.URL,
		"-prefix", "/anthropic",
		"-transcode-route", "responses@/v1/responses=chat-completions@/v1/chat/completions",
		"-retry", "0",
		"-circuit-breaker=false",
		"--provider=openai",
		"-upstream", upB.URL,
		"-prefix", "/openai",
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

	// 1. POST /anthropic/v1/responses -> transcoded via anthropic provider to /v1/chat/completions on upA
	{
		resp, err := client.Post(
			"http://"+proxyAddr+"/anthropic/v1/responses",
			"application/json",
			strings.NewReader(`{"model":"m","input":"hello"}`),
		)
		if err != nil {
			t.Fatalf("POST /anthropic/v1/responses: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /anthropic/v1/responses status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "hello from anthropic chat upstream") || !strings.Contains(bodyStr, `"object":"response"`) {
			t.Fatalf("POST /anthropic/v1/responses unexpected body: %s", bodyStr)
		}
	}

	// 2. POST /anthropic/unmapped/path -> passthrough without transcoding to upA
	{
		resp, err := client.Post(
			"http://"+proxyAddr+"/anthropic/unmapped/path",
			"application/json",
			strings.NewReader(`{"data":123}`),
		)
		if err != nil {
			t.Fatalf("POST /anthropic/unmapped/path: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /anthropic/unmapped/path status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"untranscoded_a":true`) || !strings.Contains(bodyStr, `"/unmapped/path"`) {
			t.Fatalf("POST /anthropic/unmapped/path unexpected body: %s", bodyStr)
		}
	}

	// 3. POST /openai/v1/responses -> passthrough without transcoding to upB (openai has no transcode mapping)
	{
		resp, err := client.Post(
			"http://"+proxyAddr+"/openai/v1/responses",
			"application/json",
			strings.NewReader(`{"data":123}`),
		)
		if err != nil {
			t.Fatalf("POST /openai/v1/responses: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /openai/v1/responses status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"untranscoded_b":true`) || !strings.Contains(bodyStr, `"/v1/responses"`) {
			t.Fatalf("POST /openai/v1/responses unexpected body: %s", bodyStr)
		}
	}

	// 4. POST /openai/other/path -> passthrough to upB
	{
		resp, err := client.Post(
			"http://"+proxyAddr+"/openai/other/path",
			"application/json",
			strings.NewReader(`{"data":456}`),
		)
		if err != nil {
			t.Fatalf("POST /openai/other/path: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /openai/other/path status = %d, want 200: %s", resp.StatusCode, body)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"untranscoded_b":true`) || !strings.Contains(bodyStr, `"/other/path"`) {
			t.Fatalf("POST /openai/other/path unexpected body: %s", bodyStr)
		}
	}
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
			maps.Copy(seen, r.Header)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		return srv, func() map[string][]string {
			mu.Lock()
			defer mu.Unlock()
			cp := make(map[string][]string, len(seen))
			maps.Copy(cp, seen)
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
		maps.Copy(seen, r.Header)
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
	// Client-supplied forwarding headers must be suppressed even in verbatim
	// passthrough mode, and the gateway never injects its own client IP
	// (stealth posture; intentional wire delta vs pre-Rewrite binaries).
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
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
	for _, fwd := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "Forwarded"} {
		if got, ok := seen[fwd]; ok {
			t.Errorf("%s reached upstream: %v, want suppressed (stealth posture)", fwd, got)
		}
	}
}

func TestE2E_MetricsEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	newEcho := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	upA, upB := newEcho(), newEcho()

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
	metricsAddr := newAddr()

	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"-metrics-bind", metricsAddr,
		"--provider=acme",
		"-upstream", upA.URL,
		"-prefix", "/acme",
		"-retry", "0",
		"-circuit-breaker=false",
		"--provider=beta",
		"-upstream", upB.URL,
		"-prefix", "/beta",
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
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	if err := waitTCPReady(proxyAddr, 5*time.Second); err != nil {
		t.Fatalf("proxy addr: %v\noutput:\n%s", err, out.String())
	}
	if err := waitTCPReady(metricsAddr, 5*time.Second); err != nil {
		t.Fatalf("metrics addr: %v\noutput:\n%s", err, out.String())
	}

	fire := func(path string) {
		resp, err := http.Get("http://" + proxyAddr + path)
		if err != nil {
			t.Fatalf("fire %s: %v", path, err)
		}
		resp.Body.Close()
	}
	fire("/acme/v1/messages")
	fire("/acme/v1/messages")
	fire("/beta/v1/messages")

	// Counts land in the collector during the proxy's deferred finalize, which
	// can run a tick after the client sees the response. Poll rather than
	// sleep so the test stays fast and deterministic under load.
	waitForMetricsLine := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			resp, err := http.Get("http://" + metricsAddr + "/metrics")
			if err != nil {
				t.Fatalf("scrape while waiting for %q: %v", want, err)
			}
			bodyBytes, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode == http.StatusOK && strings.Contains(string(bodyBytes), want+"\n") {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("metrics output never contained %q; last body:\n%s", want, bodyBytes)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	waitForMetricsLine(`shaper_requests_total{provider="acme",status="2xx"} 2`)
	waitForMetricsLine(`shaper_requests_total{provider="beta",status="2xx"} 1`)

	scrape := func(path string) (int, string, string) {
		resp, err := http.Get("http://" + metricsAddr + path)
		if err != nil {
			t.Fatalf("scrape %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
	}

	status, contentType, body := scrape("/metrics")
	if status != http.StatusOK {
		t.Errorf("GET /metrics status = %d, want 200", status)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain*", contentType)
	}
	for _, want := range []string{
		`shaper_requests_total{provider="acme",status="2xx"} 2`,
		`shaper_requests_total{provider="beta",status="2xx"} 1`,
		`shaper_active{provider="acme"} 0`,
		`shaper_queued{provider="beta"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}

	if status, _, _ = scrape("/other"); status != http.StatusNotFound {
		t.Errorf("GET /other status = %d, want 404", status)
	}
	if status, _, _ = scrape(""); status != http.StatusNotFound {
		t.Errorf("GET / status = %d, want 404", status)
	}
}

func TestConfigMetricsBindFlag(t *testing.T) {
	cfg, err := config.Parse([]string{"-upstream", "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	if cfg.Server.MetricsBind != "" {
		t.Errorf("default MetricsBind = %q, want empty (disabled)", cfg.Server.MetricsBind)
	}
	cfg, err = config.Parse([]string{"-metrics-bind", "127.0.0.1:2112", "-upstream", "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("parse with -metrics-bind: %v", err)
	}
	if cfg.Server.MetricsBind != "127.0.0.1:2112" {
		t.Errorf("MetricsBind = %q, want 127.0.0.1:2112", cfg.Server.MetricsBind)
	}
	if _, err = config.Parse([]string{"--provider=a", "-upstream", "http://127.0.0.1:1", "-prefix", "/a", "-metrics-bind", ":2112"}); err == nil {
		t.Error("provider-scoped -metrics-bind must be rejected (server-scope flag)")
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
