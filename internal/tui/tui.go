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
// It renders a full-screen, interactive dashboard with five tabs:
//   - Overview: throughput sparkline, concurrency gauge, status distribution,
//     queue depth, in-flight requests, summary
//   - Requests: scrollable, inspectable log with search/filter
//   - Network: Chrome DevTools-equivalent network panel with request/response
//     inspection, waterfall timing, content-type detection, and filtering
//   - Concurrency: live gauge, per-route bars, oldest queued age
//   - Routes: sorted per-route stats table
//
// The TUI listens for metrics.Snapshot messages on a channel and refreshes
// at ~4 fps. It supports full mouse interaction (click to select, wheel to
// scroll) and keyboard navigation.
package tui

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
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
}

func NewModel(conc int) Model {
	return Model{
		conc:      conc,
		startTime: time.Now(),
		resetCh:   make(chan struct{}, 1),
	}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case resetMsg:
		select {
		case m.resetCh <- struct{}{}:
		default:
		}
	case metrics.Snapshot:
		m.snap = msg
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyPressMsg:
		m, cmd = m.handleKey(msg)
	case tea.MouseClickMsg:
		m, cmd = m.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		m, cmd = m.handleMouseWheel(msg)
	}
	m.networkFiltered = m.computeVisibleNetworkEntries()
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
		m.cursor -= m.visibleRows()
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.adjustViewport()
		return m, nil
	}
	if keyCode == tea.KeyPgDown {
		m.cursor += m.visibleRows()
		if m.cursor > m.maxCursor() {
			m.cursor = m.maxCursor()
		}
		m.adjustViewport()
		return m, nil
	}
	if keyCode == tea.KeyHome {
		m.cursor = 0
		m.adjustViewport()
		return m, nil
	}
	if keyCode == tea.KeyEnd {
		m.cursor = m.maxCursor()
		m.adjustViewport()
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
		m.switchTab(tabConcurrency)
	case "5":
		m.switchTab(tabRoutes)

	case "j", "down":
		m.moveCursor(1)
	case "k", "up":
		m.moveCursor(-1)
	case "g":
		m.cursor, m.scroll = 0, 0
	case "G":
		m.cursor, m.scroll = m.maxCursor(), m.maxScroll()

	case "ctrl+u":
		m.cursor -= m.visibleRows() / 2
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.adjustViewport()
	case "ctrl+d":
		m.cursor += m.visibleRows() / 2
		if m.cursor > m.maxCursor() {
			m.cursor = m.maxCursor()
		}
		m.adjustViewport()

	case "enter", " ", "space":
		if m.canInspect() {
			m.mode = modeDetail
		}

	case "/":
		if m.tab == tabRequests || m.tab == tabNetwork {
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
}

func (m *Model) moveCursor(delta int) {
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

func (m *Model) adjustViewport() {
	maxC := m.maxCursor()
	if m.cursor > maxC {
		m.cursor = maxC
	}
	visible := m.visibleRows()
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+visible {
		m.scroll = m.cursor - visible + 1
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
	v := m.height - 8
	if m.tab == tabConcurrency {
		v -= 6
	}
	return max(v, 1)
}

func (m *Model) maxCursor() int {
	switch m.tab {
	case tabRequests:
		return max(len(m.visibleEntries())-1, 0)
	case tabNetwork:
		return max(len(m.visibleNetworkEntries())-1, 0)
	case tabConcurrency:
		return max(len(m.snap.InFlight)-1, 0)
	case tabRoutes:
		stats := m.snap.RouteStats
		return max(len(stats)-1, 0)
	}
	return 0
}

func (m *Model) maxScroll() int {
	return max(m.maxCursor()-m.visibleRows()+1, 0)
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
	case tabConcurrency:
		return m.cursor < len(m.snap.InFlight)
	default:
		return false
	}
}

func (m Model) handleMouseClick(msg tea.MouseClickMsg) (Model, tea.Cmd) {
	mx := msg.Mouse().X
	my := msg.Mouse().Y
	if my == 1 {
		tabWidth := m.width / int(numTabs)
		clickedTab := mx / tabWidth
		if int(clickedTab) < int(numTabs) {
			m.switchTab(tabID(clickedTab))
		}
	}
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

func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "ai-concurrency-shaper"

	if m.width == 0 || m.height == 0 {
		return v
	}

	if m.width < 60 {
		v.SetContent(fmt.Sprintf(" Terminal too narrow (%d cols, min 60) ─ resize to continue", m.width))
		return v
	}

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
		olLines := strings.Count(overlay, "\n") + 1
		m.padLines(&b, olLines)
	case modeHelp:
		help := m.renderHelpOverlay()
		b.WriteString(help)
		hlLines := strings.Count(help, "\n") + 1
		m.padLines(&b, hlLines)
	case modeConfirm:
		confirm := m.renderConfirmOverlay()
		b.WriteString(confirm)
		clLines := strings.Count(confirm, "\n") + 1
		m.padLines(&b, clLines)
	default:
		content := m.renderContent()
		b.WriteString(content)
		lines := strings.Count(content, "\n") + 1
		m.padLines(&b, lines)
	}

	if m.mode == modeFilter && (m.tab == tabRequests || m.tab == tabNetwork) {
		b.WriteByte('\n')
		b.WriteString(filterPromptStyle.Render(fmt.Sprintf(" Filter: %s█", m.filterText)))
	} else {
		b.WriteByte('\n')
	}

	b.WriteString(m.renderFooter())
	v.SetContent(b.String())
	return v
}

func (m *Model) padLines(b *strings.Builder, lines int) {
	visible := m.height - 8
	for i := lines; i < visible; i++ {
		b.WriteByte('\n')
	}
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
	names := []string{"1 Overview", "2 Requests", "3 Network", "4 Concurrency", "5 Routes"}
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
		return m.renderDashboard()
	case tabRequests:
		return m.renderRequests()
	case tabNetwork:
		return m.renderNetwork()
	case tabConcurrency:
		return m.renderConcurrency()
	case tabRoutes:
		return m.renderRoutes()
	}
	return ""
}

