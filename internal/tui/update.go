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
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
)

// redrawInterval is the coarse full-redraw cadence.
const redrawInterval = 10 * time.Second

func (m Model) resyncTickCmd() tea.Cmd {
	return tea.Tick(redrawInterval, func(time.Time) tea.Msg {
		return resyncTickMsg{}
	})
}

func immediateResyncDrawCmd() tea.Cmd {
	return func() tea.Msg {
		return resyncDrawMsg{}
	}
}

func resyncRedrawSequence() tea.Cmd {
	return tea.Sequence(tea.ClearScreen, immediateResyncDrawCmd())
}

func redrawMarker(epoch int) string {
	if epoch%2 == 0 {
		return "\x1b[0m"
	}
	return "\x1b[00m"
}

func (m Model) Init() tea.Cmd {
	// Ask the terminal for its background color. The response arrives as a
	// tea.BackgroundColorMsg (handled in Update) and swaps to the light
	// palette if the terminal reports a light background; terminals that do
	// not answer simply keep the dark default.
	return tea.Batch(m.resyncTickCmd(), tea.RequestBackgroundColor)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case resetMsg:
		select {
		case m.resetCh <- struct{}{}:
		default:
		}
	case logPollTickMsg:
		m.drainLogs()
	case animTickMsg:
		if msg.generation != m.animTickGen {
			// Stale schedule (superseded by a later AddToast): drop the tick
			// without re-rendering or re-arming. Model state has not changed
			// since the last update, so the tail processing is unnecessary.
			return m, nil
		}
		// No state to change; flows through to re-render and, if still
		// animating, schedule the next tick below.
	case resyncTickMsg:
		return m, resyncRedrawSequence()
	case resyncDrawMsg:
		m.redrawEpoch++
		return m, m.resyncTickCmd()
	case metrics.Snapshot:
		// Sugar for the single-provider case: treat it as an update for the
		// active provider so Run() callers that don't tag updates keep working.
		// Guarded like the ProviderUpdate case below: a model with no
		// providers must not index into the empty slice.
		if len(m.providers) == 0 {
			return m, cmd
		}
		m.providers[m.active].snap = msg
		m.snap = msg
		m.dashboardLinesCache = nil
	case ProviderUpdate:
		if msg.Index < 0 || msg.Index >= len(m.providers) {
			return m, cmd
		}
		m.providers[msg.Index].snap = msg.Snapshot
		if msg.Index == m.active {
			// Mirror into the flat snapshot so the renderers (and the
			// legacy case above) stay in sync with the active provider.
			m.snap = msg.Snapshot
			m.dashboardLinesCache = nil
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dashboardLinesCache = nil
	case tea.BackgroundColorMsg:
		// The terminal reported its background (see Init). Repaint the whole
		// UI for the light palette when the background is light; the dark
		// palette is the built-in default. Rendering caches are invalidated so
		// the next View repaints from the new theme, and scrollbars repaint too.
		m.styles = newTheme(msg.IsDark())
		m.dashboardLinesCache = nil
		m.applyScrollbarTheme()
	case tea.KeyPressMsg:
		m, cmd = m.handleKey(msg)
	case tea.MouseClickMsg:
		m, cmd = m.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		m, cmd = m.handleMouseWheel(msg)
	case tea.MouseMotionMsg:
		m, cmd = m.handleMouseMotion(msg)
	case tea.MouseReleaseMsg:
		m, cmd = m.handleMouseRelease(msg)
	}
	m.networkFiltered = m.computeVisibleNetworkEntries()
	m.toasts = toast.VisibleToasts(m.toasts)

	// If the anchored Logs detail item was evicted from the ring, close the
	// overlay rather than leave it pinned to nothing. This runs on every
	// update cycle so eviction is detected without waiting for a keypress.
	if m.mode == modeDetail && m.tab == tabLogs && !m.detailStillPresent() {
		m.mode = modeBrowse
		m.logDetailAnchor = logDetailAnchor{}
	}
	// Same for Network: if the anchored entry is no longer in the journal,
	// close the overlay so the operator never sees a stale detail view.
	if m.mode == modeDetail && m.tab == tabNetwork && m.networkDetailAnchor != nil && !m.networkDetailStillPresent() {
		m.mode = modeBrowse
		m.networkDetailAnchor = nil
	}

	// Tail-follow: while on the Logs tab and not paused by scroll input, keep the
	// viewport pinned to the newest lines so incoming logs scroll into view.
	if m.tab == tabLogs && m.followLogs && m.width > 0 && m.height > 0 {
		m.cursor = m.maxCursor()
		m.scroll = m.maxScroll()
	}
	m.adjustViewport()

	if c := m.toastAnimCmd(); c != nil {
		cmd = tea.Batch(cmd, c)
	}
	return m, cmd
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	// Palette mode: fully self-contained key handling.
	if m.mode == modePalette {
		return m.handlePaletteKey(msg)
	}

	// Help mode: any key dismisses (checked before quit so 'q' in help doesn't kill)
	if m.mode == modeHelp {
		m.mode = modeBrowse
		return m, nil
	}

	if m.mode == modeConfirm {
		switch msg.String() {
		case "y":
			m.mode = modeBrowse
			return m, tea.Batch(m.resetCmd())
		case "n", "esc":
			m.mode = modeBrowse
			return m, nil
		default:
			m.mode = modeBrowse
			return m, nil
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		// Best-effort final drain so the last render includes everything
		// published before this key was processed (see flushPendingLogs).
		m.flushPendingLogs()
		return m, tea.Quit
	}

	if m.mode == modeDetail {
		switch msg.String() {
		case "esc", "enter", " ", "space":
			m.mode = modeBrowse
			m.logDetailAnchor = logDetailAnchor{}
			m.networkDetailAnchor = nil
			return m, nil
		default:
			return m, nil
		}
	}

	if m.mode == modeFilter {
		switch msg.String() {
		case "esc":
			m.mode = modeBrowse
			m.filterText = ""
			return m, nil
		case "enter":
			m.mode = modeBrowse
			return m, nil
		case "backspace", "ctrl+h":
			runes := []rune(m.filterText)
			if len(runes) > 0 {
				m.filterText = string(runes[:len(runes)-1])
			}
			return m, nil
		default:
			// Only accumulate printable characters into the filter.
			// Key.Text is non-empty only for printable characters in
			// real terminal input. Special keys (arrows, F-keys,
			// Home/End, etc.) and modifier combos (ctrl+, alt+) all
			// have empty Text, preventing them from corrupting the
			// filter query.
			if msg.Key().Text != "" {
				m.filterText += msg.Key().Text
			}
			return m, nil
		}
	}

	keyCode := msg.Key().Code
	if keyCode == tea.KeyHome {
		m.hScroll[m.tab] = 0
	}
	if keyCode == tea.KeyPgUp {
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(-m.dataRows())
		} else {
			m.cursor -= m.dataRows()
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.adjustViewport()
		}
		return m, nil
	}
	if keyCode == tea.KeyPgDown {
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(m.dataRows())
		} else {
			m.cursor += m.dataRows()
			if m.cursor > m.maxCursor() {
				m.cursor = m.maxCursor()
			}
			m.adjustViewport()
		}
		return m, nil
	}
	if keyCode == tea.KeyHome {
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scroll = 0
			m.cursor = 0
		} else {
			m.cursor = 0
			m.adjustViewport()
		}
		return m, nil
	}
	if keyCode == tea.KeyEnd {
		if m.tab == tabLogs {
			m.followLogs = true
		}
		if m.tab == tabDashboard {
			m.scroll = m.maxScroll()
			m.cursor = m.scroll
		} else {
			m.cursor = m.maxCursor()
			m.adjustViewport()
		}
		return m, nil
	}

	// Provider switching (multi-provider): Tab cycles to the next provider,
	// Shift+Tab to the previous; both wrap. bubbletea decodes Shift+Tab as
	// the Tab key code with a Shift modifier rather than a distinct key
	// constant, so the modifier tells the two apart.
	if keyCode == tea.KeyTab {
		if msg.Key().Mod != 0 {
			m.cycleProvider(-1)
		} else {
			m.cycleProvider(1)
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+k":
		m.openPalette()
		return m, nil
	case "?":
		m.mode = modeHelp
	case "1":
		m.switchTab(tabDashboard)
	case "2":
		m.switchTab(tabRequests)
	case "3":
		m.switchTab(tabNetwork)
	case "4":
		m.switchTab(tabLogs)
	case "5":
		m.switchTab(tabConcurrency)
	case "6":
		m.switchTab(tabRoutes)

	case "j", "down":
		m.moveCursor(1)
	case "k", "up":
		m.moveCursor(-1)
	case "h", "left":
		m.adjustHScroll(-1)
	case "l", "right":
		m.adjustHScroll(1)
	case "H", "shift+left":
		m.adjustHScroll(-m.viewportWidth() / 2)
	case "L", "shift+right":
		m.adjustHScroll(m.viewportWidth() / 2)
	case "g":
		if m.tab == tabLogs {
			m.followLogs = false
		}
		m.cursor, m.scroll = 0, 0
	case "G":
		if m.tab == tabLogs {
			m.followLogs = true
		}
		if m.tab == tabDashboard {
			m.scroll = m.maxScroll()
			m.cursor = m.scroll
		} else {
			m.cursor, m.scroll = m.maxCursor(), m.maxScroll()
		}

	case "ctrl+u":
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(-m.dataRows() / 2)
		} else {
			m.cursor -= m.dataRows() / 2
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.adjustViewport()
		}
	case "ctrl+d":
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(m.dataRows() / 2)
		} else {
			m.cursor += m.dataRows() / 2
			if m.cursor > m.maxCursor() {
				m.cursor = m.maxCursor()
			}
			m.adjustViewport()
		}

	case "enter", " ", "space":
		if m.canInspect() {
			m.mode = modeDetail
			if m.tab == tabLogs {
				if item := m.logItemAtCursor(); item != nil {
					m.logDetailAnchor = logDetailAnchor{seq: item.seq, text: item.text}
				}
			}
			if m.tab == tabNetwork {
				entries := m.visibleNetworkEntries()
				if m.cursor < len(entries) {
					m.networkDetailAnchor = entries[m.cursor]
				}
			}
		}

	case "/":
		if m.tab == tabRequests || m.tab == tabNetwork || m.tab == tabLogs {
			m.mode = modeFilter
			m.filterText = ""
		}

	case "c":
		m.mode = modeConfirm

	case "t":
		if m.tab == tabNetwork {
			m.networkFilterType = networkFilterType((int(m.networkFilterType) + 1) % 5)
			m.cursor = 0
			m.scroll = 0
		}

	case "s":
		if m.tab == tabNetwork {
			m.networkFilterStatus = networkFilterStatus((int(m.networkFilterStatus) + 1) % 4)
			m.cursor = 0
			m.scroll = 0
		}
	}

	return m, nil
}

