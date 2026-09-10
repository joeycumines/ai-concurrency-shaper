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
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
)

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

func TestLogWiring_StdlibWarningToasts(t *testing.T) {
	prevFlags := log.Flags()
	prevOut := log.Writer()
	prevDefault := slog.Default()
	defer func() {
		// Restore slog BEFORE the stdlib writer: slog.SetDefault bridges the
		// standard logger through its handler whenever the restored default's
		// handler is not the internal zero handler, so restoring it last would
		// clobber the just-restored output and leak this wiring into later tests.
		slog.SetDefault(prevDefault)
		log.SetFlags(prevFlags)
		log.SetOutput(prevOut)
	}()

	buf := NewLogBuffer(8)
	// Exactly the sequence main.go uses when -tui is set, in order:
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	log.SetFlags(log.LstdFlags)
	log.SetOutput(buf)

	log.Printf("WARNING: route %q specifies group %q with limit %d, but group already has limit %d. Using %d.",
		"POST /v1/messages", "llm", 5, 10, 10)
	log.Printf("auto-detecting LLM endpoints (%d patterns) at concurrency %d", 24, 4)

	lines, _ := buf.ReadNew(0)
	if len(lines) != 2 {
		t.Fatalf("captured %d lines, want 2: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "level=INFO") {
		t.Fatalf("stdlib warning was bridged through slog as a structured record: %q", lines[0])
	}
	if !strings.Contains(lines[0], "WARNING: route") {
		t.Fatalf("stdlib warning lost its text: %q", lines[0])
	}

	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.handleLogLines(lines)
	if len(m.toasts) != 1 {
		t.Fatalf("toasts = %d, want 1 (the WARNING prose toasts; the startup summary does not)", len(m.toasts))
	}
	if !strings.Contains(m.toasts[0].Message, "WARNING: route") {
		t.Fatalf("toast message = %q, want the route warning", m.toasts[0].Message)
	}
}

func TestFollowLogs_TailFollowPinsBottom(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.switchTab(tabLogs)
	if !m.followLogs {
		t.Fatal("switching to Logs should enable followLogs")
	}

	// Write more lines than the viewport can hold.
	for range 40 {
		m.logRing.Write([]byte(strings.Repeat("x", 8) + "\n"))
	}
	m = update(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.cursor != m.maxCursor() {
		t.Errorf("following viewport should pin cursor to newest line: cursor=%d max=%d", m.cursor, m.maxCursor())
	}
	if !m.followLogs {
		t.Fatal("followLogs should remain true while pinned")
	}

	// Explicit scroll pauses following.
	m = update(m, tea.KeyPressMsg{Text: "k"})
	if m.followLogs {
		t.Fatal("manual scroll should pause followLogs")
	}

	// G re-engages following at the bottom.
	m = update(m, tea.KeyPressMsg{Text: "G"})
	if !m.followLogs {
		t.Fatal("G should re-engage followLogs")
	}
	if m.cursor != m.maxCursor() {
		t.Errorf("G should jump to bottom: cursor=%d max=%d", m.cursor, m.maxCursor())
	}
}

func TestFollowLogs_ArrowKeysPauseFollowing(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.switchTab(tabLogs)
	for range 5 {
		m.logRing.Write([]byte("line\n"))
	}
	m = update(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(m, tea.KeyPressMsg{Text: "down"})
	if m.followLogs {
		t.Fatal("arrow key should pause followLogs")
	}
}

func TestHandleLogLines_ToastsActionableAndDedups(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40

	m.handleLogLines([]string{
		"proxy transport error: boom",
		"proxy transport error: boom",
		"some routine line",
		"auto-detecting LLM endpoints",
	})

	if len(m.toasts) != 1 {
		t.Fatalf("toasts = %d, want 1 (actionable + dedup)", len(m.toasts))
	}
	if m.toasts[0].Message != "proxy transport error: boom" {
		t.Errorf("toast message = %q, want %q", m.toasts[0].Message, "proxy transport error: boom")
	}
	// Every line lands in the render-facing ring regardless of toasting.
	if got := len(m.visibleLogLines()); got != 4 {
		t.Errorf("visibleLogLines = %d, want 4", got)
	}
}

func TestToastAnimCmd_WhileAnimating(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.AddToast(&toast.Toast{Message: "alert", Duration: 5 * time.Second})
	if m.toastAnimCmd() == nil {
		t.Fatal("toastAnimCmd should be non-nil while a toast is live")
	}

	// A toast in its settled middle phase is not animating, but the ticker must
	// stay armed so the slide-out window is actually rendered (the 250ms metric
	// redraw cadence can otherwise skip it entirely). It arms a one-shot at
	// SlideOutStart rather than idle 30ms ticks.
	m2 := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m2.AddToast(&toast.Toast{Message: "settled", Duration: 25 * time.Second})
	prev := m2.toasts[0]
	prev.CreatedAt = time.Now().Add(-5 * time.Second)
	m2.toasts[0] = prev
	if m2.toastAnimCmd() == nil {
		t.Fatal("toastAnimCmd stays armed while a live fixed toast has a slide-out")
	}

	// A settled sticky toast (Duration 0, past its slide-in) has no animation
	// left and no slide-out ahead: it must not reap a ticker, which would
	// re-render identical content at toastAnimInterval for its whole lifetime. It is
	// refreshed on the metrics redraw cadence instead. An already-expired toast in
	// the slice is skipped the same way.
	m4 := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m4.toasts = []*toast.Toast{
		{Message: "pinned", Duration: 0, CreatedAt: time.Now().Add(-2 * time.Second)},
		{Message: "gone", Duration: 1 * time.Second, CreatedAt: time.Now().Add(-2 * time.Second)},
	}
	if m4.toastAnimCmd() != nil {
		t.Fatal("toastAnimCmd should be nil for a settled sticky toast - no animation remains")
	}

	// An expired toast must not keep the ticker armed.
	m3 := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m3.AddToast(&toast.Toast{Message: "done", Duration: 1 * time.Second})
	m3.toasts[0].CreatedAt = time.Now().Add(-2 * time.Second)
	m3.toasts = toast.VisibleToasts(m3.toasts)
	if m3.toastAnimCmd() != nil {
		t.Fatal("toastAnimCmd should be nil once no toast is live")
	}
}

// TestToastAnimSingleOwner_SettledDoesNotStack pins the single-owner animation
// ticker: while a settled fixed-duration toast has only a future slide-out
// pending, a burst of unrelated updates arms the one-shot slide-out tick
// exactly once — later updates neither stack a second tick nor advance the
// armed deadline.

func TestToastAnimSingleOwner_SettledDoesNotStack(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "settled", Duration: 25 * time.Second})
	m.toasts[0].CreatedAt = time.Now().Add(-5 * time.Second)

	next, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	cur := next.(Model)
	if cmd == nil || !cur.animTickDeadline.After(time.Now()) {
		t.Fatalf("first update must arm the future slide-out one-shot: cmd=%v deadline=%v", cmd, cur.animTickDeadline)
	}
	deadline := cur.animTickDeadline

	for i := 1; i < 50; i++ {
		next, cmd := cur.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		cur = next.(Model)
		if cmd != nil {
			t.Fatalf("unrelated update %d stacked a second tick despite armed deadline %v", i, deadline)
		}
		if !cur.animTickDeadline.Equal(deadline) {
			t.Fatalf("unrelated update %d moved the armed deadline: %v -> %v", i, deadline, cur.animTickDeadline)
		}
	}
}

// TestToastAnimSingleOwner_AnimatingArmsOnce pins that a fast burst of updates
// during a toast's slide-in arms a single 30ms animation ticker instead of one
// per update. At most one natural re-arm is tolerated in case the armed
// interval elapses mid-burst on a very slow machine.

func TestToastAnimSingleOwner_AnimatingArmsOnce(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "alert", Duration: 5 * time.Second})
	m.toasts[0].CreatedAt = time.Now().Add(-10 * time.Millisecond)

	cmds := 0
	cur := m
	for range 50 {
		next, cmd := cur.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		cur = next.(Model)
		if cmd != nil {
			cmds++
			if got := time.Until(cur.animTickDeadline); got <= 0 || got > toastAnimInterval {
				t.Fatalf("armed deadline %v is not within one animation interval (%v)", cur.animTickDeadline, toastAnimInterval)
			}
		}
	}
	if cmds == 0 || cmds > 2 {
		t.Fatalf("burst of 50 updates while animating armed %d ticks, want 1 (at most 2 with one natural re-arm)", cmds)
	}

	// Simulate the armed tick firing mid-burst: once the deadline has passed,
	// the next update must re-arm exactly one fresh animation tick.
	cur.animTickDeadline = time.Now().Add(-time.Millisecond)
	next, cmd := cur.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	cur = next.(Model)
	if cmd == nil {
		t.Fatal("after the armed tick fires, the next update must re-arm the animation ticker")
	}
	if got := time.Until(cur.animTickDeadline); got <= 0 || got > toastAnimInterval {
		t.Fatalf("re-armed deadline %v is not within one animation interval (%v)", cur.animTickDeadline, toastAnimInterval)
	}
}

