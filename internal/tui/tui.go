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

// Package tui provides a Bubble Tea v2 terminal dashboard for the proxy.
//
// It renders a full-screen, interactive dashboard with six tabs:
//   - Overview: circuit breaker (when configured), throughput sparkline,
//     active + queued bars with labels, status counts, in-flight requests,
//     summary
//   - Requests: scrollable, inspectable log with search/filter
//   - Network: Chrome DevTools-equivalent network panel with request/response
//     inspection, waterfall timing, content-type detection, and filtering
//   - Logs: captured application log output (replaces stderr printing)
//   - Concurrency: live gauge, per-route bars, oldest queued age
//   - Routes: sorted per-route stats table
//
// The TUI listens for metrics.Snapshot messages on a channel and refreshes
// at ~4 fps. It supports full mouse interaction (click to select, wheel to
// scroll) and keyboard navigation.
package tui

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/scrollbar"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/viewport"
	"github.com/rivo/uniseg"
)

type uiMode int

const (
	modeBrowse uiMode = iota
	modeDetail
	modeFilter
	modeHelp
	modeConfirm
)

type tabID int

const (
	tabDashboard tabID = iota
	tabRequests
	tabNetwork
	tabLogs
	tabConcurrency
	tabRoutes
	numTabs
)

// tabNames is the single source of truth for tab labels used both for rendering
// and for mouse hit-testing. Each entry is rendered as " "+name+" " with
// PaddingLeft(1) PaddingRight(1) in the theme, so the visible cell width is
// len-agnostic and measured via style rendering in tabAt().
var tabNames = []string{"1 Overview", "2 Requests", "3 Network", "4 Logs", "5 Concurrency", "6 Routes"}

// networkFilterType controls which content types are shown in the Network tab.
type networkFilterType int

const (
	networkFilterAll networkFilterType = iota
	networkFilterJSON
	networkFilterHTML
	networkFilterEvents
	networkFilterOther
)

// networkFilterStatus controls which status code ranges are shown.
type networkFilterStatus int

const (
	networkStatusAll networkFilterStatus = iota
	networkStatus2xx
	networkStatus4xx
	networkStatus5xx
)

const (
	logRingCapacity      = 2048
	defaultToastDuration = 5 * time.Second
	defaultToastWidth    = 80
	contentStartRow      = 3
	redrawInterval       = 10 * time.Second
)

// logRing is a thread-safe ring buffer of log lines.
type logRing struct {
	mu       sync.Mutex
	lines    []string
	head     int
	count    int
	capacity int
}

func newLogRing(capacity int) *logRing {
	return &logRing{lines: make([]string, capacity), capacity: capacity}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := string(p)
	for text != "" {
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			if r.count < r.capacity {
				r.lines[(r.head+r.count)%r.capacity] = text
				r.count++
			} else {
				r.lines[r.head] = text
				r.head = (r.head + 1) % r.capacity
			}
			break
		}
		line := text[:idx]
		text = text[idx+1:]
		// Blank lines are deliberately skipped here too, mirroring
		// LogBuffer.Write; pinned by TestLogRing_WriteEmptyLinesSkipped.
		if line == "" {
			continue
		}
		if r.count < r.capacity {
			r.lines[(r.head+r.count)%r.capacity] = line
			r.count++
		} else {
			r.lines[r.head] = line
			r.head = (r.head + 1) % r.capacity
		}
	}
	return len(p), nil
}

func (r *logRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return nil
	}
	out := make([]string, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.lines[(r.head+i)%r.capacity]
	}
	return out
}

// Len returns the number of retained lines, guarded by the same lock that
// protects Write, so reading the total cannot race a concurrent writer.
func (r *logRing) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// logWriter wraps a logRing as an io.Writer.
type logWriter struct{ ring *logRing }

func (w *logWriter) Write(p []byte) (int, error) { return w.ring.Write(p) }

// LogBufferCapacity is the default line capacity of a LogBuffer.
const LogBufferCapacity = 2048

// logBufLine is a single captured line with a globally unique sequence number.
// The sequence number lets a poller read only the lines written since its last
// read without deduplicating by snapshot identity.
type logBufLine struct {
	seq  uint64
	text string
}

// LogBuffer is a thread-safe, bounded capture buffer for program log output. It
// is the sink that main() installs for the global log/slog writers when the TUI
// is enabled, so that (a) logs never leak to stderr and (b) the Logs tab
// has a bounded, pollable source of lines. Lines beyond capacity evict the oldest.
// Bounded means line count: an individual line — and thus the transient
// allocation publishing it — is as large as its input write.
//
// Once RedirectTo hands the buffer a live target (main.go does this the moment
// the TUI exits), Writes stream straight through to that target instead: after
// the dashboard is gone there is no poller left to drain the ring, so teardown
// telemetry must not be parked here to die with the process. The flip also
// hands over every line no poller has ever delivered, plus any pending
// fragment, so nothing undelivered is stranded in the dead buffer.
type LogBuffer struct {
	mu       sync.Mutex
	lines    []logBufLine
	head     int
	count    int
	capacity int
	seq      uint64
	pending  []byte

	// polled is the delivery high-water mark: the highest sequence number any
	// ReadNew call has returned to a poller. Lines above it were never seen by
	// the TUI and are exactly what RedirectTo forwards at teardown.
	polled uint64

	passthrough io.Writer
}

// NewLogBuffer returns an empty LogBuffer with the given line capacity.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &LogBuffer{lines: make([]logBufLine, capacity), capacity: capacity}
}

// maxPendingLine caps the unterminated fragment a LogBuffer retains between
// Writes (64 KiB). Both real producers (stdlib log, slog TextHandler)
// terminate every write with a newline, so the fragment stays empty in
// practice; the cap only bounds memory if some writer streams newline-free
// bytes without end.
const maxPendingLine = 64 << 10

// Write captures complete newline-terminated lines from p, assembling a logical
// line that is split across multiple Write calls (io.Writer has no line-boundary
// guarantees, so a trailing unterminated fragment is carried forward until a
// newline arrives or Flush is called rather than being published as a phony
// line). The retained fragment never exceeds maxPendingLine bytes: a writer
// that streams past the cap without a newline has its accumulated fragment
// published at the cap boundary with Flush semantics, so retained memory stays
// bounded while no bytes are dropped or reordered. It always consumes all of p
// and reports that: the caller's byte count is the full length of the input,
// never a partial write.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.passthrough != nil {
		return b.passthrough.Write(p)
	}
	total := len(p)
	// Prepend any fragment left over from a prior write so a line split across
	// Write boundaries is reassembled before splitting again.
	if b.pending != nil {
		data := make([]byte, 0, len(b.pending)+len(p))
		data = append(data, b.pending...)
		data = append(data, p...)
		b.pending = nil
		p = data
	}
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx >= 0 {
			seg := p[:idx]
			p = p[idx+1:]
			// Blank segments are deliberately not published: real producers
			// (stdlib log, slog TextHandler) never emit bare empty lines and
			// they would only spend ring capacity — pinned by
			// TestLogBuffer_EmptySegmentsSkipped.
			if len(seg) > 0 {
				b.publish(seg)
			}
			continue
		}
		// Unterminated tail. Retain it — as a copy (io.Writer implementations
		// must not retain p, and the caller is free to reuse its buffer after
		// Write returns) — unless it exceeds the cap, in which case publish the
		// overflow now rather than let retained memory grow without bound.
		if len(p) > maxPendingLine {
			b.publish(p[:maxPendingLine])
			p = p[maxPendingLine:]
			continue
		}
		b.pending = append([]byte(nil), p...)
		break
	}
	return total, nil
}

// publish appends one complete line to the ring, evicting the oldest line when
// full. Callers must hold b.mu.
func (b *LogBuffer) publish(text []byte) {
	b.seq++
	line := logBufLine{seq: b.seq, text: string(text)}
	if b.count < b.capacity {
		b.lines[(b.head+b.count)%b.capacity] = line
		b.count++
	} else {
		b.lines[b.head] = line
		b.head = (b.head + 1) % b.capacity
	}
}

// Flush publishes any unterminated fragment retained from prior Write calls as
// a single final line. It is idempotent and a no-op when no fragment is pending.
func (b *LogBuffer) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return
	}
	b.publish(b.pending)
	b.pending = nil
}

// RedirectTo switches the buffer into live passthrough mode: from this call on,
// Write delegates directly to w (under the same lock, so producers cannot tear
// a line across the switch), bypassing the bounded ring entirely. Before the
// flip it hands w everything no poller has delivered: every retained line whose
// sequence number exceeds the ReadNew high-water mark, then any pending
// fragment, so nothing undelivered is stranded in the dead buffer. Forwarded
// content is dropped from the buffer — repeated redirects, or a restore-and-flip
// cycle, can therefore never emit a line twice, and lines a poller already
// delivered are never re-emitted. A nil w restores ordinary buffering without
// forwarding anything. This is what main.go invokes the moment the TUI exits:
// with the dashboard gone there is no poller to drain the ring, so
// graceful-shutdown logging must stream to stderr instead of being parked here
// until process exit.
func (b *LogBuffer) RedirectTo(w io.Writer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if w == nil {
		b.passthrough = nil
		return
	}
	b.passthrough = w
	var out bytes.Buffer
	for i := 0; i < b.count; i++ {
		if line := b.lines[(b.head+i)%b.capacity]; line.seq > b.polled {
			out.WriteString(line.text)
			out.WriteByte('\n')
		}
	}
	out.Write(b.pending)
	// Best-effort flush mirroring the io.Writer contract elsewhere in this
	// type: a failing sink cannot be meaningfully handled under this lock,
	// and the handed-off content is dropped either way.
	_, _ = w.Write(out.Bytes()) //nolint:errcheck
	b.pending = nil
	b.head, b.count = 0, 0
}

// Revision returns the sequence number of the most recently written line (0 when
// none have been written). It is monotonic across overwrites.
func (b *LogBuffer) Revision() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// ReadNew returns every retained line whose sequence number exceeds after, in
// write order, together with the revision the caller must resume from. Both are
// computed under a single lock, so a line written in the middle of a poll can
// never be returned twice: this poll either returns it (and the caller resumes
// past it) or misses it (and the next poll returns it, since it is still
// retained). Lines evicted by the bounded capacity are simply not returned; the
// revision still advances past them so an evicted line is never re-read.
//
// Each call also advances the buffer's polled high-water mark to the returned
// revision, recording what has been delivered; RedirectTo relies on that mark
// to forward only lines no poller ever saw.
func (b *LogBuffer) ReadNew(after uint64) ([]string, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.polled = b.seq
	if after >= b.seq {
		return nil, b.seq
	}
	var out []string
	for i := 0; i < b.count; i++ {
		if line := b.lines[(b.head+i)%b.capacity]; line.seq > after {
			out = append(out, line.text)
		}
	}
	return out, b.seq
}

type Model struct {
	width, height int
	tab           tabID
	mode          uiMode
	cursor        int
	scroll        int
	conc          int
	snap          metrics.Snapshot
	startTime     time.Time
	filterText    string

	// styles is the active color/style set. It defaults to the dark palette
	// (set by NewModelForProviders) and is swapped to the light palette when
	// the program receives a tea.BackgroundColorMsg reporting a light
	// terminal background (see Init / Update). All render paths resolve
	// styles through this field.
	styles tuiTheme

	resetCh chan struct{}
	journal *journal.Journal

	// Network tab state.
	networkFilterType   networkFilterType
	networkFilterStatus networkFilterStatus

	// networkFiltered caches the result of computeVisibleNetworkEntries()
	// so the heavy filter work runs once per Update cycle instead of
	// multiple times per View frame.
	networkFiltered []*journal.Entry

	// dashboardLinesCache stores the rendered dashboard lines so
	// dashboardLines() is called at most once per Update cycle instead of
	// once per maxCursor() call (up to 4 times per frame). It is invalidated
	// only when the underlying snapshot, terminal size, or active tab changes.
	dashboardLinesCache []string

	logRing    *logRing
	toasts     []*toast.Toast
	scrollbars [numTabs]scrollbar.Model

	// followLogs makes the Logs tab anchor on the newest lines, auto-scrolling
	// as they arrive. Any explicit scroll input pauses following; G/End or
	// switching back to the Logs tab re-engages it.
	followLogs bool

	// toastSeen deduplicates log lines that already triggered a toast so the
	// same recurring message does not spam the dashboard. It is bounded: when
	// it reaches toastSeenMax keys, the oldest toastSeenEvict entries are
	// evicted in insertion order, so recently seen errors keep their dedup
	// protection instead of the whole set resetting simultaneously.
	toastSeen map[string]struct{}

	// toastSeenOrder preserves the insertion order of the keys currently in
	// toastSeen (oldest first) so eviction can drop the oldest entries. It
	// always contains exactly the keys present in toastSeen.
	toastSeenOrder []string

	// logBuf wires the model to the shared captured-log buffer when Run
	// installs one (nil otherwise), and logBufSeen is the model's read cursor
	// into it. The cursor is confined to the update loop — both periodic
	// draining (logPollTickMsg) and the quit-time flush read through it inside
	// Update — so a line can never be extracted on one goroutine and lost
	// before another applies it. nil logBuf means no buffer wiring.
	logBuf     *LogBuffer
	logBufSeen uint64

	// animTickDeadline is the time at which the single armed animation tick will
	// fire (zero = none armed). It makes the animation ticker single-owner: once
	// Update arms a tick for a future moment, unrelated updates (snapshots, mouse
	// motion, log polls) must not stack a second independent ticker. The deadline
	// is cleared by AddToast so a newly added toast re-arms promptly.
	animTickDeadline time.Time

	// animTickGen invalidates outstanding animation ticks. tea.Tick commands
	// cannot be cancelled, so every armed tick captures the generation at arming
	// time and a stale-generation animTickMsg is dropped on arrival instead of
	// stacking a redundant ticker under a delayed event loop. AddToast is the
	// only mutator: it is the sole point where an armed schedule is superseded.
	animTickGen uint64

	dragging        bool
	dragStartY      int
	dragStartScroll int

	// redrawEpoch makes View.Content differ on resync frames without changing
	// visible text. It is paired with ClearScreen so Bubble Tea's renderer
	// cannot skip the repaint after tmux or terminal state changes.
	redrawEpoch int

	// providers holds the per-provider state for a multi-provider dashboard,
	// and active is the index of the provider currently shown. With a single
	// unnamed provider the dashboard behaves exactly like the legacy model: the
	// flat conc/snap/journal fields below mirror the active provider's state.
	providers []providerState
	active    int
}

