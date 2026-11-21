package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/investigation"
)

func (m Model) updateRuntime(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		switch {
		case m.export.rawPrompt:
			m.export.input = appendBoundedInput(m.export.input, msg.Content, 8)
		case m.controlPanel.form:
			m.controlPanel.input = appendBoundedInput(m.controlPanel.input, msg.Content, 256)
		case m.insights.inputMode:
			m.insights.input = appendBoundedInput(m.insights.input, msg.Content, 160)
		case m.mode == modePalette:
			m.palette.query = appendBoundedInput(m.palette.query, msg.Content, 256)
			m.filterPaletteCmds()
		case m.mode == modeFilter:
			m.filterText = appendBoundedInput(m.filterText, msg.Content, 512)
			m.cursor = 0
			m.scroll = 0
			if m.tab == tabRequests {
				m.insights.factOffset = 0
				return m, m.requestFacts(), true
			}
		}
		return m, nil, true
	case observabilityTick:
		if m.runtime.Observations != nil {
			m.insights.snapshot = m.runtime.Observations.Snapshot()
		}
		// Once per second, force a full rebuild so an in-place journal
		// eviction (constant length, new entry IDs) is reflected even when
		// no filter control changed. Between ticks the cheap signature gates
		// the work away from mouse motion and other high-rate messages.
		m.networkFilterGen = ""
		if m.runtime.Controls != nil {
			m.controlPanel.snapshot = m.runtime.Controls.Snapshot()
			m.syncControlLimits()
		}
		commands := []tea.Cmd{observabilityTickCmd()}
		if m.tab == tabInsights && m.insights.follow && !m.insights.pending {
			commands = append(commands, m.requestInsights())
		}
		if m.tab == tabRequests && !m.insights.factsPending && !m.inspection.open {
			commands = append(commands, m.requestFacts())
		}
		return m, tea.Batch(commands...), true
	case insightResultMsg:
		if msg.generation != m.insights.generation {
			return m, nil, true
		}
		m.insights.pending = false
		if msg.err != nil {
			m.insights.err = msg.err.Error()
			return m, nil, true
		}
		m.rememberHeatmapSelection()
		m.acceptResources(msg.result)
		m.insights.result = msg.result
		m.alignHeatmapSelection()
		m.insights.err = ""
		a, b := m.insights.axis()
		if m.insights.cursor.Before(a) {
			m.insights.cursor = a
		}
		if !m.insights.cursor.Before(b) || m.insights.follow {
			m.insights.cursor = b.Add(-time.Nanosecond)
		}
		return m, nil, true
	case requestResultMsg:
		if msg.generation != m.insights.factGeneration {
			return m, nil, true
		}
		m.insights.factsPending = false
		if msg.err != nil {
			m.tell(msg.err.Error())
			return m, nil, true
		}
		selected, had := m.selectedFact()
		m.insights.facts = msg.result
		m.insights.acceptedFactQuery = msg.query
		if had {
			for i, f := range msg.result.Requests {
				if f.ID == selected.ID {
					m.cursor = i
					break
				}
			}
		}
		m.adjustViewport()
		return m, nil, true
	case controlResultMsg:
		m.controlPanel.pending = false
		if msg.err != nil {
			m.tell("Control was not applied: " + msg.err.Error())
		} else {
			m.tell(msg.result.Summary)
		}
		if m.runtime.Controls != nil {
			m.controlPanel.snapshot = m.runtime.Controls.Snapshot()
			m.syncControlLimits()
		}
		return m, nil, true
	case exportResultMsg:
		m.export.pending = false
		if msg.err != nil {
			m.tell("Export failed: " + msg.err.Error())
		} else {
			m.tell("Saved " + msg.path)
			title := "EXPORT SAVED"
			if msg.raw {
				title = "RAW EXPORT SAVED"
			}
			m.inspection = inspectionState{open: true, title: title, rawLines: m.savedExportLines(msg.path, msg.raw)}
			m.reflowInspection()
		}
		return m, nil, true
	case tea.WindowSizeMsg:
		m.rememberHeatmapSelection()
		m.width, m.height = msg.Width, msg.Height
		m.alignHeatmapSelection()
		m.controlPanel.pressed = -1
		m.export.pressed = -1
		m.insights.dragging = false
		m.dragging = false
		if m.controlPanel.preview != nil {
			m.controlPanel.page = 0
			m.controlPanel.seenThrough = 0
			m.controlPanel.focus = 0
		}
		if m.export.rawReview {
			m.export.page = 0
			m.export.seenThrough = 0
			m.export.focus = 0
		}
		m.reflowInspection()
		m.dashboardLinesCache = nil
		m.adjustViewport()
		if m.tab == tabInsights {
			return m, m.requestInsights(), true
		}
		return m, nil, true
	case tea.BlurMsg:
		m.controlPanel.pressed = -1
		m.export.pressed = -1
		m.insights.dragging = false
		m.dragging = false
		return m, nil, true
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.cancelQueries()
			m.flushPendingLogs()
			return m, tea.Quit, true
		}
		if next, cmd, ok := m.handleExportKey(msg); ok {
			return next, cmd, true
		}
		if next, cmd, ok := m.handleControlKey(msg); ok {
			return next, cmd, true
		}
		if next, cmd, ok := m.handleInspectionKey(msg); ok {
			return next, cmd, true
		}
		if m.mode == modePalette || m.mode == modeConfirm || m.mode == modeHelp {
			return m, nil, false
		}
		if next, cmd, ok := m.handleFactKey(msg); ok {
			return next, cmd, true
		}
		if m.mode == modeFilter {
			return m, nil, false
		}
		if next, cmd, ok := m.handleInsightKey(msg); ok {
			return next, cmd, true
		}
		switch msg.String() {
		case "a":
			if m.tab == tabNetwork {
				m.networkAll = !m.networkAll
				m.cursor = 0
				m.scroll = 0
				return m, nil, true
			}
		case "q":
			m.cancelQueries()
			m.flushPendingLogs()
			return m, tea.Quit, true
		case "tab", "shift+tab":
			if len(m.providers) == 0 {
				return m, nil, true
			}
			delta := 1
			if msg.String() == "shift+tab" {
				delta = -1
			}
			m.active = (m.active + delta + len(m.providers)) % len(m.providers)
			m.syncActive()
			m.cursor = 0
			m.scroll = 0
			m.insights.factOffset = 0
			if m.tab == tabInsights {
				m.insights.all = false
				return m, m.requestInsights(), true
			}
			if m.tab == tabRequests {
				return m, m.requestFacts(), true
			}
			return m, nil, true
		case "7":
			m.switchTab(tabInsights)
			return m, m.requestInsights(), true
		case "8":
			m.switchTab(tabControls)
			return m, nil, true
		case "2":
			m.switchTab(tabRequests)
			m.insights.cohort = false
			return m, m.requestFacts(), true
		case "?":
			m.openRuntimeHelp()
			return m, nil, true
		case "ctrl+k":
			m.openPalette()
			return m, nil, true
		case "enter", " ", "space":
			if m.tab == tabNetwork {
				entries := m.visibleNetworkEntries()
				if m.cursor < len(entries) {
					m.openNetwork(entries[m.cursor])
				}
				return m, nil, true
			}
			if m.tab == tabDashboard || m.tab == tabConcurrency || m.tab == tabRoutes {
				m.insights.view = insightRates
				if m.tab == tabConcurrency {
					m.insights.view = insightCapacity
					m.insights.all = false
				}
				m.switchTab(tabInsights)
				return m, m.requestInsights(), true
			}
			if m.tab == tabLogs {
				if item := m.logItemAtCursor(); item != nil {
					m.inspection = inspectionState{open: true, title: "LOG MESSAGE", rawLines: strings.Split(item.text, "\n")}
					m.reflowInspection()
				}
				return m, nil, true
			}
		case "e":
			if m.tab == tabNetwork {
				return m, m.beginExport("network"), true
			}
			if m.tab == tabLogs {
				return m, m.beginExport("logs"), true
			}
		case "E":
			if m.tab == tabNetwork {
				return m, m.beginRawExport(), true
			}
		}
	case tea.MouseClickMsg:
		return m.runtimeMouseClick(msg)
	case tea.MouseMotionMsg:
		if m.insights.dragging {
			m.insights.cursor = m.chartTime(msg.X)
			m.insights.brushEnd = m.insights.cursor
			return m, nil, true
		}
		if m.controlPanel.preview != nil || m.export.rawReview || m.export.rawPrompt || m.controlPanel.form || m.inspection.open {
			return m, nil, true
		}
	case tea.MouseReleaseMsg:
		if m.controlPanel.preview != nil {
			hit := m.controlButtonAt(msg.X, msg.Y)
			armed := m.controlPanel.pressed
			m.controlPanel.pressed = -1
			if msg.Button == tea.MouseLeft && hit >= 0 && hit == armed {
				if hit == 0 {
					m.controlPanel.preview = nil
				} else {
					return m, m.applyControl(), true
				}
			}
			return m, nil, true
		}
		if m.export.rawReview {
			hit := m.rawButtonAt(msg.X, msg.Y)
			armed := m.export.pressed
			m.export.pressed = -1
			if msg.Button == tea.MouseLeft && hit >= 0 && hit == armed {
				if hit == 0 {
					m.export.rawReview = false
					m.export.selection = investigation.Selection{}
				} else {
					return m, m.writeRaw(), true
				}
			}
			return m, nil, true
		}
		if m.insights.dragging {
			m.insights.dragging = false
			if absInt(msg.X-m.insights.dragX) <= 1 {
				m.insights.brushStart = time.Time{}
				m.insights.brushEnd = time.Time{}
			}
			return m, nil, true
		}
	case tea.MouseWheelMsg:
		if m.mode == modePalette {
			d := 3
			if msg.Button == tea.MouseWheelUp {
				d = -3
			}
			m.movePaletteCursor(d)
			return m, nil, true
		}
		if m.export.rawReview {
			code := tea.KeyDown
			if msg.Button == tea.MouseWheelUp {
				code = tea.KeyUp
			}
			return m.handleExportKey(tea.KeyPressMsg{Code: code})
		}
		if m.controlPanel.preview != nil {
			d := 1
			if msg.Button == tea.MouseWheelUp {
				d = -1
			}
			m.moveReviewPage(d)
			return m, nil, true
		}
		if m.inspection.open {
			d := 3
			if msg.Button == tea.MouseWheelUp {
				d = -3
			}
			m.inspection.scroll = max(0, min(m.inspection.scroll+d, len(m.inspection.lines)-m.inspectionRows()))
			return m, nil, true
		}
		if m.export.rawReview || m.export.rawPrompt || m.controlPanel.form {
			return m, nil, true
		}
		if msg.Y == 0 && len(m.providers) > 0 {
			d := 1
			if msg.Button == tea.MouseWheelUp {
				d = -1
			}
			m.active = (m.active + d + len(m.providers)) % len(m.providers)
			m.syncActive()
			m.cursor = 0
			if m.tab == tabInsights {
				m.insights.all = false
				return m, m.requestInsights(), true
			}
			if m.tab == tabRequests {
				return m, m.requestFacts(), true
			}
			return m, nil, true
		}
		if m.tab == tabInsights {
			g := m.insightGeometry()
			if msg.X < g.x || msg.X >= g.x+g.w || msg.Y < g.y || msg.Y >= g.y+g.h {
				return m, nil, true
			}
			if msg.Button == tea.MouseWheelUp {
				m.insightZoom(.5)
			} else if msg.Button == tea.MouseWheelDown {
				m.insightZoom(2)
			}
			return m, m.requestInsights(), true
		}
	}
	return m, nil, false
}
func (m Model) runtimeMouseClick(msg tea.MouseClickMsg) (Model, tea.Cmd, bool) {
	if msg.Button != tea.MouseLeft && msg.Button != tea.MouseNone {
		return m, nil, true
	}
	if msg.Y == m.height-1 {
		if key, ok := m.footerActionAt(msg.X); ok {
			next, cmd := m.Update(keyForAction(key))
			return next.(Model), cmd, true
		}
		return m, nil, true
	}
	if m.mode == modePalette {
		if index, ok := m.paletteItemAt(msg.X, msg.Y); ok {
			m.palette.cursor = index
			cmd := m.executePaletteCmd(m.palette.cmds[index])
			return m, cmd, true
		}
		return m, nil, true
	}
	if m.controlPanel.preview != nil {
		m.controlPanel.pressed = m.controlButtonAt(msg.X, msg.Y)
		if m.controlPanel.pressed >= 0 {
			m.controlPanel.focus = m.controlPanel.pressed
		}
		return m, nil, true
	}
	if m.export.rawReview {
		m.export.pressed = m.rawButtonAt(msg.X, msg.Y)
		if m.export.pressed >= 0 {
			m.export.focus = m.export.pressed
		}
		return m, nil, true
	}
	if m.export.rawPrompt || m.controlPanel.form || m.insights.inputMode {
		return m, nil, true
	}
	if m.inspection.open {
		if msg.Y == m.contentStartRow()+1 && m.inspection.entry != nil {
			m.inspection.section = (m.inspection.section + 1) % 7
			m.inspection.scroll = 0
			m.rebuildInspection()
		}
		return m, nil, true
	}
	if msg.X < 0 || msg.X >= m.width || msg.Y < 0 || msg.Y >= m.height {
		return m, nil, true
	}
	if n := m.runtimeToastCount(); n > 0 && msg.Y >= m.height-1-n && msg.Y < m.height-1 {
		return m, nil, true
	}
	if msg.Y == 0 {
		if target, ok := m.runtimeProviderAt(msg.X); ok {
			m.active = target
			m.syncActive()
			m.cursor = 0
			m.scroll = 0
		} else {
			return m, nil, true
		}
		if m.tab == tabInsights {
			m.insights.all = false
			return m, m.requestInsights(), true
		}
		if m.tab == tabRequests {
			return m, m.requestFacts(), true
		}
		return m, nil, true
	}
	if msg.Y == m.headerRowCount() {
		if id, ok := m.adaptiveTabAt(msg.X); ok {
			m.switchTab(id)
			if id == tabInsights {
				return m, m.requestInsights(), true
			}
			if id == tabRequests {
				return m, m.requestFacts(), true
			}
		}
		return m, nil, true
	}
	if m.tab == tabInsights {
		g := m.insightGeometry()
		if msg.Y == g.y+g.h+1 {
			a, b := m.insights.axis()
			for _, marker := range m.insights.result.Markers {
				if !b.After(a) {
					continue
				}
				x := g.x + int(float64(marker.At.Sub(a))/float64(b.Sub(a))*float64(g.w))
				if absInt(msg.X-x) <= 1 {
					m.openMarker(marker)
					break
				}
			}
			return m, nil, true
		}
		if msg.Y >= g.y && msg.Y < g.y+g.h && msg.X >= g.x && msg.X < g.x+g.w {
			if m.insights.view == insightDistribution {
				start := max(0, min(m.insights.bin-g.h/2, 23-g.h))
				m.insights.bin = min(start+msg.Y-g.y, 22)
				return m, nil, true
			}
			if m.insights.view == insightCompare {
				row := (msg.Y-g.y)/m.comparisonRowHeight() + m.comparisonStart()
				m.insights.series = min(row, max(len(m.insights.result.ByProvider)-1, 0))
				return m, nil, true
			}
			m.insights.cursor = m.chartTime(msg.X)
			m.insights.follow = false
			m.insights.dragging = true
			m.insights.dragX = msg.X
			m.insights.brushStart = m.insights.cursor
			m.insights.brushEnd = m.insights.cursor
			if m.insights.view == insightLatency {
				m.insights.bin = msg.Y - g.y
				m.insights.binSelection = nil
			}
			return m, nil, true
		}
		if msg.Y == m.contentStartRow() {
			m.insights.view = (m.insights.view + 1) % len(insightNames)
			m.resetInsightView()
			return m, nil, true
		}
		if msg.Y == m.contentStartRow()+2 {
			return m.handleInsightKey(keyForAction("r"))
		}
		if msg.Y == m.contentStartRow()+1 {
			m.insights.all = !m.insights.all
			return m, m.requestInsights(), true
		}
		if msg.Y == m.contentStartRow()+4 {
			if m.insights.view == insightLatency || m.insights.view == insightDistribution {
				m.insights.metric = (m.insights.metric + 1) % 3
				m.insights.binSelection = nil
				return m, nil, true
			}
			if m.insights.view == insightCompare {
				return m, nil, true
			}
			names := m.legendNames()
			x := 1
			for i, n := range names {
				if n == "" {
					continue
				}
				label := "●" + n + " "
				if m.width < 60 {
					label = "●" + truncatePlain(n, 5) + " "
				}
				w := unstyledWidth(label)
				if msg.X >= x && msg.X < x+w {
					m.insights.series = i
					m.insights.hidden[i] = !m.insights.hidden[i]
					break
				}
				x += w
			}
			return m, nil, true
		}
		return m, nil, true
	}
	if m.tab == tabRequests && m.runtime.Observations != nil {
		row := msg.Y - m.contentStartRow() - m.requestHeaderRows()
		if row >= 0 {
			row /= m.factRowHeight()
			m.cursor = min(m.scroll+row, max(len(m.insights.facts.Requests)-1, 0))
			m.adjustViewport()
		}
		return m, nil, true
	}
	if m.tab == tabNetwork && m.width < 72 {
		row := (msg.Y - m.contentStartRow() - 2)
		if row >= 0 {
			m.cursor = min(m.scroll+row/m.networkRowHeight(), m.maxCursor())
			m.adjustViewport()
		}
		return m, nil, true
	}
	if m.tab == tabControls {
		row := msg.Y - m.contentStartRow() - m.controlPoolRowOffset()
		if p, ok := m.selectedControlProvider(); ok && row >= 0 && row+m.controlPoolStart(len(p.Pools)) < len(p.Pools) {
			m.cursor = row + m.controlPoolStart(len(p.Pools))
		}
		return m, nil, true
	}
	if m.tab == tabDashboard && msg.Y >= m.contentStartRow() && msg.Y < m.contentStartRow()+8 {
		m.insights.view = insightRates
		if msg.Y >= m.contentStartRow()+4 {
			m.insights.view = insightCapacity
			m.insights.all = false
		}
		m.switchTab(tabInsights)
		return m, m.requestInsights(), true
	}
	return m, nil, false
}
func (m *Model) openRuntimeHelp() {
	m.inspection = inspectionState{open: true, title: "KEYS & MOUSE", rawLines: []string{
		"NAVIGATION", "1–8 switch workspace. Tab / Shift+Tab changes provider without leaving the workspace. Ctrl+K opens the searchable command palette. q or Ctrl+C quits outside text entry.", "", "INSIGHTS", "t / T next / previous visualization; v chooses client/admission/attempt duration, a capacity resource or usage/attempt series families. a selects fleet/provider. w cycles 1m, 5m, 15m, 1h, 6h, 24h, 7d, 30d, 90d, 365d. r accepts two absolute RFC3339 times. f follows or pauses the display.", "h/l or arrows move the time cursor. H/L or PgUp/PgDn pan. +/- zoom about the cursor. j/k selects a series or histogram row; Space toggles a series. z expands all heatmap bins.", "b begins a brush, arrows extend it, Enter finishes. Mouse drag does the same. Enter inspects exact observations. i opens the matching request cohort. e exports the selected whole intervals. m inspects timeline events; clicking event markers does the same.", "", "REQUESTS", "/ accepts text plus outcome:failed, stage:conversion_request, provider:NAME or live:true. f toggles failed only. a changes fleet/provider. [ and ] page through retained facts. B leaves a time cohort. n opens the exact Network request; x reviews cancellation of a live ID.", "", "NETWORK", "a includes every provider and pre-provider ingress failures. Enter opens retained evidence. Tab cycles HTTP, decisions, four byte legs and contracts. Scroll reads every retained section. e exports metadata; E opens a typed RAW consent and paged review for the selected request.", "", "RUNTIME CONTROLS", "p pauses new admission, u resumes, d drains. P/D adds an expiry. Select a pool then r or +/- edits its positive limit and optional expiry. Every change opens a revision-bound review. Read every page, then Tab from Cancel to Apply. No action is triggered by drawing the review.", "", "SMALL PANES", "All destinations remain in the numbered tab rail. Forms and reviews use the full pane. Long evidence wraps and scrolls. Resize preserves time selection, query filters and the selected request; resizing a review resets its read-through state and clears any mouse press.", "", "EXPORTS", fmt.Sprintf("Destination: %s", m.runtime.ExportDir), "e exports a frozen selection to a new file. Request and log exports include their displayed metadata; metadata investigation bundles remove arbitrary headers, bodies, URLs and free error text. Raw investigation exports require explicit consent.", "", "COLOR", "Blue indicates selection/information, green successful work, orange queue/held/warning, red failed work. Text and glyphs carry the same distinctions in monochrome. -tui-theme auto|dark|light and -tui-color auto|truecolor|256|16|mono override negotiation."}}
	m.reflowInspection()
}

func (m *Model) syncControlLimits() {
	for i := range m.providers {
		for _, p := range m.controlPanel.snapshot.Providers {
			if p.Name != m.providerID(i) {
				continue
			}
			for _, pool := range p.Pools {
				if pool.ID == "default" {
					m.providers[i].conc = pool.Limit
				}
			}
		}
	}
	m.syncActive()
}

func appendBoundedInput(current, pasted string, limit int) string {
	for _, r := range pasted {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if r < 32 || r == 127 || (r >= 128 && r <= 159) {
			continue
		}
		if len(current)+len(string(r)) > limit {
			break
		}
		current += string(r)
	}
	return current
}
