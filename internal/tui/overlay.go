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
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/rivo/uniseg"
)

func (m Model) resetCmd() tea.Cmd {
	return func() tea.Msg {
		return resetMsg{}
	}
}

type resetMsg struct{}

func (m Model) renderConfirmOverlay() string {
	return m.styles.overlayStyle.Render(
		fmt.Sprintf(" Reset Stats \n\n"+
			" Clear all cumulative counters?\n"+
			" (Proxied, Passthrough, Timeouts, etc.)\n\n"+
			" y = yes    n/Esc = no")) + "\n"
}

// renderMetaDrawer renders the engine-level meta drawer toggled by clicking
// the brand block. It shows CLI version, Go runtime, proxy uptime, and a
// placeholder for config reload status.
func runtimeVersion() string {
	return runtime.Version()
}

func (m Model) renderMetaDrawer() string {
	uptime := time.Since(m.startTime).Truncate(time.Second)
	return m.styles.overlayStyle.Render(
		fmt.Sprintf(" Engine Meta \n\n"+
			" CLI Version:  ai-concurrency-shaper\n"+
			" Go Runtime:   %s\n"+
			" Uptime:       %s\n"+
			" Providers:    %d\n"+
			" Active:       %d\n\n"+
			" [Esc/Enter] close ",
			runtimeVersion(), uptime, len(m.providers), m.snap.Active)) + "\n"
}

