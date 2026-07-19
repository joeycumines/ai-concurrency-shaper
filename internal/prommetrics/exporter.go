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

// Package prommetrics exposes the proxy's existing in-process collectors
// (metrics.Collector, queue.Limiter, circuitbreaker.Breaker) as a
// Prometheus metrics endpoint.
//
// The Exporter implements prometheus.Collector by reading point-in-time
// snapshots on each scrape. It holds no state of its own and performs no
// double bookkeeping — the existing atomic-counter collectors remain the
// single source of truth.
package prommetrics

import (
	"sort"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/prometheus/client_golang/prometheus"
)

// statusLabels maps metrics.Snapshot.StatusCounts index (bucket = code/100,
// clamped to [0, statusBuckets-1]) to its Prometheus label value. Index 0 is
// the catch-all "other" bucket for codes >=600 (code/100 >= statusBuckets).
// Note: code 0 is dropped entirely by metrics.RecordStatus and never reaches
// any bucket — it indicates the response was never written to.
var statusLabels = [6]string{"other", "1xx", "2xx", "3xx", "4xx", "5xx"}

// Exporter implements prometheus.Collector by reading point-in-time
// snapshots from the proxy's existing collectors. It holds no state of its
// own and performs no double bookkeeping.
type Exporter struct {
	met           *metrics.Collector
	mainLimiter   *queue.Limiter
	globalLimiter *queue.Limiter            // nil if -global-concurrency is 0
	routeLimiters map[string]*queue.Limiter // keyed by raw pattern or group; read-only after construction
	breaker       *circuitbreaker.Breaker   // nil if -circuit-breaker=false
	version       string

	// Proxy-level descriptors.
	descActiveRequests          *prometheus.Desc
	descQueuedRequests          *prometheus.Desc
	descProxiedRequestsTotal    *prometheus.Desc
	descPassthroughRequestsTotal *prometheus.Desc
	descQueueTimeoutsTotal      *prometheus.Desc
	descCancelledRequestsTotal  *prometheus.Desc
	descCircuitRejectedTotal    *prometheus.Desc
	descAbortedRequestsTotal    *prometheus.Desc
	descRetriesInFlight         *prometheus.Desc
	descThroughputRPS           *prometheus.Desc
	descStatusResponsesTotal    *prometheus.Desc
	descInFlightLimited          *prometheus.Desc
	descInFlightPassthrough     *prometheus.Desc
	descOldestQueuedAgeSeconds  *prometheus.Desc
	descBuildInfo                *prometheus.Desc

	// Limiter-level descriptors (labels: limiter, route).
	descLimiterActive          *prometheus.Desc
	descLimiterWaiters         *prometheus.Desc
	descLimiterAcquiredTotal   *prometheus.Desc
	descLimiterReleasedTotal   *prometheus.Desc
	descLimiterTimeoutsTotal   *prometheus.Desc
	descLimiterWithheld        *prometheus.Desc
	descLimiterLimit           *prometheus.Desc
	descLimiterEffectiveLimit  *prometheus.Desc

	// Circuit-breaker-level descriptors.
	descCBState                  *prometheus.Desc
	descCBFailures               *prometheus.Desc
	descCBConsecutiveFailures   *prometheus.Desc
	descCBTotalFailuresTotal    *prometheus.Desc
	descCBTotalSuccessesTotal   *prometheus.Desc
	descCBCurrentPenaltySeconds  *prometheus.Desc
	descCBNextRetryTimestamp     *prometheus.Desc
}

