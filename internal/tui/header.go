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
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"
)

// providerName returns the display name for the active provider, prefixed with a
// single leading space. A single unnamed provider (and the zero Model) keep the
// legacy " ⚡ shaper" brand in the header.
func (m Model) providerName() string {
	if len(m.providers) == 0 {
		return " ⚡ shaper"
	}
	if len(m.providers) == 1 && m.providers[0].name == "" {
		return " ⚡ shaper"
	}
	return " " + m.providerLabel(m.active)
}

// providerLabel returns the bare display name of provider i, falling back to a
// stable "provider-N" label when it has no name.
func (m Model) providerLabel(i int) string {
	if i < 0 || i >= len(m.providers) {
		return ""
	}
	if name := m.providers[i].name; name != "" {
		return name
	}
	return fmt.Sprintf("provider-%d", i+1)
}

// hasSwitcher reports whether the header row renders provider chips: more than
// one provider, or a single provider with a name.
func (m Model) hasSwitcher() bool {
	return len(m.providers) > 1 || (len(m.providers) == 1 && m.providers[0].name != "")
}

// renderProviderSwitcher renders the provider chips shown on the right of the
// header in multi-provider mode. It returns "" when there is no switcher (the
// single unnamed provider — preserving the legacy header exactly — or when the
// width budget cannot fit even the active chip). The rendered string is exactly
// strings.Join(budgetedChips().parts, " ") — the same parts chipAt hit-tests,
// so what the user sees is what the user clicks.
func (m Model) renderProviderSwitcher() string {
	if !m.hasSwitcher() {
		return ""
	}
	rows := m.chipRowsLayout()
	if len(rows) == 0 {
		return ""
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = strings.Join(r.parts, " ")
	}
	return strings.Join(lines, "\n")
}

// headerBody renders the header's left side: provider identity and live
// counters, truncated to never exceed the usable row width on its own. It is
// shared by renderHeader and budgetedChips so the budget the chips degrade
// against is computed from the exact body the row displays. The '✗' (U+2717)
// and '⚡' (U+26A1) glyphs occupy 1 and 2 cells respectively.
//
// The truncation happens on the plain string, BEFORE headerStyle wraps it:
// truncateANSI would append ESC[0m inside the styled row and kill
// headerStyle's background for the remainder of the line.
//
// fleetSummary returns the one-line fleet aggregate observability strip
// (M6/G8) summarizing active, queued, open breaker counts, and the busiest
// provider across all configured providers.
// identityWidth returns the cell width of the active-provider identity
// segment (provider name + scroll affordance) used for mouse-wheel
// hit-testing in the header.
func (m Model) identityWidth() int {
	if !m.hasSwitcher() {
		return 0
	}
	// " " + label + " ↕"
	return lipgloss.Width(" " + m.providerLabel(m.active) + " ↕")
}

// fleetStats returns the aggregate observability strip without the legacy
// "Fleet:" prefix, since the active provider identity already shows which
// dashboard is selected.
func (m Model) fleetStats() string {
	var totalActive, totalQueued int64
	var openBreakers int
	var maxActive int64 = -1
	var maxThroughput float64 = -1
	var busiest string

	for i, p := range m.providers {
		totalActive += p.snap.Active
		totalQueued += p.snap.Queued
		if p.snap.CircuitBreaker != nil && p.snap.CircuitBreaker.State == "OPEN" {
			openBreakers++
		}
		label := m.providerLabel(i)
		if p.snap.Active > maxActive || (p.snap.Active == maxActive && p.snap.Throughput > maxThroughput) {
			maxActive = p.snap.Active
			maxThroughput = p.snap.Throughput
			busiest = label
		}
	}

	return fmt.Sprintf("%d active · %d queued · %d OPEN · busiest: %s",
		totalActive, totalQueued, openBreakers, busiest)
}

