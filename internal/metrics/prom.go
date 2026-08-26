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
	"strings"
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

// WritePrometheus writes one provider's snapshot as Prometheus text-format
// exposition lines, every series carrying a provider label so one /metrics
// endpoint can fan out over the whole fleet. Lines are stable and sorted by
// construction; no timestamps are emitted (scrape time is scrape time).
// When snap.CircuitBreaker is nil (breaker disabled or not merged) the breaker
// series is omitted rather than guessed.
func WritePrometheus(w io.Writer, provider string, snap Snapshot) error {
	label := `provider="` + escapeLabelValue(provider) + `"`
	lines := []string{
		fmt.Sprintf("shaper_active{%s} %d", label, snap.Active),
		fmt.Sprintf("shaper_queued{%s} %d", label, snap.Queued),
		fmt.Sprintf("shaper_retries_in_flight{%s} %d", label, snap.RetriesInFlight),
		fmt.Sprintf("shaper_clean_proxied_total{%s} %d", label, snap.TotalProxied),
		fmt.Sprintf("shaper_clean_passthrough_total{%s} %d", label, snap.TotalPassThrough),
		fmt.Sprintf("shaper_aborted_total{%s} %d", label, snap.TotalAborted),
		fmt.Sprintf("shaper_circuit_rejected_total{%s} %d", label, snap.TotalCircuitRejected),
	}
	for i := 1; i < int(statusBuckets); i++ {
		lines = append(lines, fmt.Sprintf("shaper_requests_total{%s,status=%q} %d", label, statusBucketNames[i], snap.StatusCounts[i]))
	}
	if cb := snap.CircuitBreaker; cb != nil {
		state := "closed"
		switch cb.State {
		case "HALF_OPEN":
			state = "half_open"
		case "OPEN":
			state = "open"
		}
		value := 0
		switch state {
		case "half_open":
			value = 1
		case "open":
			value = 2
		}
		lines = append(lines, fmt.Sprintf("shaper_breaker_state{%s,state=%q} %d", label, state, value))
	}
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	return nil
}
