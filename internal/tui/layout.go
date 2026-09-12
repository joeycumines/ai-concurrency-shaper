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

import ()

// headerRowCount returns the number of rows the header occupies. In
// single-provider mode (or when the switcher is elided) this is 1. In fleet
// mode with wrapped provider chips it is 1 + the number of additional chip
// rows. The result is capped so at least 1 content row remains.
func (m Model) headerRowCount() int {
	rows := 1
	if m.hasSwitcher() {
		if layout := m.chipRowsLayout(); len(layout) > 1 {
			rows = len(layout)
		}
	}
	// Cap: header + tab bar + separator + >=1 content row + footer must fit.
	maxRows := max(m.height-4, 1)
	if rows > maxRows {
		rows = maxRows
	}
	return rows
}

// contentStartRow returns the first row of the content area, below the
// header rows, tab bar, and separator.
func (m Model) contentStartRow() int {
	return m.headerRowCount() + 2
}

func (m *Model) visibleRows() int {
	// Reserve only the chrome (header, tabbar, separator, footer). Filter input
	// and active toasts are overlays above the footer; they reduce the
	// scrollable content area only while they are present, so no space is
	// wasted when they are absent.
	v := m.height - m.headerRowCount() - 3
	if m.mode == modeFilter && (m.tab == tabRequests || m.tab == tabNetwork || m.tab == tabLogs) {
		v--
	}
	if m.mode == modeBrowse {
		if n := len(m.toasts); n > 0 {
			v -= min(n, 3)
		}
	}
	if v < 0 {
		return 0
	}
	return v
}

// dataRows returns the number of data rows displayed for the active tab,
// after reserving fixed header, filter summary, and count lines. For the
// dashboard the entire content area is scrollable, so dataRows == visibleRows.
func (m *Model) dataRows() int {
	switch m.tab {
	case tabRequests:
		fixed := 2 // table header + count line
		if m.filterText != "" {
			fixed++ // filter summary
		}
		return max(m.visibleRows()-fixed, 1)
	case tabNetwork:
		fixed := 2 // table header + count line
		if m.networkFilterType != networkFilterAll || m.networkFilterStatus != networkStatusAll {
			fixed++ // type/status filter summary
		}
		if m.filterText != "" {
			fixed++ // text filter summary
		}
		return max(m.visibleRows()-fixed, 1)
	case tabLogs:
		fixed := 0 // no header row; line numbers are embedded in data rows
		if m.filterText != "" {
			fixed++ // filter summary
		}
		return max(m.visibleRows()-fixed, 1)
	case tabRoutes:
		return max(m.visibleRows()-2, 1) // header + count line
	case tabConcurrency:
		// Section headers/gauges for Concurrency Gauge, Queue Depth, and the
		// In-Flight Requests title occupy the first 10 rows of the content area.
		return max(m.visibleRows()-10, 1)
	case tabDashboard:
		return m.visibleRows()
	}
	return m.visibleRows()
}

// contentHeaderRows returns the number of fixed rows at the top of the
// scrollable area before the first data row. Clicks inside these rows should
// not move the cursor.
func (m Model) contentHeaderRows() int {
	switch m.tab {
	case tabRequests:
		if m.filterText != "" {
			return 2
		}
		return 1
	case tabNetwork:
		n := 1
		if m.networkFilterType != networkFilterAll || m.networkFilterStatus != networkStatusAll {
			n++
		}
		if m.filterText != "" {
			n++
		}
		return n
	case tabLogs:
		if m.filterText != "" {
			return 1 // filter summary only
		}
		return 0 // no header; line numbers are embedded in data rows
	case tabRoutes:
		return 1
	case tabConcurrency:
		return 10
	}
	return 0
}

// viewportWidth returns the width available for scrollable content,
// excluding the scrollbar column and separator.
func (m *Model) viewportWidth() int {
	return max(m.width-1, 1)
}

// gaugeBarWidth returns the inner block count for renderGaugeBar so the full
// rendered bar ("  [" + blocks + "]  ") matches the queue bar width and fits
// within the viewport width with a symmetrical two-cell left and right margin
// before the scrollbar column.
func (m *Model) gaugeBarWidth() int {
	// Full bar visual width: 3 + blocks + 1 + 2 = blocks + 6.
	// Must fit within viewportWidth.
	return max(m.viewportWidth()-6, 0)
}

// hBarWidth returns the inner block count for renderHBar so the full
// bar ("  [" + blocks + "]  ") fits within the viewport width with a
// symmetrical two-cell left and right margin before the scrollbar
// column.
func (m *Model) hBarWidth() int {
	// Full bar visual width: 3 + blocks + 1 + 2 = blocks + 6.
	// Must fit within viewportWidth.
	return max(m.viewportWidth()-6, 0)
}

// gaugeTrackWidth returns the bar track width used for the Active
// gauge in the dual bars row. The Status section bar uses the same
// width so its brackets align with the Active gauge at the same
// column (both section labels are 10 cells wide). The dual bars row
// has 27 fixed cells of labels, brackets, gap, and trailing spaces;
// the Active gauge takes half of the remaining width.
func (m *Model) gaugeTrackWidth() int {
	return max((m.viewportWidth()-27)/2, 0)
}

func (m *Model) maxCursor() int {
	switch m.tab {
	case tabDashboard:
		return max(len(m.cachedDashboardLines())-1, 0)
	case tabRequests:
		return max(len(m.visibleEntries())-1, 0)
	case tabNetwork:
		return max(len(m.visibleNetworkEntries())-1, 0)
	case tabLogs:
		return max(len(m.visibleLogLines())-1, 0)
	case tabConcurrency:
		return max(len(m.snap.InFlight)-1, 0)
	case tabRoutes:
		stats := m.snap.RouteStats
		return max(len(stats)-1, 0)
	}
	return 0
}

func (m *Model) maxScroll() int {
	return max(m.maxCursor()-m.dataRows()+1, 0)
}

// applyScrollbarTheme paints every per-tab scrollbar with the active theme's
// thumb/track colors. Called from NewModelForProviders and on theme swaps (and
// re-applied on each updateScrollbars) so a background change repaints the
// scrollbars too.
func (m *Model) applyScrollbarTheme() {
	for i := range m.scrollbars {
		m.scrollbars[i].ThumbStyle = m.styles.scrollbarThumb
		m.scrollbars[i].TrackStyle = m.styles.scrollbarTrack
	}
}

func (m *Model) updateScrollbars() {
	contentHeight := m.maxCursor() + 1
	viewportHeight := m.dataRows()
	sb := &m.scrollbars[m.tab]
	sb.ContentHeight = contentHeight
	sb.ViewportHeight = viewportHeight
	sb.YOffset = m.scroll
	m.applyScrollbarTheme()
}
