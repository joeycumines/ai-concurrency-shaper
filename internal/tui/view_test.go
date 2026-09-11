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
)

func TestViewRenders(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	m.height = 24
	v := m.View()
	if v.Content == "" {
		t.Fatal("View() returned empty content")
	}
	if !strings.Contains(v.Content, "shaper") {
		t.Errorf("View should contain 'shaper', got: %s", v.Content)
	}
}

func TestViewContainsAllTabs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 8}})
	m.width = 80
	m.height = 24
	v := m.View()
	for _, tab := range []string{"1 Overview", "2 Requests", "3 Network", "4 Logs", "5 Concurrency", "6 Routes"} {
		if !strings.Contains(v.Content, tab) {
			t.Errorf("View missing tab %q", tab)
		}
	}
}

func TestViewContainsKeybindings(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	for _, kb := range []string{"j/k:scroll", "?:help"} {
		if !strings.Contains(v.Content, kb) {
			t.Errorf("View missing keybinding %q", kb)
		}
	}
}

func TestViewWithEmptySnapshot(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap = metrics.NewCollector().Snapshot()
	v := m.View()
	if v.Content == "" {
		t.Fatal("View returned empty for zero snapshot")
	}
	if !strings.Contains(v.Content, "No requests yet") {
		t.Error("Should show 'No requests yet' for empty log")
	}
}

func TestAltScreenEnabled(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	if !v.AltScreen {
		t.Error("AltScreen should be enabled")
	}
}

func TestMouseModeEnabled(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %d, want MouseModeCellMotion", v.MouseMode)
	}
}

func secondClearIndex(output string) (int, bool) {
	clearIdx := strings.Index(output, "\x1b[2J")
	if clearIdx < 0 {
		return 0, false
	}
	secondClearIdx := strings.Index(output[clearIdx+len("\x1b[2J"):], "\x1b[2J")
	if secondClearIdx < 0 {
		return 0, false
	}
	return clearIdx + len("\x1b[2J") + secondClearIdx, true
}

// TestFooterMentionsReset pins the footer's c:reset hint so the binding stays
// discoverable from every tab.
func TestFooterMentionsReset(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	s := stripANSI(m.renderFooter())
	if !strings.Contains(s, "c:reset") {
		t.Errorf("footer should contain %q, got:\n%s", "c:reset", s)
	}
}

func TestView_NarrowTerminal(t *testing.T) {
	// View should render without panic at various narrow widths.
	for w := 1; w <= 80; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		v := m.View()
		if v.Content == "" && w >= 4 {
			t.Errorf("width=%d: View returned empty content", w)
		}
	}
}

func TestView_TinyTerminal(t *testing.T) {
	// Very small terminal: 10x5.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 10
	m.height = 5
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 20)
	v := m.View()
	if v.Content == "" {
		t.Error("View should render something even at 10x5")
	}
}

func TestView_Height4Minimum(t *testing.T) {
	// Minimum viable terminal: header(1) + tabbar(1) + separator(1) + footer(1) = 4.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 4
	m.tab = tabDashboard
	v := m.View()
	if v.Content == "" {
		t.Error("View should render at height 4")
	}
}

func TestView_Height3TooSmall(t *testing.T) {
	// Height 3 is below minimum — should return empty.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 3
	v := m.View()
	if v.Content != "" {
		t.Error("View should return empty for height < 4")
	}
}

func TestView_ZeroSize(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	v := m.View()
	if v.Content != "" {
		t.Error("View should return empty for zero size")
	}
}

// ─── Viewport: Scrollbar at Various Sizes ───

func TestFooter_AnchoredAtBottom(t *testing.T) {
	for _, tab := range []tabID{tabDashboard, tabRequests, tabNetwork, tabLogs, tabConcurrency, tabRoutes} {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = 80
		m.height = 24
		m.tab = tab

		// Give each tab enough content that it is non-empty.
		switch tab {
		case tabDashboard:
			m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
		case tabRequests:
			m.snap.LogEntries = []metrics.RequestLogEntry{}
			for i := range 25 {
				m.snap.LogEntries = append(m.snap.LogEntries, metrics.RequestLogEntry{
					Method: "POST", Path: "/v1/messages", Status: 200,
					Time: time.Now().Add(-time.Duration(i) * time.Second),
				})
			}
		case tabNetwork:
			// journal is nil in the test model; keep empty to avoid panic.
		case tabLogs:
			m.logRing.Write([]byte("first log line\nsecond log line\n"))
		case tabConcurrency:
			for i := range 15 {
				m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
					ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
				})
			}
		case tabRoutes:
			m.snap.RouteStats = map[string]metrics.RouteStat{
				"POST /v1/messages": {Total: 1},
			}
		}

		v := m.View()
		lines := strings.Split(v.Content, "\n")

		// The view must produce exactly one line per terminal row.
		if len(lines) != m.height {
			t.Errorf("tab=%d: view has %d lines, want %d", tab, len(lines), m.height)
			continue
		}

		// The footer must be the very last line.
		last := lines[len(lines)-1]
		if !strings.Contains(last, "1-6:tab") {
			t.Errorf("tab=%d: last line should be footer, got %q", tab, last)
		}
	}
}

