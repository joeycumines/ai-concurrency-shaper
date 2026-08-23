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

// Package toast provides short-lived notification messages for the TUI dashboard.
package toast

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Animation timings. A toast slides in over the first slideIn of its lifetime,
// settles, then slides out over the final slideOut. When the lifetime is too
// short for both phases to fit without overlapping (Duration < slideIn+slideOut)
// the entry phase is suppressed entirely. What remains depends on how short the
// lifetime is: at Duration <= slideOut the exit begins at rest at birth and runs
// continuously to expiry; for slideOut < Duration < slideIn+slideOut the toast
// sits settled until exitRel, then exits — see entryPlays/exitRel. These are
// pure functions of CreatedAt/Duration so the same values drive rendering, the
// animation ticker, and SlideOutStart.
const (
	slideIn  = 250 * time.Millisecond
	slideOut = 250 * time.Millisecond
)

// Toast represents a single notification to display at the bottom of a pane.
type Toast struct {
	Message   string
	Style     lipgloss.Style
	Width     int
	CreatedAt time.Time
	Duration  time.Duration // 0 = until explicitly dismissed
}

// Show initialises the CreatedAt timestamp and returns the toast.
func (t *Toast) Show() *Toast {
	t.CreatedAt = time.Now()
	return t
}

// ExpiredAt reports whether the toast has exceeded its display duration at the
// given time. A zero duration (sticky) toast never expires. The boundary is
// inclusive — a toast is expired exactly at Duration — so expiry, AnimatingAt and
// slideDistance cannot disagree about the same instant.
func (t *Toast) ExpiredAt(now time.Time) bool {
	if t.Duration <= 0 {
		return false
	}
	return now.Sub(t.CreatedAt) >= t.Duration
}

// Expired reports whether the toast has exceeded its display duration at the
// current time.
func (t *Toast) Expired() bool {
	return t.ExpiredAt(time.Now())
}

// exitRel returns when the slide-out begins, relative to CreatedAt:
// Duration-slideOut clamped at zero, so a duration no longer than the exit
// window starts exiting immediately rather than before it exists.
func (t *Toast) exitRel() time.Duration {
	if t.Duration <= slideOut {
		return 0
	}
	return t.Duration - slideOut
}

// entryPlays reports whether the slide-in phase is rendered at all. The entry
// is suppressed whenever the exit would begin before the entry finished
// (Duration < slideIn+slideOut): sliding in toward rest and then instantly
// jumping onto the exit ramp would render as a visible discontinuity, and a
// suppressed entry means the toast sits settled until exitRel before exiting —
// or, when Duration <= slideOut puts exitRel at birth, exits continuously from
// rest. Sticky toasts (Duration <= 0) always play the entry.
func (t *Toast) entryPlays() bool {
	return t.Duration <= 0 || t.Duration-slideOut >= slideIn
}

// slideDistance returns how many columns the toast is currently shifted right
// of its settled position for the given time. A positive offset translates the
// toast to the right while its rendered width is capped to the same bounding
// box, so slide-in starts shifted fully right (rendered as leading spaces) and
// eases leftward into place, and slide-out continues that rightward travel out
// of view before expiry.
//
// Trajectory by phase, all derived from the same exitRel/entryPlays math that
// drives AnimatingAt and SlideOutStart:
//   - expired instants render settled (offset 0), matching ExpiredAt;
//   - entry phase [0, slideIn) eases from full offset to rest — only when
//     entryPlays, i.e. the exit cannot begin inside the entry window;
//   - exit phase [exitRel, Duration) rises continuously from rest (offset 0 at
//     its exact start) to full offset scaled over min(Duration, slideOut), so
//     there is never a jump anywhere on the trajectory, including at the
//     handoff where a full-length entry meets the exit;
//   - otherwise the toast is settled at offset 0.
//
// A toast rendered without Show() has a zero CreatedAt and is treated as fully
// settled (offset 0), so callers that render long-lived toasts without animation
// are unaffected.
func (t *Toast) slideDistance(now time.Time) int {
	if t.CreatedAt.IsZero() {
		return 0
	}
	age := now.Sub(t.CreatedAt)
	// Expired toasts never slide — keep this in lockstep with ExpiredAt/AnimatingAt
	// so the public Render APIs and the stated inclusive-boundary invariant cannot
	// disagree at the exact Duration instant or for short durations past expiry.
	if t.Duration > 0 && age >= t.Duration {
		return 0
	}
	switch {
	case t.entryPlays() && age >= 0 && age < slideIn:
		p := float64(age) / float64(slideIn)
		// Start fully offset and settle to rest.
		d := 1.0 - p
		if d < 0 {
			d = 0
		}
		return int(d * float64(t.animWidth()))
	case t.Duration > 0 && age >= t.exitRel():
		outLen := min(t.Duration, slideOut)
		p := float64(age-t.exitRel()) / float64(outLen)
		if p < 0 {
			p = 0
		}
		if p > 1 {
			p = 1
		}
		return int(p * float64(t.animWidth()))
	}
	return 0
}

