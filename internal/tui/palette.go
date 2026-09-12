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
	"charm.land/lipgloss/v2"
)

// paletteCmdKind identifies what a command palette entry does when selected.
type paletteCmdKind int

const (
	cmdSwitchProvider paletteCmdKind = iota
	cmdSwitchTab
	cmdShowHelp
	cmdResetStats
)

// paletteCmd is one selectable entry in the command palette.
type paletteCmd struct {
	label string
	kind  paletteCmdKind
	arg   int
}

// paletteState is the live state of the command palette overlay.
type paletteState struct {
	query  string
	cursor int
	cmds   []paletteCmd
}

// paletteCommands builds the full unfiltered command list.
func (m Model) paletteCommands() []paletteCmd {
	var cmds []paletteCmd
	for i := range m.providers {
		label := m.providerLabel(i)
		if i == m.active {
			label += " (active)"
		}
		cmds = append(cmds, paletteCmd{label: "Switch to: " + label, kind: cmdSwitchProvider, arg: i})
	}
	for i, name := range tabNames {
		cmds = append(cmds, paletteCmd{label: "Tab: " + strings.TrimSpace(name), kind: cmdSwitchTab, arg: i})
	}
	cmds = append(cmds,
		paletteCmd{label: "Show help", kind: cmdShowHelp},
		paletteCmd{label: "Reset stats", kind: cmdResetStats},
	)
	return cmds
}

// fuzzyMatch reports whether query matches label as a case-insensitive
// ordered subsequence. An empty query matches everything.
func fuzzyMatch(query, label string) bool {
	qRunes := []rune(strings.ToLower(query))
	if len(qRunes) == 0 {
		return true
	}
	qi := 0
	for _, lr := range strings.ToLower(label) {
		if lr == qRunes[qi] {
			qi++
			if qi == len(qRunes) {
				return true
			}
		}
	}
	return false
}

// openPalette enters the command palette.
func (m *Model) openPalette() {
	m.mode = modePalette
	m.palette.query = ""
	m.palette.cursor = 0
	m.palette.cmds = m.paletteCommands()
}

// closePalette returns to browse mode.
func (m *Model) closePalette() {
	m.mode = modeBrowse
	m.palette = paletteState{}
}

// filterPaletteCmds re-runs the query filter.
func (m *Model) filterPaletteCmds() {
	var filtered []paletteCmd
	for _, c := range m.paletteCommands() {
		if fuzzyMatch(m.palette.query, c.label) {
			filtered = append(filtered, c)
		}
	}
	m.palette.cmds = filtered
	if m.palette.cursor >= len(filtered) {
		m.palette.cursor = max(len(filtered)-1, 0)
	}
}

// movePaletteCursor shifts the palette cursor by delta.
func (m *Model) movePaletteCursor(delta int) {
	if len(m.palette.cmds) == 0 {
		return
	}
	m.palette.cursor += delta
	if m.palette.cursor < 0 {
		m.palette.cursor = 0
	}
	if m.palette.cursor > len(m.palette.cmds)-1 {
		m.palette.cursor = len(m.palette.cmds) - 1
	}
}

// executePaletteCmd runs the selected palette entry.
func (m *Model) executePaletteCmd(cmd paletteCmd) tea.Cmd {
	switch cmd.kind {
	case cmdSwitchProvider:
		m.switchProvider(cmd.arg)
		m.closePalette()
	case cmdSwitchTab:
		m.switchTab(tabID(cmd.arg))
		m.closePalette()
	case cmdShowHelp:
		m.mode = modeHelp
		m.palette = paletteState{}
	case cmdResetStats:
		m.mode = modeConfirm
		m.palette = paletteState{}
	}
	return nil
}

