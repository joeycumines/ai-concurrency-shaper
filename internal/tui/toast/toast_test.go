package toast

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"
)

// stripANSI removes ANSI escape sequences for reliable assertions.
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
			} else if i+1 < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestToast_BottomRender(t *testing.T) {
	tt := &Toast{Message: "saved"}
	got := tt.Render(20, 5)
	stripped := stripANSI(got)
	if !strings.Contains(stripped, "saved") {
		t.Error("expected output to contain 'saved'")
	}
	lines := strings.Split(stripped, "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (4 padding + 1 message), got %d", len(lines))
	}
	if lines[0] != "" {
		t.Errorf("expected first line to be empty (padding), got %q", lines[0])
	}
	// The last line should contain "saved" (may have padding from style).
	if !strings.Contains(lines[4], "saved") {
		t.Errorf("expected last line to contain 'saved', got %q", lines[4])
	}
}

func TestToast_WithStyle(t *testing.T) {
	style := lipgloss.NewStyle().Bold(true)
	tt := &Toast{Message: "alert", Style: style}
	got := tt.Render(20, 3)
	stripped := stripANSI(got)
	if !strings.Contains(stripped, "alert") {
		t.Error("expected output to contain 'alert'")
	}
	// Styled text should contain ANSI escape codes in the raw output.
	lastLine := strings.Split(got, "\n")[2]
	if lastLine == "alert" {
		t.Error("expected styled text to contain ANSI escape codes, got plain text")
	}
}

func TestToast_WithWidth(t *testing.T) {
	tt := &Toast{Message: "short", Width: 20}
	got := tt.Render(20, 3)
	stripped := stripANSI(got)
	if !strings.Contains(stripped, "short") {
		t.Error("expected output to contain 'short'")
	}
}

func TestToast_ZeroBounds(t *testing.T) {
	tt := &Toast{Message: "msg"}
	tests := []struct {
		name string
		w, h int
	}{
		{"zero width", 0, 5},
		{"zero height", 5, 0},
		{"negative width", -1, 5},
		{"negative height", 5, -1},
		{"both zero", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tt.Render(tc.w, tc.h)
			if got != "" {
				t.Errorf("expected empty string, got %q", got)
			}
		})
	}
}

func TestToast_EmptyMessage(t *testing.T) {
	tt := &Toast{}
	if got := tt.Render(20, 5); got != "" {
		t.Errorf("expected empty string for empty message, got %q", got)
	}
}

func TestToast_Expired(t *testing.T) {
	tt := &Toast{Message: "old", Duration: 50 * time.Millisecond}
	tt.Show()
	if tt.Expired() {
		t.Fatal("toast should not be expired immediately")
	}
	time.Sleep(100 * time.Millisecond)
	if !tt.Expired() {
		t.Fatal("toast should be expired after duration")
	}
}

func TestToast_NotExpiredWithZeroDuration(t *testing.T) {
	tt := (&Toast{Message: "sticky"}).Show()
	if tt.Expired() {
		t.Fatal("toast with zero duration should never expire")
	}
}

func TestVisibleToasts(t *testing.T) {
	live := (&Toast{Message: "live"}).Show()
	expired := (&Toast{Message: "expired", Duration: 1 * time.Millisecond}).Show()
	time.Sleep(5 * time.Millisecond)

	result := VisibleToasts([]*Toast{live, expired})
	if len(result) != 1 {
		t.Fatalf("expected 1 visible toast, got %d", len(result))
	}
	if result[0].Message != "live" {
		t.Errorf("expected 'live', got %q", result[0].Message)
	}
}

func TestShowSetsCreatedAt(t *testing.T) {
	tt := &Toast{Message: "test"}
	if !tt.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be zero before Show()")
	}
	tt.Show()
	if tt.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set after Show()")
	}
}

func TestToast_NotAnimating_Settled(t *testing.T) {
	// A toast rendered without Show() has a zero CreatedAt and must be treated as
	// fully settled so pre-existing callers are unaffected.
	tt := &Toast{Message: "saved"}
	if tt.AnimatingAt(time.Now()) {
		t.Fatal("toast without Show() should not be animating")
	}
	if got := tt.slideDistance(time.Now()); got != 0 {
		t.Errorf("slideDistance = %d, want 0", got)
	}
}

