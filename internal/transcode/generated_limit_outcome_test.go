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

package transcode

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An oversized generated response is a bound on this proxy's own output. The
// recorded outcome must say so: it is a local conversion failure, not an
// upstream body failure that would count against the breaker.
func TestGeneratedResponseLimitRecordsLocalConversionOutcome(t *testing.T) {
	roundTrip := func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"` +
				strings.Repeat("x", 4096) + `"}}]}`)),
		}, nil
	}
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}
	outcomes := make(chan Outcome, 1)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
				// The minimum legal rendered-response bound; the content overflows it.
				GeneratedResponseBytes: MinGeneratedResponseBytes,
			},
		},
		roundTrip,
		func(o Outcome) { outcomes <- o },
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an oversized rendered response", rec.Code)
	}
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceLocalResponseConversionError {
		t.Fatalf("provenance = %v, want local response conversion error", outcome.Provenance)
	}
	if outcome.UpstreamFailure || !outcome.LocalFailure {
		t.Fatalf("UpstreamFailure=%v LocalFailure=%v, want a local failure that is not an upstream failure",
			outcome.UpstreamFailure, outcome.LocalFailure)
	}
}
