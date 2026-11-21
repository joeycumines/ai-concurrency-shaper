package tui

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func (m Model) insightDetails() []string {
	s := m.insights
	p, ok := m.cursorPoint()
	r := []string{m.styles.sectionStyle.Render("SELECTED INTERVAL"), s.cursor.UTC().Format(time.RFC3339)}
	if !ok {
		return append(r, "No observations at this time.", "Gaps are never drawn as zero.")
	}
	r = append(r, coverageLabel(p), fmt.Sprintf("coverage %.1f%%", p.Coverage*100), "")
	switch s.view {
	case insightUsage:
		r = append(r, m.styles.sectionStyle.Render("SOURCE-REPORTED USAGE"), fmt.Sprintf("%d terminal usage reports", p.Counts.UsageReports), "")
		known := []uint64{p.Counts.UsageInputKnown, p.Counts.UsageOutputKnown, p.Counts.UsageTotalKnown}
		tokens := []uint64{p.Counts.UsageInputTokens, p.Counts.UsageOutputTokens, p.Counts.UsageTotalTokens}
		for i, name := range []string{"Input", "Output", "Total"} {
			v := "unknown"
			if known[i] > 0 {
				v = fmt.Sprintf("%d tokens", tokens[i])
			}
			r = append(r, name+": "+v, fmt.Sprintf("  %d / %d reports known", known[i], p.Counts.UsageReports))
		}
		r = append(r, "", fmt.Sprintf("%d reported corrections", p.Counts.UsageCorrections), fmt.Sprintf("%d overflow omissions", p.Counts.UsageOverflow), "Known zero is a reported zero.", "Absent usage stays unknown.", "Input + output may differ from total.", "v cycles tokens, coverage, reports.")
	case insightFidelity:
		r = append(r, m.styles.sectionStyle.Render("CONVERSION DECISIONS"), fmt.Sprintf("%d approved losses", p.Counts.ConversionLosses), fmt.Sprintf("%d semantic notes", p.Counts.ConversionNotes), fmt.Sprintf("%d dropped decisions", p.Counts.ConversionDropped), "", fmt.Sprintf("%d request conversion failures", p.Counts.Failures[5]), fmt.Sprintf("%d response conversion failures", p.Counts.Failures[6]), fmt.Sprintf("%d rejected logical requests", p.Counts.Rejected), "", "Losses are explicit policy events.", "A note is not a loss or failure.", "i opens requests for exact evidence.")
	case insightAttempts:
		r = append(r, m.styles.sectionStyle.Render("PHYSICAL UPSTREAM EXCHANGES"), fmt.Sprintf("%d attempts", p.Counts.Attempts), fmt.Sprintf("%d retries", p.Counts.Retries), fmt.Sprintf("%d auth resends", p.Counts.AuthResends), "", fmt.Sprintf("%d upstream 401", p.Counts.Upstream401), fmt.Sprintf("%d upstream 429", p.Counts.Upstream429), fmt.Sprintf("%d upstream 5xx", p.Counts.Upstream5xx), "", "Attempts include retries/resends.", "Response status is recorded on headers.", "Header delay is not first-token time.", "v cycles counts and status classes.")
	case insightCapacity:
		r = append(r, m.styles.sectionStyle.Render("ADMISSION RESOURCES"))
		if s.all {
			r = append(r, "Fleet active/wire/queue are sums.", "No combined fleet capacity exists.", "a selects a provider; v picks a pool.")
		} else if len(p.Resources) > 0 {
			if pool, ok := m.selectedResource(p); ok {
				r = append(r, pool.Provider+" / "+pool.ID, "Kind: "+pool.Kind)
				for i, n := range []string{"Occupied", "Cooldown", "Limit", "Queued"} {
					v, known := m.seriesValue(p, i)
					label := "unknown coverage"
					if known {
						label = compactNumber(v)
					}
					r = append(r, n+": "+label)
				}
				r = append(r, "", "Cooldown is within occupied.", "Overlapping pools are not summed.", "Resize debt preserves existing leases.")
			}
		}
	default:
		r = append(r, fmt.Sprintf("%d arrived / %d terminal", p.Counts.Received, p.Counts.Terminal), fmt.Sprintf("%d completed / %d failed", p.Counts.Completed, p.Counts.Failed), fmt.Sprintf("%d aborted / %d rejected", p.Counts.Aborted, p.Counts.Rejected), fmt.Sprintf("%d attempts · %d retries", p.Counts.Attempts, p.Counts.Retries), fmt.Sprintf("%d auth resends", p.Counts.AuthResends), "", m.styles.sectionStyle.Render("LATENCY BIN BOUNDS"))
		for _, q := range []float64{.5, .95, .99} {
			a, b, known := m.pointHistogram(p).Quantile(q)
			v := "—"
			if known {
				v = secondsLabel(a) + " … " + secondsLabel(b)
			}
			r = append(r, fmt.Sprintf("p%02.0f %s", q*100, v))
		}
		r = append(r, "", fmt.Sprintf("%d losses · %d notes", p.Counts.ConversionLosses, p.Counts.ConversionNotes), fmt.Sprintf("%d usage reports", p.Counts.UsageReports))
	}
	r = append(r, "", "Enter  inspect exact cell", "i      open request cohort", "b      brush a time interval", "e      export selected range")
	return r
}

