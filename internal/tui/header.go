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
	layout := m.budgetedChips()
	return strings.Join(layout.parts, " ")
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
func (m Model) fleetSummary() string {
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

	return fmt.Sprintf("Fleet: %d active · %d queued · %d OPEN · busiest: %s",
		totalActive, totalQueued, openBreakers, busiest)
}

func (m Model) headerBody(reserveForSwitcher bool) string {
	var body string
	if len(m.providers) > 1 {
		body = " " + m.fleetSummary()
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
func (m Model) budgetedChips() chipLayout {
	if !m.hasSwitcher() {
		return chipLayout{}
	}
	budget := m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
	if budget < chipFloor {
		return chipLayout{}
	}

	labels := make([]string, len(m.providers))
	natural := make([]int, len(m.providers))
	for i := range m.providers {
		labels[i] = " " + m.providerLabel(i) + " "
		// natural is the chip's full RENDERED width — label plus the chip
		// style's 1+1 padding cells — not the bare label width, so a chip
		// whose slack is fully restored renders byte-identically to an
		// unbudgeted chip instead of being permanently truncated by the
		// two padding cells. Either chip style yields the same width here:
		// both add exactly one padding cell per side, and bold/colour
		// sequences carry no width.
		natural[i] = lipgloss.Width(m.styles.chipActiveStyle.Render(labels[i]))
	}

	// total returns the floor cost of exactly these providers as a chip row:
	// chipFloor per chip plus one gap between adjacent chips.
	total := func(keep []int) int {
		return len(keep)*chipFloor + (len(keep) - 1)
	}

	// Select kept chips right-to-left at floor width.
	keep := make([]int, 0, len(labels))
	prefix := func(i int, rest []int) []int {
		out := make([]int, 0, len(rest)+1)
		out = append(out, i)
		out = append(out, rest...)
		return out
	}
	kept := func(i int) bool {
		return slices.Contains(keep, i)
	}
	for i := range slices.Backward(labels) {
		candidate := prefix(i, keep)
		if total(candidate) <= budget {
			keep = candidate
			continue
		}
		if i != m.active {
			break // rule 2: trailing chip does not fit, stop walking left
		}
		// The active chip must be kept: evict the leftmost kept chips until
		// the row fits (the active chip wins over any number of inactive
		// ones to its left).
		for len(keep) > 0 && total(candidate) > budget {
			keep = keep[1:]
			candidate = prefix(i, keep)
		}
		if total(candidate) > budget {
			return chipLayout{} // unreachable: budget >= chipFloor
		}
		keep = candidate
	}
	// The walk can stop left of the active chip's position (rule 2 break),
	// leaving the ACTIVE provider unkept; insert it at its display position,
	// then evict inactive chips (from either end) until the row fits — the
	// active chip always wins.
	if !kept(m.active) {
		pos := 0
		for pos < len(keep) && keep[pos] < m.active {
			pos++
		}
		keep = append(keep[:pos], append([]int{m.active}, keep[pos:]...)...)
		for len(keep) > 1 && total(keep) > budget {
			// Evict the rightmost inactive chip; the active chip is never
			// dropped.
			last := len(keep) - 1
			if keep[last] == m.active {
				last = 0 // only the leftmost remains to evict
			}
			keep = append(keep[:last], keep[last+1:]...)
		}
		if total(keep) > budget {
			return chipLayout{} // unreachable: budget >= chipFloor
		}
	}

	// Restore slack toward natural widths: the active chip first, then the
	// remaining kept chips left-to-right.
	widths := make([]int, len(keep))
	for k := range keep {
		widths[k] = chipFloor
	}
	slack := budget - total(keep)
	order := make([]int, 0, len(keep))
	for k, i := range keep {
		if i == m.active {
			order = append(order, k)
		}
	}
	for k := range keep {
		if keep[k] != m.active {
			order = append(order, k)
		}
	}
	for _, k := range order {
		for slack > 0 && widths[k] < natural[keep[k]] {
			widths[k]++
			slack--
		}
	}

	parts := make([]string, len(keep))
	for k, i := range keep {
		if i == m.active {
			parts[k] = truncateANSI(m.styles.chipActiveStyle.Render(labels[i]), widths[k])
		} else {
			parts[k] = truncateANSI(m.styles.chipInactiveStyle.Render(labels[i]), widths[k])
		}
	}
	return chipLayout{parts: parts, providers: keep}
}

// chipAt maps a click column on header row 0 to a provider chip index,
// following the same right-aligned layout renderHeader displays: the row's
// content spans m.width-2 usable cells, so the last chip ends at column
// m.width-2 and each chip occupies [x-w+1, x] for its rendered width w. Only
// chips that survive the width budget are hit-testable — a dropped chip is
// invisible and must not be clickable.
func (m Model) chipAt(mx int) (int, bool) {
	if !m.hasSwitcher() {
		return 0, false
	}
	layout := m.budgetedChips()
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
	// With a switcher, render with the reserved body so the truncated body
	// the chips were budgeted against is the one displayed; without one,
	// render the natural body capped at the usable width (single-provider
	// rows keep their legacy content; only a too-narrow terminal truncates).
	body := m.headerBody(m.hasSwitcher())
	if switcher := m.renderProviderSwitcher(); switcher != "" {
		// Right-align the switcher inside the header content: headerStyle
		// adds 1 cell of padding on each side, so the usable body width is
		// m.width-2; the extra cell is a visual gap before the chips.
		if pad := m.width - lipgloss.Width(body) - lipgloss.Width(switcher) - 3; pad > 0 {
			body += strings.Repeat(" ", pad)
		}
		body += " " + switcher
	}
	return m.styles.headerStyle.Render(body)
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
