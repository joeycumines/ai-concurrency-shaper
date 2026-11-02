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

// Breaker-resolution tests for the HALF_OPEN probe contract: every admitted
// probe is resolved or cancelled on every path, and only a definitive,
// un-aborted, attempted upstream outcome may close it as a success.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// halfOpenBreaker returns a breaker driven to HALF_OPEN, ready to issue a probe.
func halfOpenBreaker(t *testing.T) *circuitbreaker.Breaker {
	t.Helper()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	waitForHalfOpenProbeWindow(t, b)
	return b
}

// takeProbe admits one HALF_OPEN probe and returns its epoch.
func takeProbe(t *testing.T, b *circuitbreaker.Breaker) uint64 {
	t.Helper()
	epoch, err := b.Allow()
	if err != nil || epoch == 0 {
		t.Fatalf("Allow = (%d, %v), want a non-zero HALF_OPEN probe epoch", epoch, err)
	}
	return epoch
}

// newTestProxy builds a minimal Proxy whose breaker field is the given breaker.
func newTestProxy(b *circuitbreaker.Breaker) *Proxy {
	return &Proxy{breaker: b}
}

// TestResolveBreakerResultCancelledProbeIsNotSuccess pins that a native
// HALF_OPEN probe whose upstream answered 200 but whose request context was
// cancelled before classification (clientAborted=true, upstreamSuccess=false,
// suppressibleAbort=false for an observed 200) is cancelled, never recorded as
// a success. The classification is produced by classifyNativeExchange itself so
// the test covers the real input shape, then fed to both resolution paths.
func TestResolveBreakerResultCancelledProbeIsNotSuccess(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(cancelled)
	rec := &statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		status:         http.StatusOK,
	}
	cancel() // the client leaves after the upstream's 200, before classification

	result := classifyNativeExchange(rec, req, nil, true, false, 0, time.Now())
	if !result.clientAborted {
		t.Fatal("precondition: the cancelled request context marks the exchange client-aborted")
	}
	if result.upstreamSuccess {
		t.Fatal("precondition: a cancelled 2xx is not an upstream success")
	}
	if result.suppressibleAbort {
		t.Fatal("precondition: an observed 200 is not a suppressible abort")
	}

	for _, tc := range []struct {
		name    string
		resolve func(p *Proxy, result exchangeResult, epoch uint64)
	}{
		{
			name: "resolveBreakerResult",
			resolve: func(p *Proxy, result exchangeResult, epoch uint64) {
				p.resolveBreakerResult(result, nil, time.Now(), epoch)
			},
		},
		{
			name: "resolveAbortedExchange",
			resolve: func(p *Proxy, result exchangeResult, epoch uint64) {
				p.resolveAbortedExchange(result, nil, time.Now(), epoch, false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := halfOpenBreaker(t)
			epoch := takeProbe(t, b)
			p := newTestProxy(b)
			tc.resolve(p, result, epoch)

			stats := b.Stats()
			if stats.State == circuitbreaker.Closed {
				t.Fatal("the breaker closed on a client-aborted probe: RecordSuccess was recorded")
			}
			if stats.TotalSuccesses != 0 {
				t.Fatalf("TotalSuccesses = %d, want 0: the cancelled probe must not be credited", stats.TotalSuccesses)
			}
			// The cancelled probe left the live set, so a fresh probe is
			// admittable immediately and gets a new epoch.
			next, err := b.Allow()
			if err != nil || next == 0 || next == epoch {
				t.Fatalf("Allow after cancel = (%d, %v), want a fresh non-zero probe epoch", next, err)
			}
		})
	}
}

// TestResolveBreakerResultCircuitOpenIsNotProbeSuccess pins that a breaker
// rejection produced between retries (transportErr=ErrCircuitOpen without
// proxyGeneratedError, exactly what serveTranscodeHandler records for an
// outcome.CircuitRejected exchange) is a local failure: the probe is cancelled,
// never closed as success, on both resolution paths.
func TestResolveBreakerResultCircuitOpenIsNotProbeSuccess(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		status:         http.StatusBadGateway,
		transportErr:   circuitbreaker.ErrCircuitOpen,
	}
	result := classifyNativeExchange(rec, req, nil, true, false, 0, time.Now())
	if result.upstreamFailure {
		t.Fatal("precondition: a circuit-open rejection is not an upstream failure")
	}

	for _, tc := range []struct {
		name    string
		resolve func(p *Proxy, result exchangeResult, epoch uint64)
	}{
		{
			name: "resolveBreakerResult",
			resolve: func(p *Proxy, result exchangeResult, epoch uint64) {
				p.resolveBreakerResult(result, nil, time.Now(), epoch)
			},
		},
		{
			name: "resolveAbortedExchange",
			resolve: func(p *Proxy, result exchangeResult, epoch uint64) {
				p.resolveAbortedExchange(result, nil, time.Now(), epoch, false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := halfOpenBreaker(t)
			epoch := takeProbe(t, b)
			p := newTestProxy(b)
			tc.resolve(p, result, epoch)

			stats := b.Stats()
			if stats.State == circuitbreaker.Closed {
				t.Fatal("the breaker closed on a circuit-open rejection: RecordSuccess was recorded")
			}
			if stats.TotalSuccesses != 0 {
				t.Fatalf("TotalSuccesses = %d, want 0", stats.TotalSuccesses)
			}
			next, err := b.Allow()
			if err != nil || next == 0 || next == epoch {
				t.Fatalf("Allow after cancel = (%d, %v), want a fresh non-zero probe epoch", next, err)
			}
		})
	}
}
