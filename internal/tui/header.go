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

// Fleet header layout invariants (fleet / multi-provider mode, -tui).
//
// Usable width is m.width-2 (headerStyle pads 1 cell each side). The body is
// the fleet identity + aggregate stats rendered by headerBody; the chips are
// " "+label+" " rendered with chipActiveStyle / chipInactiveStyle. Natural
// widths are measured with lipgloss.Width on the styled strings (headerBody
// without reservation for the body, chipActiveStyle.Render(label) for chips).
//
// Invariant — first-chip-truncated => wrap-all (empty row 0):
//
//	Row 0 may contain chips only when the first provider's chip would render
//	at its natural width without truncation. After measuring body natural and
//	chip naturals, greedily pack the maximal prefix k of providers that fit
//	naturally alongside the body: sum(natural[0:k])+(k-1)+1+bodyWidth(k) <=
//	usable, where bodyWidth(k) is the truncated body that still fits. If no
//	chip fits naturally (available < natural[0]), row 0 is body-only: an empty
//	chipLayout (no providers) so renderHeader draws the body at the full
//	usable width (headerBody(false) cap) and every provider chip begins on
//	row 1+. Truncation, if unavoidable at ultra-narrow widths, is therefore
//	distributed across a full-width chip row and never leaves a lone
//	single-letter orphan on row 0. For rows where k>0 the row-0 chips are
//	allocated their exact natural widths (defensive truncateANSI is identity);
//
// Wrapped rows (row 1+) use fullBudget = usable and greedy floorCost packing
// (floorCost = n*chipFloor+(n-1)), with the existing slack-restoration order
// (active chip first, then left-to-right) and truncateANSI — truncation may
// still occur there at ultra-narrow widths but never on row 0;
//
// Height cap maxRows = max(height-4,1) governs both chipRowsLayout and
// headerRowCount / renderHeader; if capping would drop the active chip it is
// injected into the last row (evicting peers as needed). chipRowsLayout is
// the single source of truth for both renderHeader and chipAt, and
// headerRowCount/contentStartRow/visibleRows stay in lockstep.
//
// Breakpoints are dynamic (natural-fit), not fixed width thresholds:
//
//	The shared-row vs wrapped-row transition is derived from whether chips fit
//	naturally beside the fleet identity body, not from a fixed chipFloor
//	reserve. Representative behaviour (height 24, 3 long names
//	anthropic-eu-central / openai-prod-longname / acme-edge-provider,
//	body ~86 cells natural, chip naturals ~24/24/22):
//	  ~20 cols (usable 18): row 0 body-only (truncated to usable), row 1
//	     holds chips at floor widths; hrc=2. At 20-40 cols the header is
//	     always body-only on row 0, chips on row 1+.
//	  ~40 cols (usable 38): row 0 body-only, row 1 holds 2-3 chips at
//	     reduced but still legible widths (> chipFloor singleton).
//	  ~80 cols (usable 78): row 0 body-only for the 3-long fleet; row 1
//	     holds chips full-width. For a 2-short fleet (acme/anthropic,
//	     body ~54, naturals 8+13) 80 cols already fits single-row
//	     (body+chips naturally, hrc=1).
//	  ~100 cols (usable 98): 3-long remains body-only on row 0 (hrc=2,
//	     available 11 < 24), 2-short single-row.
//	  ~120 cols (usable 118): 3-long shares one chip on row 0 (first chip
//	     24 fits in available 31) and wraps remaining 2 to row 1 (hrc=2);
//	     2-short is single-row with spare slack. This row is falsifiable: at
//	     120 cols with 2 short providers the header MUST be single-row at
//	     natural widths; at 120 cols with 3-long the header MUST have row 0
//	     chip at natural width (no truncation on row 0).
//	  ~150 cols (usable 148): 3-long shares two chips on row 0 (available
//	     61 fits 24+24+1) and wraps one to row 1 (hrc=2).
//	  ~180 cols (usable 178): 3-long collapses to single-row body+all chips
//	     at natural widths (hrc=1). Narrower 3-long fleets collapse earlier
//	     when the body is shorter or provider count is smaller.
//
// Falsifiable checks: at 20-30 cols headerRowCount is body-only row 0 plus
// chip row(s); first visible chip width equals its natural when row 0 holds
// chips. At 120 cols with 2 short providers chipRowsLayout is single row,
// each chip at natural width, header never exceeds width.

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
// counters, truncated to never exceed the usable row width on its own. The
// fleet header's chip placement is decided by chipRowsLayout via natural-fit
// (see fleet invariants); headerBody itself always caps to the full usable
// width. The '✗' (U+2717) and '⚡' (U+26A1) glyphs occupy 1 and 2 cells
// respectively.
//
// The truncation happens on the plain string, BEFORE headerStyle wraps it:
// truncateANSI would append ESC[0m inside the styled row and kill
// headerStyle's background for the remainder of the line.
//
// identityWidth returns the cell width of the active-provider identity
// segment (provider name + scroll affordance) used for mouse-wheel and
// click hit-testing in the header. It is the natural
// lipgloss.Width(" "+label+" ↕") independent of headerBody truncation, so
// at narrow widths the hit region may extend beyond the visible truncated
// prefix but stays consistent between wheel and click and is always bounded
// by the terminal width.
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

