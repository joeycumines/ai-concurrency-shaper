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
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

// scrollbarTop returns the first terminal row that belongs to the scrollbar
// track for the current tab. It is offset past the fixed header rows so that
// the scrollbar aligns with the scrollable data area.
func scrollbarTop(m Model) int { return m.contentStartRow() + m.contentHeaderRows() }

func TestMouseClickContentArea_SetsCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: 5})
	// Row 5 skips the table header on row 3, so it maps to data row 1.
	if m2.cursor != 1 {
		t.Errorf("cursor = %d, want 1 (row 5 - m.contentStartRow() 3 - header 1)", m2.cursor)
	}
}

func TestMouseClickContentArea_ClampsToMaxCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: 20})
	if m2.cursor > m2.maxCursor() {
		t.Errorf("cursor = %d, should be clamped to maxCursor = %d", m2.cursor, m2.maxCursor())
	}
}

func TestMouseClickContentArea_DashboardSetsCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: 5})
	// Dashboard is scrollable, so a content click sets the cursor/scroll position.
	if m2.cursor != 2 {
		t.Errorf("cursor = %d, want 2 (row 5 - m.contentStartRow() 3 + scroll 0)", m2.cursor)
	}
}

func TestMouseClickContentArea_LogsTabNoHeader(t *testing.T) {
	// The Logs tab has no header row (line numbers are embedded in data rows),
	// so clicking the first content row should set cursor=0, not be ignored.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("line1\nline2\nline3\n"))
	// contentHeaderRows for Logs without filter is 0, so clicking
	// m.contentStartRow() (row 3) maps to cursor 0.
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: m.contentStartRow()})
	if m2.cursor != 0 {
		t.Errorf("Logs first-row click: cursor = %d, want 0 (no header offset)", m2.cursor)
	}
	// Second row should map to cursor 1.
	m3 := update(m, tea.MouseClickMsg{X: 10, Y: m.contentStartRow() + 1})
	if m3.cursor != 1 {
		t.Errorf("Logs second-row click: cursor = %d, want 1", m3.cursor)
	}
}

func TestMouseDrag_MotionUpdatesScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.dragging = true
	m2 := update(m, tea.MouseMotionMsg{X: 79, Y: 18})
	if m2.scroll == 0 {
		t.Error("mouse motion while dragging should update scroll")
	}
}

func TestMouseDrag_MotionWithoutDragIgnored(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseMotionMsg{X: 79, Y: 18})
	if m2.dragging {
		t.Error("mouse motion without prior click should not set dragging")
	}
	if m2.scroll != 0 {
		t.Error("mouse motion without dragging should not change scroll")
	}
}

func TestMouseRelease_ClearsDragging(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.dragging = true
	m2 := update(m, tea.MouseReleaseMsg{})
	if m2.dragging {
		t.Error("mouse release should clear dragging")
	}
}

// ─── Viewport: Terminal Size Adaptation ───

func TestMouseClickContentArea_CursorFollowsClick(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.scroll = 10
	// Click on the 2nd visible data row (past the table header).
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: m.contentStartRow() + m.contentHeaderRows() + 1})
	if m2.cursor != 11 {
		t.Errorf("cursor = %d, want 11 (scroll 10 + relative data row 1)", m2.cursor)
	}
}

func TestMouseClickContentArea_ClampedAtEnd(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
	m.scroll = 0
	// Click way past the end.
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: m.contentStartRow() + 100})
	if m2.cursor > m2.maxCursor() {
		t.Errorf("cursor = %d, should be clamped to %d", m2.cursor, m2.maxCursor())
	}
}

// ─── Viewport: Mouse Drag Precision ───

func TestMouseDrag_TopToBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	// Start drag at top of the scrollbar track.
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m)})
	if m2.scroll != 0 {
		t.Errorf("initial drag at top: scroll = %d, want 0", m2.scroll)
	}
	// Drag to bottom. With the corrected geometry (dragRange = trackHeight - thumbHeight)
	// the user can reach scrollMax.
	m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: scrollbarTop(m) + m.dataRows() - 1})
	sm := m.maxScroll()
	if sm > 0 && m3.scroll < sm-3 {
		t.Errorf("drag to bottom: scroll = %d, want >= %d (scrollMax=%d)", m3.scroll, sm-3, sm)
	}
}

func TestMouseDrag_Monotonic(t *testing.T) {
	// Dragging from top to bottom should produce monotonically increasing scroll.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m)})
	prev := -1
	for dy := 0; dy < m.dataRows(); dy++ {
		y := scrollbarTop(m) + dy
		m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: y})
		if m3.scroll < prev {
			t.Errorf("drag not monotonic: scroll at y=%d is %d, was %d", y, m3.scroll, prev)
		}
		prev = m3.scroll
	}
}

func TestMouseDrag_Proportional(t *testing.T) {
	// Dragging to 25%, 50%, 75% of track should produce proportional scroll.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)

	trackTop := scrollbarTop(m)

	quarter := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	quarter = update(quarter, tea.MouseMotionMsg{X: 79, Y: trackTop + m.dataRows()/4})

	half := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	half = update(half, tea.MouseMotionMsg{X: 79, Y: trackTop + m.dataRows()/2})

	threeQ := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	threeQ = update(threeQ, tea.MouseMotionMsg{X: 79, Y: trackTop + 3*m.dataRows()/4})

	// Quarter should be roughly 25% of max scroll.
	if quarter.scroll < 18 || quarter.scroll > 25 {
		t.Errorf("quarter drag: scroll = %d, want [18, 25]", quarter.scroll)
	}
	// Half should be roughly 50%.
	if half.scroll < 40 || half.scroll > 55 {
		t.Errorf("half drag: scroll = %d, want [40, 55]", half.scroll)
	}
	// 3/4 should be roughly 75%.
	if threeQ.scroll < 65 || threeQ.scroll > 81 {
		t.Errorf("three-quarter drag: scroll = %d, want [65, 81]", threeQ.scroll)
	}
}

func TestMouseDrag_ReachBottom(t *testing.T) {
	// Dragging from the very top to the very bottom of the scrollbar track
	// must reach scrollMax, proving the user can scroll to the last line.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	trackTop := scrollbarTop(m)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: trackTop + m.dataRows() - 1})
	sm := m.maxScroll()
	if sm > 0 && m3.scroll < sm {
		t.Errorf("full drag: scroll = %d, want %d (scrollMax)", m3.scroll, sm)
	}
}

// ─── Viewport: Keyboard Navigation at Various Sizes ───
