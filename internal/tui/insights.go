package tui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

const (
	insightRates = iota
	insightLatency
	insightCapacity
	insightDistribution
	insightCompare
	insightUsage
	insightFidelity
	insightAttempts
)

var insightNames = []string{"RATES", "LATENCY", "CAPACITY", "DISTRIBUTION", "COMPARE", "USAGE", "FIDELITY", "ATTEMPTS"}
var insightWindows = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 90 * 24 * time.Hour, 365 * 24 * time.Hour}

type insightsState struct {
	acceptedFactQuery observation.RequestQuery
	resources         []resourceRef
	binSelection      *binRange

	markerIndex int

	durationKind             string
	durationMin, durationMax *int64

	view, metric, series, bin, resource int
	all, follow, pending, fullHeatmap   bool
	span                                time.Duration
	from, to, cursor                    time.Time
	brushStart, brushEnd                time.Time
	brushing, dragging                  bool
	dragX                               int
	hidden                              [4]bool
	result                              observation.QueryResult
	snapshot                            observation.Snapshot
	generation                          uint64
	cancel                              context.CancelFunc
	err                                 string
	input                               string
	inputMode                           bool
	facts                               observation.RequestResult
	factGeneration                      uint64
	factCancel                          context.CancelFunc
	factsPending                        bool
	factOffset                          int
	cohort                              bool
}

func newInsightsState() insightsState {
	now := time.Now()
	return insightsState{all: true, follow: true, span: 15 * time.Minute, from: now.Add(-15 * time.Minute), to: now, cursor: now, bin: 0}
}
func (s insightsState) axis() (time.Time, time.Time) {
	if !s.result.Query.From.IsZero() {
		return s.result.Query.From, s.result.Query.To
	}
	return s.from, s.to
}
func (s insightsState) selectionRange() (time.Time, time.Time) {
	if !s.brushStart.IsZero() && !s.brushEnd.IsZero() {
		if s.brushStart.Before(s.brushEnd) {
			return s.brushStart, s.brushEnd
		}
		return s.brushEnd, s.brushStart
	}
	return s.axis()
}
func (m Model) insightScope() string {
	if m.insights.all {
		return "ALL PROVIDERS"
	}
	return m.providerLabel(m.active)
}
func (m *Model) insightZoom(factor float64) {
	s := &m.insights
	from, to := s.axis()
	span := time.Duration(float64(to.Sub(from)) * factor)
	span = max(10*time.Second, min(span, 366*24*time.Hour))
	center := s.cursor
	if center.Before(from) || center.After(to) {
		center = from.Add(to.Sub(from) / 2)
	}
	s.from = center.Add(-span / 2)
	s.to = s.from.Add(span)
	s.span = span
	s.follow = false
	s.brushStart = time.Time{}
	s.brushEnd = time.Time{}
}
func (m *Model) insightPan(direction int) {
	s := &m.insights
	from, to := s.axis()
	d := to.Sub(from) / 2 * time.Duration(direction)
	s.from = from.Add(d)
	s.to = to.Add(d)
	s.cursor = s.cursor.Add(d)
	s.follow = false
	s.span = to.Sub(from)
}

// resetInsightView clears the per-visualization selections. Switching views by
// any route (t/T, the heading click, or the command palette) must start from
// the same state; otherwise a bin, series or hidden flag from the old chart
// silently carries into the new one.
func (m *Model) resetInsightView() {
	s := &m.insights
	s.metric = 0
	s.bin = 0
	s.binSelection = nil
	s.series = 0
	s.hidden = [4]bool{}
}