// handlePaletteKey processes keyboard input while the palette is open.
func (m Model) handlePaletteKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.flushPendingLogs()
		return m, tea.Quit
	case "esc", "ctrl+k":
		m.closePalette()
		return m, nil
	case "enter":
		if len(m.palette.cmds) == 0 {
			m.closePalette()
			return m, nil
		}
		return m, m.executePaletteCmd(m.palette.cmds[m.palette.cursor])
	case "up":
		m.movePaletteCursor(-1)
		return m, nil
	case "down":
		m.movePaletteCursor(1)
		return m, nil
	case "backspace", "ctrl+h":
		runes := []rune(m.palette.query)
		if len(runes) > 0 {
			m.palette.query = string(runes[:len(runes)-1])
			m.filterPaletteCmds()
		}
		return m, nil
	default:
		if msg.Key().Text != "" {
			m.palette.query += msg.Key().Text
			m.filterPaletteCmds()
		}
		return m, nil
	}
}

// renderCommandPalette renders the palette overlay in the content area. The
// frame has a rounded border (2 outer rows) and horizontal padding, so the
// inner budget is visibleRows()-2. The query line always occupies 1 inner
// row; the hint is shown only when at least one item/empty row plus the hint
// still fits, so countContentLines(palette) <= visibleRows() for every
// terminal height \u2014 in particular h=8 (vr=4) omits the hint and shows a
// single item (1 query +1 item +2 border =4) instead of overflowing.
func (m Model) renderCommandPalette() string {
	vw := m.viewportWidth()
	frameWidth := max(min(vw, 60), 8)
	innerWidth := max(frameWidth-4, 4)

	queryLine := fmt.Sprintf(" \u276f %s\u2588", m.palette.query)
	if lipgloss.Width(queryLine) > innerWidth {
		queryLine = truncatePlain(queryLine, innerWidth)
	}
	queryRendered := m.styles.paletteInputStyle.Render(queryLine)

	hint := " \u2191\u2193 navigate \u00b7 enter select \u00b7 esc close "
	if lipgloss.Width(hint) > innerWidth {
		hint = truncatePlain(hint, innerWidth)
	}
	hintRendered := m.styles.paletteDimStyle.Render(hint)
	emptyRendered := m.styles.paletteDimStyle.Render(" No matching commands")

	vr := m.visibleRows()
	innerBudget := max(
		// border top+bottom
		vr-2, 1)
	remaining := innerBudget - 1 // after query

	var lines []string
	lines = append(lines, queryRendered)

	if len(m.palette.cmds) == 0 {
		if remaining >= 2 {
			lines = append(lines, emptyRendered)
			lines = append(lines, hintRendered)
		} else if remaining >= 1 {
			lines = append(lines, emptyRendered)
		}
	} else {
		if remaining >= 2 {
			// Room for at least one item plus the hint.
			visible := max(min(len(m.palette.cmds), remaining-1), 1)
			start := 0
			if m.palette.cursor >= visible {
				start = m.palette.cursor - visible + 1
			}
			end := min(start+visible, len(m.palette.cmds))
			for i := start; i < end; i++ {
				label := " " + m.palette.cmds[i].label
				if lipgloss.Width(label) > innerWidth {
					label = truncatePlain(label, innerWidth)
				}
				padded := label + strings.Repeat(" ", max(innerWidth-lipgloss.Width(label), 0))
				if i == m.palette.cursor {
					lines = append(lines, m.styles.paletteSelectedStyle.Render(padded))
				} else {
					lines = append(lines, m.styles.rowStyle.Render(padded))
				}
			}
			lines = append(lines, hintRendered)
		} else if remaining >= 1 {
			visible := min(len(m.palette.cmds), remaining)
			start := 0
			if m.palette.cursor >= visible {
				start = m.palette.cursor - visible + 1
			}
			end := min(start+visible, len(m.palette.cmds))
			for i := start; i < end; i++ {
				label := " " + m.palette.cmds[i].label
				if lipgloss.Width(label) > innerWidth {
					label = truncatePlain(label, innerWidth)
				}
				padded := label + strings.Repeat(" ", max(innerWidth-lipgloss.Width(label), 0))
				if i == m.palette.cursor {
					lines = append(lines, m.styles.paletteSelectedStyle.Render(padded))
				} else {
					lines = append(lines, m.styles.rowStyle.Render(padded))
				}
			}
		}
	}

	content := strings.Join(lines, "\n")
	return m.styles.paletteFrameStyle.Width(frameWidth).Render(content) + "\n"
}
