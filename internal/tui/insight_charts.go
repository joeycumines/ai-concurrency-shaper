package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

type plotCell struct {
	bits   rune
	series int
}

var brailleBits = [4][2]rune{{1, 8}, {2, 16}, {4, 32}, {64, 128}}

func plotDot(cells []plotCell, w, h, x, y, series int) {
	if x < 0 || y < 0 || x >= w*2 || y >= h*4 {
		return
	}
	i := (y/4)*w + x/2
	cells[i].bits |= brailleBits[y%4][x%2]
	cells[i].series = series
}
func plotLine(cells []plotCell, w, h, x0, y0, x1, y1, series int) {
	dx := absInt(x1 - x0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -absInt(y1 - y0)
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		plotDot(cells, w, h, x0, y0, series)
		if x0 == x1 && y0 == y1 {
			break
		}
		e := 2 * err
		if e >= dy {
			err += dy
			x0 += sx
		}
		if e <= dx {
			err += dx
			y0 += sy
		}
	}
}
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (m Model) renderLineChart() []string {
	g := m.insightGeometry()
	a, b := m.insights.axis()
	cells := make([]plotCell, g.w*g.h)
	values := make([][4]float64, g.w*2)
	known := make([][4]bool, g.w*2)
	maximum := 0.0
	for x := range values {
		t := a.Add(time.Duration(float64(b.Sub(a)) * float64(x) / float64(len(values))))
		p, present := pointAt(m.insights.result.Points, t)
		if !present {
			continue
		}
		for series := range 4 {
			if m.insights.hidden[series] {
				continue
			}
			v, ok := m.seriesValue(p, series)
			if !ok || math.IsInf(v, 0) || math.IsNaN(v) {
				continue
			}
			values[x][series] = v
			known[x][series] = true
			maximum = math.Max(maximum, v)
		}
	}
	if maximum == 0 {
		maximum = 1
	} else {
		maximum *= 1.08
	}
	for series := range 4 {
		lastX, lastY := -1, -1
		for x := range values {
			if !known[x][series] {
				lastX, lastY = -1, -1
				continue
			}
			y := g.h*4 - 1 - int(values[x][series]/maximum*float64(g.h*4-1))
			if lastX >= 0 {
				plotLine(cells, g.w, g.h, lastX, lastY, x, y, series)
			} else {
				plotDot(cells, g.w, g.h, x, y, series)
			}
			lastX, lastY = x, y
		}
	}
	styles := m.seriesStyles()
	cursor := m.cursorColumn()
	rows := make([]string, g.h)
	for y := 0; y < g.h; y++ {
		label := ""
		if y == 0 {
			label = compactNumber(maximum)
		}
		if y == g.h-1 {
			label = "0"
		}
		if y == g.h/2 {
			label = compactNumber(maximum / 2)
		}
		var row strings.Builder
		row.WriteString(m.styles.dimStyle2.Render(fmt.Sprintf("%7s│", label)))
		for x := 0; x < g.w; x++ {
			c := cells[y*g.w+x]
			switch {
			case x == cursor:
				row.WriteString(m.styles.sectionStyle.Render("│"))
			case c.bits != 0:
				row.WriteString(styles[c.series].Render(string(rune(0x2800) + c.bits)))
			case m.brushColumn(x):
				row.WriteString(m.styles.filterPromptStyle.Render("·"))
			case y == g.h-1:
				row.WriteString(m.styles.dimStyle2.Render("·"))
			default:
				row.WriteByte(' ')
			}
		}
		rows[y] = row.String()
	}
	return rows
}
func (m Model) brushColumn(x int) bool {
	if m.insights.brushStart.IsZero() || m.insights.brushEnd.IsZero() {
		return false
	}
	a, b := m.insights.selectionRange()
	t := m.chartTime(x + m.insightGeometry().x)
	return !t.Before(a) && !t.After(b)
}

type binRange struct{ lo, hi int }

