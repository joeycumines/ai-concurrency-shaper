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
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/rivo/uniseg"
)

func TestLogDetail_ShowsFullMessage(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	longLine := "This is a very long log line that definitely exceeds the 79-cell viewport width and should be fully visible in the detail view."
	m.logRing.Write([]byte(longLine + "\n"))
	m.cursor = 0
	m = update(m, special("enter"))

	detail := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detail, "Log Line 1") {
		t.Errorf("detail should show the log line heading, got: %q", detail)
	}
	// The text is wrapped, so verify the full content is present across
	// the joined rows (whitespace is normalized by the wrapping).
	joined := strings.Join(strings.Fields(detail), " ")
	if !strings.Contains(joined, longLine) {
		t.Errorf("detail should contain the full wrapped message, got: %q", detail)
	}
}

func TestLogDetail_WrapsToFitViewport(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	longLine := strings.Repeat("word ", 40)
	m.logRing.Write([]byte(longLine + "\n"))
	m.cursor = 0
	m = update(m, special("enter"))

	detail := m.renderDetailOverlay()
	for _, row := range strings.Split(detail, "\n") {
		if w := uniseg.StringWidth(stripANSI(row)); w > m.viewportWidth() {
			t.Errorf("detail row exceeds viewport width %d: %d cells %q", m.viewportWidth(), w, stripANSI(row))
		}
	}
}

func TestLogDetail_LongLineProducesMultipleRowsWithoutLoss(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	longLine := strings.Repeat("abcdefghij", 20) // 200 chars, no spaces
	m.logRing.Write([]byte(longLine + "\n"))
	m.cursor = 0
	m = update(m, special("enter"))

	detail := stripANSI(m.renderDetailOverlay())
	// The full text must be present across the wrapped rows (no loss).
	joined := strings.Join(strings.Fields(detail), "")
	if !strings.Contains(joined, longLine) {
		t.Errorf("wrapped detail must preserve the full 200-char text, got: %q", detail)
	}
}

// TestWrapText_WideGraphemeAtTinyWidth pins the word-split loop against an
// infinite loop: a grapheme cluster wider than the wrap width (a 2-cell emoji
// at width 1) must be emitted whole on its own row, not repeatedly re-truncated
// to "". Regression for a process freeze reachable from the log detail view on
// very narrow viewports.

func TestWrapText_WideGraphemeAtTinyWidth(t *testing.T) {
	done := make(chan []string, 1)
	go func() {
		done <- wrapText("⚡ wide start then more words here", 1)
	}()
	select {
	case rows := <-done:
		joined := strings.Join(rows, "")
		if !strings.Contains(joined, "⚡") {
			t.Error("wide emoji dropped by wrapping")
		}
		if joined != "⚡widestartthenmorewordshere" {
			t.Errorf("wrapText must not lose text, got %q", joined)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wrapText did not return: infinite loop on wide grapheme at width 1")
	}
}

// TestLogDetail_ClampedToVisibleRows pins that a message wrapping past the
// visible-row budget is truncated with an indicator instead of emitting an
// oversized frame (which would push the chrome into terminal scrollback).

func TestLogDetail_ClampedToVisibleRows(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte(strings.Repeat("word ", 2000) + "\n"))
	m.cursor = 0
	m = update(m, special("enter"))

	overlay := m.renderDetailOverlay()
	if n := countContentLines(overlay); n > m.visibleRows() {
		t.Errorf("detail overlay emits %d lines for a %d-row viewport", n, m.visibleRows())
	}
	if !strings.Contains(stripANSI(overlay), "more lines withheld") {
		t.Error("clamped detail must carry a truncation indicator")
	}
	if !strings.Contains(stripANSI(overlay), "[Esc/Enter] close") {
		t.Error("clamped detail must keep the close hint visible")
	}
}

func TestLogDetail_EmptySelectionDoesNotPanic(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.cursor = 0
	m.mode = modeDetail
	// No log lines: renderDetailOverlay must return "" without panicking.
	if s := m.renderDetailOverlay(); s != "" {
		t.Errorf("empty selection should render empty, got: %q", s)
	}
}

func TestLogDetail_EnterOpensAndEscCloses(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("hello\n"))
	m.cursor = 0

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatalf("after Enter: mode = %v, want modeDetail", m.mode)
	}
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mode != modeBrowse {
		t.Fatalf("after Escape: mode = %v, want modeBrowse", m.mode)
	}
}

// ─── T04: identity-pinned Logs detail view ───

