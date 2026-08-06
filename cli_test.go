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
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if _, after, ok := strings.Cut(line, "listening on "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// proxySubprocess owns exactly one cmd.Wait: the goroutine started by
// startProxyCmd. done is closed when Wait returns, and waitErr is assigned
// before done is closed, so reading it after <-done is race-free. wait() is
// non-destructive: any number of callers may wait on the same process.
type proxySubprocess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
}

// wait blocks until the process exits (returning its Wait error, or nil) or
// the timeout elapses. Multiple callers may call it concurrently; the first
// to observe the exit reaps the result for all of them.
func (s *proxySubprocess) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-s.done:
		if s.waitErr != nil {
			return fmt.Errorf("proxy exited with error: %v", s.waitErr)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("proxy did not exit within %s", timeout)
	}
}

// startProxyCmd builds the proxy binary and starts it, owning exactly one
// cmd.Wait via a goroutine. The returned cleanup (via t.Cleanup) terminates
// the process and reaps the result; it never calls cmd.Wait again (Go
// rejects a second wait).
func startProxyCmd(t *testing.T, args ...string) (*proxySubprocess, *lockedBuffer) {
	t.Helper()

	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var out lockedBuffer
	cmd := exec.Command(bin, args...)
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { stdinR.Close() })
	cmd.Stdin = stdinR
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v\n%s", err, out.String())
	}

	sub := &proxySubprocess{
		cmd:  cmd,
		done: make(chan struct{}),
	}
	go func() {
		sub.waitErr = cmd.Wait()
		close(sub.done)
	}()

	t.Cleanup(func() {
		if sub.cmd.Process == nil {
			return
		}
		_ = sub.cmd.Process.Signal(syscall.SIGTERM)
		if err := sub.wait(t, 5*time.Second); err != nil {
			_ = sub.cmd.Process.Kill()
			// Best effort: a descendant may still hold the stdio pipes.
			// The test is over; do not block forever on the reap.
			_ = sub.wait(t, 5*time.Second)
		}
	})

	return sub, &out
}

// waitForListeningAddr polls the subprocess output for the listening address.
func (s *proxySubprocess) waitForListeningAddr(t *testing.T, out *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		addr := parseListeningAddr(out)
		if addr != "" {
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	// Reap if the signal sufficed; the cleanup force-kills otherwise.
	_ = s.wait(t, 5*time.Second)
	t.Fatalf("proxy did not report a listening address")
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
	// The bind failure is hit in run() before the TUI goroutine starts:
	// net.Listen returns an error, run() returns it, and main() exits with
	// a non-zero status via log.Fatalf — the TUI never initializes. The
	// isolation above is still defense-in-depth for the (flaky)
	// alternative where the bind unexpectedly succeeds: it keeps bubbletea
	// from taking control of the parent's terminal before the test's
	// timeout kills the process.
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

	sub, out := startProxyCmd(t,
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

	proxyAddr := sub.waitForListeningAddr(t, out)

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

	_ = sub.cmd.Process.Signal(syscall.SIGTERM)

	if err := sub.wait(t, 5*time.Second); err != nil {
		_ = sub.cmd.Process.Kill()
		if kerr := sub.wait(t, 5*time.Second); kerr != nil {
			t.Fatalf("proxy did not exit even after SIGKILL (%v)\noutput:\n%s", kerr, out.String())
		}
		t.Fatalf("proxy did not exit after SIGTERM (%v)\noutput:\n%s", err, out.String())
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

	sub, out := startProxyCmd(t,
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

	proxyAddr := sub.waitForListeningAddr(t, out)

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

	_ = sub.cmd.Process.Signal(syscall.SIGTERM)

	if err := sub.wait(t, 5*time.Second); err != nil {
		_ = sub.cmd.Process.Kill()
		if kerr := sub.wait(t, 5*time.Second); kerr != nil {
			t.Fatalf("proxy did not exit even after SIGKILL (%v)\noutput:\n%s", kerr, out.String())
		}
		t.Fatalf("proxy did not exit after SIGTERM (%v)\noutput:\n%s", err, out.String())
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

	// The upstream tracks concurrent requests so the shared limiter's cap of
	// 3 is observable: with 4 concurrent requests across the two routes, the
	// fourth must block until one of the first three completes. The message
	// handlers sleep briefly so admitted requests overlap deterministically;
	// without the sleep, instantaneous handlers can serialize at the
	// scheduler level under -race and the observed peak never reaches the
	// limiter cap even though the limiter admitted them concurrently.
	var (
		mu           sync.Mutex
		active       int
		peakActive   int
		releaseFirst = make(chan struct{})
		started      = make(chan struct{})
		startedOnce  sync.Once
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > peakActive {
			peakActive = active
		}
		startedOnce.Do(func() { close(started) })
		mu.Unlock()

		// The first request holds the slot until released; the others hold
		// it long enough to be observed overlapping.
		if r.URL.Path == "/v1/chat/completions" {
			<-releaseFirst
		} else {
			time.Sleep(200 * time.Millisecond)
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)

		mu.Lock()
		active--
		mu.Unlock()
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	met := metrics.NewCollector()
	limiter := queue.NewLimiterWithCooldown(10, 0)
	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(matcher),
		proxy.WithLimiter(limiter),
		proxy.WithMetrics(met),
		proxy.WithRouteLimiters(routeLimiters),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// The first request acquires a slot through the shared group limiter and
	// holds it at the upstream.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach the upstream")
	}

	// Two more requests can acquire the remaining two slots, but a fourth
	// must wait for the first to release.
	done := make(chan struct{})
	for range 3 {
		go func() {
			defer func() { done <- struct{}{} }()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		}()
	}

	// With the first holding a slot, at most 3 requests are active upstream
	// at once; the fourth waits. Poll until exactly 3 are active (they must
	// all be admitted through the shared group limiter), then assert the cap
	// held. A fixed sleep here would be flaky under -race, and failing with
	// the upstream handler still blocked would deadlock the server close.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release() // never leave the upstream handler blocked

	deadline := time.Now().Add(5 * time.Second)
	var peak int
	for {
		mu.Lock()
		peak = peakActive
		mu.Unlock()
		if peak == 3 {
			break
		}
		if peak > 3 {
			release()
			t.Fatalf("peak active upstream requests = %d, want <= 3 (group limiter cap)", peak)
		}
		if time.Now().After(deadline) {
			release()
			t.Fatalf("peak active upstream requests = %d, want 3 (group limiter shared)", peak)
		}
		time.Sleep(10 * time.Millisecond)
	}

	release()
	<-firstDone
	for range 3 {
		<-done
	}

	// Both routes used the shared group limiter: the peak of exactly 3
	// across the two routes proves the group cap applied.
	mu.Lock()
	defer mu.Unlock()
	if peakActive != 3 {
		t.Fatalf("peakActive = %d, want 3", peakActive)
	}
}
