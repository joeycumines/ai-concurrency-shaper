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

package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

func update(m Model, msg tea.Msg) Model {
	m2, _ := m.Update(msg)
	return m2.(Model)
}

// helper: send a key by rune. Sets both Code and Text to properly
// simulate real terminal input where Key.Text is populated for
// printable characters.

func key(r rune) tea.Msg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// helper: send a special key by string (e.g. "enter", "esc", "down", "up").
// Caveat: this creates a KeyPressMsg with Text=k, which differs from real
// terminal events where special keys have Code=KeyXxx and Text="". This
// works because handleKey's switch statements match on msg.String(), which
// returns the same value for both representations. However, if a special
// key is NOT matched in a switch and falls through to the default case,
// this helper would incorrectly simulate a printable key (non-empty Text).
// For testing non-printable key rejection, use KeyPressMsg{Code: tea.KeyUp}
// directly (see TestFilterModeArrowKeysIgnored).

func special(k string) tea.Msg {
	return tea.KeyPressMsg{Text: k}
}

// ringTexts extracts the text of a ring snapshot for assertions that only
// care about line content.

func TestDetailOverlayRequests(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{
			Time:     time.Date(2026, 6, 8, 12, 30, 45, 0, time.UTC),
			Method:   "POST",
			Path:     "/v1/messages",
			Status:   200,
			Duration: 150 * time.Millisecond,
			Limited:  true,
		},
	}

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}

	v := m.View()
	text := stripANSI(v.Content)

	checks := []struct {
		field string
		want  string
	}{
		{"header", "Request Detail"},
		{"method label", "Method:"},
		{"method value", "POST"},
		{"path label", "Path:"},
		{"path value", "/v1/messages"},
		{"status label", "Status:"},
		{"status value", "200"},
		{"duration label", "Duration:"},
		{"duration value", "150ms"},
		{"limited label", "Limited:"},
		{"limited value", "true"},
		{"close hint", "close"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("overlay missing %s: want %q", c.field, c.want)
		}
	}

	// Dismiss with Escape
	m = update(m, special("esc"))
	if m.mode != modeBrowse {
		t.Fatal("Escape should return to browse mode")
	}
	v2 := m.View()
	text2 := stripANSI(v2.Content)
	if strings.Contains(text2, "Request Detail") {
		t.Error("overlay should not be visible after Escape")
	}

	// Dismiss with Enter
	m = update(m, special("enter"))
	m = update(m, special("enter"))
	if m.mode != modeBrowse {
		t.Fatal("Enter should dismiss detail mode")
	}

	// Dismiss with Space
	m = update(m, special("enter"))
	m = update(m, key(' '))
	if m.mode != modeBrowse {
		t.Fatal("Space should dismiss detail mode")
	}
}

func TestDetailOverlayConcurrency(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{
		{ID: 42, Method: "POST", Path: "/v1/messages", Limited: true},
	}

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}

	v := m.View()
	text := stripANSI(v.Content)

	checks := []struct {
		field string
		want  string
	}{
		{"header", "In-Flight Detail"},
		{"id label", "ID:"},
		{"id value", "42"},
		{"method label", "Method:"},
		{"method value", "POST"},
		{"path label", "Path:"},
		{"path value", "/v1/messages"},
		{"limited label", "Limited:"},
		{"limited value", "true"},
		{"age label", "Age:"},
		{"total label", "Total:"},
		{"close hint", "close"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("overlay missing %s: want %q", c.field, c.want)
		}
	}

	m = update(m, special("esc"))
	if m.mode != modeBrowse {
		t.Fatal("Escape should return to browse mode")
	}
}

func TestSnapshotUpdate(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	snap := metrics.NewCollector().Snapshot()
	snap.Active = 3
	snap.Queued = 5
	snap.Throughput = 42.5
	m = update(m, snap)

	if m.snap.Active != 3 {
		t.Errorf("Active = %d, want 3", m.snap.Active)
	}
	if m.snap.Queued != 5 {
		t.Errorf("Queued = %d, want 5", m.snap.Queued)
	}
}

