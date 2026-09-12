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
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/rivo/uniseg"
)

func TestDashboardShowsSparkline(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m.snap.Sparkline = []int{0, 1, 3, 5, 8, 5, 3, 1, 0}

	v := m.View()
	if !strings.Contains(v.Content, "Throughput") {
		t.Error("Dashboard should contain throughput sparkline section")
	}
}

func TestSparklineEmptyState(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m.snap.Sparkline = nil
	v := m.View()
	if !strings.Contains(v.Content, m.styles.dimStyle2.Render("  —")) {
		t.Errorf("Empty sparkline should render a dim em dash, got: %s", v.Content)
	}
}

func TestConcurrencyTabInFlight(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{
		{ID: 1, Method: "POST", Path: "/v1/messages", Limited: true},
		{ID: 2, Method: "GET", Path: "/health", Limited: false},
	}

	v := m.View()
	if !strings.Contains(v.Content, "POST") {
		t.Error("Concurrency tab should show in-flight methods")
	}
	if !strings.Contains(v.Content, "/v1/messages") {
		t.Error("Concurrency tab should show paths")
	}
}

func TestDashboardSummary(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.tab = tabDashboard
	m.snap.TotalProxied = 100
	m.snap.TotalPassThrough = 50
	m.snap.TotalTimeout = 3
	m.snap.TotalCancelled = 1
	m.snap.TotalCircuitRejected = 2
	m.snap.StatusCounts = [6]int64{0, 0, 90, 5, 8, 3}

	v := m.View()
	if !strings.Contains(v.Content, "Summary") {
		t.Error("Dashboard should have a Summary section")
	}
	if !strings.Contains(v.Content, "Clean proxied: 100") {
		t.Error("Dashboard should show clean proxied count")
	}
	if !strings.Contains(v.Content, "Clean passthrough: 50") {
		t.Error("Dashboard should show clean passthrough count")
	}
}

func TestDashboardSummaryShowsAborted(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 40
	m.tab = tabDashboard
	m.snap.TotalProxied = 10
	m.snap.TotalPassThrough = 5
	m.snap.TotalAborted = 2
	m.snap.StatusCounts = [6]int64{0, 0, 12, 0, 0, 0}

	v := m.View()
	content := stripANSI(v.Content)
	if !strings.Contains(content, "Aborted: 2") {
		t.Fatalf("Dashboard should show aborted count, got: %s", content)
	}
	bar := stripANSI(strings.Join(m.renderStatusBar(m.hBarWidth()), " "))
	if !strings.Contains(bar, "Aborted:2") {
		t.Fatalf("status bar should label aborted committed statuses, got: %s", bar)
	}
}

func TestConcurrencyTabInFlightEmptyIsDim(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	s := m.renderConcurrency()
	if !strings.Contains(s, m.styles.dimStyle2.Render("  No requests in flight.\n")) {
		t.Errorf("Concurrency tab should show a dim empty in-flight message, got: %s", s)
	}
}

func TestRequestsTabMarksAbortedRows(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Time: time.Now(), Method: "POST", Path: "/v1/messages", Status: 200, Aborted: true},
	}

	s := stripANSI(m.renderRequests())
	if !strings.Contains(s, "/v1/messages [aborted]") {
		t.Fatalf("Requests tab should mark aborted rows, got: %s", s)
	}
}

