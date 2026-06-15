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

// Package toast provides short-lived notification messages for the TUI dashboard.
package toast

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Toast represents a single notification to display at the bottom of a pane.
type Toast struct {
	Message   string
	Style     lipgloss.Style
	Width     int
	CreatedAt time.Time
	Duration  time.Duration // 0 = until explicitly dismissed
}

// Show initialises the CreatedAt timestamp and returns the toast.
func (t *Toast) Show() *Toast {
	t.CreatedAt = time.Now()
	return t
}

// Expired reports whether the toast has exceeded its display duration.
func (t *Toast) Expired() bool {
	if t.Duration <= 0 {
		return false
	}
	return time.Since(t.CreatedAt) > t.Duration
}

// defaultStyle returns the default toast style (blue background, white text).
func defaultStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#E6EDF3")).
		Background(lipgloss.Color("#1F6FEB")).
		PaddingLeft(1).
		PaddingRight(1).
		Bold(true)
}

// hasStyle reports whether the toast has a non-default style set.
// It checks by seeing if the style renders differently than the zero style.
func hasStyle(s lipgloss.Style) bool {
	// A rendered zero-value style still produces ANSI reset codes.
	// We detect "no custom style" by comparing the rendered empty string.
	return s.Render("") != lipgloss.NewStyle().Render("")
}

// Render produces a toast string positioned at the bottom of the given
// bounds (width, height). Returns empty string for zero/negative bounds or
// empty message.
func (t *Toast) Render(width, height int) string {
	if width <= 0 || height <= 0 || t.Message == "" {
		return ""
	}

	w := width
	if t.Width > 0 && t.Width < w {
		w = t.Width
	}

	style := t.Style
	if !hasStyle(style) {
		style = defaultStyle()
	}

	rendered := style.MaxWidth(w).Render(t.Message)

	// Position at the bottom of available height.
	if height > 1 {
		return strings.Repeat("\n", height-1) + rendered
	}
	return rendered
}

// VisibleToasts filters the input slice, removing expired entries and
// returning only toasts that should still be displayed.
func VisibleToasts(toasts []*Toast) []*Toast {
	live := make([]*Toast, 0, len(toasts))
	for _, t := range toasts {
		if !t.Expired() {
			live = append(live, t)
		}
	}
	return live
}
