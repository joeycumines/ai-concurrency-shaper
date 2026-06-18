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
//   - Overview: throughput sparkline, concurrency gauge, status distribution,
//     queue depth, in-flight requests, summary
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
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
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

// logWriter wraps a logRing as an io.Writer.
type logWriter struct{ ring *logRing }

func (w *logWriter) Write(p []byte) (int, error) { return w.ring.Write(p) }

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

	dragging        bool
	dragStartY      int
	dragStartScroll int

	// redrawEpoch makes View.Content differ on resync frames without changing
	// visible text. It is paired with ClearScreen so Bubble Tea's renderer
	// cannot skip the repaint after tmux or terminal state changes.
	redrawEpoch int
}

func NewModel(conc int) Model {
	m := Model{
		conc:      conc,
		startTime: time.Now(),
		resetCh:   make(chan struct{}, 1),
		logRing:   newLogRing(logRingCapacity),
	}
	for i := range m.scrollbars {
		m.scrollbars[i] = *scrollbar.New()
	}
	return m
}

func (m *Model) LogWriter() io.Writer { return &logWriter{ring: m.logRing} }

type resyncTickMsg struct{}
type resyncDrawMsg struct{}

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
	m.toasts = append(m.toasts, t)
}

func (m Model) Init() tea.Cmd { return m.resyncTickCmd() }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case resetMsg:
		select {
		case m.resetCh <- struct{}{}:
		default:
		}
	case resyncTickMsg:
		return m, resyncRedrawSequence()
	case resyncDrawMsg:
		m.redrawEpoch++
		return m, m.resyncTickCmd()
	case metrics.Snapshot:
		m.snap = msg
		m.dashboardLinesCache = nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dashboardLinesCache = nil
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
	m.adjustViewport()
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
		if m.tab == tabDashboard {
			m.scroll = m.maxScroll()
			m.cursor = m.scroll
		} else {
			m.cursor = m.maxCursor()
			m.adjustViewport()
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
		m.cursor, m.scroll = 0, 0
	case "G":
		if m.tab == tabDashboard {
			m.scroll = m.maxScroll()
			m.cursor = m.scroll
		} else {
			m.cursor, m.scroll = m.maxCursor(), m.maxScroll()
		}

	case "ctrl+u":
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
}

