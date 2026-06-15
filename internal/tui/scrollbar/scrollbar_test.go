package scrollbar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func withContentHeight(h int) Option  { return func(m *Model) { m.ContentHeight = h } }
func withViewportHeight(h int) Option { return func(m *Model) { m.ViewportHeight = h } }
func withYOffset(y int) Option        { return func(m *Model) { m.YOffset = y } }
func withChars(thumb, track string) Option {
	return func(m *Model) { m.ThumbChar = thumb; m.TrackChar = track }
}
func withStyles(thumb, track lipgloss.Style) Option {
	return func(m *Model) { m.ThumbStyle = thumb; m.TrackStyle = track }
}

func TestScrollbarMath(t *testing.T) {
	tests := []struct {
		name            string
		contentHeight   int
		viewportHeight  int
		yOffset         int
		expectThumbSize int
		expectThumbTop  int
	}{
		{"Full Visibility", 10, 10, 0, 10, 0},
		{"Double Content", 20, 10, 0, 5, 0},
		{"Double Content Scrolled Middle", 20, 10, 10, 5, 5},
		{"Huge Content (Min Height)", 1000, 10, 0, 1, 0},
		{"Empty Content", 0, 10, 0, 10, 0},
		{"YOffset Clamped Above Max", 20, 10, 999, 5, 5},
		{"YOffset Clamped Below Zero", 20, 10, -5, 5, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(
				withContentHeight(tc.contentHeight),
				withViewportHeight(tc.viewportHeight),
				withYOffset(tc.yOffset),
				withChars("T", "."),
				withStyles(lipgloss.NewStyle(), lipgloss.NewStyle()),
			)
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) != tc.viewportHeight {
				t.Errorf("Expected view height %d, got %d", tc.viewportHeight, len(lines))
			}
			thumbCount := 0
			firstThumb := -1
			for i, line := range lines {
				if strings.Contains(line, "T") {
					thumbCount++
					if firstThumb == -1 {
						firstThumb = i
					}
				}
			}
			if thumbCount != tc.expectThumbSize {
				t.Errorf("Expected thumb size %d, got %d", tc.expectThumbSize, thumbCount)
			}
			if firstThumb != tc.expectThumbTop {
				t.Errorf("Expected thumb top index %d, got %d", tc.expectThumbTop, firstThumb)
			}
		})
	}
}

func TestScrollbarZeroViewportHeight(t *testing.T) {
	m := New(withContentHeight(10), withViewportHeight(0), withYOffset(0))
	if got := m.View(); got != "" {
		t.Fatalf("expected empty view for zero viewport height, got %q", got)
	}
}

func TestSettersChain(t *testing.T) {
	thumbStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("57"))
	trackStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	m := New().
		SetContentHeight(100).
		SetPosition(10).
		SetViewportHeight(20).
		SetThumbStyle(thumbStyle).
		SetTrackStyle(trackStyle).
		SetChars("█", "│")

	if m.ContentHeight != 100 {
		t.Errorf("expected ContentHeight=100, got %d", m.ContentHeight)
	}
	if m.YOffset != 10 {
		t.Errorf("expected YOffset=10, got %d", m.YOffset)
	}
	if m.ViewportHeight != 20 {
		t.Errorf("expected ViewportHeight=20, got %d", m.ViewportHeight)
	}
}

func TestRenderBounds(t *testing.T) {
	bounds := Rect{Size: Size{Width: 1, Height: 8}}
	m := New().SetContentHeight(100).SetViewportHeight(10)
	result := m.RenderBounds(bounds)
	lines := strings.Split(result, "\n")
	if len(lines) != 8 {
		t.Errorf("expected 8 lines from RenderBounds height, got %d", len(lines))
	}
}

func TestRenderBoundsZeroHeight(t *testing.T) {
	bounds := Rect{Size: Size{Width: 1, Height: 0}}
	m := New().SetContentHeight(10).SetViewportHeight(5)
	if got := m.RenderBounds(bounds); got != "" {
		t.Errorf("expected empty string for zero height, got %q", got)
	}
}

func TestClickYOffset_ClickTop(t *testing.T) {
	m := New(withContentHeight(100), withViewportHeight(24), withYOffset(50))
	got := m.ClickYOffset(0)
	if got != 0 {
		t.Errorf("click at top: offset = %d, want 0", got)
	}
}

