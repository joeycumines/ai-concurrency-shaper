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
	"bytes"
	"fmt"
	"image/color"
	"math"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/viewport"
	"github.com/rivo/uniseg"
)

// helper: apply a message and return the updated model (type-asserted).
func update(m Model, msg tea.Msg) Model {
	m2, _ := m.Update(msg)
	return m2.(Model)
}

// helper: send a key by rune. Sets both Code and Text to properly
// simulate real terminal input where Key.Text is populated for
// printable characters.
func key(r rune) tea.Msg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// helper: send a special key by string (e.g. "enter", "esc", "down", "up").
// Caveat: this creates a KeyPressMsg with Text=k, which differs from real
// terminal events where special keys have Code=KeyXxx and Text="". This
// works because handleKey's switch statements match on msg.String(), which
// returns the same value for both representations. However, if a special
// key is NOT matched in a switch and falls through to the default case,
// this helper would incorrectly simulate a printable key (non-empty Text).
// For testing non-printable key rejection, use KeyPressMsg{Code: tea.KeyUp}
// directly (see TestFilterModeArrowKeysIgnored).
func special(k string) tea.Msg {
	return tea.KeyPressMsg{Text: k}
}

type safeBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// scrollbarTop returns the first terminal row that belongs to the scrollbar
// track for the current tab. It is offset past the fixed header rows so that
// the scrollbar aligns with the scrollable data area.
func scrollbarTop(m Model) int { return contentStartRow + m.contentHeaderRows() }

func TestViewRenders(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	m.height = 24
	v := m.View()
	if v.Content == "" {
		t.Fatal("View() returned empty content")
	}
	if !strings.Contains(v.Content, "shaper") {
		t.Errorf("View should contain 'shaper', got: %s", v.Content)
	}
}

func TestViewContainsAllTabs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 8}})
	m.width = 80
	m.height = 24
	v := m.View()
	for _, tab := range []string{"1 Overview", "2 Requests", "3 Network", "4 Logs", "5 Concurrency", "6 Routes"} {
		if !strings.Contains(v.Content, tab) {
			t.Errorf("View missing tab %q", tab)
		}
	}
}

func TestViewContainsKeybindings(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	for _, kb := range []string{"j/k:scroll", "?:help"} {
		if !strings.Contains(v.Content, kb) {
			t.Errorf("View missing keybinding %q", kb)
		}
	}
}

func TestViewWithEmptySnapshot(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap = metrics.NewCollector().Snapshot()
	v := m.View()
	if v.Content == "" {
		t.Fatal("View returned empty for zero snapshot")
	}
	if !strings.Contains(v.Content, "No requests yet") {
		t.Error("Should show 'No requests yet' for empty log")
	}
}

func TestTabSwitching(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	m = update(m, key('2'))
	if m.tab != tabRequests {
		t.Errorf("tab = %d, want tabRequests", m.tab)
	}

	m = update(m, key('3'))
	if m.tab != tabNetwork {
		t.Errorf("tab = %d, want tabNetwork", m.tab)
	}

	m = update(m, key('4'))
	if m.tab != tabLogs {
		t.Errorf("tab = %d, want tabLogs", m.tab)
	}

	m = update(m, key('5'))
	if m.tab != tabConcurrency {
		t.Errorf("tab = %d, want tabConcurrency", m.tab)
	}

	m = update(m, key('6'))
	if m.tab != tabRoutes {
		t.Errorf("tab = %d, want tabRoutes", m.tab)
	}

	m = update(m, key('1'))
	if m.tab != tabDashboard {
		t.Errorf("tab = %d, want tabDashboard", m.tab)
	}
}

func TestScrollDown(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	for range 50 {
		m.snap.LogEntries = append(m.snap.LogEntries, metrics.RequestLogEntry{
			Method: "POST", Path: "/v1/messages", Status: 200,
			Duration: time.Millisecond,
		})
	}

	for range 5 {
		m = update(m, key('j'))
	}
	if m.cursor != 5 {
		t.Errorf("cursor = %d, want 5", m.cursor)
	}
}

func TestScrollUp(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 5
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('k'))
	if m.cursor != 4 {
		t.Errorf("cursor = %d, want 4", m.cursor)
	}
}

func TestScrollStaysInBounds(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, key('k'))
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
}

func TestGoToTop(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 30
	m.scroll = 20
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('g'))
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("cursor=%d scroll=%d, want 0,0", m.cursor, m.scroll)
	}
}

func TestGoToBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('G'))
	if m.cursor != 49 {
		t.Errorf("cursor = %d, want 49", m.cursor)
	}
}

func TestDetailOverlayRequests(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{
			Time:     time.Date(2026, 6, 8, 12, 30, 45, 0, time.UTC),
			Method:   "POST",
			Path:     "/v1/messages",
			Status:   200,
			Duration: 150 * time.Millisecond,
			Limited:  true,
		},
	}

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}

	v := m.View()
	text := stripANSI(v.Content)

	checks := []struct {
		field string
		want  string
	}{
		{"header", "Request Detail"},
		{"method label", "Method:"},
		{"method value", "POST"},
		{"path label", "Path:"},
		{"path value", "/v1/messages"},
		{"status label", "Status:"},
		{"status value", "200"},
		{"duration label", "Duration:"},
		{"duration value", "150ms"},
		{"limited label", "Limited:"},
		{"limited value", "true"},
		{"close hint", "close"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("overlay missing %s: want %q", c.field, c.want)
		}
	}

	// Dismiss with Escape
	m = update(m, special("esc"))
	if m.mode != modeBrowse {
		t.Fatal("Escape should return to browse mode")
	}
	v2 := m.View()
	text2 := stripANSI(v2.Content)
	if strings.Contains(text2, "Request Detail") {
		t.Error("overlay should not be visible after Escape")
	}

	// Dismiss with Enter
	m = update(m, special("enter"))
	m = update(m, special("enter"))
	if m.mode != modeBrowse {
		t.Fatal("Enter should dismiss detail mode")
	}

	// Dismiss with Space
	m = update(m, special("enter"))
	m = update(m, key(' '))
	if m.mode != modeBrowse {
		t.Fatal("Space should dismiss detail mode")
	}
}

func TestDetailOverlayConcurrency(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{
		{ID: 42, Method: "POST", Path: "/v1/messages", Limited: true},
	}

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}

	v := m.View()
	text := stripANSI(v.Content)

	checks := []struct {
		field string
		want  string
	}{
		{"header", "In-Flight Detail"},
		{"id label", "ID:"},
		{"id value", "42"},
		{"method label", "Method:"},
		{"method value", "POST"},
		{"path label", "Path:"},
		{"path value", "/v1/messages"},
		{"limited label", "Limited:"},
		{"limited value", "true"},
		{"age label", "Age:"},
		{"total label", "Total:"},
		{"close hint", "close"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("overlay missing %s: want %q", c.field, c.want)
		}
	}

	m = update(m, special("esc"))
	if m.mode != modeBrowse {
		t.Fatal("Escape should return to browse mode")
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;31;42mbold red\x1b[0m", "bold red"},
		{"no codes here", "no codes here"},
		{"\x1b[2J\x1b[Hclear", "clear"},
	}
	for _, tt := range tests {
		got := stripANSI(tt.input)
		if got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestStripANSI_SwallowsNonCSISequences pins review-10 #5d: OSC payloads
// (terminated by BEL or ST) and DCS/SOS/PM/APC bodies (terminated by ST) must
// not leak their bytes into stripped output — logged text can never inject
// terminal control sequences into the Logs tab or a toast.
func TestStripANSI_SwallowsNonCSISequences(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"\x1b]0;evil\x07ok", "ok"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\after", "linkafter"},
		{"\x1bP+q544e\x1b\\tail", "tail"},
		{"\x1bXsos body\x1b\\end", "end"},
		{"\x1b^pm body\x1b\\end", "end"},
		{"\x1b_apc body\x1b\\end", "end"},
		{"head\x1b]0;mid\x07tail", "headtail"},
		{"\x1b]unterminated-eof", ""},
		{"\x1bPunterminated-eof", ""},
	}
	for _, tt := range tests {
		got := stripANSI(tt.input)
		if got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestStripANSI_StripsControlBytes pins review-14 #5: C0 controls other than
// tab, DEL, and the C1 block are stripped — both their UTF-8 encodings and
// stray raw bytes in the 0x80–0x9F range, which are the 8-bit control
// positions legacy terminals act on. Valid multibyte text must survive
// untouched even when its continuation bytes fall inside that range (the
// emoji row guards against any naive byte-level implementation). The \n row
// additionally pins the review-15 #1 rebuttal: no literal newline can survive
// stripping, so multi-line toast messages with unindented continuations are
// impossible to produce.
func TestStripANSI_StripsControlBytes(t *testing.T) {
	tests := []struct{ input, want string }{
		{"a\rb", "ab"},
		{"x\by", "xy"},
		{"tab\tkept", "tab\tkept"},
		{"\x00nul\x07bell", "nulbell"},
		{"del\x7f!", "del!"},
		{"lone\x9b2J", "lone2J"},
		{"caf\u00e9 stays", "caf\u00e9 stays"},
		{"caf\u00e9 \u009bok", "caf\u00e9 ok"},
		{"emoji \U0001F512 lock", "emoji \U0001F512 lock"},
		{"line1\nline2", "line1line2"},
	}
	for _, tt := range tests {
		if got := stripANSI(tt.input); got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestStripANSI_PreservesEncodedReplacementChar pins review-16 #1: a
// legitimately encoded U+FFFD (bytes EF BF BD) must survive stripping byte-for-
// byte. utf8.DecodeRuneInString reports utf8.RuneError for it (size 3), the
// same rune it reports for an invalid byte (size 1), so a decoder that keys on
// the rune alone corrupts the valid encoding down to its lone first byte.
// Stray-byte passthrough outside C1 and C1 dropping are re-pinned here so the
// size-based distinction cannot regress either contract.
func TestStripANSI_PreservesEncodedReplacementChar(t *testing.T) {
	tests := []struct{ input, want string }{
		{"\uFFFD", "\uFFFD"},
		{"bad \uFFFD ok", "bad \uFFFD ok"},
		{"\uFFFD\uFFFD", "\uFFFD\uFFFD"},
		{"\xff", "\xff"},
		{"lone\x9b2J", "lone2J"},
	}
	for _, tt := range tests {
		if got := stripANSI(tt.input); got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestToastToastWidth_Fallback pins review-14 #8: with the terminal size not
// yet known (or degenerately narrow) the toast width falls back to an 80-column
// assumption minus the four reserved margin cells, so startup toasts keep a
// right margin instead of rendering flush against the pane edge once the first
// WindowSizeMsg paints.
func TestToastToastWidth_Fallback(t *testing.T) {
	tests := []struct {
		width int
		want  int
	}{
		{0, 76},
		{-3, 76},
		{4, 76},
		{5, 1},
		{100, 96},
	}
	for _, tt := range tests {
		if got := toastToastWidth(tt.width); got != tt.want {
			t.Errorf("toastToastWidth(%d) = %d, want %d", tt.width, got, tt.want)
		}
	}
}

func TestSnapshotUpdate(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	snap := metrics.NewCollector().Snapshot()
	snap.Active = 3
	snap.Queued = 5
	snap.Throughput = 42.5
	m = update(m, snap)

	if m.snap.Active != 3 {
		t.Errorf("Active = %d, want 3", m.snap.Active)
	}
	if m.snap.Queued != 5 {
		t.Errorf("Queued = %d, want 5", m.snap.Queued)
	}
}

func TestWindowSize(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m = update(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 {
		t.Errorf("size = %dx%d, want 120x40", m.width, m.height)
	}
}

func TestRenderGaugeBar(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	g := m.renderGaugeBar(5, 10, 20)
	if !strings.Contains(g, "█") {
		t.Error("gauge should contain filled blocks")
	}
	if strings.Contains(g, "%") {
		t.Errorf("gauge should not contain a percentage, got: %s", g)
	}
	stripped := stripANSI(g)
	if got, want := uniseg.StringWidth(stripped), 3+20+1+2; got != want {
		t.Errorf("gauge visible width = %d, want %d; got: %q", got, want, stripped)
	}
	if !strings.HasSuffix(stripped, "]  ") {
		t.Errorf("gauge should have two-cell right padding, got: %q", stripped)
	}
}

func TestRenderGaugeBarOverflow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	g := m.renderGaugeBar(100, 10, 20)
	if strings.Contains(g, "%") {
		t.Errorf("gauge should not contain a percentage, got: %s", g)
	}
	stripped := stripANSI(g)
	if got, want := uniseg.StringWidth(stripped), 3+20+1+2; got != want {
		t.Errorf("gauge visible width = %d, want %d; got: %q", got, want, stripped)
	}
	if !strings.HasSuffix(stripped, "]  ") {
		t.Errorf("gauge should have two-cell right padding, got: %q", stripped)
	}
}

func TestRenderGaugeBarEmpty(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	g := m.renderGaugeBar(0, 10, 20)
	if strings.Contains(g, "█") && !strings.Contains(g, "░") {
		t.Error("empty gauge should not contain filled blocks")
	}
	stripped := stripANSI(g)
	if !strings.HasSuffix(stripped, "]  ") {
		t.Errorf("empty gauge should have two-cell right padding, got: %q", stripped)
	}
}

func TestRenderHBar_Padding(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	g := stripANSI(m.renderHBar(5, 10, 20, m.queueFillStyle(5, 10)))
	if !strings.HasPrefix(g, "  [") {
		t.Errorf("hbar should have two-cell left margin, got: %q", g)
	}
	if !strings.HasSuffix(g, "]  ") {
		t.Errorf("hbar should have two-cell right padding, got: %q", g)
	}
	if got, want := uniseg.StringWidth(g), 3+20+1+2; got != want {
		t.Errorf("hbar visible width = %d, want %d; got: %q", got, want, g)
	}
}

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
	// The two header/tab texts are white ink on blue fills; #FFFFFF vs #0969DA
	// measures ~5.2:1 and is included exactly because it is the tightest of the
	// light palette.
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

func TestGaugeFillStyleSeverity(t *testing.T) {
	cases := []struct {
		name     string
		pct      int
		wantHex  string
		wantBold bool
	}{
		{"blue at 59", 59, "#58A6FF", true},
		{"amber at 60", 60, "#D29922", true},
		{"amber at 89", 89, "#D29922", true},
		{"red at 90", 90, "#F85149", true},
		{"red at 95", 95, "#F85149", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			style := darkModel().gaugeFillStyle(c.pct)
			gotHex := hexString(style.GetForeground())
			if gotHex != c.wantHex {
				t.Errorf("gaugeFillStyle(%d) foreground = %s, want %s", c.pct, gotHex, c.wantHex)
			}
			if gotBold := style.GetBold(); gotBold != c.wantBold {
				t.Errorf("gaugeFillStyle(%d) bold = %v, want %v", c.pct, gotBold, c.wantBold)
			}
		})
	}
}

func TestQueueFillStyleSeverity(t *testing.T) {
	cases := []struct {
		name     string
		value    int
		max      int
		wantHex  string
		wantBold bool
	}{
		{"empty at zero", 0, 16, "#21262D", false},
		{"fractional visible", 1, 300, "#39D353", true},
		{"vivid green below half", 7, 16, "#39D353", true},
		{"orange at 50% bound", 8, 16, "#F0883E", true},
		{"orange at 89%", 14, 16, "#F0883E", true},
		{"red at 90%", 15, 16, "#F85149", true},
		{"red at full", 16, 16, "#F85149", true},
		{"invalid max zero", 0, 0, "#21262D", false},
		{"invalid max with value", 1, 0, "#21262D", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			style := darkModel().queueFillStyle(c.value, c.max)
			gotHex := hexString(style.GetForeground())
			if gotHex != c.wantHex {
				t.Errorf("queueFillStyle(%d, %d) foreground = %s, want %s", c.value, c.max, gotHex, c.wantHex)
			}
			if gotBold := style.GetBold(); gotBold != c.wantBold {
				t.Errorf("queueFillStyle(%d, %d) bold = %v, want %v", c.value, c.max, gotBold, c.wantBold)
			}
		})
	}
}

func TestGaugeBarWidth(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	if got, want := m.gaugeBarWidth(), m.hBarWidth(); got != want {
		t.Errorf("gaugeBarWidth() = %d, want hBarWidth() = %d", got, want)
	}
}

func TestAltScreenEnabled(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	if !v.AltScreen {
		t.Error("AltScreen should be enabled")
	}
}

func TestMouseModeEnabled(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %d, want MouseModeCellMotion", v.MouseMode)
	}
}

func TestRoutesTabSortedByTotal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRoutes
	m.snap.RouteStats = map[string]metrics.RouteStat{
		"POST /v1/chat/completions": {Total: 5},
		"POST /v1/messages":         {Total: 100},
		"POST /embeddings":          {Total: 1},
	}

	v := m.View()
	msgIdx := strings.Index(v.Content, "POST /v1/messages")
	chatIdx := strings.Index(v.Content, "POST /v1/chat/completions")
	if msgIdx < 0 || chatIdx < 0 {
		t.Fatal("missing route entries")
	}
	if msgIdx > chatIdx {
		t.Error("POST /v1/messages (total=100) should appear before chat/completions (total=5)")
	}
}

func TestRoutesTabDeterministicSort(t *testing.T) {
	// Three routes with the same total — must sort alphabetically every time.
	for iter := range 10 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = 80
		m.height = 24
		m.tab = tabRoutes
		m.snap.RouteStats = map[string]metrics.RouteStat{
			"POST /v1/messages":         {Total: 5},
			"POST /v1/chat/completions": {Total: 5},
			"POST /embeddings":          {Total: 5},
		}

		v := m.View()
		// All tied at 5 → alphabetical: embeddings, chat/completions, messages
		embIdx := strings.Index(v.Content, "POST /embeddings")
		chatIdx := strings.Index(v.Content, "POST /v1/chat/completions")
		msgIdx := strings.Index(v.Content, "POST /v1/messages")
		if chatIdx < 0 || embIdx < 0 || msgIdx < 0 {
			t.Fatalf("iter %d: missing route entries", iter)
		}
		if embIdx >= chatIdx {
			t.Errorf("iter %d: embeddings (%d) should precede chat/completions (%d)", iter, embIdx, chatIdx)
		}
		if chatIdx >= msgIdx {
			t.Errorf("iter %d: chat/completions (%d) should precede messages (%d)", iter, chatIdx, msgIdx)
		}
	}
}

func TestMouseClickTabBar(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	// Click on the first tab area (row 1)
	m = update(m, tea.MouseClickMsg{X: 3, Y: 1})
	if m.tab != tabDashboard {
		t.Errorf("expected tabDashboard after clicking first tab, got %d", m.tab)
	}
}

func TestStatusStyle(t *testing.T) {
	m := darkModel()
	for _, code := range []int{200, 201, 301, 404, 429, 500, 503, 0, 99} {
		_ = m.statusStyle(code) // just verify no panic
	}
}

func TestDashboardShowsSparkline(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m.snap.Sparkline = []int{0, 1, 3, 5, 8, 5, 3, 1, 0}

	v := m.View()
	if !strings.Contains(v.Content, "Throughput") {
		t.Error("Dashboard should contain throughput sparkline section")
	}
}

func TestSparklineFillStyleSeverity(t *testing.T) {
	cases := []struct {
		name string
		last int
		want string
	}{
		{"blue at 50%", 5, "#58A6FF"},
		{"amber at 70%", 7, "#D29922"},
		{"red at window max", 10, "#F85149"},
		{"blue at zero value", 0, "#58A6FF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hexString(darkModel().sparklineFillStyle(c.last, 10).GetForeground())
			if got != c.want {
				t.Errorf("sparklineFillStyle(%d, 10) foreground = %s, want %s", c.last, got, c.want)
			}
		})
	}
}

func TestSparklineEmptyState(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m.snap.Sparkline = nil
	v := m.View()
	if !strings.Contains(v.Content, m.styles.dimStyle2.Render("  —")) {
		t.Errorf("Empty sparkline should render a dim em dash, got: %s", v.Content)
	}
}

func TestQuit(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	_, cmd := m.Update(key('q'))
	if cmd == nil {
		t.Fatal("quit should return a command")
	}
	// The cmd should produce a QuitMsg
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected QuitMsg, got %T", msg)
	}
}

