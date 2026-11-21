package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/rivo/uniseg"
)

// RuntimeOptions connects the terminal to the same typed observations and local
// controls used by the gateway. A nil hub retains the embeddable legacy model.
// Theme and Color are explicit command-line preferences; auto uses Bubble Tea's
// terminal negotiation, including tmux. No terminal capabilities are assumed.
type RuntimeOptions struct {
	IngressJournal *journal.Journal

	Observations *observation.Hub
	Controls     *control.Controller
	Theme        string
	Color        string
	ExportDir    string
}

func (m Model) WithRuntime(o RuntimeOptions) Model {
	m.runtime = o
	if o.Theme == "light" {
		m.themeDark = false
	} else {
		m.themeDark = true
	}
	m.styles = newTheme(m.themeDark)
	m.applyScrollbarTheme()
	if o.Controls != nil {
		m.controlPanel.snapshot = o.Controls.Snapshot()
	}
	return m
}
func (m Model) providerID(i int) string {
	if i >= 0 && i < len(m.providers) && m.providers[i].id != "" {
		return m.providers[i].id
	}
	return m.providerLabel(i)
}
func (m Model) enriched() bool { return m.runtime.Observations != nil || m.runtime.Controls != nil }
func (m Model) availableTabs() []string {
	if m.enriched() {
		return tabNames
	}
	return tabNames[:6]
}
func (m Model) colorOptions() []tea.ProgramOption {
	p := map[string]colorprofile.Profile{"truecolor": colorprofile.TrueColor, "256": colorprofile.ANSI256, "16": colorprofile.ANSI, "mono": colorprofile.Ascii}
	if v, ok := p[m.runtime.Color]; ok {
		return []tea.ProgramOption{tea.WithColorProfile(v)}
	}
	return nil
}

// A tab occupies exactly the same cells for painting and pointer input. On a
// small pane all eight destinations remain visible: the current tab gets its
// name and the other destinations retain their numbered keyboard targets.
type tabCell struct {
	id       tabID
	text     string
	x, width int
}

func (m Model) adaptiveTabs() []tabCell {
	names := m.availableTabs()
	if m.width < 3*len(names)+2 {
		if m.width < 5 {
			return []tabCell{{m.tab, truncatePlain(fmt.Sprintf("%d", m.tab+1), max(m.width, 0)), 0, max(m.width, 0)}}
		}
		label := fitPlain(names[m.tab], m.width-4)
		count := tabID(len(names))
		return []tabCell{{(m.tab + count - 1) % count, "‹ ", 0, 2}, {m.tab, label, 2, m.width - 4}, {(m.tab + 1) % count, " ›", m.width - 2, 2}}
	}
	full := 0
	for _, n := range names {
		full += uniseg.StringWidth(n) + 2
	}
	var cells []tabCell
	if full <= m.width {
		x := 0
		for i, n := range names {
			w := uniseg.StringWidth(n) + 2
			cells = append(cells, tabCell{tabID(i), " " + n + " ", x, w})
			x += w
		}
		return cells
	}
	// Every other tab takes three cells. Remaining cells belong to the active
	// name; this keeps its focus visible after every resize, even at 40 columns.
	nameBudget := max(m.width-3*(len(names)-1), 3)
	x := 0
	for i, n := range names {
		text := fmt.Sprintf(" %d ", i+1)
		if tabID(i) == m.tab {
			text = " " + truncatePlain(n, max(nameBudget-2, 1)) + " "
		}
		w := uniseg.StringWidth(text)
		if x+w > m.width {
			break
		}
		cells = append(cells, tabCell{tabID(i), text, x, w})
		x += w
	}
	return cells
}
func (m Model) renderAdaptiveTabs() string {
	var b strings.Builder
	for _, c := range m.adaptiveTabs() {
		style := m.styles.tabInactiveStyle.Padding(0)
		if c.id == m.tab {
			style = m.styles.tabActiveStyle.Padding(0)
		}
		b.WriteString(style.Render(c.text))
	}
	return b.String()
}
func (m Model) adaptiveTabAt(x int) (tabID, bool) {
	for _, c := range m.adaptiveTabs() {
		if x >= c.x && x < c.x+c.width {
			return c.id, true
		}
	}
	return 0, false
}

