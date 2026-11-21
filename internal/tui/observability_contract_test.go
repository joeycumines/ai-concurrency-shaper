package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func TestRuntimeReceivesObservationsBeforeInitialTerminalSize(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 0, 0)
	// Bubble Tea may deliver snapshots and log refreshes before WindowSizeMsg.
	// Previously compact Overview sliced at width-2 and killed the gateway.
	for _, size := range [][2]int{{0, 0}, {1, 1}, {2, 3}, {40, 20}} {
		m.width, m.height = size[0], size[1]
		m = update(m, ProviderUpdate{Index: 0, Snapshot: metrics.Snapshot{Sparkline: []int{1, 2, 3}}})
		_ = m.View()
		_ = m.cachedDashboardLines()
	}
}

func TestPendingRequestExportBindsAcceptedQueryAndRows(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabRequests
	accepted := observation.RequestQuery{Limit: 200, Offset: 200, Search: "accepted-query", Providers: []string{"alpha"}}
	facts := observation.RequestResult{Total: 201, Requests: []observation.RequestFact{{ID: "accepted-id", Provider: "alpha", Path: "/accepted"}}}
	m.insights.factGeneration = 9
	m = update(m, requestResultMsg{generation: 9, query: accepted, result: facts})
	m.filterText = "pending-query provider:beta"
	pending := m.requestFacts()
	defer m.cancelQueries()
	if pending == nil || !m.insights.factsPending {
		t.Fatal("replacement did not start")
	}
	if q := m.displayedFactQuery(); q.Search != "accepted-query" || q.Offset != 200 {
		t.Fatalf("pending scope replaced accepted: %+v", q)
	}
	rendered := stripANSI(m.renderRequestExplorer())
	if !strings.Contains(rendered, "rows retain previous scope") || !strings.Contains(rendered, "201–201 / 201") {
		t.Fatalf("pending rows mislabeled: %s", rendered)
	}
	cmd := m.beginExport("requests")
	if cmd == nil {
		t.Fatal("export unavailable")
	}
	// A later accepted result must not mutate the already frozen export.
	m = update(m, requestResultMsg{generation: m.insights.factGeneration, query: observation.RequestQuery{Search: "new", Limit: 200}, result: observation.RequestResult{Requests: []observation.RequestFact{{ID: "new-id"}}}})
	result := cmd().(exportResultMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	bytes, err := os.ReadFile(result.path)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Query  observation.RequestQuery  `json:"query"`
		Result observation.RequestResult `json:"result"`
	}
	if err := json.Unmarshal(bytes, &out); err != nil {
		t.Fatal(err)
	}
	if out.Query.Search != "accepted-query" || out.Query.Offset != 200 || len(out.Result.Requests) != 1 || out.Result.Requests[0].ID != "accepted-id" {
		t.Fatalf("export joined different generations: %s", bytes)
	}
}

func TestExportSavedReviewDistinguishesRawFromMetadata(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	metadata := update(m, exportResultMsg{path: "/tmp/meta.json"})
	raw := update(m, exportResultMsg{path: "/tmp/raw.json", raw: true})
	metaText := strings.ToUpper(strings.Join(metadata.inspection.lines, " ") + " " + metadata.inspection.title)
	rawText := strings.ToUpper(strings.Join(raw.inspection.lines, " ") + " " + raw.inspection.title)
	if !strings.Contains(metaText, "METADATA") || strings.Contains(metaText, "RAW") {
		t.Fatalf("metadata review mislabeled: %s", metaText)
	}
	if !strings.Contains(rawText, "RAW") || strings.Contains(rawText, "METADATA") {
		t.Fatalf("raw review mislabeled: %s", rawText)
	}
}