func TestInitStartsResyncLoop(t *testing.T) {
	if cmd := NewModelForProviders([]ProviderMeta{{Concurrency: 4}}).Init(); cmd == nil {
		t.Fatal("Init should start the periodic resync loop")
	}
}

func TestUpdateResyncTickSchedulesClearThenDraw(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	updated, cmd := m.Update(resyncTickMsg{})
	if cmd == nil {
		t.Fatal("resync tick should schedule clear-then-draw")
	}
	got := updated.(Model)
	if got.redrawEpoch != 0 {
		t.Fatalf("resync tick changed redrawEpoch = %d, want 0", got.redrawEpoch)
	}
}

func TestUpdateResyncDrawTogglesInvisibleMarker(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	before := m.View()

	updated, cmd := m.Update(resyncDrawMsg{})
	if cmd == nil {
		t.Fatal("resync draw should schedule the next tick")
	}
	got := updated.(Model)
	after := got.View()

	if got.redrawEpoch != 1 {
		t.Fatalf("redrawEpoch = %d, want 1", got.redrawEpoch)
	}
	if before.Content == after.Content {
		t.Fatal("resync draw did not change raw View.Content")
	}
	if got, want := stripANSI(before.Content), stripANSI(after.Content); got != want {
		t.Fatalf("resync marker changed visible content: before %q after %q", got, want)
	}
}

func TestResyncDoesNotInvalidateDashboardCache(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.tab = tabDashboard
	m.dashboardLinesCache = []string{"cached"}

	m = update(m, resyncTickMsg{})
	if len(m.dashboardLinesCache) != 1 || m.dashboardLinesCache[0] != "cached" {
		t.Fatalf("resync tick invalidated dashboard cache: %#v", m.dashboardLinesCache)
	}

	m = update(m, resyncDrawMsg{})
	if len(m.dashboardLinesCache) != 1 || m.dashboardLinesCache[0] != "cached" {
		t.Fatalf("resync draw invalidated dashboard cache: %#v", m.dashboardLinesCache)
	}
}

func TestProgramResyncClearsThenRedraws(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	var in bytes.Buffer
	out := &safeBuffer{}
	p := tea.NewProgram(
		m,
		tea.WithWindowSize(80, 24),
		tea.WithInput(&in),
		tea.WithOutput(out),
		tea.WithoutSignals(),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
	)

	errs := make(chan error, 1)
	go func() {
		_, err := p.Run()
		errs <- err
	}()

	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(out.String(), "shaper") {
			break
		}
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("program failed: %v", err)
			}
			t.Fatal("program exited before initial render")
		case <-deadline:
			t.Fatalf("timed out waiting for initial render; output=%q", out.String())
		default:
		}
	}

	p.Send(resyncTickMsg{})
	resyncDeadline := time.After(2 * time.Second)
	for {
		output := out.String()
		if secondClearIdx, ok := secondClearIndex(output); ok && strings.LastIndex(output, "shaper") > secondClearIdx {
			break
		}
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("program failed: %v", err)
			}
			t.Fatal("program exited before resync render")
		case <-resyncDeadline:
			t.Fatalf("timed out waiting for resync redraw; output=%q", out.String())
		default:
		}
	}
	p.Quit()

	exitDeadline := time.After(2 * time.Second)
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("program failed: %v", err)
		}
	case <-exitDeadline:
		t.Fatal("timed out waiting for program exit")
	}
}

func secondClearIndex(output string) (int, bool) {
	clearIdx := strings.Index(output, "\x1b[2J")
	if clearIdx < 0 {
		return 0, false
	}
	secondClearIdx := strings.Index(output[clearIdx+len("\x1b[2J"):], "\x1b[2J")
	if secondClearIdx < 0 {
		return 0, false
	}
	return clearIdx + len("\x1b[2J") + secondClearIdx, true
}

func TestArrowKeyScrolling(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("down arrow: cursor = %d, want 1", m.cursor)
	}

	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("up arrow: cursor = %d, want 0", m.cursor)
	}
}

func TestConcurrencyTabInFlight(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{
		{ID: 1, Method: "POST", Path: "/v1/messages", Limited: true},
		{ID: 2, Method: "GET", Path: "/health", Limited: false},
	}

	v := m.View()
	if !strings.Contains(v.Content, "POST") {
		t.Error("Concurrency tab should show in-flight methods")
	}
	if !strings.Contains(v.Content, "/v1/messages") {
		t.Error("Concurrency tab should show paths")
	}
}

func TestDashboardSummary(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.tab = tabDashboard
	m.snap.TotalProxied = 100
	m.snap.TotalPassThrough = 50
	m.snap.TotalTimeout = 3
	m.snap.TotalCancelled = 1
	m.snap.TotalCircuitRejected = 2
	m.snap.StatusCounts = [6]int64{0, 0, 90, 5, 8, 3}

	v := m.View()
	if !strings.Contains(v.Content, "Summary") {
		t.Error("Dashboard should have a Summary section")
	}
	if !strings.Contains(v.Content, "Clean proxied: 100") {
		t.Error("Dashboard should show clean proxied count")
	}
	if !strings.Contains(v.Content, "Clean passthrough: 50") {
		t.Error("Dashboard should show clean passthrough count")
	}
}

func TestDashboardSummaryShowsAborted(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 40
	m.tab = tabDashboard
	m.snap.TotalProxied = 10
	m.snap.TotalPassThrough = 5
	m.snap.TotalAborted = 2
	m.snap.StatusCounts = [6]int64{0, 0, 12, 0, 0, 0}

	v := m.View()
	content := stripANSI(v.Content)
	if !strings.Contains(content, "Aborted: 2") {
		t.Fatalf("Dashboard should show aborted count, got: %s", content)
	}
	bar := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	if !strings.Contains(bar, "Aborted:2") {
		t.Fatalf("status bar should label aborted committed statuses, got: %s", bar)
	}
}

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

func TestConcurrencyTabInFlightEmptyIsDim(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	s := m.renderConcurrency()
	if !strings.Contains(s, m.styles.dimStyle2.Render("  No requests in flight.\n")) {
		t.Errorf("Concurrency tab should show a dim empty in-flight message, got: %s", s)
	}
}

func TestHelpOverlayToggle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	// Press '?' to open help
	m = update(m, key('?'))
	if m.mode != modeHelp {
		t.Fatal("should be in help mode")
	}

	v := m.View()
	text := stripANSI(v.Content)
	if !strings.Contains(text, "Keybindings") {
		t.Errorf("help overlay should contain 'Keybindings', got:\n%s", text)
	}
	if !strings.Contains(text, "Ctrl") {
		t.Errorf("help should mention Ctrl key for quit")
	}

	// Press any key to dismiss
	m = update(m, key('q'))
	// 'q' in help mode dismisses help (doesn't quit)
	if m.mode != modeBrowse {
		t.Fatal("should be back in browse mode after ?")
	}
}

func TestHelpOverlayDismissWithAnyKey(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	m = update(m, key('?'))
	if m.mode != modeHelp {
		t.Fatal("should be in help mode")
	}

	// Dismiss with 'x'
	m = update(m, key('x'))
	if m.mode != modeBrowse {
		t.Fatal("any key should dismiss help")
	}
}

// The provider-switch binding documents the switcher, which only exists in
// multi-provider mode (or with a single named provider). A single unnamed
// provider must keep the legacy overlay byte-identical.
func TestHelpOverlaySingleProviderOmitsSwitchProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	if m.hasSwitcher() {
		t.Fatal("a single unnamed provider must not render a provider switcher")
	}
	if s := m.renderHelpOverlay(); strings.Contains(s, "Switch provider") {
		t.Errorf("single-provider help overlay must not document the provider switcher, got:\n%s", s)
	}
}

func TestHelpOverlayMultiProviderShowsSwitchProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24

	if !m.hasSwitcher() {
		t.Fatal("multiple providers must render a provider switcher")
	}
	s := m.renderHelpOverlay()
	for _, want := range []string{"Tab/Shift+Tab", "Switch provider"} {
		if !strings.Contains(s, want) {
			t.Errorf("multi-provider help overlay should document %q, got:\n%s", want, s)
		}
	}
}

// A single provider with an explicit name also renders the switcher, so its
// overlay documents the binding too.
func TestHelpOverlaySingleNamedProviderShowsSwitchProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	m.width = 80
	m.height = 24

	if !m.hasSwitcher() {
		t.Fatal("a single named provider renders a provider switcher")
	}
	if s := m.renderHelpOverlay(); !strings.Contains(s, "Switch provider") {
		t.Errorf("single-named-provider help overlay should document the provider switcher, got:\n%s", s)
	}
}

func TestFilterModeToggle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	// Press '/' to enter filter mode
	m = update(m, special("/"))
	if m.mode != modeFilter {
		t.Fatal("should be in filter mode")
	}
	if m.filterText != "" {
		t.Fatal("filter text should be empty initially")
	}
}

func TestFilterModeAccumulate(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	// Enter filter mode and type 'messages'
	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, key('e'))
	m = update(m, key('s'))

	if m.filterText != "mes" {
		t.Errorf("filterText = %q, want %q", m.filterText, "mes")
	}
}

func TestFilterModeEnterAccepts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, special("enter"))

	if m.mode != modeBrowse {
		t.Fatal("Enter should accept filter and return to browse")
	}
	if m.filterText != "m" {
		t.Errorf("filterText = %q, want %q", m.filterText, "m")
	}
}

func TestFilterModeEscClears(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, special("esc"))

	if m.mode != modeBrowse {
		t.Fatal("Esc should return to browse")
	}
	if m.filterText != "" {
		t.Errorf("filterText = %q, want empty", m.filterText)
	}
}

func TestFilterModeBackspace(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, key('e'))
	m = update(m, key('s'))
	m = update(m, special("backspace"))

	if m.filterText != "me" {
		t.Errorf("filterText = %q, want %q", m.filterText, "me")
	}
}