func TestToast_Animating_SlideIn(t *testing.T) {
	tt := &Toast{Message: "alert", Duration: 5 * time.Second}
	tt.Show()
	if !tt.AnimatingAt(time.Now()) {
		t.Fatal("freshly shown toast should be animating (slide-in)")
	}
}

func TestToast_Animating_SettledAfterSlideIn(t *testing.T) {
	// Two slide-in durations into the lifetime the toast is fully visible.
	tt := &Toast{Message: "alert", Duration: 5 * time.Second}
	tt.CreatedAt = time.Now().Add(-2 * slideIn)
	if tt.AnimatingAt(time.Now()) {
		t.Fatal("toast should be settled after slide-in completes")
	}
	if got := tt.slideDistance(time.Now()); got != 0 {
		t.Errorf("slideDistance = %d, want 0", got)
	}
}

func TestToast_Animating_SlideOut(t *testing.T) {
	tt := &Toast{Message: "alert", Duration: 5 * time.Second}
	tt.CreatedAt = time.Now().Add(-(tt.Duration - slideOut/2))
	if !tt.AnimatingAt(time.Now()) {
		t.Fatal("toast inside the slide-out window should be animating")
	}
}

// TestToast_AnimatingAt_TruncatedSlideOutHead pins the regression where the
// animation ticker missed the slide-out: slideDistance int-truncates the first
// ~1/animWidth of the slide to 0 columns, so an offset-based test would treat the
// toast as settled and the ticker would stop before the visible slide out begins.
// AnimatingAt keys off the window itself, so that truncated head is still animating.
// A single frozen `now` drives both calculations so the precondition cannot be
// disturbed by wall-clock drift between calls.
func TestToast_AnimatingAt_TruncatedSlideOutHead(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, Width: 24}
	tt.CreatedAt = now.Add(-5*time.Second + slideOut).Add(-time.Millisecond)
	if got := tt.slideDistance(now); got != 0 {
		t.Fatalf("precondition: slideDistance must still be 0 in the truncated head, got %d", got)
	}
	if !tt.AnimatingAt(now) {
		t.Fatal("slide-out window should be open (and animated) even while slideDistance still truncates to 0")
	}
}

func TestToast_AnimatingAt_FutureCreated(t *testing.T) {
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: time.Now().Add(time.Hour)}
	if tt.AnimatingAt(time.Now()) {
		t.Fatal("toast created in the future should not be animating")
	}
}

func TestToast_SlideOutStart(t *testing.T) {
	created := time.Now()
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: created}
	want := created.Add(5*time.Second - slideOut)
	if got := tt.SlideOutStart(); !got.Equal(want) {
		t.Errorf("SlideOutStart = %v, want %v", got, want)
	}
}

func TestToast_SlideOutStart_Sticky(t *testing.T) {
	tt := &Toast{Message: "pinned", Duration: 0, CreatedAt: time.Now()}
	if got := tt.SlideOutStart(); !got.IsZero() {
		t.Errorf("sticky toast SlideOutStart = %v, want zero time", got)
	}
}

// TestToast_SlideOutStart_ZeroCreatedAt pins review-14 #6: a toast rendered
// without Show() carries a zero CreatedAt, and every other public time-derived
// method (slideDistance, AnimatingAt) treats that as fully settled/no schedule.
// SlideOutStart must agree instead of returning CreatedAt.Add(exitRel) computed
// off the zero instant.
func TestToast_SlideOutStart_ZeroCreatedAt(t *testing.T) {
	tt := &Toast{Message: "unshown", Duration: 5 * time.Second}
	if got := tt.SlideOutStart(); !got.IsZero() {
		t.Errorf("SlideOutStart without Show() = %v, want zero time", got)
	}
}

