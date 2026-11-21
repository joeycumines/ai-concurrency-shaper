package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui/toast"
)

func (m Model) runtimeView() tea.View {
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion, WindowTitle: "ai-concurrency-shaper"}
	if m.width <= 0 || m.height <= 0 {
		return v
	}
	if m.height < 4 {
		v.SetContent(fitRows([]string{"shaper · 1–8 tabs · q quit"}, m.width, m.height))
		return v
	}
	var content string
	switch {
	case m.export.rawPrompt:
		content = m.renderRawPrompt()
	case m.export.rawReview:
		content = m.renderRawReview()
	case m.controlPanel.preview != nil:
		content = m.renderControlReview()
	case m.controlPanel.form:
		content = m.renderControlForm()
	case m.inspection.open:
		content = m.renderInspection()
	case m.insights.inputMode:
		rows := []string{m.styles.sectionStyle.Render(" ABSOLUTE TIME RANGE"), "", "Two UTC or offset RFC3339 timestamps", "separated by a space. At most 366 days.", "", "Example:", "2026-11-21T12:00:00Z", "2026-11-21T13:00:00Z", "", m.styles.filterPromptStyle.Render(inputLine(" > ", m.insights.input, m.viewportWidth())), "", "Ctrl+U clears · Enter queries · Esc back"}
		content = fitRows(rows, m.viewportWidth(), m.visibleRows())
	case m.mode == modePalette:
		content = m.renderCommandPalette()
	case m.mode == modeConfirm:
		content = fitRows([]string{m.styles.sectionStyle.Render(" RESET LEGACY DISPLAY COUNTERS"), "", "Canonical observations and history remain.", "", "y resets the legacy counters.", "n / Esc cancels."}, m.viewportWidth(), m.visibleRows())
	case m.mode == modeDetail:
		content = m.renderDetailOverlay()
	default:
		if m.tab == tabInsights {
			content = m.renderInsights()
		} else if m.tab == tabControls {
			content = m.renderControls()
		} else if m.tab == tabRequests && m.runtime.Observations != nil {
			content = m.renderRequestExplorer()
		} else if m.tab == tabNetwork && m.width < 72 {
			content = m.renderCompactNetwork()
		} else if m.tab == tabDashboard && m.width < 72 {
			content = m.renderCompactOverview()
		} else {
			m.updateScrollbars()
			content = m.renderContentWithScrollbar()
		}
	}
	rows := []string{m.renderHeader(), m.renderTabBar(), m.styles.sepStyle.Render(strings.Repeat("─", max(m.width, 0)))}
	body := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for i := 0; i < m.visibleRows(); i++ {
		if i < len(body) {
			rows = append(rows, body[i])
		} else {
			rows = append(rows, "")
		}
	}
	// Preserve the original ephemeral log/error notifications without moving
	// chart geometry or covering an effect-review action. Notifications also
	// remain in Logs after their on-screen lifetime.
	if n := m.runtimeToastCount(); n > 0 {
		visible := toast.VisibleToasts(m.toasts)
		start := len(visible) - n
		for i := range n {
			at := len(rows) - n + i
			if at >= m.contentStartRow() {
				rows[at] = visible[start+i].Render(m.width, 1)
			}
		}
	}
	if m.mode == modeFilter {
		rows = append(rows, m.styles.filterPromptStyle.Render(inputLine(" / ", m.filterText, m.width)))
	}
	rows = append(rows, m.runtimeFooter())
	base := lipgloss.NewStyle().Foreground(m.styles.rowStyle.GetForeground())
	if m.themeDark {
		base = base.Background(lipgloss.Color("#0D1117"))
	} else {
		base = base.Background(lipgloss.Color("#FFFFFF"))
	}
	text := strings.TrimSuffix(fitRows(rows, m.width, m.height), "\n")
	if m.runtime.Color == "mono" {
		plain := strings.Split(text, "\n")
		for i := range plain {
			plain[i] = stripANSI(plain[i])
		}
		text = strings.Join(plain, "\n")
	} else {
		text = base.Render(text)
	}
	v.SetContent(text + redrawMarker(m.redrawEpoch))
	return v
}

type footerAction struct{ label, key string }

