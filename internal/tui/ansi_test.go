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
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

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

// TestStripANSI_SwallowsNonCSISequences pins OSC payloads
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

// TestStripANSI_StripsControlBytes pins C0 controls other than
// tab, DEL, and the C1 block are stripped — both their UTF-8 encodings and
// stray raw bytes in the 0x80–0x9F range, which are the 8-bit control
// positions legacy terminals act on. Valid multibyte text must survive
// untouched even when its continuation bytes fall inside that range (the
// emoji row guards against any naive byte-level implementation). The \n row
// additionally pins the rebuttal: no literal newline can survive
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

// TestStripANSI_PreservesEncodedReplacementChar pins the encoded replacement
// character: a
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
