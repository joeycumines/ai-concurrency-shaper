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
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

func responseHeaders(rec *statusRecorder) http.Header {
	if rec.entry != nil && rec.entry.ResponseHeaders != nil {
		return rec.entry.ResponseHeaders
	}
	return rec.ResponseWriter.Header()
}

// parseRetryAfterFromRecorder extracts the remaining Retry-After duration
// from the statusRecorder's captured response headers. Returns 0 if absent or
// invalid. Supports both delay-seconds (integer) and HTTP-date formats per
// RFC 9110 §10.2.3. receivedAt is the header receipt time captured in rec.responseAt;
// evaluatedAt is the time of evaluation, passed in by the caller together with
// classification so both decisions share one anchor.
func parseRetryAfterFromRecorder(rec *statusRecorder, now time.Time) time.Duration {
	return circuitbreaker.ParseRetryAfter(responseHeaders(rec), rec.responseAt, now)
}

// RoundTrip implements http.RoundTripper, delegating to the retry-aware
// transport set up during construction.
func (p *Proxy) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := p.transport.RoundTrip(r)
	if resp != nil {
		resp.Request = r
	}
	if resp != nil && resp.Body != nil {
		if state := copyErrorStateFromContext(r.Context()); state != nil {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				// Upgraded protocol streams are bidirectional connection traffic, not
				// an HTTP response body copied by ReverseProxy.copyResponse. Leave the
				// body untouched so optional interfaces such as CloseWrite survive. The
				// proxy marks failures while writing/flushing the 101 handshake as
				// aborted, but post-handshake upgraded-stream copy errors belong to the
				// upgraded protocol lifetime and are outside HTTP response-body abort
				// accounting.
			} else if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
				if cw, ok := resp.Body.(closeWriter); ok {
					resp.Body = &trackingReadWriteCloseWriter{ReadWriteCloser: rwc, closeWriter: cw, state: state}
				} else {
					resp.Body = &trackingReadWriteCloser{ReadWriteCloser: rwc, state: state}
				}
			} else {
				// httputil.ReverseProxy's response copy path uses Read directly, so
				// tracking Read is sufficient for ordinary responses. Do not add a
				// generic WriterTo passthrough here: WriterTo reports a single error
				// for both source reads and downstream writes, which would destroy the
				// provenance this wrapper exists to preserve.
				resp.Body = &trackingReadCloser{ReadCloser: resp.Body, state: state}
			}
		}
	}
	return resp, err
}
