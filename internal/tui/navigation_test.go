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
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/viewport"
)

func TestTabSwitching(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	m = update(m, key('2'))
	if m.tab != tabRequests {
		t.Errorf("tab = %d, want tabRequests", m.tab)
	}

	m = update(m, key('3'))
	if m.tab != tabNetwork {
		t.Errorf("tab = %d, want tabNetwork", m.tab)
	}

	m = update(m, key('4'))
	if m.tab != tabLogs {
		t.Errorf("tab = %d, want tabLogs", m.tab)
	}

	m = update(m, key('5'))
	if m.tab != tabConcurrency {
		t.Errorf("tab = %d, want tabConcurrency", m.tab)
	}

	m = update(m, key('6'))
	if m.tab != tabRoutes {
		t.Errorf("tab = %d, want tabRoutes", m.tab)
	}

	m = update(m, key('1'))
	if m.tab != tabDashboard {
		t.Errorf("tab = %d, want tabDashboard", m.tab)
	}
}

func TestScrollDown(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	for range 50 {
		m.snap.LogEntries = append(m.snap.LogEntries, metrics.RequestLogEntry{
			Method: "POST", Path: "/v1/messages", Status: 200,
			Duration: time.Millisecond,
		})
	}

	for range 5 {
		m = update(m, key('j'))
	}
	if m.cursor != 5 {
		t.Errorf("cursor = %d, want 5", m.cursor)
	}
}

func TestScrollUp(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 5
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('k'))
	if m.cursor != 4 {
		t.Errorf("cursor = %d, want 4", m.cursor)
	}
}

func TestScrollStaysInBounds(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, key('k'))
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
}

func TestGoToTop(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 30
	m.scroll = 20
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('g'))
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("cursor=%d scroll=%d, want 0,0", m.cursor, m.scroll)
	}
}

func TestGoToBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('G'))
	if m.cursor != 49 {
		t.Errorf("cursor = %d, want 49", m.cursor)
	}
}

func TestAdjustViewportClampsOnFilterShrink(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
		{Method: "GET", Path: "/health", Status: 200},
		{Method: "GET", Path: "/health", Status: 500},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
	}

	// Position cursor deep into the list.
	m.cursor = 4
	m.scroll = 4

	// Enter filter mode and shrink the list.
	m = update(m, key('/'))
	m = update(m, key('h')) // filter to "h" (3 entries: health x2, chat/completions)

	// After the filter keystroke, adjustViewport should clamp cursor/scroll.
	if m.cursor > m.maxCursor() {
		t.Errorf("cursor (%d) not clamped to maxCursor (%d)", m.cursor, m.maxCursor())
	}
	if m.scroll > m.maxScroll() {
		t.Errorf("scroll (%d) not clamped to maxScroll (%d)", m.scroll, m.maxScroll())
	}

	// Ensure a render would show rows (not a blank screen).
	entries := m.visibleEntries()
	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(entries))
	if start >= end {
		t.Errorf("render would be blank: start=%d >= end=%d", start, end)
	}
}

func TestVisibleRows_NormalTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	v := m.visibleRows()
	if v != 20 {
		t.Errorf("visibleRows = %d, want 20 (24-4)", v)
	}
}

func TestVisibleRows_FilterTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.mode = modeFilter
	m.filterText = "x"
	v := m.visibleRows()
	if v != 19 {
		t.Errorf("visibleRows = %d, want 19 (24-4-1)", v)
	}
}

func TestVisibleRows_ToastOverlay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.AddToast(&toast.Toast{Message: "a", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "b", Duration: 5 * time.Second})
	v := m.visibleRows()
	if v != 18 {
		t.Errorf("visibleRows = %d, want 18 (24-4-2)", v)
	}
}

func TestVisibleRows_ConcurrencyTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	v := m.visibleRows()
	if v != 20 {
		t.Errorf("visibleRows = %d, want 20 (24-4)", v)
	}
}

func TestDataRows_PerTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	m.tab = tabRequests
	if got, want := m.dataRows(), 18; got != want {
		t.Errorf("requests dataRows = %d, want %d (header+count)", got, want)
	}

	m.tab = tabNetwork
	if got, want := m.dataRows(), 18; got != want {
		t.Errorf("network dataRows = %d, want %d (header+count)", got, want)
	}

	m.tab = tabLogs
	if got, want := m.dataRows(), 20; got != want {
		t.Errorf("logs dataRows = %d, want %d (no header row)", got, want)
	}

	m.tab = tabRoutes
	if got, want := m.dataRows(), 18; got != want {
		t.Errorf("routes dataRows = %d, want %d (header+count)", got, want)
	}

	m.tab = tabConcurrency
	if got, want := m.dataRows(), 10; got != want {
		t.Errorf("concurrency dataRows = %d, want %d (10 fixed rows)", got, want)
	}

	m.tab = tabDashboard
	if got, want := m.dataRows(), 20; got != want {
		t.Errorf("dashboard dataRows = %d, want %d", got, want)
	}
}

