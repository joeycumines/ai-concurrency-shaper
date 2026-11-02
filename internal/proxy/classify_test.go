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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// TestIsUpstreamFailureStatusWithoutRecorder pins that classification is total
// and conservative: a missing recorder carries no upstream observation, so it
// must neither panic nor report an upstream failure.
func TestIsUpstreamFailureStatusWithoutRecorder(t *testing.T) {
	if isUpstreamFailureStatus(nil, time.Now(), nil) {
		t.Fatal("a missing recorder must not classify as an upstream failure")
	}
}

// TestClassifyNativeExchangeTransportErrorIsNotLocalFailure pins that a 502 the
// proxy writes for a genuine upstream transport failure is an upstream failure
// only. Reporting it as a local failure too would misdescribe the exchange: the
// upstream was attempted and failed, rather than the proxy failing on its own.
func TestClassifyNativeExchangeTransportErrorIsNotLocalFailure(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &statusRecorder{
		ResponseWriter:      httptest.NewRecorder(),
		status:              http.StatusBadGateway,
		proxyGeneratedError: true,
		transportErr:        errors.New("dial tcp 127.0.0.1:1: connect: connection refused"),
		// The round trip started and then failed: this is the fact that makes
		// the error an upstream observation rather than a local defect.
		upstreamAttempted: true,
	}
	result := classifyNativeExchange(rec, req, nil, true, false, 0, time.Now())
	if !result.upstreamFailure {
		t.Fatal("a real upstream transport error is not classified as an upstream failure")
	}
	if result.localFailure {
		t.Fatal("a real upstream transport error is classified as a local failure")
	}
}

// TestClassifyNativeExchangeLocalUpgradeFailureIsLocal pins the converse: a 101
// upgrade the proxy could not hand off locally never reached the upstream's
// health, so it is a local failure, not an upstream one. It also pins the
// mutual exclusion with upstreamSuccess: a HALF_OPEN probe that ends in a local
// upgrade handoff failure with a non-zero status must not be reported as an
// upstream success, or a resolution path could close the breaker on it.
func TestClassifyNativeExchangeLocalUpgradeFailureIsLocal(t *testing.T) {
	for _, status := range []int{0, http.StatusSwitchingProtocols, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := &statusRecorder{
				ResponseWriter:      httptest.NewRecorder(),
				status:              status,
				localUpgradeFailure: true,
				aborted:             true,
			}
			result := classifyNativeExchange(rec, req, nil, true, false, 1, time.Now())
			if result.upstreamFailure {
				t.Fatal("a locally failed upgrade handoff is classified as an upstream failure")
			}
			if !result.localFailure {
				t.Fatal("a locally failed upgrade handoff is not classified as a local failure")
			}
			if result.upstreamSuccess {
				t.Fatal("a local failure is also reported as an upstream success")
			}
		})
	}
}

// TestClassifyNativeExchangeRetryCircuitOpenIsLocal pins the same exclusion for
// a breaker rejection produced between retries: the exchange never reached the
// upstream, so it is local and can never be an upstream success.
func TestClassifyNativeExchangeRetryCircuitOpenIsLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		status:         http.StatusBadGateway,
		transportErr:   circuitbreaker.ErrCircuitOpen,
	}
	result := classifyNativeExchange(rec, req, nil, true, false, 1, time.Now())
	if result.upstreamSuccess {
		t.Fatal("a breaker rejection is also reported as an upstream success")
	}
	if result.upstreamFailure {
		t.Fatal("a breaker rejection is classified as an upstream failure")
	}
	if !result.localFailure {
		t.Fatal("a breaker rejection is not classified as a local failure")
	}
}

// TestIsBreakerSuccessStatusRefusesLocalFailures pins the guard at the helper
// itself, so no future caller can turn a local failure into a breaker success.
func TestIsBreakerSuccessStatusRefusesLocalFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *statusRecorder
	}{
		{
			name: "local upgrade failure",
			rec:  &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusSwitchingProtocols, localUpgradeFailure: true},
		},
		{
			name: "breaker rejection",
			rec:  &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusServiceUnavailable, transportErr: circuitbreaker.ErrCircuitOpen},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if isBreakerSuccessStatus(tc.rec, time.Now(), 1, nil) {
				t.Fatal("a local failure was reported as a breaker success")
			}
		})
	}
}

// TestClassifyNativeExchangePreAttemptTransportErrorIsLocal pins that a
// transport error observed before the round trip started is a local defect:
// the proxy never reached the upstream, so reporting it as an upstream failure
// would let a local bug open the breaker.
func TestClassifyNativeExchangePreAttemptTransportErrorIsLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil)
	rec := &statusRecorder{
		ResponseWriter:      httptest.NewRecorder(),
		status:              http.StatusBadGateway,
		proxyGeneratedError: true,
		transportErr:        errors.New("client sent unsupported CONNECT request"),
	}
	result := classifyNativeExchange(rec, req, nil, false, false, 0, time.Now())
	if result.upstreamFailure {
		t.Fatal("a pre-attempt local rejection is classified as an upstream failure")
	}
	if !result.localFailure {
		t.Fatal("a pre-attempt local rejection is not classified as a local failure")
	}
}
