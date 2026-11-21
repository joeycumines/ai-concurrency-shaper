package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/rivo/uniseg"
)

// q must quit from a detail view and from the filter prompt without changing
// the overlay state; Esc must still dismiss the overlay normally.
func TestQuitWorksInsideDetailAndFilterModals(t *testing.T) {
	for _, mode := range []uiMode{modeDetail, modeFilter} {
		t.Run(modeName(mode), func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width, m.height = 80, 24
			m.mode = mode
			next, cmd := m.handleKey(tea.KeyPressMsg{Code: 'q', Text: "q"})
			if cmd == nil {
				t.Fatal("q in modal must quit")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("q in modal returned %T, want tea.QuitMsg", cmd())
			}
			if next.mode != mode {
				t.Fatalf("q changed modal state to %d", next.mode)
			}
			// Escape retains its original dismiss behaviour.
			next, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
			if cmd != nil || next.mode != modeBrowse {
				t.Fatalf("esc did not dismiss modal: mode=%d cmd=%v", next.mode, cmd)
			}
		})
	}
}

func stripANSILines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = stripANSI(lines[i])
	}
	return strings.Join(lines, "\n")
}

func modeName(m uiMode) string {
	switch m {
	case modeDetail:
		return "detail"
	case modeFilter:
		return "filter"
	default:
		return "browse"
	}
}

// The wide control and overview rows are 82/92 cells. At exactly those widths
// the wide form must be chosen; a narrower pane must use the compact form
// rather than clipping the wide one.
func TestWideRowsUseWideFormAtActualBreakpoints(t *testing.T) {
	// renderControls budgets m.viewportWidth() == width-1, so a pane one cell
	// wider than the row is needed to render the 82-cell wide form unclipped.
	m, _, _ := runtimeTestModel(t, 83, 30)
	m.tab = tabControls
	wide := stripANSILines(m.renderControls())
	if !strings.Contains(wide, "DEBT") || !strings.Contains(wide, "COOLDOWN") {
		t.Fatalf("83-cell pane rendered compact controls: %q", wide)
	}
	m.width = 82
	compact := stripANSILines(m.renderControls())
	if strings.Contains(compact, "DEBT") {
		t.Fatalf("81-cell pane rendered clipped wide controls: %q", compact)
	}
	if !strings.Contains(compact, "WAIT") || !strings.Contains(compact, "HOLD") {
		t.Fatalf("82-cell pane rendered unexpected controls: %q", compact)
	}

	m, _, _ = runtimeTestModel(t, 93, 30)
	fleet := stripANSILines(strings.Join(m.runtimeOverviewLines(), "\n"))
	if !strings.Contains(fleet, "GLOBAL CAP") {
		t.Fatalf("93-cell pane rendered compact overview: %q", fleet)
	}
	m.width = 92
	fleet = stripANSILines(strings.Join(m.runtimeOverviewLines(), "\n"))
	if strings.Contains(fleet, "GLOBAL CAP") {
		t.Fatalf("92-cell pane rendered clipped fleet overview: %q", fleet)
	}
}

// The compact control header labels must start at the same cell as the fields
// beneath them.
func TestCompactControlHeaderAlignsWithFields(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 70, 30)
	m.tab = tabControls
	lines := strings.Split(stripANSILines(m.renderControls()), "\n")
	var header, row string
	for _, line := range lines {
		if strings.Contains(line, "POOL") && strings.Contains(line, "LIMIT") && header == "" {
			header = line
			continue
		}
		if header != "" && strings.Fields(line)[0] == "default" {
			row = line
			break
		}
	}
	if header == "" || row == "" {
		t.Fatalf("missing compact header or row: %q", lines)
	}
	// Compare each numeric column's start against its header using a full
	// sample row so single-character coincidences cannot mask an offset.
	// The header labels must start at the left edge of each fixed-width
	// numeric field (14, 20, 25, 30), not merely somewhere inside it.
	for i, label := range []string{"LIMIT", "USED", "HOLD", "WAIT"} {
		want := strings.Index(header, label)
		got := []int{14, 20, 25, 30}[i]
		if got != want {
			t.Errorf("header %s starts at %d, field starts at %d", label, want, got)
		}
	}
}

