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
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui"
)

// version is set via ldflags at build time (e.g. -ldflags -X main.version=1.2.3).
var version = "dev"

func main() {
	if err := run(); err != nil {
		// Write fatal errors straight to stderr. In TUI mode run() may have
		// redirected the global log writer into the on-screen buffer, so the
		// error must be emitted explicitly to remain visible to the operator.
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		// GNU convention: usage errors (unknown flag, malformed command line)
		// exit 2 so callers can distinguish "bad invocation" from "runtime
		// failure", matching the pre-sectioned binary where the flag package
		// itself exited 2 on unparseable arguments.
		if errors.Is(err, config.ErrUsage) {
			fmt.Fprintf(os.Stderr, "run with -h for usage\n")
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// buildProvider assembles one provider's proxy from its resolved configuration:
// the per-provider request journal, metrics collector and upstream transport, plus
// the proxy itself wired onto them.
//
// The journal is shared between retry body replay and the TUI's Network
// inspection panel. Its capacity scales inversely with the body limit so the
// default worst-case memory footprint stays roughly bounded (~512 MiB) regardless
// of how large -retry-max-body-mb is configured.
func buildProvider(p *config.Provider) (*proxy.Proxy, *metrics.Collector, *journal.Journal, error) {
	maxBody := int64(p.RetryMaxBodyMB) << 20
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

	met := metrics.NewCollector()
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: p.MaxIdleConnsPerHost(),
		IdleConnTimeout:     120 * time.Second,
		DisableKeepAlives:   p.DisableKeepAlives,
	}

	prx, err := proxy.New(
		proxy.WithUpstream(p.UpstreamURL()),
		proxy.WithMatcher(p.Matcher()),
		proxy.WithLimiter(p.DefaultLimiter()),
		proxy.WithMetrics(met),
		proxy.WithQueueTimeout(p.QueueTimeout),
		proxy.WithGlobalLimiter(p.GlobalLimiter()),
		proxy.WithRouteLimiters(p.RouteLimiters()),
		proxy.WithMaxRetries(p.RetryMax),
		proxy.WithMaxBodyBytes(maxBody),
		proxy.WithRetryWaitMin(p.RetryWaitMin),
		proxy.WithRetryWaitMax(p.RetryWaitMax),
		proxy.WithRetryMinDelay(p.RetryMinDelay),
		proxy.WithRetrySkipOn429(p.RetrySkipOn429),
		proxy.WithCancelCooldown(p.CancelCooldown),
		proxy.WithFailureHold(p.FailureHold),
		proxy.WithAdaptiveHeadroom(p.AdaptiveHeadroom),
		proxy.WithAdaptiveHeadroomWindow(p.AdaptiveHeadroomWindow),
		proxy.WithLimitAll(p.LimitAll),
		proxy.WithTransport(transport),
		proxy.WithJournal(j),
		proxy.WithBreaker(p.Breaker()),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("proxy config: %w", err)
	}
	return prx, met, j, nil
}

// upstreamMaxIdleConnsPerHost returns the minimum number of idle connections
// the upstream transport should keep open per host. It is derived from the
// configured route/global concurrency limiters so that multi-route or grouped
// configurations do not thrash TCP connections after bursts, while still
// honoring the global concurrency cap and a safe default floor.
//
// limitAll indicates that every non-matching request is routed through the
// default limiter (the same one set by WithLimiter/-concurrency). In that mode
// the default pool always gates non-matching traffic, so its capacity must be
// counted toward the idle-connection pool regardless of whether any pattern
// itself falls through to it.
//
// This helper is pinned by upstreamMaxIdleConnsPerHost() in main_test.go and
// is kept alongside the config-provider's MaxIdleConnsPerHost derivation for
// cross-checking in the non-sectioned (legacy) construction path.
func upstreamMaxIdleConnsPerHost(globalConcurrency, concurrency int, patterns []route.Pattern, routeLimiters map[string]*queue.Limiter, limitAll bool) int {
	routePoolMax := 0
	for _, lim := range routeLimiters {
		routePoolMax += lim.Limit()
	}

	defaultPoolUsed := limitAll
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

// logProviderConfig prints the resolved startup configuration for one provider.
// For a single provider this reproduces the legacy startup log lines exactly.
func logProviderConfig(pr *config.Provider) {
	patterns := pr.Patterns()
	if len(pr.Limits) > 0 {
		var parts []string
		for _, pat := range patterns {
			parts = append(parts, pat.String())
		}
		log.Printf("limiting %d route(s) at concurrency %d: %s",
			len(patterns), pr.Concurrency, strings.Join(parts, ", "))
	} else {
		log.Printf("auto-detecting LLM endpoints (%d patterns) at concurrency %d",
			len(patterns), pr.Concurrency)
	}
	if pr.GlobalConcurrency > 0 {
		log.Printf("global concurrency limit: %d", pr.GlobalConcurrency)
	}
	if pr.RetryMax != 0 {
		if pr.RetryMax < 0 {
			log.Printf("retry: unlimited (backoff %s–%s)", pr.RetryWaitMin, pr.RetryWaitMax)
		} else {
			log.Printf("retry: max %d attempts (backoff %s–%s)", pr.RetryMax, pr.RetryWaitMin, pr.RetryWaitMax)
		}
	}
	if breaker := pr.Breaker(); breaker != nil {
		log.Printf("circuit breaker: threshold=%d window=%s open-timeout=%s penalty=%s max-penalty=%s",
			pr.CBThreshold, pr.CBWindow, pr.CBOpenTimeout, pr.CBPenalty, pr.CBMaxPenalty)
	}
	if pr.RetryMinDelay > 0 {
		log.Printf("retry min delay: %s", pr.RetryMinDelay)
	}
	if pr.RetrySkipOn429 {
		log.Printf("retry skip 429: enabled")
	}
	if pr.ReleaseCooldown > 0 {
		log.Printf("release cooldown: %s", pr.ReleaseCooldown)
	}
	if pr.CancelCooldown > 0 {
		log.Printf("cancel cooldown: %s", pr.CancelCooldown)
	}
	if pr.FailureHold > 0 {
		// Hyphen-bound on purpose: the Logs tab's actionable-line heuristic
		// toasts whole-word "failure". "failure-hold" is one identifier (like
		// open-timeout=10s), which the classifier ignores — a space-separated
		// "failure hold: 2s" reads like prose and raised a false toast on every
		// TUI start. Keep the summary hyphen-bound.
		log.Printf("failure-hold: %s", pr.FailureHold)
	}
	if pr.AdaptiveHeadroom {
		log.Printf("adaptive headroom: enabled (window %s)", pr.AdaptiveHeadroomWindow)
	}
}

// drainResetSignals empties the buffered reset channel so a burst of "Reset
// Stats" confirmations collapses into the single fleet-wide reset that
// follows it. It never blocks: the channel is buffered (cap 1) and the
// model's sends are non-blocking, so at most a few signals can pend.
func drainResetSignals(resetCh <-chan struct{}) {
	for {
		select {
		case <-resetCh:
			continue
		default:
			return
		}
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			// -h/-help at any scope: print the full sectioned usage to stdout
			// and exit 0, before any validation or logging setup.
			config.PrintUsage(os.Stdout)
			return nil
		}
		return err
	}

	if cfg.Server.Version {
		fmt.Println(version)
		return nil
	}

	// In TUI mode, capture output of the global stdlib logger and slog.Default()
	// into an on-screen bounded buffer instead of letting it degrade the terminal
	// dashboard. The buffer is created here — before ResolveAndValidate (whose
	// route-limiter construction emits group-conflict warnings) and before the
	// per-provider config summaries below — so even config warnings and startup
	// messages appear in the Logs tab. Direct
	// os.Stderr writes, standalone log.Logger/slog.Logger instances, and anything
	// emitted before this wiring are NOT captured. Fatal errors are written to
	// stderr explicitly by main() and are never swallowed by this redirect; once
	// the TUI exits the buffer is redirected back to stderr so shutdown logging
	// stays visible. Only TUI mode is affected; non-TUI runs keep normal stderr
	// logging.
	//
	// Order is load-bearing: slog.SetDefault rewires the standard logger
	// through its handler at INFO level, so installing log.SetOutput before it
	// would silently demote every stdlib line (including a config WARNING)
	// into structured level=INFO records that the Logs tab correctly
	// treats as non-actionable — suppressing their toast. Restoring the stdlib
	// writer afterwards keeps those lines in their timestamped identity, which
	// the actionable-keyword heuristics classify as intended.
	var logBuf *tui.LogBuffer
	if cfg.Server.TUI {
		logBuf = tui.NewLogBuffer(tui.LogBufferCapacity)
		slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
		log.SetFlags(log.LstdFlags)
		log.SetOutput(logBuf)
	}

	if err := cfg.ResolveAndValidate(); err != nil {
		return err
	}

	// Build a proxy for every provider and mount each at its prefix on the
	// shared dispatcher. With a single (legacy) bare-root provider this is a
	// transparent pass-through, so startup output is byte-identical to before.
	var (
		entries []router.Provider
		mets    []*metrics.Collector
		js      []*journal.Journal
	)
	for _, pr := range cfg.Providers {
		p, met, j, err := buildProvider(pr)
		if err != nil {
			return err
		}
		mets = append(mets, met)
		js = append(js, j)
		entries = append(entries, router.Provider{Name: pr.Name, Prefix: pr.Prefix, Proxy: p})
		logProviderConfig(pr)
	}

	h, err := router.New(entries)
	if err != nil {
		return err
	}

	srv := &http.Server{Addr: cfg.Server.Bind, Handler: h}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tuiProgram *tea.Program
	tuiDone := make(chan struct{})

	if cfg.Server.TUI {
		log.Println("TUI dashboard enabled")
		metas := make([]tui.ProviderMeta, len(cfg.Providers))
		for i, pr := range cfg.Providers {
			metas[i] = tui.ProviderMeta{
				Name:        pr.Name,
				Concurrency: pr.Concurrency,
				Journal:     js[i],
			}
		}
		updCh := make(chan tui.ProviderUpdate, 16)
		progCh := make(chan *tea.Program, 1)
		resetCh := make(chan struct{}, 1)
		go func() {
			tui.Run(updCh, metas, progCh, resetCh, logBuf)
			// The dashboard is gone and its poller with it: stream any further
			// logging (the shutdown sequence below) straight to stderr rather
			// than parking it in a buffer nobody drains.
			if logBuf != nil {
				logBuf.RedirectTo(os.Stderr)
			}
			close(tuiDone)
			stop() // trigger graceful shutdown when TUI exits
		}()
		tuiProgram = <-progCh
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			defer close(updCh) // unblocks the snapshot reader goroutine in tui.Run()
			for {
				select {
				case <-ctx.Done():
					return
				case <-resetCh:
					// "Reset Stats": drain any coalesced requests, then
					// zero every provider's cumulative counters (the
					// overlay promises "all cumulative counters", so the
					// reset is fleet-wide, not per-view).
					drainResetSignals(resetCh)
					for _, mc := range mets {
						mc.Reset()
					}
				case <-ticker.C:
					// Snapshot every provider's collector and merge that
					// provider's breaker stats (as before) before sending.
					for i, pr := range cfg.Providers {
						snap := mets[i].Snapshot()
						if breaker := pr.Breaker(); breaker != nil {
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
						case updCh <- tui.ProviderUpdate{Index: i, Snapshot: snap}:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}()
	}

	// Ensure the TUI exits cleanly and the terminal is restored, even on
	// fatal error paths (e.g. bind address in use). Kill() restores the
	// terminal internally, so no separate restore call is needed.
	//
	// On the error path the TUI is still running and nothing has told it to
	// quit, so we proactively stop() the context and send Quit(). Quit() is
	// fire-and-forget because it blocks on bubbletea's unbuffered message
	// channel; a synchronous call could deadlock if the event loop is
	// wedged, making the Kill() fallback below unreachable.
	//
	// We wait for tuiDone before Kill() to serialize the two teardown paths:
	// p.Run() performs its own shutdown() on return, and Kill() calls it
	// again. Both are guarded by sync.Once, so the second is a no-op, but
	// ordering them avoids any concurrent-teardown race. The 3-second
	// timeout is a last resort for a genuinely stuck program.
	defer func() {
		if tuiProgram != nil {
			stop()
			go tuiProgram.Quit()
			select {
			case <-tuiDone:
			case <-time.After(3 * time.Second):
			}
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