func TestFilterModeBackspaceMultiByte(t *testing.T) {
	// Verify that backspace removes a full rune, not just one byte.
	// The Japanese character '日' is 3 bytes in UTF-8.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	// Simulate typing multi-byte characters by setting filterText directly
	// and then pressing backspace to verify rune-aware deletion.
	m.filterText = "abc日"
	m = update(m, special("backspace"))

	if m.filterText != "abc" {
		t.Errorf("filterText = %q, want %q after backspace on multi-byte rune", m.filterText, "abc")
	}
	// Verify the string is valid UTF-8.
	if !utf8.ValidString(m.filterText) {
		t.Errorf("filterText is invalid UTF-8: %x", m.filterText)
	}

	// Backspace again removes 'c'.
	m = update(m, special("backspace"))
	if m.filterText != "ab" {
		t.Errorf("filterText = %q, want %q", m.filterText, "ab")
	}

	// Two multi-byte runes, delete one.
	m.filterText = "日月"
	m = update(m, special("backspace"))
	if m.filterText != "日" {
		t.Errorf("filterText = %q, want %q after deleting multi-byte rune", m.filterText, "日")
	}
}

func TestFilterFiltersEntries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "GET", Path: "/health", Status: 200},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
	}

	// Filter for "messages"
	entries := m.visibleEntries()
	if len(entries) != 4 {
		t.Fatalf("without filter: got %d entries, want 4", len(entries))
	}

	// Apply filter
	m.filterText = "messages"
	entries = m.visibleEntries()
	if len(entries) != 2 {
		t.Errorf("with filter 'messages': got %d entries, want 2", len(entries))
	}

	// Filter for "health"
	m.filterText = "health"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("with filter 'health': got %d entries, want 1", len(entries))
	}
	if entries[0].Path != "/health" {
		t.Errorf("filtered entry path = %q, want /health", entries[0].Path)
	}

	// Filter with no matches
	m.filterText = "nonexistent"
	entries = m.visibleEntries()
	if len(entries) != 0 {
		t.Errorf("with non-matching filter: got %d entries, want 0", len(entries))
	}
}

func TestFilterByMethod(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "GET", Path: "/v1/models", Status: 200},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
	}

	// Filter by method "POST"
	m.filterText = "post"
	entries := m.visibleEntries()
	if len(entries) != 2 {
		t.Errorf("filter 'post': got %d entries, want 2", len(entries))
	}

	// Filter by method "GET"
	m.filterText = "get"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter 'get': got %d entries, want 1", len(entries))
	}
}

func TestFilterByStatus(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
		{Method: "GET", Path: "/health", Status: 500},
	}

	// Filter by status code "429"
	m.filterText = "429"
	entries := m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter '429': got %d entries, want 1", len(entries))
	}

	// Filter by partial status "4" (matches 429)
	m.filterText = "4"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter '4': got %d entries, want 1", len(entries))
	}

	// Filter by "500"
	m.filterText = "500"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter '500': got %d entries, want 1", len(entries))
	}
}

func TestAdjustViewportClampsOnFilterShrink(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
		{Method: "GET", Path: "/health", Status: 200},
		{Method: "GET", Path: "/health", Status: 500},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
	}

	// Position cursor deep into the list.
	m.cursor = 4
	m.scroll = 4

	// Enter filter mode and shrink the list.
	m = update(m, key('/'))
	m = update(m, key('h')) // filter to "h" (3 entries: health x2, chat/completions)

	// After the filter keystroke, adjustViewport should clamp cursor/scroll.
	if m.cursor > m.maxCursor() {
		t.Errorf("cursor (%d) not clamped to maxCursor (%d)", m.cursor, m.maxCursor())
	}
	if m.scroll > m.maxScroll() {
		t.Errorf("scroll (%d) not clamped to maxScroll (%d)", m.scroll, m.maxScroll())
	}

	// Ensure a render would show rows (not a blank screen).
	entries := m.visibleEntries()
	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(entries))
	if start >= end {
		t.Errorf("render would be blank: start=%d >= end=%d", start, end)
	}
}

func TestTruncateRuneCount(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		// Short strings pass through unchanged.
		{"hello", 10, "hello"},
		{"", 5, ""},
		// Exact length - no truncation.
		{"hello", 5, "hello"},
		// Truncation with ellipsis (1 rune for ...).
		{"hello", 4, "hel…"},
		{"hello", 3, "he…"},
		{"hello", 2, "h…"},
		// maxLen == 1 -> just ellipsis.
		{"hello", 1, "…"},
		// maxLen == 0 -> empty.
		{"hello", 0, ""},
		// Multi-byte characters - truncation is rune-aware.
		{"日本語説明", 5, "日本語説明"},
		{"日本語説明", 4, "日本語…"},
		{"日本語説明", 3, "日本…"},
		{"日本語説明", 2, "日…"},
		{"日本語説明", 1, "…"},
	}
	for _, tt := range tests {
		got := truncate(tt.input, tt.maxLen)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) produced invalid UTF-8: %x", tt.input, tt.maxLen, got)
		}
	}
}

func TestTruncateBytesZeroAlloc(t *testing.T) {
	// Verify truncateBytes handles multi-byte UTF-8 correctly without
	// converting the entire byte slice to a string or rune array.
	// Semantics match truncate: total output ≤ maxRunes runes.
	tests := []struct {
		input []byte
		max   int
		want  string
	}{
		{[]byte("hello"), 10, "hello"},
		{[]byte("hello"), 5, "hello"},
		{[]byte("hello"), 4, "hel…"},
		{[]byte("hello"), 3, "he…"},
		{[]byte("hello"), 2, "h…"},
		{[]byte("hello"), 1, "…"},
		{[]byte("hello"), 0, ""},
		// Multi-byte: Japanese chars are 3 bytes each.
		{[]byte("日本語説明"), 5, "日本語説明"},
		{[]byte("日本語説明"), 4, "日本語…"},
		{[]byte("日本語説明"), 2, "日…"},
		{[]byte("日本語説明"), 1, "…"},
		{[]byte(""), 5, ""},
		// Mixed ASCII + multi-byte. "abc日本" = 5 runes.
		{[]byte("abc日本"), 5, "abc日本"},
		{[]byte("abc日本"), 4, "abc…"},
	}
	for _, tt := range tests {
		got := truncateBytes(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateBytes(%q, %d) produced invalid UTF-8: %x", tt.input, tt.max, got)
		}
	}
}

func TestFilterModeArrowKeysIgnored(t *testing.T) {
	// Verify that non-printable control keys (arrows, F-keys, etc.)
	// do NOT corrupt the filter text. In Bubble Tea v2, these keys
	// have Key.Text == "" (no printable characters), so they must
	// be silently discarded in the filter default handler.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	// Enter filter mode and type a character.
	m = update(m, special("/"))
	m = update(m, key('a'))
	if m.filterText != "a" {
		t.Fatalf("filterText = %q, want %q before arrow key", m.filterText, "a")
	}

	// Press Up arrow (special key code, no printable Text).
	// In real terminal input, Key.Code = KeyUp, Key.Text = "".
	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.filterText != "a" {
		t.Errorf("after KeyUp: filterText = %q, want %q (arrow key should not corrupt filter)", m.filterText, "a")
	}

	// Press Down arrow.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.filterText != "a" {
		t.Errorf("after KeyDown: filterText = %q, want %q (arrow key should not corrupt filter)", m.filterText, "a")
	}

	// Press F1 (function key).
	m = update(m, tea.KeyPressMsg{Code: tea.KeyF1})
	if m.filterText != "a" {
		t.Errorf("after F1: filterText = %q, want %q (F-key should not corrupt filter)", m.filterText, "a")
	}

	// Press Home.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.filterText != "a" {
		t.Errorf("after Home: filterText = %q, want %q (Home should not corrupt filter)", m.filterText, "a")
	}
}

func TestAddToast_DefaultDuration(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	tt := &toast.Toast{Message: "test"}
	m.AddToast(tt)
	if tt.Duration != defaultToastDuration {
		t.Errorf("Duration = %v, want %v", tt.Duration, defaultToastDuration)
	}
}

func TestAddToast_AppendsToSlice(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "first"})
	m.AddToast(&toast.Toast{Message: "second"})
	if len(m.toasts) != 2 {
		t.Fatalf("len(toasts) = %d, want 2", len(m.toasts))
	}
	if m.toasts[0].Message != "first" {
		t.Errorf("toasts[0].Message = %q, want %q", m.toasts[0].Message, "first")
	}
}

func TestToastExpired_PurgedInUpdate(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	tt := &toast.Toast{Message: "expires", Duration: 1 * time.Millisecond}
	m.AddToast(tt)
	time.Sleep(10 * time.Millisecond)
	m = update(m, metrics.Snapshot{})
	if len(m.toasts) != 0 {
		t.Errorf("len(toasts) = %d after expired toast purged, want 0", len(m.toasts))
	}
}

func TestToastNotExpired_KeptInUpdate(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	tt := &toast.Toast{Message: "alive", Duration: 5 * time.Second}
	m.AddToast(tt)
	m = update(m, metrics.Snapshot{})
	if len(m.toasts) != 1 {
		t.Errorf("len(toasts) = %d, want 1 (non-expired toast should be kept)", len(m.toasts))
	}
}

func TestToastStacking_ShowsMultipleToasts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "first", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "second", Duration: 5 * time.Second})
	v := m.View()
	content := stripANSI(v.Content)
	if !strings.Contains(content, "first") {
		t.Error("View should contain 'first' toast")
	}
	if !strings.Contains(content, "second") {
		t.Error("View should contain 'second' toast")
	}
}

func TestToastStacking_LimitsToThree(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "t1", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "t2", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "t3", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "t4", Duration: 5 * time.Second})
	v := m.View()
	content := stripANSI(v.Content)
	if strings.Contains(content, "t1") {
		t.Error("View should NOT contain oldest toast 't1' (max 3 visible)")
	}
	for _, msg := range []string{"t2", "t3", "t4"} {
		if !strings.Contains(content, msg) {
			t.Errorf("View should contain toast %q", msg)
		}
	}
}

func TestToastStacking_MostRecentAtBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "older", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "newer", Duration: 5 * time.Second})
	v := m.View()
	content := stripANSI(v.Content)
	olderIdx := strings.Index(content, "older")
	newerIdx := strings.Index(content, "newer")
	if olderIdx < 0 || newerIdx < 0 {
		t.Fatal("both toasts should be visible")
	}
	if olderIdx >= newerIdx {
		t.Error("older toast should appear before (above) newer toast")
	}
}

func TestToastStacking_NoToastsRendersNothing(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	v := m.View()
	content := stripANSI(v.Content)
	if content == "" {
		t.Fatal("View should render even without toasts")
	}
}

func TestToastNotShownInDetailMode(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{{Method: "POST", Path: "/v1/messages", Status: 200}}
	m.AddToast(&toast.Toast{Message: "test", Duration: 5 * time.Second})
	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}
	v := m.View()
	content := stripANSI(v.Content)
	if strings.Contains(content, "test") {
		t.Error("toast should not render in detail mode")
	}
}

func TestRequestsTabMarksAbortedRows(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Time: time.Now(), Method: "POST", Path: "/v1/messages", Status: 200, Aborted: true},
	}

	s := stripANSI(m.renderRequests())
	if !strings.Contains(s, "/v1/messages [aborted]") {
		t.Fatalf("Requests tab should mark aborted rows, got: %s", s)
	}
}

// ─── TUI-06: Scroll / Viewport Behavior ───

func TestVisibleRows_NormalTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	v := m.visibleRows()
	if v != 20 {
		t.Errorf("visibleRows = %d, want 20 (24-4)", v)
	}
}

func TestVisibleRows_FilterTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.mode = modeFilter
	m.filterText = "x"
	v := m.visibleRows()
	if v != 19 {
		t.Errorf("visibleRows = %d, want 19 (24-4-1)", v)
	}
}

func TestVisibleRows_ToastOverlay(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.AddToast(&toast.Toast{Message: "a", Duration: 5 * time.Second})
	m.AddToast(&toast.Toast{Message: "b", Duration: 5 * time.Second})
	v := m.visibleRows()
	if v != 18 {
		t.Errorf("visibleRows = %d, want 18 (24-4-2)", v)
	}
}

func TestVisibleRows_ConcurrencyTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	v := m.visibleRows()
	if v != 20 {
		t.Errorf("visibleRows = %d, want 20 (24-4)", v)
	}
}

func TestDataRows_PerTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	m.tab = tabRequests
	if got, want := m.dataRows(), 18; got != want {
		t.Errorf("requests dataRows = %d, want %d (header+count)", got, want)
	}

	m.tab = tabNetwork
	if got, want := m.dataRows(), 18; got != want {
		t.Errorf("network dataRows = %d, want %d (header+count)", got, want)
	}

	m.tab = tabLogs
	if got, want := m.dataRows(), 20; got != want {
		t.Errorf("logs dataRows = %d, want %d (no header row)", got, want)
	}

	m.tab = tabRoutes
	if got, want := m.dataRows(), 18; got != want {
		t.Errorf("routes dataRows = %d, want %d (header+count)", got, want)
	}

	m.tab = tabConcurrency
	if got, want := m.dataRows(), 10; got != want {
		t.Errorf("concurrency dataRows = %d, want %d (10 fixed rows)", got, want)
	}

	m.tab = tabDashboard
	if got, want := m.dataRows(), 20; got != want {
		t.Errorf("dashboard dataRows = %d, want %d", got, want)
	}
}

func TestVisibleRows_MatchesHeightLessChrome(t *testing.T) {
	// visibleRows is the space between chrome and footer; it may be zero at
	// the documented minimum height of 4.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 4
	if got, want := m.visibleRows(), 0; got != want {
		t.Errorf("height=4: visibleRows = %d, want %d", got, want)
	}
	m.height = 8
	if got, want := m.visibleRows(), 4; got != want {
		t.Errorf("height=8: visibleRows = %d, want %d", got, want)
	}
}

func TestMaxCursor_Dashboard(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	// The dashboard is now scrollable when its content exceeds the viewport.
	got := m.maxCursor()
	if got <= 0 {
		t.Errorf("maxCursor on dashboard = %d, want > 0 in the default 24-line terminal", got)
	}
}

func TestMaxCursor_Requests(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
	if m.maxCursor() != 4 {
		t.Errorf("maxCursor = %d, want 4", m.maxCursor())
	}
}

func TestMaxCursor_Logs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("line1\nline2\nline3\n"))
	if m.maxCursor() != 2 {
		t.Errorf("maxCursor = %d, want 2", m.maxCursor())
	}
}

func TestMaxCursor_Concurrency(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = make([]metrics.InFlightEntry, 3)
	if m.maxCursor() != 2 {
		t.Errorf("maxCursor = %d, want 2", m.maxCursor())
	}
}

func TestMaxCursor_Routes(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRoutes
	m.snap.RouteStats = map[string]metrics.RouteStat{
		"POST /a": {Total: 1},
		"POST /b": {Total: 2},
	}
	if m.maxCursor() != 1 {
		t.Errorf("maxCursor = %d, want 1", m.maxCursor())
	}
}

func TestMaxScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	ms := m.maxScroll()
	if ms < 0 {
		t.Errorf("maxScroll = %d, want >= 0", ms)
	}
}