// AnimatingAt reports whether the toast is inside a rendered animation window at
// the given time, i.e. whether the renderer should keep an animation ticker running
// so the toast visibly animates. The ticker keys off the windows themselves rather than
// the rendered pixel offset: slideDistance int-truncates the first ~1/animWidth of
// each slide to 0 columns, so an offset-based test would silently drop that tail of
// the motion and skip the slide.
//
// The windows are exactly those slideDistance animates: the entry phase when it
// plays (a suppressed entry renders settled — no ticker needed), and the exit
// phase from exitRel until the inclusive expiry boundary. A toast is never
// reported as animating past its expiry, keeping this consistent with ExpiredAt.
func (t *Toast) AnimatingAt(now time.Time) bool {
	if t.CreatedAt.IsZero() {
		return false
	}
	age := now.Sub(t.CreatedAt)
	if age < 0 {
		return false
	}
	if t.Duration > 0 && age >= t.Duration {
		return false
	}
	if t.entryPlays() && age < slideIn {
		return true
	}
	return t.Duration > 0 && age >= t.exitRel()
}

// SlideOutStart returns the time at which the toast will begin its slide-out, so the
// dashboard can schedule a single animation tick at that moment instead of ticking at
// animation cadence for the whole settled middle of the toast's lifetime. Sticky
// toasts (Duration <= 0) never slide out and return a zero time. A toast with a
// zero CreatedAt (rendered without Show()) is treated like slideDistance and
// AnimatingAt treat it — fully settled — and also reports a zero time rather
// than a schedule computed off the zero instant. The value is CreatedAt plus
// exitRel — Duration-slideOut clamped at zero — so it always names the instant
// slideDistance actually starts rising: for durations no longer than the exit
// window that is CreatedAt itself.
func (t *Toast) SlideOutStart() time.Time {
	if t.Duration <= 0 || t.CreatedAt.IsZero() {
		return time.Time{}
	}
	return t.CreatedAt.Add(t.exitRel())
}

// animWidth bounds the slide travel so the toast never shifts so far that only
// an empty sliver remains. It is a small fraction of the toast width, capped.
func (t *Toast) animWidth() int {
	w := t.Width
	if w <= 0 {
		w = 24
	}
	d := w / 4
	if d < 4 {
		return 4
	}
	if d > 16 {
		return 16
	}
	return d
}

// defaultStyle returns the default toast style (blue background, white text).
func defaultStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#E6EDF3")).
		Background(lipgloss.Color("#1F6FEB")).
		PaddingLeft(1).
		PaddingRight(1).
		Bold(true)
}

// hasStyle reports whether the toast has a non-default style set.
// It checks by seeing if the style renders differently than the zero style.
//
// The probe renders the empty string, so it only sees properties that affect
// empty output (colors, padding, bold). Properties that alter rendering only
// for non-empty content — MaxWidth among them — are indistinguishable from an
// unset style here. That is safe by construction: RenderAt applies its own
// MaxWidth cap to whichever style wins, so a missed MaxWidth-only style loses
// nothing, and any style lacking visual attributes falls back to the default
// look exactly as intended.
func hasStyle(s lipgloss.Style) bool {
	// A rendered zero-value style still produces ANSI reset codes.
	// We detect "no custom style" by comparing the rendered empty string.
	return s.Render("") != lipgloss.NewStyle().Render("")
}

// Render produces a toast string positioned at the bottom of the given
// bounds (width, height), using the current time for any slide animation.
// Returns empty string for zero/negative bounds or empty message.
func (t *Toast) Render(width, height int) string {
	return t.RenderAt(width, height, time.Now())
}

// marginCells is the fixed left margin every toast keeps inside its width
// budget, so a settled toast does not anchor flush against column zero while
// callers reserve symmetric headroom on the right (toastToastWidth subtracts
// four cells).
const marginCells = 2

// RenderAt renders the toast at the bottom of the given bounds using now for
// the slide offset, so callers and tests can render deterministically at a
// fixed instant. Returns empty string for zero/negative bounds or empty message.
func (t *Toast) RenderAt(width, height int, now time.Time) string {
	if width <= 0 || height <= 0 || t.Message == "" {
		return ""
	}

	w := width
	if t.Width > 0 && t.Width < w {
		w = t.Width
	}

	style := t.Style
	if !hasStyle(style) {
		style = defaultStyle()
	}

	// Reserve the left margin inside the width budget; degenerate budgets too
	// narrow for margin plus content drop the margin instead of the message.
	margin := marginCells
	avail := w - margin
	if avail < 2 {
		margin = 0
		avail = w
	}

	// Apply the slide offset: shift the toast right by `off` columns (beyond the
	// margin) and cap the styled message to the remaining width so the composed
	// line stays within the pane and never wraps. MaxWidth caps the width;
	// shorter messages render narrower, which is what lets the toast sit at its
	// shifted offset without overflow.
	off := t.slideDistance(now)
	if off >= avail {
		off = avail - 1
	}
	rendered := style.MaxWidth(max(avail-off, 1)).Render(t.Message)
	if prefix := margin + off; prefix > 0 {
		rendered = strings.Repeat(" ", prefix) + rendered
	}

	// Position at the bottom of available height.
	if height > 1 {
		return strings.Repeat("\n", height-1) + rendered
	}
	return rendered
}

// VisibleToasts filters the input slice, removing expired entries and
// returning only toasts that should still be displayed.
func VisibleToasts(toasts []*Toast) []*Toast {
	live := make([]*Toast, 0, len(toasts))
	for _, t := range toasts {
		if !t.Expired() {
			live = append(live, t)
		}
	}
	return live
}