// TestAddToast_ClearsArmedAnimTick pins that adding a toast clears any tick
// already armed for an older toast's future slide-out, so the new toast's
// slide-in re-arms promptly instead of waiting out the stale deadline.

func TestAddToast_ClearsArmedAnimTick(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "old", Duration: 25 * time.Second})
	m.toasts[0].CreatedAt = time.Now().Add(-5 * time.Second)

	next, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	if cmd == nil || m.animTickDeadline.IsZero() {
		t.Fatal("precondition: an update must arm the old toast's slide-out tick")
	}

	m.AddToast(&toast.Toast{Message: "new", Duration: 5 * time.Second})
	if !m.animTickDeadline.IsZero() {
		t.Fatalf("AddToast must clear the armed deadline so the new toast's slide-in starts promptly, got %v", m.animTickDeadline)
	}
}

// TestHandleLogLines_DedupAcrossTimestamps exercises the toast path end to end:
// two identical slog errors separated by timestamps produce one toast, a
// distinct warning produces another.

func TestHandleLogLines_DedupAcrossTimestamps(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.handleLogLines([]string{
		`time=2026-08-19T07:38:15.123Z level=ERROR msg="proxy transport error: boom"`,
		`time=2026-08-19T07:38:42.987Z level=ERROR msg="proxy transport error: boom"`,
		`time=2026-08-19T07:38:43.001Z level=WARN msg="queue timeout"`,
	})
	if len(m.toasts) != 2 {
		t.Fatalf("toasts = %d, want 2 (recurring boom dedups, warning toasts)", len(m.toasts))
	}
}

