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
	"image/color"
	"math"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func hexString(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", r>>8, g>>8, b>>8)
}

// darkModel returns a bare Model with the default dark palette populated, for
// style-selection tests that only read m.styles (no renderer wiring needed).
func darkModel() Model { return Model{styles: newTheme(true)} }

// relativeLuminance returns the WCAG relative luminance of a color, each sRGB
// channel linearized per the WCAG 2.x transfer function. Used by the theme
// contrast tests to prove the light palette actually meets AA on a light terminal.
func relativeLuminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	lin := func(v uint32) float64 {
		x := float64(v) / 0xFFFF
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// contrastRatio returns the WCAG contrast ratio between two colors (L1+0.05)/(L2+0.05)
// with the lighter luminance always in the numerator, so >= 4.5 means AA for normal text.
func contrastRatio(a, b color.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TestBackgroundColorMsgSwitchesTheme proves the TUI defaults to the dark palette
// and swaps to the light palette when the terminal reports a light background
// (tea.BackgroundColorMsg), then back to dark on a dark report. The swaps
// cover the whole theme, including the per-tab scrollbar thumb/track.
func TestBackgroundColorMsgSwitchesTheme(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	if got, want := hexString(m.styles.rowStyle.GetForeground()), "#E6EDF3"; got != want {
		t.Fatalf("default row foreground = %s, want dark default %s", got, want)
	}
	if got, want := hexString(m.scrollbars[0].ThumbStyle.GetForeground()), "#58A6FF"; got != want {
		t.Fatalf("default scrollbar thumb = %s, want dark default %s", got, want)
	}

	m = update(m, tea.BackgroundColorMsg{Color: color.White})
	if got, want := hexString(m.styles.rowStyle.GetForeground()), "#24292F"; got != want {
		t.Errorf("light row foreground = %s, want %s", got, want)
	}
	// Hue-to-state meaning survives the palette swap.
	if got, want := hexString(m.styles.statusOkStyle.GetForeground()), "#1A7F37"; got != want {
		t.Errorf("light status-ok foreground = %s, want %s", got, want)
	}
	if got, want := hexString(m.styles.gaugeCriticalStyle.GetForeground()), "#CF222E"; got != want {
		t.Errorf("light gauge-critical foreground = %s, want %s", got, want)
	}
	if got, want := hexString(m.scrollbars[0].ThumbStyle.GetForeground()), "#0969DA"; got != want {
		t.Errorf("light scrollbar thumb = %s, want %s", got, want)
	}

	m = update(m, tea.BackgroundColorMsg{Color: color.Black})
	if got, want := hexString(m.styles.rowStyle.GetForeground()), "#E6EDF3"; got != want {
		t.Errorf("restored dark row foreground = %s, want %s", got, want)
	}
}

// TestLightThemeContrast pins the light palette to WCAG AA (>= 4.5:1) for
// every text-bearing ink against the light surface it actually renders on. This is the
// regression guard for the "pale text invisible on a light terminal" bug: a future
// pastel edit to any of these styles fails loudly instead of shipping.
func TestLightThemeContrast(t *testing.T) {
	white := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	// The header text is white ink on its blue fill; #FFFFFF vs #0969DA
	// measures ~5.2:1 and is included exactly because it is tighter than
	// most of the light palette. The selected-tab fills below (#0550AE,
	// ~7.6:1) are the roomiest, not the tightest.
	tests := []struct {
		name   string
		ink    lipgloss.Style
		ground color.Color
	}{
		{"row on white", lightTheme().rowStyle, white},
		{"section on white", lightTheme().sectionStyle, white},
		{"dim on white", lightTheme().dimStyle2, white},
		{"table header on white", lightTheme().tableHeaderStyle, white},
		{"header on its blue fill", lightTheme().headerStyle, color.RGBA{0x09, 0x69, 0xDA, 0xFF}},
		{"status ok on white", lightTheme().statusOkStyle, white},
		{"status info on white", lightTheme().statusInfoStyle, white},
		{"status client error on white", lightTheme().statusClientErrStyle, white},
		{"status server error on white", lightTheme().statusServerErrStyle, white},
		{"gauge normal on white", lightTheme().gaugeNormalStyle, white},
		{"gauge warn on white", lightTheme().gaugeWarnStyle, white},
		{"gauge critical on white", lightTheme().gaugeCriticalStyle, white},
		{"queue warn on white", lightTheme().queueWarnStyle, white},
		{"sparkline on white", lightTheme().sparklineStyle, white},
		{"circuit open on white", lightTheme().circuitOpenStyle, white},
		{"chip active ink on its fill", lightTheme().chipActiveStyle, color.RGBA{0x05, 0x50, 0xAE, 0xFF}},
		{"tab active ink on its fill", lightTheme().tabActiveStyle, color.RGBA{0x05, 0x50, 0xAE, 0xFF}},
		{"tab inactive on its bg", lightTheme().tabInactiveStyle, color.RGBA{0xEA, 0xF1, 0xF6, 0xFF}},
		{"footer on its bg", lightTheme().footerStyle, color.RGBA{0xEA, 0xF1, 0xF6, 0xFF}},
		{"overlay on white", lightTheme().overlayStyle, white},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			fg := c.ink.GetForeground()
			if fg == nil {
				t.Fatal("ink style has no foreground color")
			}
			if r := contrastRatio(fg, c.ground); r < 4.5 {
				t.Errorf("contrast = %.2f:1, want >= 4.5:1 (%s on %s)",
					r, hexString(fg), hexString(c.ground))
			}
		})
	}
}