// TestToast_SlideOutStart_PastWindow pins the contract that a toast whose window was
// created in the past still reports its slide-out boundary: a slide-out that has
// already begun is handled by AnimatingAt, so callers only schedule for
// SlideOutStart when it is still in the future.
func TestToast_SlideOutStart_PastWindow(t *testing.T) {
	created := time.Now().Add(-(5*time.Second - slideOut/2))
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: created}
	if !tt.AnimatingAt(time.Now()) {
		t.Fatal("toast already inside its slide-out window should be animating")
	}
	if got := tt.SlideOutStart(); got.After(time.Now()) {
		t.Errorf("SlideOutStart = %v, want it to be in the past", got)
	}
}

func TestToast_Render_SlideInPrefixesSpaces(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", CreatedAt: now.Add(-40 * time.Millisecond)}
	got := tt.RenderAt(40, 1, now)
	stripped := stripANSI(got)
	if !strings.HasPrefix(stripped, "  ") {
		t.Errorf("slide-in render should be offset right of the left edge, got %q", stripped)
	}
	if !strings.Contains(stripped, "alert") {
		t.Errorf("slide-in render should still show the message, got %q", stripped)
	}
}

func TestToast_Render_SettledNoOffset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now.Add(-time.Second)}
	got := tt.RenderAt(40, 1, now)
	// A settled toast (age 1s, no slide phase) must render exactly like an
	// unanimated toast with the same message/style.
	baseline := stripANSI((&Toast{Message: "alert"}).RenderAt(40, 1, now))
	if stripANSI(got) != baseline {
		t.Errorf("settled toast should render identically to unanimated toast\n got: %q\nwant: %q", stripANSI(got), baseline)
	}
}

// TestToast_Render_LongMessageStaysSingleLine pins the review-15 #1 rebuttal:
// lipgloss v2 MaxWidth truncates an over-long message instead of wrapping it,
// so continuation lines that would lose RenderAt's leading margin/slide prefix
// can never exist. If a dependency bump ever turns this into wrapping, this
// fails instead of the layout silently breaking.
func TestToast_Render_LongMessageStaysSingleLine(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: strings.Repeat("x", 40)}
	got := stripANSI(tt.RenderAt(40, 5, now))
	lines := strings.Split(got, "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (4 padding + 1 message), got %d: %q", len(lines), got)
	}
	last := lines[4]
	if !strings.HasPrefix(last, "  ") {
		t.Errorf("message line should keep its left margin, got %q", last)
	}
	if w := uniseg.StringWidth(strings.TrimLeft(last, " ")); w > 40-2 {
		t.Errorf("message line content width %d exceeds available width 38: %q", w, last)
	}
	if !strings.Contains(last, "xx") {
		t.Errorf("truncated message content missing from %q", last)
	}
}

// TestExpiredAt_Boundary pins the inclusive-expiry contract: a toast is expired
// exactly at its Duration, matching AnimatingAt's decision that the toast is no
// longer animating at that instant. This prevents the one-frame boundary where
// the toast was neither expired nor settled consistently.
func TestExpiredAt_Boundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	duration := 5 * time.Second
	tt := &Toast{Message: "alert", Duration: duration, CreatedAt: now.Add(-duration)}
	if !tt.ExpiredAt(now) {
		t.Fatal("a toast exactly at its Duration boundary should be expired")
	}
	if tt.AnimatingAt(now) {
		t.Fatal("a toast exactly at its Duration boundary should not be animating")
	}
}

// TestToast_SlideOutStart_ShortDuration pins the clamp: a duration no longer
// than the slide-out window slides out immediately, so SlideOutStart must not
// precede CreatedAt.
func TestToast_SlideOutStart_ShortDuration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, d := range []time.Duration{100 * time.Millisecond, slideOut, slideOut / 2} {
		tt := &Toast{Message: "alert", Duration: d, CreatedAt: now}
		if got := tt.SlideOutStart(); !got.Equal(now) {
			t.Errorf("SlideOutStart for duration %v = %v, want %v", d, got, now)
		}
	}
	// A duration longer than the window keeps the usual offset.
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now}
	if got, want := tt.SlideOutStart(), now.Add(5*time.Second-slideOut); !got.Equal(want) {
		t.Errorf("SlideOutStart for 5s = %v, want %v", got, want)
	}
}

