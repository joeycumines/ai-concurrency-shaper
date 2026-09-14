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
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/scrollbar"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
)

type uiMode int

const (
	modeBrowse uiMode = iota
	modeDetail
	modeFilter
	modeHelp
	modeConfirm
	modePalette
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

	// hScroll holds the horizontal scroll offset for tabs whose rows are
	// wider than the terminal (currently Network and Logs). It is keyed by
	// tab, mirroring the per-tab scrollbar state, and is clamped by
	// adjustHScroll against the widest rendered row.
	hScroll map[tabID]int

	// logDetailAnchor pins the open Logs detail view to the logRing item that
	// was selected when modeDetail was entered. seq is the ring sequence
	// number, which is unique per accepted line and survives position shifts
	// caused by new arrivals; text is the full line content. Eviction closes
	// the overlay once the sequence can no longer be resolved.
	logDetailAnchor logDetailAnchor

	// networkDetailAnchor pins the open Network detail view to the journal
	// Entry that was selected when modeDetail was entered. The pointer is
	// stable (journal entries are immutable and never rewritten in place);
	// eviction is detected by scanning the current journal for the ID.
	networkDetailAnchor *journal.Entry

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

	// palette is the live state of the command palette overlay.
	// It is only meaningful when mode == modePalette.
	palette paletteState
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
		hScroll:    make(map[tabID]int),
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
