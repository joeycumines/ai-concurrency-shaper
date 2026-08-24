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
	"charm.land/lipgloss/v2"
)

// tuiTheme bundles every color that the dashboard renders. Rather than a
// handful of package-global style vars, all styles resolve through the model's
// tuiTheme so a change of terminal background — the tea.BackgroundColorMsg the
// program receives from tea.RequestBackgroundColor — can retheme the whole UI
// in a single place.
//
// The dark palette is the default (and the one pinned by the unit tests, which
// never receive a background message); the light palette re-inks the SAME hue
// to state mapping with darker, high-contrast colors so the dashboard stays
// readable on a white or light terminal. Hue never changes meaning across
// palettes: green=ok, blue=info/limit, orange=warn, red=error.
type tuiTheme struct {
	headerStyle lipgloss.Style
	// chipActiveStyle / chipInactiveStyle render the provider switcher chips
	// in the header (multi-provider mode). They follow the same hue-to-state
	// contract as the tab bar: blue background = selected/active, muted =
	// inactive.
	chipActiveStyle        lipgloss.Style
	chipInactiveStyle      lipgloss.Style
	sectionStyle           lipgloss.Style
	dimStyle2              lipgloss.Style
	sepStyle               lipgloss.Style
	filterPromptStyle      lipgloss.Style
	tabActiveStyle         lipgloss.Style
	tabInactiveStyle       lipgloss.Style
	tableHeaderStyle       lipgloss.Style
	rowStyle               lipgloss.Style
	rowSelectedStyle       lipgloss.Style
	gaugeNormalStyle       lipgloss.Style
	gaugeWarnStyle         lipgloss.Style
	gaugeCriticalStyle     lipgloss.Style
	queueWarnStyle         lipgloss.Style
	sparklineStyle         lipgloss.Style
	gaugeEmptyStyle        lipgloss.Style
	statusInfoStyle        lipgloss.Style
	statusOkStyle          lipgloss.Style
	statusRedirectStyle    lipgloss.Style
	statusClientErrStyle   lipgloss.Style
	statusServerErrStyle   lipgloss.Style
	limitedTag             string
	passTag                string
	overlayStyle           lipgloss.Style
	footerStyle            lipgloss.Style
	circuitClosedStyle     lipgloss.Style
	circuitOpenStyle       lipgloss.Style
	circuitHalfOpenStyle   lipgloss.Style
	queueFillDefaultStyle  lipgloss.Style
	waterfallQueueStyle    lipgloss.Style
	waterfallTTFBStyle     lipgloss.Style
	waterfallDownloadStyle lipgloss.Style

	// scrollbarThumb/scrollbarTrack are applied to every per-tab scrollbar
	// when the theme is (re)applied.
	scrollbarThumb lipgloss.Style
	scrollbarTrack lipgloss.Style
}

// newTheme builds the style set for the given background. dark=true is the
// default (near-black backgrounds, Light-High-Contrast-style #E6EDF3 text on
// GitHub-Dark surfaces); dark=false targets white/light backgrounds with dark
// ink. See the tuiTheme doc for the hue contract that both palettes share.
func newTheme(dark bool) tuiTheme {
	if dark {
		return darkTheme()
	}
	return lightTheme()
}

