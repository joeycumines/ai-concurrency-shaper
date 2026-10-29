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
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// TestProxy_SupersederQueueTimeoutLeavesSupersededProbeStanding pins the
// admission contract end to end. A HALF_OPEN probe is in flight but has
// outlived the open timeout, so a later request supersedes it and then times
// out waiting for a slot the probe holds. That request cancels only its own
// probe, so the superseded probe's success still closes the breaker. Each
// subtest makes a different wait the one that times out.
func TestProxy_SupersederQueueTimeoutLeavesSupersededProbeStanding(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	limitedPat, _ := route.Parse("POST /v1/messages")

	newBreaker := func(t *testing.T) *circuitbreaker.Breaker {
		t.Helper()
		return mustBreaker(t,
			circuitbreaker.WithFailureThreshold(2),
			circuitbreaker.WithWindow(10*time.Second),
			circuitbreaker.WithOpenTimeout(20*time.Millisecond),
			circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
		)
	}
	// inFlightProbe admits the probe that stands for an upstream request still
	// running, then lets it outlive the open timeout.
	inFlightProbe := func(t *testing.T, b *circuitbreaker.Breaker) uint64 {
		t.Helper()
		waitForHalfOpenProbeWindow(t, b)
		epoch, err := b.Allow()
		if err != nil || epoch == 0 {
			t.Fatalf("Allow = (%d, %v), want the in-flight probe", epoch, err)
		}
		time.Sleep(40 * time.Millisecond)
		return epoch
	}
	serveSuperseder := func(t *testing.T, p *Proxy) {
		t.Helper()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, committedOrPlainRequest(context.Background(), false))
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("superseding request status = %d, want 504 queue timeout", rec.Code)
		}
	}

	t.Run("route slot", func(t *testing.T) {
		b := newBreaker(t)
		older := inFlightProbe(t, b)
		lim := queue.NewLimiterWithCooldown(1, 0)
		defer fillQueue(t, lim)()
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher([]route.Pattern{limitedPat})),
			WithLimiter(lim),
			WithMetrics(metrics.NewCollector()),
			WithBreaker(b),
			WithQueueTimeout(20*time.Millisecond),
		)
		if err != nil {
			t.Fatal(err)
		}
		serveSuperseder(t, p)
		b.RecordSuccess(time.Now(), older)
		if got := b.State(); got != circuitbreaker.Closed {
			t.Fatalf("breaker state = %s, want CLOSED from the superseded probe's success", got)
		}
	})

	t.Run("global slot", func(t *testing.T) {
		b := newBreaker(t)
		older := inFlightProbe(t, b)
		global := queue.NewLimiterWithCooldown(1, 0)
		defer fillQueue(t, global)()
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher([]route.Pattern{limitedPat})),
			WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
			WithGlobalLimiter(global),
			WithMetrics(metrics.NewCollector()),
			WithBreaker(b),
			WithQueueTimeout(20*time.Millisecond),
		)
		if err != nil {
			t.Fatal(err)
		}
		serveSuperseder(t, p)
		b.RecordSuccess(time.Now(), older)
		if got := b.State(); got != circuitbreaker.Closed {
			t.Fatalf("breaker state = %s, want CLOSED from the superseded probe's success", got)
		}
	})

	t.Run("passthrough global slot", func(t *testing.T) {
		b := newBreaker(t)
		older := inFlightProbe(t, b)
		global := queue.NewLimiterWithCooldown(1, 0)
		defer fillQueue(t, global)()
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher(nil)),
			WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
			WithGlobalLimiter(global),
			WithMetrics(metrics.NewCollector()),
			WithBreaker(b),
			WithQueueTimeout(20*time.Millisecond),
		)
		if err != nil {
			t.Fatal(err)
		}
		serveSuperseder(t, p)
		b.RecordSuccess(time.Now(), older)
		if got := b.State(); got != circuitbreaker.Closed {
			t.Fatalf("breaker state = %s, want CLOSED from the superseded probe's success", got)
		}
	})
}
