package tui

import (
	"context"
	"encoding/json"
	"image/color"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/rivo/uniseg"
)

func runtimeTestModel(t *testing.T, w, h int) (Model, *control.Controller, *queue.Limiter) {
	t.Helper()
	hub := observation.New(observation.Config{Providers: []string{"alpha", "beta"}, ManualTicks: true})
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	c := control.New()
	pool := queue.NewLimiterWithCooldown(4, 0)
	if e := c.RegisterProvider("alpha", map[string]*queue.Limiter{"default": pool}); e != nil {
		t.Fatal(e)
	}
	if e := c.RegisterProvider("beta", map[string]*queue.Limiter{"default": queue.NewLimiterWithCooldown(8, 0)}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.Close)
	m := NewModelForProviders([]ProviderMeta{{ID: "alpha", Name: "Alpha 東京", Concurrency: 4, Journal: journal.New(16, 1<<20)}, {ID: "beta", Name: "Beta", Concurrency: 8, Journal: journal.New(16, 1<<20)}}).WithRuntime(RuntimeOptions{Observations: hub, Controls: c, Theme: "dark", ExportDir: t.TempDir()})
	m.width = w
	m.height = h
	m.insights.snapshot = hub.Snapshot()
	return m, c, pool
}
func pressRune(m Model, r rune) (Model, tea.Cmd) {
	next, cmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	return next.(Model), cmd
}
func pressCode(m Model, code rune) (Model, tea.Cmd) {
	next, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return next.(Model), cmd
}
func runImmediate(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = runImmediate(t, m, c)
		}
		return m
	}
	next, _ := m.Update(msg)
	return next.(Model)
}
func readControlReview(m Model) Model {
	for m.controlPanel.page < m.reviewPages()-1 {
		m, _ = pressCode(m, tea.KeyRight)
	}
	return m
}
func assertFrameFits(t *testing.T, m Model) {
	t.Helper()
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) > m.height {
		t.Fatalf("%dx%d got %d rows", m.width, m.height, len(lines))
	}
	for i, s := range lines {
		s = stripANSI(s)
		if n := uniseg.StringWidth(s); n > m.width {
			t.Fatalf("%dx%d row %d has %d cells: %q", m.width, m.height, i, n, s)
		}
	}
}
func TestSmallPaneControlReviewActuallyChangesRuntime(t *testing.T) {
	for _, height := range []int{20, 40} {
		t.Run(time.Duration(height).String(), func(t *testing.T) {
			m, c, _ := runtimeTestModel(t, 40, height)
			m.tab = tabControls
			m, _ = pressRune(m, 'p')
			if m.controlPanel.preview == nil {
				t.Fatal("pause did not create preview")
			}
			if c.Snapshot().Providers[0].State != control.Running {
				t.Fatal("preview mutated runtime")
			}
			assertFrameFits(t, m)
			if m.reviewPages() > 1 {
				m, _ = pressCode(m, tea.KeyTab)
				if m.controlPanel.focus != 0 {
					t.Fatal("apply available before all effect text read")
				}
			}
			m, _ = pressCode(m, tea.KeyEnter)
			if m.controlPanel.preview != nil {
				t.Fatal("Cancel was not the default")
			}
			if c.Snapshot().Providers[0].State != control.Running {
				t.Fatal("Cancel changed runtime")
			}
			m, _ = pressRune(m, 'p')
			m = readControlReview(m)
			m, _ = pressCode(m, tea.KeyTab)
			if m.controlPanel.focus != 1 {
				t.Fatal("apply did not become focusable after reading")
			}
			var cmd tea.Cmd
			m, cmd = pressCode(m, tea.KeyEnter)
			m = runImmediate(t, m, cmd)
			if c.Snapshot().Providers[0].State != control.Paused {
				t.Fatal("reviewed pause was not applied")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if err := c.Gate(ctx, "alpha"); err != context.DeadlineExceeded {
				t.Fatalf("paused gate returned %v", err)
			}
			m, _ = pressRune(m, 'u')
			m = readControlReview(m)
			m, _ = pressCode(m, tea.KeyTab)
			m, cmd = pressCode(m, tea.KeyEnter)
			_ = runImmediate(t, m, cmd)
			if err := c.Gate(context.Background(), "alpha"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestSmallPaneResizeFormExpiryAndLeaseConservation(t *testing.T) {
	m, c, pool := runtimeTestModel(t, 40, 20)
	m.tab = tabControls
	releases := make([]func(), 3)
	for i := range releases {
		var err error
		releases[i], err = pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer releases[i]()
	}
	m, _ = pressRune(m, 'r')
	next, _ := m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = next.(Model)
	for _, r := range "2 60ms" {
		m, _ = pressRune(m, r)
	}
	m, _ = pressCode(m, tea.KeyEnter)
	if m.controlPanel.preview == nil {
		t.Fatal("valid form did not create review")
	}
	if pool.Limit() != 4 {
		t.Fatal("form or preview changed pool")
	}
	m = readControlReview(m)
	m, _ = pressCode(m, tea.KeyTab)
	m, cmd := pressCode(m, tea.KeyEnter)
	m = runImmediate(t, m, cmd)
	if pool.Limit() != 2 {
		t.Fatal("resize not applied")
	}
	s := c.Snapshot().Providers[0].Pools[0]
	if s.Occupied != 3 || s.Debt != 1 {
		t.Fatalf("resize lost leases: %+v", s)
	}
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for pool.Limit() != 4 {
		select {
		case <-deadline:
			t.Fatal("TTL failed to restore baseline")
		case <-ticker.C:
		}
	}
}
func TestReviewPointerRequiresMatchingPressAndSurvivesResizeSafely(t *testing.T) {
	m, c, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabControls
	m, _ = pressRune(m, 'd')
	m = readControlReview(m)
	y := m.contentStartRow() + m.visibleRows() - 1
	next, _ := m.Update(tea.MouseReleaseMsg{X: 16, Y: y, Button: tea.MouseLeft})
	m = next.(Model)
	if c.Snapshot().Providers[0].State != control.Running {
		t.Fatal("release without press applied")
	}
	next, _ = m.Update(tea.MouseClickMsg{X: 16, Y: y, Button: tea.MouseLeft})
	m = next.(Model)
	if m.controlPanel.pressed != 1 {
		t.Fatal("apply button was not armed")
	}
	m = update(m, tea.WindowSizeMsg{Width: 40, Height: 40})
	next, cmd := m.Update(tea.MouseReleaseMsg{X: 16, Y: y, Button: tea.MouseLeft})
	m = runImmediate(t, next.(Model), cmd)
	if c.Snapshot().Providers[0].State != control.Running {
		t.Fatal("resize retained a stale armed button")
	}
	m = readControlReview(m)
	y = m.contentStartRow() + m.visibleRows() - 1
	m = update(m, tea.MouseClickMsg{X: 16, Y: y, Button: tea.MouseLeft})
	m = update(m, tea.BlurMsg{})
	next, cmd = m.Update(tea.MouseReleaseMsg{X: 16, Y: y, Button: tea.MouseLeft})
	_ = runImmediate(t, next.(Model), cmd)
	if c.Snapshot().Providers[0].State != control.Running {
		t.Fatal("blur retained a stale armed button")
	}
}
func TestAbsoluteRangeAndFiltersAcceptPrintableQuitAndDigits(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabInsights
	m, _ = pressRune(m, 'r')
	m = update(m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	text := "2026-11-21T12:00:00Z 2026-11-21T13:00:00Z"
	for _, r := range text {
		m, _ = pressRune(m, r)
	}
	assertFrameFits(t, m)
	m, cmd := pressCode(m, tea.KeyEnter)
	if m.insights.inputMode || m.insights.span != time.Hour || m.insights.follow {
		t.Fatalf("absolute range not accepted: %+v", m.insights)
	}
	m = runImmediate(t, m, cmd)
	m.tab = tabRequests
	m, _ = pressRune(m, '/')
	for _, r := range "q123 provider:beta outcome:failed" {
		m, cmd = pressRune(m, r)
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("printable q quit in filter")
		}
	}
	if !strings.Contains(m.filterText, "q123") {
		t.Fatalf("filter lost text: %q", m.filterText)
	}
	q := m.factQuery()
	if q.Search != "q123" || q.Providers[0] != "beta" || q.Outcome != observation.OutcomeFailed {
		t.Fatalf("structured filter = %+v", q)
	}
	m = update(m, tea.WindowSizeMsg{Width: 160, Height: 48})
	if m.filterText != "q123 provider:beta outcome:failed" {
		t.Fatal("resize lost filter")
	}
	assertFrameFits(t, m)
}
func TestQueryGenerationKeepsAcceptedAxisUntilReplacement(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	a := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := a.Add(time.Minute)
	m.insights.result = observation.QueryResult{Query: observation.Query{From: a, To: b}}
	m.insights.follow = false
	m.insights.cursor = a.Add(30 * time.Second)
	m.insightZoom(.5)
	cmd := m.requestInsights()
	if cmd == nil {
		t.Fatal("query missing")
	}
	if start, end := m.insights.axis(); !start.Equal(a) || !end.Equal(b) {
		t.Fatal("pending query changed painted axis")
	}
	gen := m.insights.generation
	m = update(m, insightResultMsg{generation: gen - 1, result: observation.QueryResult{Query: observation.Query{From: a.Add(-time.Hour), To: b}}})
	if start, _ := m.insights.axis(); !start.Equal(a) {
		t.Fatal("stale query replaced accepted axis")
	}
	m = runImmediate(t, m, cmd)
	if m.insights.pending {
		t.Fatal("accepted response did not clear pending")
	}
	if start, end := m.insights.axis(); end.Sub(start) != 30*time.Second {
		t.Fatal("new axis not accepted")
	}
}
func TestBrushKeyboardAndMouseUseSameTimeGridAcrossResize(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.tab = tabInsights
	a := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := a.Add(time.Minute)
	m.insights.result = observation.QueryResult{Query: observation.Query{From: a, To: b}}
	m.insights.cursor = a
	m.insights.follow = false
	g := m.insightGeometry()
	x1, x2 := g.x+3, g.x+13
	y := g.y + 2
	m = update(m, tea.MouseClickMsg{X: x1, Y: y, Button: tea.MouseLeft})
	m = update(m, tea.MouseMotionMsg{X: x2, Y: y, Button: tea.MouseLeft})
	m = update(m, tea.MouseReleaseMsg{X: x2, Y: y, Button: tea.MouseLeft})
	first, last := m.insights.selectionRange()
	if !first.Equal(m.chartTime(x1)) || !last.Equal(m.chartTime(x2)) {
		t.Fatal("brush disagrees with painted time cells")
	}
	cursor := m.insights.cursor
	m = update(m, tea.WindowSizeMsg{Width: 160, Height: 48})
	a2, b2 := m.insights.selectionRange()
	if !a2.Equal(first) || !b2.Equal(last) || !m.insights.cursor.Equal(cursor) {
		t.Fatal("resize changed time selection")
	}
	m = update(m, tea.WindowSizeMsg{Width: 40, Height: 40})
	assertFrameFits(t, m)
}
func TestGapCursorDoesNotBorrowAdjacentObservation(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	a := time.Now().UTC()
	m.insights.result = observation.QueryResult{Query: observation.Query{From: a, To: a.Add(time.Minute)}, Points: []observation.Point{{Start: a, End: a.Add(10 * time.Second), Counts: observation.Counters{Terminal: 99}, Complete: true, ElapsedNS: int64(10 * time.Second)}}}
	m.insights.cursor = a.Add(20 * time.Second)
	if _, ok := m.cursorPoint(); ok {
		t.Fatal("gap borrowed a sample")
	}
	m.openPointInspection()
	if !strings.Contains(strings.Join(m.inspection.lines, " "), "No observations") {
		t.Fatal("gap inspection lacks explicit unknown")
	}
}
func TestHeatmapDrilldownRetainsWholeDrawnBinBounds(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.insights.view = insightLatency
	m.insights.fullHeatmap = true
	m.insights.metric = 2
	m.insights.bin = 2
	m.insights.cohort = true
	m.setDurationFilter()
	bins := m.heatmapBins()
	want := bins[2]
	q := m.factQuery()
	if q.DurationKind != "attempt" {
		t.Fatal(q.DurationKind)
	}
	if want.lo > 0 && (q.DurationMinNS == nil || *q.DurationMinNS != int64(binLower(want.lo)*float64(time.Second))) {
		t.Fatal("drilldown lost compressed lower bound")
	}
	if !math.IsInf(binUpper(want.hi), 1) && (q.DurationMaxNS == nil || *q.DurationMaxNS != int64(binUpper(want.hi)*float64(time.Second))) {
		t.Fatal("drilldown lost compressed upper bound")
	}
}
func TestExactRequestJoinRetainsEvidenceAndShowsEviction(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	u, _ := url.Parse("https://example.invalid/v1/messages")
	entry := &journal.Entry{RequestID: "wanted", URL: u, StatusCode: 400, Diagnostic: &diagnostic.Snapshot{Failures: []diagnostic.Failure{{Stage: "request_decode", Kind: "unknown_field", Path: "$.descendant.new_field", Fingerprint: "safe-group"}}}}
	m.providers[1].journal.Record(entry)
	m.providers[1].journal.Record(&journal.Entry{RequestID: "different", URL: u, StatusCode: 200})
	if !m.openJoinedNetwork("wanted") {
		t.Fatal("join failed")
	}
	if m.inspection.entry.RequestID != "wanted" || m.active != 1 {
		t.Fatal("join selected neighbor/provider")
	}
	m.inspection.section = 1
	m.rebuildInspection()
	if !strings.Contains(strings.Join(m.inspection.lines, " "), "safe-group") {
		t.Fatal("failure evidence missing")
	}
	assertFrameFits(t, m)
	if m.openJoinedNetwork("evicted") {
		t.Fatal("missing ID fell back to nearby exchange")
	}
	if !strings.Contains(m.statusText, "evicted") {
		t.Fatal("missing raw evidence was not explained")
	}
}
func TestRawExportConsentPagedReviewAndMetadataRedaction(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	u, _ := url.Parse("https://secret.invalid/path?token=secret-value")
	entry := &journal.Entry{RequestID: "id-1", Method: "POST", URL: u, RequestHeaders: http.Header{"Authorization": []string{"Bearer secret-value"}}, RequestBody: []byte(`{"prompt":"secret-value"}`), StatusCode: 400}
	m.journal.Record(entry)
	m.openNetwork(entry)
	cmd := m.beginExport("inspection")
	m = runImmediate(t, m, cmd)
	files, err := os.ReadDir(m.runtime.ExportDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("metadata file missing %v %v", files, err)
	}
	data, _ := os.ReadFile(m.runtime.ExportDir + "/" + files[0].Name())
	if strings.Contains(string(data), "secret-value") {
		t.Fatal("metadata leaked body/header/url")
	}
	m.openNetwork(entry)
	m.beginRawExport()
	for _, r := range "RAW" {
		m, _ = pressRune(m, r)
	}
	m, _ = pressCode(m, tea.KeyEnter)
	if !m.export.rawReview {
		t.Fatal("typed consent did not open review")
	}
	m, _ = pressCode(m, tea.KeyEnter)
	if m.export.rawReview {
		t.Fatal("raw review did not default to Cancel")
	}
	files, _ = os.ReadDir(m.runtime.ExportDir)
	if len(files) != 1 {
		t.Fatal("Cancel wrote raw file")
	}
	m.beginRawExport()
	for _, r := range "RAW" {
		m, _ = pressRune(m, r)
	}
	m, _ = pressCode(m, tea.KeyEnter)
	for m.export.page < m.rawReviewPages()-1 {
		m, _ = pressCode(m, tea.KeyRight)
	}
	assertFrameFits(t, m)
	m, _ = pressCode(m, tea.KeyTab)
	m, cmd = pressCode(m, tea.KeyEnter)
	_ = runImmediate(t, m, cmd)
	files, _ = os.ReadDir(m.runtime.ExportDir)
	if len(files) != 2 {
		t.Fatalf("raw file not created: %d", len(files))
	}
	found := false
	for _, f := range files {
		d, _ := os.ReadFile(m.runtime.ExportDir + "/" + f.Name())
		var envelope struct {
			Raw bool `json:"raw"`
		}
		_ = json.Unmarshal(d, &envelope)
		if envelope.Raw {
			found = true
			if !strings.Contains(string(d), "Bearer secret-value") {
				t.Fatal("explicit raw export lost retained headers")
			}
		}
	}
	if !found {
		t.Fatal("no raw export")
	}
}
func TestTerminalControlEvidenceIsVisibleButNeverExecutable(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m.inspection = inspectionState{open: true, title: "EVIDENCE", rawLines: []string{"x\x1b]52;c;c2VjcmV0\a\x1b[31my"}}
	m.reflowInspection()
	raw := strings.Join(m.inspection.lines, " ")
	if strings.Contains(raw, "\x1b") || !strings.Contains(raw, "\\u001b") {
		t.Fatalf("unsafe or silently discarded control bytes: %q", raw)
	}
	assertFrameFits(t, m)
}
func TestBothThemesKeepChromaticIdentityAndTextContrast(t *testing.T) {
	for _, dark := range []bool{true, false} {
		th := newTheme(dark)
		background := color.Color(lipgloss.Color("#FFFFFF"))
		if dark {
			background = lipgloss.Color("#0D1117")
		}
		styles := []lipgloss.Style{th.headerStyle, th.rowStyle, th.dimStyle2, th.sectionStyle, th.statusOkStyle, th.statusClientErrStyle, th.statusServerErrStyle, th.rowSelectedStyle, th.paletteSelectedStyle, th.paletteDimStyle}
		for i, s := range styles {
			bg := s.GetBackground()
			if _, empty := bg.(lipgloss.NoColor); empty || bg == nil {
				bg = background
			}
			if ratio := contrastRatio(s.GetForeground(), bg); ratio < 4.5 {
				t.Errorf("dark=%t style=%d contrast %.2f", dark, i, ratio)
			}
		}
		if hexString(th.statusOkStyle.GetForeground()) == hexString(th.sectionStyle.GetForeground()) || hexString(th.statusClientErrStyle.GetForeground()) == hexString(th.statusServerErrStyle.GetForeground()) {
			t.Fatal("semantic hues collapsed")
		}
	}
}
func TestExplicitThemeIgnoresLateTerminalBackgroundReply(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 40, 20)
	m = m.WithRuntime(RuntimeOptions{Observations: m.runtime.Observations, Controls: m.runtime.Controls, Theme: "light"})
	m = update(m, tea.BackgroundColorMsg{Color: color.Black})
	if m.themeDark || hexString(m.styles.rowStyle.GetForeground()) != "#24292F" {
		t.Fatal("explicit light theme was overridden")
	}
}