func TestVisibleRows_MatchesHeightLessChrome(t *testing.T) {
	// visibleRows is the space between chrome and footer; it may be zero at
	// the documented minimum height of 4.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 4
	if got, want := m.visibleRows(), 0; got != want {
		t.Errorf("height=4: visibleRows = %d, want %d", got, want)
	}
	m.height = 8
	if got, want := m.visibleRows(), 4; got != want {
		t.Errorf("height=8: visibleRows = %d, want %d", got, want)
	}
}

func TestMaxCursor_Dashboard(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	// The dashboard is now scrollable when its content exceeds the viewport.
	got := m.maxCursor()
	if got <= 0 {
		t.Errorf("maxCursor on dashboard = %d, want > 0 in the default 24-line terminal", got)
	}
}

func TestMaxCursor_Requests(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
	if m.maxCursor() != 4 {
		t.Errorf("maxCursor = %d, want 4", m.maxCursor())
	}
}

func TestMaxCursor_Logs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("line1\nline2\nline3\n"))
	if m.maxCursor() != 2 {
		t.Errorf("maxCursor = %d, want 2", m.maxCursor())
	}
}

func TestMaxCursor_Concurrency(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = make([]metrics.InFlightEntry, 3)
	if m.maxCursor() != 2 {
		t.Errorf("maxCursor = %d, want 2", m.maxCursor())
	}
}

func TestMaxCursor_Routes(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRoutes
	m.snap.RouteStats = map[string]metrics.RouteStat{
		"POST /a": {Total: 1},
		"POST /b": {Total: 2},
	}
	if m.maxCursor() != 1 {
		t.Errorf("maxCursor = %d, want 1", m.maxCursor())
	}
}

func TestMaxScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	ms := m.maxScroll()
	if ms < 0 {
		t.Errorf("maxScroll = %d, want >= 0", ms)
	}
}

func TestAdjustViewport_ScrollFollowsCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 30
	m.scroll = 0
	m.adjustViewport()
	if m.scroll == 0 {
		t.Error("scroll should have moved to follow cursor")
	}
}

func TestAdjustViewport_CursorClampedToMax(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 3)
	m.cursor = 100
	m.adjustViewport()
	if m.cursor > m.maxCursor() {
		t.Errorf("cursor = %d, want <= %d", m.cursor, m.maxCursor())
	}
}

func TestAdjustViewport_ScrollClampedToMax(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 3)
	m.scroll = 100
	m.adjustViewport()
	if m.scroll > m.maxScroll() {
		t.Errorf("scroll = %d, want <= %d", m.scroll, m.maxScroll())
	}
}

func TestMoveCursor_ClampsAtZero(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.moveCursor(-5)
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
}

func TestMoveCursor_ClampsAtMax(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 3)
	m.moveCursor(100)
	if m.cursor != m.maxCursor() {
		t.Errorf("cursor = %d, want %d", m.cursor, m.maxCursor())
	}
}

func TestSwitchTab_ResetsCursorAndScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 10
	m.scroll = 5
	m.switchTab(tabLogs)
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 after switchTab", m.cursor)
	}
	if m.scroll != 0 {
		t.Errorf("scroll = %d, want 0 after switchTab", m.scroll)
	}
}

func TestPageDown_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m = update(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.cursor == 0 {
		t.Error("PageDown should move cursor")
	}
}

func TestPageUp_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 20
	m = update(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.cursor >= 20 {
		t.Error("PageUp should decrease cursor")
	}
}

func TestHomeKey_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 30
	m = update(m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.cursor != 0 {
		t.Errorf("Home: cursor = %d, want 0", m.cursor)
	}
}

func TestEndKey_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.cursor != m.maxCursor() {
		t.Errorf("End: cursor = %d, want %d", m.cursor, m.maxCursor())
	}
}

func TestCtrlU_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 20
	m = update(m, tea.KeyPressMsg{Text: "ctrl+u"})
	if m.cursor >= 20 {
		t.Error("Ctrl-U should decrease cursor")
	}
}

func TestCtrlD_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m = update(m, tea.KeyPressMsg{Text: "ctrl+d"})
	if m.cursor == 0 {
		t.Error("Ctrl-D should increase cursor")
	}
}

// ─── TUI-07: logRing / logWriter ───

func TestSwitchTab_SetsModeBrowse(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.mode = modeHelp
	m.switchTab(tabLogs)
	if m.mode != modeBrowse {
		t.Errorf("mode = %d, want modeBrowse after switchTab", m.mode)
	}
}

func TestScrollbarClick_SetsDragging(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: 10})
	if !m2.dragging {
		t.Error("scrollbar click should set dragging = true")
	}
}