func (m Model) runtimeFooterActions() []footerAction {
	switch {
	case m.export.rawPrompt:
		return []footerAction{{"Enter review", "enter"}, {"Esc cancel", "esc"}}
	case m.controlPanel.preview != nil || m.export.rawReview:
		return []footerAction{{"← previous", "left"}, {"→ next", "right"}, {"Esc cancel", "esc"}}
	case m.controlPanel.form:
		return []footerAction{{"Enter review", "enter"}, {"Esc cancel", "esc"}}
	case m.insights.inputMode:
		return []footerAction{{"Enter query", "enter"}, {"Ctrl+U clear", "ctrl+u"}, {"Esc back", "esc"}}
	case m.inspection.open:
		if m.inspection.entry != nil {
			return []footerAction{{"e metadata", "e"}, {"E raw", "E"}, {"Esc back", "esc"}}
		}
		return []footerAction{{"e metadata", "e"}, {"Esc back", "esc"}}
	case m.mode == modeFilter:
		return []footerAction{{"Enter keep", "enter"}, {"Esc clear", "esc"}, {"Ctrl+U clear", "ctrl+u"}}
	case m.mode == modePalette:
		return []footerAction{{"Enter select", "enter"}, {"Esc back", "esc"}}
	}
	switch m.tab {
	case tabInsights:
		if m.width >= 100 {
			return []footerAction{{"t view", "t"}, {"w window", "w"}, {"r range", "r"}, {"f live", "f"}, {"+ zoom", "+"}, {"− zoom", "-"}, {"i requests", "i"}, {"e export", "e"}, {"^K commands", "ctrl+k"}}
		}
		return []footerAction{{"t view", "t"}, {"+ zoom", "+"}, {"i req", "i"}, {"e save", "e"}, {"^K", "ctrl+k"}}
	case tabControls:
		return []footerAction{{"p pause", "p"}, {"u resume", "u"}, {"r resize", "r"}, {"^K", "ctrl+k"}}
	case tabRequests:
		return []footerAction{{"Enter inspect", "enter"}, {"n wire", "n"}, {"/ filter", "/"}, {"^K", "ctrl+k"}}
	case tabNetwork, tabLogs:
		return []footerAction{{"Enter inspect", "enter"}, {"/ filter", "/"}, {"e save", "e"}, {"^K", "ctrl+k"}}
	}
	return []footerAction{{"Enter explore", "enter"}, {"^K commands", "ctrl+k"}, {"? keys", "?"}}
}
func (m Model) runtimeFooter() string {
	if status := m.exportStatus(); status != "" {
		return m.styles.footerStyle.Render(fitPlain(" "+safeTerminal(status), m.width))
	}
	var labels []string
	for _, a := range m.runtimeFooterActions() {
		labels = append(labels, a.label)
	}
	return m.styles.footerStyle.Render(fitPlain(" "+strings.Join(labels, " · "), m.width))
}
func (m Model) footerActionAt(x int) (string, bool) {
	if m.exportStatus() != "" {
		return "", false
	}
	at := 1
	for _, a := range m.runtimeFooterActions() {
		width := unstyledWidth(a.label)
		if x >= at && x < at+width && x < m.width {
			return a.key, true
		}
		at += width + 3
	}
	return "", false
}
func keyForAction(key string) tea.KeyPressMsg {
	switch key {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEsc}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "ctrl+k":
		return tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	}
	r := []rune(key)
	if len(r) > 0 {
		return tea.KeyPressMsg{Code: r[0], Text: key}
	}
	return tea.KeyPressMsg{}
}

