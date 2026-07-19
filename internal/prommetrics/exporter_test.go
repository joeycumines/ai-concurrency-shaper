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

package prommetrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// findFamily returns the metric family with the given name, or nil.
func findFamily(t *testing.T, families []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// gaugeValue returns the gauge value of the first (or only) metric in a family.
func gaugeValue(t *testing.T, f *dto.MetricFamily) float64 {
	t.Helper()
	if f == nil || len(f.GetMetric()) == 0 {
		t.Fatalf("family nil or empty")
	}
	return f.GetMetric()[0].GetGauge().GetValue()
}

// counterValue returns the counter value of the first (or only) metric in a family.
func counterValue(t *testing.T, f *dto.MetricFamily) float64 {
	t.Helper()
	if f == nil || len(f.GetMetric()) == 0 {
		t.Fatalf("family nil or empty")
	}
	return f.GetMetric()[0].GetCounter().GetValue()
}

// findLabeled finds the metric with matching label values in a family.
func findLabeled(t *testing.T, f *dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()
	if f == nil {
		return nil
	}
	for _, m := range f.GetMetric() {
		match := true
		for k, v := range want {
			got := ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == k {
					got = lp.GetValue()
					break
				}
			}
			if got != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	return nil
}

// TestExporter_BasicProxyMetrics drives a real metrics.Collector and asserts
// the proxy-level metrics appear with the expected values and labels.
func TestExporter_BasicProxyMetrics(t *testing.T) {
	met := metrics.NewCollector()
	mainLimiter := queue.NewLimiterWithCooldown(2, 0)
	exp := New(met, mainLimiter, nil, nil, nil, "test")

	// Drive the collector.
	met.IncActive()
	met.IncQueued()
	met.IncProxied()
	met.IncProxied()
	met.RecordStatus(200)                                          // bucket 2 (2xx)
	met.RecordStatus(503)                                          // bucket 5 (5xx)
	met.RecordStatus(600)                                          // bucket 0 (other — >=600 overflow)
	met.RecordStatus(0)                                            // dropped entirely, must NOT increment any bucket
	met.RecordAbortedRequest("POST", "/v1/messages", 502, 0, true) // aborted exchange

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if f := findFamily(t, families, "ai_concurrency_shaper_active_requests"); f == nil || gaugeValue(t, f) != 1 {
		t.Errorf("active_requests: want 1, got %v", f)
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_queued_requests"); f == nil || gaugeValue(t, f) != 1 {
		t.Errorf("queued_requests: want 1, got %v", f)
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_proxied_requests_total"); f == nil || counterValue(t, f) != 2 {
		t.Errorf("proxied_requests_total: want 2, got %v", f)
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_status_responses_total"); f == nil {
		t.Fatalf("status_responses_total family missing")
	} else {
		twoxx := findLabeled(t, f, map[string]string{"code": "2xx"})
		if twoxx == nil || twoxx.GetCounter().GetValue() != 1 {
			t.Errorf("status 2xx: want 1, got %v", twoxx)
		}
		fivexx := findLabeled(t, f, map[string]string{"code": "5xx"})
		if fivexx == nil || fivexx.GetCounter().GetValue() != 1 {
			t.Errorf("status 5xx: want 1, got %v", fivexx)
		}
		// >=600 overflow lands in the "other" bucket.
		other := findLabeled(t, f, map[string]string{"code": "other"})
		if other == nil || other.GetCounter().GetValue() != 1 {
			t.Errorf("status other: want 1 (from 600), got %v", other)
		}
		// code 0 is dropped by RecordStatus — must not increment any bucket.
		// Verify the 1xx/3xx/4xx buckets (untouched here) stay at 0.
		for _, code := range []string{"1xx", "3xx", "4xx"} {
			if m := findLabeled(t, f, map[string]string{"code": code}); m != nil && m.GetCounter().GetValue() != 0 {
				t.Errorf("status %s: want 0 (code 0 must not increment any bucket), got %v", code, m.GetCounter().GetValue())
			}
		}
	}
	// Aborted counter (B3): RecordAbortedRequest increments TotalAborted.
	if f := findFamily(t, families, "ai_concurrency_shaper_aborted_requests_total"); f == nil {
		t.Errorf("aborted_requests_total family missing")
	} else if counterValue(t, f) != 1 {
		t.Errorf("aborted_requests_total: want 1, got %v", counterValue(t, f))
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_build_info"); f == nil {
		t.Errorf("build_info missing")
	} else {
		bi := findLabeled(t, f, map[string]string{"version": "test"})
		if bi == nil || bi.GetGauge().GetValue() != 1 {
			t.Errorf("build_info{version=\"test\"} != 1, got %v", bi)
		}
	}
	// Default limiter series present with correct numeric values.
	// mainLimiter was created with limit=2, nothing acquired.
	if f := findFamily(t, families, "ai_concurrency_shaper_limiter_active"); f == nil {
		t.Fatalf("limiter_active missing")
	} else {
		def := findLabeled(t, f, map[string]string{"limiter": "default", "route": ""})
		if def == nil {
			t.Errorf("limiter_active{default,route=\"\"} missing")
		} else if def.GetGauge().GetValue() != 0 {
			t.Errorf("limiter_active{default}: want 0 (nothing acquired), got %v", def.GetGauge().GetValue())
		}
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_limiter_limit"); f == nil {
		t.Errorf("limiter_limit missing")
	} else {
		def := findLabeled(t, f, map[string]string{"limiter": "default", "route": ""})
		if def == nil {
			t.Errorf("limiter_limit{default} missing")
		} else if def.GetGauge().GetValue() != 2 {
			t.Errorf("limiter_limit{default}: want 2, got %v", def.GetGauge().GetValue())
		}
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_limiter_effective_limit"); f == nil {
		t.Errorf("limiter_effective_limit missing")
	} else {
		def := findLabeled(t, f, map[string]string{"limiter": "default", "route": ""})
		if def == nil {
			t.Errorf("limiter_effective_limit{default} missing")
		} else if def.GetGauge().GetValue() != 2 {
			t.Errorf("limiter_effective_limit{default}: want 2 (no withheld slots), got %v", def.GetGauge().GetValue())
		}
	}
	// Breaker metrics absent with nil breaker.
	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_state"); f != nil {
		t.Errorf("circuit_breaker_state present with nil breaker")
	}
}

// TestExporter_GlobalAndRouteLimiters verifies that the global and route
// limiter series are emitted when their limiters are non-nil.
func TestExporter_GlobalAndRouteLimiters(t *testing.T) {
	met := metrics.NewCollector()
	main := queue.NewLimiterWithCooldown(2, 0)
	global := queue.NewLimiterWithCooldown(10, 0)
	route := queue.NewLimiterWithCooldown(4, 0)
	routes := map[string]*queue.Limiter{
		"POST /v1/messages": route,
	}
	exp := New(met, main, global, routes, nil, "test")

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	f := findFamily(t, families, "ai_concurrency_shaper_limiter_active")
	if f == nil {
		t.Fatalf("limiter_active missing")
	}
	for _, want := range []map[string]string{
		{"limiter": "default", "route": ""},
		{"limiter": "global", "route": ""},
		{"limiter": "route", "route": "POST /v1/messages"},
	} {
		if m := findLabeled(t, f, want); m == nil {
			t.Errorf("missing limiter_active series for %v", want)
		}
	}
	// limiter_limit reflects configured capacity: main=2, global=10, route=4.
	if lf := findFamily(t, families, "ai_concurrency_shaper_limiter_limit"); lf == nil {
		t.Fatalf("limiter_limit missing")
	} else {
		for _, tc := range []struct {
			limiter string
			route   string
			want    float64
		}{
			{"default", "", 2},
			{"global", "", 10},
			{"route", "POST /v1/messages", 4},
		} {
			m := findLabeled(t, lf, map[string]string{"limiter": tc.limiter, "route": tc.route})
			if m == nil {
				t.Errorf("limiter_limit{%s,%s} missing", tc.limiter, tc.route)
			} else if m.GetGauge().GetValue() != tc.want {
				t.Errorf("limiter_limit{%s,%s}: want %v, got %v", tc.limiter, tc.route, tc.want, m.GetGauge().GetValue())
			}
		}
	}
	// Effective limit family present for all 3 series.
	if lf := findFamily(t, families, "ai_concurrency_shaper_limiter_effective_limit"); lf == nil {
		t.Fatalf("limiter_effective_limit missing")
	} else {
		for _, want := range []map[string]string{
			{"limiter": "default", "route": ""},
			{"limiter": "global", "route": ""},
			{"limiter": "route", "route": "POST /v1/messages"},
		} {
			if m := findLabeled(t, lf, want); m == nil {
				t.Errorf("missing effective_limit series for %v", want)
			}
		}
	}
}

// TestExporter_CircuitBreaker drives a real breaker through a failure and
// asserts the circuit_breaker_* metrics appear and reflect the recorded state.
func TestExporter_CircuitBreaker(t *testing.T) {
	met := metrics.NewCollector()
	main := queue.NewLimiterWithCooldown(2, 0)
	breaker, err := circuitbreaker.New()
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}
	exp := New(met, main, nil, nil, breaker, "test")

	// Drive a failure. epoch=0 disables stale-probe filtering (see
	// internal/circuitbreaker/circuitbreaker.go:355), so the failure is recorded.
	breaker.RecordFailure(503, 0, time.Now(), 0)

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_state"); f == nil {
		t.Fatalf("circuit_breaker_state missing")
	} else {
		// After one failure the breaker should remain CLOSED (state=0).
		if v := gaugeValue(t, f); v != 0 {
			t.Errorf("circuit_breaker_state: want 0 (CLOSED), got %v", v)
		}
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_total_failures_total"); f == nil {
		t.Fatalf("circuit_breaker_total_failures_total missing")
	} else {
		if v := counterValue(t, f); v != 1 {
			t.Errorf("total_failures_total: want 1, got %v", v)
		}
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_consecutive_failures"); f == nil {
		t.Fatalf("circuit_breaker_consecutive_failures missing")
	} else if v := gaugeValue(t, f); v != 1 {
		t.Errorf("consecutive_failures: want 1, got %v", v)
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_failures"); f == nil {
		t.Fatalf("circuit_breaker_failures missing")
	}
	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_total_successes_total"); f == nil {
		t.Fatalf("circuit_breaker_total_successes_total missing")
	}
}

