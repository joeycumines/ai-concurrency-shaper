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
	"testing"

	"github.com/rivo/uniseg"
)

func TestRenderStatusBarShowsAbortedWhenNoStatusCommitted(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	m.snap.TotalAborted = 3

	bar := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	if !strings.Contains(bar, "Aborted:3") {
		t.Fatalf("status bar should label status-0 aborted exchanges, got: %s", bar)
	}
	if strings.Contains(bar, "2xx:") || strings.Contains(bar, "5xx:") {
		t.Fatalf("status bar with no committed statuses should keep empty distribution, got: %s", bar)
	}
}

func TestRenderStatusBar_NoResponses(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	s := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	if strings.Contains(s, "No responses yet") {
		t.Errorf("empty status bar should not render text, got: %s", s)
	}
	if !strings.Contains(s, "░") {
		t.Errorf("empty status bar should render an empty track, got: %s", s)
	}
	// With no labels the bar is budgeted to leave room for the
	// 10-cell prefix, brackets, and trailing spaces.
	if got, want := uniseg.StringWidth(s), m.viewportWidth()-10; got != want {
		t.Errorf("empty status bar width = %d, want %d; got: %q", got, want, s)
	}
}

func TestRenderStatusBar_WithResponses(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 0, 90, 5, 8, 3}
	s := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	if !strings.Contains(s, "2xx:90") {
		t.Errorf("should show 2xx:90, got: %s", s)
	}
}

func TestRenderStatusBar_EmptyBarWidth(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	s := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	// With no labels the bar is budgeted to leave room for the
	// 10-cell prefix, brackets, and trailing spaces.
	if got, want := uniseg.StringWidth(s), m.viewportWidth()-10; got != want {
		t.Errorf("empty status bar width = %d, want %d; got: %q", got, want, s)
	}
}

func TestRenderStatusBar_ColoredLabels(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 1, 20, 3, 4, 5}
	s := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	for _, label := range []string{"1xx:1", "2xx:20", "3xx:3", "4xx:4", "5xx:5"} {
		if !strings.Contains(s, label) {
			t.Errorf("should show %s, got: %s", label, s)
		}
	}
	// Verify the styles emitted correspond to the token colors by checking
	// that the styled text contains an ANSI sequence for the expected hex.
	raw := strings.Join(m.renderStatusBar(m.hBarWidth()), " ")
	wantSeq := map[string]string{
		"1xx": "38;2;139;148;158",
		"2xx": "38;2;63;185;80",
		"3xx": "38;2;88;166;255",
		"4xx": "38;2;240;136;62",
		"5xx": "38;2;248;81;73",
	}
	for _, seq := range wantSeq {
		if !strings.Contains(raw, seq) {
			t.Errorf("status labels missing %q ANSI sequence, got: %s", seq, raw)
		}
	}
}

// composedStatusLines returns the status section rows exactly as the
// dashboard renders them: the 10-cell "  Status  " prefix on the bar
// row, and the wrapped labels row (if any) without a prefix.
func composedStatusLines(m Model) []string {
	lines := m.renderStatusBar(m.gaugeTrackWidth())
	out := make([]string, len(lines))
	out[0] = m.styles.sectionStyle.Render("  Status  ") + lines[0]
	copy(out[1:], lines[1:])
	return out
}

func TestRenderStatusBar_FitsViewport(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 1, 20, 3, 4, 5}
	lines := composedStatusLines(m)
	// The labels fit on the bar's line at 80 columns; they must NOT wrap.
	if len(lines) != 1 {
		t.Errorf("labels should stay on the bar's line when they fit, got %d lines: %q", len(lines), lines)
	}
	for _, l := range lines {
		if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
			t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
		}
	}
	// All labels must remain visible.
	joined := stripANSI(strings.Join(lines, "\n"))
	for _, label := range []string{"1xx:1", "2xx:20", "3xx:3", "4xx:4", "5xx:5"} {
		if !strings.Contains(joined, label) {
			t.Errorf("status bar should show %s, got: %s", label, joined)
		}
	}
}