// TestDashboard_SummaryFitsViewport reproduces the Summary section
// overflow: the composed Summary rows must fit within the viewport and
// every metric must remain visible at widths 40, 80, and 120. The
// single pre-fix row renders 114 cells (measured), overflowing the
// 39- and 79-cell viewports and silently truncating the rightmost
// metrics.
func TestDashboard_SummaryFitsViewport(t *testing.T) {
	tests := []struct {
		name  string
		width int
		set   func(*metrics.Snapshot)
	}{
		{name: "zero-widths-40", width: 40},
		{name: "zero-widths-80", width: 80},
		{name: "zero-widths-120", width: 120},
		{
			name:  "multidigit-widths-40",
			width: 40,
			set: func(s *metrics.Snapshot) {
				s.TotalProxied = 123456789
				s.TotalPassThrough = 987654321
				s.TotalAborted = 12345
				s.TotalTimeout = 67890
				s.TotalCancelled = 111213
				s.TotalCircuitRejected = 141516
			},
		},
		{
			name:  "multidigit-widths-80",
			width: 80,
			set: func(s *metrics.Snapshot) {
				s.TotalProxied = 123456789
				s.TotalPassThrough = 987654321
				s.TotalAborted = 12345
				s.TotalTimeout = 67890
				s.TotalCancelled = 111213
				s.TotalCircuitRejected = 141516
			},
		},
		{
			name:  "multidigit-widths-120",
			width: 120,
			set: func(s *metrics.Snapshot) {
				s.TotalProxied = 123456789
				s.TotalPassThrough = 987654321
				s.TotalAborted = 12345
				s.TotalTimeout = 67890
				s.TotalCancelled = 111213
				s.TotalCircuitRejected = 141516
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			if tt.set != nil {
				tt.set(&m.snap)
			}
			rows := summaryRows(m.dashboardLines())
			if len(rows) == 0 {
				t.Fatal("dashboard should render Summary metric rows")
			}
			for _, l := range rows {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("summary row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
			joined := strings.Join(rows, "\n")
			for _, label := range []string{"Clean proxied:", "Clean passthrough:", "Aborted:", "Timeouts:", "Cancelled:", "Circuit rejects:"} {
				if !strings.Contains(joined, label) {
					t.Errorf("summary should show %q, got: %q", label, joined)
				}
			}
		})
	}
}

// TestDashboard_InFlightSummaryFitsViewport reproduces the In-Flight
// summary overflow (review-09): "  N in-flight: L limited, P
// passthrough" spans exactly 39 cells for single-digit values — the
// entire 40-column viewport — and exceeds it the moment any value
// reaches two digits, so renderContentWithScrollbar silently truncates
// "passthrough". The summary must fit at any magnitude: counts are
// abbreviated via formatCount and, when the composed line still cannot
// fit, the parts pack into viewport-fitting rows. The absurd-magnitude
// cases deliberately exercise each counter independently (a consistent
// snapshot with int64-max limited AND passthrough counts would need an
// unallocatable slice; the render math treats the three numbers
// independently, and TestDashboard_AllLinesFitViewport covers the
// consistent path).
func TestDashboard_InFlightSummaryFitsViewport(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		flights   int
		limited   int64
		passthru  int64
		singleRow bool // renders the single-line "N in-flight: L limited, P passthrough" format
	}{
		{name: "single-digit", width: 40, flights: 2, limited: 1, passthru: 1, singleRow: true},
		{name: "double-digit", width: 40, flights: 10, limited: 8, passthru: 2},
		{name: "multi-digit", width: 40, flights: 123, limited: 65, passthru: 58},
		{name: "absurd", width: 40, flights: 3, limited: 9_223_372_036_854_775_807, passthru: 9_223_372_036_854_775_807},
		{name: "wide-single-digit", width: 80, flights: 2, limited: 1, passthru: 1, singleRow: true},
		{name: "wide-absurd", width: 80, flights: 3, limited: 9_223_372_036_854_775_807, passthru: 9_223_372_036_854_775_807, singleRow: true},
		{name: "wide-absurd-120", width: 120, flights: 3, limited: 9_223_372_036_854_775_807, passthru: 9_223_372_036_854_775_807, singleRow: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/width=%d", tt.name, tt.width), func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			m.snap.InFlight = make([]metrics.InFlightEntry, tt.flights)
			m.snap.InFlightLimited = tt.limited
			m.snap.InFlightPassthrough = tt.passthru

			rows := inflightSectionRows(m.dashboardLines())
			if len(rows) == 0 {
				t.Fatal("dashboard should render the In-Flight Requests section")
			}
			joined := strings.Join(rows, "\n")
			for _, label := range []string{"in-flight", "limited", "passthrough"} {
				if !strings.Contains(joined, label) {
					t.Errorf("in-flight summary should show %q, got: %q", label, joined)
				}
			}
			if tt.singleRow && !strings.Contains(joined, " in-flight: ") {
				t.Errorf("summary should use the single-line format at this width, got: %q", joined)
			}
			for _, l := range rows {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("in-flight row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
		})
	}
}

// TestDashboard_InFlightRowsFitViewport reproduces the in-flight entry
// row overflow: the path column width is derived from a
// fixed-overhead heuristic (viewportWidth()-23) that assumes the age
// renders in at most 8 cells and the method in at most 6. A request
// with a multi-hour age (e.g. "1h2m3.004s" = 10 cells) or a method
// wider than 6 cells (OPTIONS, which %-6s pads but never truncates)
// makes the row exceed the viewport, so renderContentWithScrollbar
// silently truncates the age. The path column must shrink per row to
// absorb the actual age and method widths.
func TestDashboard_InFlightRowsFitViewport(t *testing.T) {
	tests := []struct {
		name    string
		width   int
		method  string
		path    string
		age     time.Duration
		limited bool
	}{
		{name: "short-age", width: 40, method: "GET", path: "/v1/health", age: time.Second, limited: true},
		{name: "long-age", width: 40, method: "GET", path: "/v1/messages", age: time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, limited: true},
		{name: "long-age-options", width: 40, method: "OPTIONS", path: "/v1/messages", age: time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, limited: false},
		{name: "wide-long-age", width: 80, method: "GET", path: "/v1/messages", age: time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, limited: true},
		{name: "wide-short-age", width: 80, method: "POST", path: "/v1/messages", age: 2 * time.Second, limited: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/width=%d", tt.name, tt.width), func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			m.snap.InFlight = []metrics.InFlightEntry{{
				Method:    tt.method,
				Path:      tt.path,
				StartTime: time.Now().Add(-tt.age),
				Limited:   tt.limited,
			}}
			if tt.limited {
				m.snap.InFlightLimited = 1
			} else {
				m.snap.InFlightPassthrough = 1
			}

			rows := inflightSectionRows(m.dashboardLines())
			if len(rows) == 0 {
				t.Fatal("dashboard should render the In-Flight Requests section")
			}
			joined := strings.Join(rows, "\n")
			if !strings.Contains(joined, tt.method) {
				t.Errorf("in-flight row should show method %q, got: %q", tt.method, joined)
			}
			// The fix may shrink the path column but must never drop
			// or truncate the age itself.
			if tt.age >= time.Hour && !strings.Contains(joined, "1h2m3") {
				t.Errorf("in-flight row should show the full age, got: %q", joined)
			}
			for _, l := range rows {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("in-flight row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
		})
	}
}

// TestDashboard_AllLinesFitViewport asserts the whole-dashboard
// invariant: every row rendered by dashboardLines fits within the
// viewport at widths 40/80/120 across representative snapshots
// (default zero values, circuit breaker CLOSED/OPEN, retries in
// flight, multi-digit counts, sparse status counts, and in-flight
// request entries).
func TestDashboard_AllLinesFitViewport(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		set  func(*metrics.Snapshot)
	}{
		{name: "zero"},
		{
			name: "breaker-closed",
			set: func(s *metrics.Snapshot) {
				s.CircuitBreaker = &metrics.CBStats{
					State:               "CLOSED",
					Failures:            3,
					ConsecutiveFailures: 2,
					CurrentPenalty:      500 * time.Millisecond,
					NextRetry:           now.Add(500 * time.Millisecond),
				}
			},
		},
		{
			name: "breaker-open",
			set: func(s *metrics.Snapshot) {
				s.CircuitBreaker = &metrics.CBStats{
					State:               "OPEN",
					Failures:            25,
					ConsecutiveFailures: 25,
					NextRetry:           now.Add(5 * time.Second),
				}
			},
		},
		{
			name: "retries-in-flight",
			set: func(s *metrics.Snapshot) {
				s.RetriesInFlight = 3
			},
		},
		{
			name: "multidigit",
			set: func(s *metrics.Snapshot) {
				s.StatusCounts = [6]int64{0, 12_345_678, 0, 123_456_789, 0, 0}
				s.TotalProxied = 123_456_789
				s.TotalPassThrough = 987_654_321
				s.TotalAborted = 123_456_789
				s.TotalTimeout = 111_222_333
				s.TotalCancelled = 444_555_666
				s.TotalCircuitRejected = 777_888_999
			},
		},
		{
			name: "sparse",
			set: func(s *metrics.Snapshot) {
				s.StatusCounts = [6]int64{0, 0, 5, 0, 0, 0}
			},
		},
		{
			// A consistent snapshot per metrics.go (Snapshot derives
			// InFlightLimited/InFlightPassthrough from the InFlight
			// slice), with double-digit totals so the summary line
			// cannot pass at width 40 by a zero-cell margin.
			name: "inflight-entries",
			set: func(s *metrics.Snapshot) {
				s.InFlight = make([]metrics.InFlightEntry, 10)
				for i := range s.InFlight {
					s.InFlight[i] = metrics.InFlightEntry{
						Method:    "POST",
						Path:      "/v1/messages",
						StartTime: now.Add(-time.Duration(i+1) * time.Second),
						Limited:   i < 8,
					}
				}
				// One multi-hour age: "1h2m3.004s" renders 10 cells, so
				// the path column must absorb it.
				s.InFlight[0].StartTime = now.Add(-(time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond))
				s.InFlightLimited = 8
				s.InFlightPassthrough = 2
			},
		},
	}
	for _, width := range []int{40, 80, 120} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("width=%d/%s", width, tt.name), func(t *testing.T) {
				m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
				m.width = width
				m.height = 40
				if tt.set != nil {
					tt.set(&m.snap)
				}
				for _, l := range m.dashboardLines() {
					if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
						t.Errorf("dashboard row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
					}
				}
			})
		}
	}
}