func TestView_ViewportClampsOverflow(t *testing.T) {
	// When a tab's fixed chrome plus data exceeds the allocated visibleRows,
	// renderContentWithScrollbar must clip to exactly visibleRows lines instead
	// of expanding. Without this guard the output grows taller than the
	// terminal, pushing the chrome and scrollable content upward out of sight
	// while the footer stays anchored at the bottom.
	for _, tab := range []tabID{tabDashboard, tabRequests, tabNetwork, tabLogs, tabConcurrency, tabRoutes} {
		for _, h := range []int{8, 14, 50} {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = 80
			m.height = h
			m = update(m, tea.WindowSizeMsg{Width: 80, Height: h})
			m.tab = tab
			// Populate enough state that each tab has some fixed content to emit.
			switch tab {
			case tabRequests:
				m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
			case tabNetwork:
				// journal entries are exercised by network detail PTY tests;
				// browse state here relies on visibleEntries which is nil.
			case tabLogs:
				m.logRing.Write([]byte("first log line\nsecond log line\n"))
			case tabConcurrency:
				m.snap.InFlight = make([]metrics.InFlightEntry, 5)
			case tabRoutes:
				m.snap.RouteStats = map[string]metrics.RouteStat{
					"POST /v1/messages": {Total: 1},
				}
			}
			v := m.View()
			lines := strings.Split(v.Content, "\n")
			if len(lines) != h {
				t.Errorf("tab=%d height=%d: view has %d lines, want %d", tab, h, len(lines), h)
				continue
			}
			first := stripANSI(lines[0])
			if !strings.Contains(first, "shaper") {
				t.Errorf("tab=%d height=%d: header scrolled out of sight, got %q", tab, h, first)
			}
			last := lines[len(lines)-1]
			if !strings.Contains(last, "1-6:tab") {
				t.Errorf("tab=%d height=%d: footer not anchored at bottom, got %q", tab, h, last)
			}
		}
	}
}

func TestFooter_FilterPromptOnOwnLine(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.mode = modeFilter
	m.filterText = "hello"
	v := m.View()
	lines := strings.Split(v.Content, "\n")
	var footerIdx, filterIdx int
	for i, line := range lines {
		if strings.Contains(line, "1-6:tab") {
			footerIdx = i
		}
		if strings.Contains(line, "Filter: hello") {
			filterIdx = i
		}
	}
	if filterIdx == 0 {
		t.Fatal("filter prompt not found in view")
	}
	if footerIdx == 0 {
		t.Fatal("footer not found in view")
	}
	if footerIdx-filterIdx != 1 {
		t.Errorf("footer line = %d, filter line = %d, want filter immediately above footer", footerIdx, filterIdx)
	}
}

func TestFooterMentionsHorizontalScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	s := stripANSI(m.renderFooter())
	if !strings.Contains(s, "h/l:hscroll") {
		t.Errorf("footer should contain %q, got:\n%s", "h/l:hscroll", s)
	}
}

func TestViewport_SingleRow(t *testing.T) {
	// Content has exactly 1 row.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 1)
	m2 := update(m, key('j'))
	if m2.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (single row)", m2.cursor)
	}
}

func TestViewport_ExactFit(t *testing.T) {
	// Content exactly fits the data row area.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	visible := m.dataRows()
	m.snap.LogEntries = make([]metrics.RequestLogEntry, visible)
	m2 := update(m, key('G'))
	if m2.cursor != visible-1 {
		t.Errorf("cursor = %d, want %d", m2.cursor, visible-1)
	}
	if m2.scroll != 0 {
		t.Errorf("scroll = %d, want 0 (exact fit)", m2.scroll)
	}
}

func TestViewport_OnePastFit(t *testing.T) {
	// Content is exactly one more than the data row area.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	visible := m.dataRows()
	m.snap.LogEntries = make([]metrics.RequestLogEntry, visible+1)
	m2 := update(m, key('G'))
	if m2.cursor != visible {
		t.Errorf("cursor = %d, want %d", m2.cursor, visible)
	}
	if m2.scroll != 1 {
		t.Errorf("scroll = %d, want 1 (one past fit)", m2.scroll)
	}
}

func TestViewport_EmptyAfterFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
	}
	m.filterText = "nonexistent"
	m2 := update(m, key('j'))
	if m2.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (empty after filter)", m2.cursor)
	}
}

func TestViewport_CursorPreservedOnScroll(t *testing.T) {
	// After scrolling, cursor should stay at same content position.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.cursor = 50
	m.scroll = 40
	// Page up.
	m2 := update(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	// Cursor should have moved up by data rows.
	expected := 50 - m.dataRows()
	if m2.cursor != expected {
		t.Errorf("cursor = %d, want %d after PgUp", m2.cursor, expected)
	}
}