type runtimeProviderChip struct {
	index, x, width int
	label           string
}

func (m Model) runtimeHeaderParts() (string, []runtimeProviderChip, string) {
	name := m.providerLabel(m.active)
	if len(m.providers) == 1 && m.providers[0].name == "" {
		name = "⚡ shaper"
	}
	if name == "" {
		name = "shaper"
	}
	health := "LIVE"
	if m.insights.snapshot.History.Enabled {
		health = "DB"
		if !m.insights.snapshot.History.Healthy {
			health = "DB!"
		}
	}
	right := fmt.Sprintf(" %d active %d wait %s ", m.snap.Active, m.snap.Queued, health)
	brand := " shaper · " + name + " ↕"
	if m.width < 60 {
		brand = " " + name + " ↕"
		right = fmt.Sprintf(" %dA %dQ %s ", m.snap.Active, m.snap.Queued, health)
	}
	if m.width < 80 || len(m.providers) < 2 {
		return brand, nil, right
	}
	var active, queued int64
	var open int
	busy := 0
	for i, p := range m.providers {
		active += p.snap.Active
		queued += p.snap.Queued
		if p.snap.CircuitBreaker != nil && p.snap.CircuitBreaker.State == "OPEN" {
			open++
		}
		if p.snap.Active > m.providers[busy].snap.Active {
			busy = i
		}
	}
	right = fmt.Sprintf(" %d active · %d queued · %d OPEN %s ", active, queued, open, health)
	if m.width >= 120 {
		right = fmt.Sprintf(" %d active · %d queued · %d OPEN · busiest: %s %s ", active, queued, open, truncatePlain(m.providerLabel(busy), 16), health)
	}
	brand = " ⚡ shaper "
	budget := m.width - unstyledWidth(brand) - unstyledWidth(right)
	if budget < 12 {
		right = fmt.Sprintf(" %dA %dQ %d! %s ", active, queued, open, health)
		budget = m.width - unstyledWidth(brand) - unstyledWidth(right)
	}
	start := 0
	// Keep the active provider in the visible window as a fleet outgrows a pane.
	required := 0
	for i := 0; i <= m.active && i < len(m.providers); i++ {
		required += min(unstyledWidth(m.providerLabel(i))+3, 26)
	}
	if required > budget {
		start = m.active
	}
	var chips []runtimeProviderChip
	x := unstyledWidth(brand)
	for i := start; i < len(m.providers) && budget >= 4; i++ {
		label := truncatePlain(safeTerminal(m.providerLabel(i)), min(23, budget-3))
		width := unstyledWidth(label) + 2
		chips = append(chips, runtimeProviderChip{i, x, width, label})
		x += width + 1
		budget -= width + 1
	}
	return brand, chips, right
}
func (m Model) runtimeHeader() string {
	brand, chips, right := m.runtimeHeaderParts()
	left := brand
	if len(chips) > 0 {
		for _, c := range chips {
			style := m.styles.chipInactiveStyle
			if c.index == m.active {
				style = m.styles.chipActiveStyle
			}
			left += style.Render(c.label) + " "
		}
	}
	w := max(m.width-unstyledWidth(right), 0)
	left = fitStyled(left, w)
	return m.styles.headerStyle.Padding(0).Render(left + truncatePlain(right, m.width-w))
}
func (m Model) runtimeProviderAt(x int) (int, bool) {
	brand, chips, right := m.runtimeHeaderParts()
	for _, c := range chips {
		if x >= c.x && x < c.x+c.width {
			return c.index, true
		}
	}
	identity := unstyledWidth(brand)
	if len(chips) == 0 {
		identity = min(identity, max(m.width-unstyledWidth(right), 0))
	}
	if x >= 0 && x < identity && len(m.providers) > 0 {
		return (m.active + 1) % len(m.providers), true
	}
	return 0, false
}