// ProviderMeta describes one upstream provider for a multi-provider dashboard.
// A single ProviderMeta with an empty Name reproduces the legacy single-provider
// header (the "⚡ shaper" brand, no switcher chips).
type ProviderMeta struct {
	Name        string
	Concurrency int
	Journal     *journal.Journal
}

// ProviderUpdate carries the latest metrics snapshot for one provider, tagged
// with the provider's index in the metas slice passed to Run.
type ProviderUpdate struct {
	Index    int
	Snapshot metrics.Snapshot
}

// providerState is the live state kept for a single provider in a dashboard
// model: its display name, concurrency limit, journal and latest snapshot.
type providerState struct {
	name    string
	conc    int
	snap    metrics.Snapshot
	journal *journal.Journal
}

// NewModelForProviders creates a dashboard for one or more providers. Each
// provider is addressed by index everywhere: ProviderUpdate messages, the Tab /
// Shift+Tab cycle keys handled in handleKey, and the chips on header row 0.
// The active provider's fields are mirrored into m.conc/m.snap/m.journal so
// all existing renderers keep working against a single Provider.
func NewModelForProviders(metas []ProviderMeta) Model {
	m := Model{
		startTime:  time.Now(),
		resetCh:    make(chan struct{}, 1),
		logRing:    newLogRing(logRingCapacity),
		followLogs: true,
		toastSeen:  make(map[string]struct{}),
		styles:     newTheme(true),
	}
	for _, meta := range metas {
		m.providers = append(m.providers, providerState{
			name:    meta.Name,
			conc:    meta.Concurrency,
			journal: meta.Journal,
		})
	}
	m.syncActive()
	for i := range m.scrollbars {
		m.scrollbars[i] = *scrollbar.New()
	}
	m.applyScrollbarTheme()
	return m
}

// syncActive mirrors the active provider's per-provider state (concurrency
// limit, journal, snapshot) into the flat fields the renderers read.
func (m *Model) syncActive() {
	if m.active < 0 || m.active >= len(m.providers) {
		return
	}
	ps := &m.providers[m.active]
	m.conc = ps.conc
	m.journal = ps.journal
	m.snap = ps.snap
	m.dashboardLinesCache = nil
}

// switchProvider makes provider i the active one, clamping i to bounds. It
// resets the per-tab navigation state and drops the dashboard line cache so the
// next frame renders for the newly active provider.
func (m *Model) switchProvider(i int) {
	if len(m.providers) == 0 {
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= len(m.providers) {
		i = len(m.providers) - 1
	}
	m.active = i
	m.syncActive()
	m.tab = tabDashboard
	m.cursor = 0
	m.scroll = 0
	m.mode = modeBrowse
	m.filterText = ""
	m.dashboardLinesCache = nil
}

// cycleProvider moves the active provider by delta, wrapping at either end. Tab
// (delta +1) and Shift+Tab (delta -1) are wired to it in handleKey.
func (m *Model) cycleProvider(delta int) {
	if n := len(m.providers); n > 0 {
		next := (m.active + delta) % n
		if next < 0 {
			next += n
		}
		m.switchProvider(next)
	}
}

func (m *Model) LogWriter() io.Writer { return &logWriter{ring: m.logRing} }

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

func (m Model) resyncTickCmd() tea.Cmd {
	return tea.Tick(redrawInterval, func(time.Time) tea.Msg {
		return resyncTickMsg{}
	})
}

func immediateResyncDrawCmd() tea.Cmd {
	return func() tea.Msg {
		return resyncDrawMsg{}
	}
}

func resyncRedrawSequence() tea.Cmd {
	return tea.Sequence(tea.ClearScreen, immediateResyncDrawCmd())
}

func redrawMarker(epoch int) string {
	if epoch%2 == 0 {
		return "\x1b[0m"
	}
	return "\x1b[00m"
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

func (m Model) Init() tea.Cmd {
	// Ask the terminal for its background color. The response arrives as a
	// tea.BackgroundColorMsg (handled in Update) and swaps to the light
	// palette if the terminal reports a light background; terminals that do
	// not answer simply keep the dark default.
	return tea.Batch(m.resyncTickCmd(), tea.RequestBackgroundColor)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case resetMsg:
		select {
		case m.resetCh <- struct{}{}:
		default:
		}
	case logPollTickMsg:
		m.drainLogs()
	case animTickMsg:
		if msg.generation != m.animTickGen {
			// Stale schedule (superseded by a later AddToast): drop the tick
			// without re-rendering or re-arming. Model state has not changed
			// since the last update, so the tail processing is unnecessary.
			return m, nil
		}
		// No state to change; flows through to re-render and, if still
		// animating, schedule the next tick below.
	case resyncTickMsg:
		return m, resyncRedrawSequence()
	case resyncDrawMsg:
		m.redrawEpoch++
		return m, m.resyncTickCmd()
	case metrics.Snapshot:
		// Sugar for the single-provider case: treat it as an update for the
		// active provider so Run() callers that don't tag updates keep working.
		// Guarded like the ProviderUpdate case below: a model with no
		// providers must not index into the empty slice.
		if len(m.providers) == 0 {
			return m, cmd
		}
		m.providers[m.active].snap = msg
		m.snap = msg
		m.dashboardLinesCache = nil
	case ProviderUpdate:
		if msg.Index < 0 || msg.Index >= len(m.providers) {
			return m, cmd
		}
		m.providers[msg.Index].snap = msg.Snapshot
		if msg.Index == m.active {
			// Mirror into the flat snapshot so the renderers (and the
			// legacy case above) stay in sync with the active provider.
			m.snap = msg.Snapshot
			m.dashboardLinesCache = nil
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dashboardLinesCache = nil
	case tea.BackgroundColorMsg:
		// The terminal reported its background (see Init). Repaint the whole
		// UI for the light palette when the background is light; the dark
		// palette is the built-in default. Rendering caches are invalidated so
		// the next View repaints from the new theme, and scrollbars repaint too.
		m.styles = newTheme(msg.IsDark())
		m.dashboardLinesCache = nil
		m.applyScrollbarTheme()
	case tea.KeyPressMsg:
		m, cmd = m.handleKey(msg)
	case tea.MouseClickMsg:
		m, cmd = m.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		m, cmd = m.handleMouseWheel(msg)
	case tea.MouseMotionMsg:
		m, cmd = m.handleMouseMotion(msg)
	case tea.MouseReleaseMsg:
		m, cmd = m.handleMouseRelease(msg)
	}
	m.networkFiltered = m.computeVisibleNetworkEntries()
	m.toasts = toast.VisibleToasts(m.toasts)

	// Tail-follow: while on the Logs tab and not paused by scroll input, keep the
	// viewport pinned to the newest lines so incoming logs scroll into view.
	if m.tab == tabLogs && m.followLogs && m.width > 0 && m.height > 0 {
		m.cursor = m.maxCursor()
		m.scroll = m.maxScroll()
	}
	m.adjustViewport()

	if c := m.toastAnimCmd(); c != nil {
		cmd = tea.Batch(cmd, c)
	}
	return m, cmd
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	// Help mode: any key dismisses (checked before quit so 'q' in help doesn't kill)
	if m.mode == modeHelp {
		m.mode = modeBrowse
		return m, nil
	}

	if m.mode == modeConfirm {
		switch msg.String() {
		case "y":
			m.mode = modeBrowse
			return m, tea.Batch(m.resetCmd())
		case "n", "esc":
			m.mode = modeBrowse
			return m, nil
		default:
			m.mode = modeBrowse
			return m, nil
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		// Best-effort final drain so the last render includes everything
		// published before this key was processed (see flushPendingLogs).
		m.flushPendingLogs()
		return m, tea.Quit
	}

	if m.mode == modeDetail {
		switch msg.String() {
		case "esc", "enter", " ", "space":
			m.mode = modeBrowse
			return m, nil
		default:
			return m, nil
		}
	}

	if m.mode == modeFilter {
		switch msg.String() {
		case "esc":
			m.mode = modeBrowse
			m.filterText = ""
			return m, nil
		case "enter":
			m.mode = modeBrowse
			return m, nil
		case "backspace", "ctrl+h":
			runes := []rune(m.filterText)
			if len(runes) > 0 {
				m.filterText = string(runes[:len(runes)-1])
			}
			return m, nil
		default:
			// Only accumulate printable characters into the filter.
			// Key.Text is non-empty only for printable characters in
			// real terminal input. Special keys (arrows, F-keys,
			// Home/End, etc.) and modifier combos (ctrl+, alt+) all
			// have empty Text, preventing them from corrupting the
			// filter query.
			if msg.Key().Text != "" {
				m.filterText += msg.Key().Text
			}
			return m, nil
		}
	}

	keyCode := msg.Key().Code
	if keyCode == tea.KeyPgUp {
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(-m.dataRows())
		} else {
			m.cursor -= m.dataRows()
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.adjustViewport()
		}
		return m, nil
	}
	if keyCode == tea.KeyPgDown {
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(m.dataRows())
		} else {
			m.cursor += m.dataRows()
			if m.cursor > m.maxCursor() {
				m.cursor = m.maxCursor()
			}
			m.adjustViewport()
		}
		return m, nil
	}
	if keyCode == tea.KeyHome {
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scroll = 0
			m.cursor = 0
		} else {
			m.cursor = 0
			m.adjustViewport()
		}
		return m, nil
	}
	if keyCode == tea.KeyEnd {
		if m.tab == tabLogs {
			m.followLogs = true
		}
		if m.tab == tabDashboard {
			m.scroll = m.maxScroll()
			m.cursor = m.scroll
		} else {
			m.cursor = m.maxCursor()
			m.adjustViewport()
		}
		return m, nil
	}

	// Provider switching (multi-provider): Tab cycles to the next provider,
	// Shift+Tab to the previous; both wrap. bubbletea decodes Shift+Tab as
	// the Tab key code with a Shift modifier rather than a distinct key
	// constant, so the modifier tells the two apart.
	if keyCode == tea.KeyTab {
		if msg.Key().Mod != 0 {
			m.cycleProvider(-1)
		} else {
			m.cycleProvider(1)
		}
		return m, nil
	}

	switch msg.String() {
	case "?":
		m.mode = modeHelp
	case "1":
		m.switchTab(tabDashboard)
	case "2":
		m.switchTab(tabRequests)
	case "3":
		m.switchTab(tabNetwork)
	case "4":
		m.switchTab(tabLogs)
	case "5":
		m.switchTab(tabConcurrency)
	case "6":
		m.switchTab(tabRoutes)

	case "j", "down":
		m.moveCursor(1)
	case "k", "up":
		m.moveCursor(-1)
	case "g":
		if m.tab == tabLogs {
			m.followLogs = false
		}
		m.cursor, m.scroll = 0, 0
	case "G":
		if m.tab == tabLogs {
			m.followLogs = true
		}
		if m.tab == tabDashboard {
			m.scroll = m.maxScroll()
			m.cursor = m.scroll
		} else {
			m.cursor, m.scroll = m.maxCursor(), m.maxScroll()
		}

	case "ctrl+u":
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(-m.dataRows() / 2)
		} else {
			m.cursor -= m.dataRows() / 2
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.adjustViewport()
		}
	case "ctrl+d":
		if m.tab == tabLogs {
			m.followLogs = false
		}
		if m.tab == tabDashboard {
			m.scrollDashboard(m.dataRows() / 2)
		} else {
			m.cursor += m.dataRows() / 2
			if m.cursor > m.maxCursor() {
				m.cursor = m.maxCursor()
			}
			m.adjustViewport()
		}

	case "enter", " ", "space":
		if m.canInspect() {
			m.mode = modeDetail
		}

	case "/":
		if m.tab == tabRequests || m.tab == tabNetwork || m.tab == tabLogs {
			m.mode = modeFilter
			m.filterText = ""
		}

	case "c":
		m.mode = modeConfirm

	case "t":
		if m.tab == tabNetwork {
			m.networkFilterType = networkFilterType((int(m.networkFilterType) + 1) % 5)
			m.cursor = 0
			m.scroll = 0
		}

	case "s":
		if m.tab == tabNetwork {
			m.networkFilterStatus = networkFilterStatus((int(m.networkFilterStatus) + 1) % 4)
			m.cursor = 0
			m.scroll = 0
		}
	}

	return m, nil
}

