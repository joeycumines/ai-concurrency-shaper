package observation

import (
	"io"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

type metricDefinition struct {
	name, help string
	value      func(Counters) uint64
}

var counterDefinitions = []metricDefinition{
	{"conversion_dropped_total", "Conversion report entries omitted by the bounded codec report; omitted entries are not guessed as losses or notes.", func(c Counters) uint64 { return c.ConversionDropped }},
	{"requests_received_total", "Logical requests at original ingress scope.", func(c Counters) uint64 { return c.Received }},
	{"requests_terminal_total", "Logical requests with a classified terminal outcome.", func(c Counters) uint64 { return c.Terminal }},
	{"classified_completed_total", "Logical requests classified completed; HTTP status alone is insufficient.", func(c Counters) uint64 { return c.Completed }},
	{"requests_failed_total", "Logical requests which failed.", func(c Counters) uint64 { return c.Failed }},
	{"requests_aborted_total", "Logical requests aborted before successful completion.", func(c Counters) uint64 { return c.Aborted }},
	{"requests_rejected_total", "Logical requests refused locally.", func(c Counters) uint64 { return c.Rejected }},
	{"requests_unknown_total", "Logical requests whose terminal outcome is unknown.", func(c Counters) uint64 { return c.Unknown }},
	{"requests_cancelled_total", "Logical requests with observed cooperative cancellation.", func(c Counters) uint64 { return c.Cancelled }},
	{"upstream_attempts_total", "Actual signed upstream RoundTrip attempts, including retries and credential resends.", func(c Counters) uint64 { return c.Attempts }},
	{"upstream_retries_total", "Actual upstream attempts made by retry policy.", func(c Counters) uint64 { return c.Retries }},
	{"auth_resends_total", "Actual upstream attempts made by credential recovery.", func(c Counters) uint64 { return c.AuthResends }},
	{"upstream_429_total", "Actual upstream response headers with HTTP 429.", func(c Counters) uint64 { return c.Upstream429 }},
	{"upstream_401_total", "Actual upstream response headers with HTTP 401.", func(c Counters) uint64 { return c.Upstream401 }},
	{"upstream_5xx_total", "Actual upstream response headers with HTTP 5xx.", func(c Counters) uint64 { return c.Upstream5xx }},
	{"conversion_losses_total", "Explicitly approved conversion losses.", func(c Counters) uint64 { return c.ConversionLosses }},
	{"conversion_notes_total", "Conversion notes; these are not loss decisions.", func(c Counters) uint64 { return c.ConversionNotes }},
	{"usage_reports_total", "First valid source usage report per request.", func(c Counters) uint64 { return c.UsageReports }},
	{"usage_input_known_total", "First usage reports with known input tokens, including known zero.", func(c Counters) uint64 { return c.UsageInputKnown }},
	{"usage_output_known_total", "First usage reports with known output tokens, including known zero.", func(c Counters) uint64 { return c.UsageOutputKnown }},
	{"usage_total_known_total", "First usage reports with known total tokens, including known zero.", func(c Counters) uint64 { return c.UsageTotalKnown }},
	{"usage_input_tokens_total", "Input tokens from first valid reports; corrections are recorded separately.", func(c Counters) uint64 { return c.UsageInputTokens }},
	{"usage_output_tokens_total", "Output tokens from first valid reports; corrections are recorded separately.", func(c Counters) uint64 { return c.UsageOutputTokens }},
	{"usage_total_tokens_total", "Total tokens from first valid reports; never inferred by summing fields.", func(c Counters) uint64 { return c.UsageTotalTokens }},
	{"usage_corrections_total", "Later usage reports changing presence or values; source spelling alone is not a correction.", func(c Counters) uint64 { return c.UsageCorrections }},
	{"usage_overflow_total", "Token field additions omitted because a uint64 total would overflow.", func(c Counters) uint64 { return c.UsageOverflow }},
}

type prometheusCollector struct {
	hub                                                                       *Hub
	counter                                                                   []*prometheus.Desc
	active, queued, exchanges, status, failures, duration, admission, attempt *prometheus.Desc
	history                                                                   map[string]*prometheus.Desc
}

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("shaper_observed_"+name, help, labels, nil)
}