// New constructs an Exporter and pre-builds every *prometheus.Desc.
// routeLimiters may be nil or empty. globalLimiter and breaker may be nil.
func New(
	met *metrics.Collector,
	mainLimiter *queue.Limiter,
	globalLimiter *queue.Limiter,
	routeLimiters map[string]*queue.Limiter,
	breaker *circuitbreaker.Breaker,
	version string,
) *Exporter {
	e := &Exporter{
		met:           met,
		mainLimiter:   mainLimiter,
		globalLimiter: globalLimiter,
		routeLimiters: routeLimiters,
		breaker:       breaker,
		version:       version,
	}

	// Proxy-level.
	e.descActiveRequests = prometheus.NewDesc(
		"ai_concurrency_shaper_active_requests",
		"Number of requests currently being proxied upstream.",
		nil, nil,
	)
	e.descQueuedRequests = prometheus.NewDesc(
		"ai_concurrency_shaper_queued_requests",
		"Number of requests currently waiting in the queue for a concurrency slot.",
		nil, nil,
	)
	e.descProxiedRequestsTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_proxied_requests_total",
		"Total number of requests proxied to upstream (clean completions).",
		nil, nil,
	)
	e.descPassthroughRequestsTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_passthrough_requests_total",
		"Total number of requests passed through (not limited).",
		nil, nil,
	)
	e.descQueueTimeoutsTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_queue_timeouts_total",
		"Total number of requests that timed out waiting in the queue.",
		nil, nil,
	)
	e.descCancelledRequestsTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_cancelled_requests_total",
		"Total number of requests cancelled while in the queue.",
		nil, nil,
	)
	e.descCircuitRejectedTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_rejected_requests_total",
		"Total number of requests rejected immediately because the circuit breaker was open.",
		nil, nil,
	)
	e.descAbortedRequestsTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_aborted_requests_total",
		"Total number of proxied exchanges that aborted before clean completion (e.g. broken 101 upgrades, mid-stream client disconnects, panic-recovered 502s).",
		nil, nil,
	)
	e.descRetriesInFlight = prometheus.NewDesc(
		"ai_concurrency_shaper_retries_in_flight",
		"Number of retries currently in flight.",
		nil, nil,
	)
	e.descThroughputRPS = prometheus.NewDesc(
		"ai_concurrency_shaper_throughput_rps",
		"Recent request throughput in requests per second (rolling window).",
		nil, nil,
	)
	e.descStatusResponsesTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_status_responses_total",
		"Total responses proxied, bucketed by HTTP status class.",
		[]string{"code"}, nil,
	)
	e.descInFlightLimited = prometheus.NewDesc(
		"ai_concurrency_shaper_in_flight_limited",
		"In-flight requests that are subject to concurrency limiting.",
		nil, nil,
	)
	e.descInFlightPassthrough = prometheus.NewDesc(
		"ai_concurrency_shaper_in_flight_passthrough",
		"In-flight requests that bypass concurrency limiting (passthrough).",
		nil, nil,
	)
	e.descOldestQueuedAgeSeconds = prometheus.NewDesc(
		"ai_concurrency_shaper_oldest_queued_age_seconds",
		"Age in seconds of the oldest request currently queued.",
		nil, nil,
	)
	e.descBuildInfo = prometheus.NewDesc(
		"ai_concurrency_shaper_build_info",
		"Build information.",
		[]string{"version"}, nil,
	)

	// Limiter-level.
	limiterLabels := []string{"limiter", "route"}
	e.descLimiterActive = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_active",
		"Number of currently-acquired concurrency slots.",
		limiterLabels, nil,
	)
	e.descLimiterWaiters = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_waiters",
		"Number of requests waiting for a concurrency slot.",
		limiterLabels, nil,
	)
	e.descLimiterAcquiredTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_acquired_total",
		"Total concurrency-slot acquisitions since startup.",
		limiterLabels, nil,
	)
	e.descLimiterReleasedTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_released_total",
		"Total concurrency-slot releases since startup.",
		limiterLabels, nil,
	)
	e.descLimiterTimeoutsTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_timeouts_total",
		"Total requests that timed out waiting for a concurrency slot from this limiter.",
		limiterLabels, nil,
	)
	e.descLimiterWithheld = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_withheld",
		"Number of slots currently withheld from circulation by adaptive headroom.",
		limiterLabels, nil,
	)
	e.descLimiterLimit = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_limit",
		"Configured concurrency limit (channel capacity).",
		limiterLabels, nil,
	)
	e.descLimiterEffectiveLimit = prometheus.NewDesc(
		"ai_concurrency_shaper_limiter_effective_limit",
		"Effective concurrency limit (configured minus withheld slots).",
		limiterLabels, nil,
	)

	// Circuit-breaker-level.
	// state gauge: 0=CLOSED, 1=OPEN, 2=HALF_OPEN (raw int(circuitbreaker.State)).
	// Grafana value mappings translate 0/1/2 to colors; no string label is added
	// because a numeric gauge is the Prometheus-idiomatic way to expose a
	// tri-state breaker.
	e.descCBState = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_state",
		"Circuit breaker state (0=CLOSED, 1=OPEN, 2=HALF_OPEN).",
		nil, nil,
	)
	e.descCBFailures = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_failures",
		"Number of recent failures currently recorded in the breaker's failure window.",
		nil, nil,
	)
	e.descCBConsecutiveFailures = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_consecutive_failures",
		"Current consecutive-failure count.",
		nil, nil,
	)
	e.descCBTotalFailuresTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_total_failures_total",
		"Total failures recorded since startup.",
		nil, nil,
	)
	e.descCBTotalSuccessesTotal = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_total_successes_total",
		"Total successes recorded since startup.",
		nil, nil,
	)
	e.descCBCurrentPenaltySeconds = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_current_penalty_seconds",
		"Current phantom-slot hold penalty in seconds.",
		nil, nil,
	)
	e.descCBNextRetryTimestamp = prometheus.NewDesc(
		"ai_concurrency_shaper_circuit_breaker_next_retry_timestamp_seconds",
		"Unix timestamp of the next HALF_OPEN probe (0 when breaker is not OPEN).",
		nil, nil,
	)

	return e
}