func TestWindowSize(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m = update(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 {
		t.Errorf("size = %dx%d, want 120x40", m.width, m.height)
	}
}

func TestQuit(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	_, cmd := m.Update(key('q'))
	if cmd == nil {
		t.Fatal("quit should return a command")
	}
	// The cmd should produce a QuitMsg
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected QuitMsg, got %T", msg)
	}
}

func TestInitStartsResyncLoop(t *testing.T) {
	if cmd := NewModelForProviders([]ProviderMeta{{Concurrency: 4}}).Init(); cmd == nil {
		t.Fatal("Init should start the periodic resync loop")
	}
}

func TestUpdateResyncTickSchedulesClearThenDraw(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	updated, cmd := m.Update(resyncTickMsg{})
	if cmd == nil {
		t.Fatal("resync tick should schedule clear-then-draw")
	}
	got := updated.(Model)
	if got.redrawEpoch != 0 {
		t.Fatalf("resync tick changed redrawEpoch = %d, want 0", got.redrawEpoch)
	}
}

func TestUpdateResyncDrawTogglesInvisibleMarker(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	before := m.View()

	updated, cmd := m.Update(resyncDrawMsg{})
	if cmd == nil {
		t.Fatal("resync draw should schedule the next tick")
	}
	got := updated.(Model)
	after := got.View()

	if got.redrawEpoch != 1 {
		t.Fatalf("redrawEpoch = %d, want 1", got.redrawEpoch)
	}
	if before.Content == after.Content {
		t.Fatal("resync draw did not change raw View.Content")
	}
	if got, want := stripANSI(before.Content), stripANSI(after.Content); got != want {
		t.Fatalf("resync marker changed visible content: before %q after %q", got, want)
	}
}

func TestResyncDoesNotInvalidateDashboardCache(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.tab = tabDashboard
	m.dashboardLinesCache = []string{"cached"}

	m = update(m, resyncTickMsg{})
	if len(m.dashboardLinesCache) != 1 || m.dashboardLinesCache[0] != "cached" {
		t.Fatalf("resync tick invalidated dashboard cache: %#v", m.dashboardLinesCache)
	}

	m = update(m, resyncDrawMsg{})
	if len(m.dashboardLinesCache) != 1 || m.dashboardLinesCache[0] != "cached" {
		t.Fatalf("resync draw invalidated dashboard cache: %#v", m.dashboardLinesCache)
	}
}

func TestProgramResyncClearsThenRedraws(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	var in bytes.Buffer
	out := &safeBuffer{}
	p := tea.NewProgram(
		m,
		tea.WithWindowSize(80, 24),
		tea.WithInput(&in),
		tea.WithOutput(out),
		tea.WithoutSignals(),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
	)

	errs := make(chan error, 1)
	go func() {
		_, err := p.Run()
		errs <- err
	}()

	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(out.String(), "shaper") {
			break
		}
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("program failed: %v", err)
			}
			t.Fatal("program exited before initial render")
		case <-deadline:
			t.Fatalf("timed out waiting for initial render; output=%q", out.String())
		default:
		}
	}

	p.Send(resyncTickMsg{})
	resyncDeadline := time.After(2 * time.Second)
	for {
		output := out.String()
		if secondClearIdx, ok := secondClearIndex(output); ok && strings.LastIndex(output, "shaper") > secondClearIdx {
			break
		}
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("program failed: %v", err)
			}
			t.Fatal("program exited before resync render")
		case <-resyncDeadline:
			t.Fatalf("timed out waiting for resync redraw; output=%q", out.String())
		default:
		}
	}
	p.Quit()

	exitDeadline := time.After(2 * time.Second)
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("program failed: %v", err)
		}
	case <-exitDeadline:
		t.Fatal("timed out waiting for program exit")
	}
}

func TestArrowKeyScrolling(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("down arrow: cursor = %d, want 1", m.cursor)
	}

	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("up arrow: cursor = %d, want 0", m.cursor)
	}
}