func TestAdjustViewport_ScrollFollowsCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 30
	m.scroll = 0
	m.adjustViewport()
	if m.scroll == 0 {
		t.Error("scroll should have moved to follow cursor")
	}
}

func TestAdjustViewport_CursorClampedToMax(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 3)
	m.cursor = 100
	m.adjustViewport()
	if m.cursor > m.maxCursor() {
		t.Errorf("cursor = %d, want <= %d", m.cursor, m.maxCursor())
	}
}

func TestAdjustViewport_ScrollClampedToMax(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 3)
	m.scroll = 100
	m.adjustViewport()
	if m.scroll > m.maxScroll() {
		t.Errorf("scroll = %d, want <= %d", m.scroll, m.maxScroll())
	}
}

func TestMoveCursor_ClampsAtZero(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.moveCursor(-5)
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
}

func TestMoveCursor_ClampsAtMax(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 3)
	m.moveCursor(100)
	if m.cursor != m.maxCursor() {
		t.Errorf("cursor = %d, want %d", m.cursor, m.maxCursor())
	}
}

func TestSwitchTab_ResetsCursorAndScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 10
	m.scroll = 5
	m.switchTab(tabLogs)
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 after switchTab", m.cursor)
	}
	if m.scroll != 0 {
		t.Errorf("scroll = %d, want 0 after switchTab", m.scroll)
	}
}

func TestPageDown_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m = update(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.cursor == 0 {
		t.Error("PageDown should move cursor")
	}
}

func TestPageUp_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 20
	m = update(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.cursor >= 20 {
		t.Error("PageUp should decrease cursor")
	}
}

func TestHomeKey_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 30
	m = update(m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.cursor != 0 {
		t.Errorf("Home: cursor = %d, want 0", m.cursor)
	}
}

func TestEndKey_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.cursor != m.maxCursor() {
		t.Errorf("End: cursor = %d, want %d", m.cursor, m.maxCursor())
	}
}

func TestCtrlU_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 20
	m = update(m, tea.KeyPressMsg{Text: "ctrl+u"})
	if m.cursor >= 20 {
		t.Error("Ctrl-U should decrease cursor")
	}
}

func TestCtrlD_TUI06(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m = update(m, tea.KeyPressMsg{Text: "ctrl+d"})
	if m.cursor == 0 {
		t.Error("Ctrl-D should increase cursor")
	}
}

// ─── TUI-07: logRing / logWriter ───

func TestLogRing_WriteSingleLine(t *testing.T) {
	r := newLogRing(10)
	r.Write([]byte("hello"))
	snap := r.snapshot()
	if len(snap) != 1 || snap[0] != "hello" {
		t.Errorf("snapshot = %v, want [hello]", snap)
	}
}

func TestLogRing_WriteMultipleLines(t *testing.T) {
	r := newLogRing(10)
	r.Write([]byte("line1\nline2\nline3\n"))
	snap := r.snapshot()
	if len(snap) != 3 {
		t.Errorf("len(snapshot) = %d, want 3", len(snap))
	}
	if snap[0] != "line1" || snap[1] != "line2" || snap[2] != "line3" {
		t.Errorf("snapshot = %v, want [line1 line2 line3]", snap)
	}
}

func TestLogRing_WriteEmptyLinesSkipped(t *testing.T) {
	r := newLogRing(10)
	r.Write([]byte("\n\nhello\n\n"))
	snap := r.snapshot()
	if len(snap) != 1 || snap[0] != "hello" {
		t.Errorf("snapshot = %v, want [hello]", snap)
	}
}

func TestLogRing_SnapshotEmpty(t *testing.T) {
	r := newLogRing(10)
	snap := r.snapshot()
	if snap != nil {
		t.Errorf("snapshot = %v, want nil", snap)
	}
}

func TestLogRing_CapacityOverflow(t *testing.T) {
	r := newLogRing(3)
	r.Write([]byte("a\n"))
	r.Write([]byte("b\n"))
	r.Write([]byte("c\n"))
	r.Write([]byte("d\n"))
	snap := r.snapshot()
	if len(snap) != 3 {
		t.Fatalf("len(snapshot) = %d, want 3", len(snap))
	}
	if snap[0] != "b" || snap[1] != "c" || snap[2] != "d" {
		t.Errorf("snapshot = %v, want [b c d] (oldest evicted)", snap)
	}
}

func TestLogWriter_DelegatesToRing(t *testing.T) {
	r := newLogRing(10)
	w := &logWriter{ring: r}
	n, err := w.Write([]byte("via writer\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Errorf("n = %d, want 11", n)
	}
	snap := r.snapshot()
	if len(snap) != 1 || snap[0] != "via writer" {
		t.Errorf("snapshot = %v, want [via writer]", snap)
	}
}

func TestLogRing_ConcurrentWrite(t *testing.T) {
	r := newLogRing(100)
	done := make(chan struct{})
	for i := range 10 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 100 {
				r.Write(fmt.Appendf(nil, "g%d-line%d\n", i, j))
			}
		}(i)
	}
	for range 10 {
		<-done
	}
	snap := r.snapshot()
	if len(snap) == 0 {
		t.Error("expected some log lines after concurrent writes")
	}
}

// ─── TUI-08: visibleLogLines / renderLogs ───

func TestVisibleLogLines_NoFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("alpha\nbeta\ngamma\n"))
	lines := m.visibleLogLines()
	if len(lines) != 3 {
		t.Errorf("len = %d, want 3", len(lines))
	}
}

func TestVisibleLogLines_WithFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("alpha\nbeta\ngamma\n"))
	m.filterText = "alpha"
	lines := m.visibleLogLines()
	if len(lines) != 1 {
		t.Errorf("len = %d, want 1", len(lines))
	}
	if lines[0] != "alpha" {
		t.Errorf("lines[0] = %q, want alpha", lines[0])
	}
}

func TestVisibleLogLines_EmptyRing(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	lines := m.visibleLogLines()
	if lines != nil {
		t.Errorf("lines = %v, want nil", lines)
	}
}

func TestRenderLogs_NoOutput(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	s := m.renderLogs()
	if !strings.Contains(s, "No log output") {
		t.Errorf("renderLogs should mention 'No log output', got: %s", s)
	}
}

func TestRenderLogs_WithOutput(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("hello world\n"))
	s := m.renderLogs()
	if !strings.Contains(s, "hello world") {
		t.Errorf("renderLogs should contain 'hello world', got: %s", s)
	}
}

func TestRenderLogs_WithFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("alpha\nbeta\n"))
	m.filterText = "alpha"
	s := m.renderLogs()
	if !strings.Contains(s, "Filter:") {
		t.Errorf("renderLogs should show filter indicator, got: %s", s)
	}
}

// ─── TUI-09: Network Filtering / Rendering ───

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestNetworkFilterType_Cycle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	if m.networkFilterType != networkFilterAll {
		t.Errorf("initial type = %d, want all", m.networkFilterType)
	}
	m = update(m, key('t'))
	if m.networkFilterType != networkFilterJSON {
		t.Errorf("after one 't': type = %d, want json", m.networkFilterType)
	}
	for i := 1; i < 5; i++ {
		m = update(m, key('t'))
	}
	if m.networkFilterType != networkFilterAll {
		t.Errorf("after cycling: type = %d, want all", m.networkFilterType)
	}
}

func TestNetworkFilterStatus_Cycle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	if m.networkFilterStatus != networkStatusAll {
		t.Errorf("initial status = %d, want all", m.networkFilterStatus)
	}
	m = update(m, key('s'))
	if m.networkFilterStatus != networkStatus2xx {
		t.Errorf("after one 's': status = %d, want 2xx", m.networkFilterStatus)
	}
	for i := 1; i < 4; i++ {
		m = update(m, key('s'))
	}
	if m.networkFilterStatus != networkStatusAll {
		t.Errorf("after cycling: status = %d, want all", m.networkFilterStatus)
	}
}

func TestComputeVisibleNetworkEntries_NoJournal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	entries := m.computeVisibleNetworkEntries()
	if entries != nil {
		t.Errorf("entries = %v, want nil with no journal", entries)
	}
}

func TestComputeVisibleNetworkEntries_WithTypeFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json"})
	j.Record(&journal.Entry{Method: "GET", URL: mustParseURL("/health"), StatusCode: 200, ContentType: "text/html"})
	m.journal = j
	m.networkFilterType = networkFilterJSON
	entries := m.computeVisibleNetworkEntries()
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1 (json only)", len(entries))
	}
}

func TestComputeVisibleNetworkEntries_WithStatusFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json"})
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 429, ContentType: "application/json"})
	m.journal = j
	m.networkFilterStatus = networkStatus4xx
	entries := m.computeVisibleNetworkEntries()
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1 (4xx only)", len(entries))
	}
}

func TestComputeVisibleNetworkEntries_WithTextFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json"})
	j.Record(&journal.Entry{Method: "GET", URL: mustParseURL("/health"), StatusCode: 200, ContentType: "text/html"})
	m.journal = j
	m.filterText = "messages"
	entries := m.computeVisibleNetworkEntries()
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1 (text filter)", len(entries))
	}
}

func TestRenderNetwork_EmptyEntries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	s := m.renderNetwork()
	if !strings.Contains(s, "No network entries") {
		t.Errorf("should mention 'No network entries', got: %s", s)
	}
}

func TestRenderNetworkMarksAbortedEntries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	e := &journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json", Aborted: true}
	j.Record(e)
	m.journal = j
	m.networkFiltered = m.computeVisibleNetworkEntries()

	s := stripANSI(m.renderNetwork())
	if !strings.Contains(s, "abort") {
		t.Fatalf("Network tab should mark aborted entries, got: %s", s)
	}
	detail := stripANSI(m.renderNetworkDetail(e))
	if !strings.Contains(detail, "Outcome:  aborted") {
		t.Fatalf("Network detail should show aborted outcome, got: %s", detail)
	}
}

func TestRenderNetwork_WithFilterIndicators(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.networkFilterType = networkFilterJSON
	m.networkFilterStatus = networkStatus2xx
	s := m.renderNetwork()
	if !strings.Contains(s, "type:json") {
		t.Errorf("should show type filter indicator, got: %s", s)
	}
	if !strings.Contains(s, "status:2xx") {
		t.Errorf("should show status filter indicator, got: %s", s)
	}
}

// ─── TUI-10: Scrollbar, Status Bar, canInspect, Overlays ───

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
func dashboardStatusLines(lines []string) []string {
	var out []string
	for i, l := range lines {
		if !strings.HasPrefix(stripANSI(l), "  Status  ") {
			continue
		}
		out = append(out, l)
		for j := i + 1; j < len(lines) && lines[j] != ""; j++ {
			out = append(out, lines[j])
		}
	}
	return out
}

// summaryRows returns the metric rows of the Summary section: every
// dashboard line after the " Summary " section header (the last
// section rendered by dashboardLines).
func summaryRows(lines []string) []string {
	for i, l := range lines {
		if stripANSI(l) == " Summary " {
			return lines[i+1:]
		}
	}
	return nil
}

