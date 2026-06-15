package viewport

import "testing"

func TestScrollMax(t *testing.T) {
	tests := []struct {
		name           string
		contentHeight  int
		viewportHeight int
		want           int
	}{
		{"content fits", 5, 10, 0},
		{"content equals viewport", 10, 10, 0},
		{"content double viewport", 20, 10, 10},
		{"single line content", 1, 10, 0},
		{"empty content", 0, 10, 0},
		{"zero viewport", 100, 0, 0},
		{"negative viewport", 100, -5, 0},
		{"huge content", 1000000, 24, 999976},
		{"one over", 11, 10, 1},
		{"minimum viewport 1", 100, 1, 99},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrollMax(tc.contentHeight, tc.viewportHeight)
			if got != tc.want {
				t.Errorf("ScrollMax(%d, %d) = %d, want %d",
					tc.contentHeight, tc.viewportHeight, got, tc.want)
			}
		})
	}
}

func TestClampScroll(t *testing.T) {
	tests := []struct {
		name           string
		scrollOffset   int
		contentHeight  int
		viewportHeight int
		want           int
	}{
		{"zero", 0, 100, 24, 0},
		{"in range", 50, 100, 24, 50},
		{"at max", 76, 100, 24, 76},
		{"above max", 999, 100, 24, 76},
		{"negative", -5, 100, 24, 0},
		{"content fits", 5, 10, 24, 0},
		{"zero viewport", 5, 100, 0, 0},
		{"zero content", 5, 0, 24, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampScroll(tc.scrollOffset, tc.contentHeight, tc.viewportHeight)
			if got != tc.want {
				t.Errorf("ClampScroll(%d, %d, %d) = %d, want %d",
					tc.scrollOffset, tc.contentHeight, tc.viewportHeight, got, tc.want)
			}
		})
	}
}

func TestClampCursor(t *testing.T) {
	tests := []struct {
		name          string
		cursor        int
		contentHeight int
		want          int
	}{
		{"zero", 0, 100, 0},
		{"in range", 50, 100, 50},
		{"at max", 99, 100, 99},
		{"above max", 999, 100, 99},
		{"negative", -5, 100, 0},
		{"single item", 0, 1, 0},
		{"single item overflow", 5, 1, 0},
		{"zero content", 5, 0, 0},
		{"negative content", 5, -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampCursor(tc.cursor, tc.contentHeight)
			if got != tc.want {
				t.Errorf("ClampCursor(%d, %d) = %d, want %d",
					tc.cursor, tc.contentHeight, got, tc.want)
			}
		})
	}
}

func TestThumbHeight(t *testing.T) {
	tests := []struct {
		name           string
		contentHeight  int
		viewportHeight int
		want           int
	}{
		// Content fits: thumb fills track.
		{"content fits", 5, 10, 10},
		{"content equals viewport", 10, 10, 10},
		{"empty content", 0, 10, 10},
		// Double content: thumb is half.
		{"double content", 20, 10, 5},
		// Triple content: thumb is 10*10/3 ≈ 3.
		{"triple content", 30, 10, 3},
		// Huge content: thumb is minimum 1.
		{"huge content", 10000, 10, 1},
		// Edge: 1 line content, big viewport.
		{"single line", 1, 40, 40},
		// Edge: viewport 1.
		{"viewport 1 content 1", 1, 1, 1},
		{"viewport 1 content 2", 2, 1, 1},
		{"viewport 1 huge content", 1000, 1, 1},
		// Zero viewport.
		{"zero viewport", 100, 0, 0},
		// Large viewport.
		{"large viewport fits", 20, 100, 100},
		{"large viewport scroll", 200, 100, 50},
		// Verify rounding: viewport=24, content=100 → 24²/100 = 576/100 = 5.76 → 6.
		{"rounding up", 100, 24, 6},
		// viewport=24, content=200 → 576/200 = 2.88 → 3.
		{"rounding up 2", 200, 24, 3},
		// viewport=24, content=500 → 576/500 = 1.152 → 1.
		{"rounding to 1", 500, 24, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ThumbHeight(tc.contentHeight, tc.viewportHeight)
			if got != tc.want {
				t.Errorf("ThumbHeight(%d, %d) = %d, want %d",
					tc.contentHeight, tc.viewportHeight, got, tc.want)
			}
		})
	}
}

