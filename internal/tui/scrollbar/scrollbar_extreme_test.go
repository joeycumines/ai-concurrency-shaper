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

package scrollbar

import (
	"strings"
	"testing"
)

// TestScrollbarExtremeHeights pins the scrollbar wiring at extreme terminal
// heights: the view renders exactly ViewportHeight lines, the thumb fills the
// track when content fits, and click/drag offsets round-trip within bounds
// even for empty and single-row content.
func TestScrollbarExtremeHeights(t *testing.T) {
	for _, viewportHeight := range []int{0, 1, 2, 4, 5, 8} {
		for _, contentHeight := range []int{0, 1, 2, 5, 20, 100} {
			m := New(
				withContentHeight(contentHeight),
				withViewportHeight(viewportHeight),
			)
			got := m.View()
			var lines int
			if got != "" {
				lines = strings.Count(got, "\n") + 1
			}
			if lines != viewportHeight {
				t.Errorf("content=%d viewport=%d: View renders %d lines, want %d",
					contentHeight, viewportHeight, lines, viewportHeight)
			}
			if viewportHeight <= 0 {
				continue
			}
			thumb := 0
			for line := range strings.SplitSeq(got, "\n") {
				if strings.Contains(line, m.ThumbChar) {
					thumb++
				}
			}
			if contentHeight <= viewportHeight {
				if thumb != viewportHeight {
					t.Errorf("content=%d viewport=%d: content fits, thumb covers %d rows, want full %d",
						contentHeight, viewportHeight, thumb, viewportHeight)
				}
			} else if thumb < 1 {
				t.Errorf("content=%d viewport=%d: overflowing content needs >= 1 thumb row, got 0",
					contentHeight, viewportHeight)
			}
			if thumb > viewportHeight {
				t.Errorf("content=%d viewport=%d: thumb %d rows exceeds track %d",
					contentHeight, viewportHeight, thumb, viewportHeight)
			}
			for _, y := range []int{0, viewportHeight - 1} {
				off := m.ClickYOffset(y)
				if off < 0 || off > max(contentHeight-viewportHeight, 0) {
					t.Errorf("content=%d viewport=%d: ClickYOffset(%d) = %d out of bounds",
						contentHeight, viewportHeight, y, off)
				}
				drag := m.DragYOffset(y)
				if drag < 0 || drag > max(contentHeight-viewportHeight, 0) {
					t.Errorf("content=%d viewport=%d: DragYOffset(%d) = %d out of bounds",
						contentHeight, viewportHeight, y, drag)
				}
			}
		}
	}
}