// TestDashboard_SummaryFitsViewport reproduces the Summary section
// overflow: the composed Summary rows must fit within the viewport and
// every metric must remain visible at widths 40, 80, and 120. The
// single pre-fix row renders 114 cells (measured), overflowing the
// 39- and 79-cell viewports and silently truncating the rightmost
// metrics.
func TestDashboard_SummaryFitsViewport(t *testing.T) {
	tests := []struct {
		name  string
		width int
		set   func(*metrics.Snapshot)
	}{
		{name: "zero-widths-40", width: 40},
		{name: "zero-widths-80", width: 80},
		{name: "zero-widths-120", width: 120},
		{
			name:  "multidigit-widths-40",
			width: 40,
			set: func(s *metrics.Snapshot) {
				s.TotalProxied = 123456789
				s.TotalPassThrough = 987654321
				s.TotalAborted = 12345
				s.TotalTimeout = 67890
				s.TotalCancelled = 111213
				s.TotalCircuitRejected = 141516
			},
		},
		{
			name:  "multidigit-widths-80",
			width: 80,
			set: func(s *metrics.Snapshot) {
				s.TotalProxied = 123456789
				s.TotalPassThrough = 987654321
				s.TotalAborted = 12345
				s.TotalTimeout = 67890
				s.TotalCancelled = 111213
				s.TotalCircuitRejected = 141516
			},
		},
		{
			name:  "multidigit-widths-120",
			width: 120,
			set: func(s *metrics.Snapshot) {
				s.TotalProxied = 123456789
				s.TotalPassThrough = 987654321
				s.TotalAborted = 12345
				s.TotalTimeout = 67890
				s.TotalCancelled = 111213
				s.TotalCircuitRejected = 141516
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			if tt.set != nil {
				tt.set(&m.snap)
			}
			rows := summaryRows(m.dashboardLines())
			if len(rows) == 0 {
				t.Fatal("dashboard should render Summary metric rows")
			}
			for _, l := range rows {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("summary row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
			joined := strings.Join(rows, "\n")
			for _, label := range []string{"Clean proxied:", "Clean passthrough:", "Aborted:", "Timeouts:", "Cancelled:", "Circuit rejects:"} {
				if !strings.Contains(joined, label) {
					t.Errorf("summary should show %q, got: %q", label, joined)
				}
			}
		})
	}
}

// inflightSectionRows returns the rows of the In-Flight Requests
// section: every dashboard line after the section header up to the
// next blank line (the summary line(s) plus the per-request entries).
func inflightSectionRows(lines []string) []string {
	for i, l := range lines {
		if stripANSI(l) == " In-Flight Requests " {
			var out []string
			for j := i + 1; j < len(lines) && lines[j] != ""; j++ {
				out = append(out, lines[j])
			}
			return out
		}
	}
	return nil
}

// TestDashboard_InFlightSummaryFitsViewport reproduces the In-Flight
// summary overflow (review-09): "  N in-flight: L limited, P
// passthrough" spans exactly 39 cells for single-digit values — the
// entire 40-column viewport — and exceeds it the moment any value
// reaches two digits, so renderContentWithScrollbar silently truncates
// "passthrough". The summary must fit at any magnitude: counts are
// abbreviated via formatCount and, when the composed line still cannot
// fit, the parts pack into viewport-fitting rows. The absurd-magnitude
// cases deliberately exercise each counter independently (a consistent
// snapshot with int64-max limited AND passthrough counts would need an
// unallocatable slice; the render math treats the three numbers
// independently, and TestDashboard_AllLinesFitViewport covers the
// consistent path).
func TestDashboard_InFlightSummaryFitsViewport(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		flights   int
		limited   int64
		passthru  int64
		singleRow bool // renders the single-line "N in-flight: L limited, P passthrough" format
	}{
		{name: "single-digit", width: 40, flights: 2, limited: 1, passthru: 1, singleRow: true},
		{name: "double-digit", width: 40, flights: 10, limited: 8, passthru: 2},
		{name: "multi-digit", width: 40, flights: 123, limited: 65, passthru: 58},
		{name: "absurd", width: 40, flights: 3, limited: 9_223_372_036_854_775_807, passthru: 9_223_372_036_854_775_807},
		{name: "wide-single-digit", width: 80, flights: 2, limited: 1, passthru: 1, singleRow: true},
		{name: "wide-absurd", width: 80, flights: 3, limited: 9_223_372_036_854_775_807, passthru: 9_223_372_036_854_775_807, singleRow: true},
		{name: "wide-absurd-120", width: 120, flights: 3, limited: 9_223_372_036_854_775_807, passthru: 9_223_372_036_854_775_807, singleRow: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/width=%d", tt.name, tt.width), func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			m.snap.InFlight = make([]metrics.InFlightEntry, tt.flights)
			m.snap.InFlightLimited = tt.limited
			m.snap.InFlightPassthrough = tt.passthru

			rows := inflightSectionRows(m.dashboardLines())
			if len(rows) == 0 {
				t.Fatal("dashboard should render the In-Flight Requests section")
			}
			joined := strings.Join(rows, "\n")
			for _, label := range []string{"in-flight", "limited", "passthrough"} {
				if !strings.Contains(joined, label) {
					t.Errorf("in-flight summary should show %q, got: %q", label, joined)
				}
			}
			if tt.singleRow && !strings.Contains(joined, " in-flight: ") {
				t.Errorf("summary should use the single-line format at this width, got: %q", joined)
			}
			for _, l := range rows {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("in-flight row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
		})
	}
}

// TestDashboard_InFlightRowsFitViewport reproduces the in-flight entry
// row overflow (review-10): the path column width is derived from a
// fixed-overhead heuristic (viewportWidth()-23) that assumes the age
// renders in at most 8 cells and the method in at most 6. A request
// with a multi-hour age (e.g. "1h2m3.004s" = 10 cells) or a method
// wider than 6 cells (OPTIONS, which %-6s pads but never truncates)
// makes the row exceed the viewport, so renderContentWithScrollbar
// silently truncates the age. The path column must shrink per row to
// absorb the actual age and method widths.
func TestDashboard_InFlightRowsFitViewport(t *testing.T) {
	tests := []struct {
		name    string
		width   int
		method  string
		path    string
		age     time.Duration
		limited bool
	}{
		{name: "short-age", width: 40, method: "GET", path: "/v1/health", age: time.Second, limited: true},
		{name: "long-age", width: 40, method: "GET", path: "/v1/messages", age: time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, limited: true},
		{name: "long-age-options", width: 40, method: "OPTIONS", path: "/v1/messages", age: time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, limited: false},
		{name: "wide-long-age", width: 80, method: "GET", path: "/v1/messages", age: time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, limited: true},
		{name: "wide-short-age", width: 80, method: "POST", path: "/v1/messages", age: 2 * time.Second, limited: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/width=%d", tt.name, tt.width), func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			m.snap.InFlight = []metrics.InFlightEntry{{
				Method:    tt.method,
				Path:      tt.path,
				StartTime: time.Now().Add(-tt.age),
				Limited:   tt.limited,
			}}
			if tt.limited {
				m.snap.InFlightLimited = 1
			} else {
				m.snap.InFlightPassthrough = 1
			}

			rows := inflightSectionRows(m.dashboardLines())
			if len(rows) == 0 {
				t.Fatal("dashboard should render the In-Flight Requests section")
			}
			joined := strings.Join(rows, "\n")
			if !strings.Contains(joined, tt.method) {
				t.Errorf("in-flight row should show method %q, got: %q", tt.method, joined)
			}
			// The fix may shrink the path column but must never drop
			// or truncate the age itself.
			if tt.age >= time.Hour && !strings.Contains(joined, "1h2m3") {
				t.Errorf("in-flight row should show the full age, got: %q", joined)
			}
			for _, l := range rows {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("in-flight row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
		})
	}
}

// TestDashboard_AllLinesFitViewport asserts the whole-dashboard
// invariant: every row rendered by dashboardLines fits within the
// viewport at widths 40/80/120 across representative snapshots
// (default zero values, circuit breaker CLOSED/OPEN, retries in
// flight, multi-digit counts, sparse status counts, and in-flight
// request entries).
func TestDashboard_AllLinesFitViewport(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		set  func(*metrics.Snapshot)
	}{
		{name: "zero"},
		{
			name: "breaker-closed",
			set: func(s *metrics.Snapshot) {
				s.CircuitBreaker = &metrics.CBStats{
					State:               "CLOSED",
					Failures:            3,
					ConsecutiveFailures: 2,
					CurrentPenalty:      500 * time.Millisecond,
					NextRetry:           now.Add(500 * time.Millisecond),
				}
			},
		},
		{
			name: "breaker-open",
			set: func(s *metrics.Snapshot) {
				s.CircuitBreaker = &metrics.CBStats{
					State:               "OPEN",
					Failures:            25,
					ConsecutiveFailures: 25,
					NextRetry:           now.Add(5 * time.Second),
				}
			},
		},
		{
			name: "retries-in-flight",
			set: func(s *metrics.Snapshot) {
				s.RetriesInFlight = 3
			},
		},
		{
			name: "multidigit",
			set: func(s *metrics.Snapshot) {
				s.StatusCounts = [6]int64{0, 12_345_678, 0, 123_456_789, 0, 0}
				s.TotalProxied = 123_456_789
				s.TotalPassThrough = 987_654_321
				s.TotalAborted = 123_456_789
				s.TotalTimeout = 111_222_333
				s.TotalCancelled = 444_555_666
				s.TotalCircuitRejected = 777_888_999
			},
		},
		{
			name: "sparse",
			set: func(s *metrics.Snapshot) {
				s.StatusCounts = [6]int64{0, 0, 5, 0, 0, 0}
			},
		},
		{
			// A consistent snapshot per metrics.go (Snapshot derives
			// InFlightLimited/InFlightPassthrough from the InFlight
			// slice), with double-digit totals so the summary line
			// cannot pass at width 40 by a zero-cell margin.
			name: "inflight-entries",
			set: func(s *metrics.Snapshot) {
				s.InFlight = make([]metrics.InFlightEntry, 10)
				for i := range s.InFlight {
					s.InFlight[i] = metrics.InFlightEntry{
						Method:    "POST",
						Path:      "/v1/messages",
						StartTime: now.Add(-time.Duration(i+1) * time.Second),
						Limited:   i < 8,
					}
				}
				// One multi-hour age: "1h2m3.004s" renders 10 cells, so
				// the path column must absorb it (review-10).
				s.InFlight[0].StartTime = now.Add(-(time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond))
				s.InFlightLimited = 8
				s.InFlightPassthrough = 2
			},
		},
	}
	for _, width := range []int{40, 80, 120} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("width=%d/%s", width, tt.name), func(t *testing.T) {
				m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
				m.width = width
				m.height = 40
				if tt.set != nil {
					tt.set(&m.snap)
				}
				for _, l := range m.dashboardLines() {
					if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
						t.Errorf("dashboard row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
					}
				}
			})
		}
	}
}

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
func TestDashboard_StatusLineFitsViewport(t *testing.T) {
	tests := []struct {
		name     string
		width    int
		counts   [6]int64
		aborted  int64
		expected []string
	}{
		{
			name:     "narrow-all-nonzero",
			width:    40,
			counts:   [6]int64{0, 1, 20, 3, 4, 5},
			expected: []string{"1xx:1", "2xx:20", "3xx:3", "4xx:4", "5xx:5"},
		},
		{
			// review-09 repro: at 40 columns the wrapped labels row
			// ("  " + six parts joined + "  ") is 43 cells even for
			// single-digit counts, exceeding the 39-cell viewport, so
			// renderContentWithScrollbar silently truncates "Aborted:6".
			name:     "narrow-all-nonzero-aborted",
			width:    40,
			counts:   [6]int64{0, 1, 2, 3, 4, 5},
			aborted:  6,
			expected: []string{"1xx:1", "2xx:2", "3xx:3", "4xx:4", "5xx:5", "Aborted:6"},
		},
		{
			// review-09 repro: with all five classes and Aborted at
			// multi-billion magnitudes the wrapped labels row spans 67
			// cells (labelsWidth 64 + 3) — the last 28 cells are cut off.
			name:     "narrow-all-nonzero-aborted-big",
			width:    40,
			counts:   [6]int64{0, 12_345_678_901, 12_345_678_901, 12_345_678_901, 12_345_678_901, 12_345_678_901},
			aborted:  12_345_678_901,
			expected: []string{"1xx:12.3B", "2xx:12.3B", "3xx:12.3B", "4xx:12.3B", "5xx:12.3B", "Aborted:12.3B"},
		},
		{
			name:     "narrow-sparse",
			width:    40,
			counts:   [6]int64{0, 0, 2, 0, 2, 2},
			expected: []string{"2xx:2", "4xx:2", "5xx:2"},
		},
		{
			name:     "standard-sparse-multidigit",
			width:    80,
			counts:   [6]int64{0, 0, 10_000_000, 0, 123, 12},
			expected: []string{"2xx:10.0M", "4xx:123", "5xx:12"},
		},
		{
			name:     "standard-multidigit-aborted",
			width:    80,
			counts:   [6]int64{0, 99_999, 5, 8, 999, 3},
			aborted:  77_777,
			expected: []string{"1xx:99999", "2xx:5", "3xx:8", "4xx:999", "5xx:3", "Aborted:77777"},
		},
		{
			name:     "empty-with-aborted",
			width:    80,
			aborted:  42,
			expected: []string{"Aborted:42"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			m.snap.StatusCounts = tt.counts
			m.snap.TotalAborted = tt.aborted

			lines := dashboardStatusLines(m.dashboardLines())
			if len(lines) == 0 {
				t.Fatal("dashboard should render a Status section")
			}
			for _, l := range lines {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("status row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
			joined := strings.Join(lines, "\n")
			for _, label := range tt.expected {
				if !strings.Contains(joined, label) {
					t.Errorf("status section missing %q; rows = %q", label, joined)
				}
			}
		})
	}
}

func TestRenderDashboard_CircuitBreakerColors(t *testing.T) {
	for _, state := range []string{"CLOSED", "OPEN", "HALF_OPEN"} {
		t.Run(state, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = 80
			m.height = 40
			m.snap.CircuitBreaker = &metrics.CBStats{State: state}
			s := m.renderDashboardContent()
			if !strings.Contains(s, state) {
				t.Errorf("should show state %q, got: %s", state, s)
			}
		})
	}

	// Color assertions: CLOSED green, OPEN red, HALF_OPEN amber.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
	closed := m.renderDashboardContent()
	if !strings.Contains(closed, "38;2;63;185;80") {
		t.Errorf("CLOSED should render green, got: %s", closed)
	}

	m.snap.CircuitBreaker = &metrics.CBStats{State: "OPEN"}
	open := m.renderDashboardContent()
	if !strings.Contains(open, "38;2;248;81;73") {
		t.Errorf("OPEN should render red, got: %s", open)
	}

	m.snap.CircuitBreaker = &metrics.CBStats{State: "HALF_OPEN"}
	half := m.renderDashboardContent()
	if !strings.Contains(half, "38;2;240;136;62") {
		t.Errorf("HALF_OPEN should render amber, got: %s", half)
	}
}

func TestRenderDashboard_CircuitBreakerSingleLine(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.snap.CircuitBreaker = &metrics.CBStats{
		State:               "OPEN",
		Failures:            5,
		ConsecutiveFailures: 3,
		CurrentPenalty:      2 * time.Second,
		NextRetry:           time.Now().Add(time.Hour),
	}
	s := m.renderDashboardContent()
	if strings.Contains(s, "\n  |  Failures") {
		t.Errorf("circuit breaker summary should be one line, got standalone field line in: %s", s)
	}
	if strings.Count(s, "\n  State:") != 1 {
		t.Errorf("expected exactly one State line, got: %s", s)
	}
}

func TestCanInspect_Dashboard(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	if m.canInspect() {
		t.Error("canInspect should be false for dashboard")
	}
}

func TestCanInspect_Requests(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{{Method: "POST", Path: "/v1/messages", Status: 200}}
	if !m.canInspect() {
		t.Error("canInspect should be true when cursor < len(entries)")
	}
}

func TestCanInspect_Logs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("hello\n"))
	if !m.canInspect() {
		t.Error("canInspect should be true when cursor < len(lines)")
	}
}

func TestCanInspect_Concurrency(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{{ID: 1, Method: "POST", Path: "/v1/messages"}}
	if !m.canInspect() {
		t.Error("canInspect should be true when cursor < len(inflight)")
	}
}

func TestCanInspect_Default(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRoutes
	if m.canInspect() {
		t.Error("canInspect should be false for routes tab")
	}
}

func TestRenderConfirmOverlay_ContainsPrompt(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	s := m.renderConfirmOverlay()
	text := stripANSI(s)
	if !strings.Contains(text, "Clear all cumulative counters") {
		t.Errorf("confirm overlay should contain prompt text, got: %s", text)
	}
}

func TestRenderHelpOverlay_ContainsKeybindings(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	s := m.renderHelpOverlay()
	text := stripANSI(s)
	for _, kw := range []string{"Switch tab", "scroll", "filter", "Quit", "Reset Stats"} {
		if !strings.Contains(text, kw) {
			t.Errorf("help overlay should contain %q", kw)
		}
	}
}

// TestFooterMentionsReset pins the footer's c:reset hint so the binding stays
// discoverable from every tab.
func TestFooterMentionsReset(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	s := stripANSI(m.renderFooter())
	if !strings.Contains(s, "c:reset") {
		t.Errorf("footer should contain %q, got:\n%s", "c:reset", s)
	}
}

func TestSwitchTab_SetsModeBrowse(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.mode = modeHelp
	m.switchTab(tabLogs)
	if m.mode != modeBrowse {
		t.Errorf("mode = %d, want modeBrowse after switchTab", m.mode)
	}
}

func TestPerRouteRate_TUI10(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200, Time: time.Now()},
		{Method: "POST", Path: "/v1/messages", Status: 200, Time: time.Now()},
		{Method: "GET", Path: "/health", Status: 200, Time: time.Now()},
	}
	rates := m.perRouteRate()
	if len(rates) == 0 {
		t.Error("perRouteRate should return non-empty map")
	}
	msgRate, ok := rates["POST /v1/messages"]
	if !ok {
		t.Error("perRouteRate should have entry for POST /v1/messages")
	}
	if msgRate <= 0 {
		t.Errorf("rate for POST /v1/messages = %f, want > 0", msgRate)
	}
}

func TestMouseClickContentArea_SetsCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: 5})
	// Row 5 skips the table header on row 3, so it maps to data row 1.
	if m2.cursor != 1 {
		t.Errorf("cursor = %d, want 1 (row 5 - contentStartRow 3 - header 1)", m2.cursor)
	}
}

func TestMouseClickContentArea_ClampsToMaxCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: 20})
	if m2.cursor > m2.maxCursor() {
		t.Errorf("cursor = %d, should be clamped to maxCursor = %d", m2.cursor, m2.maxCursor())
	}
}

func TestMouseClickContentArea_DashboardSetsCursor(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: 5})
	// Dashboard is scrollable, so a content click sets the cursor/scroll position.
	if m2.cursor != 2 {
		t.Errorf("cursor = %d, want 2 (row 5 - contentStartRow 3 + scroll 0)", m2.cursor)
	}
}