// PrometheusCollector reads process-lifetime totals. It does not seal intervals,
// reset counters, wait for the writer or execute a database query.
func (h *Hub) PrometheusCollector() prometheus.Collector {
	c := &prometheusCollector{hub: h, active: desc("active_requests", "Current logical requests including admission wait.", "provider"), queued: desc("queued_requests", "Current logical requests in the union of admission waits.", "provider"), exchanges: desc("upstream_exchanges", "Actual attempts not yet ended at the body boundary.", "provider"), status: desc("http_status_total", "Observed response classes; side identifies downstream or upstream headers.", "provider", "side", "class"), failures: desc("failures_by_stage_total", "Failed or rejected logical requests by bounded terminal stage.", "provider", "stage"), duration: desc("client_duration_seconds", "Ingress to classified terminal completion including queue time.", "provider"), admission: desc("admission_wait_seconds", "Union of admission waits, once per terminal request which waited.", "provider"), attempt: desc("attempt_duration_seconds", "Actual upstream attempt through body terminal outcome; 101 ends at handshake.", "provider"), history: make(map[string]*prometheus.Desc)}
	for _, d := range counterDefinitions {
		c.counter = append(c.counter, desc(d.name, d.help, "provider"))
	}
	for n, help := range map[string]string{"history_enabled": "Whether optional SQLite history is enabled.", "history_writer_healthy": "Whether the latest history transaction succeeded.", "history_pending_packets": "Packets queued for history persistence.", "history_buffered_bytes": "Conservative payload bytes owned by the queue and writer; not RSS.", "history_buffer_byte_capacity": "Maximum conservative payload bytes owned by the queue and writer.", "history_queue_capacity": "Bounded asynchronous history packet queue capacity.", "history_committed_through_timestamp_seconds": "Latest durably committed observation boundary in Unix seconds.", "history_dropped_batches_total": "History packets omitted by the bounded queue.", "history_dropped_facts_total": "Request facts omitted before persistence.", "history_dropped_markers_total": "Markers omitted before persistence.", "history_write_errors_total": "Failed asynchronous history transactions."} {
		c.history[n] = desc(n, help)
	}
	return c
}
func (c *prometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.counter {
		ch <- d
	}
	for _, d := range []*prometheus.Desc{c.active, c.queued, c.exchanges, c.status, c.failures, c.duration, c.admission, c.attempt} {
		ch <- d
	}
	for _, d := range c.history {
		ch <- d
	}
}
func histogramMetric(d *prometheus.Desc, h Histogram, provider string) prometheus.Metric {
	b := make(map[float64]uint64, len(DurationBounds))
	var total uint64
	for i, bound := range DurationBounds {
		total = add64(total, h.Bins[i])
		b[bound] = total
	}
	return prometheus.MustNewConstHistogram(d, h.Count, h.Sum, b, provider)
}
func (c *prometheusCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.hub.Snapshot()
	for _, p := range s.Providers {
		for i, d := range counterDefinitions {
			ch <- prometheus.MustNewConstMetric(c.counter[i], prometheus.CounterValue, float64(d.value(p.Totals)), p.Provider)
		}
		ch <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, float64(p.Active), p.Provider)
		ch <- prometheus.MustNewConstMetric(c.queued, prometheus.GaugeValue, float64(p.Queued), p.Provider)
		ch <- prometheus.MustNewConstMetric(c.exchanges, prometheus.GaugeValue, float64(p.Exchanges), p.Provider)
		for side, name := range []string{"downstream", "upstream"} {
			for bucket, class := range []string{"1xx", "2xx", "3xx", "4xx", "5xx"} {
				ch <- prometheus.MustNewConstMetric(c.status, prometheus.CounterValue, float64(p.Totals.HTTP[side][bucket]), p.Provider, name, class)
			}
		}
		for i, stage := range Stages {
			ch <- prometheus.MustNewConstMetric(c.failures, prometheus.CounterValue, float64(p.Totals.Failures[i]), p.Provider, string(stage))
		}
		ch <- histogramMetric(c.duration, p.Duration, p.Provider)
		ch <- histogramMetric(c.admission, p.Admission, p.Provider)
		ch <- histogramMetric(c.attempt, p.AttemptDuration, p.Provider)
	}
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	v := s.History
	for n, value := range map[string]float64{"history_enabled": b(v.Enabled), "history_writer_healthy": b(v.Enabled && v.Healthy), "history_pending_packets": float64(v.Pending), "history_buffered_bytes": float64(v.BufferedBytes), "history_buffer_byte_capacity": float64(v.ByteCapacity), "history_queue_capacity": float64(v.Capacity), "history_dropped_batches_total": float64(v.DroppedBatches), "history_dropped_facts_total": float64(v.DroppedFacts), "history_dropped_markers_total": float64(v.DroppedMarkers), "history_write_errors_total": float64(v.WriteErrors)} {
		typ := prometheus.GaugeValue
		if len(n) > 6 && n[len(n)-6:] == "_total" {
			typ = prometheus.CounterValue
		}
		ch <- prometheus.MustNewConstMetric(c.history[n], typ, value)
	}
	if !v.CommittedThrough.IsZero() {
		ch <- prometheus.MustNewConstMetric(c.history["history_committed_through_timestamp_seconds"], prometheus.GaugeValue, float64(v.CommittedThrough.UnixNano())/1e9)
	}
}

// WritePrometheus appends valid text families beside the existing exposition.
func (h *Hub) WritePrometheus(w io.Writer) error {
	r := prometheus.NewRegistry()
	if err := r.Register(h.PrometheusCollector()); err != nil {
		return err
	}
	families, err := r.Gather()
	if err != nil {
		return err
	}
	for _, family := range families {
		if _, err = expfmt.MetricFamilyToText(w, family); err != nil {
			return err
		}
	}
	return nil
}
