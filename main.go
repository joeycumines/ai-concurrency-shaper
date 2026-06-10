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
					if _, exists := routeLimiters[p.Group]; !exists {
						routeLimiters[p.Group] = queue.NewLimiter(p.Limit)
					}
				} else {
					routeLimiters[p.Raw] = queue.NewLimiter(p.Limit)
				}
			}
		}
	} else {
		patterns = route.DefaultPatterns()
	}
	matcher := route.NewMatcher(patterns)

	met := metrics.NewCollector()
	limiter := queue.NewLimiter(concurrency)

	var globalLimiter *queue.Limiter
	if globalConcurrency > 0 {
		globalLimiter = queue.NewLimiter(globalConcurrency)
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

	p := proxy.New(proxy.Config{
		Upstream:      upstream,
		Matcher:       matcher,
		Limiter:       limiter,
		Metrics:       met,
		QueueTimeout:  queueTimeout,
		GlobalLimiter: globalLimiter,
		RouteLimiters: routeLimiters,
		MaxRetries:    retryMax,
		MaxBodyBytes:  int64(retryMaxBodyMB) << 20,
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     120 * time.Second,
		},
		Journal: j,
	})

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
			log.Printf("retry: unlimited (backoff capped at 30s)")
		} else {
			log.Printf("retry: max %d attempts", retryMax)
		}
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
					snapCh <- met.Snapshot()
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

// limitFlags implements flag.Value for repeatable -limit flags.
type limitFlags []string

func (f limitFlags) String() string { return strings.Join(f, ", ") }
func (f *limitFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}