// TestAnimatingAt_DoesNotReportPastExpiry pins that a short-duration toast is
// never reported as still sliding in after it has expired.
func TestAnimatingAt_DoesNotReportPastExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 100 * time.Millisecond, CreatedAt: now.Add(-150 * time.Millisecond)}
	if tt.AnimatingAt(now) {
		t.Fatal("a toast past its expiry must not report as animating, even inside the nominal slide-in window")
	}
	if tt.ExpiredAt(now) != true {
		t.Fatal("a 100ms toast at age 150ms must be expired")
	}
}

// TestSlideDistance_ExpiryInvariant pins review-06 issue 2: slideDistance must agree
// with ExpiredAt and AnimatingAt at the inclusive expiry boundary. At age==Duration
// the toast is expired, not animating, and must render with zero offset rather
// than a full animWidth slide-out or a slide-in tail.
func TestSlideDistance_ExpiryInvariant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Boundary: Duration == slideIn (250ms) at exact expiry
	tt := &Toast{Message: "alert", Duration: 250 * time.Millisecond, CreatedAt: now.Add(-250 * time.Millisecond), Width: 40}
	if !tt.ExpiredAt(now) {
		t.Fatal("should be expired at boundary")
	}
	if tt.AnimatingAt(now) {
		t.Fatal("should not be animating at boundary")
	}
	if got := tt.slideDistance(now); got != 0 {
		t.Fatalf("slideDistance at expiry boundary = %d, want 0", got)
	}
	// Long duration at exact expiry (slideOut branch left==0)
	tt2 := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now.Add(-5 * time.Second), Width: 40}
	if !tt2.ExpiredAt(now) || tt2.AnimatingAt(now) || tt2.slideDistance(now) != 0 {
		t.Fatalf("5s expiry invariant: Expired=%v Animating=%v distance=%d, want true false 0", tt2.ExpiredAt(now), tt2.AnimatingAt(now), tt2.slideDistance(now))
	}
}

// TestSlideDistance_ShortDurationPastExpiry pins that a toast with Duration < slideIn
// does not render a slide-in offset after it has expired. Without the expiry guard
// in slideDistance, age 150ms inside a 250ms slideIn window would still slide.
func TestSlideDistance_ShortDurationPastExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 100 * time.Millisecond, CreatedAt: now.Add(-150 * time.Millisecond), Width: 40}
	if !tt.ExpiredAt(now) {
		t.Fatal("100ms toast at 150ms should be expired")
	}
	if tt.AnimatingAt(now) {
		t.Fatal("should not be animating past expiry")
	}
	if got := tt.slideDistance(now); got != 0 {
		t.Fatalf("slideDistance past short expiry = %d, want 0", got)
	}
}

// TestSlideDistance_LiveStillSlides ensures the expiry guard does not suppress
// legitimate animation while the toast is still live.
func TestSlideDistance_LiveStillSlides(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now.Add(-40 * time.Millisecond), Width: 40}
	if tt.ExpiredAt(now) || !tt.AnimatingAt(now) {
		t.Fatalf("live toast should be !expired && animating, got Expired=%v Animating=%v", tt.ExpiredAt(now), tt.AnimatingAt(now))
	}
	if got := tt.slideDistance(now); got == 0 {
		t.Fatalf("live slide-in toast should have non-zero slideDistance, got 0")
	}
	// Inside slideOut window (5s - 100ms)
	tt2 := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now.Add(-5*time.Second + 100*time.Millisecond), Width: 40}
	if tt2.ExpiredAt(now) || !tt2.AnimatingAt(now) {
		t.Fatalf("toast inside slideOut should be animating, got Expired=%v Animating=%v", tt2.ExpiredAt(now), tt2.AnimatingAt(now))
	}
	if got := tt2.slideDistance(now); got == 0 {
		t.Fatalf("toast inside slideOut should have non-zero slideDistance, got 0")
	}
}