func (m Model) handleInsightKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	s := &m.insights
	if s.inputMode {
		switch msg.String() {
		case "esc":
			s.inputMode = false
			s.input = ""
			return m, nil, true
		case "enter":
			fields := strings.Fields(s.input)
			if len(fields) != 2 {
				m.tell("Enter two RFC3339 times: start end")
				return m, nil, true
			}
			a, e1 := time.Parse(time.RFC3339, fields[0])
			b, e2 := time.Parse(time.RFC3339, fields[1])
			if e1 != nil || e2 != nil || !a.Before(b) || b.Sub(a) > 366*24*time.Hour {
				m.tell("Use valid start < end, at most 366 days")
				return m, nil, true
			}
			s.from, s.to, s.span, s.follow = a, b, b.Sub(a), false
			s.cursor = a
			s.inputMode = false
			s.brushStart = time.Time{}
			s.brushEnd = time.Time{}
			return m, m.requestInsights(), true
		case "ctrl+u":
			s.input = ""
		case "backspace", "ctrl+h":
			s.input = dropGrapheme(s.input)
		default:
			if msg.Key().Text != "" && len(s.input) < 160 {
				s.input += msg.Key().Text
			}
		}
		return m, nil, true
	}
	if m.tab != tabInsights {
		return m, nil, false
	}
	switch msg.String() {
	case "t":
		s.view = (s.view + 1) % len(insightNames)
		m.resetInsightView()
		return m, nil, true
	case "T":
		s.view = (s.view + len(insightNames) - 1) % len(insightNames)
		m.resetInsightView()
		return m, nil, true
	case "v":
		s.binSelection = nil
		s.metric = (s.metric + 1) % 3
		s.resource++
		return m, nil, true
	case "a":
		s.all = !s.all
		return m, m.requestInsights(), true
	case "w":
		at := 0
		for i, d := range insightWindows {
			if s.span >= d {
				at = i + 1
			}
		}
		s.span = insightWindows[at%len(insightWindows)]
		s.to = time.Now()
		s.from = s.to.Add(-s.span)
		s.cursor = s.to
		s.brushStart = time.Time{}
		s.brushEnd = time.Time{}
		return m, m.requestInsights(), true
	case "r":
		s.inputMode = true
		from, to := s.axis()
		s.input = from.UTC().Format(time.RFC3339) + " " + to.UTC().Format(time.RFC3339)
		return m, nil, true
	case "f":
		s.follow = !s.follow
		if s.follow {
			s.cursor = time.Now()
		}
		return m, m.requestInsights(), true
	case "+", "=":
		m.insightZoom(.5)
		return m, m.requestInsights(), true
	case "-", "_":
		m.insightZoom(2)
		return m, m.requestInsights(), true
	case "H", "pgup":
		m.insightPan(-1)
		return m, m.requestInsights(), true
	case "L", "pgdown":
		m.insightPan(1)
		return m, m.requestInsights(), true
	case "h", "left", "l", "right":
		from, to := s.axis()
		step := to.Sub(from) / time.Duration(max(m.insightGeometry().w, 1))
		if msg.String() == "h" || msg.String() == "left" {
			step = -step
		}
		s.cursor = s.cursor.Add(step)
		if s.cursor.Before(from) {
			s.cursor = from
		}
		if !s.cursor.Before(to) {
			s.cursor = to.Add(-time.Nanosecond)
		}
		s.follow = false
		if s.brushing {
			s.brushEnd = s.cursor
		}
		return m, nil, true
	case "j", "down":
		if s.view == insightLatency || s.view == insightDistribution {
			s.binSelection = nil
			last := 22
			if s.view == insightLatency {
				last = max(len(m.heatmapBins())-1, 0)
			}
			s.bin = min(s.bin+1, last)
		} else {
			if s.view == insightCompare {
				s.series = min(s.series+1, max(len(s.result.ByProvider)-1, 0))
			} else {
				s.series = (s.series + 1) % m.seriesCount()
			}
		}
		return m, nil, true
	case "k", "up":
		if s.view == insightLatency || s.view == insightDistribution {
			s.binSelection = nil
			s.bin = max(s.bin-1, 0)
		} else {
			if s.view == insightCompare {
				s.series = max(s.series-1, 0)
			} else {
				s.series = (s.series + m.seriesCount() - 1) % m.seriesCount()
			}
		}
		return m, nil, true
	case " ", "space":
		if s.view != insightCompare && s.series >= 0 && s.series < len(s.hidden) {
			s.hidden[s.series] = !s.hidden[s.series]
		}
		return m, nil, true
	case "b":
		s.brushStart = s.cursor
		s.brushEnd = s.cursor
		s.brushing = true
		s.follow = false
		return m, nil, true
	case "z":
		m.rememberHeatmapSelection()
		s.fullHeatmap = !s.fullHeatmap
		m.alignHeatmapSelection()
		return m, nil, true
	case "esc":
		s.brushStart = time.Time{}
		s.brushEnd = time.Time{}
		s.brushing = false
		return m, nil, true
	case "enter":
		if s.brushing {
			s.brushing = false
			return m, nil, true
		}
		if s.view == insightDistribution {
			m.openDistributionInspection()
			return m, nil, true
		}
		if s.view == insightCompare && len(s.result.ByProvider) > 0 {
			id := s.result.ByProvider[min(s.series, len(s.result.ByProvider)-1)].Provider
			for i := range m.providers {
				if m.providerID(i) == id {
					m.active = i
					m.syncActive()
					s.all = false
					s.view = insightRates
					return m, m.requestInsights(), true
				}
			}
		}
		m.openPointInspection()
		return m, nil, true
	case "i":
		m.setDurationFilter()
		s.cohort = true
		s.factOffset = 0
		m.switchTab(tabRequests)
		m.filterText = ""
		return m, m.requestFacts(), true
	case "m":
		if len(s.result.Markers) > 0 {
			s.markerIndex = (s.markerIndex + 1) % len(s.result.Markers)
			m.openMarker(s.result.Markers[s.markerIndex])
		}
		return m, nil, true
	case "e":
		return m, m.beginExport("measurements"), true
	}
	return m, nil, false
}

