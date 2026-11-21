package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/investigation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

type exportState struct {
	pending                           bool
	rawPrompt, rawReview              bool
	input                             string
	selection                         investigation.Selection
	page, seenThrough, focus, pressed int
}
type exportResultMsg struct {
	path string
	err  error
	raw  bool
}

func (m Model) networkSelection(all bool) investigation.Selection {
	selection := investigation.Selection{Label: "TUI frozen selection"}
	if m.inspection.open && m.inspection.entry != nil {
		selection.Entries = []*journal.Entry{m.inspection.entry}
	} else {
		entries := m.visibleNetworkEntries()
		if all {
			selection.Entries = append([]*journal.Entry(nil), entries...)
		} else if m.cursor < len(entries) {
			selection.Entries = []*journal.Entry{entries[m.cursor]}
		}
	}
	for _, e := range selection.Entries {
		selection.RequestIDs = append(selection.RequestIDs, e.RequestID)
	}
	return selection
}

// clonePoints copies every slice reachable from a point so a later
// observation tick cannot mutate the value an async export has already frozen.
func clonePoints(points []observation.Point) []observation.Point {
	if points == nil {
		return nil
	}
	out := make([]observation.Point, len(points))
	for i, p := range points {
		out[i] = p
		out[i].Resources = slices.Clone(p.Resources)
	}
	return out
}

func cloneQueryResult(result observation.QueryResult) observation.QueryResult {
	result.Query.Providers = slices.Clone(result.Query.Providers)
	result.Points = clonePoints(result.Points)
	result.Markers = slices.Clone(result.Markers)
	result.ByProvider = slices.Clone(result.ByProvider)
	for i := range result.ByProvider {
		result.ByProvider[i].Points = clonePoints(result.ByProvider[i].Points)
	}
	return result
}

func cloneRequestResult(result observation.RequestResult) observation.RequestResult {
	result.Requests = slices.Clone(result.Requests)
	for i := range result.Requests {
		result.Requests[i].Attempts = slices.Clone(result.Requests[i].Attempts)
		result.Requests[i].Usage = cloneUsage(result.Requests[i].Usage)
	}
	return result
}

func cloneUsage(u *observation.Usage) *observation.Usage {
	if u == nil {
		return nil
	}
	copy := *u
	return &copy
}

func cloneControlSnapshot(s control.Snapshot) control.Snapshot {
	s.Providers = slices.Clone(s.Providers)
	for i := range s.Providers {
		s.Providers[i].Pools = slices.Clone(s.Providers[i].Pools)
	}
	s.Audit = slices.Clone(s.Audit)
	return s
}

func cloneFact(f observation.RequestFact) observation.RequestFact {
	f.Attempts = slices.Clone(f.Attempts)
	f.Usage = cloneUsage(f.Usage)
	return f
}

func (m Model) savedExportLines(path string, raw bool) []string {
	if raw {
		return []string{"RAW INVESTIGATION EXPORT", "File: " + path, "", "This bundle includes retained raw bodies, arbitrary HTTP headers and URLs, and may contain credentials, prompts, tool arguments or responses.", "Review its contents before sharing. Existing files are never overwritten."}
	}
	return []string{"METADATA EXPORT (safe by default)", "File: " + path, "", "This export contains the frozen selection taken when you pressed export.", "Arbitrary bodies, headers and URLs are omitted from investigation bundles.", "Existing files are never overwritten."}
}