func TestRenderStatusBar_FitsNarrowViewport(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 1, 20, 3, 4, 5}
	lines := composedStatusLines(m)
	// The 31-cell label group cannot share the bar's row at 40 columns,
	// so it must wrap onto a second row instead of being truncated.
	if len(lines) != 2 {
		t.Errorf("narrow viewport should wrap labels onto a second line, got %d lines: %q", len(lines), lines)
	}
	// The wrapped labels row is exactly "  " + the parts joined with
	// single spaces: 2 + 30 = 32 cells (labelsWidth is 31 here,
	// including the leading-space budget per part; the joined row
	// omits the trailing "  " the old single-line format added).
	if got := uniseg.StringWidth(stripANSI(lines[1])); got != 32 {
		t.Errorf("wrapped labels row width = %d, want 32; row = %q", got, stripANSI(lines[1]))
	}
	for _, l := range lines {
		if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
			t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
		}
	}
	// All labels must remain visible.
	joined := stripANSI(strings.Join(lines, "\n"))
	for _, label := range []string{"1xx:1", "2xx:20", "3xx:3", "4xx:4", "5xx:5"} {
		if !strings.Contains(joined, label) {
			t.Errorf("status bar should show %s, got: %s", label, joined)
		}
	}
}

func TestRenderStatusBar_OmitsZeroCounts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 0, 5, 0, 0, 0}
	lines := composedStatusLines(m)
	joined := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "2xx:5") {
		t.Errorf("should show the non-zero class 2xx:5, got: %q", joined)
	}
	for _, zero := range []string{"1xx:", "3xx:", "4xx:", "5xx:"} {
		if strings.Contains(joined, zero) {
			t.Errorf("zero-count class %q should be omitted, got: %q", zero, joined)
		}
	}
	// The composed line must still fit (budget matches render).
	for _, l := range lines {
		if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
			t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
		}
	}
}

func TestRenderStatusBar_MultiDigitSparseFitsAt80(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 0, 10_000_000, 0, 123, 12}
	lines := composedStatusLines(m)
	// The sparse labels fit at 80 columns; they must NOT wrap.
	if len(lines) != 1 {
		t.Errorf("labels should stay on the bar's line when they fit, got %d lines: %q", len(lines), lines)
	}
	joined := stripANSI(strings.Join(lines, "\n"))
	for _, label := range []string{"2xx:10.0M", "4xx:123", "5xx:12"} {
		if !strings.Contains(joined, label) {
			t.Errorf("status bar should show %s, got: %q", label, joined)
		}
	}
	for _, l := range lines {
		if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
			t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
		}
	}
}

// TestRenderStatusBar_AbbreviatesLargeCounts pins the count
// abbreviation: multi-billion status counts render abbreviated
// ("2xx:12.3B" style) and the composed line still fits, while ordinary
// counts below 10^7 render exactly as before.
func TestRenderStatusBar_AbbreviatesLargeCounts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.StatusCounts = [6]int64{0, 0, 12_345_678_901, 0, 0, 0}
	lines := composedStatusLines(m)
	joined := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "2xx:12.3B") {
		t.Errorf("multi-billion count should abbreviate to 2xx:12.3B, got: %q", joined)
	}
	for _, l := range lines {
		if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
			t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
		}
	}
	// Ordinary counts (< 10^7) must render exactly as before.
	m.snap.StatusCounts = [6]int64{0, 0, 90, 0, 0, 0}
	joined = stripANSI(strings.Join(composedStatusLines(m), "\n"))
	if !strings.Contains(joined, "2xx:90") {
		t.Errorf("ordinary counts should render exactly, got: %q", joined)
	}
}

func TestRenderStatusBar_WrapsAbortedOnNarrowViewport(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 27
	m.height = 24
	m.snap.TotalAborted = 99_999_999_999_999
	lines := composedStatusLines(m)
	// The Aborted label alone is wider than the viewport allows on the
	// bar's row, so it must wrap onto a second row.
	if len(lines) != 2 {
		t.Fatalf("aborted label should wrap on a narrow viewport, got %d lines: %q", len(lines), lines)
	}
	// The wrapped labels row is exactly "  " + the packed part: 2 + 13
	// = 15 cells for "Aborted:100T" (the abbreviated form of the
	// 14-digit count).
	want := 2 + uniseg.StringWidth("Aborted:100T")
	if got := uniseg.StringWidth(stripANSI(lines[1])); got != want {
		t.Errorf("wrapped aborted row width = %d, want %d; row = %q", got, want, stripANSI(lines[1]))
	}
	for _, l := range lines {
		if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
			t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
		}
	}
	if !strings.Contains(stripANSI(strings.Join(lines, "\n")), "Aborted:100T") {
		t.Errorf("wrapped line should show the aborted label, got: %q", lines)
	}
}