func (m *Model) switchTab(t tabID) {
	m.tab = t
	m.cursor = 0
	m.scroll = 0
	m.mode = modeBrowse
	m.dashboardLinesCache = nil
	if t == tabLogs {
		m.followLogs = true
	}
}

func (m *Model) moveCursor(delta int) {
	if m.tab == tabLogs {
		m.followLogs = false
	}
	if m.tab == tabDashboard {
		m.scrollDashboard(delta)
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	max := m.maxCursor()
	if m.cursor > max {
		m.cursor = max
	}
	m.adjustViewport()
}

// adjustHScroll shifts the active tab's horizontal viewport by delta cells,
// clamping to [0, maxHScroll]. Tabs without horizontally scrollable rows
// (maxHScroll == 0) always land at offset 0, so unrelated keys are no-ops.
// adjustHScroll applies a horizontal scroll delta to the active tab. The
// upper bound comes from maxHScroll, which measures the currently rendered
// rows — so after vertical navigation onto a page of short rows the bound can
// drop below the live offset. Clamping against that shrunken bound on a
// leftward step would teleport the view back to column 0, so an offset beyond
// the bound is only ever pulled down by moving left toward it, one step at a
// time; rightward motion is clamped to the bound as usual.
func (m *Model) adjustHScroll(delta int) {
	max := m.maxHScroll()
	offset := m.hScroll[m.tab] + delta
	if offset < 0 {
		offset = 0
	}
	if delta > 0 && offset > max {
		offset = max
	}
	m.hScroll[m.tab] = offset
}

// scrollDashboard adjusts m.scroll directly by delta for the Dashboard tab,
// clamping to the content bounds. m.cursor is pinned to the scroll position
// because the Dashboard has no visible cursor; this keeps the scrollbar model
// consistent with the rest of the viewport code.
func (m *Model) scrollDashboard(delta int) {
	m.scroll += delta
	if m.scroll < 0 {
		m.scroll = 0
	}
	max := m.maxScroll()
	if m.scroll > max {
		m.scroll = max
	}
	m.cursor = m.scroll
}

func (m *Model) adjustViewport() {
	maxC := m.maxCursor()
	if m.cursor > maxC {
		m.cursor = maxC
	}
	rows := m.dataRows()
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+rows {
		m.scroll = m.cursor - rows + 1
	}
	maxScroll := m.maxScroll()
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m *Model) canInspect() bool {
	switch m.tab {
	case tabRequests:
		return m.cursor < len(m.visibleEntries())
	case tabNetwork:
		return m.cursor < len(m.visibleNetworkEntries())
	case tabLogs:
		return m.cursor < len(m.visibleLogLines())
	case tabConcurrency:
		return m.cursor < len(m.snap.InFlight)
	default:
		return false
	}
}