func TestVisibleLogLines_NoFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("alpha\nbeta\ngamma\n"))
	lines := m.visibleLogLines()
	if len(lines) != 3 {
		t.Errorf("len = %d, want 3", len(lines))
	}
}

func TestVisibleLogLines_WithFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("alpha\nbeta\ngamma\n"))
	m.filterText = "alpha"
	lines := m.visibleLogLines()
	if len(lines) != 1 {
		t.Errorf("len = %d, want 1", len(lines))
	}
	if lines[0] != "alpha" {
		t.Errorf("lines[0] = %q, want alpha", lines[0])
	}
}

func TestVisibleLogLines_EmptyRing(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	lines := m.visibleLogLines()
	if lines != nil {
		t.Errorf("lines = %v, want nil", lines)
	}
}

func TestRenderLogs_NoOutput(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	s := m.renderLogs()
	if !strings.Contains(s, "No log output") {
		t.Errorf("renderLogs should mention 'No log output', got: %s", s)
	}
}

func TestRenderLogs_WithOutput(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("hello world\n"))
	s := m.renderLogs()
	if !strings.Contains(s, "hello world") {
		t.Errorf("renderLogs should contain 'hello world', got: %s", s)
	}
}

func TestRenderLogs_WithFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("alpha\nbeta\n"))
	m.filterText = "alpha"
	s := m.renderLogs()
	if !strings.Contains(s, "Filter:") {
		t.Errorf("renderLogs should show filter indicator, got: %s", s)
	}
}

// ─── TUI-09: Network Filtering / Rendering ───

func TestCanInspect_Dashboard(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	if m.canInspect() {
		t.Error("canInspect should be false for dashboard")
	}
}

func TestCanInspect_Requests(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{{Method: "POST", Path: "/v1/messages", Status: 200}}
	if !m.canInspect() {
		t.Error("canInspect should be true when cursor < len(entries)")
	}
}

func TestCanInspect_Logs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("hello\n"))
	if !m.canInspect() {
		t.Error("canInspect should be true when cursor < len(lines)")
	}
}

func TestCanInspect_Concurrency(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{{ID: 1, Method: "POST", Path: "/v1/messages"}}
	if !m.canInspect() {
		t.Error("canInspect should be true when cursor < len(inflight)")
	}
}

func TestCanInspect_Default(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRoutes
	if m.canInspect() {
		t.Error("canInspect should be false for routes tab")
	}
}

func TestMouseClickScrollbar_TopJumpsToTop(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.scroll = 50
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m)})
	if m2.scroll != 0 {
		t.Errorf("click at top of scrollbar: scroll = %d, want 0", m2.scroll)
	}
}

func TestMouseClickScrollbar_BottomJumpsNearBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m) + m.dataRows() - 1})
	if m2.scroll < 70 {
		t.Errorf("click at bottom of scrollbar: scroll = %d, want >= 70", m2.scroll)
	}
}

func TestMouseClickScrollbar_MiddleJumpsNearMiddle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m) + m.dataRows()/2})
	// Track height is dataRows (18), so the center maps to ~49 of maxScroll (82).
	if m2.scroll < 45 || m2.scroll > 55 {
		t.Errorf("click at middle of scrollbar: scroll = %d, want [45, 55]", m2.scroll)
	}
}

func TestUpdateScrollbars_AtVariousSizes(t *testing.T) {
	for w := 5; w <= 120; w += 5 {
		for h := 4; h <= 40; h += 4 {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = w
			m.height = h
			m.tab = tabRequests
			m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
			m.scroll = 50
			m.updateScrollbars()
			sb := m.scrollbars[m.tab]
			if sb.ContentHeight != 100 {
				t.Errorf("w=%d h=%d: ContentHeight = %d, want 100", w, h, sb.ContentHeight)
			}
			want := m.dataRows()
			if sb.ViewportHeight != want {
				t.Errorf("w=%d h=%d: ViewportHeight = %d, want %d", w, h, sb.ViewportHeight, want)
			}
		}
	}
}