// TestHandleLogLines_WarningConfigLineToasts proves the route-group config
// warning (a stdlib log.Printf at startup, captured into the Logs tab) is
// actionable prose under the widened keyword set.

func TestHandleLogLines_WarningConfigLineToasts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	line := `2026/08/19 07:38:15 WARNING: route "POST /v1/messages" specifies group "llm" with limit 5, but group already has limit 10. Using 10.`
	m.handleLogLines([]string{line})
	if len(m.toasts) != 1 {
		t.Fatalf("toasts = %d, want 1 for a config WARNING line", len(m.toasts))
	}
	if m.toasts[0].Message != strings.TrimSpace(line) {
		t.Errorf("toast message = %q, want %q", m.toasts[0].Message, line)
	}
}

func TestQuitFlushesPendingLogFragment(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.logBuf = NewLogBuffer(8)

	m.logBuf.Write([]byte("shutting dow"))
	m = update(m, tea.KeyPressMsg{Text: "q"})
	got := ringTexts(m.logRing)
	if len(got) == 0 || got[len(got)-1] != "shutting dow" {
		t.Fatalf("last ring line = %q, want the torn fragment delivered on quit", got)
	}

	// Models without buffer wiring quit cleanly as before.
	if _, cmd := NewModelForProviders([]ProviderMeta{{Concurrency: 4}}).handleKey(tea.KeyPressMsg{Text: "q"}); cmd == nil {
		t.Fatal("quit key did not return the quit command on an unwired model")
	}
}