func (m Model) renderDashboard() string {
	var b strings.Builder

	b.WriteString(sectionStyle.Render(" Throughput (10s) "))
	b.WriteByte('\n')
	b.WriteString(m.renderSparkline())
	b.WriteByte('\n')

	b.WriteByte('\n')
	b.WriteString(sectionStyle.Render(" Concurrency "))
	b.WriteByte('\n')
	b.WriteString(m.renderGaugeBar(int(m.snap.Active), m.conc, m.width-4))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  %d / %d active slots\n", m.snap.Active, m.conc)

	b.WriteByte('\n')
	b.WriteString(sectionStyle.Render(" Queue Depth "))
	b.WriteByte('\n')
	queueMax := m.conc * 4
	if queueMax == 0 {
		queueMax = 1
	}
	b.WriteString(m.renderHBar(int(m.snap.Queued), queueMax, m.width-4, queueColor))
	b.WriteByte('\n')
	if m.snap.Queued == 0 {
		b.WriteString("  Queue: empty\n")
	} else {
		fmt.Fprintf(&b, "  %d waiting\n", m.snap.Queued)
	}

	b.WriteByte('\n')
	b.WriteString(sectionStyle.Render(" Status Distribution "))
	b.WriteByte('\n')
	b.WriteString(m.renderStatusBar())
	b.WriteByte('\n')

	b.WriteByte('\n')
	b.WriteString(sectionStyle.Render(" In-Flight Requests "))
	b.WriteByte('\n')
	flights := m.snap.InFlight
	fmt.Fprintf(&b, "  %d in-flight (%d limited, %d passthrough)\n",
		len(flights), m.snap.InFlightLimited, m.snap.InFlightPassthrough)
	show := min(len(flights), 6)
	for i := 0; i < show; i++ {
		r := flights[i]
		age := r.Age().Truncate(time.Millisecond)
		tag := limitedTag
		if !r.Limited {
			tag = passTag
		}
		fmt.Fprintf(&b, "  %s %-6s %-35s %s\n", tag, r.Method, r.Path, age)
	}
	if len(flights) > show {
		fmt.Fprintf(&b, "  … and %d more\n", len(flights)-show)
	}

	b.WriteByte('\n')
	b.WriteString(sectionStyle.Render(" Summary "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  Proxied: %d  │  Passthrough: %d  │  Timeouts: %d  │  Cancelled: %d\n",
		m.snap.TotalProxied, m.snap.TotalPassThrough, m.snap.TotalTimeout, m.snap.TotalCancelled)

	return b.String()
}

func (m Model) renderSparkline() string {
	spark := m.snap.Sparkline
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
	line := "  "
	for _, v := range spark {
		idx := int(float64(v) / float64(maxVal) * float64(len(chars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(chars) {
			idx = len(chars) - 1
		}
		line += chars[idx]
	}
	return line
}

func (m Model) renderStatusBar() string {
	counts := m.snap.StatusCounts
	total := counts[1] + counts[2] + counts[3] + counts[4] + counts[5]
	if total == 0 {
		return "  No responses yet\n"
	}
	width := m.width - 4
	if width < 10 {
		width = 10
	}

	labels := []string{"1xx", "2xx", "3xx", "4xx", "5xx"}
	cvalues := []int64{counts[1], counts[2], counts[3], counts[4], counts[5]}
	colors := []lipgloss.Style{statusInfoStyle, statusOkStyle, statusRedirectStyle, statusClientErrStyle, statusServerErrStyle}

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
		fmt.Fprintf(&b, " %s:%d", labels[i], v)
	}
	return b.String()
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

	visible := m.visibleRows()
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

	if len(entries) > visible {
		fmt.Fprintf(&b, "  %d-%d / %d entries\n", start+1, end, len(entries))
	}
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

	visible := m.visibleRows()
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

	if len(entries) > visible {
		fmt.Fprintf(&b, "  %d-%d / %d entries\n", start+1, end, len(entries))
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

	queueSeg := int(math.Round(float64(queue) / float64(total) * float64(barWidth)))
	if queueSeg > barWidth {
		queueSeg = barWidth
	}
	ttfbSeg := int(math.Round(float64(ttfb) / float64(total) * float64(barWidth)))
	if queueSeg+ttfbSeg > barWidth {
		ttfbSeg = barWidth - queueSeg
	}
	downloadSeg := barWidth - queueSeg - ttfbSeg
	if downloadSeg < 0 {
		downloadSeg = 0
	}

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
	b.WriteString(m.renderGaugeBar(int(m.snap.Active), m.conc, m.width-4))
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
	b.WriteString(m.renderHBar(int(m.snap.Queued), queueMax, m.width-4, queueColor))
	b.WriteByte('\n')
	if m.snap.Queued == 0 {
		b.WriteString("  Queue: empty\n")
	} else {
		fmt.Fprintf(&b, "  %d waiting\n", m.snap.Queued)
	}
	b.WriteByte('\n')

	b.WriteString(sectionStyle.Render(" In-Flight Requests "))
	b.WriteByte('\n')
	flights := m.snap.InFlight
	if len(flights) == 0 {
		b.WriteString("  No requests in flight.\n")
		return b.String()
	}

	visible := m.visibleRows()
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
	if len(flights) > visible {
		fmt.Fprintf(&b, "  %d-%d / %d in-flight\n", start+1, end, len(flights))
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

	visible := m.visibleRows()
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
	if len(pairs) > visible {
		fmt.Fprintf(&b, "  %d-%d / %d routes\n", start+1, end, len(pairs))
	}
	return b.String()
}

func (m Model) resetCmd() tea.Cmd {
	return func() tea.Msg {
		return resetMsg{}
	}
}

type resetMsg struct{}

func (m Model) renderConfirmOverlay() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(overlayStyle.Render(
		fmt.Sprintf(" Reset Stats \n\n" +
			" Clear all cumulative counters?\n" +
			" (Proxied, Passthrough, Timeouts, etc.)\n\n" +
			" y = yes    n/Esc = no")))
	return b.String()
}

func (m Model) renderDetailOverlay() string {
	var b strings.Builder
	b.WriteString("\n")

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

	return b.String()
}

func (m Model) renderNetworkDetail(e *journal.Entry) string {
	// Compute a line budget so the overlay fits within the terminal.
	// Available content height = terminal - chrome (header, tabbar, separator,
	// footer, filter/blank line). Overlay border + padding consume 4 more.
	// We target the overlay content to fit within the visible area.
	budget := m.height - 12
	if budget < 10 {
		budget = 10 // minimum usable detail view
	}

	// Count fixed lines that are always emitted (minimum 17):
	//   Request heading, Method, URL, [blank],
	//   Response heading, Status, Type, Size, [blank],
	//   Timing heading, Queue, TTFB, Download, Total, Waterfall,
	//   [blank], close
	const fixedLines = 17

	// Remaining budget for variable sections: headers and body previews.
	varBudget := budget - fixedLines
	if varBudget < 4 {
		varBudget = 4 // at least 2 lines each for req/resp headers
	}

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
		usedReq++                                 // "Headers:" line
		maxHeaderLines := reqBudget - usedReq - 1 // -1 for potential body line
		if maxHeaderLines < 1 {
			maxHeaderLines = 1
		}
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
		maxHeaderLines := respBudget - usedResp - 1
		if maxHeaderLines < 1 {
			maxHeaderLines = 1
		}
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

	barWidth := m.width - 10
	if barWidth > 60 {
		barWidth = 60
	}
	fmt.Fprintf(&b, " %s\n", m.renderDetailWaterfall(e, barWidth))

	b.WriteString("\n [Esc/Enter] close ")
	return overlayStyle.Render(b.String())
}

func (m Model) renderDetailWaterfall(e *journal.Entry, width int) string {
	total := e.Timing.Duration()
	if total <= 0 {
		return strings.Repeat("─", width)
	}

	queue := e.Timing.QueueDuration()
	ttfb := e.Timing.TTFB()

	queueSeg := int(math.Round(float64(queue) / float64(total) * float64(width)))
	if queueSeg > width {
		queueSeg = width
	}
	ttfbSeg := int(math.Round(float64(ttfb) / float64(total) * float64(width)))
	if queueSeg+ttfbSeg > width {
		ttfbSeg = width - queueSeg
	}
	downloadSeg := width - queueSeg - ttfbSeg
	if downloadSeg < 0 {
		downloadSeg = 0
	}

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
	return overlayStyle.Render(" Keybindings \n\n" +
		" 1-5          Switch tab (Overview/Requests/Network/Concurrency/Routes)\n" +
		" j/k or ↑/↓   Scroll down/up\n" +
		" PgUp/PgDn     Page up / Page down\n" +
		" Home/End      Jump to first / last item\n" +
		" Ctrl-U / Ctrl-D  Half-page scroll\n" +
		" g             Jump to top    G      Jump to bottom\n" +
		" Enter/Space   Inspect selected entry\n" +
		" /             Filter entries (Requests/Network tabs)\n" +
		" t             Cycle type filter (Network tab)\n" +
		" s             Cycle status filter (Network tab)\n" +
		" Esc           Close overlay / Clear filter\n" +
		" ?             Show this help\n" +
		" q / Ctrl+C    Quit\n\n" +
		" Mouse: wheel scroll, click tabs to switch\n\n" +
		" [Any key] close ")
}

func (m Model) renderFooter() string {
	keys := " 1-5:tab │ j/k:scroll │ PgUp/PgDn │ Home/End │ Ctrl-U/D │ /:filter │ t:type │ s:status │ ?:help │ q:quit "
	return footerStyle.Render(keys)
}

func (m Model) renderGaugeBar(active, max, width int) string {
	if max <= 0 || width <= 0 {
		return "  [ empty ]"
	}
	pct := int(math.Round(float64(active) / float64(max) * 100))
	if pct > 100 {
		pct = 100
	}
	filled := int(math.Round(float64(pct) / 100.0 * float64(width)))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	empty := width - filled

	bar := "  ["
	bar += gaugeActiveStyle.Render(strings.Repeat("█", filled))
	if empty > 0 {
		bar += gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += fmt.Sprintf("]  %d%%", pct)
	return bar
}

func (m Model) renderHBar(value, valueMax, width int, color lipgloss.Style) string {
	if valueMax <= 0 || width <= 0 {
		return "  [ empty ]"
	}
	filled := int(math.Round(float64(value) / float64(valueMax) * float64(width)))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	empty := width - filled

	bar := "  ["
	if filled > 0 {
		bar += color.Render(strings.Repeat("█", filled))
	}
	if empty > 0 {
		bar += gaugeEmptyStyle.Render(strings.Repeat("░", empty))
	}
	bar += "]"
	return bar
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

	gaugeActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#3FB950"))

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

	queueColor = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#D29922"))

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