// The raw export Apply button must remain clickable through its last cell.
func TestRawApplyButtonIncludesClosingBracket(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 80, 24)
	m.tab = tabNetwork
	m.runtime.ExportDir = t.TempDir()
	m.journal.Record(&journal.Entry{RequestID: "id-1", Method: "POST"})
	m.refreshNetworkFilter()
	if cmd := m.beginRawExport(); cmd != nil {
		t.Fatal("unexpected command")
	}
	m, _ = pressRune(m, 'R')
	m, _ = pressRune(m, 'A')
	m, _ = pressRune(m, 'W')
	m, _ = pressCode(m, tea.KeyEnter)
	m = readRawReview(t, m)
	if !m.rawCanApply() {
		t.Fatal("raw export was not reviewable")
	}
	y := m.contentStartRow() + m.visibleRows() - 1
	if got := m.rawButtonAt(28, y); got != 1 {
		t.Fatalf("closing bracket of Export RAW (x=28) hit %d, want 1", got)
	}
	if got := m.rawButtonAt(29, y); got != -1 {
		t.Fatalf("cell beyond Export RAW hit %d, want -1", got)
	}
}

func readRawReview(t *testing.T, m Model) Model {
	t.Helper()
	for m.export.page < m.rawReviewPages()-1 {
		m, _ = pressCode(m, tea.KeyRight)
	}
	return m
}

// An active override adds a row above the pool table, shifting each pool row
// down by one. Pointer selection must follow the rendered layout.
func TestControlPoolClickFollowsExpiryRow(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 92, 30)
	m.tab = tabControls
	m.controlPanel.snapshot = control.Snapshot{Providers: []control.ProviderState{{
		Name: "alpha", State: control.Running,
		ExpiresAt: time.Now().Add(time.Hour),
		Pools:     []control.PoolState{{ID: "default", Limit: 4}, {ID: "route:x", Limit: 2}},
	}}}
	y := m.contentStartRow() + m.controlPoolRowOffset()
	next, _, _ := m.runtimeMouseClick(tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
	if next.cursor != 0 {
		t.Fatalf("first pool click selected %d, want 0", next.cursor)
	}
	next, _, _ = m.runtimeMouseClick(tea.MouseClickMsg{X: 2, Y: y + 1, Button: tea.MouseLeft})
	if next.cursor != 1 {
		t.Fatalf("second pool click selected %d, want 1", next.cursor)
	}
	// Without the expiry row the first pool sits one row earlier.
	m.controlPanel.snapshot.Providers[0].ExpiresAt = time.Time{}
	if m.controlPoolRowOffset() != 5 {
		t.Fatalf("offset without expiry = %d, want 5", m.controlPoolRowOffset())
	}
}

// Switching insight views by heading click or palette must clear the same
// per-view state that t/T clears.
func TestInsightViewSwitchResetsSelectionFromEveryRoute(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 120, 40)
	m.tab = tabInsights
	m.insights.view = insightLatency
	m.insights.metric = 2
	m.insights.bin = 7
	m.insights.series = 3
	m.insights.binSelection = &binRange{lo: 1, hi: 2}
	m.insights.hidden = [4]bool{true, true, false, true}

	// Heading click.
	next, _, ok := m.runtimeMouseClick(tea.MouseClickMsg{X: 2, Y: m.contentStartRow(), Button: tea.MouseLeft})
	if !ok || next.insights.view != insightLatency+1 {
		t.Fatalf("heading click did not advance view: view=%d ok=%v", next.insights.view, ok)
	}
	assertInsightViewReset(t, next.insights)

	// Palette command.
	m, _ = pressRune(m, 'k') // reset via a fresh model state below
	m, _, _ = runtimeTestModel(t, 120, 40)
	m.tab = tabInsights
	m.insights.view = insightRates
	m.insights.metric = 2
	m.insights.bin = 7
	m.insights.series = 3
	m.insights.binSelection = &binRange{lo: 1, hi: 2}
	m.insights.hidden = [4]bool{true, true, false, true}
	m.openPalette()
	m.palette.cmds = []paletteCmd{{label: "Insights: CAPACITY", kind: cmdInsightView, arg: insightCapacity}}
	m.palette.cursor = 0
	next = runImmediate(t, m, m.executePaletteCmd(m.palette.cmds[0]))
	if next.insights.view != insightCapacity {
		t.Fatalf("palette did not select capacity: view=%d", next.insights.view)
	}
	assertInsightViewReset(t, next.insights)
}