func (m Model) renderDetailOverlay() string {
	var b strings.Builder

	switch m.tab {
	case tabRequests:
		entries := m.visibleEntries()
		if m.cursor >= len(entries) {
			return ""
		}
		e := entries[m.cursor]
		b.WriteString(m.styles.overlayStyle.Render(
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
		if e := m.networkDetailAnchor; e != nil {
			// The anchor is a stable pointer to an immutable Entry; the
			// journal never rewrites entries in place. It may have been
			// evicted from the ring, but the overlay can still render it
			// because the Entry value is fully self-contained.
			b.WriteString(m.renderNetworkDetail(e))
		}

	case tabConcurrency:
		if m.cursor >= len(m.snap.InFlight) {
			return ""
		}
		r := m.snap.InFlight[m.cursor]
		b.WriteString(m.styles.overlayStyle.Render(
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

	case tabLogs:
		// Resolve the anchored item against the current ring snapshot. New
		// arrivals shift positions, but the sequence number keeps the view
		// pinned to the originally selected line; eviction or a filter change
		// that drops the line closes it.
		if pos, text, ok := m.resolveAnchor(m.logDetailAnchor); ok {
			b.WriteString(m.renderLogDetail(text, pos+1))
		} else {
			return ""
		}
	}

	if !builderEndsWithNewline(&b) {
		b.WriteByte('\n')
	}
	return b.String()
}

// networkDetailStillPresent reports whether the currently anchored Network
// detail entry still exists in the journal (matched by pointer identity —
// the journal never rewrites entries in place, so a live pointer implies
// the same entry).
func (m Model) networkDetailStillPresent() bool {
	if m.networkDetailAnchor == nil || m.journal == nil {
		return false
	}
	return slices.Contains(m.journal.Entries(), m.networkDetailAnchor)
}

// wrapText breaks a plain-text string into rows that each fit within width
// terminal cells, breaking on spaces when possible and on grapheme-cluster
// boundaries when a single word exceeds the width. It never drops text.
func wrapText(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	if width <= 0 {
		return []string{text}
	}
	var rows []string
	var current strings.Builder
	currentWidth := 0
	for word := range strings.FieldsSeq(text) {
		wordWidth := uniseg.StringWidth(word)
		if currentWidth > 0 && currentWidth+1+wordWidth > width {
			rows = append(rows, current.String())
			current.Reset()
			currentWidth = 0
		}
		if currentWidth > 0 {
			current.WriteByte(' ')
			currentWidth++
		}
		// A single word wider than width must be split on grapheme
		// boundaries so no text is ever lost. Progress is guaranteed because
		// the split always consumes at least one grapheme cluster: a cluster
		// wider than the remaining cut is emitted whole on its own row rather
		// than being repeatedly re-truncated to "" (a 2-cell emoji at width 1
		// would otherwise loop forever).
		for uniseg.StringWidth(word) > width {
			cut := width - currentWidth
			if cut <= 0 {
				rows = append(rows, current.String())
				current.Reset()
				currentWidth = 0
				cut = width
			}
			remaining := truncatePlain(word, cut)
			if remaining == "" {
				// The next cluster alone exceeds cut; emit it whole.
				remaining = firstGrapheme(word)
			}
			current.WriteString(remaining)
			word = word[len(remaining):]
			rows = append(rows, current.String())
			current.Reset()
			currentWidth = 0
		}
		current.WriteString(word)
		currentWidth += uniseg.StringWidth(word)
	}
	if current.Len() > 0 {
		rows = append(rows, current.String())
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	return rows
}

func (m Model) renderNetworkDetail(e *journal.Entry) string {
	// Compute a line budget so the overlay fits within the terminal.
	// The overlay is drawn inside the scrollable content area; its border and
	// padding consume 4 rows. Reserve at least a minimum usable detail view.
	budget := max(m.visibleRows()-4, 10)

	// Count fixed lines that are always emitted (minimum 17):
	//   Request heading, Method, URL, [blank],
	//   Response heading, Status, Type, Size, [blank],
	//   Timing heading, Queue, TTFB, Download, Total, Waterfall,
	//   [blank], close
	const fixedLines = 17

	// Remaining budget for variable sections: headers and body previews.
	varBudget := max(budget-fixedLines,
		// at least 2 lines each for req/resp headers
		4)

	// Split the variable budget: half for request, half for response.
	reqBudget := varBudget / 2
	respBudget := varBudget - reqBudget

	var b strings.Builder

	b.WriteString(m.styles.sectionStyle.Render(" Request "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Method:   %s\n", e.Method)
	fmt.Fprintf(&b, " URL:      %s\n", e.URL)
	usedReq := 3 // heading + method + url

	if len(e.RequestHeaders) > 0 && usedReq < reqBudget {
		keys := sortedHeaderKeys(e.RequestHeaders)
		b.WriteString(" Headers:\n")
		usedReq++ // "Headers:" line
		maxHeaderLines := max(
			// -1 for potential body line
			reqBudget-usedReq-1, 1)
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
		marker := ""
		if e.RequestBodyTruncated {
			// Before the preview: the preview is capped at 256 runes, so a
			// trailing marker would be clipped on a normal terminal width.
			marker = "(truncated) "
		}
		fmt.Fprintf(&b, " Body:     %s%s\n", marker, truncateBytes(e.RequestBody, 256))
	}
	b.WriteByte('\n')

	b.WriteString(m.styles.sectionStyle.Render(" Response "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Status:   %d\n", e.StatusCode)
	if e.Aborted {
		b.WriteString(" Outcome:  aborted\n")
	}
	fmt.Fprintf(&b, " Type:     %s\n", e.Type())
	fmt.Fprintf(&b, " Size:     %s\n", e.SizeLabel())
	usedResp := 4 // heading + status + type + size
	if e.Aborted {
		usedResp++
	}

	if len(e.ResponseHeaders) > 0 && usedResp < respBudget {
		keys := sortedHeaderKeys(e.ResponseHeaders)
		b.WriteString(" Headers:\n")
		usedResp++
		maxHeaderLines := max(respBudget-usedResp-1, 1)
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
	}
	b.WriteByte('\n')

	b.WriteString(m.styles.sectionStyle.Render(" Timing "))
	b.WriteByte('\n')
	fmt.Fprintf(&b, " Queue:    %s\n", e.Timing.QueueDuration().Truncate(time.Millisecond))
	fmt.Fprintf(&b, " TTFB:     %s\n", e.Timing.TTFB().Truncate(time.Millisecond))
	fmt.Fprintf(&b, " Download: %s\n", e.Timing.DownloadDuration().Truncate(time.Millisecond))
	if e.Timing.ResponseComplete.IsZero() {
		b.WriteString(" Total:    —\n")
	} else {
		fmt.Fprintf(&b, " Total:    %s\n", e.Timing.Duration().Truncate(time.Millisecond))
	}

	barWidth := max(min(m.width-10, 60), 0)
	fmt.Fprintf(&b, " %s\n", m.renderDetailWaterfall(e, barWidth))

	b.WriteString("\n [Esc/Enter] close ")
	return m.styles.overlayStyle.Render(b.String())
}

func (m Model) renderDetailWaterfall(e *journal.Entry, width int) string {
	if width <= 0 {
		return ""
	}
	total := e.Timing.Duration()
	if total <= 0 {
		return strings.Repeat("─", width)
	}

	queue := e.Timing.QueueDuration()
	ttfb := e.Timing.TTFB()

	queueSeg := min(int(math.Round(float64(queue)/float64(total)*float64(width))), width)
	ttfbSeg := int(math.Round(float64(ttfb) / float64(total) * float64(width)))
	if queueSeg+ttfbSeg > width {
		ttfbSeg = width - queueSeg
	}
	downloadSeg := max(width-queueSeg-ttfbSeg, 0)

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

func (m Model) renderHelpOverlay() string {
	// The provider-switch binding exists only when the header renders the
	// switcher (multi-provider, or a single named provider). A single
	// unnamed provider keeps the legacy overlay byte-identical.
	switcher := ""
	if m.hasSwitcher() {
		switcher = " Tab/Shift+Tab / Click name ↕ / Wheel  Switch provider\n"
	}
	return m.styles.overlayStyle.Render(" Keybindings \n\n"+
		" 1-6          Switch tab (Overview/Requests/Network/Logs/Concurrency/Routes)\n"+
		" j/k or ↑/↓   Scroll down/up\n"+
		" h/l or ←/→   Scroll left/right (Network/Logs)\n"+
		" PgUp/PgDn     Page up / Page down\n"+
		" Home/End      Jump to first / last item\n"+
		" Ctrl-U / Ctrl-D  Half-page scroll\n"+
		" g             Jump to top    G      Jump to bottom\n"+
		" Enter/Space   Inspect selected entry (full log message on Logs)\n"+
		" /             Filter entries (Requests/Network/Logs tabs)\n"+
		" t             Cycle type filter (Network tab)\n"+
		" s             Cycle status filter (Network tab)\n"+
		" c             Reset Stats (y confirms, n/Esc cancels)\n"+
		" Ctrl+K        Command palette\n"+
		switcher+
		" Esc           Close overlay / Clear filter\n"+
		" ?             Show this help\n"+
		" q / Ctrl+C    Quit\n\n"+
		" Mouse: wheel scroll, click tabs to switch\n\n"+
		" [Any key] close ") + "\n"
}

func (m Model) renderFooter() string {
	keys := " 1-6:tab │ j/k:scroll │ h/l:hscroll │ PgUp/PgDn │ Home/End │ Ctrl-U/D │ /:filter │ t:type │ s:status │ c:reset │ ?:help │ q:quit "
	return m.styles.footerStyle.Render(keys)
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