func (m *Model) beginExport(kind string) tea.Cmd {
	if m.export.pending {
		m.tell("Export is already writing")
		return nil
	}
	if m.runtime.ExportDir == "" {
		m.tell("Set -export-dir to an existing directory")
		return nil
	}
	dir := m.runtime.ExportDir
	var value any
	var selection *investigation.Selection
	switch kind {
	case "measurements":
		src := cloneQueryResult(m.insights.result)
		result := cloneQueryResult(m.insights.result)
		from, to := m.insights.selectionRange()
		result.Query.From = from
		result.Query.To = to
		// Export every whole interval in the selected range; edge exclusions are
		// explicit. The plotted cursor never supplies invented fractional counts.
		result.Points = nil
		for _, p := range src.Points {
			if !p.Start.Before(from) && !p.End.After(to) {
				result.Points = append(result.Points, p)
			}
		}
		result.Markers = nil
		for _, v := range src.Markers {
			if !v.At.Before(from) && v.At.Before(to) {
				result.Markers = append(result.Markers, v)
			}
		}
		result.ByProvider = nil
		for _, series := range src.ByProvider {
			copySeries := series
			copySeries.Points = nil
			for _, p := range series.Points {
				if !p.Start.Before(from) && !p.End.After(to) {
					copySeries.Points = append(copySeries.Points, p)
				}
			}
			result.ByProvider = append(result.ByProvider, copySeries)
		}
		value = struct {
			Selection    observation.Query       `json:"selection"`
			EdgePolicy   string                  `json:"edge_policy"`
			Observations observation.QueryResult `json:"observations"`
		}{result.Query, "whole contained intervals; partial edges are excluded", result}
	case "requests":
		query := m.displayedFactQuery()
		query.Providers = slices.Clone(query.Providers)
		value = struct {
			Query  observation.RequestQuery  `json:"query"`
			Result observation.RequestResult `json:"result"`
		}{query, cloneRequestResult(m.insights.facts)}
	case "logs":
		value = struct {
			Filter string   `json:"filter"`
			Lines  []string `json:"lines"`
		}{m.filterText, append([]string(nil), m.visibleLogLines()...)}
	case "controls":
		value = cloneControlSnapshot(m.controlPanel.snapshot)
	case "network":
		s := m.networkSelection(true)
		selection = &s
	case "inspection":
		if m.inspection.entry != nil {
			s := m.networkSelection(false)
			selection = &s
		} else if m.inspection.fact != nil {
			value = cloneFact(*m.inspection.fact)
		} else if m.inspection.exportValue != nil {
			value = m.inspection.exportValue
		} else {
			value = struct {
				Title string   `json:"title"`
				Lines []string `json:"lines"`
			}{m.inspection.title, append([]string(nil), m.inspection.rawLines...)}
		}
	default:
		m.tell("Nothing selected to export")
		return nil
	}
	m.export.pending = true
	m.tell("Writing frozen " + kind + " selection…")
	return func() tea.Msg {
		var path string
		var err error
		if selection != nil {
			path, err = investigation.Write(dir, *selection, false)
		} else {
			path, err = investigation.WriteJSON(dir, kind, value)
		}
		return exportResultMsg{path: path, err: err}
	}
}
func (m *Model) beginRawExport() tea.Cmd {
	if m.export.pending {
		return nil
	}
	if m.runtime.ExportDir == "" {
		m.tell("Set -export-dir to an existing directory")
		return nil
	}
	selection := m.networkSelection(false)
	if len(selection.Entries) == 0 {
		m.tell("Select a retained Network request first")
		return nil
	}
	m.export = exportState{rawPrompt: true, selection: selection, pressed: -1}
	return nil
}
func (m Model) rawReviewLines() []string {
	rows := []string{"RAW INVESTIGATION EXPORT", fmt.Sprintf("Directory: %s", m.runtime.ExportDir), fmt.Sprintf("Requests: %d", len(m.export.selection.Entries)), ""}
	for _, e := range m.export.selection.Entries {
		rows = append(rows, "Exact request: "+e.RequestID, fmt.Sprintf("Journal ID: %d", e.ID))
	}
	rows = append(rows, "", "This file includes retained raw bodies, arbitrary HTTP headers and URLs. It may contain credentials, prompts, tool arguments or responses.", "Existing retention and capture limits remain visible. Export does not reconstruct missing bytes.", "A new file is created; existing files are never overwritten.", "Share this file only after inspecting its contents.")
	var wrapped []string
	for _, r := range rows {
		wrapped = append(wrapped, wrapText(safeTerminal(r), max(m.viewportWidth()-2, 1))...)
	}
	return wrapped
}
func (m Model) rawReviewPages() int {
	n := max(m.visibleRows()-3, 1)
	return max((len(m.rawReviewLines())+n-1)/n, 1)
}
func (m Model) rawCanApply() bool {
	return m.export.rawReview && m.export.seenThrough >= m.rawReviewPages()-1 && m.width >= 28 && m.visibleRows() >= 6 && !m.export.pending
}
func (m *Model) writeRaw() tea.Cmd {
	if !m.rawCanApply() {
		return nil
	}
	s := m.export.selection
	dir := m.runtime.ExportDir
	m.export.rawReview = false
	m.export.pending = true
	return func() tea.Msg {
		p, e := investigation.Write(dir, s, true)
		return exportResultMsg{path: p, err: e, raw: true}
	}
}
func (m Model) handleExportKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	e := &m.export
	if e.rawPrompt {
		switch msg.String() {
		case "esc":
			e.rawPrompt = false
			e.selection = investigation.Selection{}
		case "backspace", "ctrl+h":
			e.input = dropGrapheme(e.input)
		case "ctrl+u":
			e.input = ""
		case "enter":
			if e.input == "RAW" {
				e.rawPrompt = false
				e.rawReview = true
				e.page = 0
				e.seenThrough = 0
				e.focus = 0
			} else {
				m.tell("Type RAW exactly to open the review")
			}
		default:
			if msg.Key().Text != "" && len(e.input) < 8 {
				e.input += msg.Key().Text
			}
		}
		return m, nil, true
	}
	if e.rawReview {
		switch msg.String() {
		case "esc":
			e.rawReview = false
			e.selection = investigation.Selection{}
		case "right", "pgdown", "j", "down":
			e.page = min(e.page+1, m.rawReviewPages()-1)
			if e.page <= e.seenThrough+1 {
				e.seenThrough = max(e.page, e.seenThrough)
			}
			e.focus = 0
			e.pressed = -1
		case "left", "pgup", "k", "up":
			e.page = max(e.page-1, 0)
			e.focus = 0
			e.pressed = -1
		case "tab", "shift+tab":
			if m.rawCanApply() {
				e.focus = 1 - e.focus
			}
		case "enter":
			if e.focus == 0 {
				e.rawReview = false
				e.selection = investigation.Selection{}
			} else {
				return m, m.writeRaw(), true
			}
		}
		return m, nil, true
	}
	return m, nil, false
}
func (m Model) renderRawPrompt() string {
	rows := []string{m.styles.queueWarnStyle.Render(" RAW EXPORT"), "", "Retained bytes can include credentials, prompts and tool arguments.", "", "Type RAW, then review the exact selection.", "", m.styles.filterPromptStyle.Render(" > " + m.export.input + "█"), "", "Enter review · Esc cancel"}
	var wrapped []string
	for _, r := range rows {
		if strings.Contains(r, "\x1b") {
			wrapped = append(wrapped, r)
		} else {
			wrapped = append(wrapped, wrapText(r, max(m.viewportWidth()-2, 1))...)
		}
	}
	return fitRows(wrapped, m.viewportWidth(), m.visibleRows())
}
func (m Model) renderRawReview() string {
	h := m.visibleRows()
	size := max(h-3, 1)
	rows := m.rawReviewLines()
	start := min(m.export.page*size, len(rows))
	out := append([]string(nil), rows[start:min(start+size, len(rows))]...)
	for len(out) < size {
		out = append(out, "")
	}
	out = append(out, m.styles.dimStyle2.Render(fmt.Sprintf(" Page %d/%d · ←/→ read", m.export.page+1, m.rawReviewPages())), m.styles.dimStyle2.Render(" Tab focus · Enter select · Esc cancel"))
	cancel, apply := " [ Cancel ] ", " [ Export RAW ] "
	if m.export.focus == 0 {
		cancel = m.styles.tabActiveStyle.Padding(0).Render(cancel)
	}
	if !m.rawCanApply() {
		apply = m.styles.dimStyle2.Render(" [ Read first ] ")
	} else if m.export.focus == 1 {
		apply = m.styles.queueWarnStyle.Reverse(true).Render(apply)
	} else {
		apply = m.styles.queueWarnStyle.Render(apply)
	}
	out = append(out, cancel+" "+apply)
	return fitRows(out, m.viewportWidth(), h)
}
func (m Model) rawButtonAt(x, y int) int {
	if !m.export.rawReview || y != m.contentStartRow()+m.visibleRows()-1 {
		return -1
	}
	if x >= 0 && x < 12 {
		return 0
	}
	// The rendered button is " [ Export RAW ] ", occupying cells 13..28
	// inclusive; a press on the closing bracket must still activate it.
	if x >= 13 && x <= 28 && m.rawCanApply() {
		return 1
	}
	return -1
}

func (m Model) exportStatus() string {
	if m.export.pending {
		return "Writing export…"
	}
	if time.Now().Before(m.statusUntil) {
		return m.statusText
	}
	return ""
}
