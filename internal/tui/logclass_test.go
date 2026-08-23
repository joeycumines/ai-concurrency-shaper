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

import "testing"

// TestLogDedupKey_TimestampInsensitive pins the toast-dedup normalization: the
// ingest-time prefix (slog time= / stdlib date stamp) varies on every line, so
// the key must ignore it or recurring errors would never dedup.
func TestLogDedupKey_TimestampInsensitive(t *testing.T) {
	slogA := `time=2026-08-19T07:38:15.123Z level=ERROR msg="proxy transport error: boom"`
	slogB := `time=2026-08-19T07:39:42.987Z level=ERROR msg="proxy transport error: boom"`
	if a, b := logDedupKey(slogA), logDedupKey(slogB); a == "" || a != b {
		t.Fatalf("same recurring slog message dedups: %q vs %q", a, b)
	}
	if got := logDedupKey(slogA); got != "proxy transport error: boom" {
		t.Fatalf("slog key = %q, want %q", got, "proxy transport error: boom")
	}

	stdlibA := `2026/08/19 07:38:15 proxy transport error: boom`
	stdlibB := `2026/08/19 07:39:42 proxy transport error: boom`
	if a, b := logDedupKey(stdlibA), logDedupKey(stdlibB); a == "" || a != b {
		t.Fatalf("same recurring stdlib message dedups: %q vs %q", a, b)
	}

	distinct := `time=2026-08-19T07:38:15.123Z level=ERROR msg="queue full refused"`
	if a, b := logDedupKey(slogA), logDedupKey(distinct); a == b {
		t.Fatalf("distinct messages must not dedup: %q == %q", a, b)
	}

	if got, want := logDedupKey("proxy transport error: boom"), "proxy transport error: boom"; got != want {
		t.Fatalf("plain line key = %q, want %q", got, want)
	}
}

// TestLogDedupKey_NonSlogMsgSubstring pins the slog-shape gate on the dedup
// key: a non-slog line that merely contains " msg=" must key on its full
// timestamp-stripped remainder instead of a bogus slog msg fragment, while a
// genuine slog line still extracts exactly its msg attribute — unescaped when
// quoted.
func TestLogDedupKey_NonSlogMsgSubstring(t *testing.T) {
	line := `2026/08/19 07:38:15 job failed msg=retry: boom`
	if got, want := logDedupKey(line), "job failed msg=retry: boom"; got != want {
		t.Fatalf("non-slog line key = %q, want %q", got, want)
	}

	slog := `time=2026-08-19T07:38:15.123Z level=ERROR msg="proxy transport error: boom"`
	if got := logDedupKey(slog); got != "proxy transport error: boom" {
		t.Fatalf("slog key = %q, want %q", got, "proxy transport error: boom")
	}

	esc := `time=2026-08-19T07:38:15.123Z level=ERROR msg="say \"boom\" loudly"`
	if got, want := logDedupKey(esc), `say "boom" loudly`; got != want {
		t.Fatalf("escaped-quote slog key = %q, want %q", got, want)
	}
}

