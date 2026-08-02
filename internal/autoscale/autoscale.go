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

// Package autoscale implements an AIMD (Additive Increase, Multiplicative
// Decrease) dynamic concurrency controller for a reverse proxy that bounds
// downstream concurrency.
//
// The Autoscaler wraps a [queue.Limiter] and adjusts the effective
// concurrency limit in response to observed response status codes. On each
// successful request the target limit is incremented by a fixed step
// (additive increase), subject to a configurable cooldown and a hard
// ceiling (max). On each rate-limit signal — 429 Too Many Requests, a
// confirmed 403 ban, or optionally any 5xx — the target is multiplied by a
// decrease ratio (multiplicative decrease), subject to a hard floor (min).
//
// The controller manipulates the limiter by withholding and restoring
// slots. At construction time, SetMaxWithheld is called with (max - min) so
// the limiter allows the full adjustment range, and (max - initial) slots
// are withheld so the starting effective limit equals initial. On increase,
// RestoreSlot is called once per step; on decrease, WithholdSlot is called
// once per unit of reduction.
//
// The Retry-After header is accepted but deliberately NOT used to scale the
// magnitude of a decrease. The ratio alone governs how aggressively the
// limit is cut. This prevents a malicious or buggy upstream from collapsing
// the concurrency limit to the floor by sending enormous Retry-After values.
package autoscale

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
)

// --- Option Interface ---

// Option configures an Autoscaler.
type Option interface {
	applyAutoscaleOption(cfg *autoscaleConfig) error
}

// --- Unexported Config Struct ---

type autoscaleConfig struct {
	limiter          *queue.Limiter
	min              int
	max              int
	initial          int
	increaseStep     int
	decreaseRatio    float64
	increaseCooldown time.Duration
	decreaseOn5xx    bool
}

func (c *autoscaleConfig) applyDefaults() {
	if c.min <= 0 {
		c.min = 1
	}
	if c.increaseStep <= 0 {
		c.increaseStep = 1
	}
	if c.decreaseRatio <= 0 {
		c.decreaseRatio = 0.5
	}
	if c.increaseCooldown < 0 {
		c.increaseCooldown = 0
	}
	// decreaseOn5xx defaults to false (zero value) — no default needed.
	if c.max <= 0 {
		c.max = 1
	}
	if c.initial <= 0 {
		c.initial = c.max
	}
}

// --- Concrete Options ---

// LimiterOption sets the limiter that the autoscaler will control.
type LimiterOption struct {
	value *queue.Limiter
}

// WithLimiter returns an option that sets the limiter. Must be non-nil.
func WithLimiter(l *queue.Limiter) *LimiterOption {
	return &LimiterOption{value: l}
}

func (o *LimiterOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value == nil {
		return errors.New("autoscale: limiter is required")
	}
	cfg.limiter = o.value
	return nil
}

// MinOption sets the minimum concurrency limit (the floor for decreases).
type MinOption struct {
	value int
}

// WithMin returns an option that sets the minimum limit. Must be >= 1.
func WithMin(n int) *MinOption {
	return &MinOption{value: n}
}

func (o *MinOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value < 1 {
		return fmt.Errorf("autoscale: min must be >= 1, got %d", o.value)
	}
	cfg.min = o.value
	return nil
}

// MaxOption sets the maximum concurrency limit (the ceiling for increases).
type MaxOption struct {
	value int
}

// WithMax returns an option that sets the maximum limit. Must be >= 1.
func WithMax(n int) *MaxOption {
	return &MaxOption{value: n}
}

func (o *MaxOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value < 1 {
		return fmt.Errorf("autoscale: max must be >= 1, got %d", o.value)
	}
	cfg.max = o.value
	return nil
}

// InitialOption sets the starting concurrency limit.
type InitialOption struct {
	value int
}

// WithInitial returns an option that sets the initial limit. Must be >= 1.
func WithInitial(n int) *InitialOption {
	return &InitialOption{value: n}
}

func (o *InitialOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value < 1 {
		return fmt.Errorf("autoscale: initial must be >= 1, got %d", o.value)
	}
	cfg.initial = o.value
	return nil
}

// IncreaseStepOption sets the additive increase step applied on each success.
type IncreaseStepOption struct {
	value int
}

// WithIncreaseStep returns an option that sets the increase step. Must be >= 1.
func WithIncreaseStep(n int) *IncreaseStepOption {
	return &IncreaseStepOption{value: n}
}