// TestDashboard_StatusLineFitsViewport reproduces the status-row overflow:
// the composed production row ("  Status  " prefix + bar + labels) must fit
// the viewport and every non-zero status class (plus Aborted) must remain
// visible. It exercises the real dashboardLines() composition, including
// sparse distributions (the budget loop skips zero classes while the render
// loop used to print them all) and multi-digit counts.
func TestDashboard_StatusLineFitsViewport(t *testing.T) {
	tests := []struct {
		name     string
		width    int
		counts   [6]int64
		aborted  int64
		expected []string
	}{
		{
			name:     "narrow-all-nonzero",
			width:    40,
			counts:   [6]int64{0, 1, 20, 3, 4, 5},
			expected: []string{"1xx:1", "2xx:20", "3xx:3", "4xx:4", "5xx:5"},
		},
		{
			// repro: at 40 columns the wrapped labels row
			// ("  " + six parts joined + "  ") is 43 cells even for
			// single-digit counts, exceeding the 39-cell viewport, so
			// renderContentWithScrollbar silently truncates "Aborted:6".
			name:     "narrow-all-nonzero-aborted",
			width:    40,
			counts:   [6]int64{0, 1, 2, 3, 4, 5},
			aborted:  6,
			expected: []string{"1xx:1", "2xx:2", "3xx:3", "4xx:4", "5xx:5", "Aborted:6"},
		},
		{
			// repro: with all five classes and Aborted at
			// multi-billion magnitudes the wrapped labels row spans 67
			// cells (labelsWidth 64 + 3) — the last 28 cells are cut off.
			name:     "narrow-all-nonzero-aborted-big",
			width:    40,
			counts:   [6]int64{0, 12_345_678_901, 12_345_678_901, 12_345_678_901, 12_345_678_901, 12_345_678_901},
			aborted:  12_345_678_901,
			expected: []string{"1xx:12.3B", "2xx:12.3B", "3xx:12.3B", "4xx:12.3B", "5xx:12.3B", "Aborted:12.3B"},
		},
		{
			name:     "narrow-sparse",
			width:    40,
			counts:   [6]int64{0, 0, 2, 0, 2, 2},
			expected: []string{"2xx:2", "4xx:2", "5xx:2"},
		},
		{
			name:     "standard-sparse-multidigit",
			width:    80,
			counts:   [6]int64{0, 0, 10_000_000, 0, 123, 12},
			expected: []string{"2xx:10.0M", "4xx:123", "5xx:12"},
		},
		{
			name:     "standard-multidigit-aborted",
			width:    80,
			counts:   [6]int64{0, 99_999, 5, 8, 999, 3},
			aborted:  77_777,
			expected: []string{"1xx:99999", "2xx:5", "3xx:8", "4xx:999", "5xx:3", "Aborted:77777"},
		},
		{
			name:     "empty-with-aborted",
			width:    80,
			aborted:  42,
			expected: []string{"Aborted:42"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tt.width
			m.height = 40
			m.snap.StatusCounts = tt.counts
			m.snap.TotalAborted = tt.aborted

			lines := dashboardStatusLines(m.dashboardLines())
			if len(lines) == 0 {
				t.Fatal("dashboard should render a Status section")
			}
			for _, l := range lines {
				if got := uniseg.StringWidth(stripANSI(l)); got > m.viewportWidth() {
					t.Errorf("status row width = %d exceeds viewport %d; row = %q", got, m.viewportWidth(), stripANSI(l))
				}
			}
			joined := strings.Join(lines, "\n")
			for _, label := range tt.expected {
				if !strings.Contains(joined, label) {
					t.Errorf("status section missing %q; rows = %q", label, joined)
				}
			}
		})
	}
}

