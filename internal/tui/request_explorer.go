package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/rivo/uniseg"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func cancelCommand(f observation.RequestFact) control.Command {
	return control.Command{Action: control.Cancel, Provider: f.Provider, RequestID: f.ID}
}
func (m Model) selectedFact() (observation.RequestFact, bool) {
	if m.cursor >= 0 && m.cursor < len(m.insights.facts.Requests) {
		return m.insights.facts.Requests[m.cursor], true
	}
	return observation.RequestFact{}, false
}
func (m Model) handleFactKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if m.tab != tabRequests || m.runtime.Observations == nil {
		return m, nil, false
	}
	if m.mode == modeFilter {
		switch msg.String() {
		case "esc":
			m.mode = modeBrowse
			m.filterText = ""
		case "enter":
			m.mode = modeBrowse
		case "backspace", "ctrl+h":
			m.filterText = dropGrapheme(m.filterText)
		case "ctrl+u":
			m.filterText = ""
		default:
			if msg.Key().Text != "" && len(m.filterText) < 512 {
				m.filterText += msg.Key().Text
			}
		}
		m.insights.factOffset = 0
		m.cursor = 0
		return m, m.requestFacts(), true
	}
	switch msg.String() {
	case "enter", " ", "space":
		if f, ok := m.selectedFact(); ok {
			m.openFact(f)
		}
		return m, nil, true
	case "n":
		if f, ok := m.selectedFact(); ok {
			if !m.openJoinedNetwork(f.ID) {
				m.openFact(f)
			}
		}
		return m, nil, true
	case "x":
		if f, ok := m.selectedFact(); ok && f.Live {
			return m, m.previewControl(cancelCommand(f)), true
		}
		m.tell("Only a live exact request ID can be cancelled")
		return m, nil, true
	case "e":
		return m, m.beginExport("requests"), true
	case "/":
		m.mode = modeFilter
		m.filterText = ""
		return m, nil, true
	case "a":
		m.insights.all = !m.insights.all
		m.insights.factOffset = 0
		m.cursor = 0
		return m, m.requestFacts(), true
	case "[":
		m.insights.factOffset = max(0, m.insights.factOffset-200)
		m.cursor = 0
		return m, m.requestFacts(), true
	case "]":
		if m.insights.factOffset+len(m.insights.facts.Requests) < m.insights.facts.Total {
			m.insights.factOffset += 200
			m.cursor = 0
			return m, m.requestFacts(), true
		}
		return m, nil, true
	case "f":
		if m.filterText == "outcome:failed" {
			m.filterText = ""
		} else {
			m.filterText = "outcome:failed"
		}
		m.insights.factOffset = 0
		m.cursor = 0
		return m, m.requestFacts(), true
	case "B":
		m.insights.cohort = false
		m.insights.factOffset = 0
		return m, m.requestFacts(), true
	case "7":
		m.switchTab(tabInsights)
		return m, m.requestInsights(), true
	}
	return m, nil, false
}
func (m Model) renderRequestExplorer() string {
	w, h := m.viewportWidth(), m.visibleRows()
	q := m.displayedFactQuery()
	scope := "ALL PROVIDERS"
	if len(q.Providers) > 0 {
		scope = strings.Join(q.Providers, ", ")
	}
	if !q.From.IsZero() {
		a, b := q.From, q.To
		scope += " · " + a.UTC().Format("15:04:05") + "–" + b.UTC().Format("15:04:05")
	}
	rows := []string{m.styles.sectionStyle.Render(" REQUESTS · " + scope)}
	if m.insights.factsPending {
		rows = append(rows, m.styles.queueWarnStyle.Render(" Querying · rows retain previous scope"))
	} else if filter := requestFilterLabel(q); filter != "" {
		rows = append(rows, m.styles.queueWarnStyle.Render(" / "+filter))
	} else {
		hint := " / filter · f failed · n network · x cancel"
		if w < 60 {
			hint = " / filter · f failed · n network"
		}
		rows = append(rows, m.styles.dimStyle2.Render(hint))
	}
	if q.DurationKind == "admission" || q.DurationKind == "attempt" {
		rows = append(rows, m.styles.dimStyle2.Render(" "+q.DurationKind+": logical finish; live start"))
	}
	if w < 70 {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" TIME     CODE OUTCOME   DURATION"))
	} else {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" TIME     PROVIDER       CODE OUTCOME    DURATION  ATTEMPTS METHOD REQUEST"))
	}
	facts := m.insights.facts.Requests
	space := max((h-m.requestHeaderRows()-1)/m.factRowHeight(), 1)
	start := max(0, min(m.scroll, len(facts)))
	end := min(start+space, len(facts))
	for i := start; i < end; i++ {
		f := facts[i]
		state := string(f.Outcome)
		if f.Live {
			state = "active"
			if f.Queued {
				state = "queued"
			}
		}
		age := time.Duration(f.DurationNS)
		if f.Live {
			age = time.Since(f.Started)
		}
		status := "—"
		if f.Status != 0 {
			status = fmt.Sprint(f.Status)
		}
		path := safeTerminal(f.Path)
		if path == "" {
			path = f.Method
		}
		var line string
		if w < 70 {
			line = fmt.Sprintf(" %s %4s %-9s %8s", f.Started.UTC().Format("15:04:05"), status, state, age.Truncate(time.Millisecond))
		} else {
			line = fmt.Sprintf(" %s %-14s %4s %-9s %9s %8d %s", f.Started.UTC().Format("15:04:05"), truncatePlain(f.Provider, 14), status, state, age.Truncate(time.Millisecond), f.AttemptCount, f.Method+" "+path)
		}
		style := m.styles.rowStyle
		if f.Outcome == observation.OutcomeFailed {
			style = m.styles.statusServerErrStyle
		}
		if f.Queued {
			style = m.styles.queueWarnStyle
		}
		if i == m.cursor {
			style = m.styles.rowSelectedStyle
		}
		rows = append(rows, style.Render(fitPlain(line, w)))
		if m.factRowHeight() == 2 {
			rows = append(rows, style.Render(fitPlain("   "+f.Provider+" "+f.Method+" "+path, w)))
		}
	}
	if len(facts) == 0 {
		rows = append(rows, " No matching requests in this selection.")
		if m.insights.factsPending {
			rows = append(rows, " Querying…")
		}
	}
	for len(rows) < h-1 {
		rows = append(rows, "")
	}
	tail := fmt.Sprintf(" %d–%d / %d · [ ] page", q.Offset+min(start+1, len(facts)), q.Offset+end, m.insights.facts.Total)
	if m.insights.factsPending {
		tail += " · querying"
	}
	if m.insights.facts.Truncated {
		tail += " · more results"
	}
	rows = append(rows, m.styles.dimStyle2.Render(tail))
	return fitRows(rows, w, h)
}
func requestFilterLabel(q observation.RequestQuery) string {
	parts := []string{q.Search}
	if q.Outcome != "" {
		parts = append(parts, "outcome:"+string(q.Outcome))
	}
	if q.Stage != "" {
		parts = append(parts, "stage:"+string(q.Stage))
	}
	if q.LiveOnly {
		parts = append(parts, "live:true")
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
func inputLine(prefix, value string, w int) string {
	budget := max(w-len(prefix)-1, 1)
	value = safeTerminal(value)
	if unstyledWidth(value) > budget {
		value = "…" + stripANSI(skipANSI(value, unstyledWidth(value)-budget+1))
	}
	return prefix + value + "█"
}
func unstyledWidth(s string) int { return uniseg.StringWidth(stripANSI(s)) }

func (m Model) displayedFactQuery() observation.RequestQuery {
	if m.insights.acceptedFactQuery.Limit != 0 {
		return m.insights.acceptedFactQuery
	}
	return m.factQuery()
}

func (m Model) requestHeaderRows() int {
	q := m.displayedFactQuery()
	if q.DurationKind == "admission" || q.DurationKind == "attempt" {
		return 4
	}
	return 3
}
