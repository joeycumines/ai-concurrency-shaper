package tui

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

// Optional output from a behavioral rendering test, not a second mock UI.
// All gallery frames are the production Model.View fed deterministic synthetic
// observations. The flag is a Go test tool option, never application config.
var dashboardRenderDir = flag.String("dashboard-render-dir", "", "write the deterministic Model.View gallery to this directory")

func dashboardFixture(t *testing.T) Model {
	m, _, _ := runtimeTestModel(t, 160, 48)
	a := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := a.Add(15 * time.Minute)
	m.insights.from = a
	m.insights.to = b
	m.insights.cursor = a.Add(8 * time.Minute)
	m.insights.follow = false
	q := observation.Query{From: a, To: b, MaxPoints: 120}
	result := observation.QueryResult{Query: q, SourceIntervals: 2700, History: observation.Health{Enabled: true, Healthy: true, Capacity: 64, Pending: 2, CommittedThrough: b}}
	names := []string{"alpha", "beta"}
	for pi, name := range names {
		var points []observation.Point
		for i := range 120 {
			start := a.Add(time.Duration(i) * 7500 * time.Millisecond)
			end := start.Add(7500 * time.Millisecond)
			burst := math.Exp(-math.Pow((float64(i)-62)/9, 2))
			n := uint64(20 + 8*math.Sin(float64(i)/5) + float64(pi)*12 + burst*45)
			failed := uint64(0)
			if i > 56 && i < 70 {
				failed = uint64(3 + pi)
			}
			p := observation.Point{Start: start, End: end, ElapsedNS: int64(7500 * time.Millisecond), Coverage: 1, Complete: true, Samples: 15, Counts: observation.Counters{Received: n, Terminal: n - 2, Completed: n - 2 - failed, Failed: failed, Attempts: n + failed, Retries: failed, ConversionLosses: failed / 2, ConversionNotes: 2, UsageReports: n - 2, UsageInputKnown: n - 2, UsageOutputKnown: n - 2}, Gauges: observation.Gauges{Active: observation.Gauge{Area: (5 + burst*10 + float64(pi)) * float64(7500*time.Millisecond), DurationNS: int64(7500 * time.Millisecond), Min: 3, Max: 16, Last: 6, Known: true}, Queued: observation.Gauge{Area: burst * 14 * float64(7500*time.Millisecond), DurationNS: int64(7500 * time.Millisecond), Min: 0, Max: 16, Last: 3, Known: true}}}
			idx := 7 + pi + int(burst*5)
			histN := n - 2
			p.Duration.Bins[idx] = histN / 2
			p.Duration.Bins[min(idx+1, 22)] = histN / 3
			p.Duration.Bins[min(idx+3, 22)] = histN - histN/2 - histN/3
			p.Counts.UsageInputTokens = (n - 2) * uint64(320+pi*40)
			p.Counts.UsageOutputTokens = (n - 2) * uint64(72+i%30)
			p.Counts.UsageTotalKnown = n - 2
			p.Counts.UsageTotalTokens = p.Counts.UsageInputTokens + p.Counts.UsageOutputTokens
			p.Counts.Upstream429 = failed / 2
			p.Counts.Upstream5xx = failed
			p.Counts.AuthResends = uint64(i % 3)
			p.Counts.HTTP[1][1] = n - failed
			p.Counts.HTTP[1][3] = failed / 2
			p.Counts.HTTP[1][4] = failed
			p.Counts.Failures[5] = failed / 2
			p.Counts.Failures[6] = failed - failed/2
			if i >= 90 && i < 100 {
				p.Counts.UsageOutputKnown = 0
				p.Counts.UsageOutputTokens = 0
				p.Counts.UsageTotalKnown = 0
				p.Counts.UsageTotalTokens = 0
			}
			p.Duration.Count = histN
			for bin, count := range p.Duration.Bins {
				if count > 0 {
					p.Duration.Sum += float64(count) * (binLower(bin) + binUpper(bin)) / 2
				}
			}
			p.Admission.Bins[5] = n / 4
			p.Admission.Bins[9] = n / 8
			p.Admission.Count = n/4 + n/8
			p.Admission.Sum = float64(p.Admission.Count) * .12
			p.AttemptDuration = p.Duration
			p.AttemptDuration.Bins[idx] += p.Counts.Attempts - p.Duration.Count
			p.AttemptDuration.Count = p.Counts.Attempts
			p.AttemptDuration.Sum += float64(p.Counts.Attempts-p.Duration.Count) * (binLower(idx) + binUpper(idx)) / 2
			p.Resources = []observation.ResourceIntegral{{Provider: name, ID: "default", Kind: "default", Limit: observation.Gauge{Known: true, DurationNS: p.ElapsedNS, Area: 16 * float64(p.ElapsedNS), Min: 16, Max: 16, Last: 16}, Occupied: p.Gauges.Active, Cooldown: observation.Gauge{Known: true, DurationNS: p.ElapsedNS, Area: 2 * float64(p.ElapsedNS), Min: 1, Max: 4, Last: 2}, Queued: p.Gauges.Queued}}
			if i >= 38 && i <= 40 {
				p.Complete = false
				p.Gap = true
				p.Coverage = .5
				p.ElapsedNS = 0
			}
			points = append(points, p)
		}
		result.ByProvider = append(result.ByProvider, observation.ProviderSeries{Provider: name, Points: points})
	}
	for i := range 120 {
		p := result.ByProvider[0].Points[i]
		other := result.ByProvider[1].Points[i]
		p.Counts.Add(other.Counts)
		mergeHistogram(&p.Duration, other.Duration)
		mergeHistogram(&p.Admission, other.Admission)
		mergeHistogram(&p.AttemptDuration, other.AttemptDuration)
		p.Gauges.Active.Area += other.Gauges.Active.Area
		p.Gauges.Queued.Area += other.Gauges.Queued.Area
		p.Resources = append(p.Resources, other.Resources...)
		result.Points = append(result.Points, p)
	}
	result.Markers = []observation.Marker{{ID: "control-1", At: a.Add(8 * time.Minute), Provider: "alpha", Kind: "control", Summary: "Resize default pool 12 → 16"}, {ID: "failure-1", At: a.Add(7 * time.Minute), Provider: "beta", Kind: "failure", Summary: "Response conversion failures"}}
	m.insights.result = result
	m.insights.snapshot.History = result.History
	for i := range m.providers {
		m.providers[i].snap = metrics.Snapshot{Active: int64(6 + i*3), Queued: int64(4 + i), Throughput: 5.4 + float64(i)*2.3, TotalProxied: 2480, TotalAborted: 2, TotalCancelled: 1, StatusCounts: [6]int64{0, 0, 2480, 0, 12, 4}, Sparkline: []int{1, 2, 2, 3, 3, 2, 4, 3, 4, 5, 4, 3, 2, 3, 4, 6, 7, 8, 6, 5, 7, 8, 9, 8, 7, 6, 5, 5, 6, 7, 5, 4}}
	}
	m.syncActive()
	m.controlPanel.snapshot.Providers[0].Pools[0] = control.PoolState{ID: "default", Limit: 16, Baseline: 12, Effective: 16, Active: 9, Occupied: 12, Cooldown: 3, Waiters: 4}
	m.controlPanel.snapshot.Providers[1].Pools[0] = control.PoolState{ID: "default", Limit: 8, Baseline: 8, Effective: 8, Active: 7, Occupied: 8, Cooldown: 1, Waiters: 5}
	u, _ := url.Parse("https://upstream.example/v1/messages")
	d := diagnostic.Snapshot{Version: 1, RequestID: "req-7f3c9a21", Started: a.Add(8 * time.Minute), ClientProtocol: "anthropic-messages", UpstreamProtocol: "openai-responses", ClientModel: "agent-editor", Model: "reasoning-large", Stream: true, CaptureBudget: 8192, Sealed: true, AllowedLosses: []string{"request_reasoning"}, Capabilities: []string{"reasoning"}, ClientVersions: map[string]string{"Anthropic-Version": "2023-06-01", "X-Stainless-Package-Version": "0.80.0"}, Failures: []diagnostic.Failure{{Stage: "request_decode", Kind: "unknown_field", Path: "$.descendant.new_client_field", Fingerprint: "request_decode/unknown_field/cc248f", Detail: "The client sent a field outside the pinned request contract."}}, Contracts: []diagnostic.Contract{{Name: "anthropic-sdk-go/messages", Version: "pinned snapshot", SHA256: "237db86606cf03d371777a24ea127cbb54adc344fd0a03e92e34209824fbd9af"}}, Decisions: []diagnostic.Decision{{Stage: "request", Feature: "request_reasoning", Path: "$.thinking", Detail: "Explicit approved loss"}}}
	d.Legs[0] = diagnostic.Leg{Name: diagnostic.ClientRequest, Observed: true, BytesKnown: true, ObservedBytes: 139, CapturedBytes: 139, Complete: true, ContentType: "application/json", Body: []byte(`{"model":"agent-editor","new_client_field":true,"messages":[{"role":"user","content":"Update the QR code asset."}]}`)}
	d.Legs[0].ObservedBytes = int64(len(d.Legs[0].Body))
	d.Legs[0].CapturedBytes = len(d.Legs[0].Body)
	e := &journal.Entry{ID: 41, RequestID: d.RequestID, Method: "POST", URL: u, StatusCode: 400, ContentType: "application/json", Diagnostic: &d, RequestHeaders: http.Header{"Anthropic-Version": []string{"2023-06-01"}}, Timing: journal.Timing{QueueStart: a.Add(8 * time.Minute), QueueEnd: a.Add(8 * time.Minute), ResponseComplete: a.Add(8*time.Minute + 12*time.Millisecond)}}
	m.journal.Record(e)
	m.networkFiltered = m.computeVisibleNetworkEntries()
	for i := range 24 {
		f := observation.RequestFact{ID: fmt.Sprintf("req-%08x", 100+i), Provider: names[i%2], Method: "POST", Path: "/v1/messages", Status: 200, Outcome: observation.OutcomeCompleted, Started: a.Add(time.Duration(8*60+i) * time.Second), Finished: a.Add(time.Duration(8*60+i)*time.Second + 600*time.Millisecond), DurationNS: int64(600 * time.Millisecond), AttemptCount: 1}
		if i%5 == 0 {
			f.Status = 400
			f.Outcome = observation.OutcomeFailed
			f.Stage = observation.StageConversionRequest
			f.Fingerprint = "request_decode/unknown_field/cc248f"
			if i == 0 {
				f.ID = d.RequestID
			}
		}
		m.insights.facts.Requests = append(m.insights.facts.Requests, f)
	}
	m.insights.facts.Total = 24
	return m
}
func TestResponsiveDashboardEveryWorkspaceAndVisualization(t *testing.T) {
	base := dashboardFixture(t)
	for _, w := range []int{12, 20, 28, 32, 40, 41, 60, 71, 72, 80, 100, 110, 120, 160, 200} {
		for _, h := range []int{6, 10, 16, 20, 24, 40, 48} {
			for _, dark := range []bool{true, false} {
				m := base
				m.width = w
				m.height = h
				m.themeDark = dark
				m.styles = newTheme(dark)
				for tab := range numTabs {
					m.tab = tab
					assertFrameFits(t, m)
					if tab == tabInsights {
						for view := range insightNames {
							m.insights.view = view
							assertFrameFits(t, m)
						}
					}
				}
			}
		}
	}
}
func TestAdaptiveTabRailKeepsActiveDestinationAndMouseGeometry(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	for w := 5; w <= 180; w++ {
		m.width = w
		for tab := range numTabs {
			m.tab = tab
			visible := false
			for _, cell := range m.adaptiveTabs() {
				if cell.id == tab {
					visible = true
				}
				for x := cell.x; x < cell.x+cell.width; x++ {
					id, ok := m.adaptiveTabAt(x)
					if !ok || id != cell.id {
						t.Fatalf("%d cols tab %d x%d: hit %d,%t", w, tab, x, id, ok)
					}
				}
			}
			if !visible {
				t.Fatalf("active tab %d vanished at %d cols", tab, w)
			}
		}
	}
}
func TestAdaptiveTabRailLegacyArrowsWrapAvailableDestinations(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	if m.enriched() || len(m.availableTabs()) != 6 {
		t.Fatal("regression requires the six-tab legacy model")
	}
	for width := 5; width < 3*len(m.availableTabs())+2; width++ {
		m.width = width
		m.tab = tabDashboard
		for step := 1; step <= 2*len(m.availableTabs()); step++ {
			id, ok := m.adaptiveTabAt(0)
			want := tabID((len(m.availableTabs()) - step%len(m.availableTabs())) % len(m.availableTabs()))
			if !ok || id != want {
				t.Fatalf("width %d left step %d: got %d,%t, want %d", width, step, id, ok, want)
			}
			m.tab = id
			_ = m.renderAdaptiveTabs() // Following the arrow must remain renderable.
		}
		for step := 1; step <= 2*len(m.availableTabs()); step++ {
			id, ok := m.adaptiveTabAt(width - 1)
			want := tabID(step % len(m.availableTabs()))
			if !ok || id != want {
				t.Fatalf("width %d right step %d: got %d,%t, want %d", width, step, id, ok, want)
			}
			m.tab = id
			_ = m.renderAdaptiveTabs()
		}
	}
}
func TestRenderDashboardGallery(t *testing.T) {
	m := dashboardFixture(t)
	cases := []struct {
		name string
		w, h int
		dark bool
		tab  tabID
		view int
		mode string
	}{
		{"overview-light-40x20", 40, 20, false, tabDashboard, 0, ""},
		{"rates-light-40x20", 40, 20, false, tabInsights, insightRates, ""},
		{"controls-light-40x20", 40, 20, false, tabControls, 0, ""},
		{"rates-dark-40x40", 40, 40, true, tabInsights, insightRates, ""},
		{"usage-light-40x20", 40, 20, false, tabInsights, insightUsage, ""},
		{"usage-dark-160x48", 160, 48, true, tabInsights, insightUsage, ""},
		{"fidelity-light-160x48", 160, 48, false, tabInsights, insightFidelity, ""},
		{"attempts-dark-120x40", 120, 40, true, tabInsights, insightAttempts, ""},
		{"overview-dark-40x20", 40, 20, true, tabDashboard, 0, ""},
		{"rates-dark-40x20", 40, 20, true, tabInsights, insightRates, ""},
		{"heatmap-light-40x40", 40, 40, false, tabInsights, insightLatency, ""},
		{"controls-dark-40x20", 40, 20, true, tabControls, 0, ""},
		{"review-light-40x20", 40, 20, false, tabControls, 0, "review"},
		{"rates-dark-160x48", 160, 48, true, tabInsights, insightRates, ""},
		{"heatmap-light-160x48", 160, 48, false, tabInsights, insightLatency, ""},
		{"distribution-dark-120x40", 120, 40, true, tabInsights, insightDistribution, ""},
		{"capacity-light-80x24", 80, 24, false, tabInsights, insightCapacity, ""},
		{"comparison-dark-160x48", 160, 48, true, tabInsights, insightCompare, ""},
		{"requests-dark-40x20", 40, 20, true, tabRequests, 0, ""},
		{"evidence-dark-40x20", 40, 20, true, tabNetwork, 0, "evidence"},
	}
	if *dashboardRenderDir != "" {
		if err := os.MkdirAll(*dashboardRenderDir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := m
			v.width = c.w
			v.height = c.h
			v.themeDark = c.dark
			v.styles = newTheme(c.dark)
			v.tab = c.tab
			v.insights.view = c.view
			if c.view == insightCapacity {
				v.insights.all = false
				v.insights.result.Points = v.insights.result.ByProvider[0].Points
			}
			if c.mode == "review" {
				p := control.Preview{Token: "synthetic-render-only", Revision: 2, ExpiresAt: time.Date(2099, 1, 1, 0, 0, 30, 0, time.UTC), Command: control.Command{Action: control.Resize, Provider: "alpha", Pool: "default", Limit: 8, TTL: 10 * time.Minute}, Effect: "Resize default from 16 to 8.", Consequences: []string{"Existing leases and cooldown holds remain in force.", "New work waits while occupied slots exceed the new limit.", "After 10m, the current process baseline is restored unless superseded."}}
				v.controlPanel.preview = &p
				v.controlPanel.pressed = -1
			}
			if c.mode == "evidence" {
				v.openNetwork(v.networkFiltered[0])
				v.inspection.section = 1
				v.rebuildInspection()
			}
			assertFrameFits(t, v)
			if *dashboardRenderDir != "" {
				frame := v.View().Content
				if err := os.WriteFile(filepath.Join(*dashboardRenderDir, c.name+".ansi"), []byte(frame), 0600); err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(frame, "\n")
				for i := range lines {
					lines[i] = stripANSI(lines[i])
				}
				if err := os.WriteFile(filepath.Join(*dashboardRenderDir, c.name+".txt"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