// TestExporter_CircuitBreakerOpen trips the breaker into OPEN and asserts the
// state gauge reads 1 (OPEN) and next_retry_timestamp_seconds is a plausible
// future Unix time. This covers the non-trivial NextRetry emission branch
// (zero when not OPEN, Unix timestamp when OPEN) and the state-gauge mapping
// for the OPEN case — both untested by TestExporter_CircuitBreaker.
func TestExporter_CircuitBreakerOpen(t *testing.T) {
	met := metrics.NewCollector()
	main := queue.NewLimiterWithCooldown(2, 0)
	// Default failure threshold is 5; record 5 failures to trip OPEN.
	breaker, err := circuitbreaker.New()
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}
	exp := New(met, main, nil, nil, breaker, "test")

	now := time.Now()
	for range 5 {
		breaker.RecordFailure(503, 0, now, 0)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_state"); f == nil {
		t.Fatalf("circuit_breaker_state missing")
	} else if v := gaugeValue(t, f); v != 1 {
		t.Errorf("circuit_breaker_state: want 1 (OPEN), got %v", v)
	}

	// next_retry_timestamp_seconds must be a plausible future Unix time when OPEN.
	if f := findFamily(t, families, "ai_concurrency_shaper_circuit_breaker_next_retry_timestamp_seconds"); f == nil {
		t.Fatalf("circuit_breaker_next_retry_timestamp_seconds missing")
	} else {
		v := gaugeValue(t, f)
		if v <= float64(now.Unix()) {
			t.Errorf("next_retry_timestamp_seconds: want > now (%d), got %v", now.Unix(), v)
		}
	}
}