func fitPlain(s string, w int) string {
	s = truncatePlain(s, max(w, 0))
	return s + strings.Repeat(" ", max(w-uniseg.StringWidth(s), 0))
}
func fitStyled(s string, w int) string {
	s = truncateANSI(s, max(w, 0))
	return s + strings.Repeat(" ", max(w-uniseg.StringWidth(stripANSI(s)), 0))
}

// safeTerminal preserves all printable text while making terminal control
// bytes visible. Raw local evidence is inspectable without executing OSC/CSI.
func safeTerminal(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r < 32 || r == 127 || (r >= 0x80 && r <= 0x9f):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (m *Model) tell(s string) { m.statusText = s; m.statusUntil = time.Now().Add(8 * time.Second) }

type observabilityTick struct{}

func observabilityTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return observabilityTick{} })
}

type insightResultMsg struct {
	generation uint64
	result     observation.QueryResult
	err        error
}
type requestResultMsg struct {
	query      observation.RequestQuery
	generation uint64
	result     observation.RequestResult
	err        error
}

func (m *Model) requestInsights() tea.Cmd {
	if m.runtime.Observations == nil {
		return nil
	}
	if m.insights.cancel != nil {
		m.insights.cancel()
	}
	m.insights.generation++
	generation := m.insights.generation
	if m.insights.follow {
		m.insights.to = time.Now()
		m.insights.from = m.insights.to.Add(-m.insights.span)
	}
	q := observation.Query{From: m.insights.from, To: m.insights.to, MaxPoints: min(max(m.width*2, 40), 800)}
	if !m.insights.all {
		q.Providers = []string{m.providerID(m.active)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	m.insights.cancel = cancel
	m.insights.pending = true
	hub := m.runtime.Observations
	return func() tea.Msg {
		defer cancel()
		result, err := hub.Query(ctx, q)
		return insightResultMsg{generation, result, err}
	}
}
func (m *Model) requestFacts() tea.Cmd {
	if m.runtime.Observations == nil {
		return nil
	}
	if m.insights.factCancel != nil {
		m.insights.factCancel()
	}
	m.insights.factGeneration++
	generation := m.insights.factGeneration
	q := m.factQuery()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	m.insights.factCancel = cancel
	m.insights.factsPending = true
	hub := m.runtime.Observations
	return func() tea.Msg {
		defer cancel()
		v, err := hub.Requests(ctx, q)
		return requestResultMsg{query: q, generation: generation, result: v, err: err}
	}
}
func (m Model) factQuery() observation.RequestQuery {
	q := observation.RequestQuery{Limit: 200, Offset: m.insights.factOffset}
	if !m.insights.all {
		q.Providers = []string{m.providerID(m.active)}
	}
	if m.insights.cohort {
		q.From, q.To = m.insights.selectionRange()
		q.DurationKind = m.insights.durationKind
		q.DurationMinNS = m.insights.durationMin
		q.DurationMaxNS = m.insights.durationMax
	}
	var free []string
	for p := range strings.FieldsSeq(m.filterText) {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			free = append(free, p)
			continue
		}
		switch k {
		case "outcome":
			q.Outcome = observation.Outcome(v)
		case "stage":
			q.Stage = observation.Stage(v)
		case "provider":
			q.Providers = []string{v}
		case "live":
			q.LiveOnly = v == "true"
		default:
			free = append(free, p)
		}
	}
	q.Search = strings.Join(free, " ")
	return q
}

func (m *Model) cancelQueries() {
	if m.insights.cancel != nil {
		m.insights.cancel()
	}
	if m.insights.factCancel != nil {
		m.insights.factCancel()
	}
}