func TestAnnualRangeAndPresetsAreReachableInSmallPane(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabInsights
	m.insights.follow = false
	m.insights.span = 7 * 24 * time.Hour
	for _, span := range []time.Duration{30 * 24 * time.Hour, 90 * 24 * time.Hour, 365 * 24 * time.Hour, time.Minute} {
		m, _ = pressRune(m, 'w')
		if m.insights.span != span {
			t.Fatalf("window=%s want %s", m.insights.span, span)
		}
	}
	m, _ = pressRune(m, 'r')
	m = update(m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	for _, r := range "2025-01-01T00:00:00Z 2026-01-01T00:00:00Z" {
		m, _ = pressRune(m, r)
	}
	m, _ = pressCode(m, tea.KeyEnter)
	if m.insights.inputMode || m.insights.span != 365*24*time.Hour {
		t.Fatal("annual absolute range rejected")
	}
	m.insights.result = observation.QueryResult{}
	m.insightZoom(2)
	if m.insights.span != 366*24*time.Hour {
		t.Fatalf("maximum zoom=%s", m.insights.span)
	}
	m.cancelQueries()
	assertFrameFits(t, m)
}

func TestUsageKnownZeroAbsentCoverageAndMergedCounters(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.insights.view = insightUsage
	p := observation.Point{Complete: true, ElapsedNS: int64(time.Second), Counts: observation.Counters{UsageReports: 3, UsageInputKnown: 1, UsageInputTokens: 0, UsageCorrections: 2, UsageOverflow: 1, ConversionLosses: 7, ConversionNotes: 8, Upstream429: 9}}
	p.Counts.HTTP[1][4] = 4
	p.Counts.Failures[5] = 5
	if v, ok := m.seriesValue(p, 0); !ok || v != 0 {
		t.Fatalf("known zero=%g,%t", v, ok)
	}
	if _, ok := m.seriesValue(p, 1); ok {
		t.Fatal("absent usage plotted as zero")
	}
	m.insights.metric = 1
	if v, ok := m.seriesValue(p, 0); !ok || v < 33.33 || v > 33.34 {
		t.Fatalf("reported-field coverage=%g,%t", v, ok)
	}
	p.Complete = false
	if _, ok := m.seriesValue(p, 0); ok {
		t.Fatal("partial temporal coverage plotted confidently")
	}
	p.Complete = true
	merged := mergePoints([]observation.Point{p, p})
	expected := p.Counts
	expected.Add(p.Counts)
	if merged.Counts != expected {
		t.Fatalf("merged counters lost measures: got %+v want %+v", merged.Counts, expected)
	}
	m.insights.view = insightFidelity
	if v, ok := m.seriesValue(p, 3); !ok || v != 5 {
		t.Fatalf("conversion failures=%g,%t", v, ok)
	}
	m.insights.view = insightAttempts
	m.insights.metric = 1
	if v, ok := m.seriesValue(p, 1); !ok || v != 9 {
		t.Fatalf("upstream 429=%g,%t", v, ok)
	}
}

func TestCapacityMixedUnknownCoverageStaysUnknown(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.insights.view = insightCapacity
	m.insights.all = false
	p := observation.Point{Complete: true, ElapsedNS: int64(10 * time.Second), Resources: []observation.ResourceIntegral{{ID: "default", Limit: observation.Gauge{Known: true, DurationNS: int64(5 * time.Second), Area: 20 * float64(time.Second)}, Occupied: observation.Gauge{Known: true, DurationNS: int64(10 * time.Second), Area: 10 * float64(time.Second)}}}}
	if _, ok := m.seriesValue(p, 0); ok {
		t.Fatal("mixed unknown capacity shown as full-window occupancy")
	}
	p.Resources[0].Limit.DurationNS = p.ElapsedNS
	if v, ok := m.seriesValue(p, 0); !ok || v != 1 {
		t.Fatalf("fully covered occupancy=%g,%t", v, ok)
	}
	m.insights.all = true
	if _, ok := m.seriesValue(p, 2); ok {
		t.Fatal("invented fleet capacity")
	}
}

func TestJournalBodiesSurviveMetadataOnlyDiagnostics(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	e := &journal.Entry{RequestID: "native", RequestBody: []byte(`{"native_request":"retained"}`), ResponseBody: []byte(`{"native_response":"retained"}`), Diagnostic: &diagnostic.Snapshot{CaptureBudget: 0}}
	m.openNetwork(e)
	raw := strings.Join(m.inspection.rawLines, "\n")
	for _, needle := range []string{"native_request", "native_response", "provider wire bytes", "JOURNAL WIRE BUFFERS"} {
		if !strings.Contains(raw, needle) {
			t.Fatalf("retained evidence lost %q: %s", needle, raw)
		}
	}
	// All text is scrollable at forty columns, and remains available after resize.
	m, _ = pressCode(m, tea.KeyEnd)
	assertFrameFits(t, m)
	m = update(m, tea.WindowSizeMsg{Width: 160, Height: 48})
	if !strings.Contains(strings.Join(m.inspection.lines, " "), "native_response") {
		t.Fatal("resize lost retained body")
	}
}

func TestIngressEvidenceJoinsAndFleetNetworkRemainSeparateFromProviders(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.runtime.IngressJournal = journal.New(4, 1024)
	e := &journal.Entry{RequestID: "unmatched-id", StatusCode: 404}
	m.runtime.IngressJournal.Record(e)
	m.providers[0].journal.Record(&journal.Entry{RequestID: "provider-id", StatusCode: 200})
	providers := len(m.providers)
	if !m.openJoinedNetwork("unmatched-id") || m.inspection.entry != e || m.active != 0 || len(m.providers) != providers {
		t.Fatal("ingress exact join mutated provider scope")
	}
	m.inspection.open = false
	m.tab = tabNetwork
	m, _ = pressRune(m, 'a')
	if len(m.networkFiltered) != 2 || m.networkSources[e] != "ingress" {
		t.Fatal("fleet network omitted labeled ingress")
	}
	m.filterText = "unmatched-id"
	m.networkFiltered = m.computeVisibleNetworkEntries()
	if len(m.networkFiltered) != 1 || m.networkFiltered[0] != e {
		t.Fatal("ingress filtering lost stable ID")
	}
	assertFrameFits(t, m)
}

func TestAllInteractiveViewsLegendGeometryAndLongComparison(t *testing.T) {
	m := dashboardFixture(t)
	m.width = 40
	m.height = 20
	m.tab = tabInsights
	for view := range insightNames {
		m.insights.view = view
		m.insights.series = 0
		m.insights.hidden = [4]bool{}
		if view == insightLatency || view == insightDistribution || view == insightCompare {
			continue
		}
		x := 1
		for i, n := range m.legendNames() {
			if n == "" {
				continue
			}
			m = update(m, tea.MouseClickMsg{X: x, Y: m.contentStartRow() + 4, Button: tea.MouseLeft})
			if !m.insights.hidden[i] || m.insights.series != i {
				t.Fatalf("view %d legend %d hit mismatch", view, i)
			}
			x += unstyledWidth("●" + n + " ")
		}
	}
	m.insights.view = insightCompare
	m.insights.series = 37
	m, _ = pressRune(m, ' ')
	m, _ = pressRune(m, 't')
	if m.insights.series != 0 {
		t.Fatal("view transition retained out-of-range provider index")
	}
	m.insights.view = insightDistribution
	m.insights.bin = 8
	m, _ = pressCode(m, tea.KeyEnter)
	if m.inspection.title != "DURATION DISTRIBUTION" || !strings.Contains(strings.Join(m.inspection.rawLines, " "), "observations in bin") {
		t.Fatal("distribution does not inspect selected histogram")
	}
}

func TestCapacitySeriesFollowsPoolIdentityAcrossIntervalReordering(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.insights.view = insightCapacity
	m.insights.all = false
	resource := func(id string, n int64) observation.ResourceIntegral {
		return observation.ResourceIntegral{Provider: "alpha", Kind: "route", ID: id, Limit: observation.Gauge{Known: true, DurationNS: int64(time.Second), Area: 4 * float64(time.Second)}, Occupied: observation.Gauge{Known: true, DurationNS: int64(time.Second), Area: float64(n) * float64(time.Second)}}
	}
	p := observation.Point{Complete: true, ElapsedNS: int64(time.Second), Resources: []observation.ResourceIntegral{resource("a", 1), resource("b", 3)}}
	q := p
	q.Resources = []observation.ResourceIntegral{resource("b", 3), resource("a", 1)}
	m.acceptResources(observation.QueryResult{Points: []observation.Point{p, q}})
	for _, point := range []observation.Point{p, q} {
		if v, ok := m.seriesValue(point, 0); !ok || v != 1 {
			t.Fatalf("resource changed identity: %g,%t", v, ok)
		}
	}
	q.Resources = q.Resources[:1]
	if _, ok := m.seriesValue(q, 0); ok {
		t.Fatal("missing selected pool substituted a neighbor")
	}
}

func TestModalTypingPasteAndMouseCommandsNeverTriggerHiddenControls(t *testing.T) {
	m, c, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabControls
	m = update(m, keyForAction("ctrl+k"))
	for _, r := range "pause" {
		m, _ = pressRune(m, r)
	}
	if m.mode != modePalette || m.controlPanel.preview != nil {
		t.Fatal("typing into palette activated hidden controls")
	}
	m = update(m, tea.PasteMsg{Content: " with expiry"})
	if m.palette.query != "pause with expiry" {
		t.Fatalf("paste=%q", m.palette.query)
	}
	if len(m.palette.cmds) != 1 {
		t.Fatalf("expiry command not discoverable: %+v", m.palette.cmds)
	}
	m = update(m, tea.MouseClickMsg{X: 3, Y: m.contentStartRow() + 2, Button: tea.MouseLeft})
	if !m.controlPanel.form || m.controlPanel.command.Action != "pause" {
		t.Fatal("palette click did not open the requested review form")
	}
	m = update(m, keyForAction("ctrl+u"))
	m = update(m, tea.PasteMsg{Content: "10m\x1b[31m"})
	if strings.Contains(m.controlPanel.input, "\x1b") {
		t.Fatal("paste retained executable controls")
	}
	if c.Snapshot().Providers[0].State != "running" {
		t.Fatal("form changed runtime")
	}
	m = update(m, keyForAction("esc"))
	m.tab = tabInsights
	m = update(m, tea.MouseClickMsg{X: 12, Y: m.contentStartRow() + 2, Button: tea.MouseLeft})
	if !m.insights.inputMode {
		t.Fatal("date row did not open range editor")
	}
	m = update(m, keyForAction("ctrl+u"))
	m = update(m, tea.PasteMsg{Content: "2026-01-01T00:00:00Z\n2026-02-01T00:00:00Z"})
	m = update(m, keyForAction("enter"))
	if m.insights.inputMode || m.insights.span != 31*24*time.Hour {
		t.Fatal("pasted absolute range was not accepted")
	}
	m.cancelQueries()
}

func TestRuntimeProviderAndFooterMouseGeometryMatchesRenderedCells(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 160, 48)
	for _, w := range []int{40, 80, 100, 120, 160} {
		m.width = w
		_, chips, _ := m.runtimeHeaderParts()
		for _, chip := range chips {
			for x := chip.x; x < chip.x+chip.width; x++ {
				id, ok := m.runtimeProviderAt(x)
				if !ok || id != chip.index {
					t.Fatalf("%d cols x%d hit %d %t", w, x, id, ok)
				}
			}
		}
		if _, ok := m.runtimeProviderAt(w - 1); ok {
			t.Fatalf("%d cols status padding changed provider", w)
		}
		assertFrameFits(t, m)
	}
	m.width = 40
	m.height = 20
	m.tab = tabControls
	actions := m.runtimeFooterActions()
	x := 1
	for _, action := range actions {
		for cell := x; cell < min(x+unstyledWidth(action.label), m.width); cell++ {
			key, ok := m.footerActionAt(cell)
			if !ok || key != action.key {
				t.Fatalf("footer label %s hit %q,%t", action.label, key, ok)
			}
		}
		x += unstyledWidth(action.label) + 3
	}
	m = update(m, tea.MouseClickMsg{X: 2, Y: 19, Button: tea.MouseLeft})
	if m.controlPanel.preview == nil || m.controlPanel.preview.Command.Action != "pause" {
		t.Fatal("mouse footer pause did not open review")
	}
	if m.controlPanel.focus != 0 {
		t.Fatal("mouse preview did not default to Cancel")
	}
}

