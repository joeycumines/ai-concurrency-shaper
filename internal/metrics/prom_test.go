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

func TestWritePrometheusGoldenLines(t *testing.T) {
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
	if err := WritePrometheus(&buf, "anthropic", snap); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
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
		`shaper_breaker_state{provider="anthropic",state="open"} 2`,
	}
	for _, want := range wantLines {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing line %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "0xx") {
		t.Errorf("synthetic 0xx bucket must not be exported:\n%s", got)
	}
	if strings.Contains(got, `state="closed"`) || strings.Contains(got, `state="half_open"`) {
		t.Errorf("only the current breaker state may be reported:\n%s", got)
	}
}

func TestWritePrometheusBreakerStates(t *testing.T) {
	tests := []struct {
		cbState string
		want    string
	}{
		{"CLOSED", `shaper_breaker_state{provider="p",state="closed"} 0`},
		{"OPEN", `shaper_breaker_state{provider="p",state="open"} 2`},
		{"HALF_OPEN", `shaper_breaker_state{provider="p",state="half_open"} 1`},
	}
	for _, tt := range tests {
		t.Run(tt.cbState, func(t *testing.T) {
			var buf bytes.Buffer
			snap := Snapshot{CircuitBreaker: &CBStats{State: tt.cbState}}
			if err := WritePrometheus(&buf, "p", snap); err != nil {
				t.Fatalf("WritePrometheus: %v", err)
			}
			if !strings.Contains(buf.String(), tt.want+"\n") {
				t.Errorf("output missing %q:\n%s", tt.want, buf.String())
			}
		})
	}
}

func TestWritePrometheusOmitsBreakerWithoutStats(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheus(&buf, "p", Snapshot{}); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if strings.Contains(buf.String(), "breaker") {
		t.Errorf("breaker series emitted without CircuitBreaker stats:\n%s", buf.String())
	}
}

func TestWritePrometheusEscapesLabelValues(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheus(&buf, "we\"ird\\name\nx", Snapshot{}); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `provider="we\"ird\\name\nx"`) {
		t.Errorf("label value not escaped per Prometheus text format:\n%s", got)
	}
}

func TestWritePrometheusUnnamedProviderLabel(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrometheus(&buf, "default", Snapshot{}); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if !strings.Contains(buf.String(), `shaper_active{provider="default"} 0`) {
		t.Errorf("unnamed provider must export as provider=\"default\":\n%s", buf.String())
	}
}
