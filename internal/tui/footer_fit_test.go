package tui

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

// TestRenderFooter_FitsEveryWidth pins the footer's width fit across the whole
// range that previously panicked. The old fit loop sliced at a fixed index
// (segs[6:]) behind a len(segs) > 2 guard, so once repeated middle drops
// shrank the segment list it ran past the end — a terminal narrowed enough to
// need those drops crashed the TUI. Every width must render without panicking,
// never overflow, and keep the leading tab hint and the trailing quit hint
// while they still fit.
func TestRenderFooter_FitsEveryWidth(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	for width := 1; width <= 200; width++ {
		m.width = width
		footer := stripANSI(m.renderFooter())
		if got := uniseg.StringWidth(footer); got > width {
			t.Fatalf("width %d: footer measures %d columns: %q", width, got, footer)
		}
		if width >= 32 {
			if !strings.Contains(footer, "1-6:tab") {
				t.Errorf("width %d: footer dropped the leading tab hint: %q", width, footer)
			}
			if !strings.Contains(footer, "q:quit") {
				t.Errorf("width %d: footer dropped the trailing quit hint: %q", width, footer)
			}
		}
	}
}
