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
// health, so it is a local failure, not an upstream one.
func TestClassifyNativeExchangeLocalUpgradeFailureIsLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &statusRecorder{
		ResponseWriter:      httptest.NewRecorder(),
		localUpgradeFailure: true,
		aborted:             true,
	}
	result := classifyNativeExchange(rec, req, nil, true, false, 0, time.Now())
	if result.upstreamFailure {
		t.Fatal("a locally failed upgrade handoff is classified as an upstream failure")
	}
	if !result.localFailure {
		t.Fatal("a locally failed upgrade handoff is not classified as a local failure")
	}
}