func (m Model) heatmapBins() []binRange {
	lo, hi := 0, 22
	if !m.insights.fullHeatmap {
		found := false
		for _, p := range m.insights.result.Points {
			h := m.pointHistogram(p)
			for i, n := range h.Bins {
				if n > 0 {
					if !found {
						lo, hi = i, i
						found = true
					} else {
						lo = min(lo, i)
						hi = max(hi, i)
					}
				}
			}
		}
		if found {
			lo = max(lo-1, 0)
			hi = min(hi+1, 22)
		}
	}
	rows := min(m.insightGeometry().h, hi-lo+1)
	var bins []binRange
	for r := range rows {
		start := lo + (hi-lo+1)*r/rows
		end := lo + (hi-lo+1)*(r+1)/rows - 1
		bins = append(bins, binRange{start, end})
	}
	// Slow bins at top, fast bins at bottom, as on latency heatmaps.
	for i, j := 0, len(bins)-1; i < j; i, j = i+1, j-1 {
		bins[i], bins[j] = bins[j], bins[i]
	}
	if len(bins) > 0 && len(bins) < m.insightGeometry().h {
		expanded := make([]binRange, m.insightGeometry().h)
		for i := range expanded {
			expanded[i] = bins[i*len(bins)/len(expanded)]
		}
		bins = expanded
	}
	return bins
}
func binUpper(i int) float64 {
	if i >= len(observation.DurationBounds) {
		return math.Inf(1)
	}
	return observation.DurationBounds[max(i, 0)]
}
func binLower(i int) float64 {
	if i <= 0 {
		return 0
	}
	return observation.DurationBounds[min(i-1, len(observation.DurationBounds)-1)]
}
func countBins(h observation.Histogram, r binRange) uint64 {
	var n uint64
	for i := r.lo; i <= r.hi; i++ {
		n += h.Bins[i]
	}
	return n
}
func (m Model) renderHeatmap() []string {
	g := m.insightGeometry()
	bins := m.heatmapBins()
	maxCount := uint64(1)
	for _, p := range m.insights.result.Points {
		hist := m.pointHistogram(p)
		for _, r := range bins {
			maxCount = max(maxCount, countBins(hist, r))
		}
	}
	cursor := m.cursorColumn()
	var out []string
	for row, r := range bins {
		var line strings.Builder
		label := secondsLabel(binUpper(r.hi))
		if row > 0 && bins[row-1] == r {
			label = ""
		}
		selected := row == min(m.insights.bin, len(bins)-1)
		if m.insights.binSelection != nil {
			target := *m.insights.binSelection
			selected = r.lo <= target.hi && r.hi >= target.lo
		}
		if selected && (row == 0 || bins[row-1] != r) {
			label = "▸" + label
		}
		line.WriteString(m.styles.dimStyle2.Render(fmt.Sprintf("%7s│", label)))
		for x := 0; x < g.w; x++ {
			p, ok := pointAt(m.insights.result.Points, m.chartTime(x+g.x))
			if !ok || p.Samples == 0 || !p.Complete {
				if x == cursor {
					line.WriteString(m.styles.sectionStyle.Render("│"))
				} else {
					line.WriteString(m.styles.dimStyle2.Render("┊"))
				}
				continue
			}
			n := countBins(m.pointHistogram(p), r)
			if n == 0 {
				line.WriteString(m.styles.dimStyle2.Render("·"))
				continue
			}
			level := min(4, max(1, int(math.Ceil(math.Log1p(float64(n))/math.Log1p(float64(maxCount))*4))))
			glyph := []string{"·", "░", "▒", "▓", "█"}[level]
			style := m.styles.sparklineStyle
			if level >= 3 {
				style = m.styles.gaugeWarnStyle
			}
			if level >= 4 {
				style = m.styles.queueWarnStyle
			}
			if x == cursor {
				style = style.Underline(true)
			}
			line.WriteString(style.Render(glyph))
		}
		out = append(out, line.String())
	}
	for len(out) < g.h {
		out = append(out, strings.Repeat(" ", g.w+8))
	}
	return out
}
func (m Model) renderDistribution() []string {
	g := m.insightGeometry()
	a, b := m.insights.selectionRange()
	var hist observation.Histogram
	for _, p := range m.insights.result.Points {
		if !p.Start.Before(a) && !p.End.After(b) {
			mergeHistogram(&hist, m.pointHistogram(p))
		}
	}
	maxCount := uint64(1)
	for _, n := range hist.Bins {
		maxCount = max(maxCount, n)
	}
	start := max(0, min(m.insights.bin-g.h/2, 23-g.h))
	end := min(start+g.h, 23)
	var rows []string
	for i := start; i < end; i++ {
		marker := " "
		if i == m.insights.bin {
			marker = "▸"
		}
		label := secondsLabel(binUpper(i))
		n := hist.Bins[i]
		barWidth := max(g.w-9, 1)
		length := int(float64(n) / float64(maxCount) * float64(barWidth))
		if n > 0 && length == 0 {
			length = 1
		}
		style := m.styles.sparklineStyle
		if i == m.insights.bin {
			style = m.styles.queueWarnStyle
		}
		rows = append(rows, m.styles.dimStyle2.Render(fmt.Sprintf("%s%6s│", marker, label))+style.Render(strings.Repeat("█", length))+strings.Repeat(" ", barWidth-length)+fmt.Sprintf(" %7d", n))
	}
	for len(rows) < g.h {
		rows = append(rows, "")
	}
	return rows
}
func (m Model) renderComparison() []string {
	g := m.insightGeometry()
	series := m.insights.result.ByProvider
	maximum := uint64(1)
	for _, s := range series {
		p := m.comparisonSummary(s.Points)
		maximum = max(maximum, p.Counts.Terminal)
	}
	var rows []string
	if len(series) == 0 {
		rows = append(rows, " No provider observations in range.")
	}
	start := m.comparisonStart()
	for i := start; i < len(series) && len(rows) < g.h; i++ {
		s := series[i]
		p := m.comparisonSummary(s.Points)
		barWidth := max(g.w-15, 1)
		bar := int(float64(p.Counts.Terminal) / float64(maximum) * float64(barWidth))
		if p.Counts.Terminal > 0 && bar == 0 {
			bar = 1
		}
		style := m.styles.sparklineStyle
		if p.Counts.Failed > 0 {
			style = m.styles.queueWarnStyle
		}
		label := truncatePlain(s.Provider, 13)
		prefix := " "
		if i == m.insights.series {
			prefix = "▸"
		}
		rows = append(rows, fmt.Sprintf("%s%-13s ", prefix, label)+style.Render(strings.Repeat("█", bar))+strings.Repeat(" ", barWidth-bar)+fmt.Sprintf(" %6d", p.Counts.Terminal))
		if len(series)*2 <= g.h {
			_, p95, ok := p.Duration.Quantile(.95)
			latency := "—"
			if ok {
				latency = secondsLabel(p95)
			}
			rows = append(rows, m.styles.dimStyle2.Render(fmt.Sprintf("   %d failed · %d attempts · p95 ≤%s", p.Counts.Failed, p.Counts.Attempts, latency)))
		}
	}
	for len(rows) < g.h {
		rows = append(rows, "")
	}
	return rows
}