// TestLogDrain_TickThenQuitDeliversExactlyOnce models the message flow that
// review-05 flagged: a poll tick delivers some lines through Update, more lines
// are written afterwards (including a torn tail), and quit then drains the
// rest. Because the read cursor lives on the model and every delivery happens
// inside Update, the previously-polled batch can neither be lost to an
// in-flight send nor delivered twice.

func TestLogDrain_TickThenQuitDeliversExactlyOnce(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.logBuf = NewLogBuffer(8)

	m.logBuf.Write([]byte("one\ntwo\n"))
	m.drainLogs() // what Update does for a logPollTickMsg
	if got := ringTexts(m.logRing); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("after tick ring = %v, want [one two]", got)
	}

	m.logBuf.Write([]byte("three\ntorn"))
	m = update(m, tea.KeyPressMsg{Text: "q"})
	want := []string{"one", "two", "three", "torn"}
	if got := ringTexts(m.logRing); len(got) != len(want) {
		t.Fatalf("ring = %v, want %v (exactly once each)", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ring[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}

	// A late tick queued behind the quit redelivers nothing.
	m.drainLogs()
	if got := ringTexts(m.logRing); len(got) != len(want) {
		t.Fatalf("late tick duplicated delivery: %v", got)
	}
}

// TestLogDrain_QuitDeliversNeverPolledLines pins that quitting delivers lines
// no tick ever extracted — complete lines as well as the torn fragment.

func TestLogDrain_QuitDeliversNeverPolledLines(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.logBuf = NewLogBuffer(8)

	m.logBuf.Write([]byte("complete\n"))
	m.logBuf.Write([]byte("torn"))
	m = update(m, tea.KeyPressMsg{Text: "q"})
	got := ringTexts(m.logRing)
	if len(got) != 2 || got[0] != "complete" || got[1] != "torn" {
		t.Fatalf("ring = %v, want [complete torn]", got)
	}
}

func TestToastSeenOldestFirstEviction(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	const total = toastSeenMax + 2
	keys := make([]string, total)
	for i := range total {
		line := fmt.Sprintf(`level=ERROR msg="boom %03d"`, i)
		key := logDedupKey(line)
		if key == "" {
			t.Fatalf("dedup key empty for %q", line)
		}
		keys[i] = key
		m.handleLogLines([]string{line})
	}

	// Deterministic trace: inserts fill the set to exactly toastSeenMax; the
	// next insert fires one eviction of the toastSeenEvict oldest keys; the
	// remaining inserts grow it again. The set stays bounded by toastSeenMax.
	if len(m.toastSeen) != total-toastSeenEvict {
		t.Fatalf("|toastSeen| = %d, want %d after the threshold crossing", len(m.toastSeen), total-toastSeenEvict)
	}
	for i := range toastSeenEvict { // keys[0..toastSeenEvict-1] evicted oldest-first
		if _, ok := m.toastSeen[keys[i]]; ok {
			t.Fatalf("key %q should have been evicted", keys[i])
		}
	}
	survivors := keys[toastSeenEvict:]
	if len(m.toastSeenOrder) != len(survivors) {
		t.Fatalf("|toastSeenOrder| = %d, want %d", len(m.toastSeenOrder), len(survivors))
	}
	for j, key := range m.toastSeenOrder {
		if want := survivors[j]; key != want {
			t.Fatalf("toastSeenOrder[%d] = %q, want survivor %q in insertion order", j, key, want)
		}
	}

	// Replaying an evicted error re-toasts exactly once: the new toast joins as
	// the newest survivor of the bounded live-toast slice (review-07 #3 cap).
	// Replaying a retained one stays deduplicated and leaves the slice alone.
	evicted := fmt.Sprintf(`level=ERROR msg="boom %03d"`, 0)
	retained := fmt.Sprintf(`level=ERROR msg="boom %03d"`, total-1)

	m.handleLogLines([]string{evicted})
	if len(m.toasts) != toastLiveMax {
		t.Fatalf("live toasts = %d, want the hard cap %d", len(m.toasts), toastLiveMax)
	}
	if got, want := m.toasts[len(m.toasts)-1].Message, evicted; got != want {
		t.Fatalf("evicted key did not re-toast: newest toast %q, want %q", got, want)
	}

	snapshot := append([]*toast.Toast(nil), m.toasts...)
	m.handleLogLines([]string{retained})
	if len(m.toasts) != toastLiveMax {
		t.Fatalf("live toasts = %d, want unchanged cap %d", len(m.toasts), toastLiveMax)
	}
	for i := range snapshot {
		if m.toasts[i] != snapshot[i] {
			t.Fatalf("retained key lost dedup protection: toast slice changed at %d", i)
		}
	}
}

// TestLogBuffer_RedirectToStreamsThrough pins review-10 #4 / review-11 #2:
// RedirectTo flips the buffer into live passthrough — the torn fragment held at
// flip time is flushed to the target first, subsequent Writes bypass the ring
// entirely, and nothing new lands in the polled buffer.

func TestHandleLogLines_EmptyMsgStillToasts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	line := `time=2026-08-22T10:00:00.000Z level=ERROR msg=""`
	m.handleLogLines([]string{line})
	if len(m.toasts) != 1 {
		t.Fatalf("toasts = %d, want 1 for an actionable empty-msg line (dropped?)", len(m.toasts))
	}
	// Un-keyable lines cannot dedup: a repeat raises another toast.
	m.handleLogLines([]string{line})
	if len(m.toasts) != 2 {
		t.Fatalf("toasts after repeat = %d, want 2 (empty-key lines cannot dedup)", len(m.toasts))
	}
	// Both occurrences still reach the render ring.
	if got := len(m.visibleLogLines()); got != 2 {
		t.Fatalf("visibleLogLines = %d, want 2", got)
	}
}

