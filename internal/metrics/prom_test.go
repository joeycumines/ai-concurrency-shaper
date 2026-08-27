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
	"bytes"
	"strings"
	"testing"
)

func TestWritePrometheusFleetGoldenLines(t *testing.T) {
	c := NewCollector()
	// Status buckets are fed by RecordStatus (the proxy calls it once per
	// committed response); RecordRequest feeds routes/throughput/log ring.
	c.RecordStatus(200)
	c.RecordStatus(200)
	c.RecordStatus(404)
	c.RecordRequest("POST", "/v1/messages", 200, 0, true)
	c.RecordRequest("POST", "/v1/messages", 200, 0, true)
	c.RecordRequest("GET", "/health", 404, 0, false)
	c.RecordAbortedRequest("POST", "/v1/messages", 0, 0, true)
	// Clean-completion counters are driven by the proxy after the exchange
	// finishes; RecordRequest only feeds routes/throughput/log ring.
	c.IncProxied()
	c.IncProxied()
	c.IncPassThrough()

	snap := c.Snapshot()
	snap.CircuitBreaker = &CBStats{State: "OPEN"}

	var buf bytes.Buffer
	if err := WritePrometheusFleet(&buf, []ProviderSnapshot{{Name: "anthropic", Snapshot: snap}}); err != nil {
		t.Fatalf("WritePrometheusFleet: %v", err)
	}
	got := buf.String()

	wantLines := []string{
		`shaper_active{provider="anthropic"} 0`,
		`shaper_queued{provider="anthropic"} 0`,
		`shaper_retries_in_flight{provider="anthropic"} 0`,
		`shaper_clean_proxied_total{provider="anthropic"} 2`,
		`shaper_clean_passthrough_total{provider="anthropic"} 1`,
		`shaper_aborted_total{provider="anthropic"} 1`,
		`shaper_circuit_rejected_total{provider="anthropic"} 0`,
		`shaper_requests_total{provider="anthropic",status="1xx"} 0`,
		`shaper_requests_total{provider="anthropic",status="2xx"} 2`,
		`shaper_requests_total{provider="anthropic",status="3xx"} 0`,
		`shaper_requests_total{provider="anthropic",status="4xx"} 1`,
		`shaper_requests_total{provider="anthropic",status="5xx"} 0`,
		`shaper_breaker_state{provider="anthropic"} 2`,
	}
	for i, want := range wantLines {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing line %q in output:\n%s", want, got)
			continue
		}
		if i > 0 && strings.Index(got, wantLines[i-1]+"\n") > strings.Index(got, want+"\n") {
			t.Errorf("line %q appears before %q; stable ordering broken:\n%s", want, wantLines[i-1], got)
		}
	}
	if strings.Contains(got, "0xx") {
		t.Errorf("synthetic 0xx bucket must not be exported:\n%s", got)
	}
	if strings.Contains(got, "state=") {
		t.Errorf("breaker series must carry no state label (value encodes it):\n%s", got)
	}
}

func TestWritePrometheusBreakerStates(t *testing.T) {
	tests := []struct {
		cbState string
		want    string
	}{
		{"CLOSED", `shaper_breaker_state{provider="p"} 0`},
		{"OPEN", `shaper_breaker_state{provider="p"} 2`},
		{"HALF_OPEN", `shaper_breaker_state{provider="p"} 1`},
		{"UNEXPECTED", `shaper_breaker_state{provider="p"} 0`},
	}
	for _, tt := range tests {
		t.Run(tt.cbState, func(t *testing.T) {
			var buf bytes.Buffer
			snap := Snapshot{CircuitBreaker: &CBStats{State: tt.cbState}}
			if err := WritePrometheusFleet(&buf, []ProviderSnapshot{{Name: "p", Snapshot: snap}}); err != nil {
				t.Fatalf("WritePrometheusFleet: %v", err)
			}
			if !strings.Contains(buf.String(), tt.want+"\n") {
				t.Errorf("output missing %q:\n%s", tt.want, buf.String())
			}
		})
	}
}

func TestWritePrometheusOmitsBreakerWithoutStats(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheusFleet(&buf, []ProviderSnapshot{{Name: "p", Snapshot: Snapshot{}}}); err != nil {
		t.Fatalf("WritePrometheusFleet: %v", err)
	}
	if strings.Contains(buf.String(), "breaker") {
		t.Errorf("breaker series emitted without CircuitBreaker stats:\n%s", buf.String())
	}
}

