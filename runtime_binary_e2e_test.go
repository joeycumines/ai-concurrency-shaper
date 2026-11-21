package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/history"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// TestBinaryObservationGatewayRoundTrip covers the actual CLI, dedicated metrics
// listener, request pipeline and shutdown ownership in one running executable.
// TCP-only readiness must not create logical requests, and scraping must neither
// consume counters nor enter the proxy's request observation middleware.
func TestBinaryObservationGatewayRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("building and starting the real gateway is an integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("this subprocess shutdown check requires Unix SIGTERM; native gateway integration tests run separately on Windows")
	}
	const alphaRequest = `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	var alphaAttempts, betaAttempts atomic.Int64
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || string(body) != alphaRequest {
			t.Errorf("alpha request: method=%s path=%s body=%q err=%v", r.Method, r.URL.Path, body, err)
		}
		w.Header().Set("Content-Type", "text/plain")
		if alphaAttempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "alpha unavailable")
			return
		}
		_, _ = io.WriteString(w, "alpha completed")
	}))
	t.Cleanup(alpha.Close)
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/native" {
			t.Errorf("beta request: method=%s path=%s", r.Method, r.URL.Path)
		}
		betaAttempts.Add(1)
		_, _ = io.WriteString(w, "beta native")
	}))
	t.Cleanup(beta.Close)

	path := filepath.Join(t.TempDir(), "gateway history # ü 東京.db")
	started := time.Now()
	sub, output := startProxyCmd(t,
		"-bind", "127.0.0.1:0", "-metrics-bind", "127.0.0.1:0",
		"-history-db", path, "-history-durability", "full", "-diagnostic-capture-bytes", "4096",
		"--provider=alpha", "-prefix", "/a", "-upstream", alpha.URL,
		"-retry", "1", "-retry-wait-min", "1ms", "-retry-wait-max", "1ms", "-retry-min-delay", "0",
		"-release-cooldown", "0", "-failure-hold", "0", "-circuit-breaker=false",
		"--provider=beta", "-prefix", "/b", "-upstream", beta.URL,
		"-retry", "0", "-release-cooldown", "0", "-circuit-breaker=false",
	)
	proxyAddr := sub.waitForListeningAddr(t, output)
	metricsAddr := binaryObservationMetricsAddress(t, sub, output)
	if proxyAddr == metricsAddr {
		t.Fatal("proxy and metrics unexpectedly share a listener")
	}
	for range 3 {
		for _, addr := range []string{proxyAddr, metricsAddr} {
			if err := waitTCPReady(addr, 5*time.Second); err != nil {
				t.Fatalf("TCP readiness: %v\n%s", err, output.String())
			}
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	metricsURL := "http://" + metricsAddr + "/metrics"
	initial := binaryObservationScrape(t, client, metricsURL)
	for _, provider := range []string{"alpha", "beta", observation.UnresolvedProvider} {
		for _, name := range []string{"requests_received_total", "requests_terminal_total", "upstream_attempts_total"} {
			binaryObservationWantSample(t, initial, "shaper_observed_"+name, map[string]string{"provider": provider}, 0)
		}
	}

	for _, tc := range []struct {
		method, path, body, response string
		status                       int
	}{
		{http.MethodPost, "/a/v1/messages", alphaRequest, "alpha completed", http.StatusOK},
		{http.MethodGet, "/b/native", "", "beta native", http.StatusOK},
		{http.MethodGet, "/outside", "", "", http.StatusNotFound},
	} {
		req, err := http.NewRequest(tc.method, "http://"+proxyAddr+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", tc.method, tc.path, err, output.String())
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != tc.status || (tc.response != "" && string(body) != tc.response) {
			t.Fatalf("%s: status=%d body=%q read=%v close=%v\n%s", tc.path, response.StatusCode, body, readErr, closeErr, output.String())
		}
	}
	if alphaAttempts.Load() != 2 || betaAttempts.Load() != 1 {
		t.Fatalf("actual upstream exchanges: alpha=%d beta=%d", alphaAttempts.Load(), betaAttempts.Load())
	}

	// EOF can reach the client just before deferred journal/fact publication.
	// Wait for both measurement layers to settle rather than assuming ordering.
	deadline := time.Now().Add(10 * time.Second)
	for {
		families := binaryObservationScrape(t, client, metricsURL)
		settled := true
		for _, provider := range []string{"alpha", "beta", observation.UnresolvedProvider} {
			v, err := binaryObservationSample(families, "shaper_observed_requests_terminal_total", map[string]string{"provider": provider})
			settled = settled && err == nil && v == 1
		}
		for _, provider := range []string{"alpha", "beta"} {
			v, err := binaryObservationSample(families, "shaper_requests_total", map[string]string{"provider": provider, "status": "2xx"})
			settled = settled && err == nil && v == 1
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observations did not settle after completed HTTP requests\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := map[string]observation.Counters{
		"alpha":                        {Received: 1, Terminal: 1, Completed: 1, Attempts: 2, Retries: 1, Upstream5xx: 1, HTTP: [2][5]uint64{{0, 1}, {0, 1, 0, 0, 1}}},
		"beta":                         {Received: 1, Terminal: 1, Completed: 1, Attempts: 1, HTTP: [2][5]uint64{{0, 1}, {0, 1}}},
		observation.UnresolvedProvider: {Received: 1, Terminal: 1, Rejected: 1, HTTP: [2][5]uint64{{0, 0, 0, 1}}, Failures: [8]uint64{1: 1}},
	}
	for range 4 {
		binaryObservationCheckScrape(t, binaryObservationScrape(t, client, metricsURL), want)
	}

	// Read through the supported SQLite interface while the gateway owns the
	// writer, retaining the exact published identities for comparison on reopen.
	liveReader, err := history.Open(path, history.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("open live read-only history: %v", err)
	}
	t.Cleanup(func() { _ = liveReader.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var before observation.RequestResult
	for {
		before, err = liveReader.Requests(ctx, observation.RequestQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if before.Total >= 3 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("history did not publish three facts: %+v\n%s", before, output.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if before.Total != 3 || len(before.Requests) != 3 || before.Truncated {
		t.Fatalf("readiness/scrapes introduced phantom requests: %+v", before)
	}
	if err = liveReader.Close(ctx); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	if err = sub.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v\n%s", err, output.String())
	}
	if err = sub.wait(t, 15*time.Second); err != nil {
		t.Fatalf("graceful gateway shutdown: %v\n%s", err, output.String())
	}
	ended := time.Now()
	if strings.Contains(output.String(), "graceful shutdown incomplete") {
		t.Fatalf("gateway did not complete graceful shutdown\n%s", output.String())
	}

	reopened, err := history.Open(path, history.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("reopen after process exit: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	readCtx, readCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readCancel()
	after, err := reopened.Requests(readCtx, observation.RequestQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("facts or identities changed across shutdown/reopen:\nbefore=%+v\nafter=%+v", before, after)
	}
	binaryObservationCheckFacts(t, after.Requests)
	intervals, _, truncated, err := reopened.Read(readCtx, observation.Query{From: started, To: ended.Add(time.Second), MaxPoints: 120}, observation.Exclusion{})
	if err != nil || truncated {
		t.Fatalf("read durable intervals: %v truncated=%v", err, truncated)
	}
	got := make(map[string]observation.Counters)
	for _, interval := range intervals {
		if interval.ElapsedNS <= 0 || interval.Epoch != after.Requests[0].Epoch || interval.Gap || interval.Overlap {
			t.Fatalf("invalid persisted measurement boundary: %+v", interval)
		}
		counts := got[interval.Provider]
		counts.Add(interval.Counts)
		got[interval.Provider] = counts
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable intervals do not match scraped provider cohorts:\ngot=%+v\nwant=%+v", got, want)
	}
	health := reopened.Health()
	if !health.Enabled || health.CommittedThrough.IsZero() || health.DroppedBatches != 0 || health.DroppedFacts != 0 || health.WriteErrors != 0 {
		t.Fatalf("history lost observations: %+v", health)
	}
}

func binaryObservationMetricsAddress(t *testing.T, sub *proxySubprocess, output *lockedBuffer) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		for line := range strings.SplitSeq(output.String(), "\n") {
			if _, address, ok := strings.Cut(line, "metrics endpoint listening on "); ok {
				if address, ok = strings.CutSuffix(strings.TrimSpace(address), "/metrics"); ok {
					return address
				}
			}
		}
		select {
		case <-sub.done:
			t.Fatalf("gateway exited before publishing metrics address: %v\n%s", sub.waitErr, output.String())
		case <-deadline.C:
			t.Fatalf("gateway did not publish metrics address\n%s", output.String())
		case <-tick.C:
		}
	}
}

func binaryObservationScrape(t *testing.T, client *http.Client, address string) map[string]*dto.MetricFamily {
	t.Helper()
	response, err := client.Get(address)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scrape status=%d", response.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		t.Fatalf("official Prometheus text parser rejected combined exposition: %v", err)
	}
	return families
}

func binaryObservationSample(families map[string]*dto.MetricFamily, name string, labels map[string]string) (float64, error) {
	var selected *dto.Metric
	for _, metric := range families[name].GetMetric() {
		if len(metric.GetLabel()) != len(labels) {
			continue
		}
		matches := true
		for _, label := range metric.GetLabel() {
			value, ok := labels[label.GetName()]
			matches = matches && ok && value == label.GetValue()
		}
		if !matches {
			continue
		}
		if selected != nil {
			return 0, fmt.Errorf("duplicate sample %s%v", name, labels)
		}
		selected = metric
	}
	if selected == nil {
		return 0, fmt.Errorf("missing sample %s%v", name, labels)
	}
	switch {
	case selected.Counter != nil:
		return selected.GetCounter().GetValue(), nil
	case selected.Gauge != nil:
		return selected.GetGauge().GetValue(), nil
	case selected.Untyped != nil:
		return selected.GetUntyped().GetValue(), nil
	case selected.Histogram != nil:
		return float64(selected.GetHistogram().GetSampleCount()), nil
	default:
		return 0, fmt.Errorf("unexpected metric type %s%v", name, labels)
	}
}

func binaryObservationWantSample(t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string, want float64) {
	t.Helper()
	got, err := binaryObservationSample(families, name, labels)
	if err != nil || got != want {
		t.Fatalf("%s%v = %v, want %v: %v", name, labels, got, want, err)
	}
}

func binaryObservationCheckScrape(t *testing.T, families map[string]*dto.MetricFamily, want map[string]observation.Counters) {
	t.Helper()
	if len(families["shaper_observed_requests_received_total"].GetMetric()) != len(want) {
		t.Fatal("unexpected provider cohort in the actual metrics endpoint")
	}
	for provider, counts := range want {
		labels := map[string]string{"provider": provider}
		for name, value := range map[string]uint64{
			"requests_received_total": counts.Received, "requests_terminal_total": counts.Terminal,
			"classified_completed_total": counts.Completed, "requests_failed_total": counts.Failed,
			"requests_aborted_total": counts.Aborted, "requests_rejected_total": counts.Rejected,
			"requests_unknown_total": counts.Unknown, "requests_cancelled_total": counts.Cancelled,
			"upstream_attempts_total": counts.Attempts, "upstream_retries_total": counts.Retries,
			"auth_resends_total": counts.AuthResends, "upstream_5xx_total": counts.Upstream5xx,
			"usage_reports_total": 0, "active_requests": 0, "queued_requests": 0, "upstream_exchanges": 0,
			"client_duration_seconds": 1, "attempt_duration_seconds": counts.Attempts,
		} {
			binaryObservationWantSample(t, families, "shaper_observed_"+name, labels, float64(value))
		}
		for side, sideName := range []string{"downstream", "upstream"} {
			for class, className := range []string{"1xx", "2xx", "3xx", "4xx", "5xx"} {
				binaryObservationWantSample(t, families, "shaper_observed_http_status_total", map[string]string{"provider": provider, "side": sideName, "class": className}, float64(counts.HTTP[side][class]))
			}
		}
		if provider == observation.UnresolvedProvider {
			continue
		}
		for class, className := range []string{"1xx", "2xx", "3xx", "4xx", "5xx"} {
			binaryObservationWantSample(t, families, "shaper_requests_total", map[string]string{"provider": provider, "status": className}, float64(counts.HTTP[0][class]))
		}
		binaryObservationWantSample(t, families, "shaper_active", labels, 0)
		binaryObservationWantSample(t, families, "shaper_queued", labels, 0)
		binaryObservationWantSample(t, families, "shaper_aborted_total", labels, 0)
	}
	binaryObservationWantSample(t, families, "shaper_clean_proxied_total", map[string]string{"provider": "alpha"}, 1)
	binaryObservationWantSample(t, families, "shaper_clean_passthrough_total", map[string]string{"provider": "beta"}, 1)
	for name, value := range map[string]float64{"history_enabled": 1, "history_writer_healthy": 1, "history_dropped_batches_total": 0, "history_dropped_facts_total": 0, "history_write_errors_total": 0} {
		binaryObservationWantSample(t, families, "shaper_observed_"+name, nil, value)
	}
}

func binaryObservationCheckFacts(t *testing.T, facts []observation.RequestFact) {
	t.Helper()
	seen := make(map[string]bool)
	seenProviders := make(map[string]bool)
	for _, fact := range facts {
		if fact.ID == "" || seen[fact.ID] || fact.Epoch == "" || fact.JournalID == 0 || fact.Live || fact.Cancelled || fact.DurationNS <= 0 || !fact.Finished.After(fact.Started) || fact.AttemptsTruncated || fact.Usage != nil {
			t.Fatalf("invalid persisted terminal fact: %+v", fact)
		}
		seen[fact.ID] = true
		if seenProviders[fact.Provider] || fact.ReceivedProvider != fact.Provider || fact.Epoch != facts[0].Epoch {
			t.Fatalf("provider or process identity changed: %+v", fact)
		}
		seenProviders[fact.Provider] = true
		var path, method string
		var statuses []int
		switch fact.Provider {
		case "alpha":
			path, method, statuses = "/a/v1/messages", http.MethodPost, []int{503, 200}
		case "beta":
			path, method, statuses = "/b/native", http.MethodGet, []int{200}
		case observation.UnresolvedProvider:
			path, method = "/outside", http.MethodGet
		default:
			t.Fatalf("unexpected persisted provider %q", fact.Provider)
		}
		wantStatus, wantOutcome := http.StatusOK, observation.OutcomeCompleted
		if fact.Provider == observation.UnresolvedProvider {
			wantStatus, wantOutcome = http.StatusNotFound, observation.OutcomeRejected
			if fact.Stage != observation.StageAdmission || fact.Fingerprint == "" {
				t.Fatalf("unmatched request lost diagnostic classification: %+v", fact)
			}
		}
		if fact.Path != path || fact.Method != method || fact.Status != wantStatus || fact.Outcome != wantOutcome || fact.AttemptCount != uint64(len(statuses)) || len(fact.Attempts) != len(statuses) {
			t.Fatalf("persisted request does not describe the actual HTTP exchange: %+v", fact)
		}
		for i, attempt := range fact.Attempts {
			kind, outcome := observation.AttemptInitial, observation.OutcomeCompleted
			if i > 0 {
				kind = observation.AttemptRetry
			}
			if statuses[i] >= 400 {
				outcome = observation.OutcomeFailed
			}
			if attempt.Provider != fact.Provider || attempt.Number != uint64(i+1) || attempt.Kind != kind || attempt.Status != statuses[i] || attempt.Outcome != outcome || attempt.DurationNS <= 0 || !attempt.Finished.After(attempt.Started) {
				t.Fatalf("persisted attempt does not describe the actual upstream exchange: %+v", attempt)
			}
		}
	}
}
