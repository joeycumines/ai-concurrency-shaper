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
	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/viewport"
)

func (m Model) handleMouseClick(msg tea.MouseClickMsg) (Model, tea.Cmd) {
	mx := msg.Mouse().X
	my := msg.Mouse().Y

	// Provider switcher chips (row 0). chipAt does the same right-aligned
	// cumulative-width hit test that renderHeader uses to position the chips, so a
	// click lands on whichever chip the user sees under the cursor.
	if my == 0 {
		if i, ok := m.chipAt(mx); ok {
			m.switchProvider(i)
		}
		return m, nil
	}

	// Tab bar (row 1).
	if my == 1 {
		// Clicks beyond the terminal width land on non-existent cells and
		// must not switch tabs. tabAt deliberately ignores m.width, so this
		// guard is the boundary between rendered and unrendered columns.
		if mx >= m.width {
			return m, nil
		}
		if clickedTab, ok := m.tabAt(mx); ok {
			m.switchTab(clickedTab)
		}
		return m, nil
	}

	// Content area starts at row 3 (header=0, tabbar=1, separator=2).
	contentEndRow := contentStartRow + m.visibleRows()
	if my < contentStartRow || my >= contentEndRow {
		return m, nil
	}

	// A genuine content-area click (below the tab bar) pauses tail-following
	// on the Logs tab. Tab-bar clicks — including empty space and tab
	// switches — do NOT affect followLogs here, so switching to Logs via the
	// tab header keeps following enabled (see switchTab).
	if m.tab == tabLogs {
		m.followLogs = false
	}

	// Scrollbar column (rightmost): jump scroll and begin drag. The scrollbar
	// track is aligned with the scrollable data rows, below the fixed header
	// rows returned by contentHeaderRows().
	if mx == m.width-1 {
		contentHeight := m.maxCursor() + 1
		trackHeight := m.dataRows()
		headerRows := m.contentHeaderRows()
		trackStartRow := contentStartRow + headerRows
		if my < trackStartRow {
			return m, nil
		}
		relativeY := max(my-trackStartRow, 0)
		if relativeY >= trackHeight {
			relativeY = trackHeight - 1
		}
		// Clicking directly on the thumb should not jump; only track clicks
		// move the thumb to that proportional position. If the content fits the
		// viewport there is nothing to scroll, so don't start a drag.
		if sm := viewport.ScrollMax(contentHeight, trackHeight); sm > 0 {
			thumbTop := viewport.ThumbTop(m.scroll, contentHeight, trackHeight)
			thumbHeight := viewport.ThumbHeight(contentHeight, trackHeight)
			if relativeY < thumbTop || relativeY >= thumbTop+thumbHeight {
				m.scroll = viewport.ScrollFromThumb(relativeY, contentHeight, trackHeight)
			}
			m.dragging = true
		}
		m.dragStartY = relativeY
		m.dragStartScroll = m.scroll
		m.cursor = viewport.ClampCursor(
			m.scroll+min(trackHeight/2, m.maxCursor()-m.scroll),
			contentHeight)
		return m, nil
	}

	relativeRow := my - contentStartRow - m.contentHeaderRows()
	if relativeRow < 0 {
		return m, nil
	}
	m.cursor = viewport.CursorFromClick(relativeRow, m.scroll, m.maxCursor()+1)
	return m, nil
}

func (m Model) handleMouseWheel(msg tea.MouseWheelMsg) (Model, tea.Cmd) {
	switch msg.Mouse().Button {
	case tea.MouseWheelUp:
		m.moveCursor(-3)
	case tea.MouseWheelDown:
		m.moveCursor(3)
	}
	return m, nil
}

func (m Model) handleMouseMotion(msg tea.MouseMotionMsg) (Model, tea.Cmd) {
	if !m.dragging {
		return m, nil
	}
	if m.tab == tabLogs {
		m.followLogs = false
	}
	my := msg.Mouse().Y
	contentHeight := m.maxCursor() + 1
	trackHeight := m.dataRows()
	if viewport.ScrollMax(contentHeight, trackHeight) <= 0 {
		return m, nil
	}
	trackStartRow := contentStartRow + m.contentHeaderRows()
	if my < trackStartRow {
		my = trackStartRow
	}
	relativeY := max(my-trackStartRow, 0)
	if relativeY >= trackHeight {
		relativeY = trackHeight - 1
	}
	// Drag by delta from the grab point. The thumb occupies thumbHeight rows
	// so it can only travel (trackHeight - thumbHeight) rows. Dividing by this
	// dragRange ensures the scroll maps 1:1 to the thumb position, keeping the
	// thumb under the cursor and allowing the user to reach scrollMax.
	// Use int64 arithmetic to prevent overflow on large content heights
	// (matching the viewport package's approach).
	delta := relativeY - m.dragStartY
	sm := viewport.ScrollMax(contentHeight, trackHeight)
	thumbH := viewport.ThumbHeight(contentHeight, trackHeight)
	dragRange := trackHeight - thumbH
	if dragRange <= 0 {
		dragRange = trackHeight
	}
	scroll := m.dragStartScroll + int(int64(delta)*int64(sm)/int64(dragRange))
	m.scroll = viewport.ClampScroll(scroll, contentHeight, trackHeight)
	m.cursor = viewport.ClampCursor(
		m.scroll+min(trackHeight/2, contentHeight-1-m.scroll),
		contentHeight)
	return m, nil
}

func (m Model) handleMouseRelease(msg tea.MouseReleaseMsg) (Model, tea.Cmd) {
	m.dragging = false
	m.dragStartY = 0
	m.dragStartScroll = 0
	return m, nil
}