func TestGenericInspectionExportRemainsBoundToInspectedEvidence(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.inspection = inspectionState{open: true, title: "LOG MESSAGE", rawLines: []string{"the exact pinned log message"}}
	m.insights.result = observation.QueryResult{Points: []observation.Point{{Counts: observation.Counters{Terminal: 987}}}}
	cmd := m.beginExport("inspection")
	if cmd == nil {
		t.Fatal("log inspection export missing")
	}
	result := cmd().(exportResultMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	bytes, err := os.ReadFile(result.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bytes), "exact pinned log message") || strings.Contains(string(bytes), "987") {
		t.Fatalf("inspection exported unrelated chart cell: %s", bytes)
	}
}

func TestHeatmapResizePreservesSelectedDurationCohort(t *testing.T) {
	m := dashboardFixture(t)
	m.width = 40
	m.height = 20
	m.tab = tabInsights
	m.insights.view = insightLatency
	m.insights.fullHeatmap = true
	m.insights.bin = 2
	m.insights.cohort = true
	m.setDurationFilter()
	before := m.factQuery()
	selected := m.selectedHeatmapRange()
	for _, size := range [][2]int{{160, 48}, {40, 40}, {40, 20}} {
		m = update(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if m.selectedHeatmapRange() != selected {
			t.Fatalf("%v changed selected underlying bins", size)
		}
		m.setDurationFilter()
		after := m.factQuery()
		if *after.DurationMinNS != *before.DurationMinNS || *after.DurationMaxNS != *before.DurationMaxNS {
			t.Fatalf("%v changed cohort bounds", size)
		}
		assertFrameFits(t, m)
	}
	m.cancelQueries()
}

func TestComparisonMouseUsesVisibleProviderOffsetAndBrush(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabInsights
	m.insights.view = insightCompare
	a := time.Now().Add(-time.Minute)
	b := a.Add(time.Minute)
	m.insights.result.Query = observation.Query{From: a, To: b}
	for i := range 40 {
		m.insights.result.ByProvider = append(m.insights.result.ByProvider, observation.ProviderSeries{Provider: strings.Repeat("p", i+1), Points: []observation.Point{{Start: a, End: a.Add(30 * time.Second), Counts: observation.Counters{Terminal: 2}}, {Start: a.Add(30 * time.Second), End: b, Counts: observation.Counters{Terminal: 7}}}})
	}
	m.insights.series = 30
	start := m.comparisonStart()
	g := m.insightGeometry()
	m = update(m, tea.MouseClickMsg{X: g.x + 1, Y: g.y + 1, Button: tea.MouseLeft})
	if m.insights.series != start+1 {
		t.Fatalf("clicked provider=%d want %d", m.insights.series, start+1)
	}
	m.insights.brushStart = a
	m.insights.brushEnd = a.Add(30 * time.Second)
	if got := m.comparisonSummary(m.insights.result.ByProvider[0].Points).Counts.Terminal; got != 2 {
		t.Fatalf("brush count=%d", got)
	}
}
func TestAttemptCohortLabelsItsLogicalTimeBasisAndKeepsRowHitsAligned(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabRequests
	m.insights.acceptedFactQuery = observation.RequestQuery{Limit: 200, DurationKind: "attempt"}
	m.insights.facts.Requests = []observation.RequestFact{{ID: "first"}, {ID: "second"}}
	m.insights.facts.Total = 2
	if !strings.Contains(stripANSI(m.renderRequestExplorer()), "attempt: logical finish; live start") {
		t.Fatal("attempt cohort pretends to be an attempt-time exemplar query")
	}
	m = update(m, tea.MouseClickMsg{X: 2, Y: m.contentStartRow() + m.requestHeaderRows() + 2, Button: tea.MouseLeft})
	if f, ok := m.selectedFact(); !ok || f.ID != "second" {
		t.Fatal("scope-note row shifted request hit geometry")
	}
	assertFrameFits(t, m)
}
