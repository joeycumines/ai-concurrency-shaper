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
	"regexp"
	"strconv"
	"strings"
)

// This file owns captured-log-line classification for the Logs tab: parsing
// slog TextHandler-shaped records, deciding whether a line warrants a toast,
// and deriving the deduplication key that collapses recurring incidents.
// Everything here operates on already ANSI-stripped lines as captured.

var (
	// logDatePrefixRe strips a stdlib log prefix (YYYY/MM/DD HH:MM:SS) so the
	// dedup key of a recurring stdlib message is stable across occurrences even
	// though each line carries a different timestamp.
	logDatePrefixRe = regexp.MustCompile(`^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} `)

	// slogLevelOffsetRe strips the numeric offset slog's TextHandler appends to
	// custom severities ("WARN-3", "ERROR+8"), so severity lookup keys on the
	// base name that actually carries the signal.
	slogLevelOffsetRe = regexp.MustCompile(`[+-][0-9]+$`)
)

// slogField is one top-level key=value pair of an slog TextHandler record.
type slogField struct {
	key   string
	value string
}

// slogRecord is a parsed slog TextHandler-shaped line: its leading top-level
// fields, the semantic (unescaped) msg value that terminates the run, and the
// raw remainder of the line after the msg token.
type slogRecord struct {
	fields []slogField
	msg    string
	rest   string
}

// scanQuoted returns the index of the closing quote of the double-quoted string
// starting at s[i], honoring backslash escapes.
func scanQuoted(s string, i int) (int, bool) {
	i++
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
		case '"':
			return i, true
		default:
			i++
		}
	}
	return 0, false
}

// isSlogKeyChar reports whether c may appear in a bare slog attribute key.
func isSlogKeyChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
		c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		return true
	}
	return false
}

// parseSlogRecord parses an slog TextHandler-shaped line: a run of top-level
// key=value fields (bare or quoted keys; bare \S+ or quoted values with \\
// escapes, separated by single spaces) whose LAST parsed field is msg. It stops
// at the first token that does not continue the shape, mirroring the anchoring
// of the regex it replaces: any junk between fields or before msg makes the
// line not slog-shaped. ok is false when the line does not have that shape,
// with a nil record.
func parseSlogRecord(line string) (*slogRecord, bool) {
	rec := &slogRecord{}
	i := 0
	for {
		if i >= len(line) {
			return nil, false
		}
		var key string
		if line[i] == '"' {
			end, ok := scanQuoted(line, i)
			if !ok {
				return nil, false
			}
			raw := line[i : end+1]
			u, err := strconv.Unquote(raw)
			if err != nil {
				return nil, false
			}
			key = u
			i = end + 1
		} else {
			j := i
			for j < len(line) && isSlogKeyChar(line[j]) {
				j++
			}
			if j == i {
				return nil, false
			}
			key = line[i:j]
			i = j
		}
		if i >= len(line) || line[i] != '=' {
			return nil, false
		}
		i++
		var value string
		if i < len(line) && line[i] == '"' {
			end, ok := scanQuoted(line, i)
			if !ok {
				return nil, false
			}
			raw := line[i : end+1]
			value = raw[1 : len(raw)-1]
			if u, err := strconv.Unquote(raw); err == nil {
				value = u
			}
			i = end + 1
		} else {
			j := i
			for j < len(line) && line[j] != ' ' {
				j++
			}
			if j == i {
				return nil, false
			}
			value = line[i:j]
			i = j
		}
		if key == "msg" {
			if i < len(line) && line[i] == ' ' {
				i++
			}
			rec.msg = value
			rec.rest = line[i:]
			return rec, true
		}
		rec.fields = append(rec.fields, slogField{key: key, value: value})
		if i >= len(line) {
			return nil, false
		}
		if line[i] != ' ' {
			return nil, false
		}
		i++
	}
}

// errWarnKeywords are whole-word tokens that make a log line eligible for a
// toast. Matching is token-boundary based (see wordContains) so that keywords
// embedded inside identifier-like config values — e.g. "open-timeouts=10s"
// must not trip on "timeouts" — never produce a false-positive toast. The
// stems and every inflected form common failure prose actually uses are listed
// explicitly: token matching never matches a prefix of a longer word, so
// "fail" alone cannot catch "failed", "fails", or "failing".
var errWarnKeywords = []string{
	"error", "errors",
	"fail", "fails", "failing", "failed", "failure", "failures",
	"panic", "panicked", "panicking",
	"warn", "warning", "warnings",
	"timeout", "timeouts",
	"timed out",
}

