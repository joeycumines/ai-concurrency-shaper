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
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// hookBody runs onFirstRead before the first byte of the request body is
// handed to the proxy, so a test can change shared state exactly while the
// proxy is inspecting the body.
type hookBody struct {
	io.ReadCloser
	once        sync.Once
	onFirstRead func()
}

func (b *hookBody) Read(p []byte) (int, error) {
	b.once.Do(b.onFirstRead)
	return b.ReadCloser.Read(p)
}

// TestProxy_QueueCommentsBreakerRejectionAfterBodyProbeIsPlain503 pins that a
// breaker rejection is decided before the queue-comment commit: when another
// request claims the HALF_OPEN probe while this request's body is being
// inspected, the client receives a plain 503, never a committed SSE stream
// with a failed close that blames the upstream.
func TestProxy_QueueCommentsBreakerRejectionAfterBodyProbeIsPlain503(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	waitForHalfOpenProbeWindow(t, b)
	pat, _ := route.Parse("POST /v1/messages")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(b),
		WithQueueComments(10*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &hookBody{ReadCloser: r.Body, onFirstRead: func() {
			// A competing request takes the recovery probe.
			if _, err := b.Allow(); err != nil {
				t.Errorf("competing Allow = %v, want the probe", err)
			}
		}}
		p.ServeHTTP(w, r)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want no committed SSE stream", ct)
	}
	if strings.Contains(string(body), "queue-wait") {
		t.Fatalf("body = %q, want no queue comments", body)
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
}

// fillQueue holds lim's only slot and parks one waiter on it, so a proxy with
// a queue depth limit of 1 rejects the next request. The returned func
// releases both.
func fillQueue(t *testing.T, lim *queue.Limiter) func() {
	t.Helper()
	release, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if r, err := lim.Acquire(ctx); err == nil {
			r()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for lim.Stats().Waiters < 1 {
		if time.Now().After(deadline) {
			t.Fatal("waiter never registered")
		}
		time.Sleep(time.Millisecond)
	}
	return func() {
		cancel()
		<-done
		release()
	}
}

// TestProxy_QueueDepthRejectionLeavesBreakerUntouched pins that a request
// refused by the queue depth bound never mutates the breaker: an elapsed OPEN
// period is not turned into a HALF_OPEN with no probe, and an in-flight probe
// that outlived the open timeout can still close the breaker with its result.
func TestProxy_QueueDepthRejectionLeavesBreakerUntouched(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	newProxy := func(t *testing.T, b *circuitbreaker.Breaker, lim *queue.Limiter) *Proxy {
		t.Helper()
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher([]route.Pattern{pat})),
			WithLimiter(lim),
			WithMetrics(metrics.NewCollector()),
			WithBreaker(b),
			WithQueueDepthLimit(1),
		)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	newBreaker := func(t *testing.T) *circuitbreaker.Breaker {
		return mustBreaker(t,
			circuitbreaker.WithFailureThreshold(2),
			circuitbreaker.WithWindow(10*time.Second),
			circuitbreaker.WithOpenTimeout(20*time.Millisecond),
			circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
		)
	}

	t.Run("elapsed open period stays open", func(t *testing.T) {
		b := newBreaker(t)
		waitForHalfOpenProbeWindow(t, b)
		lim := queue.NewLimiterWithCooldown(1, 0)
		p := newProxy(t, b, lim)
		defer fillQueue(t, lim)()

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, committedOrPlainRequest(context.Background(), false))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if got := b.State(); got != circuitbreaker.Open {
			t.Fatalf("breaker state = %s, want OPEN (a rejected request admitted no probe)", got)
		}
	})

	t.Run("superseded probe result still counts", func(t *testing.T) {
		b := newBreaker(t)
		waitForHalfOpenProbeWindow(t, b)
		epoch, err := b.Allow()
		if err != nil || epoch == 0 {
			t.Fatalf("Allow = (%d, %v), want a probe", epoch, err)
		}
		time.Sleep(30 * time.Millisecond)
		lim := queue.NewLimiterWithCooldown(1, 0)
		p := newProxy(t, b, lim)
		defer fillQueue(t, lim)()

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, committedOrPlainRequest(context.Background(), false))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		b.RecordSuccess(time.Now(), epoch)
		if got := b.State(); got != circuitbreaker.Closed {
			t.Fatalf("breaker state = %s, want CLOSED from the in-flight probe's success", got)
		}
	})
}

// TestProxy_QueueCommentsStalledUploadHonoursQueueTimeout pins that the body
// inspection preceding the queue-comment commit is bounded by the queue
// timeout on a real HTTP/1.1 connection: a client that sends its headers and
// part of its body, then stalls, gets the queue-timeout answer instead of
// pinning the handler for as long as it keeps the connection open.
func TestProxy_QueueCommentsStalledUploadHonoursQueueTimeout(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueComments(10*time.Millisecond),
		WithQueueTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const partial = `{"model":"m"`
	if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\n"+
		"Host: proxy.test\r\n"+
		"Accept: text/event-stream\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: 100\r\n\r\n"+partial); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response while the upload is stalled (after %v): %v", time.Since(start), err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 queue timeout", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("response took %v, want it bounded by the queue timeout", elapsed)
	}
}

// TestStatusRecorderCommittedDuplicate2xxKeepsFirstObservation pins that
// only the first status written after the queue-comment commit is the
// upstream's: a superfluous second 2xx (ignored by net/http on an uncommitted
// exchange) must not rewrite the recorded status or response timing.
func TestStatusRecorderCommittedDuplicate2xxKeepsFirstObservation(t *testing.T) {
	rec := &statusRecorder{
		ResponseWriter:    httptest.NewRecorder(),
		commentsCommitted: true,
		terminalWritten:   true,
	}
	rec.WriteHeader(http.StatusCreated)
	first := rec.responseAt
	time.Sleep(2 * time.Millisecond)
	rec.WriteHeader(http.StatusAccepted)
	if got := rec.observedStatus(); got != http.StatusCreated {
		t.Fatalf("observed status = %d, want the first post-commit status %d", got, http.StatusCreated)
	}
	if !rec.responseAt.Equal(first) {
		t.Fatalf("responseAt moved from %v to %v on a superfluous WriteHeader", first, rec.responseAt)
	}
}