func TestMouseClickContentArea_LogsTabNoHeader(t *testing.T) {
	// The Logs tab has no header row (line numbers are embedded in data rows),
	// so clicking the first content row should set cursor=0, not be ignored.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs
	m.logRing.Write([]byte("line1\nline2\nline3\n"))
	// contentHeaderRows for Logs without filter is 0, so clicking
	// contentStartRow (row 3) maps to cursor 0.
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: contentStartRow})
	if m2.cursor != 0 {
		t.Errorf("Logs first-row click: cursor = %d, want 0 (no header offset)", m2.cursor)
	}
	// Second row should map to cursor 1.
	m3 := update(m, tea.MouseClickMsg{X: 10, Y: contentStartRow + 1})
	if m3.cursor != 1 {
		t.Errorf("Logs second-row click: cursor = %d, want 1", m3.cursor)
	}
}

func TestScrollbarClick_SetsDragging(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: 10})
	if !m2.dragging {
		t.Error("scrollbar click should set dragging = true")
	}
}

func TestScrollbarClick_JumpsScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: 18})
	if m2.scroll == 0 {
		t.Error("scrollbar click near bottom should set scroll > 0")
	}
}

func TestMouseDrag_MotionUpdatesScroll(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.dragging = true
	m2 := update(m, tea.MouseMotionMsg{X: 79, Y: 18})
	if m2.scroll == 0 {
		t.Error("mouse motion while dragging should update scroll")
	}
}

func TestMouseDrag_MotionWithoutDragIgnored(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, tea.MouseMotionMsg{X: 79, Y: 18})
	if m2.dragging {
		t.Error("mouse motion without prior click should not set dragging")
	}
	if m2.scroll != 0 {
		t.Error("mouse motion without dragging should not change scroll")
	}
}

func TestMouseRelease_ClearsDragging(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.dragging = true
	m2 := update(m, tea.MouseReleaseMsg{})
	if m2.dragging {
		t.Error("mouse release should clear dragging")
	}
}

// ─── Viewport: Terminal Size Adaptation ───

func TestView_NarrowTerminal(t *testing.T) {
	// View should render without panic at various narrow widths.
	for w := 1; w <= 80; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		v := m.View()
		if v.Content == "" && w >= 4 {
			t.Errorf("width=%d: View returned empty content", w)
		}
	}
}

func TestView_TinyTerminal(t *testing.T) {
	// Very small terminal: 10x5.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 10
	m.height = 5
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 20)
	v := m.View()
	if v.Content == "" {
		t.Error("View should render something even at 10x5")
	}
}

func TestView_Height4Minimum(t *testing.T) {
	// Minimum viable terminal: header(1) + tabbar(1) + separator(1) + footer(1) = 4.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 4
	m.tab = tabDashboard
	v := m.View()
	if v.Content == "" {
		t.Error("View should render at height 4")
	}
}

func TestView_Height3TooSmall(t *testing.T) {
	// Height 3 is below minimum — should return empty.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 3
	v := m.View()
	if v.Content != "" {
		t.Error("View should return empty for height < 4")
	}
}

func TestView_ZeroSize(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	v := m.View()
	if v.Content != "" {
		t.Error("View should return empty for zero size")
	}
}

// ─── Viewport: Scrollbar at Various Sizes ───

func TestScrollbar_NarrowTerminal(t *testing.T) {
	// Scrollbar should render at narrow widths.
	for w := 5; w <= 40; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		v := m.View()
		// Should contain scrollbar characters.
		if !strings.Contains(v.Content, "│") && !strings.Contains(v.Content, "█") {
			t.Errorf("width=%d: View missing scrollbar chars", w)
		}
	}
}

func TestScrollbar_VariousHeights(t *testing.T) {
	// Scrollbar should work at various terminal heights.
	for h := 4; h <= 40; h++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = 80
		m.height = h
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
		v := m.View()
		if v.Content == "" {
			t.Errorf("height=%d: View returned empty", h)
		}
	}
}

// ─── Viewport: Mouse Click Precision ───

func TestMouseClickScrollbar_TopJumpsToTop(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.scroll = 50
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m)})
	if m2.scroll != 0 {
		t.Errorf("click at top of scrollbar: scroll = %d, want 0", m2.scroll)
	}
}

func TestMouseClickScrollbar_BottomJumpsNearBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m) + m.dataRows() - 1})
	if m2.scroll < 70 {
		t.Errorf("click at bottom of scrollbar: scroll = %d, want >= 70", m2.scroll)
	}
}

func TestMouseClickScrollbar_MiddleJumpsNearMiddle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m) + m.dataRows()/2})
	// Track height is dataRows (18), so the center maps to ~49 of maxScroll (82).
	if m2.scroll < 45 || m2.scroll > 55 {
		t.Errorf("click at middle of scrollbar: scroll = %d, want [45, 55]", m2.scroll)
	}
}

func TestMouseClickContentArea_CursorFollowsClick(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.scroll = 10
	// Click on the 2nd visible data row (past the table header).
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: contentStartRow + m.contentHeaderRows() + 1})
	if m2.cursor != 11 {
		t.Errorf("cursor = %d, want 11 (scroll 10 + relative data row 1)", m2.cursor)
	}
}

func TestMouseClickContentArea_ClampedAtEnd(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
	m.scroll = 0
	// Click way past the end.
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: contentStartRow + 100})
	if m2.cursor > m2.maxCursor() {
		t.Errorf("cursor = %d, should be clamped to %d", m2.cursor, m2.maxCursor())
	}
}

// ─── Viewport: Mouse Drag Precision ───

func TestMouseDrag_TopToBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	// Start drag at top of the scrollbar track.
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m)})
	if m2.scroll != 0 {
		t.Errorf("initial drag at top: scroll = %d, want 0", m2.scroll)
	}
	// Drag to bottom. With the corrected geometry (dragRange = trackHeight - thumbHeight)
	// the user can reach scrollMax.
	m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: scrollbarTop(m) + m.dataRows() - 1})
	sm := m.maxScroll()
	if sm > 0 && m3.scroll < sm-3 {
		t.Errorf("drag to bottom: scroll = %d, want >= %d (scrollMax=%d)", m3.scroll, sm-3, sm)
	}
}

func TestMouseDrag_Monotonic(t *testing.T) {
	// Dragging from top to bottom should produce monotonically increasing scroll.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: scrollbarTop(m)})
	prev := -1
	for dy := 0; dy < m.dataRows(); dy++ {
		y := scrollbarTop(m) + dy
		m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: y})
		if m3.scroll < prev {
			t.Errorf("drag not monotonic: scroll at y=%d is %d, was %d", y, m3.scroll, prev)
		}
		prev = m3.scroll
	}
}

func TestMouseDrag_Proportional(t *testing.T) {
	// Dragging to 25%, 50%, 75% of track should produce proportional scroll.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)

	trackTop := scrollbarTop(m)

	quarter := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	quarter = update(quarter, tea.MouseMotionMsg{X: 79, Y: trackTop + m.dataRows()/4})

	half := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	half = update(half, tea.MouseMotionMsg{X: 79, Y: trackTop + m.dataRows()/2})

	threeQ := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	threeQ = update(threeQ, tea.MouseMotionMsg{X: 79, Y: trackTop + 3*m.dataRows()/4})

	// Quarter should be roughly 25% of max scroll.
	if quarter.scroll < 18 || quarter.scroll > 25 {
		t.Errorf("quarter drag: scroll = %d, want [18, 25]", quarter.scroll)
	}
	// Half should be roughly 50%.
	if half.scroll < 40 || half.scroll > 55 {
		t.Errorf("half drag: scroll = %d, want [40, 55]", half.scroll)
	}
	// 3/4 should be roughly 75%.
	if threeQ.scroll < 65 || threeQ.scroll > 81 {
		t.Errorf("three-quarter drag: scroll = %d, want [65, 81]", threeQ.scroll)
	}
}

func TestMouseDrag_ReachBottom(t *testing.T) {
	// Dragging from the very top to the very bottom of the scrollbar track
	// must reach scrollMax, proving the user can scroll to the last line.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	trackTop := scrollbarTop(m)
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: trackTop})
	m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: trackTop + m.dataRows() - 1})
	sm := m.maxScroll()
	if sm > 0 && m3.scroll < sm {
		t.Errorf("full drag: scroll = %d, want %d (scrollMax)", m3.scroll, sm)
	}
}

// ─── Viewport: Keyboard Navigation at Various Sizes ───

func TestKeyboardScroll_NarrowTerminal(t *testing.T) {
	for w := 10; w <= 80; w += 10 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
		m2 := update(m, key('j'))
		if m2.cursor != 1 {
			t.Errorf("width=%d: cursor = %d, want 1 after j", w, m2.cursor)
		}
	}
}

func TestKeyboardScroll_TinyTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 10
	m.height = 6
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 20)
	// Should not panic.
	for range 5 {
		m = update(m, key('j'))
	}
	if m.cursor < 0 || m.cursor > m.maxCursor() {
		t.Errorf("cursor = %d, out of bounds [0, %d]", m.cursor, m.maxCursor())
	}
}

func TestGoToTop_NarrowTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 15
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m.cursor = 30
	m.scroll = 20
	m2 := update(m, key('g'))
	if m2.cursor != 0 || m2.scroll != 0 {
		t.Errorf("cursor=%d scroll=%d, want 0,0", m2.cursor, m2.scroll)
	}
}

func TestGoToBottom_NarrowTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 15
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)
	m2 := update(m, key('G'))
	if m2.cursor != m2.maxCursor() {
		t.Errorf("cursor = %d, want %d", m2.cursor, m2.maxCursor())
	}
}

// ─── Viewport: Content Rendering at Various Sizes ───

func TestRenderContent_NarrowWidth(t *testing.T) {
	for w := 5; w <= 80; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = []metrics.RequestLogEntry{
			{Method: "POST", Path: "/v1/messages", Status: 200, Duration: time.Millisecond},
		}
		s := m.renderRequests()
		if !strings.Contains(s, "POST") {
			t.Errorf("width=%d: renderRequests missing POST", w)
		}
	}
}

func TestRenderDashboard_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabDashboard
		s := m.renderDashboardContent()
		if !strings.Contains(s, "Throughput") && w >= 15 {
			t.Errorf("width=%d: renderDashboard missing Throughput", w)
		}
	}
}

func TestRenderNetwork_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabNetwork
		s := m.renderNetwork()
		// Should not panic at any width.
		_ = s
	}
}

func TestRenderLogs_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabLogs
		m.logRing.Write([]byte("test log line\n"))
		s := m.renderLogs()
		if !strings.Contains(s, "test log line") {
			t.Errorf("width=%d: renderLogs missing content", w)
		}
	}
}

func TestRenderConcurrency_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabConcurrency
		m.snap.InFlight = []metrics.InFlightEntry{
			{ID: 1, Method: "POST", Path: "/v1/messages", Limited: true},
		}
		s := m.renderConcurrency()
		if !strings.Contains(s, "POST") && w >= 15 {
			t.Errorf("width=%d: renderConcurrency missing POST", w)
		}
	}
}

func TestRenderRoutes_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRoutes
		m.snap.RouteStats = map[string]metrics.RouteStat{
			"POST /v1/messages": {Total: 10},
		}
		s := m.renderRoutes()
		if !strings.Contains(s, "POST") && w >= 15 {
			t.Errorf("width=%d: renderRoutes missing POST", w)
		}
	}
}

// ─── Viewport: Scrollbar Model Integration ───

func TestUpdateScrollbars_AtVariousSizes(t *testing.T) {
	for w := 5; w <= 120; w += 5 {
		for h := 4; h <= 40; h += 4 {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = w
			m.height = h
			m.tab = tabRequests
			m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
			m.scroll = 50
			m.updateScrollbars()
			sb := m.scrollbars[m.tab]
			if sb.ContentHeight != 100 {
				t.Errorf("w=%d h=%d: ContentHeight = %d, want 100", w, h, sb.ContentHeight)
			}
			want := m.dataRows()
			if sb.ViewportHeight != want {
				t.Errorf("w=%d h=%d: ViewportHeight = %d, want %d", w, h, sb.ViewportHeight, want)
			}
		}
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

func TestViewport_SingleRow(t *testing.T) {
	// Content has exactly 1 row.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 1)
	m2 := update(m, key('j'))
	if m2.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (single row)", m2.cursor)
	}
}

func TestViewport_ExactFit(t *testing.T) {
	// Content exactly fits the data row area.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	visible := m.dataRows()
	m.snap.LogEntries = make([]metrics.RequestLogEntry, visible)
	m2 := update(m, key('G'))
	if m2.cursor != visible-1 {
		t.Errorf("cursor = %d, want %d", m2.cursor, visible-1)
	}
	if m2.scroll != 0 {
		t.Errorf("scroll = %d, want 0 (exact fit)", m2.scroll)
	}
}

func TestViewport_OnePastFit(t *testing.T) {
	// Content is exactly one more than the data row area.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	visible := m.dataRows()
	m.snap.LogEntries = make([]metrics.RequestLogEntry, visible+1)
	m2 := update(m, key('G'))
	if m2.cursor != visible {
		t.Errorf("cursor = %d, want %d", m2.cursor, visible)
	}
	if m2.scroll != 1 {
		t.Errorf("scroll = %d, want 1 (one past fit)", m2.scroll)
	}
}

func TestViewport_EmptyAfterFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
	}
	m.filterText = "nonexistent"
	m2 := update(m, key('j'))
	if m2.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (empty after filter)", m2.cursor)
	}
}

func TestViewport_CursorPreservedOnScroll(t *testing.T) {
	// After scrolling, cursor should stay at same content position.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.cursor = 50
	m.scroll = 40
	// Page up.
	m2 := update(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	// Cursor should have moved up by data rows.
	expected := 50 - m.dataRows()
	if m2.cursor != expected {
		t.Errorf("cursor = %d, want %d after PgUp", m2.cursor, expected)
	}
}

func TestFooter_AnchoredAtBottom(t *testing.T) {
	for _, tab := range []tabID{tabDashboard, tabRequests, tabNetwork, tabLogs, tabConcurrency, tabRoutes} {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = 80
		m.height = 24
		m.tab = tab

		// Give each tab enough content that it is non-empty.
		switch tab {
		case tabDashboard:
			m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
		case tabRequests:
			m.snap.LogEntries = []metrics.RequestLogEntry{}
			for i := range 25 {
				m.snap.LogEntries = append(m.snap.LogEntries, metrics.RequestLogEntry{
					Method: "POST", Path: "/v1/messages", Status: 200,
					Time: time.Now().Add(-time.Duration(i) * time.Second),
				})
			}
		case tabNetwork:
			// journal is nil in the test model; keep empty to avoid panic.
		case tabLogs:
			m.logRing.Write([]byte("first log line\nsecond log line\n"))
		case tabConcurrency:
			for i := range 15 {
				m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
					ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
				})
			}
		case tabRoutes:
			m.snap.RouteStats = map[string]metrics.RouteStat{
				"POST /v1/messages": {Total: 1},
			}
		}

		v := m.View()
		lines := strings.Split(v.Content, "\n")

		// The view must produce exactly one line per terminal row.
		if len(lines) != m.height {
			t.Errorf("tab=%d: view has %d lines, want %d", tab, len(lines), m.height)
			continue
		}

		// The footer must be the very last line.
		last := lines[len(lines)-1]
		if !strings.Contains(last, "1-6:tab") {
			t.Errorf("tab=%d: last line should be footer, got %q", tab, last)
		}
	}
}