func TestLogDetailPin_NewArrivalsDoNotChangeDisplay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	original := "original log message that stays displayed"
	m.logRing.Write([]byte(original + "\n"))
	m.cursor = 0

	// Open detail on the original line.
	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}
	detailBefore := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detailBefore, original) {
		t.Fatalf("setup: detail should show original, got: %q", detailBefore)
	}

	// New log lines arrive while the detail is open.
	for range 5 {
		m = update(m, logPollTickMsg{})
	}
	m.logRing.Write([]byte("new log line one\nnew log line two\nnew log line three\n"))
	m = update(m, logPollTickMsg{})

	// The overlay must still show the ORIGINAL message, not the new lines
	// that have shifted the cursor position.
	detailAfter := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detailAfter, original) {
		t.Errorf("detail must keep the original message after new arrivals, got: %q", detailAfter)
	}
	if strings.Contains(detailAfter, "new log line one") {
		t.Error("detail must not display a newer line that shifted into the anchored position")
	}
}

func TestLogDetailPin_EvictionClosesOverlay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	original := "original message that will be evicted"
	m.logRing.Write([]byte(original + "\n"))
	m.cursor = 0

	// Open detail on the original line.
	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}
	if !strings.Contains(stripANSI(m.renderDetailOverlay()), original) {
		t.Fatal("setup: detail should show original")
	}

	// Overflow the 2048-line ring to evict the original.
	for i := range 2050 {
		m.logRing.Write([]byte(fmt.Sprintf("filler %d\n", i)))
	}
	// 'x' is ignored by handleKey in modeDetail, so the Update-tail eviction
	// check is the only path that can close the overlay (a dismiss key like
	// space would mask whether eviction detection actually works).
	m = update(m, key('x'))

	// The overlay must close (mode back to browse) and the render must
	// not show the evicted message.
	if m.mode != modeBrowse {
		t.Errorf("mode after eviction = %v, want modeBrowse (overlay must close)", m.mode)
	}
}

func TestLogDetailPin_StillPresentAppendKeepsMessage(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	original := "still present message"
	m.logRing.Write([]byte(original + "\n"))
	m.cursor = 0

	m = update(m, special("enter"))

	// Append a few lines (no eviction).
	m.logRing.Write([]byte("later line A\nlater line B\n"))
	m = update(m, logPollTickMsg{})

	// The overlay must still show the original.
	detail := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detail, original) {
		t.Errorf("detail must keep the original after append, got: %q", detail)
	}
	if m.mode != modeDetail {
		t.Error("overlay must stay open when the item is still present")
	}
}

// ─── T04: identity-pinned Logs detail view (direct logRing writes) ───

func TestLogDetailPin_RingAppendKeepsOriginal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	original := "original log message that stays displayed"
	m.logRing.Write([]byte(original + "\n"))
	m.cursor = 0

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}

	// Write directly to the ring (new arrivals).
	m.logRing.Write([]byte("new line A\nnew line B\n"))

	// The overlay must still show the original.
	detail := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detail, original) {
		t.Errorf("detail must keep the original after ring append, got: %q", detail)
	}
}

func TestLogDetailPin_RingEvictionClosesOverlay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	original := "original message that will be evicted"
	m.logRing.Write([]byte(original + "\n"))
	m.cursor = 0

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}

	// Overflow the 2048-line ring to evict the original.
	var sb strings.Builder
	for i := range 2050 {
		fmt.Fprintf(&sb, "filler %d\n", i)
	}
	m.logRing.Write([]byte(sb.String()))

	// 'x' is ignored by handleKey in modeDetail, so the Update-tail eviction
	// check is the only path that can close the overlay.
	m = update(m, key('x'))

	// The overlay must close.
	if m.mode != modeBrowse {
		t.Errorf("mode after ring eviction = %v, want modeBrowse", m.mode)
	}
}

// TestLogDetailPin_DuplicateLinesKeepsSelectedOccurrence pins the sequence-
// number anchor against duplicate text: with two identical lines in the ring,
// opening detail on the SECOND occurrence must keep showing that occurrence
// after further appends. The committed text-scan anchor would resolve to the
// first occurrence instead.

func TestLogDetailPin_DuplicateLinesKeepsSelectedOccurrence(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	shared := "duplicate log line"
	m.logRing.Write([]byte("unrelated first line\n"))
	m.logRing.Write([]byte(shared + "\n"))
	m.cursor = 1 // the second occurrence of the shared text

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}
	detail := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detail, "Log Line 2") {
		t.Fatalf("setup: detail should show list position 2, got: %q", detail)
	}

	// Appends shift nothing relevant, but a later occurrence of the same text
	// must not hijack the anchor; the resolved position stays 2 (1-based).
	m.logRing.Write([]byte(shared + "\n"))
	detail = stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detail, "Log Line 2") {
		t.Errorf("detail must stay pinned to the second occurrence, got: %q", detail)
	}
	if m.mode != modeDetail {
		t.Error("overlay must stay open while the anchored occurrence is retained")
	}
}

