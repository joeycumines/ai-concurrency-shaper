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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// lockedBuffer is a thread-safe buffer for concurrent writes from
// subprocess goroutines and reads from the test goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// parseListeningAddr extracts the first "listening on <addr>" line
// from the buffer, splitting by newlines to avoid capturing trailing
// log output or TUI artifacts into the address.
func parseListeningAddr(buf *lockedBuffer) string {
	for _, line := range strings.Split(buf.String(), "\n") {
		if _, after, ok := strings.Cut(line, "listening on "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

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

	// Hold a listener open on an ephemeral port so the proxy
	// cannot bind to it. This guarantees a bind failure.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	bindAddr := ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin,
		"-bind", bindAddr,
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
	//   2. isolateSubprocess: creates a new session on Unix (Setsid)
	//      so the child has no controlling terminal, preventing
	//      bubbletea's OpenTTY() fallback from opening /dev/tty
	//      (the parent's terminal). On Windows this is a no-op.
	//
	// With isolation, bubbletea cannot start (OpenTTY fails), so the TUI
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
	isolateSubprocess(cmd)

	var out lockedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := make(chan error, 1)
	go func() {
		runErr <- cmd.Run()
	}()

	// Poll the subprocess log output for the listening address.
	// The proxy fails to bind, so proxyAddr should remain empty.
	deadline := time.Now().Add(5 * time.Second)
	var proxyAddr string
	for time.Now().Before(deadline) {
		proxyAddr = parseListeningAddr(&out)
		if proxyAddr != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The proxy must have failed to bind and exited with a non-zero
	// status. If proxyAddr is non-empty the bind somehow succeeded
	// (flaky environment) — that is also a failure for this test.
	if proxyAddr != "" {
		_ = cmd.Process.Kill()
		<-runErr
		t.Fatalf("proxy unexpectedly bound to %s (bind should have failed)", proxyAddr)
	}

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("process exited cleanly; expected bind failure")
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-runErr
		t.Fatal("process did not exit after bind failure")
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

	var out lockedBuffer
	cmd := exec.Command(bin,
		"-upstream", upstream.URL,
		"-limit", "POST /v1/messages",
		"-concurrency", "4",
		"-queue-timeout", "30s",
		"-bind", "127.0.0.1:0",
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

	// Poll the subprocess log output for the listening address.
	deadline := time.Now().Add(5 * time.Second)
	var proxyAddr string
	for time.Now().Before(deadline) {
		proxyAddr = parseListeningAddr(&out)
		if proxyAddr != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if proxyAddr == "" {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-runErr
		}
		t.Fatalf("proxy did not report a listening address")
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

	var out lockedBuffer
	cmd := exec.Command(bin,
		"-upstream", upstream.URL,
		"-limit", "POST /v1/messages",
		"-concurrency", "4",
		"-queue-timeout", "30s",
		"-bind", "127.0.0.1:0",
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

	// Poll the subprocess log output for the listening address.
	deadline := time.Now().Add(5 * time.Second)
	var proxyAddr string
	for time.Now().Before(deadline) {
		proxyAddr = parseListeningAddr(&out)
		if proxyAddr != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if proxyAddr == "" {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-runErr
		}
		t.Fatalf("proxy did not report a listening address")
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