func (m *Model) switchTab(t tabID) {
	m.tab = t
	m.cursor = 0
	m.scroll = 0
	m.mode = modeBrowse
	m.dashboardLinesCache = nil
	if t == tabLogs {
		m.followLogs = true
	}
}

func (m *Model) moveCursor(delta int) {
	if m.tab == tabLogs {
		m.followLogs = false
	}
	if m.tab == tabDashboard {
		m.scrollDashboard(delta)
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	max := m.maxCursor()
	if m.cursor > max {
		m.cursor = max
	}
	m.adjustViewport()
}

// scrollDashboard adjusts m.scroll directly by delta for the Dashboard tab,
// clamping to the content bounds. m.cursor is pinned to the scroll position
// because the Dashboard has no visible cursor; this keeps the scrollbar model
// consistent with the rest of the viewport code.
func (m *Model) scrollDashboard(delta int) {
	m.scroll += delta
	if m.scroll < 0 {
		m.scroll = 0
	}
	max := m.maxScroll()
	if m.scroll > max {
		m.scroll = max
	}
	m.cursor = m.scroll
}

func (m *Model) adjustViewport() {
	maxC := m.maxCursor()
	if m.cursor > maxC {
		m.cursor = maxC
	}
	rows := m.dataRows()
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+rows {
		m.scroll = m.cursor - rows + 1
	}
	maxScroll := m.maxScroll()
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m *Model) visibleRows() int {
	// Reserve only the chrome (header, tabbar, separator, footer). Filter input
	// and active toasts are overlays above the footer; they reduce the
	// scrollable content area only while they are present, so no space is
	// wasted when they are absent.
	v := m.height - 4
	if m.mode == modeFilter && (m.tab == tabRequests || m.tab == tabNetwork || m.tab == tabLogs) {
		v--
	}
	if m.mode == modeBrowse {
		if n := len(m.toasts); n > 0 {
			v -= min(n, 3)
		}
	}
	if v < 0 {
		return 0
	}
	return v
}

// dataRows returns the number of data rows displayed for the active tab,
// after reserving fixed header, filter summary, and count lines. For the
// dashboard the entire content area is scrollable, so dataRows == visibleRows.
func (m *Model) dataRows() int {
	switch m.tab {
	case tabRequests:
		fixed := 2 // table header + count line
		if m.filterText != "" {
			fixed++ // filter summary
		}
		return max(m.visibleRows()-fixed, 1)
	case tabNetwork:
		fixed := 2 // table header + count line
		if m.networkFilterType != networkFilterAll || m.networkFilterStatus != networkStatusAll {
			fixed++ // type/status filter summary
		}
		if m.filterText != "" {
			fixed++ // text filter summary
		}
		return max(m.visibleRows()-fixed, 1)
	case tabLogs:
		fixed := 0 // no header row; line numbers are embedded in data rows
		if m.filterText != "" {
			fixed++ // filter summary
		}
		return max(m.visibleRows()-fixed, 1)
	case tabRoutes:
		return max(m.visibleRows()-2, 1) // header + count line
	case tabConcurrency:
		// Section headers/gauges for Concurrency Gauge, Queue Depth, and the
		// In-Flight Requests title occupy the first 10 rows of the content area.
		return max(m.visibleRows()-10, 1)
	case tabDashboard:
		return m.visibleRows()
	}
	return m.visibleRows()
}

// contentHeaderRows returns the number of fixed rows at the top of the
// scrollable area before the first data row. Clicks inside these rows should
// not move the cursor.
func (m Model) contentHeaderRows() int {
	switch m.tab {
	case tabRequests:
		if m.filterText != "" {
			return 2
		}
		return 1
	case tabNetwork:
		n := 1
		if m.networkFilterType != networkFilterAll || m.networkFilterStatus != networkStatusAll {
			n++
		}
		if m.filterText != "" {
			n++
		}
		return n
	case tabLogs:
		if m.filterText != "" {
			return 1 // filter summary only
		}
		return 0 // no header; line numbers are embedded in data rows
	case tabRoutes:
		return 1
	case tabConcurrency:
		return 10
	}
	return 0
}

// viewportWidth returns the width available for scrollable content,
// excluding the scrollbar column and separator.
func (m *Model) viewportWidth() int {
	return max(m.width-1, 1)
}

// gaugeBarWidth returns the inner block count for renderGaugeBar so the full
// rendered bar ("  [" + blocks + "]  ") matches the queue bar width and fits
// within the viewport width with a symmetrical two-cell left and right margin
// before the scrollbar column.
func (m *Model) gaugeBarWidth() int {
	// Full bar visual width: 3 + blocks + 1 + 2 = blocks + 6.
	// Must fit within viewportWidth.
	return max(m.viewportWidth()-6, 0)
}

// hBarWidth returns the inner block count for renderHBar so the full
// bar ("  [" + blocks + "]  ") fits within the viewport width with a
// symmetrical two-cell left and right margin before the scrollbar
// column.
func (m *Model) hBarWidth() int {
	// Full bar visual width: 3 + blocks + 1 + 2 = blocks + 6.
	// Must fit within viewportWidth.
	return max(m.viewportWidth()-6, 0)
}

// gaugeTrackWidth returns the bar track width used for the Active
// gauge in the dual bars row. The Status section bar uses the same
// width so its brackets align with the Active gauge at the same
// column (both section labels are 10 cells wide). The dual bars row
// has 27 fixed cells of labels, brackets, gap, and trailing spaces;
// the Active gauge takes half of the remaining width.
func (m *Model) gaugeTrackWidth() int {
	return max((m.viewportWidth()-27)/2, 0)
}

func (m *Model) maxCursor() int {
	switch m.tab {
	case tabDashboard:
		return max(len(m.cachedDashboardLines())-1, 0)
	case tabRequests:
		return max(len(m.visibleEntries())-1, 0)
	case tabNetwork:
		return max(len(m.visibleNetworkEntries())-1, 0)
	case tabLogs:
		return max(len(m.visibleLogLines())-1, 0)
	case tabConcurrency:
		return max(len(m.snap.InFlight)-1, 0)
	case tabRoutes:
		stats := m.snap.RouteStats
		return max(len(stats)-1, 0)
	}
	return 0
}

func (m *Model) maxScroll() int {
	return max(m.maxCursor()-m.dataRows()+1, 0)
}

// visibleEntries returns the currently visible request entries, respecting filter.
func (m *Model) visibleEntries() []metrics.RequestLogEntry {
	if m.filterText == "" {
		return m.snap.LogEntries
	}
	var filtered []metrics.RequestLogEntry
	lower := strings.ToLower(m.filterText)
	for _, e := range m.snap.LogEntries {
		if strings.Contains(strings.ToLower(e.Method), lower) ||
			strings.Contains(strings.ToLower(e.Path), lower) ||
			strings.Contains(strconv.Itoa(e.Status), lower) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// visibleNetworkEntries returns the cached filtered list. The cache is
// refreshed once per Update cycle to avoid redundant allocations.
func (m Model) visibleNetworkEntries() []*journal.Entry {
	return m.networkFiltered
}

// visibleLogLines returns log lines for the Logs tab, respecting filter.
func (m *Model) visibleLogLines() []string {
	all := m.logRing.snapshot()
	if all == nil {
		return nil
	}
	if m.filterText == "" {
		return all
	}
	var filtered []string
	lower := strings.ToLower(m.filterText)
	for _, line := range all {
		if strings.Contains(strings.ToLower(line), lower) {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

// applyScrollbarTheme paints every per-tab scrollbar with the active theme's
// thumb/track colors. Called from NewModelForProviders and on theme swaps (and
// re-applied on each updateScrollbars) so a background change repaints the
// scrollbars too.
func (m *Model) applyScrollbarTheme() {
	for i := range m.scrollbars {
		m.scrollbars[i].ThumbStyle = m.styles.scrollbarThumb
		m.scrollbars[i].TrackStyle = m.styles.scrollbarTrack
	}
}

func (m *Model) updateScrollbars() {
	contentHeight := m.maxCursor() + 1
	viewportHeight := m.dataRows()
	sb := &m.scrollbars[m.tab]
	sb.ContentHeight = contentHeight
	sb.ViewportHeight = viewportHeight
	sb.YOffset = m.scroll
	m.applyScrollbarTheme()
}

// truncateANSI truncates line to at most width terminal cells, preserving the
// CSI/SGR escape sequences produced by lipgloss. It appends a reset sequence
// (ESC[0m) if truncation occurs so that active styles do not leak into the
// trailing padding. It intentionally does not handle OSC/DCS/APC/SOS
// sequences because the TUI only emits SGR styling.
func truncateANSI(line string, width int) string {
	return truncateGraphemes(line, width, true)
}

// truncatePlain truncates a plain, unstyled string to at most width terminal
// cells using the same grapheme-cluster semantics as truncateANSI. It never
// emits escape sequences: the callers feed the result into a surrounding
// lipgloss style, where an embedded reset would strip that style's background
// from the rest of the rendered row.
func truncatePlain(s string, width int) string {
	return truncateGraphemes(s, width, false)
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
// rows returned by contentHeaderRows().
func (m Model) renderContentWithScrollbar() string {
	content := m.renderContent()
	if content == "" {
		return ""
	}
	sb := m.scrollbars[m.tab]
	scrollbarCol := sb.View()
	sbLines := strings.Split(scrollbarCol, "\n")

	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	contentWidth := m.viewportWidth()
	headerRows := m.contentHeaderRows()
	visibleRows := m.visibleRows()

	var b strings.Builder
	// Clamp the rendered content to the allocated viewport. Individual tab
	// renderers are expected to stay within visibleRows, but this guard
	// prevents any overflow from pushing chrome or footer off-screen.
	contentLimit := min(len(lines), visibleRows)
	for i := range visibleRows {
		if i < contentLimit {
			line := lines[i]
			stripped := stripANSI(line)
			visibleCells := uniseg.StringWidth(stripped)
			if visibleCells > contentWidth {
				truncated := truncateANSI(line, contentWidth)
				b.WriteString(truncated)
				actualWidth := uniseg.StringWidth(stripANSI(truncated))
				if actualWidth < contentWidth {
					b.WriteString(strings.Repeat(" ", contentWidth-actualWidth))
				}
			} else {
				b.WriteString(line)
				b.WriteString(strings.Repeat(" ", contentWidth-visibleCells))
			}
		} else {
			b.WriteString(strings.Repeat(" ", contentWidth))
		}
		if i >= headerRows {
			if sbIdx := i - headerRows; sbIdx < len(sbLines) {
				b.WriteString(sbLines[sbIdx])
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// computeVisibleNetworkEntries performs the actual filter logic used to
// rebuild the networkFiltered cache.
func (m Model) computeVisibleNetworkEntries() []*journal.Entry {
	if m.journal == nil {
		return nil
	}
	all := m.journal.Entries()
	if all == nil {
		return nil
	}

	var filtered []*journal.Entry
	if m.filterText != "" {
		lower := strings.ToLower(m.filterText)
		for _, e := range all {
			if strings.Contains(strings.ToLower(e.Name()), lower) ||
				strings.Contains(strings.ToLower(e.Method), lower) ||
				strings.Contains(strings.ToLower(e.URL.Path), lower) ||
				strings.Contains(strconv.Itoa(e.StatusCode), lower) ||
				strings.Contains(strings.ToLower(e.Type()), lower) {
				filtered = append(filtered, e)
			}
		}
	} else {
		filtered = all
	}

	if m.networkFilterType != networkFilterAll {
		var byType []*journal.Entry
		for _, e := range filtered {
			matches := false
			switch m.networkFilterType {
			case networkFilterJSON:
				matches = e.Type() == "json"
			case networkFilterHTML:
				matches = e.Type() == "html"
			case networkFilterEvents:
				matches = e.Type() == "events"
			case networkFilterOther:
				matches = e.Type() != "json" && e.Type() != "html" && e.Type() != "events"
			}
			if matches {
				byType = append(byType, e)
			}
		}
		filtered = byType
	}

	if m.networkFilterStatus != networkStatusAll {
		var byStatus []*journal.Entry
		for _, e := range filtered {
			matches := false
			switch m.networkFilterStatus {
			case networkStatus2xx:
				matches = e.StatusCode >= 200 && e.StatusCode < 300
			case networkStatus4xx:
				matches = e.StatusCode >= 400 && e.StatusCode < 500
			case networkStatus5xx:
				matches = e.StatusCode >= 500 && e.StatusCode < 600
			}
			if matches {
				byStatus = append(byStatus, e)
			}
		}
		filtered = byStatus
	}

	return filtered
}

func (m *Model) canInspect() bool {
	switch m.tab {
	case tabRequests:
		return m.cursor < len(m.visibleEntries())
	case tabNetwork:
		return m.cursor < len(m.visibleNetworkEntries())
	case tabLogs:
		return m.cursor < len(m.visibleLogLines())
	case tabConcurrency:
		return m.cursor < len(m.snap.InFlight)
	default:
		return false
	}
}

func (m Model) handleMouseClick(msg tea.MouseClickMsg) (Model, tea.Cmd) {
	mx := msg.Mouse().X
	my := msg.Mouse().Y

	// Provider switcher chips (row 0). chipAt does the same right-aligned
	// cumulative-width hit test that renderHeader uses to position the chips, so a
	// click lands on whichever chip the user sees under the cursor.
	if my == 0 {
		if i, ok := m.chipAt(mx); ok {
			m.switchProvider(i)
		}
		return m, nil
	}

	// Tab bar (row 1).
	if my == 1 {
		// Clicks beyond the terminal width land on non-existent cells and
		// must not switch tabs. tabAt deliberately ignores m.width, so this
		// guard is the boundary between rendered and unrendered columns.
		if mx >= m.width {
			return m, nil
		}
		if clickedTab, ok := m.tabAt(mx); ok {
			m.switchTab(clickedTab)
		}
		return m, nil
	}

	// Content area starts at row 3 (header=0, tabbar=1, separator=2).
	contentEndRow := contentStartRow + m.visibleRows()
	if my < contentStartRow || my >= contentEndRow {
		return m, nil
	}

	// A genuine content-area click (below the tab bar) pauses tail-following
	// on the Logs tab. Tab-bar clicks — including empty space and tab
	// switches — do NOT affect followLogs here, so switching to Logs via the
	// tab header keeps following enabled (see switchTab).
	if m.tab == tabLogs {
		m.followLogs = false
	}

	// Scrollbar column (rightmost): jump scroll and begin drag. The scrollbar
	// track is aligned with the scrollable data rows, below the fixed header
	// rows returned by contentHeaderRows().
	if mx == m.width-1 {
		contentHeight := m.maxCursor() + 1
		trackHeight := m.dataRows()
		headerRows := m.contentHeaderRows()
		trackStartRow := contentStartRow + headerRows
		if my < trackStartRow {
			return m, nil
		}
		relativeY := max(my-trackStartRow, 0)
		if relativeY >= trackHeight {
			relativeY = trackHeight - 1
		}
		// Clicking directly on the thumb should not jump; only track clicks
		// move the thumb to that proportional position. If the content fits the
		// viewport there is nothing to scroll, so don't start a drag.
		if sm := viewport.ScrollMax(contentHeight, trackHeight); sm > 0 {
			thumbTop := viewport.ThumbTop(m.scroll, contentHeight, trackHeight)
			thumbHeight := viewport.ThumbHeight(contentHeight, trackHeight)
			if relativeY < thumbTop || relativeY >= thumbTop+thumbHeight {
				m.scroll = viewport.ScrollFromThumb(relativeY, contentHeight, trackHeight)
			}
			m.dragging = true
		}
		m.dragStartY = relativeY
		m.dragStartScroll = m.scroll
		m.cursor = viewport.ClampCursor(
			m.scroll+min(trackHeight/2, m.maxCursor()-m.scroll),
			contentHeight)
		return m, nil
	}

	relativeRow := my - contentStartRow - m.contentHeaderRows()
	if relativeRow < 0 {
		return m, nil
	}
	m.cursor = viewport.CursorFromClick(relativeRow, m.scroll, m.maxCursor()+1)
	return m, nil
}

func (m Model) handleMouseWheel(msg tea.MouseWheelMsg) (Model, tea.Cmd) {
	switch msg.Mouse().Button {
	case tea.MouseWheelUp:
		m.moveCursor(-3)
	case tea.MouseWheelDown:
		m.moveCursor(3)
	}
	return m, nil
}

func (m Model) handleMouseMotion(msg tea.MouseMotionMsg) (Model, tea.Cmd) {
	if !m.dragging {
		return m, nil
	}
	if m.tab == tabLogs {
		m.followLogs = false
	}
	my := msg.Mouse().Y
	contentHeight := m.maxCursor() + 1
	trackHeight := m.dataRows()
	if viewport.ScrollMax(contentHeight, trackHeight) <= 0 {
		return m, nil
	}
	trackStartRow := contentStartRow + m.contentHeaderRows()
	if my < trackStartRow {
		my = trackStartRow
	}
	relativeY := max(my-trackStartRow, 0)
	if relativeY >= trackHeight {
		relativeY = trackHeight - 1
	}
	// Drag by delta from the grab point. The thumb occupies thumbHeight rows
	// so it can only travel (trackHeight - thumbHeight) rows. Dividing by this
	// dragRange ensures the scroll maps 1:1 to the thumb position, keeping the
	// thumb under the cursor and allowing the user to reach scrollMax.
	// Use int64 arithmetic to prevent overflow on large content heights
	// (matching the viewport package's approach).
	delta := relativeY - m.dragStartY
	sm := viewport.ScrollMax(contentHeight, trackHeight)
	thumbH := viewport.ThumbHeight(contentHeight, trackHeight)
	dragRange := trackHeight - thumbH
	if dragRange <= 0 {
		dragRange = trackHeight
	}
	scroll := m.dragStartScroll + int(int64(delta)*int64(sm)/int64(dragRange))
	m.scroll = viewport.ClampScroll(scroll, contentHeight, trackHeight)
	m.cursor = viewport.ClampCursor(
		m.scroll+min(trackHeight/2, contentHeight-1-m.scroll),
		contentHeight)
	return m, nil
}

func (m Model) handleMouseRelease(msg tea.MouseReleaseMsg) (Model, tea.Cmd) {
	m.dragging = false
	m.dragStartY = 0
	m.dragStartScroll = 0
	return m, nil
}

func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "ai-concurrency-shaper"

	if m.width == 0 || m.height == 0 {
		return v
	}

	if m.width < 1 || m.height < 4 {
		v.SetContent("")
		return v
	}

	m.updateScrollbars()

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteByte('\n')
	b.WriteString(m.renderTabBar())
	b.WriteByte('\n')
	b.WriteString(m.styles.sepStyle.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	switch m.mode {
	case modeDetail:
		overlay := m.renderDetailOverlay()
		b.WriteString(overlay)
		m.padLines(&b, countContentLines(overlay))
	case modeHelp:
		help := m.renderHelpOverlay()
		b.WriteString(help)
		m.padLines(&b, countContentLines(help))
	case modeConfirm:
		confirm := m.renderConfirmOverlay()
		b.WriteString(confirm)
		m.padLines(&b, countContentLines(confirm))
	default:
		content := m.renderContentWithScrollbar()
		b.WriteString(content)
		m.padLines(&b, countContentLines(content))
	}

	if m.mode == modeFilter && (m.tab == tabRequests || m.tab == tabNetwork || m.tab == tabLogs) {
		b.WriteString(m.styles.filterPromptStyle.Render(fmt.Sprintf(" Filter: %s█", m.filterText)))
	}

	// Place the footer on the next row without inserting a wasted blank row.
	// When the content/overlay/prompt already ends with a newline, no extra
	// newline is needed; otherwise add exactly one.
	if !builderEndsWithNewline(&b) {
		b.WriteByte('\n')
	}

	visible := toast.VisibleToasts(m.toasts)
	if len(visible) > 0 && m.mode == modeBrowse {
		// Toasts are ephemeral overlays; draw them just above the footer so
		// they temporarily cover the bottom of the scrollable pane instead of
		// permanently reserving a block of empty space below the footer.
		start := 0
		if len(visible) > 3 {
			start = len(visible) - 3
		}
		for i := start; i < len(visible); i++ {
			if toastStr := visible[i].Render(m.width, 1); toastStr != "" {
				if !builderEndsWithNewline(&b) {
					b.WriteByte('\n')
				}
				b.WriteString(toastStr)
			}
		}
	}

	// Footer is always the very last row, anchored to the terminal bottom.
	if !builderEndsWithNewline(&b) {
		b.WriteByte('\n')
	}
	b.WriteString(m.renderFooter())
	b.WriteString(redrawMarker(m.redrawEpoch))

	v.SetContent(b.String())
	return v
}

func (m *Model) padLines(b *strings.Builder, lines int) {
	visible := m.visibleRows()
	for i := lines; i < visible; i++ {
		b.WriteByte('\n')
	}
}

// countContentLines returns the number of row-separating newlines in s. A
// trailing newline does not introduce an extra row; it separates the last
// content line from whatever follows (e.g. the footer).
func countContentLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if strings.HasSuffix(s, "\n") {
		return n
	}
	return n + 1
}

// builderEndsWithNewline reports whether the builder is non-empty and its
// last byte is '\n'. This centralizes the check so there is a single point
// of change if a more efficient approach (e.g. tracking the last byte
// written) is needed later. strings.Builder.String() returns a zero-copy
// view of the underlying buffer via unsafe.String, so the lookup is both
// O(1) and allocation-free.
func builderEndsWithNewline(b *strings.Builder) bool {
	if b.Len() == 0 {
		return false
	}
	// This builder is only ever written to with valid UTF-8 strings and
	// single '\n' bytes, so reading the last byte to check for '\n' is safe.
	return b.String()[b.Len()-1] == '\n'
}

// providerName returns the display name for the active provider, prefixed with a
// single leading space. A single unnamed provider (and the zero Model) keep the
// legacy " ⚡ shaper" brand in the header.
func (m Model) providerName() string {
	if len(m.providers) == 0 {
		return " ⚡ shaper"
	}
	if len(m.providers) == 1 && m.providers[0].name == "" {
		return " ⚡ shaper"
	}
	return " " + m.providerLabel(m.active)
}

// providerLabel returns the bare display name of provider i, falling back to a
// stable "provider-N" label when it has no name.
func (m Model) providerLabel(i int) string {
	if i < 0 || i >= len(m.providers) {
		return ""
	}
	if name := m.providers[i].name; name != "" {
		return name
	}
	return fmt.Sprintf("provider-%d", i+1)
}

// hasSwitcher reports whether the header row renders provider chips: more than
// one provider, or a single provider with a name.
func (m Model) hasSwitcher() bool {
	return len(m.providers) > 1 || (len(m.providers) == 1 && m.providers[0].name != "")
}

// renderProviderSwitcher renders the provider chips shown on the right of the
// header in multi-provider mode. It returns "" when there is no switcher (the
// single unnamed provider — preserving the legacy header exactly — or when the
// width budget cannot fit even the active chip). The rendered string is exactly
// strings.Join(budgetedChips().parts, " ") — the same parts chipAt hit-tests,
// so what the user sees is what the user clicks.
func (m Model) renderProviderSwitcher() string {
	if !m.hasSwitcher() {
		return ""
	}
	layout := m.budgetedChips()
	return strings.Join(layout.parts, " ")
}

// headerBody renders the header's left side: provider identity and live
// counters, truncated to never exceed the usable row width on its own. It is
// shared by renderHeader and budgetedChips so the budget the chips degrade
// against is computed from the exact body the row displays. The '✗' (U+2717)
// and '⚡' (U+26A1) glyphs occupy 1 and 2 cells respectively.
//
// The truncation happens on the plain string, BEFORE headerStyle wraps it:
// truncateANSI would append ESC[0m inside the styled row and kill
// headerStyle's background for the remainder of the line.
//
// fleetSummary returns the one-line fleet aggregate observability strip
// (M6/G8) summarizing active, queued, open breaker counts, and the busiest
// provider across all configured providers.
func (m Model) fleetSummary() string {
	var totalActive, totalQueued int64
	var openBreakers int
	var maxActive int64 = -1
	var maxThroughput float64 = -1
	var busiest string

	for i, p := range m.providers {
		totalActive += p.snap.Active
		totalQueued += p.snap.Queued
		if p.snap.CircuitBreaker != nil && p.snap.CircuitBreaker.State == "OPEN" {
			openBreakers++
		}
		label := m.providerLabel(i)
		if p.snap.Active > maxActive || (p.snap.Active == maxActive && p.snap.Throughput > maxThroughput) {
			maxActive = p.snap.Active
			maxThroughput = p.snap.Throughput
			busiest = label
		}
	}
	if busiest == "" && len(m.providers) > 0 {
		busiest = m.providerLabel(0)
	}

	return fmt.Sprintf("Fleet: %d active · %d queued · %d OPEN · busiest: %s",
		totalActive, totalQueued, openBreakers, busiest)
}

func (m Model) headerBody(reserveForSwitcher bool) string {
	var body string
	if len(m.providers) > 1 {
		body = " " + m.fleetSummary()
	} else {
		uptime := time.Since(m.startTime).Truncate(time.Second)
		body = fmt.Sprintf("%s │ %d/%d active │ %d queued │ %.1f req/s │ %d ✗ TO │ uptime %s",
			m.providerName(), m.snap.Active, m.conc, m.snap.Queued, m.snap.Throughput,
			m.snap.TotalTimeout, uptime)
	}
	usable := max(m.width-2, 1)
	cap := usable
	if reserveForSwitcher && m.hasSwitcher() {
		// 1 gap + the active chip's floor. Any chip that fits beyond the
		// active one only shrinks this body further via budgetedChips'
		// budget computation, which uses this same reserve.
		cap -= 1 + chipFloor
		if cap < 1 {
			cap = 1
		}
	}
	if lipgloss.Width(body) > cap {
		body = truncatePlain(body, cap)
	}
	return body
}

// chipFloor is the smallest usable chip width: one visible label character
// plus the one-cell padding on each side of it.
const chipFloor = 3

// chipLayout is the outcome of one width-budgeting decision: the rendered chip
// parts in display (provider) order and, in lockstep, the provider index each
// part belongs to. parts and providers always have equal length; a nil parts
// means the switcher is elided for this width.
type chipLayout struct {
	parts     []string
	providers []int
}

// budgetedChips is the single source of truth for the provider switcher at a
// given width: renderHeader displays its parts and chipAt hit-tests exactly
// those parts, so the visible layout and the click targets cannot disagree.
//
// Chips are joined with single spaces and right-aligned in renderHeader; the
// usable content width is m.width-2 (headerStyle pads 1 cell each side) and 1
// more cell is reserved as the visual gap before the switcher:
//
//	budget := m.width - 2 - lipgloss.Width(headerBody(true)) - 1
//
// headerBody(true) itself reserves 1+chipFloor cells for the active chip, so
// budget >= chipFloor holds whenever the width can host any chip at all; a
// smaller budget elides the switcher (rule 3 below) and providerName carries
// the identity.
//
// Chips degrade to fit that budget, in order:
//
//  1. Shorten: chip labels are truncated to their allotted cells via
//     truncateANSI, preserving chip styling (the appended reset cannot leak
//     past the chip because each chip's styles re-open on the next chip).
//  2. Drop: trailing (leftmost-displayed) chips are dropped entirely.
//     Providers are never removed from the model — only from this row — and
//     Tab/Shift+Tab keep cycling the full set. The ACTIVE chip is never
//     dropped, so the user always sees which dashboard they are on; under a
//     tight budget that can mean evicting inactive chips that would otherwise
//     fit to its left.
//  3. Elide: if even the active chip at its floor width exceeds the budget,
//     the whole switcher is dropped and providerName carries the identity.
//
// Slack left after the floor widths are covered is restored to the chips'
// full rendered widths — active chip first, then the rest left-to-right — so
// the active chip's label is the most legible one on the row. A chip whose
// slack is fully restored renders byte-identically to an unbudgeted chip.
func (m Model) budgetedChips() chipLayout {
	if !m.hasSwitcher() {
		return chipLayout{}
	}
	budget := m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
	if budget < chipFloor {
		return chipLayout{}
	}

	labels := make([]string, len(m.providers))
	natural := make([]int, len(m.providers))
	for i := range m.providers {
		labels[i] = " " + m.providerLabel(i) + " "
		// natural is the chip's full RENDERED width — label plus the chip
		// style's 1+1 padding cells — not the bare label width, so a chip
		// whose slack is fully restored renders byte-identically to an
		// unbudgeted chip instead of being permanently truncated by the
		// two padding cells. Either chip style yields the same width here:
		// both add exactly one padding cell per side, and bold/colour
		// sequences carry no width.
		natural[i] = lipgloss.Width(m.styles.chipActiveStyle.Render(labels[i]))
	}

	// total returns the floor cost of exactly these providers as a chip row:
	// chipFloor per chip plus one gap between adjacent chips.
	total := func(keep []int) int {
		return len(keep)*chipFloor + (len(keep) - 1)
	}

	// Select kept chips right-to-left at floor width.
	keep := make([]int, 0, len(labels))
	prefix := func(i int, rest []int) []int {
		out := make([]int, 0, len(rest)+1)
		out = append(out, i)
		out = append(out, rest...)
		return out
	}
	kept := func(i int) bool {
		return slices.Contains(keep, i)
	}
	for i := range slices.Backward(labels) {
		candidate := prefix(i, keep)
		if total(candidate) <= budget {
			keep = candidate
			continue
		}
		if i != m.active {
			break // rule 2: trailing chip does not fit, stop walking left
		}
		// The active chip must be kept: evict the leftmost kept chips until
		// the row fits (the active chip wins over any number of inactive
		// ones to its left).
		for len(keep) > 0 && total(candidate) > budget {
			keep = keep[1:]
			candidate = prefix(i, keep)
		}
		if total(candidate) > budget {
			return chipLayout{} // unreachable: budget >= chipFloor
		}
		keep = candidate
	}
	// The walk can stop left of the active chip's position (rule 2 break),
	// leaving the ACTIVE provider unkept; insert it at its display position,
	// then evict inactive chips (from either end) until the row fits — the
	// active chip always wins.
	if !kept(m.active) {
		pos := 0
		for pos < len(keep) && keep[pos] < m.active {
			pos++
		}
		keep = append(keep[:pos], append([]int{m.active}, keep[pos:]...)...)
		for len(keep) > 1 && total(keep) > budget {
			// Evict the rightmost inactive chip; the active chip is never
			// dropped.
			last := len(keep) - 1
			if keep[last] == m.active {
				last = 0 // only the leftmost remains to evict
			}
			keep = append(keep[:last], keep[last+1:]...)
		}
		if total(keep) > budget {
			return chipLayout{} // unreachable: budget >= chipFloor
		}
	}

	// Restore slack toward natural widths: the active chip first, then the
	// remaining kept chips left-to-right.
	widths := make([]int, len(keep))
	for k := range keep {
		widths[k] = chipFloor
	}
	slack := budget - total(keep)
	order := make([]int, 0, len(keep))
	for k, i := range keep {
		if i == m.active {
			order = append(order, k)
		}
	}
	for k := range keep {
		if keep[k] != m.active {
			order = append(order, k)
		}
	}
	for _, k := range order {
		for slack > 0 && widths[k] < natural[keep[k]] {
			widths[k]++
			slack--
		}
	}

	parts := make([]string, len(keep))
	for k, i := range keep {
		if i == m.active {
			parts[k] = truncateANSI(m.styles.chipActiveStyle.Render(labels[i]), widths[k])
		} else {
			parts[k] = truncateANSI(m.styles.chipInactiveStyle.Render(labels[i]), widths[k])
		}
	}
	return chipLayout{parts: parts, providers: keep}
}

// chipAt maps a click column on header row 0 to a provider chip index,
// following the same right-aligned layout renderHeader displays: the row's
// content spans m.width-2 usable cells, so the last chip ends at column
// m.width-2 and each chip occupies [x-w+1, x] for its rendered width w. Only
// chips that survive the width budget are hit-testable — a dropped chip is
// invisible and must not be clickable.
func (m Model) chipAt(mx int) (int, bool) {
	if !m.hasSwitcher() {
		return 0, false
	}
	layout := m.budgetedChips()
	right := m.width - 2
	for i, v := range slices.Backward(layout.parts) {
		w := lipgloss.Width(v)
		if mx >= right-w+1 && mx <= right {
			return layout.providers[i], true
		}
		right -= w + 1
	}
	return 0, false
}

func (m Model) renderHeader() string {
	// With a switcher, render with the reserved body so the truncated body
	// the chips were budgeted against is the one displayed; without one,
	// render the natural body capped at the usable width (single-provider
	// rows keep their legacy content; only a too-narrow terminal truncates).
	body := m.headerBody(m.hasSwitcher())
	if switcher := m.renderProviderSwitcher(); switcher != "" {
		// Right-align the switcher inside the header content: headerStyle
		// adds 1 cell of padding on each side, so the usable body width is
		// m.width-2; the extra cell is a visual gap before the chips.
		if pad := m.width - lipgloss.Width(body) - lipgloss.Width(switcher) - 3; pad > 0 {
			body += strings.Repeat(" ", pad)
		}
		body += " " + switcher
	}
	return m.styles.headerStyle.Render(body)
}

// renderTab renders a single tab label using the theme. The selected tab
// (tabID(i) == m.tab) uses tabActiveStyle; all others use tabInactiveStyle.
// Both styles share the same horizontal box model (PaddingLeft(1).PaddingRight(1)),
// differing only in color and weight, so the rendered widths are equal for a
// given label — but the measurement MUST come from the same code path that
// renderTabBar uses, so hit-testing (tabAt) tracks the exact pixels on screen.
func (m Model) renderTab(i int, name string) string {
	if tabID(i) == m.tab {
		return m.styles.tabActiveStyle.Render(" " + name + " ")
	}
	return m.styles.tabInactiveStyle.Render(" " + name + " ")
}

func (m Model) renderTabBar() string {
	parts := make([]string, len(tabNames))
	for i, name := range tabNames {
		parts[i] = m.renderTab(i, name)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// tabAt returns which tab, if any, occupies cell x on the tab bar row.
// It measures each tab's visible cell width as rendered by the current theme
// (padding included) so hit-testing tracks the exact pixels the user sees,
// not a ratio of terminal width. The tab bar is left-aligned at x=0 with no
// reflow; cells beyond the total bar width are empty space and return !ok.
func (m Model) tabAt(x int) (tabID, bool) {
	if x < 0 {
		return 0, false
	}
	offset := 0
	for i, name := range tabNames {
		// Measure using renderTab so hit-testing tracks the exact rendered
		// geometry — the selected tab uses tabActiveStyle, others use
		// tabInactiveStyle. Both share the same padding, but rendering must
		// never hard-code one style for all tabs.
		rendered := m.renderTab(i, name)
		w := uniseg.StringWidth(stripANSI(rendered))
		if w <= 0 {
			// Defensive fallback: if a style is zero-value (Render returns the
			// raw input), the width is len(content)+2 padding = name+4.
			// This branch is effectively unreachable because " "+name+" " is
			// always at least 3 cells, but guards against impossible states.
			w = uniseg.StringWidth(name) + 4
		}

		if x >= offset && x < offset+w {
			return tabID(i), true
		}
		offset += w
	}
	return 0, false
}
func (m Model) renderContent() string {
	switch m.tab {
	case tabDashboard:
		return m.renderDashboardContent()
	case tabRequests:
		return m.renderRequests()
	case tabNetwork:
		return m.renderNetwork()
	case tabLogs:
		return m.renderLogs()
	case tabConcurrency:
		return m.renderConcurrency()
	case tabRoutes:
		return m.renderRoutes()
	}
	return ""
}

// renderDashboardContent returns the portion of the dashboard that is visible
// in the current viewport, using m.scroll as the top offset.
func (m Model) renderDashboardContent() string {
	lines := m.cachedDashboardLines()
	visible := m.visibleRows()
	start := max(min(m.scroll, len(lines)), 0)
	end := min(start+visible, len(lines))
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n") + "\n"
}

func (m Model) renderSparkline() string {
	spark := m.snap.Sparkline
	if len(spark) == 0 {
		return m.styles.dimStyle2.Render("  —")
	}
	maxVal := 0
	for _, v := range spark {
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == 0 {
		maxVal = 1
	}
	chars := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	var line strings.Builder
	line.WriteString("  ")
	for _, v := range spark {
		idx := max(int(float64(v)/float64(maxVal)*float64(len(chars)-1)), 0)
		if idx >= len(chars) {
			idx = len(chars) - 1
		}
		line.WriteString(chars[idx])
	}
	style := m.sparklineFillStyle(spark[len(spark)-1], maxVal)
	return style.Render(line.String())
}

func (m Model) sparklineFillStyle(last, max int) lipgloss.Style {
	if max <= 0 {
		return m.styles.sparklineStyle
	}
	pct := min(int(math.Round(float64(last)/float64(max)*100)), 100)
	switch {
	case pct >= 90:
		return m.styles.gaugeCriticalStyle
	case pct >= 60:
		return m.styles.gaugeWarnStyle
	default:
		return m.styles.sparklineStyle
	}
}

// renderStatusBar renders the HTTP status distribution as a stacked
// bar with count labels. The width parameter is a maximum bar track
// hint; the function reduces the track as needed so the full status
// line — the 10-cell "  Status  " prefix added by the caller, the
// bracketed bar, the inline labels, and the trailing spaces — fits
// within the viewport. When the viewport is too narrow for the labels
// to share the bar's line, the labels wrap onto one or more additional
// rows, packed so that every row fits the viewport.
//
// Zero-count status classes are omitted from both the bar segments
// and the labels (StatusCounts only ever increments, so negative
// counts cannot occur; non-positive classes are skipped regardless).
// Counts of 10^7 and above are abbreviated via formatCount so the
// labels stay short even at extreme magnitudes. The returned slice
// contains one or more single-line strings, never an embedded newline.
func (m Model) renderStatusBar(width int) []string {
	width = max(width, 0)
	vw := m.viewportWidth()

	counts := m.snap.StatusCounts
	total := counts[1] + counts[2] + counts[3] + counts[4] + counts[5]

	labels := []string{"1xx", "2xx", "3xx", "4xx", "5xx"}
	cvalues := []int64{counts[1], counts[2], counts[3], counts[4], counts[5]}
	colors := []lipgloss.Style{m.styles.statusInfoStyle, m.styles.statusOkStyle, m.styles.statusRedirectStyle, m.styles.statusClientErrStyle, m.styles.statusServerErrStyle}

	// The rendered labels are exactly the non-zero classes, matching
	// the bar segments; budget the same set so the width math matches
	// what is printed.
	var labelParts []string
	for i, v := range cvalues {
		if v <= 0 {
			continue
		}
		labelParts = append(labelParts, colors[i].Render(fmt.Sprintf("%s:%s", labels[i], formatCount(v))))
	}
	var labelsWidth int
	for _, p := range labelParts {
		labelsWidth += 1 + uniseg.StringWidth(stripANSI(p)) // leading space
	}
	var abortedPart string
	if m.snap.TotalAborted > 0 {
		abortedPart = m.styles.gaugeWarnStyle.Render("Aborted:" + formatCount(m.snap.TotalAborted))
		labelsWidth += 1 + uniseg.StringWidth(stripANSI(abortedPart))
	}

	// Full status line: prefix=10 + brackets=2 + trailing=2.
	// The bar track is reduced so the line fits within the viewport.
	barWidth := min(width, max(vw-labelsWidth-14, 0))
	wrap := (len(labelParts) > 0 || abortedPart != "") && labelsWidth > vw-14
	if wrap {
		// The labels cannot share the bar's line even at zero track
		// width; give the bar as much room as the line allows and
		// wrap the labels onto their own line(s).
		barWidth = min(width, max(vw-14, 0))
	}

	var b strings.Builder
	b.WriteString("[")
	if total == 0 {
		b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", barWidth)))
	} else {
		pos := 0
		for i, v := range cvalues {
			if v <= 0 {
				continue
			}
			seg := int(math.Round(float64(v) / float64(total) * float64(barWidth)))
			if seg == 0 {
				seg = 1
			}
			if pos+seg > barWidth {
				seg = barWidth - pos
			}
			b.WriteString(colors[i].Render(strings.Repeat("█", seg)))
			pos += seg
		}
		if pos < barWidth {
			b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", barWidth-pos)))
		}
	}
	b.WriteString("]")

	if wrap {
		b.WriteString("  ")
		// The labels cannot share the bar's line; pack them into one or
		// more rows that fit the viewport: each row is the parts joined
		// with single spaces within a viewportWidth-2 budget, indented
		// by two cells. A single part is at most 13 cells (8 + a
		// 5-cell abbreviated count, e.g. "Aborted:1000M"), far below
		// the budget for viewports of 40 columns and wider, so no part
		// is ever split or dropped; the Aborted label is joined into
		// the same set so the packer treats it uniformly with the
		// status classes.
		parts := make([]string, 0, len(labelParts)+1)
		parts = append(parts, labelParts...)
		if abortedPart != "" {
			parts = append(parts, abortedPart)
		}
		rows := packParts(parts, max(vw-2, 0), " ")
		lines := make([]string, 0, len(rows)+1)
		lines = append(lines, b.String())
		for _, row := range rows {
			lines = append(lines, "  "+row)
		}
		return lines
	}

	for _, p := range labelParts {
		b.WriteString(" ")
		b.WriteString(p)
	}
	if abortedPart != "" {
		b.WriteString(" ")
		b.WriteString(abortedPart)
	}
	b.WriteString("  ")
	return []string{b.String()}
}

// formatCount renders a non-negative counter for display, abbreviating
// magnitudes of 10^7 and above with a single decimal and an M/B/T/P/E
// suffix so the abbreviated display never exceeds 5 cells (e.g.
// 12,345,678 -> "12.3M", 12,345,678,901 -> "12.3B"). Counts below
// 10^7 render exactly. Counters only ever increment, so negative
// values cannot occur.
func formatCount(v int64) string {
	if v < 10_000_000 {
		return strconv.FormatInt(v, 10)
	}
	units := []string{"M", "B", "T", "P", "E"}
	scale := int64(1_000_000)
	for i, u := range units {
		// v < scale*1000 without overflow (scale is 10^18 at i=4,
		// where the comparison always holds for int64).
		if v/1000 < scale || i == len(units)-1 {
			scaled := float64(v) / float64(scale)
			if scaled >= 99.95 { // would round to 100.0+ with one decimal
				return fmt.Sprintf("%d%s", int64(math.Round(scaled)), u)
			}
			return fmt.Sprintf("%.1f%s", scaled, u)
		}
		scale *= 1000
	}
	return strconv.FormatInt(v, 10)
}

// packParts packs label:value parts into rows joined by sep so that
// every row fits within vw cells, starting a new row whenever the next
// part would overflow. Each part must itself fit within vw; parts are
// never split across rows. Widths are measured on the ANSI-stripped
// content, so escape sequences in parts do not count toward the row
// width.
func packParts(parts []string, vw int, sep string) []string {
	var rows []string
	var b strings.Builder
	width := 0
	sepWidth := uniseg.StringWidth(stripANSI(sep))
	for _, p := range parts {
		extra := 0
		if width > 0 {
			extra = sepWidth
		}
		if width+extra+uniseg.StringWidth(stripANSI(p)) > vw {
			if width > 0 {
				rows = append(rows, b.String())
			}
			b.Reset()
			width = 0
			extra = 0
		}
		if extra > 0 {
			b.WriteString(sep)
		}
		b.WriteString(p)
		width += extra + uniseg.StringWidth(stripANSI(p))
	}
	if width > 0 {
		rows = append(rows, b.String())
	}
	return rows
}

// dashboardLines builds the full list of rendered lines for the Dashboard tab.
// It always renders all content (including up to six in-flight requests);
// renderDashboardContent() windows these lines by m.scroll and m.visibleRows().
func (m Model) dashboardLines() []string {
	var lines []string

	if cb := m.snap.CircuitBreaker; cb != nil {
		lines = append(lines, m.styles.sectionStyle.Render(" Circuit Breaker "))
		var stateStyle lipgloss.Style
		switch cb.State {
		case "CLOSED":
			stateStyle = m.styles.circuitClosedStyle
		case "OPEN":
			stateStyle = m.styles.circuitOpenStyle
		case "HALF_OPEN":
			stateStyle = m.styles.circuitHalfOpenStyle
		default:
			stateStyle = lipgloss.NewStyle()
		}
		// The breaker summary is packed into rows that fit the viewport
		// (same mechanism as the Summary metrics): parts joined by
		// "  |  " within a viewportWidth-2 budget, two cells of indent
		// per row.
		cbParts := []string{"State: " + stateStyle.Render(cb.State)}
		cbParts = append(cbParts,
			"Failures: "+formatCount(cb.Failures),
			"Consecutive: "+formatCount(cb.ConsecutiveFailures))
		if cb.CurrentPenalty > 0 {
			cbParts = append(cbParts, "Penalty: "+cb.CurrentPenalty.Truncate(time.Millisecond).String())
		}
		if !cb.NextRetry.IsZero() {
			if until := time.Until(cb.NextRetry).Truncate(time.Millisecond); until > 0 {
				cbParts = append(cbParts, "Next probe: "+until.String())
			}
		}
		for _, row := range packParts(cbParts, max(m.viewportWidth()-2, 0), "  |  ") {
			lines = append(lines, "  "+row)
		}
		lines = append(lines, "")
	}
	lines = append(lines, m.styles.sectionStyle.Render(" Throughput (10s) "))
	lines = append(lines, m.renderSparkline())
	lines = append(lines, "")

	gaugeWidth := m.gaugeTrackWidth()

	lines = append(lines, m.renderDualBars(gaugeWidth))

	if m.snap.RetriesInFlight > 0 {
		lines = append(lines, fmt.Sprintf("  %d active retries", m.snap.RetriesInFlight))
	}

	lines = append(lines, "")
	statusLines := m.renderStatusBar(gaugeWidth)
	lines = append(lines, m.styles.sectionStyle.Render("  Status  ")+statusLines[0])
	lines = append(lines, statusLines[1:]...)

	lines = append(lines, "")
	lines = append(lines, m.styles.sectionStyle.Render(" In-Flight Requests "))
	flights := m.snap.InFlight
	// The summary renders as a single line whenever the abbreviated
	// counts fit the viewport (always at width >= 80: the widest single
	// line form, with 5-cell "1000M"-style counts, is at most 51 cells).
	// When it cannot fit — narrow viewports with multi-digit counts —
	// the three parts pack into rows within a viewportWidth-2 budget,
	// indented by two cells, so "passthrough" is never silently
	// truncated by renderContentWithScrollbar.
	summary := fmt.Sprintf("  %s in-flight: %s limited, %s passthrough",
		formatCount(int64(len(flights))), formatCount(m.snap.InFlightLimited), formatCount(m.snap.InFlightPassthrough))
	if uniseg.StringWidth(summary) <= m.viewportWidth() {
		lines = append(lines, summary)
	} else {
		parts := []string{
			formatCount(int64(len(flights))) + " in-flight",
			formatCount(m.snap.InFlightLimited) + " limited",
			formatCount(m.snap.InFlightPassthrough) + " passthrough",
		}
		for _, row := range packParts(parts, max(m.viewportWidth()-2, 0), "  ") {
			lines = append(lines, "  "+row)
		}
	}
	// The path column shrinks per row so the row always fits the
	// viewport: the fixed overhead is the indent, tag, method, and
	// age, of which only the last two vary — the age renders up to
	// 18 cells for extreme durations (e.g. "2562047h47m16.854s")
	// and %-6s pads the method but never truncates it (OPTIONS is 7
	// cells) — so no fixed overhead heuristic can guarantee the fit.
	// A fixed 23-cell assumption (15 fixed + 8-cell age) overflows
	// for multi-hour ages: row = 15 + pathWidth + ageWidth with
	// pathWidth = vw-23 exceeds vw whenever ageWidth > 8 (e.g.
	// 41 > 39 at width 40 for "1h2m3.004s"). Instead each row's path
	// width is derived from that row's actual overhead: row = fixed +
	// pathWidth <= viewportWidth whenever the overhead itself fits
	// the viewport, which always holds at the supported widths
	// (40/80/120: the widest overhead, an 18-cell age with OPTIONS,
	// is 34 cells, leaving a 5-cell path at width 40), since
	// truncate limits ASCII paths to pathWidth cells and %-*s pads
	// to exactly pathWidth.
	show := min(len(flights), 6)
	for i := range show {
		r := flights[i]
		age := r.Age().Truncate(time.Millisecond)
		tag := m.styles.limitedTag
		if !r.Limited {
			tag = m.styles.passTag
		}
		fixed := 2 + uniseg.StringWidth(stripANSI(tag)) + 1 + max(6, uniseg.StringWidth(r.Method)) + 1 + 1 + uniseg.StringWidth(age.String())
		pathWidth := min(35, max(m.viewportWidth()-fixed, 0))
		lines = append(lines, fmt.Sprintf("  %s %-6s %-*s %s", tag, r.Method, pathWidth, truncate(r.Path, pathWidth), age))
	}
	if len(flights) > show {
		lines = append(lines, fmt.Sprintf("  … and %d more", len(flights)-show))
	}

	lines = append(lines, "")
	lines = append(lines, m.styles.sectionStyle.Render(" Summary "))
	// The Summary metrics are packed into rows that fit the viewport:
	// "Label: count" parts joined by "  │  ", each row indented by two
	// cells. A single part is at most 26 cells (the longest label,
	// "Clean passthrough", plus ": " and a 7-digit exact count), so
	// every rendered row provably fits viewports of 28 cells or more,
	// and any two parts (with the 5-cell separator) fit within a
	// 57-cell budget, so two metrics per row fit at viewports of 59
	// cells or more; the packer therefore renders multiple metrics per
	// row at standard widths and one or two per row at 40 columns.
	summaryMetrics := []struct {
		label string
		value int64
	}{
		{"Clean proxied", m.snap.TotalProxied},
		{"Clean passthrough", m.snap.TotalPassThrough},
		{"Aborted", m.snap.TotalAborted},
		{"Timeouts", m.snap.TotalTimeout},
		{"Cancelled", m.snap.TotalCancelled},
		{"Circuit rejects", m.snap.TotalCircuitRejected},
	}
	summaryParts := make([]string, 0, len(summaryMetrics))
	for _, sm := range summaryMetrics {
		summaryParts = append(summaryParts, sm.label+": "+formatCount(sm.value))
	}
	for _, row := range packParts(summaryParts, max(m.viewportWidth()-2, 0), "  \u2502  ") {
		lines = append(lines, "  "+row)
	}

	return lines
}

// cachedDashboardLines returns the dashboard lines for the current Update
// cycle, building them lazily on first access. The cache is reset only by
// data-mutating messages (metrics.Snapshot), terminal resizes
// (tea.WindowSizeMsg), and tab switches, so the expensive formatting work is
// skipped for high-frequency input messages such as mouse motion.
func (m *Model) cachedDashboardLines() []string {
	if m.dashboardLinesCache != nil {
		return m.dashboardLinesCache
	}
	m.dashboardLinesCache = m.dashboardLines()
	return m.dashboardLinesCache
}

func (m Model) renderRequests() string {
	var b strings.Builder
	entries := m.visibleEntries()

	if len(entries) == 0 {
		if m.filterText != "" {
			fmt.Fprintf(&b, "  No requests matching %q\n", m.filterText)
		} else {
			b.WriteString("  No requests yet.\n")
		}
		return b.String()
	}

	if m.filterText != "" {
		fmt.Fprintf(&b, "  Filter: %q  (%d / %d entries)\n", m.filterText, len(entries), len(m.snap.LogEntries))
	}

	b.WriteString("  ")
	b.WriteString(m.styles.tableHeaderStyle.Render(
		fmt.Sprintf("%-8s %-6s %4s  %9s  %s", "Time", "Method", "St", "Duration", "Path")))
	b.WriteByte('\n')

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(entries))

	for i := start; i < end; i++ {
		e := entries[i]
		style := m.styles.rowStyle
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}
		stStr := m.statusStyle(e.Status).Render(fmt.Sprintf("%4d", e.Status))
		path := e.Path
		if e.Aborted {
			path += " [aborted]"
		}
		line := fmt.Sprintf("%-8s %-6s %s  %9s  %s",
			e.Time.Format("15:04:05"), e.Method, stStr,
			e.Duration.Truncate(time.Millisecond), path)
		b.WriteString(style.Render("  " + line))
		b.WriteByte('\n')
	}

	// Count line is always emitted for the non-empty state; it is part of the
	// fixed rows reserved by dataRows().
	fmt.Fprintf(&b, "  %d-%d / %d entries\n", start+1, end, len(entries))
	return b.String()
}

func (m Model) renderNetwork() string {
	var b strings.Builder
	entries := m.visibleNetworkEntries()

	// Filter indicators.
	filters := ""
	if m.networkFilterType != networkFilterAll {
		typeLabels := []string{"all", "json", "html", "events", "other"}
		filters += fmt.Sprintf(" [type:%s]", typeLabels[m.networkFilterType])
	}
	if m.networkFilterStatus != networkStatusAll {
		statusLabels := []string{"all", "2xx", "4xx", "5xx"}
		filters += fmt.Sprintf(" [status:%s]", statusLabels[m.networkFilterStatus])
	}
	if filters != "" {
		fmt.Fprintf(&b, "  Filters:%s\n", filters)
	}

	// Column header (always shown).
	b.WriteString("  ")
	b.WriteString(m.styles.tableHeaderStyle.Render(
		fmt.Sprintf("%-22s %-6s %4s  %-6s %7s  %8s  %s",
			"Name", "Method", "St", "Type", "Size", "Time", "Waterfall")))
	b.WriteByte('\n')

	if len(entries) == 0 {
		if m.filterText != "" {
			fmt.Fprintf(&b, "  No entries matching %q\n", m.filterText)
		} else {
			b.WriteString("  No network entries yet.\n")
		}
		return b.String()
	}

	if m.filterText != "" {
		fmt.Fprintf(&b, "  Filter: %q  (%d / %d entries)\n", m.filterText, len(entries), m.journal.Len())
	}

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(entries))

	for i := start; i < end; i++ {
		e := entries[i]
		style := m.styles.rowStyle
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}

		name := truncate(e.Name(), 22)
		stStr := m.networkStatusStyle(e.StatusCode).Render(fmt.Sprintf("%4d", e.StatusCode))
		typeStr := e.Type()
		if e.Aborted {
			typeStr = "abort"
		}
		sizeStr := e.SizeLabel()
		timeStr := e.Timing.Duration().Truncate(time.Millisecond).String()
		waterfall := m.renderWaterfall(e)

		line := fmt.Sprintf("%-22s %-6s %s  %-6s %7s  %8s  %s",
			name, e.Method, stStr, typeStr, sizeStr, timeStr, waterfall)
		b.WriteString(style.Render("  " + line))
		b.WriteByte('\n')
	}

	// Count line always emitted for the non-empty state; it is part of the
	// fixed rows reserved by dataRows().
	fmt.Fprintf(&b, "  %d-%d / %d entries\n", start+1, end, len(entries))
	return b.String()
}

// renderLogs renders the dedicated Logs tab showing captured log output.
func (m Model) renderLogs() string {
	var b strings.Builder
	lines := m.visibleLogLines()

	if len(lines) == 0 {
		if m.filterText != "" {
			fmt.Fprintf(&b, "  No log lines matching %q\n", m.filterText)
		} else {
			b.WriteString("  No log output yet.\n")
		}
		return b.String()
	}

	if m.filterText != "" {
		fmt.Fprintf(&b, "  Filter: %q  (%d / %d lines)\n", m.filterText, len(lines), m.logRing.Len())
	}

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(lines))

	for i := start; i < end; i++ {
		style := m.styles.rowStyle
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}
		b.WriteString(style.Render(fmt.Sprintf("  %6d  ", i+1) + lines[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderWaterfall renders a mini timing bar for a single entry.
// The bar shows: [queue|ttfb|download] as colored segments.
func (m Model) renderWaterfall(e *journal.Entry) string {
	total := e.Timing.Duration()
	if total <= 0 {
		return "·"
	}

	// Available width for the waterfall bar.
	barWidth := 20
	if m.width > 100 {
		barWidth = 30
	}

	queue := e.Timing.QueueDuration()
	ttfb := e.Timing.TTFB()

	queueSeg := min(int(math.Round(float64(queue)/float64(total)*float64(barWidth))), barWidth)
	ttfbSeg := int(math.Round(float64(ttfb) / float64(total) * float64(barWidth)))
	if queueSeg+ttfbSeg > barWidth {
		ttfbSeg = barWidth - queueSeg
	}
	downloadSeg := max(barWidth-queueSeg-ttfbSeg, 0)

	var b strings.Builder
	if queueSeg > 0 {
		b.WriteString(m.styles.waterfallQueueStyle.Render(strings.Repeat("█", queueSeg)))
	}
	if ttfbSeg > 0 {
		b.WriteString(m.styles.waterfallTTFBStyle.Render(strings.Repeat("█", ttfbSeg)))
	}
	if downloadSeg > 0 {
		b.WriteString(m.styles.waterfallDownloadStyle.Render(strings.Repeat("█", downloadSeg)))
	}
	return b.String()
}

func (m Model) networkStatusStyle(code int) lipgloss.Style {
	switch {
	case code >= 200 && code < 300:
		return m.styles.statusOkStyle
	case code >= 300 && code < 400:
		return m.styles.statusRedirectStyle
	case code >= 400 && code < 500:
		return m.styles.statusClientErrStyle
	case code >= 500:
		return m.styles.statusServerErrStyle
	default:
		return m.styles.dimStyle2
	}
}

func (m Model) renderConcurrency() string {
	var b strings.Builder

	b.WriteString(m.styles.sectionStyle.Render(" Concurrency Gauge "))
	b.WriteByte('\n')
	b.WriteString(m.renderGaugeBar(int(m.snap.Active), m.conc, m.gaugeBarWidth()))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  %d / %d active  │  %d queued  │  %.1f req/s\n",
		m.snap.Active, m.conc, m.snap.Queued, m.snap.Throughput)

	oldestAge := m.oldestQueuedAge()
	if m.snap.Queued > 0 {
		fmt.Fprintf(&b, "  Oldest queued: %s\n", oldestAge.Truncate(time.Millisecond))
	} else {
		b.WriteString("  Oldest queued: —\n")
	}
	b.WriteByte('\n')

	b.WriteString(m.styles.sectionStyle.Render(" Queue Depth "))
	b.WriteByte('\n')
	queueMax := m.conc * 4
	if queueMax == 0 {
		queueMax = 1
	}
	b.WriteString(m.renderHBar(int(m.snap.Queued), queueMax, m.hBarWidth(), m.queueFillStyle(int(m.snap.Queued), queueMax)))
	b.WriteByte('\n')
	if m.snap.Queued == 0 {
		b.WriteString(m.styles.dimStyle2.Render("  Queue: empty\n"))
	} else {
		fmt.Fprintf(&b, "  %d waiting\n", m.snap.Queued)
	}
	b.WriteByte('\n')

	b.WriteString(m.styles.sectionStyle.Render(" In-Flight Requests "))
	b.WriteByte('\n')
	flights := m.snap.InFlight
	if len(flights) == 0 {
		b.WriteString(m.styles.dimStyle2.Render("  No requests in flight.\n"))
		return b.String()
	}

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(flights))

	for i := start; i < end; i++ {
		r := flights[i]
		style := m.styles.rowStyle
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}
		age := r.Age().Truncate(time.Millisecond)
		totalAge := r.TotalAge().Truncate(time.Millisecond)
		tag := m.styles.limitedTag
		if !r.Limited {
			tag = m.styles.passTag
		}
		line := fmt.Sprintf("  %s %-6s %-35s age=%s  total=%s",
			tag, r.Method, r.Path, age, totalAge)
		b.WriteString(style.Render(line))
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Model) oldestQueuedAge() time.Duration {
	return m.snap.OldestQueuedAge
}

func (m Model) perRouteRate() map[string]float64 {
	rates := make(map[string]float64)
	cutoff := time.Now().Add(-10 * time.Second)
	counts := make(map[string]int)
	for _, e := range m.snap.LogEntries {
		if e.Time.After(cutoff) {
			key := e.Method + " " + e.Path
			counts[key]++
		}
	}
	windowStart := time.Now() // will be set to the oldest entry in the window
	hasEntry := false
	for _, e := range m.snap.LogEntries {
		if e.Time.After(cutoff) {
			if !hasEntry || e.Time.Before(windowStart) {
				windowStart = e.Time
			}
			hasEntry = true
		}
	}
	elapsed := 10.0 // fixed 10-second window
	if hasEntry {
		elapsed = time.Since(windowStart).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
	}
	for k, v := range counts {
		rates[k] = float64(v) / elapsed
	}
	return rates
}

func (m Model) renderRoutes() string {
	var b strings.Builder
	stats := m.snap.RouteStats
	if len(stats) == 0 {
		b.WriteString("  No route data yet.\n")
		return b.String()
	}

	type routePair struct {
		key  string
		stat metrics.RouteStat
	}
	pairs := make([]routePair, 0, len(stats))
	for k, v := range stats {
		pairs = append(pairs, routePair{k, v})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].stat.Total != pairs[j].stat.Total {
			return pairs[i].stat.Total > pairs[j].stat.Total
		}
		return pairs[i].key < pairs[j].key
	})

	rates := m.perRouteRate()

	b.WriteString("  ")
	b.WriteString(m.styles.tableHeaderStyle.Render(
		fmt.Sprintf("%-32s %5s %5s %5s %5s %5s %7s", "Route", "Total", "2xx", "4xx", "5xx", "✗ TO", "req/s")))
	b.WriteByte('\n')

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(pairs))

	for i := start; i < end; i++ {
		p := pairs[i]
		style := m.styles.rowStyle
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}
		s := p.stat
		rate := rates[p.key]
		line := fmt.Sprintf("%-32s %5d %5d %5d %5d %5d %7.1f",
			p.key, s.Total, s.Statuses[2], s.Statuses[4], s.Statuses[5], s.Timeouts, rate)
		b.WriteString(style.Render("  " + line))
		b.WriteByte('\n')
	}

	// Count line always emitted for the non-empty state; it is part of the
	// fixed rows reserved by dataRows().
	fmt.Fprintf(&b, "  %d-%d / %d routes\n", start+1, end, len(pairs))
	return b.String()
}

func (m Model) resetCmd() tea.Cmd {
	return func() tea.Msg {
		return resetMsg{}
	}
}

type resetMsg struct{}

func (m Model) renderConfirmOverlay() string {
	return m.styles.overlayStyle.Render(
		fmt.Sprintf(" Reset Stats \n\n"+
			" Clear all cumulative counters?\n"+
			" (Proxied, Passthrough, Timeouts, etc.)\n\n"+
			" y = yes    n/Esc = no")) + "\n"
}

func (m Model) renderDetailOverlay() string {
	var b strings.Builder

	switch m.tab {
	case tabRequests:
		entries := m.visibleEntries()
		if m.cursor >= len(entries) {
			return ""
		}
		e := entries[m.cursor]
		b.WriteString(m.styles.overlayStyle.Render(
			fmt.Sprintf(" Request Detail \n"+
				" Time:     %s\n"+
				" Method:   %s\n"+
				" Path:     %s\n"+
				" Status:   %d\n"+
				" Duration: %s\n"+
				" Limited:  %v\n"+
				"\n [Esc/Enter] close ",
				e.Time.Format("15:04:05.000"), e.Method, e.Path,
				e.Status, e.Duration.Truncate(time.Millisecond), e.Limited)))

	case tabNetwork:
		entries := m.visibleNetworkEntries()
		if m.cursor >= len(entries) {
			return ""
		}
		e := entries[m.cursor]
		b.WriteString(m.renderNetworkDetail(e))

	case tabConcurrency:
		if m.cursor >= len(m.snap.InFlight) {
			return ""
		}
		r := m.snap.InFlight[m.cursor]
		b.WriteString(m.styles.overlayStyle.Render(
			fmt.Sprintf(" In-Flight Detail \n"+
				" ID:       %d\n"+
				" Method:   %s\n"+
				" Path:     %s\n"+
				" Limited:  %v\n"+
				" Age:      %s\n"+
				" Total:    %s\n"+
				"\n [Esc/Enter] close ",
				r.ID, r.Method, r.Path, r.Limited,
				r.Age().Truncate(time.Millisecond),
				r.TotalAge().Truncate(time.Millisecond))))
	}

	if !builderEndsWithNewline(&b) {
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Model) renderNetworkDetail(e *journal.Entry) string {
	// Compute a line budget so the overlay fits within the terminal.
	// The overlay is drawn inside the scrollable content area; its border and
	// padding consume 4 rows. Reserve at least a minimum usable detail view.
	budget := max(m.visibleRows()-4, 10)

	// Count fixed lines that are always emitted (minimum 17):
	//   Request heading, Method, URL, [blank],
	//   Response heading, Status, Type, Size, [blank],
	//   Timing heading, Queue, TTFB, Download, Total, Waterfall,
	//   [blank], close
	const fixedLines = 17

	// Remaining budget for variable sections: headers and body previews.
	varBudget := max(budget-fixedLines,
		// at least 2 lines each for req/resp headers
		4)

	// Split the variable budget: half for request, half for response.
	reqBudget := varBudget / 2
	respBudget := varBudget - reqBudget

	var b strings.Builder

	b.WriteString(m.styles.sectionStyle.Render(" Request "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Method:   %s\n", e.Method)
	fmt.Fprintf(&b, " URL:      %s\n", e.URL)
	usedReq := 3 // heading + method + url

	if len(e.RequestHeaders) > 0 && usedReq < reqBudget {
		keys := sortedHeaderKeys(e.RequestHeaders)
		b.WriteString(" Headers:\n")
		usedReq++ // "Headers:" line
		maxHeaderLines := max(
			// -1 for potential body line
			reqBudget-usedReq-1, 1)
		shown := 0
		for _, k := range keys {
			if shown >= maxHeaderLines {
				fmt.Fprintf(&b, "   … and %d more\n", len(keys)-shown)
				usedReq++
				break
			}
			fmt.Fprintf(&b, "   %s: %s\n", k, strings.Join(e.RequestHeaders[k], ", "))
			shown++
			usedReq++
		}
	}
	if len(e.RequestBody) > 0 && usedReq < reqBudget {
		preview := truncateBytes(e.RequestBody, 256)
		fmt.Fprintf(&b, " Body:     %s\n", preview)
		usedReq++
	}
	b.WriteByte('\n')

	b.WriteString(m.styles.sectionStyle.Render(" Response "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Status:   %d\n", e.StatusCode)
	if e.Aborted {
		b.WriteString(" Outcome:  aborted\n")
	}
	fmt.Fprintf(&b, " Type:     %s\n", e.Type())
	fmt.Fprintf(&b, " Size:     %s\n", e.SizeLabel())
	usedResp := 4 // heading + status + type + size
	if e.Aborted {
		usedResp++
	}

	if len(e.ResponseHeaders) > 0 && usedResp < respBudget {
		keys := sortedHeaderKeys(e.ResponseHeaders)
		b.WriteString(" Headers:\n")
		usedResp++
		maxHeaderLines := max(respBudget-usedResp-1, 1)
		shown := 0
		for _, k := range keys {
			if shown >= maxHeaderLines {
				fmt.Fprintf(&b, "   … and %d more\n", len(keys)-shown)
				usedResp++
				break
			}
			fmt.Fprintf(&b, "   %s: %s\n", k, strings.Join(e.ResponseHeaders[k], ", "))
			shown++
			usedResp++
		}
	}
	if len(e.ResponseBody) > 0 && usedResp < respBudget {
		preview := truncateBytes(e.ResponseBody, 256)
		fmt.Fprintf(&b, " Body:     %s\n", preview)
		usedResp++
	}
	b.WriteByte('\n')

	b.WriteString(m.styles.sectionStyle.Render(" Timing "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Queue:    %s\n", e.Timing.QueueDuration().Truncate(time.Millisecond))
	fmt.Fprintf(&b, " TTFB:     %s\n", e.Timing.TTFB().Truncate(time.Millisecond))
	fmt.Fprintf(&b, " Download: %s\n", e.Timing.DownloadDuration().Truncate(time.Millisecond))
	if e.Timing.ResponseComplete.IsZero() {
		b.WriteString(" Total:    —\n")
	} else {
		fmt.Fprintf(&b, " Total:    %s\n", e.Timing.Duration().Truncate(time.Millisecond))
	}

	barWidth := max(min(m.width-10, 60), 0)
	fmt.Fprintf(&b, " %s\n", m.renderDetailWaterfall(e, barWidth))

	b.WriteString("\n [Esc/Enter] close ")
	return m.styles.overlayStyle.Render(b.String())
}

func (m Model) renderDetailWaterfall(e *journal.Entry, width int) string {
	if width <= 0 {
		return ""
	}
	total := e.Timing.Duration()
	if total <= 0 {
		return strings.Repeat("─", width)
	}

	queue := e.Timing.QueueDuration()
	ttfb := e.Timing.TTFB()

	queueSeg := min(int(math.Round(float64(queue)/float64(total)*float64(width))), width)
	ttfbSeg := int(math.Round(float64(ttfb) / float64(total) * float64(width)))
	if queueSeg+ttfbSeg > width {
		ttfbSeg = width - queueSeg
	}
	downloadSeg := max(width-queueSeg-ttfbSeg, 0)

	var b strings.Builder
	if queueSeg > 0 {
		b.WriteString(m.styles.waterfallQueueStyle.Render(strings.Repeat("█", queueSeg)))
	}
	if ttfbSeg > 0 {
		b.WriteString(m.styles.waterfallTTFBStyle.Render(strings.Repeat("█", ttfbSeg)))
	}
	if downloadSeg > 0 {
		b.WriteString(m.styles.waterfallDownloadStyle.Render(strings.Repeat("█", downloadSeg)))
	}
	return b.String()
}

func (m Model) renderHelpOverlay() string {
	// The provider-switch binding exists only when the header renders the
	// switcher (multi-provider, or a single named provider). A single
	// unnamed provider keeps the legacy overlay byte-identical.
	switcher := ""
	if m.hasSwitcher() {
		switcher = " Tab/Shift+Tab  Switch provider\n"
	}
	return m.styles.overlayStyle.Render(" Keybindings \n\n"+
		" 1-6          Switch tab (Overview/Requests/Network/Logs/Concurrency/Routes)\n"+
		" j/k or ↑/↓   Scroll down/up\n"+
		" PgUp/PgDn     Page up / Page down\n"+
		" Home/End      Jump to first / last item\n"+
		" Ctrl-U / Ctrl-D  Half-page scroll\n"+
		" g             Jump to top    G      Jump to bottom\n"+
		" Enter/Space   Inspect selected entry\n"+
		" /             Filter entries (Requests/Network/Logs tabs)\n"+
		" t             Cycle type filter (Network tab)\n"+
		" s             Cycle status filter (Network tab)\n"+
		" c             Reset Stats (y confirms, n/Esc cancels)\n"+
		switcher+
		" Esc           Close overlay / Clear filter\n"+
		" ?             Show this help\n"+
		" q / Ctrl+C    Quit\n\n"+
		" Mouse: wheel scroll, click tabs to switch\n\n"+
		" [Any key] close ") + "\n"
}

func (m Model) renderFooter() string {
	keys := " 1-6:tab │ j/k:scroll │ PgUp/PgDn │ Home/End │ Ctrl-U/D │ /:filter │ t:type │ s:status │ c:reset │ ?:help │ q:quit "
	return m.styles.footerStyle.Render(keys)
}

func (m Model) renderGaugeBar(active, max, width int) string {
	if max <= 0 || width <= 0 {
		return m.styles.dimStyle2.Render("  [ empty ]")
	}
	pct := min(int(math.Round(float64(active)/float64(max)*100)), 100)
	filled := min(int(math.Round(float64(pct)/100.0*float64(width))), width)
	if filled < 0 {
		filled = 0
	}
	empty := width - filled

	bar := "  ["
	bar += m.gaugeFillStyle(pct).Render(strings.Repeat("█", filled))
	if empty > 0 {
		bar += m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]  "
	return bar
}

func (m Model) gaugeFillStyle(pct int) lipgloss.Style {
	switch {
	case pct >= 90:
		return m.styles.gaugeCriticalStyle
	case pct >= 60:
		return m.styles.gaugeWarnStyle
	default:
		return m.styles.gaugeNormalStyle
	}
}

func (m Model) renderHBar(value, valueMax, width int, color lipgloss.Style) string {
	if valueMax <= 0 || width <= 0 {
		return m.styles.dimStyle2.Render("  [ empty ]")
	}
	filled := max(min(int(math.Round(float64(value)/float64(valueMax)*float64(width))), width), 0)
	empty := width - filled

	bar := "  ["
	if filled > 0 {
		bar += color.Render(strings.Repeat("█", filled))
	}
	if empty > 0 {
		bar += m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]  "
	return bar
}

// renderDualBars renders the Concurrency gauge and Queue Depth bar
// side by side on a single line, each with a left-hand label in
// the section header color (#58A6FF).
// activeWidth controls the LHS (Active) gauge width; the RHS (Queued)
// bar gets whatever space remains. The left edge of the LHS bar and the
// right edge of the RHS bar align with the positions the previous
// single bar occupied, and the two bars meet in the middle with a gap.
func (m Model) renderDualBars(activeWidth int) string {
	activeWidth = max(activeWidth, 0)
	vw := m.viewportWidth()

	// Label widths (visible, excluding ANSI codes): "  Active  " = 10, "Queued   " = 9
	// Layout: "  Active  [" + lhsBlocks + "]  Queued   [" + rhsBlocks + "]  "
	// Total = 10 + 1 + activeWidth + 1 + 2 + 9 + 1 + rhsWidth + 1 + 2 = activeWidth + rhsWidth + 27
	// rhsWidth = vw - activeWidth - 27
	lhsLabel := m.styles.sectionStyle.Render("  Active  ")
	rhsLabel := m.styles.sectionStyle.Render("Queued   ")
	trailing := "  "
	rhsWidth := max(vw-activeWidth-27, 0)

	queueMax := m.conc * 4
	if queueMax == 0 {
		queueMax = 1
	}

	var b strings.Builder

	// LHS: concurrency gauge bar
	b.WriteString(lhsLabel)
	b.WriteString("[")
	if m.conc > 0 && activeWidth > 0 {
		lhsFilled := max(min(int(math.Round(float64(m.snap.Active)/float64(m.conc)*float64(activeWidth))), activeWidth), 0)
		pct := min(int(math.Round(float64(m.snap.Active)/float64(m.conc)*100)), 100)
		b.WriteString(m.gaugeFillStyle(pct).Render(strings.Repeat("█", lhsFilled)))
		if lhsFilled < activeWidth {
			b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", activeWidth-lhsFilled)))
		}
	} else {
		b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", activeWidth)))
	}
	b.WriteString("]")

	// Gap between bars
	b.WriteString("  ")

	// RHS: queue depth bar
	b.WriteString(rhsLabel)
	b.WriteString("[")
	if queueMax > 0 && rhsWidth > 0 {
		rhsFilled := max(min(int(math.Round(float64(m.snap.Queued)/float64(queueMax)*float64(rhsWidth))), rhsWidth), 0)
		b.WriteString(m.queueFillStyle(int(m.snap.Queued), queueMax).Render(strings.Repeat("█", rhsFilled)))
		if rhsFilled < rhsWidth {
			b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", rhsWidth-rhsFilled)))
		}
	} else {
		b.WriteString(m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", rhsWidth)))
	}
	b.WriteString("]" + trailing)

	return b.String()
}

func (m Model) queueFillStyle(value, valueMax int) lipgloss.Style {
	if valueMax <= 0 {
		return m.styles.gaugeEmptyStyle
	}
	pct := min(int(math.Round(float64(value)/float64(valueMax)*100)), 100)
	switch {
	case pct >= 90:
		return m.styles.gaugeCriticalStyle
	case pct >= 50:
		return m.styles.queueWarnStyle
	case value > 0:
		return m.styles.queueFillDefaultStyle
	default:
		return m.styles.gaugeEmptyStyle
	}
}

func (m Model) statusStyle(code int) lipgloss.Style {
	switch {
	case code >= 100 && code < 200:
		return m.styles.statusInfoStyle
	case code >= 200 && code < 300:
		return m.styles.statusOkStyle
	case code >= 300 && code < 400:
		return m.styles.statusRedirectStyle
	case code >= 400 && code < 500:
		return m.styles.statusClientErrStyle
	case code >= 500:
		return m.styles.statusServerErrStyle
	default:
		return m.styles.dimStyle2
	}
}

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

// sortedHeaderKeys returns the keys of an http.Header map in alphabetical
// order so iteration is deterministic (avoids Go's randomized map order).
func sortedHeaderKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Run starts the TUI dashboard and blocks until the program exits.
// The returned *tea.Program may be used by the caller to shut down the
// TUI and restore terminal state (see Kill / RestoreTerminal).
// logPollInterval is how often the Logs tab poller checks the shared LogBuffer
// for freshly captured log lines.
const logPollInterval = 100 * time.Millisecond

// Run (multi-provider form): see the doc comment above the signature.
// resetCh is the channel the model's "Reset Stats" action signals on; main
// owns it, drains it, and resets every provider's metrics collector on
// receipt. It must be buffered (cap >= 1) so the model's non-blocking send
// never drops the very first request. A nil resetCh disables the signal.
func Run(updates <-chan ProviderUpdate, metas []ProviderMeta, progCh chan<- *tea.Program, resetCh chan struct{}, logBuf *LogBuffer) *tea.Program {
	m := NewModelForProviders(metas)
	if resetCh != nil {
		m.resetCh = resetCh
	}
	if logBuf != nil {
		m.logBuf = logBuf

	}
	p := tea.NewProgram(m)

	done := make(chan struct{})

	if logBuf != nil {
		go func() {
			defer func() { recover() }()
			ticker := time.NewTicker(logPollInterval)
			defer ticker.Stop()
			// Replay any startup lines promptly. The model drains via ReadNew(logBufSeen)
			// inside Update, so this first tick guarantees buffered startup summaries
			// (written before Run) are not delayed a full poll interval.
			// Subsequent ticks are revision-gated to avoid 10 Hz wake-ups when idle.
			// Note: bubbletea v2.0.7 Program.Send is non-blocking after shutdown
			// (select { case <-ctx.Done(): case msgs <- msg: } at tea.go:1183), so even
			// if ticker and done are simultaneously ready at shutdown, Send cannot leak.
			lastSentRev := logBuf.Revision()
			if lastSentRev != 0 {
				p.Send(logPollTickMsg{})
			}
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					if cur := logBuf.Revision(); cur != lastSentRev {
						lastSentRev = cur
						p.Send(logPollTickMsg{})
					}
				}
			}
		}()
	}

	go func() {
		defer func() { recover() }()
		for upd := range updates {
			p.Send(upd)
		}
	}()

	if progCh != nil {
		progCh <- p
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI: %v\n", err)
	}
	close(done)
	return p
}
