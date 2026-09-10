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
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/rivo/uniseg"
)

func TestRenderContentWithScrollbar_ContainsScrollbarChars(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.updateScrollbars()
	s := m.renderContentWithScrollbar()
	stripped := stripANSI(s)
	if !strings.Contains(stripped, "█") && !strings.Contains(stripped, "│") {
		t.Error("renderContentWithScrollbar should contain scrollbar chars (█ or │)")
	}
}

func TestRenderContentWithScrollbar_NarrowWidth(t *testing.T) {
	for w := 5; w <= 80; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		s := m.renderContentWithScrollbar()
		if s == "" && w >= 5 {
			t.Errorf("width=%d: renderContentWithScrollbar returned empty", w)
		}
	}
}

// ─── Viewport: Edge Cases ───

func TestRenderContentWithScrollbar_RespectsCellWidth(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 24
	m.tab = tabRequests
	// CJK characters occupy two cells each; a path with several of them will
	// exceed the 39-cell content area unless truncateANSI uses visual width.
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/api/日本語説明文/tests", Status: 200, Duration: time.Millisecond},
	}

	m.updateScrollbars()
	contentWidth := m.viewportWidth()
	s := m.renderContentWithScrollbar()
	lines := strings.SplitSeq(s, "\n")
	for line := range lines {
		if line == "" {
			continue
		}
		stripped := stripANSI(line)
		cells := uniseg.StringWidth(stripped)
		// Data rows have a one-cell scrollbar column appended; header rows do
		// not. Either way the line must never exceed contentWidth+1.
		if cells > contentWidth+1 {
			t.Errorf("rendered line overflows %d content cells (got %d): %q", contentWidth, cells, line)
		}
		if !strings.Contains(line, "│") && !strings.Contains(line, "█") && cells > contentWidth {
			t.Errorf("header/empty line overflows %d content cells (got %d): %q", contentWidth, cells, line)
		}
	}
}

func TestRenderContentWithScrollbar_PreservesANSIWhenTruncated(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	// contentWidth is max(width-1, 1) = 37. The row is longer than that, so
	// truncation occurs in the path. The visible portion still contains the
	// green 2xx status style from the row, proving the row's own styling
	// survived ANSI-aware truncation and was not replaced by raw runes.
	m.width = 38
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/this/request/path/is/far/longer/than/thirty/seven/characters", Status: 200, Duration: time.Millisecond},
	}

	s := m.renderContentWithScrollbar()
	if !strings.Contains(s, "\x1b[") {
		t.Errorf("rendered content should contain ANSI sequences after truncation, got: %q", s)
	}
	// statusOkStyle foreground is #3FB950 -> "38;2;63;185;80".
	if !strings.Contains(s, "38;2;63;185;80") {
		t.Errorf("row style should survive truncation; expected green status ANSI sequence in: %q", s)
	}
}

func TestRenderContentWithScrollbar_NoCellUnderflow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	// contentWidth is max(width-1, 1) = 36. A request row has a fixed ASCII
	// prefix of 35 visible cells (two leading spaces + time/method/status/
	// duration/double-space), so appending a single CJK character makes the
	// row 37 cells wide. Truncation at 36 drops the CJK grapheme to stay
	// within the boundary, leaving a 35-cell visible string. Without padding,
	// the scrollbar column would collapse one cell leftward on that row.
	m.width = 37
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "日", Status: 200, Duration: time.Millisecond},
	}

	m.updateScrollbars()
	contentWidth := m.viewportWidth()
	if contentWidth != 36 {
		t.Fatalf("test assumption broken: contentWidth = %d, want 36", contentWidth)
	}

	var rowFound bool
	s := m.renderContentWithScrollbar()
	for line := range strings.SplitSeq(s, "\n") {
		if line == "" {
			continue
		}
		// Only inspect the actual request data row; the header and count rows
		// do not exercise the CJK boundary.
		if !strings.Contains(line, "POST") || !strings.Contains(line, "200") {
			continue
		}
		rowFound = true
		stripped := stripANSI(line)
		cells := uniseg.StringWidth(stripped)
		// Data rows carry the scrollbar column and must therefore occupy
		// exactly contentWidth+1 visible cells when padded correctly.
		if cells != contentWidth+1 {
			t.Errorf("CJK data row width = %d, want %d; line = %q", cells, contentWidth+1, line)
		}
		// Split the visible line at the contentWidth boundary so we can assert
		// that the content area itself is padded to contentWidth and the final
		// cell is the scrollbar column. This pins down the exact padding
		// behavior that prevents the scrollbar from collapsing leftward on odd
		// CJK boundaries.
		split, gotContentWidth := splitAtCells(stripped, contentWidth)
		if gotContentWidth != contentWidth {
			t.Errorf("CJK data row content width = %d, want %d (padding missing); line = %q", gotContentWidth, contentWidth, line)
		}
		if uniseg.StringWidth(stripped[split:]) != 1 {
			t.Errorf("expected final visible cell to be the scrollbar column; got %q", stripped[split:])
		}
	}
	if !rowFound {
		t.Error("rendered output did not contain the POST/200 request row")
	}
}