func (m Model) headerBody(reserveForSwitcher bool) string {
	var body string
	if len(m.providers) > 1 {
		// In fleet mode, show the active provider identity with a scroll
		// affordance (↕), followed by aggregate stats.
		body = fmt.Sprintf(" %s ↕ │ %s", m.providerLabel(m.active), m.fleetStats())
	} else {
		uptime := time.Since(m.startTime).Truncate(time.Second)
		body = fmt.Sprintf("%s │ %d/%d active │ %d queued │ %.1f req/s │ %d ✗ TO │ uptime %s",
			m.providerName(), m.snap.Active, m.conc, m.snap.Queued, m.snap.Throughput,
			m.snap.TotalTimeout, uptime)
	}
	usable := max(m.width-2, 1)
	cap := usable
	if reserveForSwitcher && m.hasSwitcher() {
		// 1 gap + the active chip's floor. Any chip that fits beyond the
		// active one only shrinks this body further via budgetedChips'
		// budget computation, which uses this same reserve.
		cap -= 1 + chipFloor
		if cap < 1 {
			cap = 1
		}
	}
	if lipgloss.Width(body) > cap {
		body = truncatePlain(body, cap)
	}
	return body
}

// chipFloor is the smallest usable chip width: one visible label character
// plus the one-cell padding on each side of it.
const chipFloor = 3

// chipLayout is the outcome of one width-budgeting decision: the rendered chip
// parts in display (provider) order and, in lockstep, the provider index each
// part belongs to. parts and providers always have equal length; a nil parts
// means the switcher is elided for this width.
type chipLayout struct {
	parts     []string
	providers []int
}

// budgetedChips is the single source of truth for the provider switcher at a
// given width: renderHeader displays its parts and chipAt hit-tests exactly
// those parts, so the visible layout and the click targets cannot disagree.
//
// Chips are joined with single spaces and right-aligned in renderHeader; the
// usable content width is m.width-2 (headerStyle pads 1 cell each side) and 1
// more cell is reserved as the visual gap before the switcher:
//
//	budget := m.width - 2 - lipgloss.Width(headerBody(true)) - 1
//
// headerBody(true) itself reserves 1+chipFloor cells for the active chip, so
// budget >= chipFloor holds whenever the width can host any chip at all; a
// smaller budget elides the switcher (rule 3 below) and providerName carries
// the identity.
//
// Chips degrade to fit that budget, in order:
//
//  1. Shorten: chip labels are truncated to their allotted cells via
//     truncateANSI, preserving chip styling (the appended reset cannot leak
//     past the chip because each chip's styles re-open on the next chip).
//  2. Drop: trailing (leftmost-displayed) chips are dropped entirely.
//     Providers are never removed from the model — only from this row — and
//     Tab/Shift+Tab keep cycling the full set. The ACTIVE chip is never
//     dropped, so the user always sees which dashboard they are on; under a
//     tight budget that can mean evicting inactive chips that would otherwise
//     fit to its left.
//  3. Elide: if even the active chip at its floor width exceeds the budget,
//     the whole switcher is dropped and providerName carries the identity.
//
// Slack left after the floor widths are covered is restored to the chips'
// full rendered widths — active chip first, then the rest left-to-right — so
// the active chip's label is the most legible one on the row. A chip whose
// slack is fully restored renders byte-identically to an unbudgeted chip.
// budgetedChips returns the first row of chipRowsLayout for backward
// compatibility with existing tests.
func (m Model) budgetedChips() chipLayout {
	rows := m.chipRowsLayout()
	if len(rows) == 0 {
		return chipLayout{}
	}
	return rows[0]
}