// TestRenderStatusBar_WrapsLabelsMultiRow pins the review-09 fix: with
// all six labels present at width 40 the wrapped labels pack into rows
// that each fit the viewport, and every part — including "Aborted" —
// stays visible. Single-digit counts pack the five status classes into
// one row (2 + 29 cells) with Aborted on its own row; abbreviated
// 12.3B counts pack three classes per row (2 + 29 and 2 + 33 cells).
// No row carries trailing padding: each is exactly "  " + the parts
// joined with single spaces.
func TestRenderStatusBar_WrapsLabelsMultiRow(t *testing.T) {
	cases := []struct {
		name    string
		counts  [6]int64
		aborted int64
		rows    []string // expected wrapped rows, bar line excluded
	}{
		{
			name:    "all-six-single-digit",
			counts:  [6]int64{0, 1, 2, 3, 4, 5},
			aborted: 6,
			rows:    []string{"  1xx:1 2xx:2 3xx:3 4xx:4 5xx:5", "  Aborted:6"},
		},
		{
			name:    "all-six-abbreviated",
			counts:  [6]int64{0, 12_345_678_901, 12_345_678_901, 12_345_678_901, 12_345_678_901, 12_345_678_901},
			aborted: 12_345_678_901,
			rows:    []string{"  1xx:12.3B 2xx:12.3B 3xx:12.3B", "  4xx:12.3B 5xx:12.3B Aborted:12.3B"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = 40
			m.height = 40
			m.snap.StatusCounts = tc.counts
			m.snap.TotalAborted = tc.aborted
			lines := composedStatusLines(m)
			if len(lines) != len(tc.rows)+1 {
				t.Fatalf("status labels should wrap onto %d rows, got %d lines: %q", len(tc.rows), len(lines), lines)
			}
			for i, want := range tc.rows {
				if got := stripANSI(lines[i+1]); got != want {
					t.Errorf("wrapped row %d = %q, want %q", i+1, got, want)
				}
			}
			for _, l := range lines {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("status line visible width = %d exceeds viewport %d; line = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
		})
	}
}

// TestRenderDualBars_WidthsAndClamps pins the dual-bars row to the
// viewport: for viewport widths >= 27 the row totals exactly
// viewportWidth() (labels, brackets, gap, and trailing spaces are 27
// fixed cells; the Active gauge and Queue bar split the rest), and it
// never exceeds the viewport at any width. Negative Active/Queued
// values (transiently possible: metrics.go Inc/Dec) must not panic —
// the filled widths are clamped to zero.
func TestRenderDualBars_WidthsAndClamps(t *testing.T) {
	widths := []int{28, 29, 40, 41, 80} // viewports 27, 28, 39, 40, 79
	values := []int64{0, 1, 2, 4, 8, 16, 32, -1}
	for _, conc := range []int{0, 4} {
		for _, width := range widths {
			t.Run(fmt.Sprintf("conc=%d/width=%d", conc, width), func(t *testing.T) {
				for _, active := range values {
					for _, queued := range values {
						m := NewModelForProviders([]ProviderMeta{{Concurrency: conc}})
						m.width = width
						m.height = 40
						m.snap.Active = active
						m.snap.Queued = queued
						s := stripANSI(m.renderDualBars(m.gaugeTrackWidth()))
						got := uniseg.StringWidth(s)
						vw := m.viewportWidth()
						if got > vw {
							t.Errorf("dual bars width = %d exceeds viewport %d (active=%d queued=%d): %q", got, vw, active, queued, s)
						}
						if vw >= 27 && got != vw {
							t.Errorf("dual bars width = %d, want exact viewport %d (active=%d queued=%d): %q", got, vw, active, queued, s)
						}
					}
				}
			})
		}
	}
}

// TestRenderDualBars_NoPanicNarrowViewport covers the documented
// degradation below 27 viewport cells: the fixed labels/brackets alone
// (27 cells) cannot fit, so the row is truncated downstream by
// renderContentWithScrollbar. The renderer itself must never panic,
// regardless of values or track widths.
func TestRenderDualBars_NoPanicNarrowViewport(t *testing.T) {
	for _, width := range []int{1, 20, 27} { // viewports 1, 19, 26
		for _, active := range []int64{0, -1, 100} {
			for _, queued := range []int64{0, -1, 100} {
				m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
				m.width = width
				m.height = 40
				m.snap.Active = active
				m.snap.Queued = queued
				s := m.renderDualBars(m.gaugeTrackWidth())
				if s == "" {
					t.Errorf("dual bars must render non-empty output (width=%d active=%d queued=%d)", width, active, queued)
				}
			}
		}
	}
}

// dashboardStatusLines extracts the Status section rows from rendered
// dashboard lines: the "  Status  " header row plus every immediately
// following non-empty row (the wrapped label rows, when the labels
// could not share the bar's line).

func TestFormatCount(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{90, "90"},
		{9_999_999, "9999999"},
		{10_000_000, "10.0M"},
		{12_345_678, "12.3M"},
		{99_949_999, "99.9M"},
		{99_950_000, "100M"},
		{999_999_999, "1000M"},
		{1_000_000_000, "1.0B"},
		{12_345_678_901, "12.3B"},
		{999_999_999_999, "1000B"},
		{1_000_000_000_000_000, "1.0P"},
		{9_223_372_036_854_775_807, "9.2E"},
	}
	for _, tt := range tests {
		if got := formatCount(tt.in); got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// The abbreviated display must never exceed 5 cells.
	for _, in := range []int64{10_000_000, 999_999_999, 999_999_999_999_999, 9_223_372_036_854_775_807} {
		if got := uniseg.StringWidth(formatCount(in)); got > 5 {
			t.Errorf("formatCount(%d) = %q exceeds 5 cells", in, formatCount(in))
		}
	}
}

func TestPackParts(t *testing.T) {
	parts := []string{
		"Clean proxied: 0",
		"Clean passthrough: 0",
		"Aborted: 0",
		"Timeouts: 0",
		"Cancelled: 0",
		"Circuit rejects: 0",
	}
	sep := "  │  "
	tests := []struct {
		vw   int
		want []string
	}{
		{
			vw: 119,
			want: []string{
				"Clean proxied: 0  │  Clean passthrough: 0  │  Aborted: 0  │  Timeouts: 0  │  Cancelled: 0  │  Circuit rejects: 0",
			},
		},
		{
			vw: 79,
			want: []string{
				"Clean proxied: 0  │  Clean passthrough: 0  │  Aborted: 0  │  Timeouts: 0",
				"Cancelled: 0  │  Circuit rejects: 0",
			},
		},
		{
			vw: 39,
			want: []string{
				"Clean proxied: 0",
				"Clean passthrough: 0  │  Aborted: 0",
				"Timeouts: 0  │  Cancelled: 0",
				"Circuit rejects: 0",
			},
		},
	}
	for _, tt := range tests {
		got := packParts(parts, tt.vw, sep)
		if len(got) != len(tt.want) {
			t.Errorf("packParts(vw=%d) = %q, want %q", tt.vw, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("packParts(vw=%d) row %d = %q, want %q", tt.vw, i, got[i], tt.want[i])
			}
			if uniseg.StringWidth(got[i]) > tt.vw {
				t.Errorf("packParts(vw=%d) row %d width = %d exceeds viewport; row = %q", tt.vw, i, uniseg.StringWidth(got[i]), got[i])
			}
		}
	}
	// A part wider than the viewport is emitted on its own row, never
	// split (the caller is responsible for bounding part widths).
	wide := []string{strings.Repeat("X", 50)}
	got := packParts(wide, 39, sep)
	if len(got) != 1 || got[0] != wide[0] {
		t.Errorf("over-wide part should be emitted unsplit, got %q", got)
	}
	// A tiny viewport must not emit spurious empty rows.
	if got := packParts(parts, 0, sep); len(got) != len(parts) {
		t.Errorf("packParts(vw=0) should emit one row per part, got %q", got)
	}
	// ANSI escapes in parts do not count toward the row width; the
	// escapes must survive in the emitted row.
	colored := []string{"\x1b[38;2;63;185;80mClean proxied: 0\x1b[m", "Clean passthrough: 0"}
	got = packParts(colored, 42, sep)
	if len(got) != 1 || !strings.Contains(got[0], "\x1b[38;2;63;185;80m") {
		t.Errorf("ANSI-colored parts should pack by visible width and keep escapes, got %q", got)
	}
}

// TestDashboard_StatusLineFitsViewport reproduces the status-row overflow:
// the composed production row ("  Status  " prefix + bar + labels) must fit
// the viewport and every non-zero status class (plus Aborted) must remain
// visible. It exercises the real dashboardLines() composition, including
// sparse distributions (the budget loop skips zero classes while the render
// loop used to print them all) and multi-digit counts.