// splitAtCells returns the byte index in s after exactly width visible cells,
// and the number of visible cells accumulated up to that index. If width is
// larger than the width of s, it returns len(s) and the actual width. The
// input is assumed to contain no ANSI escape sequences.

func splitAtCells(s string, width int) (int, int) {
	var (
		split int
		seen  int
		state = -1
	)
	for i := 0; i < len(s); {
		cluster, _, w, newState := uniseg.FirstGraphemeClusterInString(s[i:], state)
		if seen+w > width {
			return split, seen
		}
		seen += w
		split = i + len(cluster)
		i += len(cluster)
		state = newState
	}
	return split, seen
}

// ─── Provider switcher (multi-provider) ───

func TestHScroll_NetworkShiftsTruncationWindow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.journal = journal.New(8, 1024)
	m.journal.Record(&journal.Entry{
		ID:         1,
		Method:     "POST",
		URL:        mustParseURL("https://upstream.example/v1/really-long-path-name/messages"),
		StatusCode: 200,
		Timing:     journal.Timing{QueueStart: time.Now(), QueueEnd: time.Now(), ResponseHeaders: time.Now().Add(time.Millisecond), ResponseComplete: time.Now().Add(2 * time.Millisecond)},
	})
	m.networkFiltered = m.computeVisibleNetworkEntries()
	m.cursor = 0

	// Baseline: the row is clipped to the viewport, so the waterfall column
	// content near the right edge is cut off.
	before := stripANSI(m.renderContentWithScrollbar())
	if !strings.Contains(before, "POST") {
		t.Fatalf("baseline Network render missing POST row: %q", before)
	}

	// "l" shifts right by one cell; the truncation window widens.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.hScroll[tabNetwork] != 1 {
		t.Fatalf("hScroll[Network] after right = %d, want 1", m.hScroll[tabNetwork])
	}
	after := stripANSI(m.renderContentWithScrollbar())
	if after == before {
		t.Error("right key must change the rendered Network rows (horizontal shift)")
	}

	// The cursor and vertical scroll are untouched by horizontal navigation.
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("cursor/scroll = %d/%d, want 0/0 after horizontal navigation", m.cursor, m.scroll)
	}

	// "h" shifts back; the render returns to the baseline clip.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.hScroll[tabNetwork] != 0 {
		t.Fatalf("hScroll[Network] after left = %d, want 0", m.hScroll[tabNetwork])
	}
	if stripANSI(m.renderContentWithScrollbar()) != before {
		t.Error("left back to 0 must restore the baseline Network render")
	}
}