func TestThumbTop(t *testing.T) {
	tests := []struct {
		name           string
		scrollOffset   int
		contentHeight  int
		viewportHeight int
		wantTop        int
	}{
		// Content fits: always top 0.
		{"content fits", 0, 10, 24, 0},
		{"content equals viewport", 0, 24, 24, 0},
		// Scroll at 0: top is 0.
		{"at top", 0, 100, 24, 0},
		// Scroll at max: thumb at bottom.
		{"at bottom", 76, 100, 24, 18}, // th=6, maxTop=18, top=18*76/76=18
		// Scroll at middle.
		{"at middle", 38, 100, 24, 9}, // th=6, maxTop=18, top=18*38/76=9
		// Small viewport.
		{"viewport 5 content 20 scroll 0", 0, 20, 5, 0},
		{"viewport 5 content 20 scroll 15", 15, 20, 5, 4}, // th=1, maxTop=4, top=4*15/15=4
		// Edge: viewport 1.
		{"viewport 1 content 2 scroll 0", 0, 2, 1, 0},
		{"viewport 1 content 2 scroll 1", 1, 2, 1, 0},
		// Huge content.
		{"huge content scroll 0", 0, 1000000, 24, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ThumbTop(tc.scrollOffset, tc.contentHeight, tc.viewportHeight)
			if got != tc.wantTop {
				t.Errorf("ThumbTop(%d, %d, %d) = %d, want %d",
					tc.scrollOffset, tc.contentHeight, tc.viewportHeight, got, tc.wantTop)
			}
		})
	}
}

func TestThumbTopExhaustive(t *testing.T) {
	// Verify key invariants for all reasonable content/viewport combos.
	for contentHeight := range 30 {
		for viewportHeight := 1; viewportHeight <= 20; viewportHeight++ {
			sm := ScrollMax(contentHeight, viewportHeight)
			th := ThumbHeight(contentHeight, viewportHeight)
			maxTop := viewportHeight - th

			for scrollOffset := 0; scrollOffset <= sm; scrollOffset++ {
				top := ThumbTop(scrollOffset, contentHeight, viewportHeight)

				if top < 0 {
					t.Errorf("ThumbTop(%d,%d,%d) = %d < 0",
						scrollOffset, contentHeight, viewportHeight, top)
				}
				if top > maxTop {
					t.Errorf("ThumbTop(%d,%d,%d) = %d > maxTop %d",
						scrollOffset, contentHeight, viewportHeight, top, maxTop)
				}

				// Monotonicity: higher scroll offset → higher or equal thumb top.
				if scrollOffset > 0 {
					prev := ThumbTop(scrollOffset-1, contentHeight, viewportHeight)
					if top < prev {
						t.Errorf("ThumbTop not monotonic: ThumbTop(%d)=%d < ThumbTop(%d)=%d (content=%d viewport=%d)",
							scrollOffset, top, scrollOffset-1, prev, contentHeight, viewportHeight)
					}
				}
			}

			// Boundary: scroll=0 → top=0.
			if top := ThumbTop(0, contentHeight, viewportHeight); top != 0 {
				t.Errorf("ThumbTop(0,%d,%d) = %d, want 0", contentHeight, viewportHeight, top)
			}

			// Boundary: scroll=max → top=maxTop.
			if sm > 0 {
				if top := ThumbTop(sm, contentHeight, viewportHeight); top != maxTop {
					t.Errorf("ThumbTop(sm,%d,%d) = %d, want maxTop %d",
						contentHeight, viewportHeight, top, maxTop)
				}
			}
		}
	}
}

func TestThumbHeightExhaustive(t *testing.T) {
	for contentHeight := range 50 {
		for viewportHeight := 1; viewportHeight <= 30; viewportHeight++ {
			th := ThumbHeight(contentHeight, viewportHeight)
			if th < 1 && viewportHeight > 0 && contentHeight > 0 {
				t.Errorf("ThumbHeight(%d, %d) = %d, want >= 1",
					contentHeight, viewportHeight, th)
			}
			if th > viewportHeight {
				t.Errorf("ThumbHeight(%d, %d) = %d, want <= %d",
					contentHeight, viewportHeight, th, viewportHeight)
			}
			// When content fits, thumb fills viewport.
			if contentHeight <= viewportHeight {
				if th != viewportHeight {
					t.Errorf("ThumbHeight(%d, %d) = %d, want %d (content fits)",
						contentHeight, viewportHeight, th, viewportHeight)
				}
			}
			// When content is larger than viewport and viewport > 1, thumb < viewport.
			// When viewport == 1, thumb == 1 == viewportHeight is the minimum.
			if contentHeight > viewportHeight && viewportHeight > 1 {
				if th >= viewportHeight {
					t.Errorf("ThumbHeight(%d, %d) = %d, want < %d (content larger)",
						contentHeight, viewportHeight, th, viewportHeight)
				}
			}
		}
	}
}