// darkTheme returns the default near-black palette.
func darkTheme() tuiTheme {
	s := lipgloss.NewStyle
	t := tuiTheme{
		headerStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#E6EDF3")).
			Background(lipgloss.Color("#1F6FEB")).
			PaddingLeft(1).
			PaddingRight(1),

		sectionStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#58A6FF")),

		dimStyle2: s().
			Foreground(lipgloss.Color("#6E7681")),

		sepStyle: s().
			Foreground(lipgloss.Color("#30363D")),

		filterPromptStyle: s().
			Foreground(lipgloss.Color("#58A6FF")).
			Background(lipgloss.Color("#0D1117")).
			Bold(true),

		tabActiveStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#0D1117")).
			Background(lipgloss.Color("#58A6FF")).
			PaddingLeft(1).
			PaddingRight(1),

		tabInactiveStyle: s().
			Foreground(lipgloss.Color("#8B949E")).
			Background(lipgloss.Color("#161B22")).
			PaddingLeft(1).
			PaddingRight(1),

		chipActiveStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#0D1117")).
			Background(lipgloss.Color("#58A6FF")).
			PaddingLeft(1).
			PaddingRight(1),

		chipInactiveStyle: s().
			Foreground(lipgloss.Color("#8B949E")).
			Background(lipgloss.Color("#161B22")).
			PaddingLeft(1).
			PaddingRight(1),

		tableHeaderStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#58A6FF")),

		rowStyle: s().
			Foreground(lipgloss.Color("#E6EDF3")),

		rowSelectedStyle: s().
			Foreground(lipgloss.Color("#0D1117")).
			Background(lipgloss.Color("#388BFD")),

		gaugeNormalStyle: s().
			Foreground(lipgloss.Color("#58A6FF")).Bold(true),

		gaugeWarnStyle: s().
			Foreground(lipgloss.Color("#D29922")).Bold(true),

		gaugeCriticalStyle: s().
			Foreground(lipgloss.Color("#F85149")).Bold(true),

		queueWarnStyle: s().
			Foreground(lipgloss.Color("#F0883E")).Bold(true),

		sparklineStyle: s().
			Foreground(lipgloss.Color("#58A6FF")),

		gaugeEmptyStyle: s().
			Foreground(lipgloss.Color("#21262D")),

		statusInfoStyle: s().
			Foreground(lipgloss.Color("#8B949E")),

		statusOkStyle: s().
			Foreground(lipgloss.Color("#3FB950")),

		statusRedirectStyle: s().
			Foreground(lipgloss.Color("#58A6FF")),

		statusClientErrStyle: s().
			Foreground(lipgloss.Color("#F0883E")),

		statusServerErrStyle: s().
			Foreground(lipgloss.Color("#F85149")),

		limitedTag: s().
			Foreground(lipgloss.Color("#F0883E")).
			Render(" lim"),

		passTag: s().
			Foreground(lipgloss.Color("#6E7681")).
			Render(" pas"),

		overlayStyle: s().
			Foreground(lipgloss.Color("#E6EDF3")).
			Background(lipgloss.Color("#161B22")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#58A6FF")).
			Padding(1, 2).
			MarginLeft(2),

		footerStyle: s().
			Foreground(lipgloss.Color("#8B949E")).
			Background(lipgloss.Color("#0D1117")),

		circuitClosedStyle: s().
			Foreground(lipgloss.Color("#3FB950")),

		circuitOpenStyle: s().
			Foreground(lipgloss.Color("#F85149")).
			Bold(true),

		circuitHalfOpenStyle: s().
			Foreground(lipgloss.Color("#F0883E")),

		queueFillDefaultStyle: s().
			Foreground(lipgloss.Color("#39D353")).Bold(true),

		waterfallQueueStyle: s().
			Foreground(lipgloss.Color("#D29922")),

		waterfallTTFBStyle: s().
			Foreground(lipgloss.Color("#58A6FF")),

		waterfallDownloadStyle: s().
			Foreground(lipgloss.Color("#3FB950")),

		scrollbarThumb: s().
			Foreground(lipgloss.Color("#58A6FF")),

		scrollbarTrack: s().
			Foreground(lipgloss.Color("#21262D")),
	}
	return t
}

// lightTheme returns the palette for a white or light terminal background.
// Every ink carries at least ~4.5:1 contrast against white and leaves the
// hue-to-state meaning of the dark palette intact.
func lightTheme() tuiTheme {
	s := lipgloss.NewStyle
	t := tuiTheme{
		headerStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#0969DA")).
			PaddingLeft(1).
			PaddingRight(1),

		sectionStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#0550AE")),

		dimStyle2: s().
			Foreground(lipgloss.Color("#59636E")),

		sepStyle: s().
			Foreground(lipgloss.Color("#C9D4DE")),

		filterPromptStyle: s().
			Foreground(lipgloss.Color("#0550AE")).
			Background(lipgloss.Color("#EFF3F6")).
			Bold(true),

		tabActiveStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#0969DA")).
			PaddingLeft(1).
			PaddingRight(1),

		tabInactiveStyle: s().
			Foreground(lipgloss.Color("#57606A")).
			Background(lipgloss.Color("#EAF1F6")).
			PaddingLeft(1).
			PaddingRight(1),

		chipActiveStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#0969DA")).
			PaddingLeft(1).
			PaddingRight(1),

		chipInactiveStyle: s().
			Foreground(lipgloss.Color("#57606A")).
			Background(lipgloss.Color("#EAF1F6")).
			PaddingLeft(1).
			PaddingRight(1),

		tableHeaderStyle: s().
			Bold(true).
			Foreground(lipgloss.Color("#0550AE")),

		rowStyle: s().
			Foreground(lipgloss.Color("#24292F")),

		rowSelectedStyle: s().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#0550AE")),

		gaugeNormalStyle: s().
			Foreground(lipgloss.Color("#0550AE")).Bold(true),

		gaugeWarnStyle: s().
			Foreground(lipgloss.Color("#9A6700")).Bold(true),

		gaugeCriticalStyle: s().
			Foreground(lipgloss.Color("#CF222E")).Bold(true),

		queueWarnStyle: s().
			Foreground(lipgloss.Color("#BC4C00")).Bold(true),

		sparklineStyle: s().
			Foreground(lipgloss.Color("#0550AE")),

		gaugeEmptyStyle: s().
			Foreground(lipgloss.Color("#D0D7DE")),

		statusInfoStyle: s().
			Foreground(lipgloss.Color("#59636E")),

		statusOkStyle: s().
			Foreground(lipgloss.Color("#1A7F37")),

		statusRedirectStyle: s().
			Foreground(lipgloss.Color("#0550AE")),

		statusClientErrStyle: s().
			Foreground(lipgloss.Color("#BC4C00")),

		statusServerErrStyle: s().
			Foreground(lipgloss.Color("#CF222E")),

		limitedTag: s().
			Foreground(lipgloss.Color("#BC4C00")).
			Render(" lim"),

		passTag: s().
			Foreground(lipgloss.Color("#59636E")).
			Render(" pas"),

		overlayStyle: s().
			Foreground(lipgloss.Color("#1F2328")).
			Background(lipgloss.Color("#FFFFFF")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#0550AE")).
			Padding(1, 2).
			MarginLeft(2),

		footerStyle: s().
			Foreground(lipgloss.Color("#57606A")).
			Background(lipgloss.Color("#EAF1F6")),

		circuitClosedStyle: s().
			Foreground(lipgloss.Color("#1A7F37")),

		circuitOpenStyle: s().
			Foreground(lipgloss.Color("#CF222E")).
			Bold(true),

		circuitHalfOpenStyle: s().
			Foreground(lipgloss.Color("#BC4C00")),

		queueFillDefaultStyle: s().
			Foreground(lipgloss.Color("#1A7F37")).Bold(true),

		waterfallQueueStyle: s().
			Foreground(lipgloss.Color("#9A6700")),

		waterfallTTFBStyle: s().
			Foreground(lipgloss.Color("#0550AE")),

		waterfallDownloadStyle: s().
			Foreground(lipgloss.Color("#1A7F37")),

		scrollbarThumb: s().
			Foreground(lipgloss.Color("#0969DA")),

		scrollbarTrack: s().
			Foreground(lipgloss.Color("#D0D7DE")),
	}
	return t
}