func TestHScroll_NetworkClampsAtBounds(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.journal = journal.New(8, 1024)
	m.journal.Record(&journal.Entry{
		ID:         1,
		Method:     "POST",
		URL:        mustParseURL("https://upstream.example/v1/really-long-path-name/messages"),
		StatusCode: 200,
		Timing:     journal.Timing{QueueStart: time.Now(), QueueEnd: time.Now().Add(time.Millisecond), ResponseHeaders: time.Now().Add(2 * time.Millisecond), ResponseComplete: time.Now().Add(3 * time.Millisecond)},
	})
	m.networkFiltered = m.computeVisibleNetworkEntries()
	if m.maxHScroll() == 0 {
		t.Fatal("setup: Network rows must overflow the viewport for this test")
	}

	// Left at offset 0 stays at 0.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.hScroll[tabNetwork] != 0 {
		t.Fatalf("hScroll after left at 0 = %d, want 0", m.hScroll[tabNetwork])
	}

	// Far-right paging clamps at the widest row's overflow.
	for range 40 {
		m = update(m, tea.KeyPressMsg{Code: 'L', Text: "L"})
	}
	max := m.maxHScroll()
	if got := m.hScroll[tabNetwork]; got != max {
		t.Fatalf("hScroll after L paging = %d, want clamp %d", got, max)
	}

	// Far-left paging clamps back to 0.
	for range 40 {
		m = update(m, tea.KeyPressMsg{Code: 'H', Text: "H"})
	}
	if got := m.hScroll[tabNetwork]; got != 0 {
		t.Fatalf("hScroll after H paging = %d, want 0", got)
	}
}

func TestHScroll_HomeResetsNetwork(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.journal = journal.New(8, 1024)
	m.journal.Record(&journal.Entry{
		ID:         1,
		Method:     "POST",
		URL:        mustParseURL("https://upstream.example/v1/really-long-path-name/messages"),
		StatusCode: 200,
		Timing:     journal.Timing{QueueStart: time.Now(), QueueEnd: time.Now().Add(time.Millisecond), ResponseHeaders: time.Now().Add(2 * time.Millisecond), ResponseComplete: time.Now().Add(3 * time.Millisecond)},
	})
	m.networkFiltered = m.computeVisibleNetworkEntries()
	if m.maxHScroll() == 0 {
		t.Fatal("setup: Network rows must overflow the viewport for this test")
	}

	m = update(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.hScroll[tabNetwork] != 1 {
		t.Fatalf("setup: hScroll = %d, want 1", m.hScroll[tabNetwork])
	}
	m = update(m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.hScroll[tabNetwork] != 0 {
		t.Fatalf("hScroll after Home = %d, want 0", m.hScroll[tabNetwork])
	}
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("cursor/scroll = %d/%d, want 0/0 after Home", m.cursor, m.scroll)
	}
}

// TestHScroll_NoSnapOnShrunkenBound pins that a horizontal offset stranded
// above the current bound (after vertical navigation onto a page of short
// rows) is walked back one step per left keypress rather than teleporting to
// zero, and that rightward motion still clamps to the current bound.

func TestHScroll_NoSnapOnShrunkenBound(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	wide := "This log line is intentionally longer than the 79-cell viewport so it overflows and can be scrolled horizontally."
	m.logRing.Write([]byte(wide + "\n"))
	for range 30 {
		m = update(m, key('l'))
	}
	if m.hScroll[tabLogs] != 30 {
		t.Fatalf("setup: hScroll = %d, want 30", m.hScroll[tabLogs])
	}

	// Replace the content with short rows: the current bound drops to 0 while
	// the offset stays 30 (rendering shows empty space for the long rows).
	m.logRing = newLogRing(logRingCapacity)
	m.logRing.Write([]byte("short\n"))

	// Right motion clamps to the new bound.
	m = update(m, key('l'))
	if m.hScroll[tabLogs] != 0 {
		t.Fatalf("hScroll after right on short-only page = %d, want 0 (clamped)", m.hScroll[tabLogs])
	}

	// A stranded offset walks back one step per left press instead of
	// snapping to zero.
	m.hScroll[tabLogs] = 30
	m = update(m, key('h'))
	if m.hScroll[tabLogs] != 29 {
		t.Errorf("hScroll after left with stranded offset = %d, want 29 (one step, no snap)", m.hScroll[tabLogs])
	}
}

// TestHScroll_ShiftsAllDataRows pins that horizontal scrolling shifts every
// data row into the same coordinate space: a row shorter than the viewport
// must not keep its left edge while a longer neighbor row loses its cells to
// the shift (a torn table).

func TestHScroll_ShiftsAllDataRows(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte(strings.Repeat("a", 90) + "\n")) // overflows: shifts
	m.logRing.Write([]byte(strings.Repeat("b", 40) + "\n")) // fits: must shift too
	m.hScroll[tabLogs] = 10

	lines := strings.Split(m.renderContentWithScrollbar(), "\n")
	rowA := stripANSI(lines[0])
	rowB := stripANSI(lines[1])
	// The short row's line-number prefix ("       2  ") must be scrolled off
	// with the rest of its left edge; the row starts with its own content.
	if strings.Contains(rowB, "       2") {
		t.Errorf("short row kept its left edge while the long row shifted (torn table): %q", rowB)
	}
	if !strings.HasPrefix(strings.TrimRight(rowB, " "), "bbbbbbbbbb") {
		t.Errorf("short row must show its cells from the offset on, got %q", rowB)
	}
	// The long row is shifted and truncated to the viewport.
	if got := len(strings.TrimRight(rowA, " ")); got != m.viewportWidth() {
		t.Errorf("long row rendered %d cells, want %d", got, m.viewportWidth())
	}
}

