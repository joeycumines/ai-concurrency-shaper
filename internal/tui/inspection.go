package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

type inspectionState struct {
	exportValue     any
	open            bool
	title           string
	section         int
	scroll          int
	rawLines, lines []string
	entry           *journal.Entry
	fact            *observation.RequestFact
}

var inspectionSections = []string{"HTTP", "Decisions", "Client request", "Upstream request", "Upstream response", "Downstream response", "Contracts"}

func (m *Model) openPointInspection() {
	p, ok := m.cursorPoint()
	if !ok {
		m.inspection = inspectionState{open: true, title: "TIME CELL", rawLines: []string{"No observations at " + m.insights.cursor.UTC().Format(time.RFC3339Nano), "This missing cell is not replaced by an adjacent sample."}}
		m.reflowInspection()
		return
	}
	lines := m.insightDetails()
	for i := range lines {
		lines[i] = stripANSI(lines[i])
	}
	lines = append(lines, "", "Exact observation:")
	data, _ := json.MarshalIndent(p, "", "  ")
	lines = append(lines, strings.Split(string(data), "\n")...)
	m.inspection = inspectionState{open: true, title: "TIME CELL", rawLines: lines, exportValue: p}
	m.reflowInspection()
}
func (m *Model) openFact(f observation.RequestFact) {
	m.inspection = inspectionState{open: true, title: "REQUEST", fact: &f}
	m.rebuildInspection()
}
func (m *Model) openNetwork(entry *journal.Entry) {
	if entry == nil {
		return
	}
	m.inspection = inspectionState{open: true, title: "NETWORK EVIDENCE", entry: entry}
	m.rebuildInspection()
}
func (m *Model) openJoinedNetwork(id string) bool {
	for i, p := range m.providers {
		if p.journal == nil {
			continue
		}
		if e := p.journal.FindRequestID(id); e != nil {
			m.active = i
			m.syncActive()
			m.openNetwork(e)
			return true
		}
	}
	if m.runtime.IngressJournal != nil {
		if e := m.runtime.IngressJournal.FindRequestID(id); e != nil {
			m.openNetwork(e)
			return true
		}
	}
	m.tell("Raw journal entry was evicted; request metadata remains")
	return false
}
func bodyLines(body []byte) []string {
	const limit = 256 << 10
	shown := body
	if len(shown) > limit {
		shown = shown[:limit]
	}
	var lines []string
	if len(shown) == 0 {
		return []string{"No retained body bytes."}
	}
	if json.Valid(shown) {
		var b strings.Builder
		var val any
		if json.Unmarshal(shown, &val) == nil {
			j, e := json.MarshalIndent(val, "", "  ")
			if e == nil {
				b.Write(j)
				shown = []byte(b.String())
			}
		}
	}
	lines = strings.Split(safeTerminal(string(shown)), "\n")
	if len(body) > limit {
		lines = append(lines, "", "Viewer shows the first 256 KiB. Raw export includes all retained bytes.")
	}
	return lines
}
func headerLines(title string, h http.Header) []string {
	rows := []string{title}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			rows = append(rows, "  "+k+": "+v)
		}
	}
	return rows
}
func (m *Model) rebuildInspection() {
	s := &m.inspection
	s.rawLines = nil
	if e := s.entry; e != nil {
		switch s.section {
		case 0:
			url := ""
			if e.URL != nil {
				url = e.URL.String()
			}
			s.rawLines = []string{"Request ID: " + e.RequestID, fmt.Sprintf("Journal ID: %d", e.ID), e.Method + " " + url, fmt.Sprintf("HTTP %d · %s · %s", e.StatusCode, e.Type(), e.SizeLabel()), fmt.Sprintf("Total %s · queued %s", e.Timing.Duration(), e.Timing.QueueDuration()), fmt.Sprintf("Headers after %s · body phase %s", e.Timing.TTFB(), e.Timing.DownloadDuration()), fmt.Sprintf("Limited: %t · aborted: %t", e.Limited, e.Aborted), "", "Header latency is not model time to first token.", ""}
			s.rawLines = append(s.rawLines, headerLines("CLIENT REQUEST HEADERS", e.RequestHeaders)...)
			s.rawLines = append(s.rawLines, "")
			s.rawLines = append(s.rawLines, headerLines("UPSTREAM RESPONSE HEADERS", e.ResponseHeaders)...)
			// The journal's existing buffers and optional four-leg capture have
			// independent budgets. A metadata-only Diagnostic must never hide
			// bytes already retained by the journal.
			s.rawLines = append(s.rawLines, "", "JOURNAL WIRE BUFFERS", "For dispatched requests these are provider wire bytes, including rendered transcoded bytes. The four-leg sections distinguish original client and rendered protocol evidence.", "", "RETAINED JOURNAL REQUEST BODY")
			s.rawLines = append(s.rawLines, bodyLines(e.RequestBody)...)
			s.rawLines = append(s.rawLines, "", "RETAINED JOURNAL RESPONSE BODY")
			s.rawLines = append(s.rawLines, bodyLines(e.ResponseBody)...)
		case 1:
			if d := e.Diagnostic; d != nil {
				s.rawLines = []string{d.ClientProtocol + " → " + d.UpstreamProtocol, "Client model: " + d.ClientModel, "Resolved model: " + d.Model, "", "FAILURES"}
				for _, f := range d.Failures {
					s.rawLines = append(s.rawLines, f.Stage+" · "+f.Kind, "  path: "+f.Path, "  feature: "+f.Feature, "  fingerprint: "+f.Fingerprint, "  "+f.Detail, "")
				}
				if len(d.Failures) == 0 {
					s.rawLines = append(s.rawLines, "No captured conversion failure.")
				}
				s.rawLines = append(s.rawLines, "", "DECISIONS")
				for _, v := range d.Decisions {
					kind := "LOSS"
					if v.Note {
						kind = "NOTE"
					}
					s.rawLines = append(s.rawLines, kind+" · "+v.Stage+" · "+v.Feature, "  "+v.Path, "  "+v.Detail)
				}
				s.rawLines = append(s.rawLines, "", "Approved loss policy: "+strings.Join(d.AllowedLosses, ", "), "Capabilities: "+strings.Join(d.Capabilities, ", "), fmt.Sprintf("Dropped: %d stages, %d decisions, %d failures", d.DroppedStages, d.DroppedDecisions, d.DroppedFailures))
			} else {
				s.rawLines = []string{"No conversion diagnostics retained."}
			}
		case 2, 3, 4, 5:
			if d := e.Diagnostic; d != nil {
				leg := d.Legs[s.section-2]
				s.rawLines = []string{string(leg.Name), fmt.Sprintf("Observed: %t · complete: %t", leg.Observed, leg.Complete), legByteLabel(leg.BytesKnown, leg.ObservedBytes, leg.CapturedBytes), fmt.Sprintf("Truncated: %t · status: %d", leg.Truncated, leg.Status), "Content type: " + leg.ContentType, fmt.Sprintf("Total four-leg capture budget: %d bytes", d.CaptureBudget), ""}
				s.rawLines = append(s.rawLines, bodyLines(leg.Body)...)
			} else {
				s.rawLines = []string{"No four-leg capture for this native exchange.", "HTTP contains retained native request and response bytes."}
			}
		case 6:
			if d := e.Diagnostic; d != nil {
				s.rawLines = []string{fmt.Sprintf("Evidence schema %d · sealed %t", d.Version, d.Sealed), "", "CONTRACTS"}
				for _, c := range d.Contracts {
					s.rawLines = append(s.rawLines, c.Name+" · "+c.Version, "SHA-256 "+c.SHA256)
				}
				s.rawLines = append(s.rawLines, "", "CLIENT VERSIONS")
				keys := make([]string, 0, len(d.ClientVersions))
				for k := range d.ClientVersions {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					s.rawLines = append(s.rawLines, k+": "+d.ClientVersions[k])
				}
				s.rawLines = append(s.rawLines, "", "STAGES")
				for _, stage := range d.Stages {
					s.rawLines = append(s.rawLines, stage.At.UTC().Format("15:04:05.000000000")+" "+stage.Name)
				}
			} else {
				s.rawLines = []string{"No semantic contracts for a native passthrough."}
			}
		}
	} else if f := s.fact; f != nil {
		s.rawLines = []string{"Request ID: " + f.ID, "Provider: " + f.Provider, "Received provider: " + f.ReceivedProvider, f.Method + " " + f.Path, "Route: " + f.Route, fmt.Sprintf("Outcome: %s · HTTP %d · live %t", f.Outcome, f.Status, f.Live), "Failure stage: " + string(f.Stage), "Fingerprint: " + f.Fingerprint, "Started: " + f.Started.UTC().Format(time.RFC3339Nano), "Finished: " + f.Finished.UTC().Format(time.RFC3339Nano), fmt.Sprintf("Duration %s · queue wait %s", time.Duration(f.DurationNS), time.Duration(f.QueueNS)), fmt.Sprintf("Attempts %d · details truncated %t", f.AttemptCount, f.AttemptsTruncated), fmt.Sprintf("Conversion %d losses / %d notes", f.ConversionLosses, f.ConversionNotes), "", "n opens the exact retained Network request.", "x reviews cancellation of this ID if live.", "", "ATTEMPTS"}
		for _, a := range f.Attempts {
			s.rawLines = append(s.rawLines, fmt.Sprintf("#%d %s · HTTP %d · %s · %s", a.Number, a.Kind, a.Status, a.Outcome, time.Duration(a.DurationNS)))
		}
		if f.Usage != nil {
			s.rawLines = append(s.rawLines, "", fmt.Sprintf("USAGE · source %s", f.Usage.Source), fmt.Sprintf("Input %d known=%t", f.Usage.Input, f.Usage.InputKnown), fmt.Sprintf("Output %d known=%t", f.Usage.Output, f.Usage.OutputKnown), fmt.Sprintf("Total %d known=%t", f.Usage.Total, f.Usage.TotalKnown), fmt.Sprintf("Corrections %d", f.UsageCorrections))
		}
	}
	m.reflowInspection()
}
func (m *Model) reflowInspection() {
	s := &m.inspection
	s.lines = nil
	for _, r := range s.rawLines {
		s.lines = append(s.lines, wrapText(safeTerminal(r), max(m.viewportWidth()-2, 1))...)
	}
	s.scroll = max(0, min(s.scroll, len(s.lines)-m.inspectionRows()))
}
func (m Model) inspectionRows() int { return max(m.visibleRows()-3, 1) }
func (m Model) renderInspection() string {
	s := m.inspection
	rows := []string{m.styles.sectionStyle.Render(" " + s.title)}
	if s.entry != nil {
		label := fmt.Sprintf(" %d/7 %s · Tab sections", s.section+1, inspectionSections[s.section])
		rows = append(rows, m.styles.tableHeaderStyle.Render(label))
	} else {
		rows = append(rows, m.styles.dimStyle2.Render(" ↑↓ scroll · e export · Esc return"))
	}
	n := m.inspectionRows()
	for i := s.scroll; i < min(s.scroll+n, len(s.lines)); i++ {
		rows = append(rows, " "+s.lines[i])
	}
	for len(rows) < n+2 {
		rows = append(rows, "")
	}
	rows = append(rows, m.styles.dimStyle2.Render(fmt.Sprintf(" %d–%d / %d · e metadata · E raw", min(s.scroll+1, len(s.lines)), min(s.scroll+n, len(s.lines)), len(s.lines))))
	return fitRows(rows, m.viewportWidth(), m.visibleRows())
}
func (m Model) handleInspectionKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.inspection.open {
		return m, nil, false
	}
	s := &m.inspection
	switch msg.String() {
	case "esc":
		s.open = false
		return m, nil, true
	case "j", "down":
		s.scroll++
	case "k", "up":
		s.scroll--
	case "pgdown", "ctrl+d":
		s.scroll += m.inspectionRows()
	case "pgup", "ctrl+u":
		s.scroll -= m.inspectionRows()
	case "home", "g":
		s.scroll = 0
	case "end", "G":
		s.scroll = max(len(s.lines)-m.inspectionRows(), 0)
	case "tab", "shift+tab":
		if s.entry != nil {
			d := 1
			if msg.String() == "shift+tab" {
				d = 6
			}
			s.section = (s.section + d) % 7
			s.scroll = 0
			m.rebuildInspection()
		}
	case "n":
		if s.fact != nil {
			m.openJoinedNetwork(s.fact.ID)
		}
	case "x":
		if s.fact != nil && s.fact.Live {
			return m, m.previewControl(cancelCommand(*s.fact)), true
		}
	case "e":
		return m, m.beginExport("inspection"), true
	case "E":
		if s.entry != nil {
			return m, m.beginRawExport(), true
		}
	}
	s.scroll = max(0, min(s.scroll, len(s.lines)-m.inspectionRows()))
	return m, nil, true
}

