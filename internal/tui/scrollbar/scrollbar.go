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

// Package scrollbar provides a visual scrollbar component for the TUI dashboard.
//
// It renders a single-column vertical scrollbar with proportional thumb sizing,
// using integer-only arithmetic via the viewport package for mathematically
// precise positioning.
package scrollbar

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/viewport"
)

// Rect is a minimal axis-aligned rectangle used for bounds.
type Rect struct {
	Position Position
	Size     Size
}

// Position is a 2D point in cell coordinates.
type Position struct {
	X int
	Y int
}

// Size describes 2D dimensions in terminal cells.
type Size struct {
	Width  int
	Height int
}

// Model holds the scrollbar state.
type Model struct {
	ContentHeight  int
	ViewportHeight int
	YOffset        int

	ThumbStyle lipgloss.Style
	TrackStyle lipgloss.Style
	ThumbChar  string
	TrackChar  string
}

// Option configures a Model.
type Option func(*Model)

// New creates a scrollbar model with reasonable defaults.
func New(opts ...Option) *Model {
	m := &Model{
		ThumbChar: "█",
		TrackChar: "│",
		ThumbStyle: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#58A6FF")),
		TrackStyle: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#21262D")),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// View renders the scrollbar as a single-column string exactly ViewportHeight
// lines tall. A full-height thumb is rendered when content fits within the
// viewport (standard "no scrolling needed" convention).
func (m *Model) View() string {
	if m.ViewportHeight <= 0 {
		return ""
	}

	contentHeight := max(m.ContentHeight, 0)
	viewportHeight := m.ViewportHeight

	th := viewport.ThumbHeight(contentHeight, viewportHeight)
	top := viewport.ThumbTop(
		viewport.ClampScroll(m.YOffset, contentHeight, viewportHeight),
		contentHeight, viewportHeight)

	return render(viewportHeight, top, th, m)
}

// ClickYOffset converts a click within the scrollbar track to a new scroll
// offset. clickY is relative to the top of the track (0-based).
func (m *Model) ClickYOffset(clickY int) int {
	return viewport.ScrollFromThumb(clickY, m.ContentHeight, m.ViewportHeight)
}

// DragYOffset converts a drag position within the scrollbar track to a new
// scroll offset, using the thumb center as the reference point for precise
// 1:1 dragging.
func (m *Model) DragYOffset(clickY int) int {
	contentHeight := max(m.ContentHeight, 0)
	th := viewport.ThumbHeight(contentHeight, m.ViewportHeight)
	// Adjust clickY to account for the thumb center offset.
	// When dragging, the user grabbed the center of the thumb, so we need
	// to offset by half the thumb height to get the effective track position.
	adjusted := max(clickY-th/2, 0)
	maxTrack := m.ViewportHeight - 1
	if adjusted > maxTrack {
		adjusted = maxTrack
	}
	return viewport.ScrollFromThumb(adjusted, contentHeight, m.ViewportHeight)
}

// RenderBounds renders the scrollbar within a Rect, extracting the viewport
// height from the rectangle's size.
func (m *Model) RenderBounds(bounds Rect) string {
	m.ViewportHeight = bounds.Size.Height
	return m.View()
}

// ── Chainable setters ──

func (m *Model) SetContentHeight(n int) *Model  { m.ContentHeight = n; return m }
func (m *Model) SetViewportHeight(n int) *Model { m.ViewportHeight = n; return m }
func (m *Model) SetPosition(n int) *Model       { m.YOffset = n; return m }
func (m *Model) SetThumbStyle(s lipgloss.Style) *Model {
	m.ThumbStyle = s
	return m
}
func (m *Model) SetTrackStyle(s lipgloss.Style) *Model {
	m.TrackStyle = s
	return m
}
func (m *Model) SetChars(thumb, track string) *Model {
	m.ThumbChar = thumb
	m.TrackChar = track
	return m
}

func render(viewportHeight, thumbTop, thumbHeight int, m *Model) string {
	var s strings.Builder

	renderThumbChar := m.ThumbChar
	renderTrackChar := m.TrackChar
	if renderThumbChar == " " {
		renderThumbChar = "\u00A0"
	}
	if renderTrackChar == " " {
		renderTrackChar = "\u00A0"
	}

	for i := range viewportHeight {
		isThumb := thumbTop <= i && i < thumbTop+thumbHeight
		if isThumb {
			s.WriteString(m.ThumbStyle.Render(renderThumbChar))
		} else {
			s.WriteString(m.TrackStyle.Render(renderTrackChar))
		}
		if i < viewportHeight-1 {
			s.WriteRune('\n')
		}
	}
	return s.String()
}