func TestWritePrometheusEscapesLabelValues(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheusFleet(&buf, []ProviderSnapshot{{Name: "we\"ird\\name\nx"}}); err != nil {
		t.Fatalf("WritePrometheusFleet: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `provider="we\"ird\\name\nx"`) {
		t.Errorf("label value not escaped per Prometheus text format:\n%s", got)
	}
}

func TestWritePrometheusUnnamedProviderLabel(t *testing.T) {
	// Each case runs as its own one-provider fleet: production cannot
	// combine them because validateMulti enforces unique provider names and
	// an empty name only exists in single-provider legacy mode - two
	// "default"-labeled providers would emit duplicate identical series,
	// which the text format forbids.
	t.Run("explicit default", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WritePrometheusFleet(&buf, []ProviderSnapshot{{Name: "default"}}); err != nil {
			t.Fatalf("WritePrometheusFleet: %v", err)
		}
		if !strings.Contains(buf.String(), `shaper_active{provider="default"} 0`) {
			t.Errorf("explicit default label missing:\n%s", buf.String())
		}
	})
	t.Run("empty name defaults to default", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WritePrometheusFleet(&buf, []ProviderSnapshot{{Name: ""}}); err != nil {
			t.Fatalf("WritePrometheusFleet: %v", err)
		}
		if !strings.Contains(buf.String(), `shaper_active{provider="default"} 0`) {
			t.Errorf("empty name must export as provider=\"default\":\n%s", buf.String())
		}
	})
}

func TestWritePrometheusFleetEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheusFleet(&buf, nil); err != nil {
		t.Fatalf("WritePrometheusFleet: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("empty fleet emitted output:\n%s", buf.String())
	}
}

// familyRunCounts maps each metric name to the number of CONTIGUOUS blocks it
// forms in the exposition. A spec-compliant writer produces exactly one run
// per name; any value above one means two providers' series for the same
// family were interleaved with another family between them.
func familyRunCounts(t *testing.T, output string) map[string]int {
	t.Helper()
	runs := map[string]int{}
	prev := ""
	for line := range strings.SplitSeq(output, "\n") {
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, '{')
		if idx <= 0 {
			t.Fatalf("unparseable exposition line: %q", line)
		}
		if name := line[:idx]; name != prev {
			runs[name]++
			prev = name
		}
	}
	return runs
}

func TestWritePrometheusFleetGroupsByMetricName(t *testing.T) {
	// Provider b lacks a breaker on purpose: the breaker family must stay
	// contiguous across only the providers carrying it (ragged membership).
	a := Snapshot{Active: 1, Queued: 2, TotalProxied: 5, StatusCounts: [6]int64{0, 1, 2, 0, 1, 0}, CircuitBreaker: &CBStats{State: "CLOSED"}}
	b := Snapshot{Active: 3, Queued: 0, TotalProxied: 9, StatusCounts: [6]int64{0, 0, 7, 0, 0, 2}}

	var buf bytes.Buffer
	if err := WritePrometheusFleet(&buf, []ProviderSnapshot{
		{Name: "a", Snapshot: a},
		{Name: "b", Snapshot: b},
	}); err != nil {
		t.Fatalf("WritePrometheusFleet: %v", err)
	}

	runs := familyRunCounts(t, buf.String())
	// 7 gauges + shaper_requests_total (one family, five labeled series)
	// + breaker = 9 distinct metric names.
	wantFamilies := 9
	if len(runs) != wantFamilies {
		t.Errorf("exported %d distinct metric names, want %d:\n%s", len(runs), wantFamilies, buf.String())
	}
	for name, n := range runs {
		if n != 1 {
			t.Errorf("metric %s forms %d contiguous blocks, want exactly 1:\n%s", name, n, buf.String())
		}
	}

	// Non-vacuity control: the SAME series interleaved per provider must be
	// rejected by the identical check, proving the grouping assertion can
	// fail (this is the shape the old per-provider writer produced).
	interleaved := `shaper_active{provider="a"} 1
shaper_queued{provider="a"} 2
shaper_active{provider="b"} 3
shaper_queued{provider="b"} 0
`
	if runs := familyRunCounts(t, interleaved); runs["shaper_active"] == 1 && runs["shaper_queued"] == 1 {
		t.Fatalf("grouping checker accepted interleaved output - assertion is vacuous")
	}

	// The two-provider block must also contain every expected series.
	for _, want := range []string{
		`shaper_active{provider="a"} 1`,
		`shaper_active{provider="b"} 3`,
		`shaper_requests_total{provider="a",status="2xx"} 2`,
		`shaper_requests_total{provider="b",status="5xx"} 2`,
		`shaper_breaker_state{provider="a"} 0`,
	} {
		if !strings.Contains(buf.String(), want+"\n") {
			t.Errorf("fleet output missing %q:\n%s", want, buf.String())
		}
	}
	if strings.Count(buf.String(), "shaper_breaker_state") != 1 {
		t.Errorf("breaker series must appear once (provider b has none):\n%s", buf.String())
	}
}