func TestLogLineIsActionable_WholeWordTokens(t *testing.T) {
	tests := []struct {
		name, line string
		want       bool
	}{
		{"proxy transport error", "proxy transport error: boom", true},
		{"slog info config open-timeout", `time=... level=INFO msg="circuit breaker: threshold=5 window=30s open-timeout=10s penalty=2s"`, false},
		{"slog info mute", `time=... level=INFO msg="TUI dashboard enabled"`, false},
		{"slog info auto-detect", "auto-detecting LLM endpoints (24 patterns) at concurrency 4", false},
		{"slog warn level", `time=... level=WARN msg="queue timeout"`, true},
		{"slog error level", `time=... level=ERROR msg="boom"`, true},
		{"slog fatal level bare msg", `time=... level=FATAL msg="boom"`, true},
		{"word inside hyphenated id", "flags: open-timeout=10s override-error=1", false},
		{"hyphenated plural ids stay quiet", "open-timeouts=9s override-errors=1 reset", false},
		{"queue timeout standalone", "queue timeout after 30s", true},
		{"timed out phrasing", "the request timed out", true},
		{"plural errors toast", "several errors occurred", true},
		{"plural warnings toast", "2 warnings found in lint output", true},
		{"plural timeouts toast", "upstream timeouts observed", true},
		{"fails verb toast", "build fails on missing key", true},
		{"failing participle toast", "stream failing back to retry", true},
		{"panic standalone", "proxy panic: x", true},
		{"panicked past tense", "handler panicked on nil map", true},
		{"failed past tense", "request failed: connection refused", true},
		{"failure noun", "failure connecting upstream", true},
		{"failure plural", "3 failures before circuit opened", true},
		{"warning colon form", "WARNING: route conflict for POST /v1/messages", true},
		{"slog custom warn offset", `time=... level=WARN-3 msg="benign text"`, true},
		{"slog error offset", `time=... level=ERROR+1 msg="boom"`, true},
		{"slog WARNING spelling", `time=... level=WARNING msg="disk almost full"`, true},
		{"slog info offset stays quiet", `time=... level=INFO+2 msg="all good"`, false},
		// The TUI startup summary must stay hyphen-bound ("failure-hold: 2s").
		// A space between "failure" and "hold" reads like prose and toasts on
		// every start (see main.go's startup summaries); the hyphen binds the two
		// into one identifier, which isTokenChar keeps from matching the keyword.
		{"failure-hold startup summary quiet", "2026/08/23 18:43:11 failure-hold: 2s", false},
		{"failure hold prose remains actionable", "2026/08/23 18:43:11 failure hold: 2s", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logLineIsActionable(tt.line); got != tt.want {
				t.Errorf("logLineIsActionable(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestLogDedupKey_QuotedPreMsgAttribute pins review-06 issue 5 hardening: the slog
// dedup regex must handle a quoted value with spaces in an attribute preceding msg.
// Without the `(?:\S+|\"(?:\\.|[^"\\])*")` alternative, a line like
// `k="a b" level=ERROR msg="hi"` would fail to extract msg and would not dedup.
func TestLogDedupKey_QuotedPreMsgAttribute(t *testing.T) {
	cases := []struct {
		line, want string
	}{
		{`time="2026-08-19 07:38:15" level=ERROR msg="boom with spaces"`, "boom with spaces"},
		{`k="a b" level=ERROR msg="hi there"`, "hi there"},
		{`k="a \"b\" c" level=WARN msg="quoted escape"`, `quoted escape`},
		// Bare pre-msg still works
		{`time=2026-08-19T07:38:15.123Z level=ERROR msg="proxy error"`, "proxy error"},
		{`level=WARN msg=hello`, "hello"},
	}
	for _, tc := range cases {
		if got := logDedupKey(tc.line); got != tc.want {
			t.Errorf("logDedupKey(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
	// Non-slog prose containing " msg=" must still key on full remainder, not slog
	if got, want := logDedupKey(`2026/08/19 07:38:15 job failed msg=retry: boom`), "job failed msg=retry: boom"; got != want {
		t.Fatalf("non-slog prose key = %q, want %q", got, want)
	}
	// Ensure quoted msg with spaces still unescapes correctly when preceded by quoted attr
	esc := `k="x y" time=2026-08-19T07:38:15.123Z level=ERROR msg="say \"boom\" loudly"`
	if got, want := logDedupKey(esc), `say "boom" loudly`; got != want {
		t.Fatalf("escaped-quote with quoted pre-msg = %q, want %q", got, want)
	}
}

// TestLogLineIsActionable_QuotedPreMsgAttribute pins review-09 issue 1: severity
// detection must locate the REAL msg token (quote-aware), not the first
// occurrence of the substring "msg=". A quoted pre-msg attribute containing
// "msg=" previously hid the true level attribute from both the INFO suppression
// and the WARN-or-worse detection.
func TestLogLineIsActionable_QuotedPreMsgAttribute(t *testing.T) {
	tests := []struct {
		name, line string
		want       bool
	}{
		// The review's false-positive: level=INFO hidden behind a quoted
		// pre-msg value containing "msg=", payload matches the 'failed' keyword.
		{"info hidden by quoted msg= substring", `k="a msg= b" level=INFO msg="failed to open"`, false},
		// The review's false-negative: level=FATAL likewise hidden.
		{"fatal hidden by quoted msg= substring", `k="a msg= b" level=FATAL msg="shutdown"`, true},
		// Sanity: same shape with a genuinely warn-level record.
		{"warn with quoted msg= substring", `k="a msg= b" level=WARN msg="queue timeout"`, true},
		// Quoted timestamp attribute (no embedded msg=) keeps working.
		{"info with quoted time attr", `time="2026-08-19 07:38:15" level=info msg="open-timeout=10s configured"`, false},
		{"error with quoted time attr", `time="2026-08-19 07:38:15" level=ERROR msg="boom"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logLineIsActionable(tt.line); got != tt.want {
				t.Errorf("logLineIsActionable(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestLogLineIsActionable_QuotedLevelSubstring pins review-10 blocker 1:
// severity must come ONLY from a top-level level= field of the slog record.
// A quoted attribute VALUE containing "level=..." must neither suppress a real
// ERROR nor fabricate a toast for a non-info record, and a quoted attribute KEY
// ("user agent"=curl) must still parse as slog-shaped so its genuine level wins
// over keyword scanning of the message text.
func TestLogLineIsActionable_QuotedLevelSubstring(t *testing.T) {
	tests := []struct {
		name, line string
		want       bool
	}{
		{"quoted level=INFO must not suppress real ERROR", `k="level=INFO" level=ERROR msg="upstream unavailable"`, true},
		{"quoted level=ERROR must not fabricate severity", `k="level=ERROR" level=NOTICE msg="routine status report"`, false},
		{"quoted key keeps slog shape and real INFO", `"user agent"=curl level=INFO msg="failed to open"`, false},
		{"quoted key keeps slog shape and real WARN", `"user agent"=curl level=WARN msg="routine text"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logLineIsActionable(tt.line); got != tt.want {
				t.Errorf("logLineIsActionable(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestLogDedupKey_NoDelimiterCollision pins review-10 blocker 2: a msg that
// itself contains the "|" framing delimiter must never alias a different record
// whose msg is the delimiter-split prefix and whose trailing attributes supply
// the rest — the two records are distinct incidents and must not dedup together.
func TestLogDedupKey_NoDelimiterCollision(t *testing.T) {
	joined := `time=2026-08-22T10:00:00.000Z level=ERROR msg="request failed|route=a"`
	split := `time=2026-08-22T10:00:00.500Z level=ERROR msg="request failed" route=a`
	if ka, kb := logDedupKey(joined), logDedupKey(split); ka == kb {
		t.Fatalf("distinct records must not share a key: %q == %q", ka, kb)
	}
	// The framed form stays stable for repeated identical records.
	again := `time=2026-08-22T10:00:01.000Z level=ERROR msg="request failed|route=a"`
	if ka, ka2 := logDedupKey(joined), logDedupKey(again); ka != ka2 {
		t.Fatalf("identical records must dedup together: %q vs %q", ka, ka2)
	}
}

// TestLogDedupKey_QuotedAttributeKey pins review-11 #3: slog quotes keys that
// contain spaces ("user agent"=curl). Such a line must still parse as
// slog-shaped so its dedup key comes from the msg value, not from the whole
// raw line treated as prose.
func TestLogDedupKey_QuotedAttributeKey(t *testing.T) {
	line := `"user agent"=curl level=WARN msg="queue timeout" route=x`
	if got, want := logDedupKey(line), "13:queue timeout|route=x"; got != want {
		t.Fatalf("quoted-key slog line key = %q, want %q", got, want)
	}
}

// TestLogDedupKey_TrailingAttributes pins review-07 #7: structured slog records
// that differ beyond their message text (route=, err=, ...) must not collapse to
// one dedup key, while records without trailing attributes keep exactly the
// legacy keys.
func TestLogDedupKey_TrailingAttributes(t *testing.T) {
	a := `time=2026-08-22T10:00:00.000Z level=ERROR msg="request failed" route=a err=timeout`
	b := `time=2026-08-22T10:00:01.000Z level=ERROR msg="request failed" route=b err=permission-denied`
	if ka, kb := logDedupKey(a), logDedupKey(b); ka == kb {
		t.Fatalf("distinct trailing attributes must not dedup together: %q == %q", ka, kb)
	}

	aAgain := `time=2026-08-22T10:00:02.000Z level=ERROR msg="request failed" route=a err=timeout`
	if ka, ka2 := logDedupKey(a), logDedupKey(aAgain); ka != ka2 {
		t.Fatalf("identical attr sets must dedup together: %q vs %q", ka, ka2)
	}

	// No trailing attrs => legacy key, byte-for-byte.
	if got, want := logDedupKey(`time=2026-08-22T10:00:00.000Z level=ERROR msg="proxy transport error: boom"`), "proxy transport error: boom"; got != want {
		t.Fatalf("legacy no-attr key = %q, want %q", got, want)
	}
	// Bare msg form with trailing attributes also keys on them (framed: msg
	// length prefix makes the "|" join unambiguous).
	if got, want := logDedupKey(`level=WARN msg=timeout route=x`), "7:timeout|route=x"; got != want {
		t.Fatalf("bare msg with attrs key = %q, want %q", got, want)
	}
}

// TestLogLineIsActionable_UnicodeEscapeMsgNotCorrupted pins review-13 issue 1:
// actionability must be decided from a parse of the ORIGINAL line bytes, never
// from a pre-lowercased copy. Lowercasing rewrites an uppercase \U escape
// (8 hex digits to strconv.Unquote) into \u (4 hex digits), so the decoded
// message changes shape — `\U0001F512FAILED` decodes to "🔒FAILED" but its
// lowered form decodes to "\x01f512failed", where the token character '2'
// before "failed" defeats whole-word keyword matching. Since review-14 #3,
// WARN-3 itself carries a level signal (offset stripped → warn → true), so
// this line resolves through the level map without reaching that scan; the
// guard that forces the scan over the decoded msg is
// TestLogLineIsActionable_FallthroughLevelDecodesMsg below.
func TestLogLineIsActionable_UnicodeEscapeMsgNotCorrupted(t *testing.T) {
	line := `time=2026-08-22T10:00:00.000Z level=WARN-3 msg="locked \U0001F512FAILED"`
	if got := logLineIsActionable(line); !got {
		t.Fatalf("logLineIsActionable(%q) = false, want true (offset level WARN-3 must resolve via the level map over original bytes)", line)
	}
}

// TestLogLineIsActionable_FallthroughLevelDecodesMsg pins the decode guard at
// full strength after review-14 #3 gave offset levels their own signal: WARN-3
// now returns true via the level map alone, so THIS case uses a base name with
// no mapped signal (NOTICE) to force the keyword scan over the DECODED msg.
// Under a lowercase-first parse the uppercase \U escape collapses to \u,
// strconv.Unquote reads four hex digits, and the literal remainder fuses onto
// the keyword ("...\u0001f6a8error") — the preceding digit defeats whole-word
// matching and the result flips to false. Only original-byte parsing yields
// "gateway 🚨error", where the emoji is a non-token boundary and ERROR matches.
func TestLogLineIsActionable_FallthroughLevelDecodesMsg(t *testing.T) {
	line := `time=2026-08-22T10:00:00.000Z level=NOTICE msg="gateway \U0001F6A8ERROR"`
	if !logLineIsActionable(line) {
		t.Fatalf("logLineIsActionable(%q) = false, want true (decoded msg must expose 'error' at an emoji boundary)", line)
	}
}

// TestLogDedupKey_MatchesActionabilityParse pins the other half of review-13
// issue 1: logDedupKey already parses the original bytes, so once actionability
// does too, both functions derive their view of the same decoded msg. If the
// two ever diverge again (one parsing lowercased bytes, one original), this
// exact-value assertion fails against whichever side corrupts the escape.
func TestLogDedupKey_MatchesActionabilityParse(t *testing.T) {
	line := `time=2026-08-22T10:00:00.000Z level=WARN-3 msg="locked \U0001F512FAILED"`
	if got, want := logDedupKey(line), "locked 🔒failed"; got != want {
		t.Fatalf("logDedupKey(%q) = %q, want %q (decoded \\U escape, lowercased)", line, got, want)
	}
}

// TestLogLineIsActionable_LevelCaseInsensitiveOnOriginalBytes guards the fix's
// flip side: switching to original-byte parsing must not reintroduce case
// sensitivity in level matching. The old implementation lowercased the entire
// line up front; the replacement must lowercase the extracted key and value
// instead, keeping LEVEL=/ERROR-style records recognized and INFO suppression
// intact regardless of case.
func TestLogLineIsActionable_LevelCaseInsensitiveOnOriginalBytes(t *testing.T) {
	tests := []struct {
		name, line string
		want       bool
	}{
		{"uppercase value recognized", `time=2026-08-22T10:00:00.000Z level=ERROR msg="boom"`, true},
		{"uppercase key and value recognized", `LEVEL=FATAL msg="boom"`, true},
		{"mixed-case info suppresses", `Level=Info msg="all quiet on the western front"`, false},
		{"lowercase info suppresses", `level=info msg="hello world"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logLineIsActionable(tt.line); got != tt.want {
				t.Errorf("logLineIsActionable(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestLogLineIsActionable_NonSlogUnicodeKeepsWholeLineScan pins the non-slog
// path under the same contract: lines that are not slog-shaped keep whole-line
// keyword scanning, including when they carry literal backslash-U sequences
// that only a slog parse would decode.
func TestLogLineIsActionable_NonSlogUnicodeKeepsWholeLineScan(t *testing.T) {
	line := `2026/08/19 07:38:15 WARNING: route conflict \U0001F512 FAILED`
	if !logLineIsActionable(line) {
		t.Fatalf("logLineIsActionable(%q) = false, want true (whole-line keyword scan)", line)
	}
}