// healthColor returns the hex color for the brand transducer glyph based on
// current engine health: nominal (cyan/emerald), backpressure (amber), or
// breaker-open fault (coral).
func (m Model) healthColor() string {
	var openBreakers int
	var totalQueued int64
	for _, p := range m.providers {
		if p.snap.CircuitBreaker != nil && p.snap.CircuitBreaker.State == "OPEN" {
			openBreakers++
		}
		totalQueued += p.snap.Queued
	}
	if openBreakers > 0 {
		return "#FF453A"
	}
	if totalQueued > 0 {
		return "#F59E0B"
	}
	return "#00F5FF"
}

// renderBrand returns the 11-cell brand block "⎎ AI·SHAPER" with the
// transducer glyph colored by engine health, followed by the divider and
// fleet metrics. Every cell carries the header bar background so that SGR
// resets between styled fragments cannot create background holes. The
// result is capped to the usable header width.
func (m Model) renderBrand() string {
	healthHex := m.healthColor()
	barBG := lipgloss.Color(m.styles.brandBarBG)
	bezelBG := lipgloss.Color(m.styles.brandBezelBG)

	glyph := lipgloss.NewStyle().
		Foreground(lipgloss.Color(healthHex)).
		Background(bezelBG).
		Bold(true).
		Render("⎎")
	ai := m.styles.brandAIStyle.Render("AI")
	dot := m.styles.brandDotStyle.Render("·")
	name := m.styles.brandNameStyle.Render("SHAPER")
	bezelPad := lipgloss.NewStyle().Background(bezelBG).Render(" ")
	barPad := lipgloss.NewStyle().Background(barBG).Render(" ")
	brand := bezelPad + glyph + bezelPad + ai + dot + name

	sep := m.styles.brandSepStyle.Background(barBG).Render("│")
	identity := m.styles.providerHueStyle(m.active).Background(barBG).Render(" " + m.providerLabel(m.active) + " ↕")
	sep2 := lipgloss.NewStyle().Foreground(m.styles.brandSepStyle.GetForeground()).Background(barBG).Render("│")
	metrics := lipgloss.NewStyle().Background(barBG).Render(m.fleetStats())
	body := brand + sep + identity + barPad + sep2 + barPad + metrics

	usable := max(m.width-2, 1)
	if lipgloss.Width(body) > usable {
		body = truncateANSI(body, usable)
	}
	return body
}

// headerBarFill renders run cells of the header bar background. It is used
// for the unstyled runs renderHeader adds around the ANSI-styled fleet body
// (alignment gutters and chip gaps): the body's fragments end in SGR resets,
// so any plain space appended after them would fall back to the terminal's
// default background and punch a visible hole in the bar.
func (m Model) headerBarFill(run int) string {
	if run <= 0 {
		return ""
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color(m.styles.brandBarBG)).
		Render(strings.Repeat(" ", run))
}

// providerHueStyle returns a lipgloss.Style with the provider hue as
// foreground, used for the ↕ identity. It inherits the header bar
// background from the surrounding row rather than setting its own.
func (t tuiTheme) providerHueStyle(i int) lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(t.providerHue(i))).
		Bold(true)
}

// brandPrefixWidth returns the visible cell width of the brand block
// (" ⎎ AI·SHAPER") excluding the separator and identity. This is the
// fixed anchor width for brand hit-testing and identity offset.
func brandPrefixWidth() int {
	return lipgloss.Width(" ⎎ AI·SHAPER")
}

// identityOffset returns the screen column where the identity segment
// (" <label> ↕") begins in the fleet header body. It accounts for
// headerStyle PaddingLeft(1), the brand block, and the separator.
func (m Model) identityOffset() int {
	if !m.hasSwitcher() {
		return 1
	}
	return 1 + brandPrefixWidth() + 1 // padding + brand + separator
}

func (m Model) headerBody(reserveForSwitcher bool) string {
	// reserveForSwitcher is retained for compatibility but no longer affects
	// the cap — the fleet header now decides whether chips share row 0 via
	// natural-fit in chipRowsLayout (see package invariants). The body is
	// always capped to the full usable width.
	_ = reserveForSwitcher
	body := m.rawBody()
	usable := max(m.width-2, 1)
	if lipgloss.Width(body) > usable {
		body = truncatePlain(body, usable)
	}
	return body
}