// Describe implements prometheus.Collector. Sends one *Desc per metric.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.descActiveRequests
	ch <- e.descQueuedRequests
	ch <- e.descProxiedRequestsTotal
	ch <- e.descPassthroughRequestsTotal
	ch <- e.descQueueTimeoutsTotal
	ch <- e.descCancelledRequestsTotal
	ch <- e.descCircuitRejectedTotal
	ch <- e.descAbortedRequestsTotal
	ch <- e.descRetriesInFlight
	ch <- e.descThroughputRPS
	ch <- e.descStatusResponsesTotal
	ch <- e.descInFlightLimited
	ch <- e.descInFlightPassthrough
	ch <- e.descOldestQueuedAgeSeconds
	ch <- e.descBuildInfo

	ch <- e.descLimiterActive
	ch <- e.descLimiterWaiters
	ch <- e.descLimiterAcquiredTotal
	ch <- e.descLimiterReleasedTotal
	ch <- e.descLimiterTimeoutsTotal
	ch <- e.descLimiterWithheld
	ch <- e.descLimiterLimit
	ch <- e.descLimiterEffectiveLimit

	// Breaker descriptors are always sent through Describe even when the
	// breaker is nil — this keeps the registry's metric set stable across
	// config changes (Prometheus warns on descriptors that appear/disappear).
	ch <- e.descCBState
	ch <- e.descCBFailures
	ch <- e.descCBConsecutiveFailures
	ch <- e.descCBTotalFailuresTotal
	ch <- e.descCBTotalSuccessesTotal
	ch <- e.descCBCurrentPenaltySeconds
	ch <- e.descCBNextRetryTimestamp
}

