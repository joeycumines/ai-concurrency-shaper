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
	"math"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"
)

func (m Model) renderDashboardContent() string {
	lines := m.cachedDashboardLines()
	visible := m.visibleRows()
	start := max(min(m.scroll, len(lines)), 0)
	end := min(start+visible, len(lines))
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n") + "\n"
}

func (m Model) renderSparkline() string {
	spark := m.snap.Sparkline
	if len(spark) == 0 {
		return m.styles.dimStyle2.Render("  —")
	}
	maxVal := 0
	for _, v := range spark {
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == 0 {
		maxVal = 1
	}
	chars := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	var line strings.Builder
	line.WriteString("  ")
	for _, v := range spark {
		idx := max(int(float64(v)/float64(maxVal)*float64(len(chars)-1)), 0)
		if idx >= len(chars) {
			idx = len(chars) - 1
		}
		line.WriteString(chars[idx])
	}
	style := m.sparklineFillStyle(spark[len(spark)-1], maxVal)
	return style.Render(line.String())
}

func (m Model) sparklineFillStyle(last, max int) lipgloss.Style {
	if max <= 0 {
		return m.styles.sparklineStyle
	}
	pct := min(int(math.Round(float64(last)/float64(max)*100)), 100)
	switch {
	case pct >= 90:
		return m.styles.gaugeCriticalStyle
	case pct >= 60:
		return m.styles.gaugeWarnStyle
	default:
		return m.styles.sparklineStyle
	}
}

// renderStatusBar renders the HTTP status distribution as a stacked
// bar with count labels. The width parameter is a maximum bar track
// hint; the function reduces the track as needed so the full status
// line — the 10-cell "  Status  " prefix added by the caller, the
// bracketed bar, the inline labels, and the trailing spaces — fits
// within the viewport. When the viewport is too narrow for the labels
// to share the bar's line, the labels wrap onto one or more additional
// rows, packed so that every row fits the viewport.
//
// Zero-count status classes are omitted from both the bar segments
// and the labels (StatusCounts only ever increments, so negative
// counts cannot occur; non-positive classes are skipped regardless).
// Counts of 10^7 and above are abbreviated via formatCount so the
// labels stay short even at extreme magnitudes. The returned slice
// contains one or more single-line strings, never an embedded newline.
func (m Model) renderStatusBar(width int) []string {
	width = max(width, 0)
	vw := m.viewportWidth()

	counts := m.snap.StatusCounts
	total := counts[1] + counts[2] + counts[3] + counts[4] + counts[5]

	labels := []string{"1xx", "2xx", "3xx", "4xx", "5xx"}
	cvalues := []int64{counts[1], counts[2], counts[3], counts[4], counts[5]}
	colors := []lipgloss.Style{m.styles.statusInfoStyle, m.styles.statusOkStyle, m.styles.statusRedirectStyle, m.styles.statusClientErrStyle, m.styles.statusServerErrStyle}

	// The rendered labels are exactly the non-zero classes, matching
	// the bar segments; budget the same set so the width math matches
	// what is printed.
	var labelParts []string
	for i, v := range cvalues {
		if v <= 0 {
			continue
		}
		labelParts = append(labelParts, colors[i].Render(fmt.Sprintf("%s:%s", labels[i], formatCount(v))))
	}
	var labelsWidth int
	for _, p := range labelParts {
		labelsWidth += 1 + uniseg.StringWidth(stripANSI(p)) // leading space
	}
	var abortedPart string
	if m.snap.TotalAborted > 0 {
		abortedPart = m.styles.gaugeWarnStyle.Render("Aborted:" + formatCount(m.snap.TotalAborted))
		labelsWidth += 1 + uniseg.StringWidth(stripANSI(abortedPart))
	}

	// Full status line: prefix=10 + brackets=2 + trailing=2.
	// The bar track is reduced so the line fits within the viewport.
	barWidth := min(width, max(vw-labelsWidth-14, 0))
	wrap := (len(labelParts) > 0 || abortedPart != "") && labelsWidth > vw-14
	if wrap {
		// The labels cannot share the bar's line even at zero track
		// width; give the bar as much room as the line allows and
		// wrap the labels onto their own line(s).
		barWidth = min(width, max(vw-14, 0))
	}

	var b strings.Builder
	b.WriteString("[")
	if total == 0 {
		b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", barWidth)))
	} else {
		pos := 0
		for i, v := range cvalues {
			if v <= 0 {
				continue
			}
			seg := int(math.Round(float64(v) / float64(total) * float64(barWidth)))
			if seg == 0 {
				seg = 1
			}
			if pos+seg > barWidth {
				seg = barWidth - pos
			}
			b.WriteString(colors[i].Render(strings.Repeat("█", seg)))
			pos += seg
		}
		if pos < barWidth {
			b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", barWidth-pos)))
		}
	}
	b.WriteString("]")

	if wrap {
		b.WriteString("  ")
		// The labels cannot share the bar's line; pack them into one or
		// more rows that fit the viewport: each row is the parts joined
		// with single spaces within a viewportWidth-2 budget, indented
		// by two cells. A single part is at most 13 cells (8 + a
		// 5-cell abbreviated count, e.g. "Aborted:1000M"), far below
		// the budget for viewports of 40 columns and wider, so no part
		// is ever split or dropped; the Aborted label is joined into
		// the same set so the packer treats it uniformly with the
		// status classes.
		parts := make([]string, 0, len(labelParts)+1)
		parts = append(parts, labelParts...)
		if abortedPart != "" {
			parts = append(parts, abortedPart)
		}
		rows := packParts(parts, max(vw-2, 0), " ")
		lines := make([]string, 0, len(rows)+1)
		lines = append(lines, b.String())
		for _, row := range rows {
			lines = append(lines, "  "+row)
		}
		return lines
	}

	for _, p := range labelParts {
		b.WriteString(" ")
		b.WriteString(p)
	}
	if abortedPart != "" {
		b.WriteString(" ")
		b.WriteString(abortedPart)
	}
	b.WriteString("  ")
	return []string{b.String()}
}

// formatCount renders a non-negative counter for display, abbreviating
// magnitudes of 10^7 and above with a single decimal and an M/B/T/P/E
// suffix so the abbreviated display never exceeds 5 cells (e.g.
// 12,345,678 -> "12.3M", 12,345,678,901 -> "12.3B"). Counts below
// 10^7 render exactly. Counters only ever increment, so negative
// values cannot occur.
func formatCount(v int64) string {
	if v < 10_000_000 {
		return strconv.FormatInt(v, 10)
	}
	units := []string{"M", "B", "T", "P", "E"}
	scale := int64(1_000_000)
	for i, u := range units {
		// v < scale*1000 without overflow (scale is 10^18 at i=4,
		// where the comparison always holds for int64).
		if v/1000 < scale || i == len(units)-1 {
			scaled := float64(v) / float64(scale)
			if scaled >= 99.95 { // would round to 100.0+ with one decimal
				return fmt.Sprintf("%d%s", int64(math.Round(scaled)), u)
			}
			return fmt.Sprintf("%.1f%s", scaled, u)
		}
		scale *= 1000
	}
	return strconv.FormatInt(v, 10)
}

// packParts packs label:value parts into rows joined by sep so that
// every row fits within vw cells, starting a new row whenever the next
// part would overflow. Each part must itself fit within vw; parts are
// never split across rows. Widths are measured on the ANSI-stripped
// content, so escape sequences in parts do not count toward the row
// width.
func packParts(parts []string, vw int, sep string) []string {
	var rows []string
	var b strings.Builder
	width := 0
	sepWidth := uniseg.StringWidth(stripANSI(sep))
	for _, p := range parts {
		extra := 0
		if width > 0 {
			extra = sepWidth
		}
		if width+extra+uniseg.StringWidth(stripANSI(p)) > vw {
			if width > 0 {
				rows = append(rows, b.String())
			}
			b.Reset()
			width = 0
			extra = 0
		}
		if extra > 0 {
			b.WriteString(sep)
		}
		b.WriteString(p)
		width += extra + uniseg.StringWidth(stripANSI(p))
	}
	if width > 0 {
		rows = append(rows, b.String())
	}
	return rows
}

// dashboardLines builds the full list of rendered lines for the Dashboard tab.
// It always renders all content (including up to six in-flight requests);
// renderDashboardContent() windows these lines by m.scroll and m.visibleRows().
func (m Model) dashboardLines() []string {
	var lines []string

	if cb := m.snap.CircuitBreaker; cb != nil {
		lines = append(lines, m.styles.sectionStyle.Render(" Circuit Breaker "))
		var stateStyle lipgloss.Style
		switch cb.State {
		case "CLOSED":
			stateStyle = m.styles.circuitClosedStyle
		case "OPEN":
			stateStyle = m.styles.circuitOpenStyle
		case "HALF_OPEN":
			stateStyle = m.styles.circuitHalfOpenStyle
		default:
			stateStyle = lipgloss.NewStyle()
		}
		// The breaker summary is packed into rows that fit the viewport
		// (same mechanism as the Summary metrics): parts joined by
		// "  |  " within a viewportWidth-2 budget, two cells of indent
		// per row.
		cbParts := []string{"State: " + stateStyle.Render(cb.State)}
		cbParts = append(cbParts,
			"Failures: "+formatCount(cb.Failures),
			"Consecutive: "+formatCount(cb.ConsecutiveFailures))
		if cb.CurrentPenalty > 0 {
			cbParts = append(cbParts, "Penalty: "+cb.CurrentPenalty.Truncate(time.Millisecond).String())
		}
		if !cb.NextRetry.IsZero() {
			if until := time.Until(cb.NextRetry).Truncate(time.Millisecond); until > 0 {
				cbParts = append(cbParts, "Next probe: "+until.String())
			}
		}
		for _, row := range packParts(cbParts, max(m.viewportWidth()-2, 0), "  |  ") {
			lines = append(lines, "  "+row)
		}
		lines = append(lines, "")
	}
	lines = append(lines, m.styles.sectionStyle.Render(" Throughput (10s) "))
	lines = append(lines, m.renderSparkline())
	lines = append(lines, "")

	gaugeWidth := m.gaugeTrackWidth()

	lines = append(lines, m.renderDualBars(gaugeWidth))

	if m.snap.RetriesInFlight > 0 {
		lines = append(lines, fmt.Sprintf("  %d active retries", m.snap.RetriesInFlight))
	}

	lines = append(lines, "")
	statusLines := m.renderStatusBar(gaugeWidth)
	lines = append(lines, m.styles.sectionStyle.Render("  Status  ")+statusLines[0])
	lines = append(lines, statusLines[1:]...)

	lines = append(lines, "")
	lines = append(lines, m.styles.sectionStyle.Render(" In-Flight Requests "))
	flights := m.snap.InFlight
	// The summary renders as a single line whenever the abbreviated
	// counts fit the viewport (always at width >= 80: the widest single
	// line form, with 5-cell "1000M"-style counts, is at most 51 cells).
	// When it cannot fit — narrow viewports with multi-digit counts —
	// the three parts pack into rows within a viewportWidth-2 budget,
	// indented by two cells, so "passthrough" is never silently
	// truncated by renderContentWithScrollbar.
	summary := fmt.Sprintf("  %s in-flight: %s limited, %s passthrough",
		formatCount(int64(len(flights))), formatCount(m.snap.InFlightLimited), formatCount(m.snap.InFlightPassthrough))
	if uniseg.StringWidth(summary) <= m.viewportWidth() {
		lines = append(lines, summary)
	} else {
		parts := []string{
			formatCount(int64(len(flights))) + " in-flight",
			formatCount(m.snap.InFlightLimited) + " limited",
			formatCount(m.snap.InFlightPassthrough) + " passthrough",
		}
		for _, row := range packParts(parts, max(m.viewportWidth()-2, 0), "  ") {
			lines = append(lines, "  "+row)
		}
	}
	// The path column shrinks per row so the row always fits the
	// viewport: the fixed overhead is the indent, tag, method, and
	// age, of which only the last two vary — the age renders up to
	// 18 cells for extreme durations (e.g. "2562047h47m16.854s")
	// and %-6s pads the method but never truncates it (OPTIONS is 7
	// cells) — so no fixed overhead heuristic can guarantee the fit.
	// A fixed 23-cell assumption (15 fixed + 8-cell age) overflows
	// for multi-hour ages: row = 15 + pathWidth + ageWidth with
	// pathWidth = vw-23 exceeds vw whenever ageWidth > 8 (e.g.
	// 41 > 39 at width 40 for "1h2m3.004s"). Instead each row's path
	// width is derived from that row's actual overhead: row = fixed +
	// pathWidth <= viewportWidth whenever the overhead itself fits
	// the viewport, which always holds at the supported widths
	// (40/80/120: the widest overhead, an 18-cell age with OPTIONS,
	// is 34 cells, leaving a 5-cell path at width 40), since
	// truncate limits ASCII paths to pathWidth cells and %-*s pads
	// to exactly pathWidth.
	show := min(len(flights), 6)
	for i := range show {
		r := flights[i]
		age := r.Age().Truncate(time.Millisecond)
		tag := m.styles.limitedTag
		if !r.Limited {
			tag = m.styles.passTag
		}
		fixed := 2 + uniseg.StringWidth(stripANSI(tag)) + 1 + max(6, uniseg.StringWidth(r.Method)) + 1 + 1 + uniseg.StringWidth(age.String())
		pathWidth := min(35, max(m.viewportWidth()-fixed, 0))
		lines = append(lines, fmt.Sprintf("  %s %-6s %-*s %s", tag, r.Method, pathWidth, truncate(r.Path, pathWidth), age))
	}
	if len(flights) > show {
		lines = append(lines, fmt.Sprintf("  … and %d more", len(flights)-show))
	}

	lines = append(lines, "")
	lines = append(lines, m.styles.sectionStyle.Render(" Summary "))
	// The Summary metrics are packed into rows that fit the viewport:
	// "Label: count" parts joined by "  │  ", each row indented by two
	// cells. A single part is at most 26 cells (the longest label,
	// "Clean passthrough", plus ": " and a 7-digit exact count), so
	// every rendered row provably fits viewports of 28 cells or more,
	// and any two parts (with the 5-cell separator) fit within a
	// 57-cell budget, so two metrics per row fit at viewports of 59
	// cells or more; the packer therefore renders multiple metrics per
	// row at standard widths and one or two per row at 40 columns.
	summaryMetrics := []struct {
		label string
		value int64
	}{
		{"Clean proxied", m.snap.TotalProxied},
		{"Clean passthrough", m.snap.TotalPassThrough},
		{"Aborted", m.snap.TotalAborted},
		{"Timeouts", m.snap.TotalTimeout},
		{"Cancelled", m.snap.TotalCancelled},
		{"Circuit rejects", m.snap.TotalCircuitRejected},
	}
	summaryParts := make([]string, 0, len(summaryMetrics))
	for _, sm := range summaryMetrics {
		summaryParts = append(summaryParts, sm.label+": "+formatCount(sm.value))
	}
	for _, row := range packParts(summaryParts, max(m.viewportWidth()-2, 0), "  \u2502  ") {
		lines = append(lines, "  "+row)
	}

	return lines
}

// cachedDashboardLines returns the dashboard lines for the current Update
// cycle, building them lazily on first access. The cache is reset only by
// data-mutating messages (metrics.Snapshot), terminal resizes
// (tea.WindowSizeMsg), and tab switches, so the expensive formatting work is
// skipped for high-frequency input messages such as mouse motion.
func (m *Model) cachedDashboardLines() []string {
	if m.dashboardLinesCache != nil {
		return m.dashboardLinesCache
	}
	m.dashboardLinesCache = m.dashboardLines()
	return m.dashboardLinesCache
}