// rawBody returns the untruncated header body string, before any width cap.
// It is the source for naturalBodyWidth measurements in chipRowsLayout.
func (m Model) rawBody() string {
	if len(m.providers) > 1 {
		return m.renderBrand()
	}
	uptime := time.Since(m.startTime).Truncate(time.Second)
	return fmt.Sprintf("%s │ %d/%d active │ %d queued │ %.1f req/s │ %d ✗ TO │ uptime %s",
		m.providerName(), m.snap.Active, m.conc, m.snap.Queued, m.snap.Throughput,
		m.snap.TotalTimeout, uptime)
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

// budgetedChips returns the visible provider chips that share row 0 with
// the fleet identity body, when any do, as computed by chipRowsLayout. It is
// retained for compatibility and for tests that assert on the first row
// alone — renderHeader and chipAt both consume the full chipRowsLayout
// directly, so the visible layout and the click targets cannot disagree.
// Row 0 holds a maximal prefix of providers that fit naturally beside the
// body (see fleet header invariants); if none fits, row 0 is empty and
// budgetedChips is empty. Chips on row 0 are at natural widths, while
// wrapped rows (row 1+) degrade via floorCost and active-first slack
// restoration (truncateANSI). The active chip is never dropped; if even a
// floor-width chip cannot fit on any row the switcher is elided and
// providerName carries the identity.
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
// first chip would need truncation (natural-fit fails) the returned slice
// starts with an empty row 0 so renderHeader leaves the body at full usable
// width and every provider chip begins on row 1+ — the first-chip-truncated
// => wrap-all invariant (see package doc). The layout is capped to
// max(m.height-4,1) rows so headerRowCount and renderHeader stay in lockstep.
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

	fullBudget := m.width - 2
	maxRows := max(m.height-4, 1)

	if fullBudget < chipFloor {
		return nil // elide entirely — terminal too narrow for any chip
	}

	usable := max(m.width-2, 1)
	bodyW := lipgloss.Width(m.headerBody(false))
	available := usable - bodyW - 1

	// Natural-fit: maximal prefix k that fits alongside the body at its
	// natural (or usable-capped) width without truncating any chip. If the
	// first chip does not fit, k stays 0 and row 0 becomes body-only.
	k := 0
	sum := 0
	for k < len(natural) {
		nextSum := sum + natural[k]
		nextGaps := k // gaps for k+1 chips is k
		if nextSum+nextGaps <= available {
			sum = nextSum
			k++
		} else {
			break
		}
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

	if k == 0 {
		if maxRows == 1 {
			return nil // no room for a dedicated chip row
		}
		// Body-only row 0; every chip starts on row 1+ at full width.
		rows = append(rows, rowPlan{indices: nil, budget: 0})
	} else {
		indices := make([]int, k)
		for i := 0; i < k; i++ {
			indices[i] = i
		}
		// Row 0 chips are at natural widths; budget is their total natural
		// width plus gaps so width allocation restores exactly to natural.
		rows = append(rows, rowPlan{indices: indices, budget: sum + (k - 1)})
	}

	// Pack the remaining providers (k .. n-1) greedily by floorCost.
	currentIndices := make([]int, 0)
	currentBudget := fullBudget

	flushRow := func() {
		if len(currentIndices) > 0 {
			rows = append(rows, rowPlan{indices: currentIndices, budget: currentBudget})
			currentIndices = make([]int, 0)
			currentBudget = fullBudget
		}
	}

	for i := k; i < len(m.providers); i++ {
		candidate := append(append([]int{}, currentIndices...), i)
		if floorCost(candidate) <= currentBudget {
			currentIndices = candidate
		} else {
			flushRow()
			currentIndices = []int{i}
			currentBudget = fullBudget
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
		// Restore slack round-robin in provider order so no single chip
		// monopolizes the row width. Each pass distributes one cell to
		// each chip that has not yet reached its natural width.
		for slack > 0 {
			distributed := false
			for k := range r.indices {
				if slack <= 0 {
					break
				}
				if widths[k] < natural[r.indices[k]] {
					widths[k]++
					slack--
					distributed = true
				}
			}
			if !distributed {
				break
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

	// Row 0: body + right-aligned chips. The gutter between the body and the
	// chips carries the bar background (see headerBarFill).
	row0Chips := strings.Join(rows[0].parts, m.headerBarFill(1))
	row0Body := body
	if row0Chips != "" {
		gutter := 1
		if pad := m.width - lipgloss.Width(row0Body) - lipgloss.Width(row0Chips) - 3; pad > 0 {
			gutter += pad
		}
		row0Body += m.headerBarFill(gutter) + row0Chips
	}

	var lines []string
	lines = append(lines, m.styles.headerStyle.Render(row0Body))

	// Row 1+: full-width, right-aligned chips.
	for _, r := range rows[1:] {
		chips := strings.Join(r.parts, m.headerBarFill(1))
		if pad := m.width - lipgloss.Width(chips) - 2; pad > 0 {
			chips = m.headerBarFill(pad) + chips
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
