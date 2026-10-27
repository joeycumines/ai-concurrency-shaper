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
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// The queue-comment commit writes the streaming 200 before admission, so the
// recorder's status reads 200 for the rest of the exchange whatever the
// upstream later does. These tests pin that a committed exchange is classified
// from the status it actually observed after the commit, exactly like the
// same exchange without the commit.

// committedOrPlainRequest builds a bodiless limited POST (so the commit needs
// no full-duplex support) that either opts into the queue-comment SSE commit
// or does not.
func committedOrPlainRequest(ctx context.Context, committed bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if committed {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req
}

// TestProxy_QueueCommentsCancelBeforeUpstreamStatusCancelsProbe pins that a
// HALF_OPEN probe whose client cancels before the upstream answered is
// cancelled, never recorded as a success: no upstream status was observed,
// and the commit's 200 is not one.
func TestProxy_QueueCommentsCancelBeforeUpstreamStatusCancelsProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	for _, committed := range []bool{false, true} {
		name := "plain"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(2),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			waitForHalfOpenProbeWindow(t, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
				WithMetrics(metrics.NewCollector()),
				WithBreaker(b),
				WithMaxRetries(0),
				WithQueueComments(time.Hour),
				WithQueueTimeout(10*time.Second),
				WithTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
					cancel()
					return nil, context.Canceled
				})),
			)
			if err != nil {
				t.Fatal(err)
			}
			p.ServeHTTP(httptest.NewRecorder(), committedOrPlainRequest(ctx, committed))

			stats := b.Stats()
			if stats.TotalSuccesses != 0 {
				t.Fatalf("breaker successes = %d, want 0: the upstream never answered", stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state = %s, want HALF_OPEN with the probe released", state)
			}
			epoch, allowErr := b.Allow()
			if allowErr != nil || epoch == 0 {
				t.Fatalf("Allow() = epoch %d err %v, want a fresh HALF_OPEN probe", epoch, allowErr)
			}
			b.CancelProbe(epoch)
		})
	}
}

// TestProxy_QueueCommentsRetryFailureHoldsSlotAfterCancellation pins that an
// upstream failure recorded by an earlier retry attempt holds the slot when the
// client cancels a later attempt, whether or not the stream was committed.
// Before the fix the committed exchange's 200 made the cancelled attempt look
// like a delivered response, so the hold was skipped and the slot recycled
// immediately against a failing upstream.
func TestProxy_QueueCommentsRetryFailureHoldsSlotAfterCancellation(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	for _, committed := range []bool{false, true} {
		name := "plain"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			var cancel context.CancelFunc
			var calls atomic.Int64
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
				WithMetrics(metrics.NewCollector()),
				WithBreaker(nil),
				WithMaxRetries(2),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithCancelCooldown(0),
				WithFailureHold(150*time.Millisecond),
				WithQueueComments(time.Hour),
				WithQueueTimeout(10*time.Second),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					switch calls.Add(1) {
					case 1:
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("first attempt upstream failure")),
							Request:    req,
						}, nil
					case 2:
						cancel()
						return nil, context.Canceled
					default:
						return &http.Response{
							StatusCode: http.StatusOK,
							Status:     "200 OK",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("ok")),
							Request:    req,
						}, nil
					}
				})),
			)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			p.ServeHTTP(httptest.NewRecorder(), committedOrPlainRequest(ctx, committed))
			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls = %d, want the failure plus the cancelled retry", got)
			}

			start := time.Now()
			second := httptest.NewRecorder()
			p.ServeHTTP(second, committedOrPlainRequest(context.Background(), false))
			if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
				t.Fatalf("second request acquired the slot after %v, want the retry-recorded failure to hold it", elapsed)
			}
			if second.Code != http.StatusOK {
				t.Fatalf("second request status = %d, want 200", second.Code)
			}
		})
	}
}