// mergePoints merges observations before estimating quantiles. Never average
// percentiles or invent fractional requests at selection edges.
func mergePoints(points []observation.Point) observation.Point {
	var out observation.Point
	out.Complete = true
	for _, p := range points {
		out.Counts.Add(p.Counts)
		mergeHistogram(&out.Duration, p.Duration)
		mergeHistogram(&out.Admission, p.Admission)
		mergeHistogram(&out.AttemptDuration, p.AttemptDuration)
		out.ElapsedNS += p.ElapsedNS
		out.Complete = out.Complete && p.Complete
	}
	return out
}
func mergeHistogram(a *observation.Histogram, b observation.Histogram) { a.Add(b) }

func (m *Model) setDurationFilter() {
	s := &m.insights
	s.durationKind = ""
	s.durationMin = nil
	s.durationMax = nil
	var r binRange
	if s.view == insightLatency {
		bins := m.heatmapBins()
		if len(bins) == 0 {
			return
		}
		r = m.selectedHeatmapRange()
	} else if s.view == insightDistribution {
		r = binRange{s.bin, s.bin}
	} else {
		return
	}
	s.durationKind = m.latencyName()
	lower := int64(binLower(r.lo) * float64(time.Second))
	if r.lo > 0 {
		s.durationMin = &lower
	}
	upper := binUpper(r.hi)
	if !math.IsInf(upper, 1) {
		u := int64(upper * float64(time.Second))
		s.durationMax = &u
	}
}

func (m Model) renderMarkerAxis() string {
	g := m.insightGeometry()
	a, b := m.insights.axis()
	cells := make([]rune, g.w)
	for i := range cells {
		cells[i] = ' '
	}
	if b.After(a) {
		for _, marker := range m.insights.result.Markers {
			if marker.At.Before(a) || !marker.At.Before(b) {
				continue
			}
			x := int(float64(marker.At.Sub(a)) / float64(b.Sub(a)) * float64(g.w))
			if x >= 0 && x < g.w {
				cells[x] = '▲'
			}
		}
	}
	return m.styles.dimStyle2.Render(" events ") + m.styles.queueWarnStyle.Render(string(cells))
}
func (m *Model) openMarker(marker observation.Marker) {
	m.insights.cursor = marker.At
	m.insights.follow = false
	m.inspection = inspectionState{open: true, title: "TIMELINE EVENT", rawLines: []string{marker.At.UTC().Format(time.RFC3339Nano), "Provider: " + marker.Provider, "Kind: " + marker.Kind, marker.Summary, "Request ID: " + marker.RequestID, "Event ID: " + marker.ID}}
	m.reflowInspection()
}