// TestChipOnHeaderDistinctness proves the active chip's background is
// visually distinct from the header bar in both themes — a regression
// guard for the defect where identical fills made the chip invisible
// against the bar, merging it into one continuous run of text.
func TestChipOnHeaderDistinctness(t *testing.T) {
	for _, dark := range []bool{true, false} {
		theme := newTheme(dark)
		hdrBG := theme.headerStyle.GetBackground()
		chipBG := theme.chipActiveStyle.GetBackground()
		if hdrBG == nil || chipBG == nil {
			t.Fatalf("dark=%v: missing background color", dark)
		}
		if hexString(hdrBG) == hexString(chipBG) {
			t.Errorf("dark=%v: chipActiveStyle BG %s == headerStyle BG %s; chip merges into bar",
				dark, hexString(chipBG), hexString(hdrBG))
		}
	}
}

// TestSelectedTabColorConsistency proves the active tab and active chip
// carry the same background fill in each theme, so every selected-tab
// indicator is one consistent color.
func TestSelectedTabColorConsistency(t *testing.T) {
	for _, dark := range []bool{true, false} {
		theme := newTheme(dark)
		tabBG := theme.tabActiveStyle.GetBackground()
		chipBG := theme.chipActiveStyle.GetBackground()
		if tabBG == nil || chipBG == nil {
			t.Fatalf("dark=%v: missing background color", dark)
		}
		if hexString(tabBG) != hexString(chipBG) {
			t.Errorf("dark=%v: tabActiveStyle BG %s != chipActiveStyle BG %s; selected tabs are inconsistent",
				dark, hexString(tabBG), hexString(chipBG))
		}
	}
}

// TestPaletteContrastAA pins the text-bearing selected fills in both themes
// to WCAG AA (>= 4.5:1), so a future fill edit that keeps the box model but
// ruins readability fails loudly instead of shipping.
func TestPaletteContrastAA(t *testing.T) {
	for _, dark := range []bool{true, false} {
		theme := newTheme(dark)
		for _, c := range []struct {
			name   string
			ink    lipgloss.Style
			ground color.Color
		}{
			{"chip active ink on its fill", theme.chipActiveStyle, theme.chipActiveStyle.GetBackground()},
			{"tab active ink on its fill", theme.tabActiveStyle, theme.tabActiveStyle.GetBackground()},
		} {
			if c.ground == nil {
				t.Fatalf("dark=%v: %s has no background color", dark, c.name)
			}
			fg := c.ink.GetForeground()
			if fg == nil {
				t.Fatalf("dark=%v: %s has no foreground color", dark, c.name)
			}
			if r := contrastRatio(fg, c.ground); r < 4.5 {
				t.Errorf("dark=%v: %s contrast = %.2f:1, want >= 4.5:1 (%s on %s)",
					dark, c.name, r, hexString(fg), hexString(c.ground))
			}
		}
	}
}

// TestChipHitSizing proves every provider chip keeps a usable hit area:
// the shortest pinned label ("acme") renders at least chipFloor cells wide
// in either active style, and every rendered chip in the layout covers at
// least chipFloor cells so chipAt always has a real span to hit.
func TestChipHitSizing(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	for _, style := range []lipgloss.Style{m.styles.chipActiveStyle, m.styles.chipInactiveStyle} {
		if got := lipgloss.Width(style.Render(" acme ")); got < chipFloor {
			t.Errorf("shortest chip renders %d cells, want >= chipFloor %d", got, chipFloor)
		}
	}
	fleets := [][]ProviderMeta{
		{{Name: "acme", Concurrency: 4}, {Name: "anthropic", Concurrency: 8}},
		{{Name: "anthropic-eu-central", Concurrency: 4}, {Name: "openai-prod-longname", Concurrency: 8}, {Name: "acme-edge-provider", Concurrency: 12}},
	}
	for _, metas := range fleets {
		for _, w := range []int{20, 40, 80, 120, 180} {
			m := NewModelForProviders(metas)
			m.width = w
			m.height = 24
			for ri, row := range m.chipRowsLayout() {
				for k, part := range row.parts {
					if got := lipgloss.Width(part); got < chipFloor {
						t.Errorf("w=%d row=%d chip %d renders %d cells, want >= chipFloor %d", w, ri, k, got, chipFloor)
					}
				}
			}
		}
	}
}