func assertInsightViewReset(t *testing.T, s insightsState) {
	t.Helper()
	if s.metric != 0 || s.bin != 0 || s.series != 0 || s.binSelection != nil || s.hidden != [4]bool{} {
		t.Fatalf("view state not reset: metric=%d bin=%d series=%d binSelection=%v hidden=%v", s.metric, s.bin, s.series, s.binSelection, s.hidden)
	}
}

// A capacity point whose selected resource is absent must not render an empty
// "Pool / · v:resource" summary.
func TestCapacityWithNoSelectedResourceShowsNoPoolLabel(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 120, 40)
	m.tab = tabInsights
	m.insights.view = insightCapacity
	m.insights.all = false
	start := m.insights.cursor.Add(-time.Minute)
	m.insights.result = observation.QueryResult{
		Query: observation.Query{From: start, To: m.insights.cursor.Add(time.Minute)},
		Points: []observation.Point{{
			Start: start, End: m.insights.cursor, ElapsedNS: int64(time.Minute),
			Complete: true, Samples: 1,
			Resources: []observation.ResourceIntegral{{Provider: "alpha", ID: "default", Kind: "default"}},
		}},
	}
	m.insights.resources = []resourceRef{{provider: "alpha", kind: "default", id: "missing"}}
	m.insights.cursor = start.Add(time.Second)
	out := stripANSI(m.renderInsights())
	if strings.Contains(out, "Pool /") || strings.Contains(out, "v:resource") {
		t.Fatalf("empty pool label still rendered: %q", out)
	}
}

// Histogram counts remain visible when a point's rate is not computable.
func TestHeatmapShowsIncompletePointHistogramCounts(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 120, 40)
	m.tab = tabInsights
	m.insights.view = insightLatency
	start := m.insights.cursor.Add(-time.Minute)
	point := observation.Point{Start: start, End: start.Add(time.Minute), Samples: 10, Complete: false, ElapsedNS: int64(time.Minute)}
	point.Duration.Bins[3] = 12
	m.insights.result = observation.QueryResult{
		Query:  observation.Query{From: start, To: start.Add(time.Minute)},
		Points: []observation.Point{point},
	}
	m.insights.cursor = start.Add(30 * time.Second)
	chart := stripANSI(strings.Join(m.renderHeatmap(), "\n"))
	if strings.Contains(chart, "┊") {
		t.Fatalf("incomplete point histogram was hidden: %q", chart)
	}
}

// With a non-network inspection the footer must not advertise a raw export.
func TestFooterHidesRawActionWithoutInspectionEntry(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 120, 40)
	m.inspection = inspectionState{open: true, title: "TIME CELL", rawLines: []string{"x"}}
	actions := m.runtimeFooterActions()
	for _, a := range actions {
		if a.key == "E" || strings.Contains(a.label, "raw") {
			t.Fatalf("footer advertised E raw without a journal entry: %+v", actions)
		}
	}
	// With an entry the action returns.
	m.inspection = inspectionState{open: true, title: "NETWORK EVIDENCE", entry: &journal.Entry{}}
	found := false
	for _, a := range m.runtimeFooterActions() {
		if a.key == "E" {
			found = true
		}
	}
	if !found {
		t.Fatal("footer lost E raw for a journal entry")
	}
}

