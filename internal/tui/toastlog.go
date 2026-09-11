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
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
)

const (
	defaultToastDuration = 5 * time.Second
	defaultToastWidth    = 80
)

type resyncTickMsg struct{}
type resyncDrawMsg struct{}

// logPollTickMsg wakes the update loop so it can drain freshly captured log
// lines from the LogBuffer itself (see Model.drainLogs). It carries no data:
// lines are read inside Update, keeping delivery single-threaded with respect
// to quit handling.
type logPollTickMsg struct{}

// animTickMsg fires on a short interval while any toast is animating so the
// slide-in/out is visible between the coarser metrics redraw ticks.
//
// generation identifies the arming schedule: AddToast bumps the model's counter
// so ticks armed for a superseded schedule arrive stale and are ignored (see
// Update), keeping exactly one live timer generation.
type animTickMsg struct{ generation uint64 }

// toastAnimInterval is the animation tick cadence.
const toastAnimInterval = 30 * time.Millisecond

const (
	// toastSeenMax bounds the log-toast dedup set. Reaching it evicts the
	// oldest toastSeenEvict keys before the next key is inserted, so the set
	// never exceeds toastSeenMax entries.
	toastSeenMax = 400

	// toastSeenEvict is how many of the oldest dedup keys each eviction drops.
	toastSeenEvict = 200

	// toastLiveMax is the hard bound on live toasts. The Logs buffer bounds
	// retained lines, not ingest rate, so a storm of distinct actionable errors
	// would otherwise accumulate thousands of five-second Toast objects. Beyond
	// the cap the oldest alerts are dropped; every line still reaches the Logs
	// tab. The cap sits comfortably above the three-toast render limit.
	toastLiveMax = 8
)

// toastAnimCmd returns a tick command for the next moment an animation frame is
// actually needed, nil when none is.
//
// While any toast is mid slide-in/out the ticker runs at toastAnimInterval so the
// motion is smooth. When no toast is currently animating, it instead arms a single
// one-shot tick at the earliest toast's SlideOutStart, so a fixed-duration toast
// still begins its exit animation promptly — an idle 30ms tick in the settled
// middle (which can last minutes) would re-render identical content hundreds of
// times for nothing. A settled sticky toast (Duration 0) never animates again and
// arms no tick; it is refreshed on the coarser metrics redraw cadence. Expired
// toasts are pruned by Update regardless of any tick, so expiry never relies on
// this command.
//
// The timer is single-owner. Update calls this at the end of every message, but
// once a tick is armed for a future animTickDeadline a later unrelated update
// must not stack a second one; otherwise a settled toast would spawn a fresh
// sleep-per-event and the resulting burst at SlideOutStart would multiply 30ms
// animation loops. Only a tick that has actually fired (deadline now in the
// past) re-arms the next one, keeping exactly one timer generation alive. Each
// armed tick captures the current animTickGen, so a tick orphaned by AddToast
// (which bumps the generation) is recognized and discarded on arrival instead
// of stacking a redundant ticker under a delayed event loop.
func (m *Model) toastAnimCmd() tea.Cmd {
	now := time.Now()
	// A tick is already armed for a future moment; leave it in place instead of
	// scheduling an independent duplicate.
	if m.animTickDeadline.After(now) {
		return nil
	}
	gen := m.animTickGen
	for _, t := range m.toasts {
		if !t.ExpiredAt(now) && t.AnimatingAt(now) {
			m.animTickDeadline = now.Add(toastAnimInterval)
			return tea.Tick(toastAnimInterval, func(time.Time) tea.Msg { return animTickMsg{generation: gen} })
		}
	}
	var next time.Time
	for _, t := range m.toasts {
		if t.ExpiredAt(now) {
			continue
		}
		if begin := t.SlideOutStart(); !begin.IsZero() && begin.After(now) && (next.IsZero() || begin.Before(next)) {
			next = begin
		}
	}
	if next.IsZero() {
		m.animTickDeadline = time.Time{}
		return nil
	}
	m.animTickDeadline = next
	return tea.Tick(next.Sub(now), func(time.Time) tea.Msg { return animTickMsg{generation: gen} })
}

func (m *Model) AddToast(t *toast.Toast) {
	if t.Duration == 0 {
		t.Duration = defaultToastDuration
	}
	t.Show()
	// A new toast must start its slide-in promptly, so clear any tick already
	// armed for an older toast's future slide-out and invalidate its generation —
	// tea.Tick commands cannot be cancelled, so the stale tick must be
	// recognizably dead when its message eventually arrives. The subsequent
	// Update re-arms a short animation tick for the fresh toast.
	m.animTickDeadline = time.Time{}
	m.animTickGen++
	m.toasts = append(m.toasts, t)
	// Hard bound on live toasts (see toastLiveMax): a storm of distinct
	// actionable errors must not accumulate unbounded Toast objects. The oldest
	// alerts are dropped; every line still reaches the Logs tab above.
	if len(m.toasts) > toastLiveMax {
		m.toasts = m.toasts[len(m.toasts)-toastLiveMax:]
	}
}