func TestRenderDashboard_CircuitBreakerColors(t *testing.T) {
	for _, state := range []string{"CLOSED", "OPEN", "HALF_OPEN"} {
		t.Run(state, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = 80
			m.height = 40
			m.snap.CircuitBreaker = &metrics.CBStats{State: state}
			s := m.renderDashboardContent()
			if !strings.Contains(s, state) {
				t.Errorf("should show state %q, got: %s", state, s)
			}
		})
	}

	// Color assertions: CLOSED green, OPEN red, HALF_OPEN amber.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
	closed := m.renderDashboardContent()
	if !strings.Contains(closed, "38;2;63;185;80") {
		t.Errorf("CLOSED should render green, got: %s", closed)
	}

	m.snap.CircuitBreaker = &metrics.CBStats{State: "OPEN"}
	open := m.renderDashboardContent()
	if !strings.Contains(open, "38;2;248;81;73") {
		t.Errorf("OPEN should render red, got: %s", open)
	}

	m.snap.CircuitBreaker = &metrics.CBStats{State: "HALF_OPEN"}
	half := m.renderDashboardContent()
	if !strings.Contains(half, "38;2;240;136;62") {
		t.Errorf("HALF_OPEN should render amber, got: %s", half)
	}
}

func TestRenderDashboard_CircuitBreakerSingleLine(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 40
	m.snap.CircuitBreaker = &metrics.CBStats{
		State:               "OPEN",
		Failures:            5,
		ConsecutiveFailures: 3,
		CurrentPenalty:      2 * time.Second,
		NextRetry:           time.Now().Add(time.Hour),
	}
	s := m.renderDashboardContent()
	if strings.Contains(s, "\n  |  Failures") {
		t.Errorf("circuit breaker summary should be one line, got standalone field line in: %s", s)
	}
	if strings.Count(s, "\n  State:") != 1 {
		t.Errorf("expected exactly one State line, got: %s", s)
	}
}

