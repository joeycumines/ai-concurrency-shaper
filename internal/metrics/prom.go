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

package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// statusBucketNames names the six StatusCounts buckets. Index 0 has no HTTP
// meaning: RecordStatus ignores code 0, and out-of-range codes (<100 or >=600)
// are clamped into bucket 0 by the collector. Bucket 0 is therefore skipped on
// export rather than exported under a made-up name.
var statusBucketNames = [statusBuckets]string{
	"0xx",
	"1xx",
	"2xx",
	"3xx",
	"4xx",
	"5xx",
}

// escapeLabelValue escapes a string for use inside a Prometheus text-format
// label value: backslashes first, then quotes, then newlines.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// validLabelValues reports whether every label value is valid UTF-8. The
// Prometheus text parser rejects the ENTIRE document on any label value that
// is not valid UTF-8, and a client-supplied request path is arbitrary bytes,
// so a per-route series carrying one is skipped entirely (fail-closed) rather
// than sanitized: strings.ToValidUTF8 is not injective and can collide two
// distinct routes into duplicate series.
func validLabelValues(values ...string) bool {
	for _, v := range values {
		if !utf8.ValidString(v) {
			return false
		}
	}
	return true
}

// ProviderSnapshot pairs one provider's display name with its collected
// snapshot for fleet-wide exposition. An empty Name exports under the
// provider label "default".
type ProviderSnapshot struct {
	Name     string
	Snapshot Snapshot
}

// breakerSeriesValue maps a circuit-breaker state name onto its numeric gauge
// value: closed=0, half_open=1, open=2 (anything else is closed).
func breakerSeriesValue(state string) int64 {
	switch state {
	case "HALF_OPEN":
		return 1
	case "OPEN":
		return 2
	default:
		return 0
	}
}