// trajectoryAt renders the slide offset for a frozen age in milliseconds past
// the toast's CreatedAt. All trajectory tests share one frozen wall-clock base
// so no assertion can be disturbed by test-execution delay.
func trajectoryAt(tt *Toast, now time.Time, ageMs int) int {
	return tt.slideDistance(now.Add(time.Duration(ageMs) * time.Millisecond))
}

// animatingAtMs is AnimatingAt at a frozen age in milliseconds.
func animatingAtMs(tt *Toast, now time.Time, ageMs int) bool {
	return tt.AnimatingAt(now.Add(time.Duration(ageMs) * time.Millisecond))
}

// expiredAtMs is ExpiredAt at a frozen age in milliseconds.
func expiredAtMs(tt *Toast, now time.Time, ageMs int) bool {
	return tt.ExpiredAt(now.Add(time.Duration(ageMs) * time.Millisecond))
}

// assertMonotoneRise checks that offsets over the sampled ages are
// non-decreasing and end strictly positive — the shape of a clean slide-out.
func assertMonotoneRise(t *testing.T, tt *Toast, now time.Time, ages []int, label string) {
	t.Helper()
	prev := -1
	for _, ms := range ages {
		d := trajectoryAt(tt, now, ms)
		if d < prev {
			t.Fatalf("%s: offset decreased %d -> %d at age %dms (discontinuous trajectory)", label, prev, d, ms)
		}
		prev = d
	}
	if prev <= 0 {
		t.Fatalf("%s: exit never becomes visible (final offset %d)", label, prev)
	}
}

// TestSlideDistance_ShortDurationTrajectory pins the unified phase model for a
// duration below the slide-out window: the toast rises monotonically from rest
// at CreatedAt and is gone exactly at expiry. The old implementation played only
// the slide-in branch for such toasts, so the offset DECREASED until the toast
// vanished without ever sliding out.
func TestSlideDistance_ShortDurationTrajectory(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 100 * time.Millisecond, CreatedAt: now, Width: 40}

	assertMonotoneRise(t, tt, now, []int{0, 10, 25, 50, 75, 90}, "100ms")

	if got := trajectoryAt(tt, now, 0); got != 0 {
		t.Fatalf("100ms toast must start at rest, offset at 0ms = %d", got)
	}
	if got := trajectoryAt(tt, now, 99); got <= 0 {
		t.Fatalf("100ms toast should still be mid-exit at 99ms, offset = %d", got)
	}
	// Expired instants render settled and agree across the API trio.
	for _, ms := range []int{100, 150} {
		if !expiredAtMs(tt, now, ms) || animatingAtMs(tt, now, ms) || trajectoryAt(tt, now, ms) != 0 {
			t.Fatalf("age %dms: want expired/settled/0, got expired=%v animating=%v distance=%d",
				ms, expiredAtMs(tt, now, ms), animatingAtMs(tt, now, ms), trajectoryAt(tt, now, ms))
		}
	}
	if got, want := tt.SlideOutStart(), now; !got.Equal(want) {
		t.Fatalf("100ms SlideOutStart = %v, want %v (pinned clamp)", got, want)
	}
}

// TestSlideDistance_MidDurationNoJump pins review-09 #2's exact scenario: a
// 400ms toast previously sat in the slide-in branch until age==slideIn (250ms)
// while SlideOutStart claimed the exit began at 150ms; at 250ms the rendered
// offset jumped straight from ~0 columns to ~40% of animWidth. The unified
// model suppresses the colliding entry phase, so the offset stays at rest until
// the true exit start and then rises monotonically.
func TestSlideDistance_MidDurationNoJump(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const dur = 400 * time.Millisecond
	tt := &Toast{Message: "alert", Duration: dur, CreatedAt: now, Width: 40}

	outRel := dur - slideOut // 150ms — where SlideOutStart says the exit begins
	if got, want := tt.SlideOutStart(), now.Add(outRel); !got.Equal(want) {
		t.Fatalf("400ms SlideOutStart = %v, want %v", got, want)
	}
	// At rest immediately before and exactly at the exit boundary…
	if got := trajectoryAt(tt, now, int(outRel/time.Millisecond)-1); got != 0 {
		t.Fatalf("offset just before the exit start = %d, want 0 (no jump allowed)", got)
	}
	if got := trajectoryAt(tt, now, int(outRel/time.Millisecond)); got != 0 {
		t.Fatalf("offset at the exit start = %d, want 0 (exit begins from rest)", got)
	}
	// …then a monotone, visible rise through the whole exit window.
	assertMonotoneRise(t, tt, now,
		[]int{int(outRel / time.Millisecond), 160, 200, 250, 300, 350, 399}, "400ms")

	if !expiredAtMs(tt, now, 400) || animatingAtMs(tt, now, 400) || trajectoryAt(tt, now, 400) != 0 {
		t.Fatalf("expiry invariant broken at 400ms")
	}
	// Before the exit the suppressed entry renders settled, not animating.
	if animatingAtMs(tt, now, 100) {
		t.Fatal("suppressed-entry period must not report as animating (no ticker needed)")
	}
	if animatingAtMs(tt, now, 200) != true || expiredAtMs(tt, now, 200) {
		t.Fatal("mid-exit must be animating and unexpired")
	}
}