// chartGeometry is derived once from the same content budget used for drawing.
// Its x/time mapping is shared by clicks, drag brushing and keyboard movement.
type chartGeometry struct{ x, y, w, h int }

func (m Model) insightGeometry() chartGeometry {
	width := max(m.viewportWidth(), 1)
	if width >= 110 {
		width -= 36
	}
	return chartGeometry{x: 8, y: m.contentStartRow() + 5, w: max(width-9, 1), h: max(m.visibleRows()-9, 1)}
}
func (m Model) chartTime(x int) time.Time {
	a, b := m.insights.axis()
	g := m.insightGeometry()
	p := max(0, min(x-g.x, g.w-1))
	return a.Add(time.Duration(float64(b.Sub(a)) * float64(p) / float64(max(g.w, 1))))
}
func (m Model) cursorColumn() int {
	a, b := m.insights.axis()
	g := m.insightGeometry()
	if !a.Before(b) {
		return 0
	}
	return max(0, min(int(float64(m.insights.cursor.Sub(a))/float64(b.Sub(a))*float64(g.w)), g.w-1))
}
func pointAt(points []observation.Point, t time.Time) (observation.Point, bool) {
	// Binary search on immutable, monotonically indexed query cells.
	lo, hi := 0, len(points)
	for lo < hi {
		mid := (lo + hi) / 2
		if !points[mid].End.After(t) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(points) && !t.Before(points[lo].Start) && t.Before(points[lo].End) {
		return points[lo], true
	}
	return observation.Point{}, false
}
func (m Model) cursorPoint() (observation.Point, bool) {
	return pointAt(m.insights.result.Points, m.insights.cursor)
}
func pointRate(p observation.Point, n uint64) (float64, bool) {
	if !p.Complete || p.ElapsedNS <= 0 || p.Gap || p.Overlap || p.ClockStep {
		return 0, false
	}
	return float64(n) / (float64(p.ElapsedNS) / float64(time.Second)), true
}
func (m Model) pointHistogram(p observation.Point) observation.Histogram {
	switch m.insights.metric % 3 {
	case 1:
		return p.Admission
	case 2:
		return p.AttemptDuration
	}
	return p.Duration
}
func (m Model) latencyName() string {
	return []string{"client", "admission", "attempt"}[m.insights.metric%3]
}
func (m Model) graphSeries() []string {
	switch m.insights.view {
	case insightCapacity:
		return []string{"occupied", "cooldown", "limit", "queued"}
	case insightCompare:
		return []string{"requests", "failed", "p95", "attempts"}
	case insightUsage:
		return m.usageSeries()
	case insightFidelity:
		return []string{"losses", "notes", "rejected", "conv.fail"}
	case insightAttempts:
		if m.insights.metric%3 == 1 {
			return []string{"401", "429", "5xx", "attempts"}
		}
		if m.insights.metric%3 == 2 {
			return []string{"up 1xx", "up 2xx", "up 4xx", "up 5xx"}
		}
		return []string{"attempts", "retries", "auth resend", "429"}
	}
	return []string{"arrived", "terminal", "failed", "attempts"}
}
func (m Model) seriesValue(p observation.Point, series int) (float64, bool) {
	if m.insights.view == insightCapacity {
		if !p.Complete {
			return 0, false
		}
		if !m.insights.all {
			r, exists := m.selectedResource(p)
			if !exists {
				return 0, false
			}
			if p.ElapsedNS <= 0 || r.Limit.DurationNS < p.ElapsedNS || r.Occupied.DurationNS < p.ElapsedNS {
				return 0, false
			}
			switch series {
			case 0:
				return r.Occupied.Mean()
			case 1:
				return r.Cooldown.Mean()
			case 2:
				return r.Limit.Mean()
			case 3:
				return r.Queued.Mean()
			}
		}
		switch series {
		case 0:
			return p.Gauges.Active.Mean()
		case 1:
			return p.Gauges.Exchanges.Mean()
		case 2:
			return 0, false
		case 3:
			return p.Gauges.Queued.Mean()
		}
	}
	if m.insights.view == insightUsage {
		return m.usageValue(p, series)
	}
	if m.insights.view == insightFidelity {
		counts := []uint64{p.Counts.ConversionLosses, p.Counts.ConversionNotes, p.Counts.Rejected, p.Counts.Failures[5] + p.Counts.Failures[6]}
		return pointRate(p, counts[series])
	}
	if m.insights.view == insightAttempts {
		counts := []uint64{p.Counts.Attempts, p.Counts.Retries, p.Counts.AuthResends, p.Counts.Upstream429}
		if m.insights.metric%3 == 1 {
			counts = []uint64{p.Counts.Upstream401, p.Counts.Upstream429, p.Counts.Upstream5xx, p.Counts.Attempts}
		}
		if m.insights.metric%3 == 2 {
			counts = []uint64{p.Counts.HTTP[1][0], p.Counts.HTTP[1][1], p.Counts.HTTP[1][3], p.Counts.HTTP[1][4]}
		}
		return pointRate(p, counts[series])
	}
	switch series {
	case 0:
		return pointRate(p, p.Counts.Received)
	case 1:
		return pointRate(p, p.Counts.Terminal)
	case 2:
		return pointRate(p, p.Counts.Failed)
	case 3:
		return pointRate(p, p.Counts.Attempts)
	}
	return 0, false
}
func (m Model) seriesStyles() []lipgloss.Style {
	if m.insights.view == insightFidelity {
		return []lipgloss.Style{m.styles.queueWarnStyle, m.styles.sparklineStyle, m.styles.statusClientErrStyle, m.styles.statusServerErrStyle}
	}
	return []lipgloss.Style{m.styles.sparklineStyle, m.styles.statusOkStyle, m.styles.statusServerErrStyle, m.styles.queueWarnStyle}
}
func (m Model) legendNames() []string {
	names := m.graphSeries()
	if m.insights.view == insightCapacity && m.insights.all {
		names = []string{"active", "exchanges", "cap ?", "queued"}
	}
	if m.width < 60 {
		short := map[string]string{"arrived": "in", "terminal": "done", "failed": "fail", "attempts": "try", "occupied": "used", "cooldown": "hold", "exchanges": "wire", "queued": "wait", "input known": "in?", "output known": "out?", "total known": "all?", "auth resend": "auth", "corrections": "fix", "overflow": "over", "rejected": "rej", "conv.fail": "conv"}
		for i, n := range names {
			if v, ok := short[n]; ok {
				names[i] = v
			} else {
				names[i] = truncatePlain(n, 5)
			}
		}
	}
	return names
}
func (m Model) renderLegend() string {
	names := m.legendNames()
	var b strings.Builder
	for i, n := range names {
		if n == "" {
			continue
		}
		glyph := "●"
		if m.insights.hidden[i] {
			glyph = "○"
		}
		if i == m.insights.series {
			glyph = "▸"
		}
		label := glyph + n
		b.WriteString(m.seriesStyles()[i].Render(label + " "))
	}
	return b.String()
}
func (m Model) renderInsights() string {
	h := m.visibleRows()
	w := m.viewportWidth()
	if h <= 0 {
		return ""
	}
	if m.runtime.Observations == nil {
		return "Observations are unavailable.\n"
	}
	s := m.insights
	from, to := s.axis()
	mode := "HISTORY"
	if s.follow {
		mode = "LIVE"
	}
	if s.pending {
		mode += " · querying"
	}
	source := "memory · ≤15m"
	if s.snapshot.History.Enabled {
		source = "SQLite"
		if !s.snapshot.History.Healthy {
			source += " · writer degraded"
		}
	}
	heading := m.styles.sectionStyle.Render(fmt.Sprintf(" %s  %d/%d · %s", insightNames[s.view], s.view+1, len(insightNames), m.insightUnits()))
	rangeFormat := "15:04:05"
	if to.Sub(from) >= 24*time.Hour {
		rangeFormat = "2006-01-02"
	}
	rows := []string{heading, m.styles.rowStyle.Render(" " + m.insightScope() + " · " + mode), m.styles.dimStyle2.Render(" " + from.UTC().Format(rangeFormat) + " → " + to.UTC().Format(rangeFormat) + " UTC · " + source)}
	if s.err != "" {
		rows[2] = m.styles.statusServerErrStyle.Render(" " + safeTerminal(s.err))
	}
	total := mergePoints(s.result.Points)
	_, p95, ok := m.pointHistogram(total).Quantile(.95)
	p95s := "—"
	if ok {
		p95s = secondsLabel(p95)
	}
	summary := fmt.Sprintf(" %d finished · %d failed · p95 ≤%s", total.Counts.Terminal, total.Counts.Failed, p95s)
	switch s.view {
	case insightUsage:
		summary = fmt.Sprintf(" %d reports · %d corrections · v:metric", total.Counts.UsageReports, total.Counts.UsageCorrections)
	case insightFidelity:
		summary = fmt.Sprintf(" %d losses · %d notes · %d conv.fail", total.Counts.ConversionLosses, total.Counts.ConversionNotes, total.Counts.Failures[5]+total.Counts.Failures[6])
	case insightAttempts:
		summary = fmt.Sprintf(" %d attempts · %d retries · v:metric", total.Counts.Attempts, total.Counts.Retries)
	case insightCapacity:
		if s.all {
			summary = " Fleet activity · capacity is per pool"
		} else if p, ok := m.cursorPoint(); ok && len(p.Resources) > 0 {
			if r, ok := m.selectedResource(p); ok {
				summary = " Pool " + r.Provider + "/" + r.ID + " · v:resource"
			}
		}
	}
	rows = append(rows, summary)
	if s.view == insightLatency {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" "+m.latencyName()+" duration · density/count"))
	} else if s.view == insightDistribution {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" "+m.latencyName()+" · merged histogram"))
	} else if s.view == insightCompare {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" provider · historical request counts"))
	} else {
		rows = append(rows, " "+m.renderLegend())
	}
	var chart []string
	switch s.view {
	case insightLatency:
		chart = m.renderHeatmap()
	case insightDistribution:
		chart = m.renderDistribution()
	case insightCompare:
		chart = m.renderComparison()
	default:
		chart = m.renderLineChart()
	}
	if w >= 110 {
		detail := m.insightDetails()
		left := m.insightGeometry().w + 9
		for i := range chart {
			line := ""
			if i < len(detail) {
				line = detail[i]
			}
			chart[i] = fitStyled(chart[i], left) + m.styles.sepStyle.Render("│ ") + line
		}
	}
	rows = append(rows, chart...)
	g := m.insightGeometry()
	axisFormat := "15:04"
	if to.Sub(from) >= 24*time.Hour {
		axisFormat = "01/02"
	}
	rows = append(rows, "        "+fitPlain(from.UTC().Format(axisFormat), max(g.w-5, 0))+to.UTC().Format(axisFormat))
	rows = append(rows, m.renderMarkerAxis())
	p, present := m.cursorPoint()
	state := "no observations"
	if present {
		state = coverageLabel(p)
	}
	if s.result.Truncated {
		state = "SOURCE LIMIT: narrow range"
	}
	rows = append(rows, m.styles.sectionStyle.Render(fmt.Sprintf(" ▸ %s · %s", s.cursor.UTC().Format("15:04:05"), state)))
	if !s.brushStart.IsZero() {
		a, b := s.selectionRange()
		rows = append(rows, m.styles.queueWarnStyle.Render(" Brush "+a.UTC().Format("15:04:05")+"–"+b.UTC().Format("15:04:05")+" · i requests"))
	} else if present {
		if s.view == insightLatency {
			r := m.selectedHeatmapRange()
			rows = append(rows, fmt.Sprintf(" %d in (%s, %s]", countBins(m.pointHistogram(p), r), secondsLabel(binLower(r.lo)), secondsLabel(binUpper(r.hi))))
		} else if s.view == insightUsage {
			rows = append(rows, fmt.Sprintf(" %d reports · %d/%d/%d known", p.Counts.UsageReports, p.Counts.UsageInputKnown, p.Counts.UsageOutputKnown, p.Counts.UsageTotalKnown))
		} else if s.view == insightFidelity {
			rows = append(rows, fmt.Sprintf(" %d loss · %d note · Enter detail", p.Counts.ConversionLosses, p.Counts.ConversionNotes))
		} else {
			rows = append(rows, fmt.Sprintf(" %d terminal  %d failures  %d attempts", p.Counts.Terminal, p.Counts.Failed, p.Counts.Attempts))
		}
	} else {
		rows = append(rows, " Missing observations are never zero.")
	}
	if s.result.Truncated {
		rows = append(rows, m.styles.statusClientErrStyle.Render(" SOURCE LIMIT · narrow the time window"))
	}
	if len(s.result.Points) == 0 && !s.pending {
		rows = append(rows, " No observations in this time window.")
	}
	return fitRows(rows, w, h)
}
func fitRows(rows []string, w, h int) string {
	var b strings.Builder
	for i := range h {
		if i < len(rows) {
			b.WriteString(fitStyled(rows[i], w))
		} else {
			b.WriteString(strings.Repeat(" ", max(w, 0)))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func coverageLabel(p observation.Point) string {
	switch {
	case p.ClockStep:
		return "clock step; rate unknown"
	case p.Overlap:
		return "overlap; rate unknown"
	case p.Gap:
		return "gap; rate unknown"
	case p.Partial || !p.Complete:
		return "partial coverage"
	default:
		return "complete"
	}
}
func secondsLabel(n float64) string {
	if math.IsInf(n, 1) {
		return "+Inf"
	}
	if n < 1 {
		return fmt.Sprintf("%gms", n*1000)
	}
	if n < 60 {
		return fmt.Sprintf("%gs", n)
	}
	return (time.Duration(n * float64(time.Second))).String()
}
func compactNumber(n float64) string {
	switch {
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", n/1e3)
	case n >= 100:
		return fmt.Sprintf("%.0f", n)
	case n >= 10:
		return fmt.Sprintf("%.1f", n)
	default:
		return fmt.Sprintf("%.2f", n)
	}
}