// TestLogDetailPin_FilteredAnchorUsesFilteredList pins anchor creation under
// an active filter: Enter must anchor to the filtered list's item at m.cursor
// (the item the operator sees), not the unfiltered ring position.

func TestLogDetailPin_FilteredAnchorUsesFilteredList(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("noise one\n"))
	m.logRing.Write([]byte("target log message\n"))
	m.logRing.Write([]byte("noise two\n"))
	m.filterText = "target"
	m.cursor = 0 // position 0 of the FILTERED list = "target log message"

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}
	detail := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detail, "target log message") {
		t.Fatalf("detail should show the filtered list's item, got: %q", detail)
	}
	if !strings.Contains(detail, "Log Line 1") {
		t.Errorf("detail heading should be the filtered list position, got: %q", detail)
	}
}

// TestLogDetailPin_FilterChangeClosesOverlay pins that a filter edit which
// drops the anchored line closes the overlay instead of keeping it alive
// against the unfiltered ring.

func TestLogDetailPin_FilterChangeClosesOverlay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("target log message\n"))
	m.cursor = 0

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}

	// The operator edits the filter so the anchored line no longer matches.
	m.filterText = "nomatch"
	// 'x' is ignored by handleKey in modeDetail, so the Update-tail
	// unresolvable-anchor check is the only path that can close the overlay.
	m = update(m, key('x'))

	if m.mode != modeBrowse {
		t.Errorf("mode after filter change = %v, want modeBrowse (overlay must close)", m.mode)
	}
	if s := stripANSI(m.renderDetailOverlay()); s != "" {
		t.Errorf("renderDetailOverlay must render empty after the anchor is filtered out, got: %q", s)
	}
}

// ─── T05: Network detail identity pinning ───

func TestNetworkDetailPin_NewEntriesDoNotChangeDisplay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.journal = journal.New(8, 1024)
	m.journal.Record(&journal.Entry{
		ID:         1,
		Method:     "POST",
		URL:        mustParseURL("https://upstream.example/v1/original-path"),
		StatusCode: 200,
	})
	m.networkFiltered = m.computeVisibleNetworkEntries()
	m.cursor = 0

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}
	detailBefore := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detailBefore, "original-path") {
		t.Fatalf("setup: detail should show original entry, got: %q", detailBefore)
	}

	// Record newer entries while the detail is open.
	m.journal.Record(&journal.Entry{
		ID:         2,
		Method:     "GET",
		URL:        mustParseURL("https://upstream.example/v1/newer-path"),
		StatusCode: 200,
	})
	m = update(m, metrics.Snapshot{})

	// The overlay must still show the ORIGINAL entry.
	detailAfter := stripANSI(m.renderDetailOverlay())
	if !strings.Contains(detailAfter, "original-path") {
		t.Errorf("detail must keep the original entry after new records, got: %q", detailAfter)
	}
	if strings.Contains(detailAfter, "newer-path") {
		t.Error("detail must not display a newer entry")
	}
}

func TestNetworkDetailPin_EvictionClosesOverlay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.journal = journal.New(2, 1024)
	m.journal.Record(&journal.Entry{
		ID:         1,
		Method:     "POST",
		URL:        mustParseURL("https://upstream.example/v1/original"),
		StatusCode: 200,
	})
	m.networkFiltered = m.computeVisibleNetworkEntries()
	m.cursor = 0

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("setup: should be in detail mode")
	}

	// Overflow the 2-entry journal to evict the original.
	m.journal.Record(&journal.Entry{ID: 2, Method: "GET", URL: mustParseURL("https://upstream.example/v1/two"), StatusCode: 200})
	m.journal.Record(&journal.Entry{ID: 3, Method: "GET", URL: mustParseURL("https://upstream.example/v1/three"), StatusCode: 200})

	// 'x' is ignored by handleKey in modeDetail, so the Update-tail eviction
	// check is the only path that can close the overlay.
	m = update(m, key('x'))

	if m.mode != modeBrowse {
		t.Errorf("mode after journal eviction = %v, want modeBrowse", m.mode)
	}
}

// ─── T06: Help/footer text for new affordances ───