// TestHandleLogLines_DistinctAttributesToastSeparately exercises the T20 fix end
// to end through the toast path.

func TestHandleLogLines_DistinctAttributesToastSeparately(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.handleLogLines([]string{
		`time=2026-08-22T10:00:00.000Z level=ERROR msg="request failed" route=a`,
		`time=2026-08-22T10:00:01.000Z level=ERROR msg="request failed" route=b`,
	})
	if len(m.toasts) != 2 {
		t.Fatalf("toasts = %d, want 2 (different routes are different incidents)", len(m.toasts))
	}
}

// TestHandleLogLines_StripsANSI pins review-07 #8 hardening: escape sequences in
// captured lines are stripped before they reach the ring or a toast message, so
// logged text can never inject terminal control output.

func TestHandleLogLines_StripsANSI(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	line := "\x1b[31mproxy transport error\x1b[0m: boom"
	m.handleLogLines([]string{line})
	lines := m.visibleLogLines()
	if len(lines) != 1 || strings.ContainsRune(lines[0], '\x1b') {
		t.Fatalf("ring line contains escape bytes: %q", lines)
	}
	if got, want := lines[0], "proxy transport error: boom"; got != want {
		t.Fatalf("stripped ring line = %q, want %q", got, want)
	}
	if len(m.toasts) != 1 {
		t.Fatalf("toasts = %d, want 1", len(m.toasts))
	}
	if strings.ContainsRune(m.toasts[0].Message, '\x1b') {
		t.Fatalf("toast message contains escape bytes: %q", m.toasts[0].Message)
	}
}