func (m Model) seriesCount() int {
	n := 0
	for _, s := range m.graphSeries() {
		if s != "" {
			n++
		}
	}
	return max(n, 1)
}
func (m Model) insightUnits() string {
	switch m.insights.view {
	case insightRates, insightAttempts:
		return "events/s"
	case insightLatency:
		return "seconds"
	case insightDistribution:
		// The histogram counts finished requests, but the x axis follows the
		// selected duration metric, which may be admission or attempt time.
		return m.latencyName() + " seconds"
	case insightCapacity:
		return "mean slots"
	case insightCompare:
		return "requests"
	case insightUsage:
		if m.insights.metric%3 == 0 {
			return "reported tokens/s"
		}
		if m.insights.metric%3 == 1 {
			return "known coverage %"
		}
		return "reports/s"
	case insightFidelity:
		return "decisions/s"
	}
	return ""
}
func (m Model) usageSeries() []string {
	switch m.insights.metric % 3 {
	case 1:
		return []string{"input known", "output known", "total known", ""}
	case 2:
		return []string{"reports", "corrections", "overflow", ""}
	default:
		return []string{"input", "output", "total", ""}
	}
}
func (m Model) usageValue(p observation.Point, series int) (float64, bool) {
	if series >= 3 {
		return 0, false
	}
	if !p.Complete {
		return 0, false
	}
	if m.insights.metric%3 == 1 {
		if p.Counts.UsageReports == 0 {
			return 0, false
		}
		known := []uint64{p.Counts.UsageInputKnown, p.Counts.UsageOutputKnown, p.Counts.UsageTotalKnown}
		return 100 * float64(known[series]) / float64(p.Counts.UsageReports), true
	}
	if m.insights.metric%3 == 2 {
		counts := []uint64{p.Counts.UsageReports, p.Counts.UsageCorrections, p.Counts.UsageOverflow}
		return pointRate(p, counts[series])
	}
	known := []uint64{p.Counts.UsageInputKnown, p.Counts.UsageOutputKnown, p.Counts.UsageTotalKnown}
	if known[series] == 0 {
		return 0, false
	}
	tokens := []uint64{p.Counts.UsageInputTokens, p.Counts.UsageOutputTokens, p.Counts.UsageTotalTokens}
	return pointRate(p, tokens[series])
}

// Resource identity, rather than per-point slice position, anchors a capacity
// series across route churn, missing intervals, and restarts.
type resourceRef struct{ provider, kind, id string }

func queryResources(result observation.QueryResult) []resourceRef {
	seen := make(map[resourceRef]bool)
	for _, p := range result.Points {
		for _, r := range p.Resources {
			seen[resourceRef{r.Provider, r.Kind, r.ID}] = true
		}
	}
	out := make([]resourceRef, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.id != b.id {
			return a.id < b.id
		}
		return a.kind < b.kind
	})
	return out
}
func (m Model) selectedResource(p observation.Point) (observation.ResourceIntegral, bool) {
	if len(m.insights.resources) == 0 {
		if len(p.Resources) == 0 {
			return observation.ResourceIntegral{}, false
		}
		return p.Resources[m.insights.resource%len(p.Resources)], true
	}
	target := m.insights.resources[m.insights.resource%len(m.insights.resources)]
	for _, r := range p.Resources {
		if target == (resourceRef{r.Provider, r.Kind, r.ID}) {
			return r, true
		}
	}
	return observation.ResourceIntegral{}, false
}
func (m *Model) acceptResources(result observation.QueryResult) {
	old := resourceRef{}
	if len(m.insights.resources) > 0 {
		old = m.insights.resources[m.insights.resource%len(m.insights.resources)]
	}
	m.insights.resources = queryResources(result)
	m.insights.resource = 0
	for i, r := range m.insights.resources {
		if r == old {
			m.insights.resource = i
			break
		}
	}
}