// Collect implements prometheus.Collector. Calls met.Snapshot() once for a
// consistent proxy-level view, then each limiter's Stats() and (if non-nil)
// breaker.Stats(); emits one const metric per value. Must not hold any lock
// across the channel send.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	// Nil-guard required deps. met and mainLimiter are documented as required
	// but a defensive nil-check prevents a panic inside the registry's collect
	// goroutine (surfaced as a 500 to the scraper) if a caller mis-wires.
	if e.met == nil {
		return
	}
	s := e.met.Snapshot()

	ch <- prometheus.MustNewConstMetric(e.descActiveRequests, prometheus.GaugeValue, float64(s.Active))
	ch <- prometheus.MustNewConstMetric(e.descQueuedRequests, prometheus.GaugeValue, float64(s.Queued))
	ch <- prometheus.MustNewConstMetric(e.descProxiedRequestsTotal, prometheus.CounterValue, float64(s.TotalProxied))
	ch <- prometheus.MustNewConstMetric(e.descPassthroughRequestsTotal, prometheus.CounterValue, float64(s.TotalPassThrough))
	ch <- prometheus.MustNewConstMetric(e.descQueueTimeoutsTotal, prometheus.CounterValue, float64(s.TotalTimeout))
	ch <- prometheus.MustNewConstMetric(e.descCancelledRequestsTotal, prometheus.CounterValue, float64(s.TotalCancelled))
	ch <- prometheus.MustNewConstMetric(e.descCircuitRejectedTotal, prometheus.CounterValue, float64(s.TotalCircuitRejected))
	ch <- prometheus.MustNewConstMetric(e.descAbortedRequestsTotal, prometheus.CounterValue, float64(s.TotalAborted))
	ch <- prometheus.MustNewConstMetric(e.descRetriesInFlight, prometheus.GaugeValue, float64(s.RetriesInFlight))
	ch <- prometheus.MustNewConstMetric(e.descThroughputRPS, prometheus.GaugeValue, s.Throughput)
	for i, v := range s.StatusCounts {
		ch <- prometheus.MustNewConstMetric(e.descStatusResponsesTotal, prometheus.CounterValue, float64(v), statusLabels[i])
	}
	ch <- prometheus.MustNewConstMetric(e.descInFlightLimited, prometheus.GaugeValue, float64(s.InFlightLimited))
	ch <- prometheus.MustNewConstMetric(e.descInFlightPassthrough, prometheus.GaugeValue, float64(s.InFlightPassthrough))
	ch <- prometheus.MustNewConstMetric(e.descOldestQueuedAgeSeconds, prometheus.GaugeValue, s.OldestQueuedAge.Seconds())
	ch <- prometheus.MustNewConstMetric(e.descBuildInfo, prometheus.GaugeValue, 1, e.version)

	// Limiters: default (main) if present, then global if present, then routes in stable order.
	if e.mainLimiter != nil {
		e.collectLimiter(ch, "default", "", e.mainLimiter)
	}
	if e.globalLimiter != nil {
		e.collectLimiter(ch, "global", "", e.globalLimiter)
	}
	if len(e.routeLimiters) > 0 {
		routes := make([]string, 0, len(e.routeLimiters))
		for r := range e.routeLimiters {
			routes = append(routes, r)
		}
		sort.Strings(routes)
		for _, r := range routes {
			e.collectLimiter(ch, "route", r, e.routeLimiters[r])
		}
	}

	// Circuit breaker: emit only when present.
	if e.breaker != nil {
		bs := e.breaker.Stats()
		ch <- prometheus.MustNewConstMetric(e.descCBState, prometheus.GaugeValue, float64(bs.State))
		ch <- prometheus.MustNewConstMetric(e.descCBFailures, prometheus.GaugeValue, float64(bs.Failures))
		ch <- prometheus.MustNewConstMetric(e.descCBConsecutiveFailures, prometheus.GaugeValue, float64(bs.ConsecutiveFailures))
		ch <- prometheus.MustNewConstMetric(e.descCBTotalFailuresTotal, prometheus.CounterValue, float64(bs.TotalFailures))
		ch <- prometheus.MustNewConstMetric(e.descCBTotalSuccessesTotal, prometheus.CounterValue, float64(bs.TotalSuccesses))
		ch <- prometheus.MustNewConstMetric(e.descCBCurrentPenaltySeconds, prometheus.GaugeValue, bs.CurrentPenalty.Seconds())
		nextRetry := float64(0)
		if !bs.NextRetry.IsZero() {
			nextRetry = float64(bs.NextRetry.Unix())
		}
		ch <- prometheus.MustNewConstMetric(e.descCBNextRetryTimestamp, prometheus.GaugeValue, nextRetry)
	}
}

// collectLimiter emits the eight limiter_* series for a single limiter.
func (e *Exporter) collectLimiter(ch chan<- prometheus.Metric, kind, route string, l *queue.Limiter) {
	stats := l.Stats()
	ch <- prometheus.MustNewConstMetric(e.descLimiterActive, prometheus.GaugeValue, float64(stats.Active), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterWaiters, prometheus.GaugeValue, float64(stats.Waiters), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterAcquiredTotal, prometheus.CounterValue, float64(stats.TotalAcq), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterReleasedTotal, prometheus.CounterValue, float64(stats.TotalRel), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterTimeoutsTotal, prometheus.CounterValue, float64(stats.TotalTimeout), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterWithheld, prometheus.GaugeValue, float64(stats.Withheld), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterLimit, prometheus.GaugeValue, float64(l.Limit()), kind, route)
	ch <- prometheus.MustNewConstMetric(e.descLimiterEffectiveLimit, prometheus.GaugeValue, float64(l.EffectiveLimit()), kind, route)
}