func legByteLabel(known bool, observed int64, captured int) string {
	if !known {
		return fmt.Sprintf("Observed bytes unknown · captured %d", captured)
	}
	return fmt.Sprintf("Observed %d bytes · captured %d", observed, captured)
}

func (m *Model) openDistributionInspection() {
	a, b := m.insights.selectionRange()
	var h observation.Histogram
	var excluded int
	for _, p := range m.insights.result.Points {
		if !p.Start.Before(a) && !p.End.After(b) {
			h.Add(m.pointHistogram(p))
		} else if p.End.After(a) && p.Start.Before(b) {
			excluded++
		}
	}
	i := min(max(m.insights.bin, 0), 22)
	rows := []string{"Measure: " + m.latencyName() + " duration", a.UTC().Format(time.RFC3339) + " → " + b.UTC().Format(time.RFC3339), fmt.Sprintf("Selected bin (%s, %s]", secondsLabel(binLower(i)), secondsLabel(binUpper(i))), fmt.Sprintf("%d observations in bin; %d observations in selection", h.Bins[i], h.Count), fmt.Sprintf("%d partial edge intervals excluded", excluded), "", "Counts use whole contained observation intervals. Percentiles merge disjoint bins before estimation. i from the chart opens exact matching request facts."}
	if h.Count > 0 {
		rows = append(rows, fmt.Sprintf("Bin share %.3f%%", 100*float64(h.Bins[i])/float64(h.Count)))
	}
	m.inspection = inspectionState{open: true, title: "DURATION DISTRIBUTION", rawLines: rows, exportValue: struct {
		From, To    time.Time
		Measure     string
		Histogram   observation.Histogram
		SelectedBin int
	}{a, b, m.latencyName(), h, i}}
	m.reflowInspection()
}