func (o *IncreaseStepOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value < 1 {
		return fmt.Errorf("autoscale: increase step must be >= 1, got %d", o.value)
	}
	cfg.increaseStep = o.value
	return nil
}

// DecreaseRatioOption sets the multiplicative decrease ratio. On each
// qualifying failure the target is set to floor(target * ratio), subject to
// the minimum.
type DecreaseRatioOption struct {
	value float64
}

// WithDecreaseRatio returns an option that sets the decrease ratio. Must be
// > 0 and <= 1.
func WithDecreaseRatio(r float64) *DecreaseRatioOption {
	return &DecreaseRatioOption{value: r}
}

func (o *DecreaseRatioOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value <= 0 || o.value > 1 {
		return fmt.Errorf("autoscale: decrease ratio must be > 0 and <= 1, got %v", o.value)
	}
	cfg.decreaseRatio = o.value
	return nil
}

// IncreaseCooldownOption sets the minimum duration between successive
// increases. A value of 0 means no cooldown.
type IncreaseCooldownOption struct {
	value time.Duration
}

// WithIncreaseCooldown returns an option that sets the increase cooldown.
// Must be >= 0.
func WithIncreaseCooldown(d time.Duration) *IncreaseCooldownOption {
	return &IncreaseCooldownOption{value: d}
}

func (o *IncreaseCooldownOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	if o.value < 0 {
		return fmt.Errorf("autoscale: increase cooldown must be >= 0, got %v", o.value)
	}
	cfg.increaseCooldown = o.value
	return nil
}

// DecreaseOn5xxOption controls whether 5xx status codes trigger a
// multiplicative decrease. By default they are ignored.
type DecreaseOn5xxOption struct {
	value bool
}

// WithDecreaseOn5xx returns an option that enables or disables 5xx-triggered
// decreases.
func WithDecreaseOn5xx(b bool) *DecreaseOn5xxOption {
	return &DecreaseOn5xxOption{value: b}
}

func (o *DecreaseOn5xxOption) applyAutoscaleOption(cfg *autoscaleConfig) error {
	cfg.decreaseOn5xx = o.value
	return nil
}

// --- Compile-Time Compliance Checks ---

var (
	_ Option = (*LimiterOption)(nil)
	_ Option = (*MinOption)(nil)
	_ Option = (*MaxOption)(nil)
	_ Option = (*InitialOption)(nil)
	_ Option = (*IncreaseStepOption)(nil)
	_ Option = (*DecreaseRatioOption)(nil)
	_ Option = (*IncreaseCooldownOption)(nil)
	_ Option = (*DecreaseOn5xxOption)(nil)
)

// --- Factory ---

// Autoscaler is an AIMD dynamic concurrency controller. All methods are
// safe for concurrent use.
type Autoscaler struct {
	limiter *queue.Limiter
	cfg     autoscaleConfig

	mu           sync.Mutex
	target       int
	lastIncrease time.Time

	// Stats (atomic for lock-free reads).
	totalSuccesses atomic.Int64
	total429s      atomic.Int64
	total5xxs      atomic.Int64
	total403Bans   atomic.Int64
	totalIncreases atomic.Int64
	totalDecreases atomic.Int64
}

// New creates an Autoscaler with the given options. Zero-valued fields
// receive sensible defaults. Returns an error if any option validation
// fails or if the cross-field constraints are not met (e.g. max < min).
func New(opts ...Option) (*Autoscaler, error) {
	cfg := autoscaleConfig{}
	for _, o := range opts {
		if err := o.applyAutoscaleOption(&cfg); err != nil {
			return nil, err
		}
	}
	cfg.applyDefaults()

	// Cross-field validation.
	if cfg.limiter == nil {
		return nil, errors.New("autoscale: limiter is required")
	}
	if cfg.min < 1 {
		return nil, fmt.Errorf("autoscale: min must be >= 1, got %d", cfg.min)
	}
	if cfg.max < cfg.min {
		return nil, fmt.Errorf("autoscale: max (%d) must be >= min (%d)", cfg.max, cfg.min)
	}
	if cfg.initial < cfg.min || cfg.initial > cfg.max {
		return nil, fmt.Errorf("autoscale: initial (%d) must be in [%d, %d]", cfg.initial, cfg.min, cfg.max)
	}
	if cfg.increaseStep < 1 {
		return nil, fmt.Errorf("autoscale: increase step must be >= 1, got %d", cfg.increaseStep)
	}
	if cfg.decreaseRatio <= 0 || cfg.decreaseRatio > 1 {
		return nil, fmt.Errorf("autoscale: decrease ratio must be > 0 and <= 1, got %v", cfg.decreaseRatio)
	}
	if cfg.increaseCooldown < 0 {
		return nil, fmt.Errorf("autoscale: increase cooldown must be >= 0, got %v", cfg.increaseCooldown)
	}

	// Configure the limiter to allow the full adjustment range.
	cfg.limiter.SetMaxWithheld(cfg.max - cfg.min)

	// Withhold down to the initial limit.
	for range cfg.max - cfg.initial {
		cfg.limiter.WithholdSlot()
	}

	return &Autoscaler{
		limiter: cfg.limiter,
		cfg:     cfg,
		target:  cfg.initial,
	}, nil
}

