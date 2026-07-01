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

// Command ai-concurrency-shaper is a stealth reverse proxy with bounded
// concurrency for configured routes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui"
)

// version is set via ldflags at build time (e.g. -ldflags -X main.version=1.2.3).
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run() error {
	var (
		bindAddr          string
		upstreamURL       string
		limitList         limitFlags
		concurrency       int
		globalConcurrency int
		queueTimeout      time.Duration
		useTUI            bool
		retryMax          int
		retryMaxBodyMB    int64
		showVersion       bool

		// Circuit breaker flags.
		cbEnabled     bool
		cbThreshold   int
		cbWindow      time.Duration
		cbOpenTimeout time.Duration
		cbMaxOpen     time.Duration
		cbPenalty     time.Duration
		cbMaxPenalty  time.Duration

		// Enhanced retry flags.
		retryWaitMin   time.Duration
		retryWaitMax   time.Duration
		retryMinDelay  time.Duration
		retrySkipOn429 bool

		// Concurrency protection flags.
		releaseCooldown time.Duration
		cancelCooldown  time.Duration
		failureHold     time.Duration

		// Adaptive headroom.
		adaptiveHeadroom       bool
		adaptiveHeadroomWindow time.Duration

		// Transport tuning.
		upstreamDisableKeepAlives bool
	)

	flag.StringVar(&bindAddr, "bind", ":8080", "listen address")
	flag.StringVar(&upstreamURL, "upstream", "", "upstream base URL (required)")
	flag.Var(&limitList, "limit", "route pattern to limit (repeatable)")
	flag.IntVar(&concurrency, "concurrency", 4, "max concurrent limited requests")
	flag.IntVar(&globalConcurrency, "global-concurrency", 0, "global concurrency limit (0 = disabled)")
	flag.DurationVar(&queueTimeout, "queue-timeout", 30*time.Second, "max wait for a concurrency slot (0 = use request context)")
	flag.IntVar(&retryMax, "retry", -1, "max retry attempts (-1 = unlimited, 0 = disabled)")
	flag.Int64Var(&retryMaxBodyMB, "retry-max-body-mb", 5, "max request body size (MB) eligible for retry")
	flag.BoolVar(&useTUI, "tui", false, "enable terminal dashboard")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	// Circuit breaker.
	flag.BoolVar(&cbEnabled, "circuit-breaker", true, "enable circuit breaker (default: true)")
	flag.IntVar(&cbThreshold, "cb-threshold", 5, "failures within window to trip circuit breaker")
	flag.DurationVar(&cbWindow, "cb-window", 30*time.Second, "circuit breaker failure counting window")
	flag.DurationVar(&cbOpenTimeout, "cb-open-timeout", 10*time.Second, "time before circuit breaker probes (half-open)")
	flag.DurationVar(&cbMaxOpen, "cb-max-open-timeout", 120*time.Second, "max circuit breaker open timeout after backoff")
	flag.DurationVar(&cbPenalty, "cb-penalty", 2*time.Second, "base phantom concurrency hold time")
	flag.DurationVar(&cbMaxPenalty, "cb-max-penalty", 60*time.Second, "max phantom concurrency hold time")

	// Enhanced retry.
	flag.DurationVar(&retryWaitMin, "retry-wait-min", 500*time.Millisecond, "minimum retry wait")
	flag.DurationVar(&retryWaitMax, "retry-wait-max", 30*time.Second, "maximum retry wait")
	flag.DurationVar(&retryMinDelay, "retry-min-delay", 1*time.Second, "minimum delay before retrying (0 = use backoff only)")
	flag.BoolVar(&retrySkipOn429, "retry-skip-429", true, "skip retrying 429 responses to prevent concurrency amplification")
	flag.DurationVar(&releaseCooldown, "release-cooldown", 200*time.Millisecond, "delay after slot release before re-admission (0 = immediate)")
	flag.DurationVar(&cancelCooldown, "cancel-cooldown", 200*time.Millisecond, "hold slot after client cancel once an upstream attempt started (0 = immediate)")
	flag.DurationVar(&failureHold, "failure-hold", 2*time.Second, "hold slot after upstream failure even without circuit breaker (0 = disabled)")
	flag.BoolVar(&adaptiveHeadroom, "adaptive-headroom", false, "reduce effective concurrency by one slot after a 429, restoring after a quiet window")
	flag.DurationVar(&adaptiveHeadroomWindow, "adaptive-headroom-window", 30*time.Second, "duration to hold the one-slot 429 headroom")
	flag.BoolVar(&upstreamDisableKeepAlives, "upstream-disable-keep-alives", false, "disable HTTP keep-alives to upstream; avoids provider-side connection-count concurrency violations")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ai-concurrency-shaper %s\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: ai-concurrency-shaper [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return nil
	}

	if upstreamURL == "" {
		return fmt.Errorf("-upstream is required")
	}

	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		return fmt.Errorf("invalid -upstream URL: %w", err)
	}
	if upstream.Scheme == "" {
		return fmt.Errorf("-upstream URL must include scheme (http or https)")
	}

	var patterns []route.Pattern
	routeLimiters := make(map[string]*queue.Limiter)

	if len(limitList) > 0 {
		for _, s := range limitList {
			p, err := route.Parse(s)
			if err != nil {
				return fmt.Errorf("invalid -limit %q: %w", s, err)
			}
			patterns = append(patterns, p)
			if p.Limit > 0 {
				if p.Group != "" {
					// Routes in the same @group share one limiter.
					if existing, exists := routeLimiters[p.Group]; exists {
						if existing.Limit() != p.Limit {
							log.Printf("WARNING: route %q specifies group %q with limit %d, but group already has limit %d. Using %d.",
								p.Raw, p.Group, p.Limit, existing.Limit(), existing.Limit())
						}
					} else {
						routeLimiters[p.Group] = queue.NewLimiterWithCooldown(p.Limit, releaseCooldown)
					}
				} else {
					routeLimiters[p.Raw] = queue.NewLimiterWithCooldown(p.Limit, releaseCooldown)
				}
			}
		}
	} else {
		patterns = route.DefaultPatterns()
	}
	matcher := route.NewMatcher(patterns)

	met := metrics.NewCollector()
	limiter := queue.NewLimiterWithCooldown(concurrency, releaseCooldown)

	var globalLimiter *queue.Limiter
	if globalConcurrency > 0 {
		globalLimiter = queue.NewLimiterWithCooldown(globalConcurrency, releaseCooldown)
	}

	// Create the shared request journal. This is the single source of truth
	// for both retry body replay and the TUI's Network inspection panel.
	// We scale capacity inversely with the body limit so the default
	// worst-case memory footprint stays roughly bounded (~512 MiB)
	// regardless of how large retry-max-body-mb is configured.
	maxBody := int64(retryMaxBodyMB) << 20
	journalCap := 512
	if maxBody > 0 {
		if c := int((512 << 20) / (maxBody * 2)); c < journalCap {
			if c < 1 {
				c = 1
			}
			journalCap = c
		}
	}
	j := journal.New(journalCap, maxBody)

	// Create the circuit breaker when enabled.
	var breaker *circuitbreaker.Breaker
	if cbEnabled {
		var err error
		breaker, err = circuitbreaker.New(
			circuitbreaker.WithFailureThreshold(cbThreshold),
			circuitbreaker.WithWindow(cbWindow),
			circuitbreaker.WithOpenTimeout(cbOpenTimeout),
			circuitbreaker.WithMaxOpenTimeout(cbMaxOpen),
			circuitbreaker.WithBasePenalty(cbPenalty),
			circuitbreaker.WithMaxPenalty(cbMaxPenalty),
		)
		if err != nil {
			return fmt.Errorf("circuit breaker config: %w", err)
		}
	}

	effectiveMaxConcurrency := upstreamMaxIdleConnsPerHost(globalConcurrency, concurrency, patterns, routeLimiters)
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: effectiveMaxConcurrency,
		IdleConnTimeout:     120 * time.Second,
		DisableKeepAlives:   upstreamDisableKeepAlives,
	}

	p, err := proxy.New(
		proxy.WithUpstream(upstream),
		proxy.WithMatcher(matcher),
		proxy.WithLimiter(limiter),
		proxy.WithMetrics(met),
		proxy.WithQueueTimeout(queueTimeout),
		proxy.WithGlobalLimiter(globalLimiter),
		proxy.WithRouteLimiters(routeLimiters),
		proxy.WithMaxRetries(retryMax),
		proxy.WithMaxBodyBytes(int64(retryMaxBodyMB)<<20),
		proxy.WithRetryWaitMin(retryWaitMin),
		proxy.WithRetryWaitMax(retryWaitMax),
		proxy.WithRetryMinDelay(retryMinDelay),
		proxy.WithRetrySkipOn429(retrySkipOn429),
		proxy.WithCancelCooldown(cancelCooldown),
		proxy.WithFailureHold(failureHold),
		proxy.WithAdaptiveHeadroom(adaptiveHeadroom),
		proxy.WithAdaptiveHeadroomWindow(adaptiveHeadroomWindow),
		proxy.WithTransport(transport),
		proxy.WithJournal(j),
		proxy.WithBreaker(breaker),
	)
	if err != nil {
		return fmt.Errorf("proxy config: %w", err)
	}

	if len(limitList) > 0 {
		var parts []string
		for _, p := range patterns {
			parts = append(parts, p.String())
		}
		log.Printf("limiting %d route(s) at concurrency %d: %s",
			len(patterns), concurrency, strings.Join(parts, ", "))
	} else {
		log.Printf("auto-detecting LLM endpoints (%d patterns) at concurrency %d",
			len(patterns), concurrency)
	}
	if globalConcurrency > 0 {
		log.Printf("global concurrency limit: %d", globalConcurrency)
	}
	if retryMax != 0 {
		if retryMax < 0 {
			log.Printf("retry: unlimited (backoff %s–%s)", retryWaitMin, retryWaitMax)
		} else {
			log.Printf("retry: max %d attempts (backoff %s–%s)", retryMax, retryWaitMin, retryWaitMax)
		}
	}
	if breaker != nil {
		log.Printf("circuit breaker: threshold=%d window=%s open-timeout=%s penalty=%s max-penalty=%s",
			cbThreshold, cbWindow, cbOpenTimeout, cbPenalty, cbMaxPenalty)
	}
	if retryMinDelay > 0 {
		log.Printf("retry min delay: %s", retryMinDelay)
	}
	if retrySkipOn429 {
		log.Printf("retry skip 429: enabled")
	}
	if releaseCooldown > 0 {
		log.Printf("release cooldown: %s", releaseCooldown)
	}
	if cancelCooldown > 0 {
		log.Printf("cancel cooldown: %s", cancelCooldown)
	}
	if failureHold > 0 {
		log.Printf("failure hold: %s", failureHold)
	}
	if adaptiveHeadroom {
		log.Printf("adaptive headroom: enabled (window %s)", adaptiveHeadroomWindow)
	}

	srv := &http.Server{Addr: bindAddr, Handler: p}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tuiProgram *tea.Program

	if useTUI {
		log.Println("TUI dashboard enabled")
		snapCh := make(chan metrics.Snapshot, 16)
		progCh := make(chan *tea.Program, 1)
		go func() {
			tui.Run(snapCh, concurrency, j, progCh)
			stop() // trigger graceful shutdown when TUI exits
		}()
		tuiProgram = <-progCh
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			defer close(snapCh) // unblocks the snapshot reader goroutine in tui.Run()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					snap := met.Snapshot()
					if breaker != nil {
						s := breaker.Stats()
						snap.CircuitBreaker = &metrics.CBStats{
							State:               s.State.String(),
							Failures:            s.Failures,
							ConsecutiveFailures: s.ConsecutiveFailures,
							TotalFailures:       s.TotalFailures,
							TotalSuccesses:      s.TotalSuccesses,
							CurrentPenalty:      s.CurrentPenalty,
							NextRetry:           s.NextRetry,
						}
					}
					select {
					case snapCh <- snap:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	// Ensure the TUI exits cleanly and the terminal is restored, even on
	// fatal error paths (e.g. bind address in use). Kill restores the
	// former terminal state internally, so no separate restore call is
	// needed. Without this deferred cleanup a server-startup failure
	// would leave the terminal in raw + alt-screen mode.
	defer func() {
		if tuiProgram != nil {
			tuiProgram.Kill()
		}
	}()

	select {
	case <-ctx.Done():
		log.Println("shutting down...")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		return nil
	case err := <-errCh:
		return err
	}
}

// upstreamMaxIdleConnsPerHost returns the minimum number of idle connections
// the upstream transport should keep open per host. It is derived from the
// configured route/global concurrency limiters so that multi-route or grouped
// configurations do not thrash TCP connections after bursts, while still
// honoring the global concurrency cap and a safe default floor.
func upstreamMaxIdleConnsPerHost(globalConcurrency, concurrency int, patterns []route.Pattern, routeLimiters map[string]*queue.Limiter) int {
	routePoolMax := 0
	for _, lim := range routeLimiters {
		routePoolMax += lim.Limit()
	}

	defaultPoolUsed := false
	for _, p := range patterns {
		key := p.Group
		if key == "" {
			key = p.Raw
		}
		if p.Limit == 0 {
			if p.Group == "" {
				defaultPoolUsed = true
				continue
			}
			if _, ok := routeLimiters[key]; !ok {
				defaultPoolUsed = true
			}
		}
	}
	if defaultPoolUsed {
		routePoolMax += concurrency
	}
	if globalConcurrency > 0 && routePoolMax > globalConcurrency {
		routePoolMax = globalConcurrency
	}
	if routePoolMax < 20 {
		return 20
	}
	return routePoolMax
}

// limitFlags implements flag.Value for repeatable -limit flags.
type limitFlags []string

func (f limitFlags) String() string { return strings.Join(f, ", ") }
func (f *limitFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}