func TestScrollFromThumb(t *testing.T) {
	tests := []struct {
		name           string
		clickY         int
		contentHeight  int
		viewportHeight int
		wantMin        int // minimum expected (allowing rounding)
		wantMax        int // maximum expected (allowing rounding)
	}{
		{"top of track", 0, 100, 24, 0, 0},
		{"bottom of track", 23, 100, 24, 76, 76},
		{"middle of track", 12, 100, 24, 48, 54},
		{"content fits click anywhere", 5, 10, 24, 0, 0},
		{"empty content", 5, 0, 24, 0, 0},
		{"zero viewport", 0, 100, 0, 0, 0},
		{"single row viewport", 0, 100, 1, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrollFromThumb(tc.clickY, tc.contentHeight, tc.viewportHeight)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("ScrollFromThumb(%d, %d, %d) = %d, want [%d, %d]",
					tc.clickY, tc.contentHeight, tc.viewportHeight, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestCursorFromClick(t *testing.T) {
	tests := []struct {
		name          string
		clickY        int
		scrollOffset  int
		contentHeight int
		want          int
	}{
		{"first visible row", 0, 0, 100, 0},
		{"second visible row", 1, 0, 100, 1},
		{"with scroll offset", 0, 50, 100, 50},
		{"click beyond content", 50, 0, 10, 9},
		{"scroll beyond, click beyond", 50, 90, 100, 99},
		{"empty content", 0, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CursorFromClick(tc.clickY, tc.scrollOffset, tc.contentHeight)
			if got != tc.want {
				t.Errorf("CursorFromClick(%d, %d, %d) = %d, want %d",
					tc.clickY, tc.scrollOffset, tc.contentHeight, got, tc.want)
			}
		})
	}
}

func TestViewportInvariants(t *testing.T) {
	// Test invariants that must hold for all content/viewport combinations.
	for contentHeight := range 25 {
		for viewportHeight := 1; viewportHeight <= 15; viewportHeight++ {
			sm := ScrollMax(contentHeight, viewportHeight)
			th := ThumbHeight(contentHeight, viewportHeight)
			maxTop := viewportHeight - th

			// 1. ScrollMax >= 0
			if sm < 0 {
				t.Errorf("ScrollMax(%d,%d) = %d < 0", contentHeight, viewportHeight, sm)
			}

			// 2. ThumbHeight is in [1, viewportHeight] when viewport > 0.
			if viewportHeight > 0 && th < 1 {
				t.Errorf("ThumbHeight(%d,%d) = %d < 1", contentHeight, viewportHeight, th)
			}
			if th > viewportHeight {
				t.Errorf("ThumbHeight(%d,%d) = %d > %d", contentHeight, viewportHeight, th, viewportHeight)
			}

			// 3. maxTop >= 0
			if maxTop < 0 {
				t.Errorf("maxTop(%d,%d) = %d < 0", contentHeight, viewportHeight, maxTop)
			}

			// 4. ClampScroll always returns [0, ScrollMax]
			for _, offset := range []int{-100, -1, 0, sm / 2, sm, sm + 1, sm + 100} {
				clamped := ClampScroll(offset, contentHeight, viewportHeight)
				if clamped < 0 || clamped > sm {
					t.Errorf("ClampScroll(%d,%d,%d) = %d, want [0, %d]",
						offset, contentHeight, viewportHeight, clamped, sm)
				}
			}

			// 5. ClampCursor always returns [0, contentHeight-1] (or 0 if empty)
			for _, cursor := range []int{-100, -1, 0, contentHeight / 2, contentHeight - 1, contentHeight, contentHeight + 100} {
				clamped := ClampCursor(cursor, contentHeight)
				if contentHeight > 0 {
					if clamped < 0 || clamped > contentHeight-1 {
						t.Errorf("ClampCursor(%d,%d) = %d, want [0, %d]",
							cursor, contentHeight, clamped, contentHeight-1)
					}
				} else if clamped != 0 {
					t.Errorf("ClampCursor(%d,%d) = %d, want 0", cursor, contentHeight, clamped)
				}
			}

			// 6. ThumbTop is always [0, maxTop]
			for scroll := 0; scroll <= sm; scroll++ {
				top := ThumbTop(scroll, contentHeight, viewportHeight)
				if top < 0 || top > maxTop {
					t.Errorf("ThumbTop(%d,%d,%d) = %d, want [0, %d]",
						scroll, contentHeight, viewportHeight, top, maxTop)
				}
			}
		}
	}
}

func TestRoundTripScrollThumb(t *testing.T) {
	// Round-trip: scroll → thumbTop → scroll should (approximately) preserve position.
	// The tolerance accounts for integer division rounding in both directions.
	// Tolerance = ceil(scrollMax / maxTop) which is the maximum rounding error
	// from a single integer division step.
	for contentHeight := 10; contentHeight <= 50; contentHeight++ {
		for viewportHeight := 5; viewportHeight <= 20; viewportHeight++ {
			sm := ScrollMax(contentHeight, viewportHeight)
			if sm == 0 {
				continue
			}
			th := ThumbHeight(contentHeight, viewportHeight)
			maxTop := viewportHeight - th
			if maxTop <= 0 {
				continue
			}
			// Maximum rounding error tolerance: each integer division can lose
			// up to half a unit, so two divisions can lose up to ~scrollMax/maxTop.
			tolerance := max(
				// ceil(sm/maxTop)
				(sm+maxTop-1)/maxTop, 1)

			for scrollOffset := 0; scrollOffset <= sm; scrollOffset += max(sm/10, 1) {
				top := ThumbTop(scrollOffset, contentHeight, viewportHeight)
				// Use the thumb top position directly for the most precise round-trip.
				// In practice users click near the center, but this tests the math.
				roundTripped := ScrollFromThumb(top, contentHeight, viewportHeight)
				diff := roundTripped - scrollOffset
				if diff < 0 {
					diff = -diff
				}
				if diff > tolerance {
					t.Errorf("Round-trip failed: scroll=%d → top=%d → scroll=%d (diff=%d > tolerance=%d), content=%d viewport=%d",
						scrollOffset, top, roundTripped, diff, tolerance, contentHeight, viewportHeight)
				}
			}
		}
	}
}

func TestRoundTripScrollThumbCenterClick(t *testing.T) {
	// Verify that clicking the center of the thumb approximately preserves scroll.
	// When scrollMax >> maxTop, a single thumb row represents many scroll positions.
	// The center of the thumb can be off by up to half the thumb's scroll range.
	// Tolerance = ceil(scrollMax / maxTop) * ceil(thumbHeight / 2), which is the
	// maximum error from clicking the center vs the top of the thumb.
	for contentHeight := 10; contentHeight <= 100; contentHeight++ {
		for viewportHeight := 5; viewportHeight <= 24; viewportHeight++ {
			sm := ScrollMax(contentHeight, viewportHeight)
			if sm == 0 {
				continue
			}
			th := ThumbHeight(contentHeight, viewportHeight)
			maxTop := viewportHeight - th
			if maxTop <= 0 {
				continue
			}
			// Per-row scroll range.
			perRow := (sm + maxTop - 1) / maxTop
			// Center of thumb can be at most th/2 rows below top.
			// Each row represents perRow scroll positions.
			tolerance := max(perRow*(th/2+1), 1)

			for scrollOffset := 0; scrollOffset <= sm; scrollOffset += max(sm/8, 1) {
				top := ThumbTop(scrollOffset, contentHeight, viewportHeight)
				// Click the center of the thumb (as a user would).
				clickY := top + th/2
				if clickY >= viewportHeight {
					clickY = viewportHeight - 1
				}
				roundTripped := ScrollFromThumb(clickY, contentHeight, viewportHeight)
				diff := roundTripped - scrollOffset
				if diff < 0 {
					diff = -diff
				}
				if diff > tolerance {
					t.Errorf("Center-click round-trip failed: scroll=%d → top=%d → clickY=%d → scroll=%d (diff=%d > tolerance=%d), content=%d viewport=%d",
						scrollOffset, top, clickY, roundTripped, diff, tolerance, contentHeight, viewportHeight)
				}
			}
		}
	}
}

func TestFuzzViewport(t *testing.T) {
	// Fuzz-like: random-ish corner cases.
	for ch := range 200 {
		for vh := 1; vh <= 50; vh++ {
			sm := ScrollMax(ch, vh)
			th := ThumbHeight(ch, vh)

			// Thumb in [1, vh].
			if th < 1 || th > vh {
				t.Errorf("ThumbHeight(%d,%d)=%d not in [1,%d]", ch, vh, th, vh)
			}

			// maxTop >= 0.
			maxTop := vh - th
			if maxTop < 0 {
				t.Errorf("maxTop(%d,%d)=%d < 0", ch, vh, maxTop)
			}

			// Boundary scroll values.
			for _, so := range []int{0, sm} {
				top := ThumbTop(so, ch, vh)
				if top < 0 || top > maxTop {
					t.Errorf("ThumbTop(%d,%d,%d)=%d not in [0,%d]", so, ch, vh, top, maxTop)
				}
			}

			// ClampScroll for out-of-bounds.
			for _, bad := range []int{-999, -1, sm + 1, sm + 999} {
				cs := ClampScroll(bad, ch, vh)
				if cs < 0 || cs > sm {
					t.Errorf("ClampScroll(%d,%d,%d)=%d not in [0,%d]", bad, ch, vh, cs, sm)
				}
			}

			// ClampCursor for out-of-bounds.
			for _, bad := range []int{-999, -1, ch, ch + 999} {
				cc := ClampCursor(bad, ch)
				if ch > 0 {
					if cc < 0 || cc > ch-1 {
						t.Errorf("ClampCursor(%d,%d)=%d not in [0,%d]", bad, ch, cc, ch-1)
					}
				} else if cc != 0 {
					t.Errorf("ClampCursor(%d,%d)=%d, want 0", bad, ch, cc)
				}
			}
		}
	}
}