// OnSuccess records a successful request and performs an additive increase
// if the cooldown has elapsed and the target is below max.
func (a *Autoscaler) OnSuccess() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.totalSuccesses.Add(1)

	if a.target >= a.cfg.max {
		return
	}
	if a.cfg.increaseCooldown > 0 && time.Since(a.lastIncrease) < a.cfg.increaseCooldown {
		return
	}

	newTarget := min(a.target+a.cfg.increaseStep, a.cfg.max)

	// Restore one slot per unit of increase.
	for range newTarget - a.target {
		a.limiter.RestoreSlot()
	}

	a.target = newTarget
	a.lastIncrease = time.Now()
	a.totalIncreases.Add(1)
}

// OnFailure records a failure and performs a multiplicative decrease if the
// status code qualifies.
//
// statusCode is the HTTP status code (0 for transport errors). retryAfter
// is the Retry-After duration from the response header (0 if absent). It is
// accepted but deliberately NOT used to scale the decrease — the ratio alone
// governs the magnitude. This prevents a malicious or buggy upstream from
// collapsing the concurrency limit by sending enormous Retry-After values.
func (a *Autoscaler) OnFailure(statusCode int, retryAfter time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var isDecrease bool
	switch {
	case statusCode == 429:
		a.total429s.Add(1)
		isDecrease = true
	case statusCode == 403:
		a.total403Bans.Add(1)
		isDecrease = true
	case statusCode >= 500 && statusCode < 600:
		a.total5xxs.Add(1)
		isDecrease = a.cfg.decreaseOn5xx
	default:
		// Non-qualifying status — no counter change, no decrease.
		return
	}

	if !isDecrease {
		return
	}

	// retryAfter is intentionally ignored for magnitude. The decrease ratio
	// alone governs how aggressively the limit is cut. This prevents a
	// malicious upstream from collapsing the limit via large Retry-After.
	_ = retryAfter

	newTarget := max(int(math.Floor(float64(a.target)*a.cfg.decreaseRatio)), a.cfg.min)
	if newTarget >= a.target {
		// Already at or below the ratio result (e.g. target == min).
		return
	}

	for range a.target - newTarget {
		a.limiter.WithholdSlot()
	}

	a.target = newTarget
	a.totalDecreases.Add(1)
}

// Target returns the current target concurrency limit.
func (a *Autoscaler) Target() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.target
}

// Stats returns a point-in-time snapshot of the autoscaler's state.
func (a *Autoscaler) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()

	return Stats{
		Target:           a.target,
		Min:              a.cfg.min,
		Max:              a.cfg.max,
		IncreaseStep:     a.cfg.increaseStep,
		DecreaseRatio:    a.cfg.decreaseRatio,
		IncreaseCooldown: a.cfg.increaseCooldown,
		DecreaseOn5xx:    a.cfg.decreaseOn5xx,
		TotalSuccesses:   a.totalSuccesses.Load(),
		Total429s:        a.total429s.Load(),
		Total5xxs:        a.total5xxs.Load(),
		Total403Bans:     a.total403Bans.Load(),
		TotalIncreases:   a.totalIncreases.Load(),
		TotalDecreases:   a.totalDecreases.Load(),
	}
}

// Stats is a point-in-time snapshot of the autoscaler's state.
type Stats struct {
	Target           int
	Min              int
	Max              int
	IncreaseStep     int
	DecreaseRatio    float64
	IncreaseCooldown time.Duration
	DecreaseOn5xx    bool
	TotalSuccesses   int64
	Total429s        int64
	Total5xxs        int64
	Total403Bans     int64
	TotalIncreases   int64
	TotalDecreases   int64
}