// chipRowsLayout computes a multi-row chip layout. Row 0 shares space with
// the header body; subsequent rows get the full header width (m.width-2).
// Chips are packed greedily in provider order. The active chip is never
// dropped. Returns one chipLayout per row. Row 0 is the body row; when the
// body leaves no room for a chip (row0Budget < chipFloor) the returned slice
// starts with an empty row 0 so renderHeader leaves the body untouched and
// chips begin on the next line. The layout is capped to max(m.height-4,1)
// rows so headerRowCount and renderHeader stay in lockstep.
func (m Model) chipRowsLayout() []chipLayout {
	if !m.hasSwitcher() {
		return nil
	}

	labels := make([]string, len(m.providers))
	natural := make([]int, len(m.providers))
	for i := range m.providers {
		labels[i] = " " + m.providerLabel(i) + " "
		natural[i] = lipgloss.Width(m.styles.chipActiveStyle.Render(labels[i]))
	}

	row0Budget := m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
	fullBudget := m.width - 2
	maxRows := max(m.height-4, 1)

	if fullBudget < chipFloor {
		return nil // elide entirely — terminal too narrow for any chip
	}

	// floorCost returns the minimum width for a set of chips on one row.
	floorCost := func(indices []int) int {
		if len(indices) == 0 {
			return 0
		}
		return len(indices)*chipFloor + (len(indices) - 1)
	}

	// Greedy packing: fill rows left-to-right in provider order.
	type rowPlan struct {
		indices []int
		budget  int
	}
	var rows []rowPlan
	currentIndices := make([]int, 0)
	var currentBudget int

	if row0Budget < chipFloor {
		if maxRows == 1 {
			return nil // no room for a dedicated chip row
		}
		// Reserve row 0 for the body only; chips start on row 1.
		rows = append(rows, rowPlan{indices: nil, budget: 0})
		currentBudget = fullBudget
	} else {
		currentBudget = row0Budget
	}

	flushRow := func() {
		if len(currentIndices) > 0 {
			rows = append(rows, rowPlan{indices: currentIndices, budget: currentBudget})
			currentIndices = make([]int, 0)
			currentBudget = fullBudget
		}
	}

	for i := range m.providers {
		candidate := append(append([]int{}, currentIndices...), i)
		if floorCost(candidate) <= currentBudget {
			currentIndices = candidate
		} else {
			flushRow()
			currentIndices = []int{i}
			currentBudget = fullBudget
			// If it still doesn't fit on a full row, it gets truncated later.
		}
	}
	flushRow()

	// Cap to the height budget before guaranteeing the active provider.
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}

	// Ensure the active chip is present. Greedy packing already places it,
	// but truncation by maxRows can drop it. In that case inject it into
	// the last row, evicting peers until the budget is satisfied.
	hasActive := false
	for _, r := range rows {
		if slices.Contains(r.indices, m.active) {
			hasActive = true
		}
	}
	if !hasActive && len(rows) > 0 {
		last := len(rows) - 1
		r := rows[last]
		r.indices = append(r.indices, m.active)
		for len(r.indices) > 1 && floorCost(r.indices) > r.budget {
			// Evict the rightmost inactive chip (penultimate) to keep the
			// newly appended active chip.
			idx := len(r.indices) - 2
			r.indices = append(r.indices[:idx], r.indices[idx+1:]...)
		}
		rows[last] = r
	}

	// Build chipLayout for each row.
	result := make([]chipLayout, len(rows))
	for ri, r := range rows {
		widths := make([]int, len(r.indices))
		for k := range r.indices {
			widths[k] = chipFloor
		}
		slack := max(r.budget-floorCost(r.indices), 0)
		// Restore slack: active chip first, then rest left-to-right.
		order := make([]int, 0, len(r.indices))
		for k, idx := range r.indices {
			if idx == m.active {
				order = append(order, k)
			}
		}
		for k, idx := range r.indices {
			if idx != m.active {
				order = append(order, k)
			}
		}
		for _, k := range order {
			for slack > 0 && widths[k] < natural[r.indices[k]] {
				widths[k]++
				slack--
			}
		}

		parts := make([]string, len(r.indices))
		for k, idx := range r.indices {
			if idx == m.active {
				parts[k] = truncateANSI(m.styles.chipActiveStyle.Render(labels[idx]), widths[k])
			} else {
				parts[k] = truncateANSI(m.styles.chipInactiveStyle.Render(labels[idx]), widths[k])
			}
		}
		result[ri] = chipLayout{parts: parts, providers: r.indices}
	}
	return result
}

