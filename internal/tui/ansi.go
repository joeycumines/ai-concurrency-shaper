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
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

func truncateANSI(line string, width int) string {
	return truncateGraphemes(line, width, true)
}

// skipANSI returns line with the first skip visible cells removed, preserving
// ANSI escape sequences (which never consume visible width). If skip exceeds
// the line's visible width, an empty string is returned.
func skipANSI(line string, skip int) string {
	if skip <= 0 {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	cells := 0
	state := -1
	for i := 0; i < len(line); {
		if line[i] == '\x1b' {
			j := i + 1
			if j < len(line) && line[j] == '[' {
				j++
				for j < len(line) && !(line[j] >= 0x40 && line[j] <= 0x7E) {
					j++
				}
				if j < len(line) {
					j++
				}
			} else if j < len(line) {
				j++
			}
			// Preserve every escape sequence, even inside the skipped
			// region: an SGR opened before the cut point must remain in
			// effect for the text that follows it.
			b.WriteString(line[i:j])
			i = j
			state = -1
			continue
		}
		cluster, _, w, newState := uniseg.FirstGraphemeClusterInString(line[i:], state)
		if cells >= skip {
			b.WriteString(cluster)
		}
		cells += w
		i += len(cluster)
		state = newState
	}
	return b.String()
}

// truncatePlain truncates a plain, unstyled string to at most width terminal
// cells using the same grapheme-cluster semantics as truncateANSI. It never
// emits escape sequences: the callers feed the result into a surrounding
// lipgloss style, where an embedded reset would strip that style's background
// from the rest of the rendered row.
func truncatePlain(s string, width int) string {
	return truncateGraphemes(s, width, false)
}

// firstGrapheme returns the leading grapheme cluster of s ("" for empty
// input). It is the unit of progress for word-splitting loops that must
// advance even when a single cluster is wider than the available width.
func firstGrapheme(s string) string {
	cluster, _, _, _ := uniseg.FirstGraphemeClusterInString(s, -1)
	return cluster
}

// truncateGraphemes is the shared core of truncateANSI and truncatePlain: it
// walks grapheme clusters, counting cells, and stops once the next cluster
// would exceed width. When keepEscapes is set, ANSI escape sequences pass
// through without consuming width (truncateANSI); otherwise any escape bytes
// are treated as ordinary text (truncatePlain — its callers guarantee plain
// input).
func truncateGraphemes(line string, width int, keepEscapes bool) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	cells := 0
	truncated := false
	state := -1
	for i := 0; i < len(line); {
		if keepEscapes && line[i] == '\x1b' {
			j := i + 1
			if j < len(line) && line[j] == '[' {
				j++
				for j < len(line) && !(line[j] >= 0x40 && line[j] <= 0x7E) {
					j++
				}
				if j < len(line) {
					j++
				}
			} else if j < len(line) {
				j++
			}
			b.WriteString(line[i:j])
			i = j
			// ANSI sequences are non-printing separators; they must not carry
			// grapheme-cluster state across to the following visible text.
			state = -1
			continue
		}
		cluster, _, w, newState := uniseg.FirstGraphemeClusterInString(line[i:], state)
		if cells+w > width {
			truncated = true
			break
		}
		b.WriteString(cluster)
		cells += w
		i += len(cluster)
		state = newState
	}
	if truncated && keepEscapes {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// stripANSI removes ANSI escape sequences and terminal control characters from
// a string: CSI, OSC (terminated by BEL or ST), the ST-terminated string
// sequences DCS/SOS/PM/APC, bare two-byte ESC sequences, all C0 controls except
// tab, and DEL plus the C1 control block U+0080–U+009F — both their UTF-8
// encodings and stray raw bytes in that range, which are the 8-bit control
// positions legacy terminals act on. Valid multibyte text passes through
// untouched even when its continuation bytes fall inside the C1 range, and the
// payloads of string sequences are swallowed whole, so logged output cannot
// carry cursor movement or other control-function content of those classes
// into the Logs tab or a toast message.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\x1b' {
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) {
					ch := s[i]
					if ch >= 0x40 && ch <= 0x7E {
						break
					}
					i++
				}
			} else if i+1 < len(s) && s[i+1] == ']' {
				i = skipStringSequence(s, i+2, true)
			} else if i+1 < len(s) && (s[i+1] == 'P' || s[i+1] == 'X' || s[i+1] == '^' || s[i+1] == '_') {
				i = skipStringSequence(s, i+2, false)
			} else if i+1 < len(s) {
				i++
			}
			continue
		}
		if c < utf8.RuneSelf {
			// Printable ASCII and tab survive; every other C0 control and DEL
			// is dropped before it can reposition a cursor or ring a bell.
			if c == '\t' || (c >= 0x20 && c != 0x7F) {
				b.WriteByte(c)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 && r == utf8.RuneError {
			// Invalid UTF-8 byte. Stray bytes sitting in the raw C1 range are
			// dropped like the 8-bit controls a legacy terminal would act on;
			// other invalid bytes pass through untouched.
			if c < 0x80 || c > 0x9F {
				b.WriteByte(c)
			}
		} else {
			// Valid rune — including a legitimately encoded U+FFFD, which
			// DecodeRuneInString also reports as RuneError (size 3).
			if r > 0x9F {
				b.WriteString(s[i : i+size])
			}
			// Otherwise a valid UTF-8-encoded C1 control: drop.
		}
		i += size - 1 // the loop's post statement supplies the final step
	}
	return b.String()
}

// skipStringSequence returns the index of the last byte of the terminator that
// closes the OSC/DCS/SOS/PM/APC body starting at s[i] — BEL or ST for an OSC,
// ST alone for the rest — so stripANSI's loop increment steps past it. Input
// that ends before a terminator consumes through EOF (returning len(s)-1)
// rather than leak the unterminated payload.
func skipStringSequence(s string, i int, belTerminated bool) int {
	for i < len(s) {
		if belTerminated && s[i] == '\x07' {
			return i
		}
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
			return i + 1
		}
		i++
	}
	return len(s) - 1
}

// renderContentWithScrollbar wraps the active tab's content with a scrollbar
// column in the rightmost position. ANSI-aware width calculation. The
// scrollbar is aligned with the scrollable data rows, below the fixed header
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		if maxLen == 1 {
			return "…"
		}
		return ""
	}
	return string(runes[:maxLen-1]) + "…"
}

// truncateBytes truncates a byte slice to at most maxRunes runes, appending
// an ellipsis if truncated. It operates directly on the byte slice using
// utf8.DecodeRune, avoiding the deep copy (string + []rune) that would
// allocate megabytes for large response bodies at 4 fps. The ellipsis counts
// toward the rune budget: the output is at most maxRunes runes total.
func truncateBytes(b []byte, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if maxRunes == 1 {
		return "…"
	}
	if utf8.RuneCount(b) <= maxRunes {
		return string(b)
	}
	// Keep maxRunes-1 content runes + ellipsis (1 rune) = maxRunes total.
	contentRunes := maxRunes - 1
	var buf strings.Builder
	buf.Grow(maxRunes*4 + 3)
	count := 0
	for len(b) > 0 && count < contentRunes {
		r, size := utf8.DecodeRune(b)
		buf.WriteRune(r)
		b = b[size:]
		count++
	}
	buf.WriteString("…")
	return buf.String()
}