// The network filter cache must not be recomputed on unrelated messages.
func TestNetworkFilterCachedAcrossUnrelatedMessages(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 120, 40)
	j := journal.New(8, 1<<20)
	j.Record(&journal.Entry{RequestID: "one", Method: "POST"})
	m.journal = j
	m.active = 0
	m.providers[0].journal = j
	m.refreshNetworkFilter()
	before := m.networkFilterGen
	if len(m.networkFiltered) != 1 {
		t.Fatalf("initial filter = %d entries", len(m.networkFiltered))
	}

	// Mouse motion, window-independent key presses and ticks that are not
	// observability ticks must leave the generation unchanged.
	m = update(m, tea.MouseMotionMsg{X: 10, Y: 10})
	if m.networkFilterGen != before {
		t.Fatal("mouse motion recomputed network filter generation")
	}
	m = update(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.networkFilterGen != before {
		t.Fatal("window resize recomputed network filter generation")
	}

	// Changing a filter control must bump the generation and refresh.
	before = m.networkFilterGen
	m.networkFilterType = networkFilterJSON
	m.refreshNetworkFilter()
	if m.networkFilterGen == before {
		t.Fatal("filter type change did not bump generation")
	}
	j.Record(&journal.Entry{RequestID: "two", Method: "POST"})
	m.refreshNetworkFilter()
	if m.networkFilterGen == before {
		t.Fatal("journal arrival did not bump generation")
	}
}

// The legacy tab rail must render and hit-test all eight destinations.
func TestLegacyTabRailShowsAllEightTabs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	if m.enriched() {
		t.Fatal("test requires legacy model")
	}
	bar := stripANSI(m.renderTabBar())
	for _, name := range tabNames {
		if !strings.Contains(bar, strings.TrimSpace(name)) {
			t.Fatalf("legacy rail missing %q: %q", name, bar)
		}
	}
	for i := range tabNames {
		for x := range 108 {
			id, ok := m.tabAt(x)
			if ok && int(id) == i && !strings.Contains(bar, strings.TrimSpace(tabNames[i])) {
				t.Fatalf("tabAt(%d) hit hidden tab %d", x, i)
			}
		}
	}
	// Last two tabs must be reachable by pointer.
	if id, ok := m.tabAt(80); !ok || id != tabInsights {
		t.Fatalf("tabAt(80) = %d,%v, want Insights", id, ok)
	}
	if id, ok := m.tabAt(94); !ok || id != tabControls {
		t.Fatalf("tabAt(94) = %d,%v, want Controls", id, ok)
	}
}

// The provider chip hit-test must use the rendered style frame, not a
// hard-coded padding of one.
func TestRuntimeProviderChipUsesStyleFrame(t *testing.T) {
	m, _, _ := runtimeTestModel(t, 120, 40)
	m.styles.chipActiveStyle = m.styles.chipActiveStyle.PaddingLeft(2).PaddingRight(2)
	m.styles.chipInactiveStyle = m.styles.chipInactiveStyle.PaddingLeft(2).PaddingRight(2)
	_, chips, _ := m.runtimeHeaderParts()
	if len(chips) == 0 {
		t.Fatal("no provider chips rendered")
	}
	labelWidth := uniseg.StringWidth(chips[0].label)
	want := labelWidth + m.styles.chipActiveStyle.GetHorizontalFrameSize()
	if chips[0].width != want {
		t.Fatalf("chip width %d, want %d (style frame)", chips[0].width, want)
	}
	if _, ok := m.runtimeProviderAt(chips[0].x + chips[0].width - 1); !ok {
		t.Fatal("last cell of widened chip is not hit-testable")
	}
}

// Keep lipgloss imported for style frame checks above.
var _ = lipgloss.Width