// WritePrometheusFleet writes every provider's snapshot as Prometheus
// text-format exposition lines, every series carrying a provider label so one
// /metrics endpoint can fan out over the whole fleet.
//
// Series are GROUPED BY METRIC NAME: each family forms exactly one contiguous
// block across all providers that carry it. The text format requires all
// lines for a metric to be provided as a single group, so interleaving
// families per provider would violate the contract as soon as any HELP/TYPE
// metadata line is added - even though samples-only output happens to parse
// on tolerant implementations today. When snap.CircuitBreaker is nil (breaker
// disabled or not merged) the breaker series is omitted for that provider
// rather than guessed. Lines are stable and sorted by construction; no
// timestamps are emitted (scrape time is scrape time).
func WritePrometheusFleet(w io.Writer, providers []ProviderSnapshot) error {
	labels := make([]string, len(providers))
	for i, p := range providers {
		name := p.Name
		if name == "" {
			name = "default"
		}
		labels[i] = `provider="` + escapeLabelValue(name) + `"`
	}

	// group renders one series body per provider that carries it, in input
	// order, producing a contiguous block for its family.
	group := func(render func(i int) (body string, ok bool)) []string {
		var out []string
		for i := range providers {
			if body, ok := render(i); ok {
				out = append(out, body)
			}
		}
		return out
	}
	writeGroup := func(lines []string) error {
		for _, line := range lines {
			if _, err := io.WriteString(w, line); err != nil {
				return err
			}
		}
		return nil
	}
	gauge := func(name string, pick func(*Snapshot) int64) []string {
		return group(func(i int) (string, bool) {
			return fmt.Sprintf("%s{%s} %d\n", name, labels[i], pick(&providers[i].Snapshot)), true
		})
	}
	gaugeFloat := func(name string, pick func(*Snapshot) float64) []string {
		return group(func(i int) (string, bool) {
			return fmt.Sprintf("%s{%s} %.3f\n", name, labels[i], pick(&providers[i].Snapshot)), true
		})
	}

	families := [][]string{
		gauge("shaper_active", func(s *Snapshot) int64 { return s.Active }),
		gauge("shaper_queued", func(s *Snapshot) int64 { return s.Queued }),
		gaugeFloat("shaper_oldest_queued_seconds", func(s *Snapshot) float64 { return s.OldestQueuedAge.Seconds() }),
		gauge("shaper_retries_in_flight", func(s *Snapshot) int64 { return s.RetriesInFlight }),
		gauge("shaper_clean_proxied_total", func(s *Snapshot) int64 { return s.TotalProxied }),
		gauge("shaper_clean_passthrough_total", func(s *Snapshot) int64 { return s.TotalPassThrough }),
		gauge("shaper_aborted_total", func(s *Snapshot) int64 { return s.TotalAborted }),
		gauge("shaper_circuit_rejected_total", func(s *Snapshot) int64 { return s.TotalCircuitRejected }),
		gauge("shaper_queue_rejected_total", func(s *Snapshot) int64 { return s.TotalQueueRejected }),
	}
	for bucket := 1; bucket < int(statusBuckets); bucket++ {
		b := bucket
		families = append(families, group(func(i int) (string, bool) {
			return fmt.Sprintf("shaper_requests_total{%s,status=%q} %d\n",
				labels[i], statusBucketNames[b], providers[i].Snapshot.StatusCounts[b]), true
		}))
	}
	// Breaker state exports as ONE stable series per provider with the state
	// encoded in the value alone (closed=0, half_open=1, open=2): a mutating
	// state label would churn time series on every transition while
	// redundantly re-encoding what the value already says.
	families = append(families, group(func(i int) (string, bool) {
		cb := providers[i].Snapshot.CircuitBreaker
		if cb == nil {
			return "", false
		}
		return fmt.Sprintf("shaper_breaker_state{%s} %d\n", labels[i], breakerSeriesValue(cb.State)), true
	}))

	// Per-route queue observability: when providers have requests
	// waiting in their per-route queues, export the per-route queued counts
	// and oldest queued age with method and path labels.
	// Grouped by metric name across all providers.
	var routeQueuedLines []string
	for i, p := range providers {
		snap := &p.Snapshot
		if len(snap.QueuedByRoute) == 0 {
			continue
		}
		routes := make([]string, 0, len(snap.QueuedByRoute))
		for r := range snap.QueuedByRoute {
			routes = append(routes, r)
		}
		sort.Strings(routes)
		for _, r := range routes {
			count := snap.QueuedByRoute[r]
			if count <= 0 {
				continue
			}
			method, path, ok := strings.Cut(r, " ")
			if !ok {
				method = ""
				path = r
			}
			if !validLabelValues(labels[i], method, path) {
				continue
			}
			routeQueuedLines = append(routeQueuedLines, fmt.Sprintf(
				"shaper_route_queued{%s,method=\"%s\",path=\"%s\"} %d\n",
				labels[i], escapeLabelValue(method), escapeLabelValue(path), count,
			))
		}
	}
	if len(routeQueuedLines) > 0 {
		families = append(families, routeQueuedLines)
	}

	var routeAgeLines []string
	for i, p := range providers {
		snap := &p.Snapshot
		if len(snap.OldestQueuedAgeByRoute) == 0 {
			continue
		}
		routes := make([]string, 0, len(snap.OldestQueuedAgeByRoute))
		for r := range snap.OldestQueuedAgeByRoute {
			routes = append(routes, r)
		}
		sort.Strings(routes)
		for _, r := range routes {
			age := snap.OldestQueuedAgeByRoute[r]
			method, path, ok := strings.Cut(r, " ")
			if !ok {
				method = ""
				path = r
			}
			if !validLabelValues(labels[i], method, path) {
				continue
			}
			routeAgeLines = append(routeAgeLines, fmt.Sprintf(
				"shaper_route_oldest_queued_seconds{%s,method=\"%s\",path=\"%s\"} %.3f\n",
				labels[i], escapeLabelValue(method), escapeLabelValue(path), age.Seconds(),
			))
		}
	}
	if len(routeAgeLines) > 0 {
		families = append(families, routeAgeLines)
	}

	for _, lines := range families {
		if err := writeGroup(lines); err != nil {
			return err
		}
	}
	return nil
}