// TestExporter_DescribeStableAcrossNilBreaker verifies that Describe sends the
// breaker descriptors even when the breaker is nil — so the registry's metric
// set stays stable regardless of config.
func TestExporter_DescribeStableAcrossNilBreaker(t *testing.T) {
	met := metrics.NewCollector()
	main := queue.NewLimiterWithCooldown(2, 0)
	exp := New(met, main, nil, nil, nil, "test")

	descs := make(chan *prometheus.Desc, 64)
	exp.Describe(descs)
	close(descs)
	names := map[string]bool{}
	for d := range descs {
		names[d.String()] = true
	}
	for _, want := range []string{
		"ai_concurrency_shaper_circuit_breaker_state",
		"ai_concurrency_shaper_circuit_breaker_failures",
		"ai_concurrency_shaper_circuit_breaker_consecutive_failures",
		"ai_concurrency_shaper_circuit_breaker_total_failures_total",
		"ai_concurrency_shaper_circuit_breaker_total_successes_total",
		"ai_concurrency_shaper_circuit_breaker_current_penalty_seconds",
		"ai_concurrency_shaper_circuit_breaker_next_retry_timestamp_seconds",
	} {
		found := false
		for n := range names {
			if strings.Contains(n, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Describe did not send descriptor containing %q", want)
		}
	}
}

// TestExporter_HTTPHandler exercises the full /metrics HTTP path via
// httptest, verifying Prometheus text-format compliance.
func TestExporter_HTTPHandler(t *testing.T) {
	met := metrics.NewCollector()
	main := queue.NewLimiterWithCooldown(2, 0)
	exp := New(met, main, nil, nil, nil, "test")

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: want text/plain..., got %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bs := string(body)
	for _, want := range []string{
		"ai_concurrency_shaper_build_info",
		"ai_concurrency_shaper_active_requests",
		"# HELP", // HELP line present
		"# TYPE", // TYPE line present
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("body missing %q", want)
		}
	}
}