func TestRenderContent_NarrowWidth(t *testing.T) {
	for w := 5; w <= 80; w++ {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRequests
		m.snap.LogEntries = []metrics.RequestLogEntry{
			{Method: "POST", Path: "/v1/messages", Status: 200, Duration: time.Millisecond},
		}
		s := m.renderRequests()
		if !strings.Contains(s, "POST") {
			t.Errorf("width=%d: renderRequests missing POST", w)
		}
	}
}

func TestRenderDashboard_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabDashboard
		s := m.renderDashboardContent()
		if !strings.Contains(s, "Throughput") && w >= 15 {
			t.Errorf("width=%d: renderDashboard missing Throughput", w)
		}
	}
}

func TestRenderNetwork_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabNetwork
		s := m.renderNetwork()
		// Should not panic at any width.
		_ = s
	}
}

func TestRenderLogs_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabLogs
		m.logRing.Write([]byte("test log line\n"))
		s := m.renderLogs()
		if !strings.Contains(s, "test log line") {
			t.Errorf("width=%d: renderLogs missing content", w)
		}
	}
}

func TestRenderConcurrency_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabConcurrency
		m.snap.InFlight = []metrics.InFlightEntry{
			{ID: 1, Method: "POST", Path: "/v1/messages", Limited: true},
		}
		s := m.renderConcurrency()
		if !strings.Contains(s, "POST") && w >= 15 {
			t.Errorf("width=%d: renderConcurrency missing POST", w)
		}
	}
}