// ─── T02: horizontal scrolling on the Logs tab ───

func TestHScroll_LogsShiftsTruncationWindow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	longLine := "This is a very long log line that definitely exceeds the 79-cell viewport width and should be scrollable horizontally."
	m.logRing.Write([]byte(longLine + "\n"))
	m.cursor = 0

	// Baseline: the long line is clipped to the viewport.
	before := stripANSI(m.renderContentWithScrollbar())
	if !strings.Contains(before, "This is a very") {
		t.Fatalf("baseline Logs render missing long line: %q", before)
	}
	if strings.Contains(before, longLine) {
		t.Fatal("baseline should truncate the long line")
	}

	// "l" shifts right by one cell; the visible window shifts.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.hScroll[tabLogs] != 1 {
		t.Fatalf("hScroll[Logs] after right = %d, want 1", m.hScroll[tabLogs])
	}
	after := stripANSI(m.renderContentWithScrollbar())
	if after == before {
		t.Error("right key must change the rendered Logs rows (horizontal shift)")
	}

	// Vertical cursor is untouched by horizontal navigation.
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("cursor/scroll = %d/%d, want 0/0 after horizontal navigation", m.cursor, m.scroll)
	}

	// "h" shifts back; the render returns to the baseline clip.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.hScroll[tabLogs] != 0 {
		t.Fatalf("hScroll[Logs] after left = %d, want 0", m.hScroll[tabLogs])
	}
	if stripANSI(m.renderContentWithScrollbar()) != before {
		t.Error("left back to 0 must restore the baseline Logs render")
	}
}

func TestHScroll_LogsClampsAtBounds(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("This log line is intentionally longer than the 79-cell viewport so it overflows and can be scrolled horizontally.\n"))
	if m.maxHScroll() == 0 {
		t.Fatal("setup: Logs rows must overflow the viewport for this test")
	}

	// Left at offset 0 stays at 0.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.hScroll[tabLogs] != 0 {
		t.Fatalf("hScroll after left at 0 = %d, want 0", m.hScroll[tabLogs])
	}

	// Far-right paging clamps at the widest row's overflow.
	for range 40 {
		m = update(m, tea.KeyPressMsg{Code: 'L', Text: "L"})
	}
	max := m.maxHScroll()
	if got := m.hScroll[tabLogs]; got != max {
		t.Fatalf("hScroll after L paging = %d, want clamp %d", got, max)
	}

	// Far-left paging clamps back to 0.
	for range 40 {
		m = update(m, tea.KeyPressMsg{Code: 'H', Text: "H"})
	}
	if got := m.hScroll[tabLogs]; got != 0 {
		t.Fatalf("hScroll after H paging = %d, want 0", got)
	}
}

func TestHScroll_LogsDoesNotPauseFollow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("long line that overflows the viewport width for horizontal scrolling test\n"))
	if m.maxHScroll() == 0 {
		t.Fatal("setup: Logs rows must overflow the viewport for this test")
	}
	if !m.followLogs {
		t.Fatal("setup: followLogs should be true on Logs tab")
	}

	// Horizontal navigation must NOT pause followLogs.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.hScroll[tabLogs] != 1 {
		t.Fatalf("hScroll after right = %d, want 1", m.hScroll[tabLogs])
	}
	if !m.followLogs {
		t.Error("horizontal navigation must not pause followLogs")
	}

	// Vertical navigation still pauses it.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.followLogs {
		t.Error("vertical navigation must pause followLogs")
	}
}

// ─── T03: Logs detail view ───