func TestView_ViewportClampsOverflow(t *testing.T) {
	// When a tab's fixed chrome plus data exceeds the allocated visibleRows,
	// renderContentWithScrollbar must clip to exactly visibleRows lines instead
	// of expanding. Without this guard the output grows taller than the
	// terminal, pushing the chrome and scrollable content upward out of sight
	// while the footer stays anchored at the bottom.
	for _, tab := range []tabID{tabDashboard, tabRequests, tabNetwork, tabLogs, tabConcurrency, tabRoutes} {
		for _, h := range []int{8, 14, 50} {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = 80
			m.height = h
			m = update(m, tea.WindowSizeMsg{Width: 80, Height: h})
			m.tab = tab
			// Populate enough state that each tab has some fixed content to emit.
			switch tab {
			case tabRequests:
				m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)
			case tabNetwork:
				// journal entries are exercised by network detail PTY tests;
				// browse state here relies on visibleEntries which is nil.
			case tabLogs:
				m.logRing.Write([]byte("first log line\nsecond log line\n"))
			case tabConcurrency:
				m.snap.InFlight = make([]metrics.InFlightEntry, 5)
			case tabRoutes:
				m.snap.RouteStats = map[string]metrics.RouteStat{
					"POST /v1/messages": {Total: 1},
				}
			}
			v := m.View()
			lines := strings.Split(v.Content, "\n")
			if len(lines) != h {
				t.Errorf("tab=%d height=%d: view has %d lines, want %d", tab, h, len(lines), h)
				continue
			}
			first := stripANSI(lines[0])
			if !strings.Contains(first, "shaper") {
				t.Errorf("tab=%d height=%d: header scrolled out of sight, got %q", tab, h, first)
			}
			last := lines[len(lines)-1]
			if !strings.Contains(last, "1-6:tab") {
				t.Errorf("tab=%d height=%d: footer not anchored at bottom, got %q", tab, h, last)
			}
		}
	}
}

func TestFooter_FilterPromptOnOwnLine(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.mode = modeFilter
	m.filterText = "hello"
	v := m.View()
	lines := strings.Split(v.Content, "\n")
	var footerIdx, filterIdx int
	for i, line := range lines {
		if strings.Contains(line, "1-6:tab") {
			footerIdx = i
		}
		if strings.Contains(line, "Filter: hello") {
			filterIdx = i
		}
	}
	if filterIdx == 0 {
		t.Fatal("filter prompt not found in view")
	}
	if footerIdx == 0 {
		t.Fatal("footer not found in view")
	}
	if footerIdx-filterIdx != 1 {
		t.Errorf("footer line = %d, filter line = %d, want filter immediately above footer", footerIdx, filterIdx)
	}
}

func TestDashboardScrollsWithKeyboard(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	// Add enough dashboard state that the content exceeds visibleRows.
	m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
	for i := range 10 {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID:     uint64(i),
			Method: "POST",
			Path:   "/v1/messages",
		})
	}

	maxC := m.maxCursor()
	if maxC <= m.visibleRows() {
		t.Fatalf("dashboard content too small: maxCursor=%d, visible=%d", maxC, m.visibleRows())
	}

	// Jump to bottom and verify the dashboard scrolls.
	m2 := update(m, key('G'))
	if m2.scroll <= 0 {
		t.Errorf("dashboard scroll = %d, want > 0 after G", m2.scroll)
	}

	// Jump back to top.
	m3 := update(m2, key('g'))
	if m3.scroll != 0 {
		t.Errorf("dashboard scroll = %d, want 0 after g", m3.scroll)
	}
}

func TestDashboardScrollbarNotFullWhenOverflow(t *testing.T) {
	// Force dashboard overflow by shrinking the terminal so dashboard lines
	// exceed visibleRows. A 15-row terminal gives visibleRows=11 (height-4),
	// which is smaller than the default dashboard line count (~18).
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 15
	m.tab = tabDashboard
	m.dashboardLinesCache = m.dashboardLines()
	v := m.View()
	// The scrollbar must contain both track (│) and thumb (█) characters
	// in the rightmost column when content overflows.
	if m.maxCursor()+1 <= m.visibleRows() {
		t.Skipf("dashboard not overflowing: lines=%d visibleRows=%d", m.maxCursor()+1, m.visibleRows())
	}
	if !strings.Contains(v.Content, "│") {
		t.Error("dashboard overflow should render scrollbar track (│)")
	}
	if !strings.Contains(v.Content, "█") {
		t.Error("dashboard overflow should render scrollbar thumb (█)")
	}
}

func TestUpdateScrollbars_PerTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 10)
	m.updateScrollbars()
	if m.scrollbars[tabRequests].ContentHeight != 10 {
		t.Errorf("requests ContentHeight = %d, want 10", m.scrollbars[tabRequests].ContentHeight)
	}
	if m.scrollbars[tabRoutes].ContentHeight != 0 {
		t.Errorf("routes ContentHeight = %d, want 0 before visiting", m.scrollbars[tabRoutes].ContentHeight)
	}

	m.snap.RouteStats = map[string]metrics.RouteStat{
		"POST /a": {Total: 1},
		"POST /b": {Total: 2},
	}
	m.tab = tabRoutes
	m.updateScrollbars()
	if m.scrollbars[tabRoutes].ContentHeight != 2 {
		t.Errorf("routes ContentHeight = %d, want 2", m.scrollbars[tabRoutes].ContentHeight)
	}
}

func TestScrollbarDrag_GrabOffsetNoJump(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 100)
	m.scroll = 42

	// Compute where the thumb is and grab its bottom edge.
	contentHeight := m.maxCursor() + 1
	trackHeight := m.dataRows()
	trackTop := scrollbarTop(m)
	thumbTop := viewport.ThumbTop(m.scroll, contentHeight, trackHeight)
	thumbHeight := viewport.ThumbHeight(contentHeight, trackHeight)
	grabY := trackTop + thumbTop + thumbHeight - 1

	m2 := update(m, tea.MouseClickMsg{X: 79, Y: grabY})
	if m2.scroll != 42 {
		t.Fatalf("click on thumb should not jump: scroll = %d, want 42", m2.scroll)
	}

	// Drag one row down: scroll should change by a small proportional amount,
	// not by a large absolute jump.
	m3 := update(m2, tea.MouseMotionMsg{X: 79, Y: grabY + 1})
	delta := m3.scroll - 42
	if delta < 1 || delta > 10 {
		t.Errorf("drag one row: scroll delta = %d, want [1, 10]", delta)
	}
}

// ─── TUI-REVIEW-01: scratch/review-01.md fixes ───

func dashboardWithOverflow(conc, height, flights int) Model {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: conc}})
	m.width = 80
	m.height = height
	m.tab = tabDashboard
	m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
	for i := range flights {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
		})
	}
	return m
}

func TestDashboardScrollByLine(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 20)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, key('j'))
	if m2.scroll != 1 || m2.cursor != 1 {
		t.Errorf("after j: scroll=%d cursor=%d, want 1,1", m2.scroll, m2.cursor)
	}

	m3 := update(m2, key('k'))
	if m3.scroll != 0 || m3.cursor != 0 {
		t.Errorf("after k: scroll=%d cursor=%d, want 0,0", m3.scroll, m3.cursor)
	}
}

func TestDashboardScrollArrowKeys(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 20)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m2.scroll != 1 {
		t.Errorf("after Down: scroll=%d, want 1", m2.scroll)
	}

	m3 := update(m2, tea.KeyPressMsg{Code: tea.KeyUp})
	if m3.scroll != 0 {
		t.Errorf("after Up: scroll=%d, want 0", m3.scroll)
	}
}

func TestDashboardScrollPageAndEndKeys(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 30)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m2.scroll <= 0 {
		t.Errorf("after PgDown: scroll=%d, want > 0", m2.scroll)
	}
	if m2.scroll > m2.maxScroll() {
		t.Errorf("after PgDown: scroll=%d exceeds maxScroll=%d", m2.scroll, m2.maxScroll())
	}

	m3 := update(m2, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m3.scroll != m3.maxScroll() {
		t.Errorf("after End: scroll=%d, want %d", m3.scroll, m3.maxScroll())
	}

	m4 := update(m3, tea.KeyPressMsg{Code: tea.KeyHome})
	if m4.scroll != 0 || m4.cursor != 0 {
		t.Errorf("after Home: scroll=%d cursor=%d, want 0,0", m4.scroll, m4.cursor)
	}

	m5 := update(m, tea.KeyPressMsg{Text: "ctrl+d"})
	if m5.scroll <= 0 {
		t.Errorf("after Ctrl-D: scroll=%d, want > 0", m5.scroll)
	}

	m6 := update(m5, tea.KeyPressMsg{Text: "ctrl+u"})
	if m6.scroll >= m5.scroll {
		t.Errorf("after Ctrl-U: scroll=%d, want < previous %d", m6.scroll, m5.scroll)
	}
}

func TestDashboardScrollGKeys(t *testing.T) {
	m := dashboardWithOverflow(4, 12, 30)
	if m.maxScroll() <= 0 {
		t.Fatalf("need overflowing dashboard: maxScroll=%d visible=%d", m.maxScroll(), m.visibleRows())
	}

	m2 := update(m, key('G'))
	if m2.scroll != m2.maxScroll() {
		t.Errorf("after G: scroll=%d, want %d", m2.scroll, m2.maxScroll())
	}

	m3 := update(m2, key('g'))
	if m3.scroll != 0 || m3.cursor != 0 {
		t.Errorf("after g: scroll=%d cursor=%d, want 0,0", m3.scroll, m3.cursor)
	}
}

func TestDashboardScrollDoesNotAffectRequestsTab(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m2 := update(m, key('j'))
	if m2.cursor != 1 {
		t.Errorf("requests tab cursor=%d, want 1", m2.cursor)
	}
	if m2.scroll != 0 {
		t.Errorf("requests tab scroll=%d, want 0", m2.scroll)
	}
}

func TestDashboardCacheLazyEvaluation(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)

	m2 := update(m, metrics.Snapshot{})
	if m2.dashboardLinesCache != nil {
		t.Errorf("dashboardLinesCache should remain nil on non-Dashboard tab, got %d entries", len(m2.dashboardLinesCache))
	}

	m2.tab = tabDashboard
	_ = m2.cachedDashboardLines()
	if len(m2.dashboardLinesCache) == 0 {
		t.Error("cachedDashboardLines should build cache lazily when accessed on Dashboard")
	}
}

func TestDashboardCacheInvalidation(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	for i := range 20 {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
		})
	}

	m = update(m, metrics.Snapshot{})
	if len(m.dashboardLinesCache) == 0 {
		t.Fatal("expected dashboardLinesCache to be populated after snapshot")
	}
	firstCache := m.dashboardLinesCache

	// Non-mutating input messages should reuse the cached lines.
	m2 := update(m, tea.MouseMotionMsg{X: 10, Y: 10})
	if m2.dashboardLinesCache == nil {
		t.Error("MouseMotionMsg should not clear dashboardLinesCache")
	}
	m2 = update(m2, key('j'))
	if m2.dashboardLinesCache == nil {
		t.Error("KeyPressMsg should not clear dashboardLinesCache")
	}
	m2 = update(m2, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m2.dashboardLinesCache == nil {
		t.Error("MouseWheelMsg should not clear dashboardLinesCache")
	}
	m2 = update(m2, tea.MouseClickMsg{X: 1, Y: contentStartRow})
	if m2.dashboardLinesCache == nil {
		t.Error("MouseClickMsg should not clear dashboardLinesCache")
	}
	if &m2.dashboardLinesCache[0] != &firstCache[0] || len(m2.dashboardLinesCache) != len(firstCache) {
		t.Errorf("cache should be reused for non-mutating messages")
	}

	// A new snapshot mutates the underlying data and invalidates the cache;
	// adjustViewport rebuilds it, so we assert the content actually changed.
	snap := m2.snap
	snap.InFlight = append(snap.InFlight, metrics.InFlightEntry{ID: 999, Method: "GET", Path: "/extra", Limited: true})
	m3 := update(m2, snap)
	if m3.dashboardLinesCache == nil {
		t.Fatal("expected dashboardLinesCache to be rebuilt after snapshot")
	}
	if len(m3.dashboardLinesCache) <= len(firstCache) {
		t.Errorf("snapshot should produce new content: got %d lines, want more than %d", len(m3.dashboardLinesCache), len(firstCache))
	}

	// A terminal resize also invalidates the cache; the Dashboard tab rebuilds
	// it in the same Update cycle. Use a sentinel to detect rebuild without
	// relying on slice backing-array identity.
	m3.dashboardLinesCache = []string{"SENTINEL"}
	m4 := update(m3, tea.WindowSizeMsg{Width: 80, Height: 24})
	if m4.dashboardLinesCache == nil {
		t.Fatal("expected dashboardLinesCache to be rebuilt after resize")
	}
	if len(m4.dashboardLinesCache) == 1 && m4.dashboardLinesCache[0] == "SENTINEL" {
		t.Error("WindowSizeMsg should invalidate and rebuild dashboardLinesCache")
	}

	// Switching tabs discards the old tab's cached content. On the Requests tab
	// adjustViewport never calls cachedDashboardLines, so the cache stays nil.
	m5 := update(m4, key('2'))
	if m5.dashboardLinesCache != nil {
		t.Error("switching tabs should clear dashboardLinesCache")
	}
}

func TestTruncateANSI(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		width       int
		wantVisible int // 0 means use the natural width bound
	}{
		// darkModel is the default palette; TruncateANSI is color-agnostic so
		// any theme's default green suffices as a multi-byte ANSI load.
		{"short unchanged", darkModel().styles.statusOkStyle.Render("ok"), 10, 0},
		{"ascii truncation", darkModel().styles.statusOkStyle.Render(strings.Repeat("x", 50)), 10, 0},
		{"cjk truncation", darkModel().styles.statusOkStyle.Render("日本語説明文"), 4, 0},
		{"cjk full width", darkModel().styles.statusOkStyle.Render("日本語説明文"), 10, 0},
		{"emoji zwj", darkModel().styles.statusOkStyle.Render("🏳️‍🌈🏳️‍🌈🏳️‍🌈"), 4, 0},
		{"zero width", "hello", 0, 0},
		// Odd boundary: the third CJK grapheme would cross the 5-cell mark,
		// so truncation stops at the first two graphemes (4 cells). This is
		// the multi-cell underflow condition that renderContentWithScrollbar
		// must pad away.
		{"cjk odd boundary underflow", darkModel().styles.statusOkStyle.Render("日本語"), 5, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateANSI(tt.line, tt.width)
			visible := uniseg.StringWidth(stripANSI(got))
			want := tt.wantVisible
			if want == 0 {
				want = min(uniseg.StringWidth(stripANSI(tt.line)), tt.width)
			}
			if visible != want {
				t.Errorf("visible cells = %d, want %d; got %q", visible, want, got)
			}
			if want > 0 {
				// The result must still begin with the original ANSI sequences.
				if !strings.Contains(got, "\x1b[") {
					t.Errorf("truncated styled content missing ANSI sequences: %q", got)
				}
			}
			if tt.width > 0 && uniseg.StringWidth(stripANSI(tt.line)) > tt.width {
				if !strings.Contains(got, "\x1b[0m") {
					t.Errorf("truncation should append reset, got %q", got)
				}
			}
		})
	}
}

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