func TestRenderRoutes_NarrowWidth(t *testing.T) {
	for w := 10; w <= 80; w += 5 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		m.tab = tabRoutes
		m.snap.RouteStats = map[string]metrics.RouteStat{
			"POST /v1/messages": {Total: 10},
		}
		s := m.renderRoutes()
		if !strings.Contains(s, "POST") && w >= 15 {
			t.Errorf("width=%d: renderRoutes missing POST", w)
		}
	}
}

// ─── Viewport: Scrollbar Model Integration ───

func TestDashboardCacheLazyEvaluation(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 5)

	m2 := update(m, metrics.Snapshot{})
	if m2.dashboardLinesCache != nil {
		t.Errorf("dashboardLinesCache should remain nil on non-Dashboard tab, got %d entries", len(m2.dashboardLinesCache))
	}

	m2.tab = tabDashboard
	_ = m2.cachedDashboardLines()
	if len(m2.dashboardLinesCache) == 0 {
		t.Error("cachedDashboardLines should build cache lazily when accessed on Dashboard")
	}
}

func TestDashboardCacheInvalidation(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	for i := range 20 {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
		})
	}

	m = update(m, metrics.Snapshot{})
	if len(m.dashboardLinesCache) == 0 {
		t.Fatal("expected dashboardLinesCache to be populated after snapshot")
	}
	firstCache := m.dashboardLinesCache

	// Non-mutating input messages should reuse the cached lines.
	m2 := update(m, tea.MouseMotionMsg{X: 10, Y: 10})
	if m2.dashboardLinesCache == nil {
		t.Error("MouseMotionMsg should not clear dashboardLinesCache")
	}
	m2 = update(m2, key('j'))
	if m2.dashboardLinesCache == nil {
		t.Error("KeyPressMsg should not clear dashboardLinesCache")
	}
	m2 = update(m2, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m2.dashboardLinesCache == nil {
		t.Error("MouseWheelMsg should not clear dashboardLinesCache")
	}
	m2 = update(m2, tea.MouseClickMsg{X: 1, Y: m.contentStartRow()})
	if m2.dashboardLinesCache == nil {
		t.Error("MouseClickMsg should not clear dashboardLinesCache")
	}
	if &m2.dashboardLinesCache[0] != &firstCache[0] || len(m2.dashboardLinesCache) != len(firstCache) {
		t.Errorf("cache should be reused for non-mutating messages")
	}

	// A new snapshot mutates the underlying data and invalidates the cache;
	// adjustViewport rebuilds it, so we assert the content actually changed.
	snap := m2.snap
	snap.InFlight = append(snap.InFlight, metrics.InFlightEntry{ID: 999, Method: "GET", Path: "/extra", Limited: true})
	m3 := update(m2, snap)
	if m3.dashboardLinesCache == nil {
		t.Fatal("expected dashboardLinesCache to be rebuilt after snapshot")
	}
	if len(m3.dashboardLinesCache) <= len(firstCache) {
		t.Errorf("snapshot should produce new content: got %d lines, want more than %d", len(m3.dashboardLinesCache), len(firstCache))
	}

	// A terminal resize also invalidates the cache; the Dashboard tab rebuilds
	// it in the same Update cycle. Use a sentinel to detect rebuild without
	// relying on slice backing-array identity.
	m3.dashboardLinesCache = []string{"SENTINEL"}
	m4 := update(m3, tea.WindowSizeMsg{Width: 80, Height: 24})
	if m4.dashboardLinesCache == nil {
		t.Fatal("expected dashboardLinesCache to be rebuilt after resize")
	}
	if len(m4.dashboardLinesCache) == 1 && m4.dashboardLinesCache[0] == "SENTINEL" {
		t.Error("WindowSizeMsg should invalidate and rebuild dashboardLinesCache")
	}

	// Switching tabs discards the old tab's cached content. On the Requests tab
	// adjustViewport never calls cachedDashboardLines, so the cache stays nil.
	m5 := update(m4, key('2'))
	if m5.dashboardLinesCache != nil {
		t.Error("switching tabs should clear dashboardLinesCache")
	}
}

