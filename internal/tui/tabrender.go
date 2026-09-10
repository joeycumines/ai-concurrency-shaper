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

package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

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
		// Per-route breakdown (UNRESP-3): which routes are waiting and how
		// long the oldest waiter on each has waited — the operator view of
		// "which route is starved" that the aggregate count hides.
		routes := make([]string, 0, len(m.snap.QueuedByRoute))
		for route := range m.snap.QueuedByRoute {
			routes = append(routes, route)
		}
		sort.Strings(routes)
		for _, route := range routes {
			age := "—"
			if oldest, ok := m.snap.OldestQueuedAgeByRoute[route]; ok && oldest > 0 {
				age = oldest.Truncate(time.Millisecond).String()
			}
			fmt.Fprintf(&b, "    %-40s %d waiting (oldest %s)\n", route, m.snap.QueuedByRoute[route], age)
		}
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
