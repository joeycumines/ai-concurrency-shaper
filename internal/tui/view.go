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
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
	"github.com/rivo/uniseg"
)

func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "ai-concurrency-shaper"

	if m.width == 0 || m.height == 0 {
		return v
	}

	if m.width < 1 || m.height < 4 {
		v.SetContent("")
		return v
	}

	m.updateScrollbars()

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteByte('\n')
	b.WriteString(m.renderTabBar())
	b.WriteByte('\n')
	b.WriteString(m.styles.sepStyle.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	switch m.mode {
	case modeDetail:
		overlay := m.renderDetailOverlay()
		b.WriteString(overlay)
		m.padLines(&b, countContentLines(overlay))
	case modeHelp:
		help := m.renderHelpOverlay()
		b.WriteString(help)
		m.padLines(&b, countContentLines(help))
	case modeConfirm:
		confirm := m.renderConfirmOverlay()
		b.WriteString(confirm)
		m.padLines(&b, countContentLines(confirm))
	default:
		content := m.renderContentWithScrollbar()
		b.WriteString(content)
		m.padLines(&b, countContentLines(content))
	}

	if m.mode == modeFilter && (m.tab == tabRequests || m.tab == tabNetwork || m.tab == tabLogs) {
		b.WriteString(m.styles.filterPromptStyle.Render(fmt.Sprintf(" Filter: %s█", m.filterText)))
	}

	// Place the footer on the next row without inserting a wasted blank row.
	// When the content/overlay/prompt already ends with a newline, no extra
	// newline is needed; otherwise add exactly one.
	if !builderEndsWithNewline(&b) {
		b.WriteByte('\n')
	}

	visible := toast.VisibleToasts(m.toasts)
	if len(visible) > 0 && m.mode == modeBrowse {
		// Toasts are ephemeral overlays; draw them just above the footer so
		// they temporarily cover the bottom of the scrollable pane instead of
		// permanently reserving a block of empty space below the footer.
		start := 0
		if len(visible) > 3 {
			start = len(visible) - 3
		}
		for i := start; i < len(visible); i++ {
			if toastStr := visible[i].Render(m.width, 1); toastStr != "" {
				if !builderEndsWithNewline(&b) {
					b.WriteByte('\n')
				}
				b.WriteString(toastStr)
			}
		}
	}

	// Footer is always the very last row, anchored to the terminal bottom.
	if !builderEndsWithNewline(&b) {
		b.WriteByte('\n')
	}
	b.WriteString(m.renderFooter())
	b.WriteString(redrawMarker(m.redrawEpoch))

	v.SetContent(b.String())
	return v
}

func (m *Model) padLines(b *strings.Builder, lines int) {
	visible := m.visibleRows()
	for i := lines; i < visible; i++ {
		b.WriteByte('\n')
	}
}

// countContentLines returns the number of row-separating newlines in s. A
// trailing newline does not introduce an extra row; it separates the last
// content line from whatever follows (e.g. the footer).
func countContentLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if strings.HasSuffix(s, "\n") {
		return n
	}
	return n + 1
}

// builderEndsWithNewline reports whether the builder is non-empty and its
// last byte is '\n'. This centralizes the check so there is a single point
// of change if a more efficient approach (e.g. tracking the last byte
// written) is needed later. strings.Builder.String() returns a zero-copy
// view of the underlying buffer via unsafe.String, so the lookup is both
// O(1) and allocation-free.
func builderEndsWithNewline(b *strings.Builder) bool {
	if b.Len() == 0 {
		return false
	}
	// This builder is only ever written to with valid UTF-8 strings and
	// single '\n' bytes, so reading the last byte to check for '\n' is safe.
	return b.String()[b.Len()-1] == '\n'
}

func (m Model) renderContent() string {
	switch m.tab {
	case tabDashboard:
		return m.renderDashboardContent()
	case tabRequests:
		return m.renderRequests()
	case tabNetwork:
		return m.renderNetwork()
	case tabLogs:
		return m.renderLogs()
	case tabConcurrency:
		return m.renderConcurrency()
	case tabRoutes:
		return m.renderRoutes()
	}
	return ""
}

// renderContentWithScrollbar wraps the active tab's content with a scrollbar
// column in the rightmost position. ANSI-aware width calculation. The
// scrollbar is aligned with the scrollable data rows, below the fixed header
// rows returned by contentHeaderRows().
func (m Model) renderContentWithScrollbar() string {
	content := m.renderContent()
	if content == "" {
		return ""
	}
	sb := m.scrollbars[m.tab]
	scrollbarCol := sb.View()
	sbLines := strings.Split(scrollbarCol, "\n")

	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	contentWidth := m.viewportWidth()
	hScroll := m.hScroll[m.tab]
	headerRows := m.contentHeaderRows()
	visibleRows := m.visibleRows()

	var b strings.Builder
	// Clamp the rendered content to the allocated viewport. Individual tab
	// renderers are expected to stay within visibleRows, but this guard
	// prevents any overflow from pushing chrome or footer off-screen.
	contentLimit := min(len(lines), visibleRows)
	for i := range visibleRows {
		if i < contentLimit {
			line := lines[i]
			stripped := stripANSI(line)
			visibleCells := uniseg.StringWidth(stripped)
			switch {
			case i >= headerRows && hScroll > 0:
				// The shift applies to every data row, not just overflowing
				// ones: rows are columns of one table, so a row that keeps
				// its left edge while its neighbors shift is a torn table.
				// A row shorter than the offset renders empty.
				shifted := skipANSI(line, hScroll)
				visible := truncateANSI(shifted, contentWidth)
				b.WriteString(visible)
				actualWidth := uniseg.StringWidth(stripANSI(visible))
				if actualWidth < contentWidth {
					b.WriteString(strings.Repeat(" ", contentWidth-actualWidth))
				}
			case visibleCells > contentWidth:
				truncated := truncateANSI(line, contentWidth)
				b.WriteString(truncated)
				actualWidth := uniseg.StringWidth(stripANSI(truncated))
				if actualWidth < contentWidth {
					b.WriteString(strings.Repeat(" ", contentWidth-actualWidth))
				}
			default:
				b.WriteString(line)
				b.WriteString(strings.Repeat(" ", contentWidth-visibleCells))
			}
		} else {
			b.WriteString(strings.Repeat(" ", contentWidth))
		}
		if i >= headerRows {
			if sbIdx := i - headerRows; sbIdx < len(sbLines) {
				b.WriteString(sbLines[sbIdx])
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// maxHScroll returns how many cells the active tab's widest rendered row
// extends beyond the viewport. It scans the tab's rendered lines, so the
// bound always matches what renderContentWithScrollbar would draw.
func (m Model) maxHScroll() int {
	width := m.viewportWidth()
	maxWidth := 0
	for _, line := range strings.Split(m.renderContent(), "\n") {
		if w := uniseg.StringWidth(stripANSI(line)); w > maxWidth {
			maxWidth = w
		}
	}
	return max(maxWidth-width, 0)
}