// TestProxy_QueueCommentsCircuitOpenRecords503 pins that a breaker opening
// between retries is journaled as the same proxy-generated 503 on a committed
// stream as on an uncommitted one, and never fails the breaker. Before the fix
// the committed branch wrote its failed-close comment but left the journal at
// the commit's 200.
func TestProxy_QueueCommentsCircuitOpenRecords503(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	for _, committed := range []bool{false, true} {
		name := "plain"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(5))
			j := journal.New(8, 1<<16)
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
				WithMetrics(metrics.NewCollector()),
				WithJournal(j),
				WithBreaker(b),
				WithMaxRetries(0),
				WithQueueComments(time.Hour),
				WithQueueTimeout(10*time.Second),
				WithTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, circuitbreaker.ErrCircuitOpen
				})),
			)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, committedOrPlainRequest(context.Background(), committed))

			if committed && !strings.Contains(rr.Body.String(), ": queue-wait failed: circuit open") {
				t.Fatalf("body = %q, want the circuit-open failed-close comment", rr.Body.String())
			}
			entries := j.Entries()
			if len(entries) != 1 {
				t.Fatalf("journal entries = %d, want 1", len(entries))
			}
			if entries[0].StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("journal status = %d, want 503", entries[0].StatusCode)
			}
			if stats := b.Stats(); stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want neither: the upstream was never reached", stats.TotalFailures, stats.TotalSuccesses)
			}
		})
	}
}

// TestStatusRecorder_ProxyTransportOrGeneratedError pins that any
// proxy-generated error status (500, 502, 503, 429) or unobserved status (0)
// is recognized by proxyTransportOrGeneratedError, and that a proxy-generated
// non-502 status is never counted as a breaker success during a HALF_OPEN probe.
func TestStatusRecorder_ProxyTransportOrGeneratedError(t *testing.T) {
	t.Run("predicate unit cases", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			rec      *statusRecorder
			wantBool bool
		}{
			{name: "nil recorder", rec: nil, wantBool: false},
			{name: "status 0", rec: &statusRecorder{status: 0}, wantBool: true},
			{name: "status 502 proxy-generated", rec: &statusRecorder{status: 502, proxyGeneratedError: true}, wantBool: true},
			{name: "status 500 proxy-generated", rec: &statusRecorder{status: 500, proxyGeneratedError: true}, wantBool: true},
			{name: "status 503 proxy-generated", rec: &statusRecorder{status: 503, proxyGeneratedError: true}, wantBool: true},
			{name: "status 429 proxy-generated", rec: &statusRecorder{status: 429, proxyGeneratedError: true}, wantBool: true},
			{name: "status 200 normal upstream", rec: &statusRecorder{status: 200}, wantBool: false},
			{name: "status 500 normal upstream", rec: &statusRecorder{status: 500}, wantBool: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.rec.proxyTransportOrGeneratedError(); got != tc.wantBool {
					t.Fatalf("proxyTransportOrGeneratedError() = %v, want %v", got, tc.wantBool)
				}
			})
		}
	})

	t.Run("isBreakerSuccessStatus rejects proxy-generated non-502 errors during probe", func(t *testing.T) {
		now := time.Now()
		epoch := uint64(42)
		for _, status := range []int{500, 503, 429} {
			rec := &statusRecorder{
				ResponseWriter:      httptest.NewRecorder(),
				status:              status,
				proxyGeneratedError: true,
			}
			if isBreakerSuccessStatus(rec, now, epoch, nil) {
				t.Fatalf("status %d proxy-generated error: isBreakerSuccessStatus() = true, want false", status)
			}
		}
	})
}

func TestProxy_QueueCommentsHalfOpenProbeSucceedsOnUpstreamClientError(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	waitForHalfOpenProbeWindow(t, b)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(b),
		WithMaxRetries(0),
		WithQueueComments(time.Hour),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found")),
				Request:    req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	p.ServeHTTP(httptest.NewRecorder(), committedOrPlainRequest(context.Background(), true))

	if state := b.State(); state != circuitbreaker.Closed {
		t.Fatalf("breaker state = %s, want CLOSED after definitive non-failure upstream response", state)
	}
	if stats := b.Stats(); stats.TotalSuccesses != 1 || stats.TotalFailures != 2 {
		t.Fatalf("breaker successes=%d failures=%d, want one probe success and no new failures",
			stats.TotalSuccesses, stats.TotalFailures)
	}
}

func TestStatusRecorderQueueCommentsTreatsAll2xxAsSuccessfulResponses(t *testing.T) {
	rec := &statusRecorder{
		ResponseWriter:    httptest.NewRecorder(),
		commentsCommitted: true,
		terminalWritten:   true,
	}
	rec.WriteHeader(http.StatusCreated)

	if rec.aborted {
		t.Fatal("2xx upstream response marked aborted")
	}
	if !rec.committedOKObserved {
		t.Fatal("2xx upstream response was not recorded")
	}
	if got := rec.observedStatus(); got != http.StatusCreated {
		t.Fatalf("observed status = %d, want %d", got, http.StatusCreated)
	}
}
