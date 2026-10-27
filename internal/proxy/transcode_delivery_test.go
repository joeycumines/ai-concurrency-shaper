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
	"net/http"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// deliveryOutcome is a transcoded upstream response whose delivery completed.
func deliveryOutcome(status int) *transcode.Outcome {
	return &transcode.Outcome{
		UpstreamAttempted:  true,
		UpstreamStatus:     transcode.Optional[int]{Value: status, Set: true},
		Provenance:         transcode.ProvenanceUpstreamHTTP,
		DownstreamComplete: true,
	}
}

// A 200 whose delivery the recorder saw fail is neither success nor failure.
func TestClassifyTranscodeExchangeWriteFailureIsNotSuccess(t *testing.T) {
	rec := &statusRecorder{transcodeOutcome: deliveryOutcome(http.StatusOK), writeFailed: true}
	if got := classifyTranscodeExchange(rec, true); got.upstreamSuccess {
		t.Fatal("upstreamSuccess = true for a 200 whose delivery the recorder saw fail")
	}
}

// Success means the client received the whole response.
func TestClassifyTranscodeExchangeIncompleteDeliveryIsNotSuccess(t *testing.T) {
	outcome := deliveryOutcome(http.StatusOK)
	outcome.DownstreamComplete = false
	rec := &statusRecorder{transcodeOutcome: outcome}
	if got := classifyTranscodeExchange(rec, true); got.upstreamSuccess {
		t.Fatal("upstreamSuccess = true for a 200 that was not completely delivered")
	}
}

// A complete 200 remains a success.
func TestClassifyTranscodeExchangeCompleteOKIsSuccess(t *testing.T) {
	rec := &statusRecorder{transcodeOutcome: deliveryOutcome(http.StatusOK)}
	if got := classifyTranscodeExchange(rec, true); !got.upstreamSuccess {
		t.Fatal("upstreamSuccess = false for a complete 200")
	}
}

// halfOpenProbe returns a breaker whose HALF_OPEN probe is in flight, with the
// probe's epoch.
func halfOpenProbe(t *testing.T) (*circuitbreaker.Breaker, uint64) {
	t.Helper()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	waitForHalfOpenProbeWindow(t, b)
	epoch, err := b.Allow()
	if err != nil || epoch == 0 {
		t.Fatalf("Allow = (%d, %v), want the probe", epoch, err)
	}
	return b, epoch
}

// An aborted 2xx is neither success nor failure: the probe is cancelled, so
// the breaker is not closed and no later request is refused by it.
func TestResolveAbortedExchangeAborted2xxCancelsProbe(t *testing.T) {
	b, epoch := halfOpenProbe(t)
	p := &Proxy{breaker: b}
	p.resolveAbortedExchange(exchangeResult{
		upstreamAttempted: true,
		upstreamStatus:    http.StatusOK,
		upstreamSuccess:   true,
	}, nil, time.Now(), epoch, false)
	if got := b.State(); got == circuitbreaker.Closed {
		t.Fatal("breaker closed on an aborted 2xx")
	}
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow after the aborted 2xx = %v, want the probe released", err)
	}
}

// A definitive non-failure, non-2xx status still resolves an aborted probe as
// a success: the upstream answered.
func TestResolveAbortedExchangeAborted404ClosesProbe(t *testing.T) {
	b, epoch := halfOpenProbe(t)
	p := &Proxy{breaker: b}
	p.resolveAbortedExchange(exchangeResult{
		upstreamAttempted: true,
		upstreamStatus:    http.StatusNotFound,
	}, nil, time.Now(), epoch, false)
	if got := b.State(); got != circuitbreaker.Closed {
		t.Fatalf("state after an aborted 404 = %s, want CLOSED", got)
	}
}
