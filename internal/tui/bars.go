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
	"math"
	"strings"

	"charm.land/lipgloss/v2"
)

func (m Model) renderGaugeBar(active, max, width int) string {
	if max <= 0 || width <= 0 {
		return m.styles.dimStyle2.Render("  [ empty ]")
	}
	pct := min(int(math.Round(float64(active)/float64(max)*100)), 100)
	filled := min(int(math.Round(float64(pct)/100.0*float64(width))), width)
	if filled < 0 {
		filled = 0
	}
	empty := width - filled

	bar := "  ["
	bar += m.gaugeFillStyle(pct).Render(strings.Repeat("█", filled))
	if empty > 0 {
		bar += m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]  "
	return bar
}

func (m Model) gaugeFillStyle(pct int) lipgloss.Style {
	switch {
	case pct >= 90:
		return m.styles.gaugeCriticalStyle
	case pct >= 60:
		return m.styles.gaugeWarnStyle
	default:
		return m.styles.gaugeNormalStyle
	}
}

func (m Model) renderHBar(value, valueMax, width int, color lipgloss.Style) string {
	if valueMax <= 0 || width <= 0 {
		return m.styles.dimStyle2.Render("  [ empty ]")
	}
	filled := max(min(int(math.Round(float64(value)/float64(valueMax)*float64(width))), width), 0)
	empty := width - filled

	bar := "  ["
	if filled > 0 {
		bar += color.Render(strings.Repeat("█", filled))
	}
	if empty > 0 {
		bar += m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]  "
	return bar
}

// renderDualBars renders the Concurrency gauge and Queue Depth bar
// side by side on a single line, each with a left-hand label in
// the section header color (#58A6FF).
// activeWidth controls the LHS (Active) gauge width; the RHS (Queued)
// bar gets whatever space remains. The left edge of the LHS bar and the
// right edge of the RHS bar align with the positions the previous
// single bar occupied, and the two bars meet in the middle with a gap.
func (m Model) renderDualBars(activeWidth int) string {
	activeWidth = max(activeWidth, 0)
	vw := m.viewportWidth()

	// Label widths (visible, excluding ANSI codes): "  Active  " = 10, "Queued   " = 9
	// Layout: "  Active  [" + lhsBlocks + "]  Queued   [" + rhsBlocks + "]  "
	// Total = 10 + 1 + activeWidth + 1 + 2 + 9 + 1 + rhsWidth + 1 + 2 = activeWidth + rhsWidth + 27
	// rhsWidth = vw - activeWidth - 27
	lhsLabel := m.styles.sectionStyle.Render("  Active  ")
	rhsLabel := m.styles.sectionStyle.Render("Queued   ")
	trailing := "  "
	rhsWidth := max(vw-activeWidth-27, 0)

	queueMax := m.conc * 4
	if queueMax == 0 {
		queueMax = 1
	}

	var b strings.Builder

	// LHS: concurrency gauge bar
	b.WriteString(lhsLabel)
	b.WriteString("[")
	if m.conc > 0 && activeWidth > 0 {
		lhsFilled := max(min(int(math.Round(float64(m.snap.Active)/float64(m.conc)*float64(activeWidth))), activeWidth), 0)
		pct := min(int(math.Round(float64(m.snap.Active)/float64(m.conc)*100)), 100)
		b.WriteString(m.gaugeFillStyle(pct).Render(strings.Repeat("█", lhsFilled)))
		if lhsFilled < activeWidth {
			b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", activeWidth-lhsFilled)))
		}
	} else {
		b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", activeWidth)))
	}
	b.WriteString("]")

	// Gap between bars
	b.WriteString("  ")

	// RHS: queue depth bar
	b.WriteString(rhsLabel)
	b.WriteString("[")
	if queueMax > 0 && rhsWidth > 0 {
		rhsFilled := max(min(int(math.Round(float64(m.snap.Queued)/float64(queueMax)*float64(rhsWidth))), rhsWidth), 0)
		b.WriteString(m.queueFillStyle(int(m.snap.Queued), queueMax).Render(strings.Repeat("█", rhsFilled)))
		if rhsFilled < rhsWidth {
			b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", rhsWidth-rhsFilled)))
		}
	} else {
		b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", rhsWidth)))
	}
	b.WriteString("]" + trailing)

	return b.String()
}

func (m Model) queueFillStyle(value, valueMax int) lipgloss.Style {
	if valueMax <= 0 {
		return m.styles.gaugeEmptyStyle
	}
	pct := min(int(math.Round(float64(value)/float64(valueMax)*100)), 100)
	switch {
	case pct >= 90:
		return m.styles.gaugeCriticalStyle
	case pct >= 50:
		return m.styles.queueWarnStyle
	case value > 0:
		return m.styles.queueFillDefaultStyle
	default:
		return m.styles.gaugeEmptyStyle
	}
}

func (m Model) statusStyle(code int) lipgloss.Style {
	switch {
	case code >= 100 && code < 200:
		return m.styles.statusInfoStyle
	case code >= 200 && code < 300:
		return m.styles.statusOkStyle
	case code >= 300 && code < 400:
		return m.styles.statusRedirectStyle
	case code >= 400 && code < 500:
		return m.styles.statusClientErrStyle
	case code >= 500:
		return m.styles.statusServerErrStyle
	default:
		return m.styles.dimStyle2
	}
}