func TestUpdateScrollbars_PerTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 10)
	m.updateScrollbars()
	if m.scrollbars[tabRequests].ContentHeight != 10 {
		t.Errorf("requests ContentHeight = %d, want 10", m.scrollbars[tabRequests].ContentHeight)
	}
	if m.scrollbars[tabRoutes].ContentHeight != 0 {
		t.Errorf("routes ContentHeight = %d, want 0 before visiting", m.scrollbars[tabRoutes].ContentHeight)
	}

	m.snap.RouteStats = map[string]metrics.RouteStat{
		"POST /a": {Total: 1},
		"POST /b": {Total: 2},
	}
	m.tab = tabRoutes
	m.updateScrollbars()
	if m.scrollbars[tabRoutes].ContentHeight != 2 {
		t.Errorf("routes ContentHeight = %d, want 2", m.scrollbars[tabRoutes].ContentHeight)
	}
}

func TestSnapshotSugarGuardsNoProviders(t *testing.T) {
	// A model created with no providers must accept a legacy metrics.Snapshot
	// update without panicking (the ProviderUpdate guard mirrors this).
	m := NewModelForProviders(nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("metrics.Snapshot on a zero-provider model panicked: %v", r)
		}
	}()
	update(m, metrics.Snapshot{})
}

func TestResetStatsSendsOnChannel(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	// "c" opens the confirm overlay; anything other than "y" must dismiss
	// without signalling.
	m = update(m, key('n'))
	if m.mode != modeBrowse {
		t.Fatalf("after n: mode = %v, want modeBrowse", m.mode)
	}
	select {
	case <-m.resetCh:
		t.Fatal("n must not signal reset")
	default:
	}

	// c then y signals reset exactly once and returns to browse mode.
	m = update(m, key('c'))
	if m.mode != modeConfirm {
		t.Fatalf("after c: mode = %v, want modeConfirm", m.mode)
	}
	next, cmd := m.Update(key('y'))
	m2, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	m = m2
	if m.mode != modeBrowse {
		t.Fatalf("after y: mode = %v, want modeBrowse", m.mode)
	}
	if cmd == nil {
		t.Fatal("y must return the reset command")
	}
	// The command yields resetMsg; feeding it back through Update performs
	// the non-blocking send onto the reset channel.
	m = update(m, cmd())
	select {
	case <-m.resetCh:
	default:
		t.Fatal("c then y must signal reset")
	}
	select {
	case <-m.resetCh:
		t.Fatal("reset signal must not duplicate")
	default:
	}
}

// TestResetStatsSendNeverBlocks proves a second confirm while the buffer is
// still occupied is dropped, not deadlocked: the model's send is
// non-blocking and the channel is capped at one pending request.

func TestResetStatsSendNeverBlocks(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.resetCh = make(chan struct{}, 1)
	m.resetCh <- struct{}{} // occupy the buffer as main might lag

	done := make(chan Model, 1)
	go func() {
		m = update(m, key('c'))
		m = update(m, key('y'))
		done <- m
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("update with a full reset channel must not block")
	}
}

// TestFleetStrip_AggregateObservability pins the one-line fleet strip atop the
// TUI in multi-provider mode (M6/G8).

func TestDashboardScrollsWithKeyboard(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	// Add enough dashboard state that the content exceeds visibleRows.
	m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
	for i := range 10 {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID:     uint64(i),
			Method: "POST",
			Path:   "/v1/messages",
		})
	}

	maxC := m.maxCursor()
	if maxC <= m.visibleRows() {
		t.Fatalf("dashboard content too small: maxCursor=%d, visible=%d", maxC, m.visibleRows())
	}

	// Jump to bottom and verify the dashboard scrolls.
	m2 := update(m, key('G'))
	if m2.scroll <= 0 {
		t.Errorf("dashboard scroll = %d, want > 0 after G", m2.scroll)
	}

	// Jump back to top.
	m3 := update(m2, key('g'))
	if m3.scroll != 0 {
		t.Errorf("dashboard scroll = %d, want 0 after g", m3.scroll)
	}
}