// TestToastLiveCap_UnderSustainedUniqueErrors pins review-07 #3: a burst of
// distinct actionable errors cannot grow the live-toast slice without bound;
// only the newest toastLiveMax survive and the Logs tab keeps everything.

func TestToastLiveCap_UnderSustainedUniqueErrors(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	for i := range 450 {
		m.handleLogLines([]string{fmt.Sprintf(`time=2026-08-22T10:00:%02d.%03dZ level=ERROR msg="unique failure %03d"`, i/60%60, i%1000, i)})
	}
	if len(m.toasts) > toastLiveMax {
		t.Fatalf("live toasts = %d, want <= %d under sustained unique errors", len(m.toasts), toastLiveMax)
	}
	// Newest alert survived the cap; the oldest did not.
	newest := fmt.Sprintf(`time=2026-08-22T10:00:%02d.%03dZ level=ERROR msg="unique failure %03d"`, 449/60%60, 449%1000, 449)
	found := false
	for _, tt := range m.toasts {
		if tt.Message == newest {
			found = true
		}
	}
	if !found {
		t.Fatalf("newest toast missing from survivors: %+v", m.toasts)
	}
	// The complete stream remains available in the Logs tab.
	if got := len(m.visibleLogLines()); got != 450 {
		t.Fatalf("visibleLogLines = %d, want 450 (Logs tab keeps every line)", got)
	}
}

// TestAnimTick_StaleGenerationIgnored pins review-07 #2: a tick armed before an
// AddToast carries a dead generation, so its late arrival neither re-renders via
// re-arm nor stacks a ticker. Deterministic and fast: the first toast is placed
// mid slide-in so the armed tick horizon is one 30ms animation interval, and
// the stale arrival's deadline is forced into the past instead of racing wall
// clocks.

func TestAnimTick_StaleGenerationIgnored(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.AddToast(&toast.Toast{Message: "alert", Duration: 5 * time.Second})
	m.toasts[0].CreatedAt = time.Now().Add(-10 * time.Millisecond)

	next, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("precondition: update must arm the animation tick")
	}
	staleMsg := cmd() // fires after one animation interval, carrying the old generation

	// AddToast invalidates outstanding ticks...
	genBefore := m.animTickGen
	m.AddToast(&toast.Toast{Message: "fresh", Duration: 5 * time.Second})
	if m.animTickGen == genBefore {
		t.Fatal("AddToast must bump the animation generation")
	}
	if staleMsg.(animTickMsg).generation == m.animTickGen {
		t.Fatal("the pre-AddToast command must carry the old generation")
	}

	// A stale arrival while the deadline has already passed must NOT re-arm.
	m.animTickDeadline = time.Now().Add(-time.Millisecond)
	next, cmdAfterStale := m.Update(staleMsg)
	m = next.(Model)
	if cmdAfterStale != nil {
		t.Fatal("stale-generation animTickMsg must be ignored entirely")
	}

	// A current-generation arrival still re-arms the schedule.
	fresh := animTickMsg{generation: m.animTickGen}
	m.animTickDeadline = time.Now().Add(-time.Millisecond)
	next, cmdFresh := m.Update(fresh)
	if cmdFresh == nil {
		t.Fatal("current-generation animTickMsg must re-arm the animation tick")
	}
	if got := time.Until(next.(Model).animTickDeadline); got <= 0 || got > toastAnimInterval {
		t.Fatalf("re-armed deadline %v is not within one animation interval (%v)", next.(Model).animTickDeadline, toastAnimInterval)
	}
}