// handleLogLines appends captured log lines to the render-facing ring and
// raises a toast for each line that looks actionable. Incoming lines are first
// stripped of ANSI escape sequences so neither the Logs tab nor a toast message
// can inject terminal control output. Toasts are deduplicated per normalized
// line when a stable key exists; an actionable line that cannot be keyed (e.g.
// slog's msg="") toasts every occurrence rather than being dropped, matching
// logDedupKey's documented contract for empty keys.
func (m *Model) handleLogLines(lines []string) {
	for _, raw := range lines {
		line := stripANSI(raw)
		m.logRing.Write([]byte(line + "\n"))

		if strings.TrimSpace(line) == "" {
			continue
		}
		if logLineIsActionable(line) {
			key := logDedupKey(line)
			if key != "" {
				if _, seen := m.toastSeen[key]; seen {
					continue
				}
				if len(m.toastSeen) >= toastSeenMax {
					m.evictOldestToastSeen(toastSeenEvict)
				}
				m.toastSeen[key] = struct{}{}
				m.toastSeenOrder = append(m.toastSeenOrder, key)
			}

			style := lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFF0F0")).
				Background(lipgloss.Color("#B62324")).
				Bold(true).
				PaddingLeft(1).
				PaddingRight(1)
			m.AddToast(&toast.Toast{
				Message: strings.TrimSpace(line),
				Style:   style,
				Width:   toastToastWidth(m.width),
			})
		}
	}
}

// evictOldestToastSeen drops the n oldest dedup keys so the set stays bounded
// while recently seen errors keep their dedup protection. An evicted key may
// therefore re-toast once; keys are tracked in insertion order by toastSeenOrder.
func (m *Model) evictOldestToastSeen(n int) {
	for n > 0 && len(m.toastSeenOrder) > 0 {
		delete(m.toastSeen, m.toastSeenOrder[0])
		m.toastSeenOrder = m.toastSeenOrder[1:]
		n--
	}
}

// drainLogs delivers every buffered log line written since the last drain
// through handleLogLines, advancing the model's read cursor. It runs only
// inside Update (periodic ticks and the quit path are both handled there), so
// delivery is sequential with respect to quit handling: a drained line is
// applied to the model before anything else can observe or act on the cursor.
// No-op for models built without a buffer.
func (m *Model) drainLogs() {
	if m.logBuf == nil {
		return
	}
	lines, cur := m.logBuf.ReadNew(m.logBufSeen)
	m.logBufSeen = cur
	if len(lines) > 0 {
		m.handleLogLines(lines)
	}
}

// flushPendingLogs publishes any unterminated fragment held in the shared log
// buffer and drains everything not yet delivered. This is a BEST-EFFORT final
// delivery: every line the producers published before the quit key was
// processed reaches the model, plus the torn tail if the producer has gone
// quiet. Lines written concurrently with (or after) this drain are not
// delivered here — they stay above the buffer's polled high-water mark and
// RedirectTo hands them to stderr at teardown, so only lines already evicted
// under ring-capacity pressure are ever lost. A completeness guarantee would
// require stopping and joining all log producers before the final drain, which
// this architecture cannot do: main() triggers proxy shutdown only after
// tui.Run returns. What DOES hold structurally is exactly-once: the drain runs
// inside Update through the model's shared read cursor, so no line can be
// delivered twice, and the teardown forward skips everything at or below that
// same cursor. bubbletea v2.0.7 then re-renders the returned model on graceful
// exit and paints that frame while stopping the renderer (tea.go render at
// event-loop and shutdown, stopRenderer's flush), so delivered lines are part
// of the visible final output. Teardown paths that bypass Update (Program.Kill,
// context cancellation) skip this flush and the final paint; their undrained
// tail still reaches stderr via the redirect, just unpainted. Both real
// producers newline-terminate their writes, so the torn-tail case is defensive
// hardening.
func (m *Model) flushPendingLogs() {
	if m.logBuf == nil {
		return
	}
	m.logBuf.Flush()
	m.drainLogs()
}

// LogWriter returns the io.Writer that log producers write captured lines
// into; the ring delivers them to the Logs tab.
func (m *Model) LogWriter() io.Writer { return &logWriter{ring: m.logRing} }

// toastToastWidth clamps the toast width to the terminal width so a long log line
// cannot push the styled toast past the pane edge. Before the terminal size is
// known (or for degenerately narrow widths) it assumes defaultToastWidth:
// RenderAt ignores a stored Width larger than the real pane, so a wrong guess
// only forfeits the reserved right margin on narrower terminals instead of
// overflowing them, while startup toasts keep that margin on terminals this
// wide or wider.
func toastToastWidth(width int) int {
	if width > 4 {
		return width - 4
	}
	return defaultToastWidth - 4
}