// slogLevelActionable maps a top-level slog level value to its actionability:
// true for WARN-or-worse, false for trace/debug/info, and absent for base
// names that carry no signal either way. Numeric offsets the TextHandler
// appends to custom severities are stripped before lookup (see
// slogLevelOffsetRe), so WARN-3 inherits warn's signal instead of falling
// through to keyword scanning; only unknown base names (e.g. NOTICE) fall
// through. Both the WARN and WARNING spellings appear in the wild across
// bridges and migrated code, so both are mapped.
var slogLevelActionable = map[string]bool{
	"trace":    false,
	"debug":    false,
	"info":     false,
	"warn":     true,
	"warning":  true,
	"error":    true,
	"fatal":    true,
	"critical": true,
	"panic":    true,
}

// logLineIsActionable reports whether a captured log line looks like an error or
// warning worth a toast. When the line parses as an slog TextHandler record,
// severity comes ONLY from its top-level level=<value> field — never from raw
// text scanned elsewhere on the line, where a quoted attribute value such as
// k="level=INFO" could otherwise impersonate the record's real level in either
// direction (suppressing an ERROR or fabricating a toast). A slog record whose
// level carries no actionable signal falls through to keyword scanning of its
// MESSAGE alone (attributes are structured metadata, not prose); non-slog lines
// (stdlib output, plain prose) keep whole-line keyword scanning.
//
// The parse always runs over the ORIGINAL line bytes (review-13 issue 1):
// lowercasing the line first would rewrite uppercase \U escapes — which
// strconv.Unquote reads as 8 hex digits — into \u escapes read as 4 hex digits,
// corrupting the decoded message ("\U0001F512FAILED" would become
// "\x01f512failed") and diverging from logDedupKey, which parses the original
// bytes. Case insensitivity is preserved by lowercasing only the extracted
// key, level value, and keyword-scan scope.
func logLineIsActionable(line string) bool {
	keywordScope := strings.ToLower(line)
	if rec, ok := parseSlogRecord(line); ok {
		keywordScope = strings.ToLower(rec.msg)
		for _, f := range rec.fields {
			if strings.ToLower(f.key) == "level" {
				level := strings.ToLower(slogLevelOffsetRe.ReplaceAllString(f.value, ""))
				if actionable, known := slogLevelActionable[level]; known {
					return actionable
				}
				break
			}
		}
	}
	for _, kw := range errWarnKeywords {
		if wordContains(keywordScope, kw) {
			return true
		}
	}
	return false
}

// logDedupKey returns the stable deduplication key for a log line: the msg
// value of an slog key=value record — suffixed with any trailing structured
// attributes so records that differ beyond their message (route=, err=, ...)
// stay distinct incidents — or the timestamp-stripped remainder of a stdlib
// line, or the whole line otherwise, lower-cased. Each captured line carries a
// different ingest-time stamp, so extracting the message itself is what lets a
// recurring error dedup to a single toast. Lines without trailing slog
// attributes keep exactly the legacy msg-only key. Empty result means "cannot
// key this line; treat it as actionable-only-news". Severity is deliberately
// not part of the key: it precedes msg in slog output and same-message
// different-level collapse is acceptable.
//
// The msg|attrs join is framed with a decimal length prefix on the msg
// component: a bare "|" is ambiguous because a message may itself contain
// "|route=a" — length-prefixing makes joined ("msg=request failed|route=a")
// and split ("msg=request failed" route=a) records produce different keys.
func logDedupKey(line string) string {
	line = strings.TrimSpace(line)
	if rec, ok := parseSlogRecord(line); ok {
		key := strings.ToLower(strings.TrimSpace(rec.msg))
		if rest := strings.ToLower(strings.TrimSpace(rec.rest)); rest != "" {
			return strconv.Itoa(len(key)) + ":" + key + "|" + rest
		}
		return key
	}
	rest := line
	if loc := logDatePrefixRe.FindStringIndex(rest); loc != nil {
		rest = rest[loc[1]:]
	}
	return strings.ToLower(strings.TrimSpace(rest))
}

// wordContains reports whether lower contains kw delimited by non-token
// characters on both sides. Token characters are [0-9a-z_-], so hyphens and
// underscores bind within a single identifier (e.g. "open-timeout"), while
// "timeout" surrounded by spaces or punctuation still matches.
func wordContains(lower, kw string) bool {
	start := 0
	for {
		idx := strings.Index(lower[start:], kw)
		if idx < 0 {
			return false
		}
		pos := start + idx
		beforeOK := pos == 0 || !isTokenChar(lower[pos-1])
		after := pos + len(kw)
		afterOK := after >= len(lower) || !isTokenChar(lower[after])
		if beforeOK && afterOK {
			return true
		}
		start = pos + 1
	}
}

// isTokenChar reports whether c participates in an identifier-like token.
func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z',
		c >= '0' && c <= '9',
		c == '-', c == '_':
		return true
	}
	return false
}