func TestDashboardScrollbarNotFullWhenOverflow(t *testing.T) {
	// Force dashboard overflow by shrinking the terminal so dashboard lines
	// exceed visibleRows. A 15-row terminal gives visibleRows=11 (height-4),
	// which is smaller than the default dashboard line count (~18).
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 15
	m.tab = tabDashboard
	m.dashboardLinesCache = m.dashboardLines()
	v := m.View()
	// The scrollbar must contain both track (│) and thumb (█) characters
	// in the rightmost column when content overflows.
	if m.maxCursor()+1 <= m.visibleRows() {
		t.Skipf("dashboard not overflowing: lines=%d visibleRows=%d", m.maxCursor()+1, m.visibleRows())
	}
	if !strings.Contains(v.Content, "│") {
		t.Error("dashboard overflow should render scrollbar track (│)")
	}
	if !strings.Contains(v.Content, "█") {
		t.Error("dashboard overflow should render scrollbar thumb (█)")
	}
}

func TestDashboardScrollByLine(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 20)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, key('j'))
	if m2.scroll != 1 || m2.cursor != 1 {
		t.Errorf("after j: scroll=%d cursor=%d, want 1,1", m2.scroll, m2.cursor)
	}

	m3 := update(m2, key('k'))
	if m3.scroll != 0 || m3.cursor != 0 {
		t.Errorf("after k: scroll=%d cursor=%d, want 0,0", m3.scroll, m3.cursor)
	}
}

func TestDashboardScrollArrowKeys(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 20)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m2.scroll != 1 {
		t.Errorf("after Down: scroll=%d, want 1", m2.scroll)
	}

	m3 := update(m2, tea.KeyPressMsg{Code: tea.KeyUp})
	if m3.scroll != 0 {
		t.Errorf("after Up: scroll=%d, want 0", m3.scroll)
	}
}

func TestDashboardScrollPageAndEndKeys(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 30)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m2.scroll <= 0 {
		t.Errorf("after PgDown: scroll=%d, want > 0", m2.scroll)
	}
	if m2.scroll > m2.maxScroll() {
		t.Errorf("after PgDown: scroll=%d exceeds maxScroll=%d", m2.scroll, m2.maxScroll())
	}

	m3 := update(m2, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m3.scroll != m3.maxScroll() {
		t.Errorf("after End: scroll=%d, want %d", m3.scroll, m3.maxScroll())
	}

	m4 := update(m3, tea.KeyPressMsg{Code: tea.KeyHome})
	if m4.scroll != 0 || m4.cursor != 0 {
		t.Errorf("after Home: scroll=%d cursor=%d, want 0,0", m4.scroll, m4.cursor)
	}

	m5 := update(m, tea.KeyPressMsg{Text: "ctrl+d"})
	if m5.scroll <= 0 {
		t.Errorf("after Ctrl-D: scroll=%d, want > 0", m5.scroll)
	}

	m6 := update(m5, tea.KeyPressMsg{Text: "ctrl+u"})
	if m6.scroll >= m5.scroll {
		t.Errorf("after Ctrl-U: scroll=%d, want < previous %d", m6.scroll, m5.scroll)
	}
}

func TestDashboardScrollGKeys(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 30)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, key('G'))
	if m2.scroll != m2.maxScroll() {
		t.Errorf("after G: scroll=%d, want %d", m2.scroll, m2.maxScroll())
	}

	m3 := update(m2, key('g'))
	if m3.scroll != 0 || m3.cursor != 0 {
		t.Errorf("after g: scroll=%d cursor=%d, want 0,0", m3.scroll, m3.cursor)
	}
}

func TestDashboardScrollDoesNotAffectRequestsTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m2 := update(m, key('j'))
	if m2.cursor != 1 {
		t.Errorf("requests tab cursor=%d, want 1", m2.cursor)
	}
	if m2.scroll != 0 {
		t.Errorf("requests tab scroll=%d, want 0", m2.scroll)
	}
}