func (m Model) factRowHeight() int {
	if m.width < 70 {
		return 2
	}
	return 1
}
func (m Model) renderCompactNetwork() string {
	scope := m.providerID(m.active)
	if m.networkAll {
		scope = "ALL + INGRESS"
	}
	rows := []string{m.styles.sectionStyle.Render(" NETWORK · " + scope), m.styles.dimStyle2.Render(" Enter evidence · a scope · t type · s status")}
	entries := m.visibleNetworkEntries()
	n := max((m.visibleRows()-3)/m.networkRowHeight(), 1)
	start := min(m.scroll, len(entries))
	end := min(start+n, len(entries))
	for i := start; i < end; i++ {
		e := entries[i]
		style := m.styles.rowStyle
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}
		status := fmt.Sprint(e.StatusCode)
		if e.Aborted {
			status += " abort"
		}
		first := fmt.Sprintf(" %s %-6s %-6s %s", status, e.Method, e.Type(), e.Timing.Duration().Truncate(time.Millisecond))
		path := ""
		if e.URL != nil {
			path = e.URL.Path
		}
		source := m.networkSources[e]
		if source == "" {
			source = m.providerID(m.active)
		}
		if m.networkRowHeight() == 2 {
			rows = append(rows, style.Render(fitPlain(first, m.viewportWidth())), style.Render(fitStyled(skipANSI("   "+source+" "+safeTerminal(path), m.hScroll[tabNetwork]), m.viewportWidth())))
		} else {
			line := fmt.Sprintf(" %-14s %3d %-7s %-7s %9s %s", truncatePlain(source, 14), e.StatusCode, e.Method, e.Type(), e.Timing.Duration().Truncate(time.Millisecond), safeTerminal(path))
			rows = append(rows, style.Render(fitPlain(line, m.viewportWidth())))
		}
	}
	if len(entries) == 0 {
		rows = append(rows, " No network entries yet.")
	}
	for len(rows) < m.visibleRows()-1 {
		rows = append(rows, "")
	}
	rows = append(rows, m.styles.dimStyle2.Render(fmt.Sprintf(" %d–%d / %d", min(start+1, len(entries)), end, len(entries))))
	return fitRows(rows, m.viewportWidth(), m.visibleRows())
}
func (m Model) compactOverviewLines() []string {
	w := m.viewportWidth()
	rows := []string{m.styles.sectionStyle.Render(" OVERVIEW · Enter explores history"), fmt.Sprintf(" %.2f req/s · %d active · %d queued", m.snap.Throughput, m.snap.Active, m.snap.Queued), ""}
	spark := m.snap.Sparkline
	budget := max(w-2, 0)
	if len(spark) > budget {
		spark = spark[len(spark)-budget:]
	}
	copy := m
	copy.snap.Sparkline = spark
	rows = append(rows, m.styles.sectionStyle.Render(" Throughput · 10 seconds"), copy.renderSparkline(), "", m.styles.sectionStyle.Render(" Default pool · occupied / configured"), m.renderGaugeBar(m.displayPoolOccupied(), m.conc, max(w-6, 0)))
	rows = append(rows, m.styles.queueWarnStyle.Render(fmt.Sprintf(" %d queued · oldest %s", m.snap.Queued, m.oldestQueuedAge().Truncate(time.Second))))
	if cb := m.snap.CircuitBreaker; cb != nil {
		style := m.styles.statusOkStyle
		if cb.State != "CLOSED" {
			style = m.styles.queueWarnStyle
		}
		rows = append(rows, style.Render(" Breaker "+cb.State))
	}
	counts := m.snap.StatusCounts
	rows = append(rows, "", m.styles.statusOkStyle.Render(fmt.Sprintf(" 2xx:%d", counts[2]))+m.styles.queueWarnStyle.Render(fmt.Sprintf("  4xx:%d", counts[4]))+m.styles.statusServerErrStyle.Render(fmt.Sprintf("  5xx:%d", counts[5])))
	rows = append(rows, fmt.Sprintf(" %d aborted · %d cancelled", m.snap.TotalAborted, m.snap.TotalCancelled))
	if m.insights.snapshot.History.Enabled {
		health := m.insights.snapshot.History
		status := "healthy"
		if !health.Healthy {
			status = "degraded"
		}
		rows = append(rows, fmt.Sprintf(" History %s · pending %d/%d", status, health.Pending, health.Capacity))
	} else {
		rows = append(rows, m.styles.dimStyle2.Render(" Live ≤15m / 12,000 intervals"))
	}
	if m.visibleRows() > len(rows)+3 {
		rows = append(rows, "", m.styles.sectionStyle.Render(" PROVIDERS · Tab changes scope"))
		for i, p := range m.providers {
			prefix := " "
			if i == m.active {
				prefix = "▸"
			}
			rows = append(rows, fmt.Sprintf(" %s%-15s %3dA %3dQ %4.1f/s", prefix, truncatePlain(m.providerLabel(i), 15), p.snap.Active, p.snap.Queued, p.snap.Throughput))
		}
	}
	if m.visibleRows() > len(rows)+3 {
		rows = append(rows, "", m.styles.sectionStyle.Render(" IN FLIGHT"))
		for _, f := range m.snap.InFlight {
			rows = append(rows, fmt.Sprintf(" %s %s · %s", f.Method, safeTerminal(f.Path), f.Age().Truncate(time.Second)))
		}
	}
	return rows
}

func (m Model) renderCompactOverview() string {
	lines := m.compactOverviewLines()
	start := min(m.scroll, len(lines))
	return fitRows(lines[start:], m.viewportWidth(), m.visibleRows())
}

func (m Model) displayPoolOccupied() int {
	if p, ok := m.selectedControlProvider(); ok {
		for _, pool := range p.Pools {
			if pool.ID == "default" {
				return int(pool.Occupied)
			}
		}
	}
	return int(m.snap.Active)
}

func (m Model) networkRowHeight() int {
	if m.width < 72 {
		return 2
	}
	return 1
}

func (m Model) runtimeToastCount() int {
	if m.mode != modeBrowse || m.inspection.open || m.controlPanel.preview != nil || m.controlPanel.form || m.export.rawReview || m.export.rawPrompt || m.insights.inputMode {
		return 0
	}
	return min(len(toast.VisibleToasts(m.toasts)), min(3, max(m.height-5, 0)))
}
