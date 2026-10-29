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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// committedStreamPost sends a streaming request through p and returns the
// whole response body.
func committedStreamPost(t *testing.T, p *Proxy) string {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by the early SSE commit)", resp.StatusCode)
	}
	return string(body)
}

// TestProxy_QueueCommentsTransportFailureRecordsBreakerFailure pins that a
// dead upstream is an upstream failure whether or not the queue comments had
// committed the streaming representation. Before the fix the committed branch
// of the transport error handler wrote its failed-close comment but recorded
// no failure status, so classification read the commit's 200 and the breaker
// never saw the failure.
func TestProxy_QueueCommentsTransportFailureRecordsBreakerFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstreamURL, err := url.Parse(dead.URL)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()

	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	pat, _ := route.Parse("POST /messages:1")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatal(err)
	}

	body := committedStreamPost(t, p)
	if !strings.Contains(body, ": queue-wait failed: upstream") {
		t.Fatalf("transport failure must emit the upstream failed-close comment: %q", body)
	}
	stats := waitForOutcome(t, breaker, 1)
	if stats.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", stats.ConsecutiveFailures)
	}
}

// TestProxy_QueueCommentsLocalPanicFailsCloseLocally pins that a local panic
// after the queue comments committed the stream ends it with an explicit
// local failed-close comment. Before the fix the recovery's 502 was skipped
// (the commit had already written a terminal status), so the client saw the
// queue comments and then a silent clean EOF.
func TestProxy_QueueCommentsLocalPanicFailsCloseLocally(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	body := committedStreamPost(t, p)
	if !strings.Contains(body, ": queue-wait failed: internal") {
		t.Fatalf("a local panic must close the committed stream with a local failed-close comment: %q", body)
	}
	if strings.Contains(body, ": queue-wait failed: upstream") {
		t.Fatalf("a local panic must not blame the upstream: %q", body)
	}
}

// TestRecoverLocalPanicClassification pins the recorder facts a recovered
// panic leaves behind for each response state: proxy-generated only when no
// upstream status was observed, and a committed stream's failure recorded in
// the journal entry.
func TestRecoverLocalPanicClassification(t *testing.T) {
	t.Run("before any status", func(t *testing.T) {
		rr := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: rr}
		rec.recoverLocalPanic()
		if !rec.aborted || !rec.proxyGeneratedError || rr.Code != http.StatusBadGateway {
			t.Fatalf("aborted=%v proxyGenerated=%v code=%d, want true true 502", rec.aborted, rec.proxyGeneratedError, rr.Code)
		}
	})
	t.Run("after a delivered upstream status", func(t *testing.T) {
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		rec.WriteHeader(http.StatusInternalServerError)
		rec.recoverLocalPanic()
		if !rec.aborted {
			t.Fatal("aborted = false, want true")
		}
		if rec.proxyGeneratedError {
			t.Fatal("proxyGeneratedError = true, want false: the 500 came from the upstream")
		}
	})
	t.Run("committed stream with no upstream status", func(t *testing.T) {
		rr := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: rr, commentsCommitted: true, entry: &journal.Entry{}}
		rec.recordImplicitOK(nil)
		rec.recoverLocalPanic()
		if !rec.proxyGeneratedError {
			t.Fatal("proxyGeneratedError = false, want true: no upstream status was observed")
		}
		if rec.upstreamStatusOverride != http.StatusBadGateway || rec.entry.StatusCode != http.StatusBadGateway {
			t.Fatalf("override=%d entry status=%d, want 502 502", rec.upstreamStatusOverride, rec.entry.StatusCode)
		}
		if !strings.Contains(rr.Body.String(), ": queue-wait failed: internal") {
			t.Fatalf("body = %q, want the local failed-close comment", rr.Body.String())
		}
	})
	t.Run("committed stream after a delivered upstream 200", func(t *testing.T) {
		rr := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: rr, commentsCommitted: true, entry: &journal.Entry{}}
		rec.WriteHeader(http.StatusOK) // the commit's own status
		rec.recordImplicitOK(nil)
		rec.WriteHeader(http.StatusOK) // the upstream's, silenced by the commit
		rec.recoverLocalPanic()
		if rec.proxyGeneratedError {
			t.Fatal("proxyGeneratedError = true, want false: the upstream answered 200")
		}
		if rec.upstreamStatusOverride != 0 || rec.entry.StatusCode != http.StatusOK {
			t.Fatalf("override=%d entry status=%d, want 0 200", rec.upstreamStatusOverride, rec.entry.StatusCode)
		}
		if strings.Contains(rr.Body.String(), ": queue-wait failed: internal") {
			t.Fatalf("body = %q, want no failed-close comment after a delivered upstream status", rr.Body.String())
		}
	})
	t.Run("committed stream with an upstream failure status", func(t *testing.T) {
		rr := httptest.NewRecorder()
		rec := &statusRecorder{ResponseWriter: rr, commentsCommitted: true}
		rec.recordImplicitOK(nil)
		rec.WriteHeader(http.StatusTooManyRequests)
		rec.recoverLocalPanic()
		if rec.proxyGeneratedError {
			t.Fatal("proxyGeneratedError = true, want false: the 429 came from the upstream")
		}
		if strings.Contains(rr.Body.String(), ": queue-wait failed: internal") {
			t.Fatalf("body = %q, the upstream failure was already framed", rr.Body.String())
		}
	})
}