func (m *Model) moveCursor(delta int) {
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

// hBarWidth returns the inner block count for renderHBar and the status bar's
// segment width so the full bar ("  [" + blocks + "]  ") fits within the
// viewport width with a symmetrical two-cell left and right margin before the
// scrollbar column.
func (m *Model) hBarWidth() int {
	// Full bar visual width: 3 + blocks + 1 + 2 = blocks + 6.
	// Must fit within viewportWidth.
	return max(m.viewportWidth()-6, 0)
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

func (m *Model) updateScrollbars() {
	contentHeight := m.maxCursor() + 1
	viewportHeight := m.dataRows()
	sb := &m.scrollbars[m.tab]
	sb.ContentHeight = contentHeight
	sb.ViewportHeight = viewportHeight
	sb.YOffset = m.scroll
}

// truncateANSI truncates line to at most width terminal cells, preserving the
// CSI/SGR escape sequences produced by lipgloss. It appends a reset sequence
// (ESC[0m) if truncation occurs so that active styles do not leak into the
// trailing padding. It intentionally does not handle OSC/DCS/APC/SOS
// sequences because the TUI only emits SGR styling.
func truncateANSI(line string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	cells := 0
	truncated := false
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
	if truncated {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// stripANSI removes ANSI escape sequences from a string.
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

	// Tab bar (row 1).
	if my == 1 {
		tabWidth := m.width / int(numTabs)
		clickedTab := mx / tabWidth
		if int(clickedTab) < int(numTabs) {
			m.switchTab(tabID(clickedTab))
		}
		return m, nil
	}

	// Content area starts at row 3 (header=0, tabbar=1, separator=2).
	contentEndRow := contentStartRow + m.visibleRows()
	if my < contentStartRow || my >= contentEndRow {
		return m, nil
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
	b.WriteString(sepStyle.Render(strings.Repeat("─", m.width)))
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
		b.WriteString(filterPromptStyle.Render(fmt.Sprintf(" Filter: %s█", m.filterText)))
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

func (m Model) renderHeader() string {
	uptime := time.Since(m.startTime).Truncate(time.Second)
	return headerStyle.Render(
		fmt.Sprintf(" ⚡ shaper │ %d/%d active │ %d queued │ %.1f req/s │ %d ✗ TO │ uptime %s",
			m.snap.Active, m.conc, m.snap.Queued, m.snap.Throughput,
			m.snap.TotalTimeout, uptime),
	)
}

func (m Model) renderTabBar() string {
	names := []string{"1 Overview", "2 Requests", "3 Network", "4 Logs", "5 Concurrency", "6 Routes"}
	parts := make([]string, len(names))
	for i, name := range names {
		if tabID(i) == m.tab {
			parts[i] = tabActiveStyle.Render(" " + name + " ")
		} else {
			parts[i] = tabInactiveStyle.Render(" " + name + " ")
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
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
		return dimStyle2.Render("  —")
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
	style := sparklineFillStyle(spark[len(spark)-1], maxVal)
	return style.Render(line.String())
}

func sparklineFillStyle(last, max int) lipgloss.Style {
	if max <= 0 {
		return sparklineStyle
	}
	pct := min(int(math.Round(float64(last)/float64(max)*100)), 100)
	switch {
	case pct >= 90:
		return gaugeCriticalStyle
	case pct >= 60:
		return gaugeWarnStyle
	default:
		return sparklineStyle
	}
}

func (m Model) renderStatusBar() string {
	counts := m.snap.StatusCounts
	total := counts[1] + counts[2] + counts[3] + counts[4] + counts[5]
	if total == 0 {
		width := m.hBarWidth()
		return "  [" + gaugeEmptyStyle.Render(strings.Repeat("░", width)) + "]  "
	}
	labels := []string{"1xx", "2xx", "3xx", "4xx", "5xx"}
	cvalues := []int64{counts[1], counts[2], counts[3], counts[4], counts[5]}
	colors := []lipgloss.Style{statusInfoStyle, statusOkStyle, statusRedirectStyle, statusClientErrStyle, statusServerErrStyle}

	var labelsWidth int
	for i, v := range cvalues {
		labelsWidth += uniseg.StringWidth(fmt.Sprintf(" %s:%d", labels[i], v))
	}
	width := max(m.hBarWidth()-labelsWidth, 0)

	var b strings.Builder
	b.WriteString("  [")
	pos := 0
	for i, v := range cvalues {
		if v == 0 {
			continue
		}
		seg := int(math.Round(float64(v) / float64(total) * float64(width)))
		if seg == 0 {
			seg = 1
		}
		if pos+seg > width {
			seg = width - pos
		}
		b.WriteString(colors[i].Render(strings.Repeat("█", seg)))
		pos += seg
	}
	if pos < width {
		b.WriteString(gaugeEmptyStyle.Render(strings.Repeat("░", width-pos)))
	}
	b.WriteString("]")
	for i, v := range cvalues {
		b.WriteString(" ")
		b.WriteString(colors[i].Render(fmt.Sprintf("%s:%d", labels[i], v)))
	}
	b.WriteString("  ")
	return b.String()
}

// dashboardLines builds the full list of rendered lines for the Dashboard tab.
// It always renders all content (including up to six in-flight requests);
// renderDashboardContent() windows these lines by m.scroll and m.visibleRows().
func (m Model) dashboardLines() []string {
	var lines []string

	lines = append(lines, sectionStyle.Render(" Throughput (10s) "))
	lines = append(lines, m.renderSparkline())
	lines = append(lines, "")

	lines = append(lines, sectionStyle.Render(" Concurrency "))
	lines = append(lines, m.renderGaugeBar(int(m.snap.Active), m.conc, m.gaugeBarWidth()))
	lines = append(lines, fmt.Sprintf("  %d / %d active slots", m.snap.Active, m.conc))

	lines = append(lines, "")
	lines = append(lines, sectionStyle.Render(" Queue Depth "))
	queueMax := m.conc * 4
	if queueMax == 0 {
		queueMax = 1
	}
	lines = append(lines, m.renderHBar(int(m.snap.Queued), queueMax, m.hBarWidth(), queueFillStyle(int(m.snap.Queued), queueMax)))
	if m.snap.Queued == 0 {
		lines = append(lines, dimStyle2.Render("  Queue: empty"))
	} else {
		lines = append(lines, fmt.Sprintf("  %d waiting", m.snap.Queued))
	}
	if m.snap.RetriesInFlight > 0 {
		lines = append(lines, fmt.Sprintf("  %d active retries", m.snap.RetriesInFlight))
	}

	lines = append(lines, "")
	lines = append(lines, sectionStyle.Render(" Status Distribution "))
	lines = append(lines, m.renderStatusBar())

	lines = append(lines, "")
	lines = append(lines, sectionStyle.Render(" In-Flight Requests "))
	flights := m.snap.InFlight
	lines = append(lines, fmt.Sprintf("  %d in-flight (%d limited, %d passthrough)",
		len(flights), m.snap.InFlightLimited, m.snap.InFlightPassthrough))
	show := min(len(flights), 6)
	for i := range show {
		r := flights[i]
		age := r.Age().Truncate(time.Millisecond)
		tag := limitedTag
		if !r.Limited {
			tag = passTag
		}
		lines = append(lines, fmt.Sprintf("  %s %-6s %-35s %s", tag, r.Method, r.Path, age))
	}
	if len(flights) > show {
		lines = append(lines, fmt.Sprintf("  … and %d more", len(flights)-show))
	}

	lines = append(lines, "")
	lines = append(lines, sectionStyle.Render(" Summary "))
	lines = append(lines, fmt.Sprintf("  Proxied: %d  \u2502  Passthrough: %d  \u2502  Timeouts: %d  \u2502  Cancelled: %d  \u2502  Circuit rejects: %d",
		m.snap.TotalProxied, m.snap.TotalPassThrough, m.snap.TotalTimeout, m.snap.TotalCancelled, m.snap.TotalCircuitRejected))

	if cb := m.snap.CircuitBreaker; cb != nil {
		lines = append(lines, "")
		lines = append(lines, sectionStyle.Render(" Circuit Breaker "))
		var stateStyle lipgloss.Style
		switch cb.State {
		case "CLOSED":
			stateStyle = circuitClosedStyle
		case "OPEN":
			stateStyle = circuitOpenStyle
		case "HALF_OPEN":
			stateStyle = circuitHalfOpenStyle
		default:
			stateStyle = lipgloss.NewStyle()
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "  State: %s", stateStyle.Render(cb.State))
		fmt.Fprintf(&summary, "  |  Failures: %d  |  Consecutive: %d",
			cb.Failures, cb.ConsecutiveFailures)
		if cb.CurrentPenalty > 0 {
			fmt.Fprintf(&summary, "  |  Penalty: %s", cb.CurrentPenalty.Truncate(time.Millisecond))
		}
		if !cb.NextRetry.IsZero() {
			until := time.Until(cb.NextRetry).Truncate(time.Millisecond)
			if until > 0 {
				fmt.Fprintf(&summary, "  |  Next probe: %s", until)
			}
		}
		lines = append(lines, summary.String())
		lines = append(lines, "") // trailing blank before windowing
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
	b.WriteString(tableHeaderStyle.Render(
		fmt.Sprintf("%-8s %-6s %4s  %9s  %s", "Time", "Method", "St", "Duration", "Path")))
	b.WriteByte('\n')

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(entries))

	for i := start; i < end; i++ {
		e := entries[i]
		style := rowStyle
		if i == m.cursor {
			style = rowSelectedStyle
		}
		stStr := statusStyle(e.Status).Render(fmt.Sprintf("%4d", e.Status))
		line := fmt.Sprintf("%-8s %-6s %s  %9s  %s",
			e.Time.Format("15:04:05"), e.Method, stStr,
			e.Duration.Truncate(time.Millisecond), e.Path)
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
	b.WriteString(tableHeaderStyle.Render(
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
		style := rowStyle
		if i == m.cursor {
			style = rowSelectedStyle
		}

		name := truncate(e.Name(), 22)
		stStr := networkStatusStyle(e.StatusCode).Render(fmt.Sprintf("%4d", e.StatusCode))
		typeStr := e.Type()
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
		fmt.Fprintf(&b, "  Filter: %q  (%d / %d lines)\n", m.filterText, len(lines), m.logRing.count)
	}

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(lines))

	for i := start; i < end; i++ {
		style := rowStyle
		if i == m.cursor {
			style = rowSelectedStyle
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
		b.WriteString(waterfallQueueStyle.Render(strings.Repeat("█", queueSeg)))
	}
	if ttfbSeg > 0 {
		b.WriteString(waterfallTTFBStyle.Render(strings.Repeat("█", ttfbSeg)))
	}
	if downloadSeg > 0 {
		b.WriteString(waterfallDownloadStyle.Render(strings.Repeat("█", downloadSeg)))
	}
	return b.String()
}

func networkStatusStyle(code int) lipgloss.Style {
	switch {
	case code >= 200 && code < 300:
		return statusOkStyle
	case code >= 300 && code < 400:
		return statusRedirectStyle
	case code >= 400 && code < 500:
		return statusClientErrStyle
	case code >= 500:
		return statusServerErrStyle
	default:
		return dimStyle2
	}
}

func (m Model) renderConcurrency() string {
	var b strings.Builder

	b.WriteString(sectionStyle.Render(" Concurrency Gauge "))
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

	b.WriteString(sectionStyle.Render(" Queue Depth "))
	b.WriteByte('\n')
	queueMax := m.conc * 4
	if queueMax == 0 {
		queueMax = 1
	}
	b.WriteString(m.renderHBar(int(m.snap.Queued), queueMax, m.hBarWidth(), queueFillStyle(int(m.snap.Queued), queueMax)))
	b.WriteByte('\n')
	if m.snap.Queued == 0 {
		b.WriteString(dimStyle2.Render("  Queue: empty\n"))
	} else {
		fmt.Fprintf(&b, "  %d waiting\n", m.snap.Queued)
	}
	b.WriteByte('\n')

	b.WriteString(sectionStyle.Render(" In-Flight Requests "))
	b.WriteByte('\n')
	flights := m.snap.InFlight
	if len(flights) == 0 {
		b.WriteString(dimStyle2.Render("  No requests in flight.\n"))
		return b.String()
	}

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(flights))

	for i := start; i < end; i++ {
		r := flights[i]
		style := rowStyle
		if i == m.cursor {
			style = rowSelectedStyle
		}
		age := r.Age().Truncate(time.Millisecond)
		totalAge := r.TotalAge().Truncate(time.Millisecond)
		tag := limitedTag
		if !r.Limited {
			tag = passTag
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
	b.WriteString(tableHeaderStyle.Render(
		fmt.Sprintf("%-32s %5s %5s %5s %5s %5s %7s", "Route", "Total", "2xx", "4xx", "5xx", "✗ TO", "req/s")))
	b.WriteByte('\n')

	visible := m.dataRows()
	start := m.scroll
	end := min(start+visible, len(pairs))

	for i := start; i < end; i++ {
		p := pairs[i]
		style := rowStyle
		if i == m.cursor {
			style = rowSelectedStyle
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
	return overlayStyle.Render(
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
		b.WriteString(overlayStyle.Render(
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
		b.WriteString(overlayStyle.Render(
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

	b.WriteString(sectionStyle.Render(" Request "))
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

	b.WriteString(sectionStyle.Render(" Response "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Status:   %d\n", e.StatusCode)
	fmt.Fprintf(&b, " Type:     %s\n", e.Type())
	fmt.Fprintf(&b, " Size:     %s\n", e.SizeLabel())
	usedResp := 4 // heading + status + type + size

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

	b.WriteString(sectionStyle.Render(" Timing "))
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
	return overlayStyle.Render(b.String())
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
		b.WriteString(waterfallQueueStyle.Render(strings.Repeat("█", queueSeg)))
	}
	if ttfbSeg > 0 {
		b.WriteString(waterfallTTFBStyle.Render(strings.Repeat("█", ttfbSeg)))
	}
	if downloadSeg > 0 {
		b.WriteString(waterfallDownloadStyle.Render(strings.Repeat("█", downloadSeg)))
	}
	return b.String()
}

func (m Model) renderHelpOverlay() string {
	return overlayStyle.Render(" Keybindings \n\n"+
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
		" Esc           Close overlay / Clear filter\n"+
		" ?             Show this help\n"+
		" q / Ctrl+C    Quit\n\n"+
		" Mouse: wheel scroll, click tabs to switch\n\n"+
		" [Any key] close ") + "\n"
}

func (m Model) renderFooter() string {
	keys := " 1-6:tab │ j/k:scroll │ PgUp/PgDn │ Home/End │ Ctrl-U/D │ /:filter │ t:type │ s:status │ ?:help │ q:quit "
	return footerStyle.Render(keys)
}

func (m Model) renderGaugeBar(active, max, width int) string {
	if max <= 0 || width <= 0 {
		return dimStyle2.Render("  [ empty ]")
	}
	pct := min(int(math.Round(float64(active)/float64(max)*100)), 100)
	filled := min(int(math.Round(float64(pct)/100.0*float64(width))), width)
	if filled < 0 {
		filled = 0
	}
	empty := width - filled

	bar := "  ["
	bar += gaugeFillStyle(pct).Render(strings.Repeat("█", filled))
	if empty > 0 {
		bar += gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]  "
	return bar
}

func gaugeFillStyle(pct int) lipgloss.Style {
	switch {
	case pct >= 90:
		return gaugeCriticalStyle
	case pct >= 60:
		return gaugeWarnStyle
	default:
		return gaugeNormalStyle
	}
}

func (m Model) renderHBar(value, valueMax, width int, color lipgloss.Style) string {
	if valueMax <= 0 || width <= 0 {
		return dimStyle2.Render("  [ empty ]")
	}
	filled := max(min(int(math.Round(float64(value)/float64(valueMax)*float64(width))), width), 0)
	empty := width - filled

	bar := "  ["
	if filled > 0 {
		bar += color.Render(strings.Repeat("█", filled))
	}
	if empty > 0 {
		bar += gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]  "
	return bar
}

func queueFillStyle(value, valueMax int) lipgloss.Style {
	if valueMax <= 0 {
		return gaugeEmptyStyle
	}
	pct := min(int(math.Round(float64(value)/float64(valueMax)*100)), 100)
	switch {
	case pct >= 90:
		return gaugeCriticalStyle
	case pct >= 50:
		return queueWarnStyle
	case value > 0:
		return queueFillDefaultStyle
	default:
		return gaugeEmptyStyle
	}
}

func statusStyle(code int) lipgloss.Style {
	switch {
	case code >= 100 && code < 200:
		return statusInfoStyle
	case code >= 200 && code < 300:
		return statusOkStyle
	case code >= 300 && code < 400:
		return statusRedirectStyle
	case code >= 400 && code < 500:
		return statusClientErrStyle
	case code >= 500:
		return statusServerErrStyle
	default:
		return dimStyle2
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

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#E6EDF3")).
			Background(lipgloss.Color("#1F6FEB")).
			PaddingLeft(1).
			PaddingRight(1)

	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#58A6FF"))

	dimStyle2 = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E7681"))

	sepStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#30363D"))

	filterPromptStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#58A6FF")).
				Background(lipgloss.Color("#0D1117")).
				Bold(true)

	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0D1117")).
			Background(lipgloss.Color("#58A6FF")).
			PaddingLeft(1).
			PaddingRight(1)

	tabInactiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#8B949E")).
				Background(lipgloss.Color("#161B22")).
				PaddingLeft(1).
				PaddingRight(1)

	tableHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#58A6FF"))

	rowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E6EDF3"))

	rowSelectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#0D1117")).
				Background(lipgloss.Color("#388BFD"))

	gaugeNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#58A6FF")).Bold(true)

	gaugeWarnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#D29922")).Bold(true)

	gaugeCriticalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F85149")).Bold(true)

	queueWarnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F0883E")).Bold(true)

	sparklineStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#58A6FF"))

	gaugeEmptyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#21262D"))

	statusInfoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8B949E"))

	statusOkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#3FB950"))

	statusRedirectStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#58A6FF"))

	statusClientErrStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F0883E"))

	statusServerErrStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F85149"))

	limitedTag = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F0883E")).
			Render(" lim")

	passTag = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#6E7681")).
		Render(" pas")

	overlayStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E6EDF3")).
			Background(lipgloss.Color("#161B22")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#58A6FF")).
			Padding(1, 2).
			MarginLeft(2)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8B949E")).
			Background(lipgloss.Color("#0D1117"))

	circuitClosedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#3FB950"))

	circuitOpenStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F85149")).
				Bold(true)

	circuitHalfOpenStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F0883E"))

	queueFillDefaultStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#39D353")).Bold(true)

	waterfallQueueStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#D29922"))

	waterfallTTFBStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#58A6FF"))

	waterfallDownloadStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#3FB950"))
)

// Run starts the TUI dashboard and blocks until the program exits.
// The returned *tea.Program may be used by the caller to shut down the
// TUI and restore terminal state (see Kill / RestoreTerminal).
func Run(snapCh <-chan metrics.Snapshot, conc int, j *journal.Journal, progCh chan<- *tea.Program) *tea.Program {
	m := NewModel(conc)
	m.journal = j
	p := tea.NewProgram(m)

	go func() {
		defer func() { recover() }()
		for snap := range snapCh {
			p.Send(snap)
		}
	}()

	if progCh != nil {
		progCh <- p
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI: %v\n", err)
	}
	return p
}
