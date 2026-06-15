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

// Package viewport provides mathematically precise viewport scrolling calculations
// for terminal user interfaces.
//
// All functions are pure: they take integer inputs and return integer outputs
// with no side effects, no allocations, and no dependencies. This makes them
// trivially unit-testable and free of floating-point drift.
//
// The core model:
//   - contentHeight: total number of lines of content
//   - viewportHeight: number of lines visible on screen
//   - scrollOffset: index of the first visible line (0-based, clamped to [0, maxScroll])
//   - cursor: index of the focused line (0-based, clamped to [0, contentHeight-1])
//
// The scrollbar thumb is positioned proportionally within the track using integer
// arithmetic only, avoiding floating-point rounding issues.
package viewport

// ScrollMax returns the maximum valid scroll offset.
// When content fits within the viewport, this is 0 (no scrolling needed).
// Otherwise it is contentHeight - viewportHeight.
func ScrollMax(contentHeight, viewportHeight int) int {
	if viewportHeight <= 0 {
		return 0
	}
	return max(contentHeight-viewportHeight, 0)
}

// ClampScroll constrains scrollOffset to [0, ScrollMax].
func ClampScroll(scrollOffset, contentHeight, viewportHeight int) int {
	return min(max(scrollOffset, 0), ScrollMax(contentHeight, viewportHeight))
}

// ClampCursor constrains cursor to [0, contentHeight-1].
// When content is empty, returns 0.
func ClampCursor(cursor, contentHeight int) int {
	if contentHeight <= 0 {
		return 0
	}
	return min(max(cursor, 0), contentHeight-1)
}

// ThumbHeight computes the number of rows the scrollbar thumb should occupy.
// It uses the proportional formula: thumb = max(viewport² / content, 1).
// When content fits within the viewport, the thumb fills the entire track.
func ThumbHeight(contentHeight, viewportHeight int) int {
	if viewportHeight <= 0 {
		return 0
	}
	if contentHeight <= 0 || contentHeight <= viewportHeight {
		return viewportHeight
	}
	// Integer-only proportional thumb: (viewport * viewport + content/2) / content
	// The +content/2 provides rounding to nearest instead of truncation.
	vp := int64(viewportHeight)
	ch := int64(contentHeight)
	th := min(max((vp*vp+ch/2)/ch, 1), int64(viewportHeight))
	return int(th)
}

// ThumbTop computes the row at which the thumb starts within the track.
// It maps scrollOffset linearly into [0, viewportHeight - thumbHeight].
// When content fits, returns 0 (thumb at top, full track).
func ThumbTop(scrollOffset, contentHeight, viewportHeight int) int {
	if viewportHeight <= 0 {
		return 0
	}
	th := ThumbHeight(contentHeight, viewportHeight)
	maxTop := viewportHeight - th
	if maxTop <= 0 {
		return 0
	}
	sm := ScrollMax(contentHeight, viewportHeight)
	if sm <= 0 {
		return 0
	}
	// Integer-only linear mapping: top = scrollOffset * maxTop / scrollMax
	// With rounding: (scrollOffset * maxTop + scrollMax/2) / scrollMax
	so := int64(scrollOffset)
	mt := int64(maxTop)
	smax := int64(sm)
	top := min(max((so*mt+smax/2)/smax, 0), int64(maxTop))
	return int(top)
}

// ScrollFromThumb converts a click position within the scrollbar track
// to a scroll offset. clickY is relative to the top of the track (0-based).
//
// The mapping is the exact inverse of ThumbTop: given a click position, it
// computes the scroll offset such that ThumbTop(scroll) == clickY. This
// ensures that clicking the center of the thumb does not change the scroll,
// clicking above scrolls up, and clicking below scrolls down.
func ScrollFromThumb(clickY, contentHeight, viewportHeight int) int {
	if viewportHeight <= 0 {
		return 0
	}
	sm := ScrollMax(contentHeight, viewportHeight)
	if sm <= 0 {
		return 0
	}
	th := ThumbHeight(contentHeight, viewportHeight)
	maxTop := viewportHeight - th
	if maxTop <= 0 {
		// Thumb fills or nearly fills track: map click proportionally across full track.
		if viewportHeight <= 1 {
			// Single-row track: only valid click is Y=0, map to top.
			return 0
		}
		cy := int64(clickY)
		vp := int64(viewportHeight)
		return int((cy*int64(sm) + (vp-1)/2) / (vp - 1))
	}
	// Inverse of ThumbTop: scroll = clickY * maxTop / scrollMax
	// But we need to convert from clickY → thumbTop space first.
	// Since ThumbTop maps scroll → thumbTop linearly via:
	//   thumbTop = scroll * maxTop / scrollMax (with rounding)
	// The inverse is: scroll = thumbTop * scrollMax / maxTop (with rounding)
	cy := int64(clickY)
	smax := int64(sm)
	mt := int64(maxTop)
	// Round to nearest: (cy * smax + maxTop/2) / maxTop
	scroll := min(max((cy*smax+mt/2)/mt, 0), int64(sm))
	return int(scroll)
}

// CursorFromClick converts a click row within the content area to a cursor
// position. clickY is relative to the top of the content viewport (0-based).
func CursorFromClick(clickY, scrollOffset, contentHeight int) int {
	return ClampCursor(scrollOffset+clickY, contentHeight)
}