func TestClickYOffset_ClickBottom(t *testing.T) {
	m := New(withContentHeight(100), withViewportHeight(24), withYOffset(0))
	got := m.ClickYOffset(23)
	if got < 70 || got > 76 {
		t.Errorf("click at bottom: offset = %d, want [70, 76]", got)
	}
}

func TestClickYOffset_ClickMiddle(t *testing.T) {
	m := New(withContentHeight(100), withViewportHeight(24), withYOffset(0))
	got := m.ClickYOffset(12)
	// Should be near middle of [0, 76].
	if got < 45 || got > 55 {
		t.Errorf("click at middle: offset = %d, want [35, 45]", got)
	}
}

func TestClickYOffset_ContentFits(t *testing.T) {
	m := New(withContentHeight(5), withViewportHeight(24), withYOffset(0))
	got := m.ClickYOffset(12)
	if got != 0 {
		t.Errorf("click with fitting content: offset = %d, want 0", got)
	}
}

func TestClickYOffset_ClampsLargeClick(t *testing.T) {
	m := New(withContentHeight(100), withViewportHeight(24), withYOffset(0))
	got := m.ClickYOffset(999)
	if got != 76 {
		t.Errorf("click way below track: offset = %d, want 76", got)
	}
}

func TestDragYOffset_Proportional(t *testing.T) {
	// Dragging should map proportionally to the scroll range.
	m := New(withContentHeight(100), withViewportHeight(24), withYOffset(0))
	top := m.DragYOffset(0)
	bottom := m.DragYOffset(23)
	mid := m.DragYOffset(12)

	if top != 0 {
		t.Errorf("drag at top: offset = %d, want 0", top)
	}
	if bottom < 70 || bottom > 76 {
		t.Errorf("drag at bottom: offset = %d, want [70, 76]", bottom)
	}
	if mid < 35 || mid > 45 {
		t.Errorf("drag at middle: offset = %d, want [35, 45]", mid)
	}
}

func TestDragYOffset_Monotonic(t *testing.T) {
	m := New(withContentHeight(100), withViewportHeight(24), withYOffset(0))
	prev := -1
	for y := range 24 {
		got := m.DragYOffset(y)
		if got < prev {
			t.Errorf("drag not monotonic: DragYOffset(%d)=%d < DragYOffset(%d)=%d", y, got, y-1, prev)
		}
		prev = got
	}
}

func TestNarrowViewport(t *testing.T) {
	// Test scrollbar works with very narrow viewport heights.
	for vh := 1; vh <= 5; vh++ {
		m := New(
			withContentHeight(100),
			withViewportHeight(vh),
			withYOffset(0),
			withChars("T", "."),
			withStyles(lipgloss.NewStyle(), lipgloss.NewStyle()),
		)
		view := m.View()
		lines := strings.Split(view, "\n")
		if len(lines) != vh {
			t.Errorf("viewportHeight=%d: got %d lines, want %d", vh, len(lines), vh)
		}
		// Thumb should always be at least 1.
		hasThumb := false
		for _, line := range lines {
			if strings.Contains(line, "T") {
				hasThumb = true
				break
			}
		}
		if !hasThumb {
			t.Errorf("viewportHeight=%d: no thumb rendered", vh)
		}
	}
}

func TestZeroContent(t *testing.T) {
	m := New(withContentHeight(0), withViewportHeight(10), withYOffset(0))
	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 10 {
		t.Fatalf("expected 10 lines, got %d", len(lines))
	}
	// All lines should be thumb (content fits).
	for i, line := range lines {
		if !strings.Contains(line, "█") {
			t.Errorf("line %d: should be thumb (content fits)", i)
		}
	}
}

func TestScrollbarTinyTerminal(t *testing.T) {
	// Terminal with height 3 (just enough for header + tabbar + 1 content row).
	m := New(
		withContentHeight(50),
		withViewportHeight(1),
		withYOffset(0),
		withChars("T", "."),
		withStyles(lipgloss.NewStyle(), lipgloss.NewStyle()),
	)
	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	// Thumb must be present.
	if !strings.Contains(lines[0], "T") {
		t.Error("thumb should be present in single-row scrollbar")
	}
}