func (m Model) selectedHeatmapRange() binRange {
	if m.insights.binSelection != nil {
		return *m.insights.binSelection
	}
	bins := m.heatmapBins()
	if len(bins) == 0 {
		return binRange{0, 22}
	}
	return bins[min(max(m.insights.bin, 0), len(bins)-1)]
}
func (m *Model) rememberHeatmapSelection() {
	if m.insights.view != insightLatency || m.insights.binSelection != nil {
		return
	}
	r := m.selectedHeatmapRange()
	m.insights.binSelection = &r
}
func (m *Model) alignHeatmapSelection() {
	if m.insights.view != insightLatency || m.insights.binSelection == nil {
		return
	}
	target := *m.insights.binSelection
	for i, r := range m.heatmapBins() {
		if r.lo <= target.hi && r.hi >= target.lo {
			m.insights.bin = i
			return
		}
	}
}

func (m Model) comparisonStart() int {
	rows := max(m.insightGeometry().h/m.comparisonRowHeight(), 1)
	return max(0, min(m.insights.series-rows/2, len(m.insights.result.ByProvider)-rows))
}
func (m Model) comparisonRowHeight() int {
	if len(m.insights.result.ByProvider)*2 <= m.insightGeometry().h {
		return 2
	}
	return 1
}
func (m Model) comparisonSummary(points []observation.Point) observation.Point {
	from, to := m.insights.selectionRange()
	var summary observation.Point
	summary.Complete = true
	for _, p := range points {
		if !p.Start.Before(from) && !p.End.After(to) {
			summary.Counts.Add(p.Counts)
			summary.Duration.Add(p.Duration)
			summary.Admission.Add(p.Admission)
			summary.AttemptDuration.Add(p.AttemptDuration)
			summary.ElapsedNS += p.ElapsedNS
			summary.Complete = summary.Complete && p.Complete
		}
	}
	return summary
}