// chipAt maps a click at (mx, my) to a provider chip index. It uses
// chipRowsLayout so hit-testing matches the rendered geometry exactly.
// Row 0 chips are right-aligned after the header body; row 1+ chips are
// right-aligned in the full header width (m.width-2).
func (m Model) chipAt(mx, my int) (int, bool) {
	if !m.hasSwitcher() {
		return 0, false
	}
	rows := m.chipRowsLayout()
	if my < 0 || my >= len(rows) {
		return 0, false
	}
	layout := rows[my]
	if len(layout.parts) == 0 {
		return 0, false
	}

	// All chip rows are right-aligned within the full header width (m.width-2).
	// renderHeader pads row 0's body to push chips to the same right edge.
	right := m.width - 2

	for i, v := range slices.Backward(layout.parts) {
		w := lipgloss.Width(v)
		if mx >= right-w+1 && mx <= right {
			return layout.providers[i], true
		}
		right -= w + 1
	}
	return 0, false
}

func (m Model) renderHeader() string {
	body := m.headerBody(m.hasSwitcher())
	if !m.hasSwitcher() {
		return m.styles.headerStyle.Render(body)
	}
	rows := m.chipRowsLayout()
	if len(rows) == 0 {
		return m.styles.headerStyle.Render(body)
	}

	// Row 0: body + right-aligned chips.
	row0Chips := strings.Join(rows[0].parts, " ")
	row0Body := body
	if row0Chips != "" {
		if pad := m.width - lipgloss.Width(row0Body) - lipgloss.Width(row0Chips) - 3; pad > 0 {
			row0Body += strings.Repeat(" ", pad)
		}
		row0Body += " " + row0Chips
	}

	var lines []string
	lines = append(lines, m.styles.headerStyle.Render(row0Body))

	// Row 1+: full-width, right-aligned chips.
	for _, r := range rows[1:] {
		chips := strings.Join(r.parts, " ")
		if pad := m.width - lipgloss.Width(chips) - 2; pad > 0 {
			chips = strings.Repeat(" ", pad) + chips
		}
		lines = append(lines, m.styles.headerStyle.Render(chips))
	}

	return strings.Join(lines, "\n")
}

// renderTab renders a single tab label using the theme. The selected tab
// (tabID(i) == m.tab) uses tabActiveStyle; all others use tabInactiveStyle.
// Both styles share the same horizontal box model (PaddingLeft(1).PaddingRight(1)),
// differing only in color and weight, so the rendered widths are equal for a
// given label — but the measurement MUST come from the same code path that
// renderTabBar uses, so hit-testing (tabAt) tracks the exact pixels on screen.
func (m Model) renderTab(i int, name string) string {
	if tabID(i) == m.tab {
		return m.styles.tabActiveStyle.Render(" " + name + " ")
	}
	return m.styles.tabInactiveStyle.Render(" " + name + " ")
}

func (m Model) renderTabBar() string {
	parts := make([]string, len(tabNames))
	for i, name := range tabNames {
		parts[i] = m.renderTab(i, name)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// tabAt returns which tab, if any, occupies cell x on the tab bar row.
// It measures each tab's visible cell width as rendered by the current theme
// (padding included) so hit-testing tracks the exact pixels the user sees,
// not a ratio of terminal width. The tab bar is left-aligned at x=0 with no
// reflow; cells beyond the total bar width are empty space and return !ok.
func (m Model) tabAt(x int) (tabID, bool) {
	if x < 0 {
		return 0, false
	}
	offset := 0
	for i, name := range tabNames {
		// Measure using renderTab so hit-testing tracks the exact rendered
		// geometry — the selected tab uses tabActiveStyle, others use
		// tabInactiveStyle. Both share the same padding, but rendering must
		// never hard-code one style for all tabs.
		rendered := m.renderTab(i, name)
		w := uniseg.StringWidth(stripANSI(rendered))
		if w <= 0 {
			// Defensive fallback: if a style is zero-value (Render returns the
			// raw input), the width is len(content)+2 padding = name+4.
			// This branch is effectively unreachable because " "+name+" " is
			// always at least 3 cells, but guards against impossible states.
			w = uniseg.StringWidth(name) + 4
		}

		if x >= offset && x < offset+w {
			return tabID(i), true
		}
		offset += w
	}
	return 0, false
}

// renderDashboardContent returns the portion of the dashboard that is visible
