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
	"sort"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

// visibleEntries returns the currently visible request entries, respecting filter.
func (m *Model) visibleEntries() []metrics.RequestLogEntry {
	if m.filterText == "" {
		return m.snap.LogEntries
	}
	var filtered []metrics.RequestLogEntry
	lower := strings.ToLower(m.filterText)
	for _, e := range m.snap.LogEntries {
		if strings.Contains(strings.ToLower(e.Method), lower) ||
			strings.Contains(strings.ToLower(e.Path), lower) ||
			strings.Contains(strconv.Itoa(e.Status), lower) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// visibleNetworkEntries returns the cached filtered list. The cache is
// refreshed once per Update cycle to avoid redundant allocations.
func (m Model) visibleNetworkEntries() []*journal.Entry {
	return m.networkFiltered
}

// visibleLogLines returns log lines for the Logs tab, respecting filter.
func (m *Model) visibleLogLines() []string {
	items := m.logRing.snapshot()
	if len(items) == 0 {
		return nil
	}
	if m.filterText == "" {
		lines := make([]string, len(items))
		for i, item := range items {
			lines[i] = item.text
		}
		return lines
	}
	var filtered []string
	lower := strings.ToLower(m.filterText)
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.text), lower) {
			filtered = append(filtered, item.text)
		}
	}
	return filtered
}

// computeVisibleNetworkEntries performs the actual filter logic used to
// rebuild the networkFiltered cache.
func (m *Model) computeVisibleNetworkEntries() []*journal.Entry {
	var all []*journal.Entry
	m.networkSources = make(map[*journal.Entry]string)
	if m.enriched() && m.networkAll {
		for i, p := range m.providers {
			if p.journal == nil {
				continue
			}
			for _, e := range p.journal.Entries() {
				all = append(all, e)
				m.networkSources[e] = m.providerID(i)
			}
		}
		if m.runtime.IngressJournal != nil {
			for _, e := range m.runtime.IngressJournal.Entries() {
				all = append(all, e)
				m.networkSources[e] = "ingress"
			}
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].Timing.QueueStart.Before(all[j].Timing.QueueStart) })
	} else if m.journal != nil {
		all = m.journal.Entries()
		for _, e := range all {
			m.networkSources[e] = m.providerID(m.active)
		}
	}
	if all == nil {
		return nil
	}

	var filtered []*journal.Entry
	if m.filterText != "" {
		lower := strings.ToLower(m.filterText)
		for _, e := range all {
			if strings.Contains(strings.ToLower(e.Name()), lower) ||
				strings.Contains(strings.ToLower(e.Method), lower) ||
				networkMetadataMatches(e, lower) ||
				strings.Contains(strconv.Itoa(e.StatusCode), lower) ||
				strings.Contains(strings.ToLower(e.Type()), lower) {
				filtered = append(filtered, e)
			}
		}
	} else {
		filtered = all
	}

	if m.networkFilterType != networkFilterAll {
		var byType []*journal.Entry
		for _, e := range filtered {
			matches := false
			switch m.networkFilterType {
			case networkFilterJSON:
				matches = e.Type() == "json"
			case networkFilterHTML:
				matches = e.Type() == "html"
			case networkFilterEvents:
				matches = e.Type() == "events"
			case networkFilterOther:
				matches = e.Type() != "json" && e.Type() != "html" && e.Type() != "events"
			}
			if matches {
				byType = append(byType, e)
			}
		}
		filtered = byType
	}

	if m.networkFilterStatus != networkStatusAll {
		var byStatus []*journal.Entry
		for _, e := range filtered {
			matches := false
			switch m.networkFilterStatus {
			case networkStatus2xx:
				matches = e.StatusCode >= 200 && e.StatusCode < 300
			case networkStatus4xx:
				matches = e.StatusCode >= 400 && e.StatusCode < 500
			case networkStatus5xx:
				matches = e.StatusCode >= 500 && e.StatusCode < 600
			}
			if matches {
				byStatus = append(byStatus, e)
			}
		}
		filtered = byStatus
	}

	return filtered
}

// networkMetadataMatches searches bounded diagnostic metadata, never captured
// bodies. That keeps filtering responsive even with multi-megabyte exchanges.
func networkMetadataMatches(e *journal.Entry, q string) bool {
	if strings.Contains(strings.ToLower(e.RequestID), q) {
		return true
	}
	if e.URL != nil && strings.Contains(strings.ToLower(e.URL.Path), q) {
		return true
	}
	d := e.Diagnostic
	if d == nil {
		return false
	}
	fields := []string{d.ClientProtocol, d.UpstreamProtocol, d.Model, d.ClientModel}
	for _, f := range d.Failures {
		fields = append(fields, f.Stage, f.Kind, f.Path, f.Feature, f.Fingerprint)
	}
	for _, v := range d.Decisions {
		fields = append(fields, v.Stage, v.Feature, v.Path)
	}
	for k, v := range d.ClientVersions {
		fields = append(fields, k, v)
	}
	for _, v := range fields {
		if strings.Contains(strings.ToLower(v), q) {
			return true
		}
	}
	return false
}
