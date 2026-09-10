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
)

// logDetailAnchor pins an open Logs detail view to one concrete log item.
// seq is the ring sequence number the item had when detail mode was entered;
// it is the durable identity, because logRing assigns a unique, monotonically
// increasing sequence number to every accepted line and retains it until
// bounded eviction. text is the full line content. An anchor can survive
// evictions that are offset by duplicate text: it is considered present only
// while it can still be resolved in the current ring snapshot.
type logDetailAnchor struct {
	seq  uint64
	text string
}

// resolveAnchor maps an anchor to its current position in the visible log
// list and its content. It walks the ring snapshot in order, applies the
// active filter (so the position matches visibleLogLines and the overlay
// closes when the anchored line is filtered out), and selects the newest
// occurrence of the anchored text whose sequence number is not newer than
// the anchor's — i.e. the occurrence that was in place when the anchor was
// created. Eviction shifts every retained line's position down by one while
// preserving relative order, and duplicates make the exact sequence
// inaccessible, so the largest earlier sequence is the closest correct match.
// A nil result means the anchored item is gone and the overlay must close.
func (m Model) resolveAnchor(anchor logDetailAnchor) (int, string, bool) {
	if anchor.text == "" {
		return 0, "", false
	}
	items := m.logRing.snapshot()
	lower := strings.ToLower(m.filterText)
	pos := -1
	visibleCount := 0
	var text string
	for _, item := range items {
		if lower != "" && !strings.Contains(strings.ToLower(item.text), lower) {
			continue
		}
		if item.seq <= anchor.seq && item.text == anchor.text {
			pos = visibleCount
			text = item.text
		}
		visibleCount++
	}
	if pos < 0 {
		return 0, "", false
	}
	return pos, text, true
}

// logItemAtCursor returns the ring item the Logs cursor currently selects,
// resolved through the filtered list so it is the item the operator sees.
// nil when the cursor is out of range.
func (m *Model) logItemAtCursor() *logRingItem {
	lines := m.visibleLogLines()
	if m.cursor >= len(lines) {
		return nil
	}
	text := lines[m.cursor]
	for _, item := range m.logRing.snapshot() {
		if item.text == text {
			return &item
		}
	}
	return nil
}

// renderLogDetail renders a full-screen view of one log item, wrapped to the
// available width so long messages are readable without horizontal scrolling.
// index is the caller-computed 1-based list position. The output is clamped
// to the visible-row budget: a message wrapping past it is truncated with a
// "… N more lines" indicator, because the overlay has no vertical scrolling
// and an oversized frame would push the chrome into terminal scrollback.
func (m Model) renderLogDetail(text string, index int) string {
	vw := m.viewportWidth()
	var b strings.Builder
	b.WriteString(m.styles.sectionStyle.Render(fmt.Sprintf(" Log Line %d ", index)))
	b.WriteByte('\n')
	b.WriteByte('\n')
	rows := wrapText(stripANSI(text), max(vw-4, 1))
	// Budget: heading + blank + body + [indicator when clamped] + blank +
	// close hint, within visibleRows (5 fixed lines plus the indicator).
	budget := max(m.visibleRows()-5, 1)
	truncated := false
	if len(rows) > budget {
		rows = rows[:budget]
		truncated = true
	}
	for _, row := range rows {
		b.WriteString("  " + row)
		b.WriteByte('\n')
	}
	if truncated {
		b.WriteString(m.styles.dimStyle2.Render("  … more lines withheld (full text is in the log source)"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(" [Esc/Enter] close ")
	return b.String()
}

// detailStillPresent reports whether the anchored log item can still be
// resolved in the current ring snapshot. It exists so Update can close the
// overlay as soon as the underlying item is evicted or filtered out.
func (m Model) detailStillPresent() bool {
	_, _, ok := m.resolveAnchor(m.logDetailAnchor)
	return ok
}
