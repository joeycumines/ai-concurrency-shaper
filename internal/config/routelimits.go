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

package config

import (
	"fmt"
	"log"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// This file owns the -limit pattern resolution: parsing the provider's
// patterns and constructing the per-route/per-group limiter set.

// parsePatterns parses -limit patterns, falling back to the built-in defaults
// when none were configured.
func (p *Provider) parsePatterns() ([]route.Pattern, error) {
	if len(p.Limits) == 0 {
		return route.DefaultPatterns(), nil
	}
	out := make([]route.Pattern, 0, len(p.Limits))
	for _, s := range p.Limits {
		pattern, err := route.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid -limit %q: %w", s, err)
		}
		out = append(out, pattern)
	}
	return out, nil
}

// routeLimiters builds the per-route/per-group limiters exactly as the legacy
// wiring in main.go does: one limiter per distinct -limit pattern (or group),
// keyed by the pattern's group name when present and by its raw string otherwise.
// Patterns with Limit == 0 fall through to the default pool and get no limiter.
//
// validateBasic already parsed every entry of limitSpecs, so route.Parse cannot
// fail here. A group-conflict warning mirrors the legacy behavior (the first
// limiter wins).
func routeLimiters(limitSpecs []string, releaseCooldown time.Duration) map[string]*queue.Limiter {
	limiters := make(map[string]*queue.Limiter)

	for _, s := range limitSpecs {
		pattern, _ := route.Parse(s) // validated by validateBasic
		if pattern.Unlimited {
			continue // exempt class: no limiter, never acquires a slot
		}
		if pattern.Limit == 0 {
			continue // falls through to the default pool
		}
		key := pattern.Raw
		if pattern.Group != "" {
			key = pattern.Group
		}
		if existing, ok := limiters[key]; ok {
			// Group conflict warning: legacy behavior keeps the first limiter.
			if existing.Limit() != pattern.Limit {
				log.Printf("WARNING: route %q specifies group %q with limit %d, but group already has limit %d. Using %d.\n", s, pattern.Group, pattern.Limit, existing.Limit(), existing.Limit())
			}
			continue
		}
		limiters[key] = queue.NewLimiterWithCooldown(pattern.Limit, releaseCooldown)
	}
	return limiters
}