// dashboardStatusLines extracts the Status section rows from rendered
// dashboard lines: the "  Status  " header row plus every immediately
// following non-empty row (the wrapped label rows, when the labels
// could not share the bar's line).
func dashboardStatusLines(lines []string) []string {
	var out []string
	for i, l := range lines {
		if !strings.HasPrefix(stripANSI(l), "  Status  ") {
			continue
		}
		out = append(out, l)
		for j := i + 1; j < len(lines) && lines[j] != ""; j++ {
			out = append(out, lines[j])
		}
	}
	return out
}

// summaryRows returns the metric rows of the Summary section: every
// dashboard line after the " Summary " section header (the last
// section rendered by dashboardLines).
func summaryRows(lines []string) []string {
	for i, l := range lines {
		if stripANSI(l) == " Summary " {
			return lines[i+1:]
		}
	}
	return nil
}

// inflightSectionRows returns the rows of the In-Flight Requests
// section: every dashboard line after the section header up to the
// next blank line (the summary line(s) plus the per-request entries).
func inflightSectionRows(lines []string) []string {
	for i, l := range lines {
		if stripANSI(l) == " In-Flight Requests " {
			var out []string
			for j := i + 1; j < len(lines) && lines[j] != ""; j++ {
				out = append(out, lines[j])
			}
			return out
		}
	}
	return nil
}

// TestDashboard_InFlightSummaryFitsViewport reproduces the In-Flight
// summary overflow: "  N in-flight: L limited, P
// passthrough" spans exactly 39 cells for single-digit values — the
// entire 40-column viewport — and exceeds it the moment any value
// reaches two digits, so renderContentWithScrollbar silently truncates
// "passthrough". The summary must fit at any magnitude: counts are
// abbreviated via formatCount and, when the composed line still cannot
// fit, the parts pack into viewport-fitting rows. The absurd-magnitude
// cases deliberately exercise each counter independently (a consistent
// snapshot with int64-max limited AND passthrough counts would need an
// unallocatable slice; the render math treats the three numbers
// independently, and TestDashboard_AllLinesFitViewport covers the
// consistent path).

func dashboardWithOverflow(conc, height, flights int) Model {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: conc}})
	m.width = 80
	m.height = height
	m.tab = tabDashboard
	m.snap.CircuitBreaker = &metrics.CBStats{State: "CLOSED"}
	for i := range flights {
		m.snap.InFlight = append(m.snap.InFlight, metrics.InFlightEntry{
			ID: uint64(i), Method: "POST", Path: "/v1/messages", Limited: true,
		})
	}
	return m
}
