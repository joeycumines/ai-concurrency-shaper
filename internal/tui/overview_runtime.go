package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func (m Model) runtimeOverviewLines() []string {
	if m.width < 72 {
		return m.compactOverviewLines()
	}
	rows := []string{m.styles.sectionStyle.Render(" FLEET HEALTH · Tab changes provider · Enter explores time"), "", m.styles.tableHeaderStyle.Render(" PROVIDER                 STATE      ACTIVE  QUEUED    REQ/S   TERMINAL  FAILED   GLOBAL CAP")}
	for i, p := range m.providers {
		state := "running"
		global := "—"
		for _, c := range m.controlPanel.snapshot.Providers {
			if c.Name == m.providerID(i) {
				state = string(c.State)
				for _, pool := range c.Pools {
					if pool.ID == "global" {
						global = fmt.Sprint(pool.Limit)
					}
				}
			}
		}
		var terminal, failed uint64
		for _, v := range m.insights.snapshot.Providers {
			if v.Provider == m.providerID(i) {
				terminal = v.Totals.Terminal
				failed = v.Totals.Failed
			}
		}
		line := fmt.Sprintf(" %-24s %-9s %7d %7d %8.2f %10d %7d %12s", truncatePlain(m.providerLabel(i), 24), state, p.snap.Active, p.snap.Queued, p.snap.Throughput, terminal, failed, global)
		style := m.styles.rowStyle
		if state != "running" {
			style = m.styles.queueWarnStyle
		}
		if i == m.active {
			style = m.styles.rowSelectedStyle
		}
		rows = append(rows, style.Render(line))
	}
	rows = append(rows, "", m.styles.sectionStyle.Render(" SELECTED PROVIDER · "+m.providerLabel(m.active)), fmt.Sprintf(" %d active · %d queued · %.2f req/s · oldest wait %s", m.snap.Active, m.snap.Queued, m.snap.Throughput, m.oldestQueuedAge().Truncate(time.Millisecond)))
	if cb := m.snap.CircuitBreaker; cb != nil {
		style := m.styles.statusOkStyle
		if cb.State != "CLOSED" {
			style = m.styles.queueWarnStyle
		}
		rows = append(rows, style.Render(fmt.Sprintf(" Breaker %s · failures %d · penalty %s", cb.State, cb.Failures, cb.CurrentPenalty)))
	}
	rows = append(rows, "", m.styles.sectionStyle.Render(" ADMISSION POOLS · Enter / 7 → Capacity for history"), m.styles.tableHeaderStyle.Render(" POOL                             OCCUPIED  /  LIMIT    COOLDOWN  WAITING   DEBT"))
	if p, ok := m.selectedControlProvider(); ok {
		for _, pool := range p.Pools {
			rows = append(rows, fmt.Sprintf(" %-32s %8d  /  %-8d %8d %8d %6d", truncatePlain(pool.ID, 32), pool.Occupied, pool.Limit, pool.Cooldown, pool.Waiters, pool.Debt))
			if m.width >= 100 {
				rows = append(rows, " "+m.poolStrip(pool, max(min(m.width-8, 100), 1)))
			}
		}
	}
	rows = append(rows, m.styles.dimStyle2.Render(" Cooldown is included in occupied. Default, route and global caps are independent."), "", m.styles.sectionStyle.Render(" THROUGHPUT · last 10 seconds · Enter opens interactive history"), m.renderSparkline(), "")
	var facts observation.ProviderSnapshot
	for _, p := range m.insights.snapshot.Providers {
		if p.Provider == m.providerID(m.active) {
			facts = p
			break
		}
	}
	rows = append(rows, m.styles.sectionStyle.Render(" REQUEST OUTCOMES · canonical totals"), fmt.Sprintf(" %d received · %d terminal · %d completed · %d failed · %d aborted · %d rejected", facts.Totals.Received, facts.Totals.Terminal, facts.Totals.Completed, facts.Totals.Failed, facts.Totals.Aborted, facts.Totals.Rejected), fmt.Sprintf(" %d upstream attempts · %d retries · %d credential resends", facts.Totals.Attempts, facts.Totals.Retries, facts.Totals.AuthResends), fmt.Sprintf(" %d conversion losses · %d notes · %d reported usage records", facts.Totals.ConversionLosses, facts.Totals.ConversionNotes, facts.Totals.UsageReports), "", m.styles.sectionStyle.Render(" HISTORY RECORDER"))
	h := m.insights.snapshot.History
	if h.Enabled {
		state := "healthy"
		if !h.Healthy {
			state = "degraded"
		}
		watermark := "not committed yet"
		if !h.CommittedThrough.IsZero() {
			watermark = h.CommittedThrough.UTC().Format(time.RFC3339)
		}
		rows = append(rows, fmt.Sprintf(" SQLite %s · pending %d/%d · committed through %s", state, h.Pending, h.Capacity, watermark), fmt.Sprintf(" %d dropped batches · %d dropped facts · %d dropped markers · %d write errors", h.DroppedBatches, h.DroppedFacts, h.DroppedMarkers, h.WriteErrors))
	} else {
		rows = append(rows, " Live observations retain up to 15 minutes / 12,000 intervals. -history-db adds persistent time ranges.")
	}
	rows = append(rows, fmt.Sprintf(" %d live IDs · %d excess untracked requests", m.insights.snapshot.LiveRequests, m.insights.snapshot.UntrackedRequests), fmt.Sprintf(" Writer buffer %d / %d bytes", h.BufferedBytes, h.ByteCapacity))
	rows = append(rows, "", m.styles.dimStyle2.Render(" 2 requests · 3 network evidence · 4 logs · 7 time exploration · 8 runtime controls"))
	return rows
}
func (m Model) poolStrip(p control.PoolState, width int) string {
	if p.Limit <= 0 {
		return "capacity unknown"
	}
	den := max(int64(p.Limit), p.Occupied)
	occupied := min(width, int(float64(p.Occupied)/float64(den)*float64(width)))
	cooldown := min(occupied, int(float64(p.Cooldown)/float64(den)*float64(width)))
	return m.styles.sparklineStyle.Render(strings.Repeat("█", occupied-cooldown)) + m.styles.queueWarnStyle.Render(strings.Repeat("█", cooldown)) + m.styles.gaugeEmptyStyle.Render(strings.Repeat("░", width-occupied))
}