// TestSlideDistance_D300msTrajectory pins another inside-the-overlap duration:
// exit starts at Duration-slideOut = 50ms with no entry phase.
func TestSlideDistance_D300msTrajectory(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 300 * time.Millisecond, CreatedAt: now, Width: 40}

	if got, want := tt.SlideOutStart(), now.Add(50*time.Millisecond); !got.Equal(want) {
		t.Fatalf("300ms SlideOutStart = %v, want %v (+50ms)", got, want)
	}
	if got := trajectoryAt(tt, now, 49); got != 0 {
		t.Fatalf("offset at 49ms = %d, want 0", got)
	}
	if got := trajectoryAt(tt, now, 50); got != 0 {
		t.Fatalf("offset at 50ms = %d, want 0 (exit starts from rest)", got)
	}
	assertMonotoneRise(t, tt, now, []int{50, 60, 100, 150, 200, 250, 299}, "300ms")
	if animatingAtMs(tt, now, 40) {
		t.Fatal("pre-exit period of an entry-suppressed toast must not animate")
	}
}

// TestSlideDistance_D250msEqualsSlideOut pins the D==slideOut boundary: the
// entire lifetime is one exit starting at rest from CreatedAt.
func TestSlideDistance_D250msEqualsSlideOut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: slideOut, CreatedAt: now, Width: 40}

	if got, want := tt.SlideOutStart(), now; !got.Equal(want) {
		t.Fatalf("250ms SlideOutStart = %v, want %v (CreatedAt)", got, want)
	}
	if got := trajectoryAt(tt, now, 0); got != 0 {
		t.Fatalf("offset at birth = %d, want 0", got)
	}
	assertMonotoneRise(t, tt, now, []int{0, 25, 50, 100, 150, 200, 249}, "250ms")
}

// TestSlideDistance_D500msBoundary pins the seam where the full entry phase
// just barely fits before the full exit: entry plays [0,slideIn) settling to 0,
// then exit rises [slideIn, 500ms). No discontinuity at the handoff.
func TestSlideDistance_D500msBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 500 * time.Millisecond, CreatedAt: now, Width: 40}

	if got, want := tt.SlideOutStart(), now.Add(slideIn); !got.Equal(want) {
		t.Fatalf("500ms SlideOutStart = %v, want %v (+slideIn)", got, want)
	}
	// Entry visibly slides toward rest.
	entryMid := trajectoryAt(tt, now, 125)
	if entryMid <= 0 {
		t.Fatalf("entry midpoint should be visibly offset, got %d", entryMid)
	}
	if got := trajectoryAt(tt, now, int(slideIn/time.Millisecond)-1); got > 1 {
		t.Fatalf("entry tail should have settled by truncation, got %d at 249ms", got)
	}
	// Exit starts from rest and rises monotonically.
	if got := trajectoryAt(tt, now, int(slideIn/time.Millisecond)); got != 0 {
		t.Fatalf("offset at exit start = %d, want 0", got)
	}
	assertMonotoneRise(t, tt, now, []int{int(slideIn / time.Millisecond), 260, 300, 350, 400, 450, 499}, "500ms")
}