func TestScrollbar_AlignedWithDataRows(t *testing.T) {
	// The Concurrency tab has 10 fixed header rows and 10 data rows. The
	// scrollbar should appear alongside the data rows, not the header rows.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	for i := range 15 {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
		})
	}
	m.updateScrollbars()

	s := m.renderContentWithScrollbar()
	lines := strings.Split(s, "\n")
	headerRows := 10
	// The first header row should have content but no scrollbar column.
	if len(lines) <= headerRows {
		t.Fatalf("expected at least %d lines, got %d", headerRows+1, len(lines))
	}
	firstDataRow := lines[headerRows]
	lastHeaderRow := lines[headerRows-1]
	if !strings.Contains(firstDataRow, "│") && !strings.Contains(firstDataRow, "█") {
		t.Errorf("first data row should contain scrollbar chars, got: %q", firstDataRow)
	}
	if strings.Contains(lastHeaderRow, "│") || strings.Contains(lastHeaderRow, "█") {
		t.Errorf("last header row should not contain scrollbar chars, got: %q", lastHeaderRow)
	}

	// Clicking inside the header area should not interact with the scrollbar.
	m2 := update(m, tea.MouseClickMsg{X: 79, Y: contentStartRow + headerRows - 1})
	if m2.scroll != 0 {
		t.Errorf("click in header area should not scroll: scroll = %d, want 0", m2.scroll)
	}
	// Clicking at the top of the track should start a drag.
	m3 := update(m, tea.MouseClickMsg{X: 79, Y: contentStartRow + headerRows})
	if !m3.dragging {
		t.Error("click at top of data area should set dragging = true")
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

func TestProviderSwitcherKeys(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	m.width = 80
	m.height = 24

	// Tab cycles to the next provider and wraps from the last back to the first.
	for _, want := range []string{"anthropic", "openai", "acme"} {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyTab})
		if got := m.providers[m.active].name; got != want {
			t.Fatalf("after Tab: active provider = %q, want %q", got, want)
		}
		if got := m.providerName(); got != " "+want {
			t.Errorf("after Tab: header name = %q, want %q", got, " "+want)
		}
	}

	// Shift+Tab cycles to the previous provider and wraps from the first back
	// to the last.
	for _, want := range []string{"openai", "anthropic", "acme"} {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		if got := m.providers[m.active].name; got != want {
			t.Fatalf("after Shift+Tab: active provider = %q, want %q", got, want)
		}
	}
}

func TestProviderSwitcherChipClick(t *testing.T) {
	p := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	p.width = 100
	p.height = 24

	// A click on header row 0 inside a chip's right-aligned range switches
	// to that provider. The chips end at column width-2 and are joined
	// right-to-left: each chip occupies [x-w+1, x] where x is its
	// right edge and w is its rendered width.
	// The spans come from the production budgetedChips layout — the same
	// parts chipAt hit-tests — so the test cannot drift from what is
	// actually rendered. At width 100 all three chips are at full width.
	right := p.width - 2
	layout := p.budgetedChips()
	parts := layout.parts
	if len(parts) != 3 {
		t.Fatalf("budgetedChips rendered %d chips at width 100, want all 3", len(parts))
	}
	for i, part := range slices.Backward(parts) {
		w := lipgloss.Width(part)
		p = update(p, tea.MouseClickMsg{X: right, Y: 0})
		if p.active != i {
			t.Fatalf("click at column %d: active = %d, want %d", right, p.active, i)
		}
		right -= w + 1
	}

	// A click on row 0 far outside the chips (the brand-filled left side)
	// must leave the active provider unchanged.
	p = update(p, tea.MouseClickMsg{X: 0, Y: 0})
	if p.active != 0 {
		t.Errorf("click outside chips changed active to %d, want 0", p.active)
	}
}

func TestSingleProviderHeaderKeepsShaper(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	if m.hasSwitcher() {
		t.Fatal("a single unnamed provider must not render a provider switcher")
	}
	if s := m.renderProviderSwitcher(); s != "" {
		t.Fatalf("renderProviderSwitcher = %q, want \"\" for a single unnamed provider", s)
	}
	if got := m.providerName(); got != " ⚡ shaper" {
		t.Errorf("providerName = %q, want %q", got, " ⚡ shaper")
	}
	if _, ok := m.chipAt(0); ok {
		t.Error("chipAt must not hit a chip for a single unnamed provider")
	}
	h := stripANSI(m.renderHeader())
	if !strings.Contains(h, "⚡ shaper") {
		t.Errorf("header must keep the \"⚡ shaper\" brand, got: %s", h)
	}
}

// TestHeaderWidthBudget pins the narrow-pane contract of Task 9: for every
// width >= 40 and any provider names, the header row must never exceed the
// terminal width (no wrap onto the tab bar), the active provider's chip must
// stay visible and clickable, chipAt must map clicks to exactly the chips
// actually rendered (walking every rendered x-position), and the
// single-provider "⚡ shaper" header must be byte-identical at every width.
func TestHeaderWidthBudget(t *testing.T) {
	widths := []int{40, 60, 80, 120}
	multi := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}

	// (a) Row 0 never exceeds m.width, for every width and every active
	// provider.
	for _, w := range widths {
		for active := range multi {
			m := NewModelForProviders(multi)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive() // mirror the new active provider's state, as switchProvider does
			if got := lipgloss.Width(m.renderHeader()); got > w {
				t.Errorf("width=%d active=%d: header row is %d cells, exceeds %d (wrap onto tab bar):\n%s",
					w, active, got, w, m.renderHeader())
			}
			// (b) The active provider's chip is always present — visible in
			// the rendered row and returned by chipAt at its position.
			layout := m.budgetedChips()
			found := false
			for k, prov := range layout.providers {
				if prov == active {
					found = true
					// chipAt must hit-test this chip somewhere inside its
					// rendered span. Walk the right-aligned layout the same
					// way chipAt does.
					right := m.width - 2
					for i, v := range slices.Backward(layout.parts) {
						cw := lipgloss.Width(v)
						if i == k {
							if idx, ok := m.chipAt(right); !ok || idx != prov {
								t.Errorf("width=%d active=%d: chipAt(%d) = (%d,%v), want (%d,true)",
									w, active, right, idx, ok, prov)
							}
							break
						}
						right -= cw + 1
					}
				}
			}
			if !found {
				t.Errorf("width=%d active=%d: active provider's chip missing from the rendered switcher", w, active)
			}
		}
	}

	// (c) chipAt maps every rendered chip x-position to exactly that chip,
	// and never reports a provider whose chip is not rendered.
	for _, w := range widths {
		for active := range multi {
			m := NewModelForProviders(multi)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive()
			layout := m.budgetedChips()
			rendered := make(map[int]bool)
			for _, prov := range layout.providers {
				rendered[prov] = true
			}
			// Every column of the row: a hit must be a rendered chip.
			for x := 0; x < m.width; x++ {
				idx, ok := m.chipAt(x)
				if ok && !rendered[idx] {
					t.Errorf("width=%d active=%d: chipAt(%d) hit provider %d whose chip is not rendered", w, active, x, idx)
				}
			}
			// Every rendered chip: each of its columns maps back to itself.
			right := m.width - 2
			for i, v := range slices.Backward(layout.parts) {
				cw := lipgloss.Width(v)
				prov := layout.providers[i]
				for x := right - cw + 1; x <= right; x++ {
					if idx, ok := m.chipAt(x); !ok || idx != prov {
						t.Errorf("width=%d active=%d: chipAt(%d) = (%d,%v), want (%d,true) across the rendered chip span",
							w, active, x, idx, ok, prov)
					}
				}
				right -= cw + 1
			}
		}
	}

	// (d) The single-provider header keeps the legacy layout: byte-identical
	// at every width the legacy row fits (no switcher, no budgeting), and
	// truncated — never wrapping — below that. The legacy natural row is 67
	// content cells + 2 padding = 69 total.
	single := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	single.width = 80
	single.height = 24
	want := single.renderHeader()
	if w := lipgloss.Width(want); w != 69 {
		t.Fatalf("legacy single-provider header width = %d, want the 69-cell baseline this test pins", w)
	}
	for _, w := range widths {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		got := m.renderHeader()
		if w >= 69 && got != want {
			t.Errorf("width=%d: single-provider header changed:\n got: %q\nwant: %q", w, got, want)
		}
		if lipgloss.Width(got) > w {
			t.Errorf("width=%d: single-provider header is %d cells, exceeds %d", w, lipgloss.Width(got), w)
		}
	}
}

func TestSnapshotSugarGuardsNoProviders(t *testing.T) {
	// A model created with no providers must accept a legacy metrics.Snapshot
	// update without panicking (the ProviderUpdate guard mirrors this).
	m := NewModelForProviders(nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("metrics.Snapshot on a zero-provider model panicked: %v", r)
		}
	}()
	update(m, metrics.Snapshot{})
}

func TestMultiProviderHeaderShowsNames(t *testing.T) {
	// Width 120 gives the budget for both full names (at 80 the "anthropic"
	// chip legitimately truncates to "anthro" — see
	// TestHeaderWidthBudget), so this test pins natural rendering and
	// styling; the width test pins degradation.
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 120
	m.height = 24

	if !m.hasSwitcher() {
		t.Fatal("multiple providers must render a provider switcher")
	}
	// At width 120 both chips fit at their full styled widths, so the
	// budgeted layout must equal the unbudgeted rendering exactly.
	layout := m.budgetedChips()
	if len(layout.parts) != 2 || layout.providers[0] != 0 || layout.providers[1] != 1 {
		t.Fatalf("budgetedChips layout = %v, want both providers [0 1] at width 120", layout.providers)
	}
	if layout.parts[0] != m.styles.chipActiveStyle.Render(" acme ") {
		t.Errorf("active chip = %q, want the untruncated active style %q", layout.parts[0], m.styles.chipActiveStyle.Render(" acme "))
	}
	if layout.parts[1] != m.styles.chipInactiveStyle.Render(" anthropic ") {
		t.Errorf("inactive chip = %q, want the untruncated inactive style %q", layout.parts[1], m.styles.chipInactiveStyle.Render(" anthropic "))
	}
	for _, want := range []string{"acme", "anthropic"} {
		if !strings.Contains(m.renderHeader(), want) {
			t.Errorf("header should show both provider names in its chips, missing %q", want)
		}
	}

	// Switching providers updates the highlighted chip.
	m = update(m, tea.MouseClickMsg{X: m.width - 2, Y: 0})
	layout = m.budgetedChips()
	if layout.parts[0] != m.styles.chipInactiveStyle.Render(" acme ") {
		t.Errorf("after switching, acme chip = %q, want the inactive style", layout.parts[0])
	}
	if layout.parts[1] != m.styles.chipActiveStyle.Render(" anthropic ") {
		t.Errorf("after switching, anthropic chip = %q, want the active style", layout.parts[1])
	}
}

// TestResetStatsSendsOnChannel proves the c -> y confirm path delivers a
// signal on the model's reset channel (Task 6: the channel is drained by
// main, which calls Collector.Reset for every provider).
func TestResetStatsSendsOnChannel(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	// "c" opens the confirm overlay; anything other than "y" must dismiss
	// without signalling.
	m = update(m, key('n'))
	if m.mode != modeBrowse {
		t.Fatalf("after n: mode = %v, want modeBrowse", m.mode)
	}
	select {
	case <-m.resetCh:
		t.Fatal("n must not signal reset")
	default:
	}

	// c then y signals reset exactly once and returns to browse mode.
	m = update(m, key('c'))
	if m.mode != modeConfirm {
		t.Fatalf("after c: mode = %v, want modeConfirm", m.mode)
	}
	next, cmd := m.Update(key('y'))
	m2, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	m = m2
	if m.mode != modeBrowse {
		t.Fatalf("after y: mode = %v, want modeBrowse", m.mode)
	}
	if cmd == nil {
		t.Fatal("y must return the reset command")
	}
	// The command yields resetMsg; feeding it back through Update performs
	// the non-blocking send onto the reset channel.
	m = update(m, cmd())
	select {
	case <-m.resetCh:
	default:
		t.Fatal("c then y must signal reset")
	}
	select {
	case <-m.resetCh:
		t.Fatal("reset signal must not duplicate")
	default:
	}
}

// TestResetStatsSendNeverBlocks proves a second confirm while the buffer is
// still occupied is dropped, not deadlocked: the model's send is
// non-blocking and the channel is capped at one pending request.
func TestResetStatsSendNeverBlocks(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.resetCh = make(chan struct{}, 1)
	m.resetCh <- struct{}{} // occupy the buffer as main might lag

	done := make(chan Model, 1)
	go func() {
		m = update(m, key('c'))
		m = update(m, key('y'))
		done <- m
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("update with a full reset channel must not block")
	}
}

// TestFleetStrip_AggregateObservability pins the one-line fleet strip atop the
// TUI in multi-provider mode (M6/G8).
func TestFleetStrip_AggregateObservability(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "openai", Concurrency: 8},
		{Name: "anthropic", Concurrency: 4},
		{Name: "gemini", Concurrency: 6},
	}
	m := NewModelForProviders(metas)
	m.width = 80
	m.height = 24

	// Update snapshots across providers:
	// - openai: 3 active, 1 queued, circuit breaker CLOSED
	// - anthropic: 2 active, 1 queued, circuit breaker OPEN
	// - gemini: 0 active, 0 queued, circuit breaker CLOSED
	// Total: 5 active · 2 queued · 1 OPEN · busiest: openai
	m = update(m, ProviderUpdate{
		Index: 0,
		Snapshot: metrics.Snapshot{
			Active: 3,
			Queued: 1,
			CircuitBreaker: &metrics.CBStats{
				State: "CLOSED",
			},
		},
	})
	m = update(m, ProviderUpdate{
		Index: 1,
		Snapshot: metrics.Snapshot{
			Active: 2,
			Queued: 1,
			CircuitBreaker: &metrics.CBStats{
				State: "OPEN",
			},
		},
	})
	m = update(m, ProviderUpdate{
		Index: 2,
		Snapshot: metrics.Snapshot{
			Active: 0,
			Queued: 0,
			CircuitBreaker: &metrics.CBStats{
				State: "CLOSED",
			},
		},
	})

	hdr := stripANSI(m.renderHeader())
	want := "Fleet: 5 active · 2 queued · 1 OPEN · busiest: openai"
	if !strings.Contains(hdr, want) {
		t.Fatalf("header missing fleet strip %q; got header: %q", want, hdr)
	}

	// Provider switcher chips are rendered on the right side of the same row.
	layout := m.budgetedChips()
	if len(layout.parts) == 0 {
		t.Fatal("budgetedChips should fit chips at 80 cols")
	}
	// Active provider (index 0) is present
	foundActive := false
	for _, p := range layout.providers {
		if p == 0 {
			foundActive = true
			break
		}
	}
	if !foundActive {
		t.Error("active provider chip must be present in switcher")
	}
}
