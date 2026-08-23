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
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// captureSlogDefault swaps slog's default logger for a TextHandler writing into
// the returned buffer, restoring the previous default on cleanup. The package's
// tests never call t.Parallel, so the process-global swap is safe here.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// newPanickingProxy builds a minimal proxy whose upstream transport panics on
// every RoundTrip, exercising the inner panic-recovery paths of
// servePassthrough and serveLimited without touching the network.
func newPanickingProxy(t *testing.T, limitRoute string, panicValue func(attempt int) any) *Proxy {
	t.Helper()
	upstreamURL, err := url.Parse("http://upstream.test")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	pat, err := route.Parse(limitRoute)
	if err != nil {
		t.Fatalf("route.Parse(%q): %v", limitRoute, err)
	}
	attempt := 0
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempt++
			panic(panicValue(attempt))
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p
}

// TestProxy_PanicLogsAreStructured pins review-13 issue 2: the panic-recovery
// paths must emit structured key-value log records — a stable message naming
// the recovery site plus the recovered value as a separate attribute — instead
// of baking the dynamic value into the message via fmt.Sprintf. Structured
// fields are what keep the TUI's toast actionability keyed on level=ERROR and
// its dedup keys stable per site while remaining distinct per panic value.
func TestProxy_PanicLogsAreStructured(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		wantMsg string
	}{
		{"passthrough recovery", http.MethodGet, "/health", "proxy panic in servePassthrough"},
		{"limited recovery", http.MethodPost, "/v1/messages", "proxy panic in serveLimited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureSlogDefault(t)
			p := newPanickingProxy(t, "POST /v1/messages", func(int) any { return "boom from transport" })

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d (recovered local panic yields 502)", rec.Code, http.StatusBadGateway)
			}
			out := buf.String()
			if out == "" {
				t.Fatal("no slog output captured for recovered panic")
			}
			if !strings.Contains(out, "level=ERROR") {
				t.Errorf("panic record missing level=ERROR: %q", out)
			}
			if !strings.Contains(out, `msg="`+tt.wantMsg+`"`) {
				t.Errorf("panic record msg = %q, want stable message token %q", out, tt.wantMsg)
			}
			if !strings.Contains(out, `panic="boom from transport"`) {
				t.Errorf("recovered value must ride a structured panic attribute: %q", out)
			}
			// The old fmt.Sprintf form baked the value into the message:
			// msg="... : boom from transport". Guard against regression.
			if strings.Contains(out, tt.wantMsg+": boom from transport") {
				t.Errorf("panic value baked into the static msg token: %q", out)
			}
		})
	}
}

// TestProxy_PanicMsgStableAcrossValues pins the dedup contract that motivated
// the structured form: for repeated panics at one recovery site the message
// token stays constant while the panic attribute varies, so downstream
// consumers (the TUI dedup key is msg + trailing attributes) see identical
// incidents collapse only when their values match.
func TestProxy_PanicMsgStableAcrossValues(t *testing.T) {
	buf := captureSlogDefault(t)
	values := []any{"first failure", "second failure"}
	p := newPanickingProxy(t, "POST /v1/messages", func(attempt int) any { return values[attempt-1] })

	for i := range values {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want %d", i+1, rec.Code, http.StatusBadGateway)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	errorLines := 0
	for _, line := range lines {
		if !strings.Contains(line, "level=ERROR") {
			continue
		}
		errorLines++
		if !strings.Contains(line, `msg="proxy panic in serveLimited"`) {
			t.Errorf("panic record msg drifted across values: %q", line)
		}
		switch {
		case strings.Contains(line, `panic="first failure"`):
		case strings.Contains(line, `panic="second failure"`):
		default:
			t.Errorf("panic record missing distinct panic attribute: %q", line)
		}
	}
	if errorLines != len(values) {
		t.Fatalf("captured %d ERROR records, want %d; raw output:\n%s", errorLines, len(values), buf.String())
	}
}