func TestScrollbarClick_JumpsScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: 18})
	if m2.scroll == 0 {
		t.Error("scrollbar click near bottom should set scroll > 0")
	}
}

func TestScrollbar_NarrowTerminal(t *testing.T) {
	// Scrollbar should render at narrow widths.
	for w := 5; w <= 40; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		v := m.View()
		// Should contain scrollbar characters.
		if !strings.Contains(v.Content, "│") && !strings.Contains(v.Content, "█") {
			t.Errorf("width=%d: View missing scrollbar chars", w)
		}
	}
}

func TestScrollbar_VariousHeights(t *testing.T) {
	// Scrollbar should work at various terminal heights.
	for h := 4; h <= 40; h++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = 80
		m.height = h
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
		v := m.View()
		if v.Content == "" {
			t.Errorf("height=%d: View returned empty", h)
		}
	}
}

// ─── Viewport: Mouse Click Precision ───

func TestKeyboardScroll_NarrowTerminal(t *testing.T) {
	for w := 10; w <= 80; w += 10 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		m2 := update(m, key('j'))
		if m2.cursor != 1 {
			t.Errorf("width=%d: cursor = %d, want 1 after j", w, m2.cursor)
		}
	}
}

func TestKeyboardScroll_TinyTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 10
	m.height = 6
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 20)
	// Should not panic.
	for range 5 {
		m = update(m, key('j'))
	}
	if m.cursor < 0 || m.cursor > m.maxCursor() {
		t.Errorf("cursor = %d, out of bounds [0, %d]", m.cursor, m.maxCursor())
	}
}

func TestGoToTop_NarrowTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 15
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 30
	m.scroll = 20
	m2 := update(m, key('g'))
	if m2.cursor != 0 || m2.scroll != 0 {
		t.Errorf("cursor=%d scroll=%d, want 0,0", m2.cursor, m2.scroll)
	}
}

func TestGoToBottom_NarrowTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 15
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, key('G'))
	if m2.cursor != m2.maxCursor() {
		t.Errorf("cursor = %d, want %d", m2.cursor, m2.maxCursor())
	}
}

// ─── Viewport: Content Rendering at Various Sizes ───

func TestScrollbarDrag_GrabOffsetNoJump(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.scroll = 42

	// Compute where the thumb is and grab its bottom edge.
	contentHeight := m.maxCursor() + 1
	trackHeight := m.dataRows()
	trackTop := scrollbarTop(m)
	thumbTop := viewport.ThumbTop(m.scroll, contentHeight, trackHeight)
	thumbHeight := viewport.ThumbHeight(contentHeight, trackHeight)
	grabY := trackTop + thumbTop + thumbHeight - 1

	m2 := update(m, tea.MouseClickMsg{X: 79, Y: grabY})
	if m2.scroll != 42 {
		t.Fatalf("click on thumb should not jump: scroll = %d, want 42", m2.scroll)
	}

	// Drag one row down: scroll should change by a small proportional amount,
	// not by a large absolute jump.
	m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: grabY + 1})
	delta := m3.scroll - 42
	if delta < 1 || delta > 10 {
		t.Errorf("drag one row: scroll delta = %d, want [1, 10]", delta)
	}
}

// ─── TUI-REVIEW-01: scratch/review-01.md fixes ───

func TestScrollbar_AlignedWithDataRows(t *testing.T) {
	// The Concurrency tab has 10 fixed header rows and 10 data rows. The
	// scrollbar should appear alongside the data rows, not the header rows.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	for i := range 15 {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
		})
	}
	m.updateScrollbars()

	s := m.renderContentWithScrollbar()
	lines := strings.Split(s, "\n")
	headerRows := 10
	// The first header row should have content but no scrollbar column.
	if len(lines) <= headerRows {
		t.Fatalf("expected at least %d lines, got %d", headerRows+1, len(lines))
	}
	firstDataRow := lines[headerRows]
	lastHeaderRow := lines[headerRows-1]
	if !strings.Contains(firstDataRow, "│") && !strings.Contains(firstDataRow, "█") {
		t.Errorf("first data row should contain scrollbar chars, got: %q", firstDataRow)
	}
	if strings.Contains(lastHeaderRow, "│") || strings.Contains(lastHeaderRow, "█") {
		t.Errorf("last header row should not contain scrollbar chars, got: %q", lastHeaderRow)
	}

	// Clicking inside the header area should not interact with the scrollbar.
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: contentStartRow + headerRows - 1})
	if m2.scroll != 0 {
		t.Errorf("click in header area should not scroll: scroll = %d, want 0", m2.scroll)
	}
	// Clicking at the top of the track should start a drag.
	m3 := update(m, tea.MouseClickMsg{X: 79, Y: contentStartRow + headerRows})
	if !m3.dragging {
		t.Error("click at top of data area should set dragging = true")
	}
}