// TestSlideDistance_FiveSecondsLegacyPinned freezes the dominant default case:
// for durations >= slideIn+slideOut the trajectory must remain byte-identical
// to the long-standing formula (entry w->0 over [0,slideIn), rest, exit 0->w
// over the final slideOut).
func TestSlideDistance_FiveSecondsLegacyPinned(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := (&Toast{Message: "x", Width: 80}).animWidth()
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now, Width: 80}

	expect := func(age time.Duration) int {
		switch {
		case age >= 0 && age < slideIn:
			d := 1.0 - float64(age)/float64(slideIn)
			return int(d * float64(w))
		case age >= 5*time.Second:
			return 0
		default:
			left := 5*time.Second - age
			if left >= 0 && left <= slideOut {
				p := 1.0 - float64(left)/float64(slideOut)
				return int(p * float64(w))
			}
			return 0
		}
	}
	for _, ms := range []int{0, 40, 124, 200, 240, 250, 1000, 4750, 4780, 4875, 4960, 4999, 5000} {
		if got, want := tt.slideDistance(now.Add(time.Duration(ms)*time.Millisecond)), expect(time.Duration(ms)*time.Millisecond); got != want {
			t.Fatalf("5s offset at %dms = %d, want legacy %d", ms, got, want)
		}
	}
}

// TestAnimatingAt_WindowGrid sweeps durations x ages and asserts AnimatingAt
// equals the windows actually rendered by slideDistance under the unified
// phase model: entry iff it cannot collide with exit; exit over [outRel,D).
func TestAnimatingAt_WindowGrid(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	durations := []time.Duration{
		100 * time.Millisecond, slideOut, 300 * time.Millisecond, 400 * time.Millisecond,
		450 * time.Millisecond, 500 * time.Millisecond, 750 * time.Millisecond, 5 * time.Second,
	}
	for _, dur := range durations {
		tt := &Toast{Message: "a", Duration: dur, CreatedAt: now, Width: 40}
		var outRel time.Duration
		if dur > slideOut {
			outRel = dur - slideOut
		}
		entryPlays := dur <= 0 || outRel >= slideIn
		for ageMs := 0; ageMs <= 600; ageMs += 10 {
			age := time.Duration(ageMs) * time.Millisecond
			expired := dur > 0 && age >= dur
			want := false
			switch {
			case expired:
				want = false
			case entryPlays && age < slideIn:
				want = true
			case dur > 0 && age >= outRel:
				want = true
			}
			if got := tt.AnimatingAt(now.Add(age)); got != want {
				t.Fatalf("dur=%v age=%dms: AnimatingAt=%v, want %v (entryPlays=%v outRel=%v)", dur, ageMs, got, want, entryPlays, outRel)
			}
		}
	}
}

// TestToast_Render_LeftMargin pins review-08 #3: a settled toast reserves an
// explicit two-cell left margin inside its width budget instead of anchoring
// flush against column zero, so the width headroom toastToastWidth subtracts is
// symmetric rather than a dead zone on the right.
func TestToast_Render_LeftMargin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := &Toast{Message: "alert", Duration: 5 * time.Second, CreatedAt: now.Add(-time.Second)}
	got := stripANSI(tt.RenderAt(40, 1, now))
	if !strings.HasPrefix(got, "  ") {
		t.Errorf("settled toast should carry a two-cell left margin, got %q", got)
	}
	if !strings.Contains(got, "alert") {
		t.Errorf("margin must not consume the message, got %q", got)
	}
	if w := uniseg.StringWidth(got); w > 40 {
		t.Errorf("rendered toast width %d exceeds budget 40", w)
	}

	// Mid slide-in: margin plus slide offset compose left-to-right.
	tt2 := &Toast{Message: "alert", CreatedAt: now.Add(-40 * time.Millisecond)}
	got2 := stripANSI(tt2.RenderAt(40, 1, now))
	if !strings.HasPrefix(got2, "  ") {
		t.Errorf("sliding toast should also keep the margin prefix, got %q", got2)
	}
}
