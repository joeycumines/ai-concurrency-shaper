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
	"math"
	"net"
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
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/tui"
)

// version is set via ldflags at build time (e.g. -ldflags -X main.version=1.2.3).
var version = "dev"

// validateMBFlag rejects a negative or overflowing megabyte flag value
// before it is shifted to bytes (review-j finding 14): a negative or
// overflowing shift would otherwise produce a silently wrong limit. shift
// is the largest byte shift the value undergoes (e.g. 22 for
// transcode-max-request-mb, whose decoded-request limit is MB << 22).
func validateMBFlag(name string, value int64, shift uint) error {
	if value < 0 {
		return fmt.Errorf("%s must be nonnegative, got %d", name, value)
	}
	if shift >= 63 || value > math.MaxInt64>>shift {
		return fmt.Errorf("%s is too large to convert to bytes: %d MiB", name, value)
	}
	return nil
}

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
		limitAll          bool
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

		// Transcoding flags.
		transcodeRoutes            transcodeRouteFlags
		transcodeResponsesChat     bool
		transcodeMessagesChat      bool
		transcodeMessagesResponses bool
		transcodeAuth              string
		transcodeAuthSource        string
		transcodeAuthHeader        string
		transcodeAnthropicVersion  string
		transcodeModels            transcodeModelFlags
		transcodeMaxRequestMB      int64
		transcodeMaxResponseMB     int64
		transcodeAllowLoss         transcodeLossFlags
		transcodeCapabilities      transcodeCapabilityFlags
		transcodeClientQuery       transcodeClientQueryFlags
		transcodeStrictDefaults    bool
	)

	flag.StringVar(&bindAddr, "bind", ":8080", "listen address")
	flag.StringVar(&upstreamURL, "upstream", "", "upstream base URL (required)")
	flag.Var(&limitList, "limit", "route pattern to limit, matched by trailing path segments (repeatable)")
	flag.IntVar(&concurrency, "concurrency", 4, "max concurrent limited requests")
	flag.IntVar(&globalConcurrency, "global-concurrency", 0, "global concurrency limit (0 = disabled)")
	flag.BoolVar(&limitAll, "limit-all", false, "bound every request with the default limiter, not just matching limited routes")
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
	flag.Var(&transcodeRoutes, "transcode-route", "transcode route mapping clientProtocol@clientPath=upstreamProtocol@upstreamPath (repeatable); client protocols: responses, messages; upstream protocols: responses, chat-completions")
	flag.BoolVar(&transcodeResponsesChat, "transcode-responses-chat", false, "transcode /v1/responses to /v1/chat/completions (responses client, chat upstream)")
	flag.BoolVar(&transcodeMessagesChat, "transcode-messages-chat", false, "transcode /v1/messages to /v1/chat/completions (messages client, chat upstream)")
	flag.BoolVar(&transcodeMessagesResponses, "transcode-messages-responses", false, "transcode /v1/messages to /v1/responses (messages client, responses upstream)")
	// external-signer is intentionally absent: the CLI cannot supply a
	// signer, so the choice would only defer a startup failure (review-j
	// finding 14). The programmatic AuthExternalSigner mode remains for API
	// users who can provide one.
	flag.StringVar(&transcodeAuth, "transcode-auth", "auto", "upstream authentication mode: auto, none, bearer, x-api-key, api-key, header")
	flag.Var(&transcodeAllowLoss, "transcode-allow-loss", "granular loss features the transcoder may drop (repeatable, comma/space separated; prefix ! to withdraw a default); CLI mappings already approve the sensible default set (reasoning_summary, authenticated_thinking, mid_conversation_system, responses_controls, anthropic_controls, builtin_tools, usage_*), this flag adds more or removes defaults")
	flag.StringVar(&transcodeAuthSource, "transcode-auth-source", "inbound", "upstream secret source: inbound, env:NAME, file:PATH")
	flag.StringVar(&transcodeAuthHeader, "transcode-auth-header", "", "custom authentication header name (with -transcode-auth header)")
	flag.StringVar(&transcodeAnthropicVersion, "transcode-anthropic-version", "2023-06-01", "Anthropic-Version header value for Messages upstreams")
	flag.Var(&transcodeModels, "transcode-model", "client-model=upstream-model mapping (repeatable)")
	flag.Int64Var(&transcodeMaxRequestMB, "transcode-max-request-mb", 32, "max transcoded request body size (MB)")
	flag.Int64Var(&transcodeMaxResponseMB, "transcode-max-response-mb", 32, "max transcoded response body size (MB)")
	flag.Var(&transcodeCapabilities, "transcode-chat-capability", "chat-provider capability the transcoder may use (repeatable, comma/space separated; prefix ! to withdraw a default): developer_role, image_input, structured_outputs, parallel_tool_calls, stop_sequences, reasoning_effort, provider_reasoning_text, system_anywhere; the presets already enable the standard modern surface by default")
	flag.Var(&transcodeClientQuery, "transcode-allow-client-query", "client query parameter to forward on transcoded routes (repeatable, comma/space separated; prefix ! to withdraw a default); all CLI mappings allow beta by default")
	flag.BoolVar(&transcodeStrictDefaults, "transcode-strict-defaults", false, "start CLI transcode mappings from zero instead of the sensible defaults: no default chat capabilities, no beta query forwarding, and no default loss approvals (explicit -transcode-chat-capability/-transcode-allow-client-query/-transcode-allow-loss values still apply)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ai-concurrency-shaper %s\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: ai-concurrency-shaper [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	// The megabyte flags are validated against the largest byte shift each
	// value undergoes, so an overflowing shift can never silently wrap
	// (review-j finding 14).
	for _, mb := range []struct {
		name  string
		value int64
		shift uint
	}{
		// retry-max-body-mb undergoes a *2 in the journal sizing (maxBody*2):
		// it is validated against shift 21 so the product cannot overflow
		// (review-08 additional 3).
		{"retry-max-body-mb", retryMaxBodyMB, 21},
		{"transcode-max-request-mb", transcodeMaxRequestMB, 22},
		{"transcode-max-response-mb", transcodeMaxResponseMB, 20},
	} {
		if err := validateMBFlag(mb.name, mb.value, mb.shift); err != nil {
			log.Fatalf("invalid flag: %v", err)
		}
	}

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
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return fmt.Errorf("-upstream URL scheme must be http or https, got %q", upstream.Scheme)
	}
	if upstream.Hostname() == "" {
		return fmt.Errorf("-upstream URL must include a hostname")
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

	effectiveMaxConcurrency := upstreamMaxIdleConnsPerHost(globalConcurrency, concurrency, patterns, routeLimiters, limitAll)
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: effectiveMaxConcurrency,
		IdleConnTimeout:     120 * time.Second,
		DisableKeepAlives:   upstreamDisableKeepAlives,
	}

	// Wire transcoding mappings: repeatable -transcode-route values plus the
	// preset flags. Both Messages presets conflict and are rejected before
	// proxy.New runs. Every mapping carries the sensible default capability,
	// query-allowlist, and loss policies merged with the CLI additions, so a
	// minimal invocation works against a modern OpenAI-compatible chat
	// upstream out of the box.
	lossAllowed, lossNegated, err := parseNegatedLosses(transcodeAllowLoss...)
	if err != nil {
		log.Fatalf("invalid -transcode-allow-loss: %v", err)
	}
	lossPolicy := transcode.LossPolicy{Allowed: lossAllowed}
	chatCapabilities, capabilityNegated, err := parseChatCapabilities(transcodeCapabilities)
	if err != nil {
		log.Fatalf("invalid -transcode-chat-capability: %v", err)
	}
	allowedClientQuery, queryNegated, err := parseClientQuery(transcodeClientQuery)
	if err != nil {
		log.Fatalf("invalid -transcode-allow-client-query: %v", err)
	}
	mappings, err := buildTranscodeMappings(
		transcodeRoutes,
		transcodeResponsesChat,
		transcodeMessagesChat,
		transcodeMessagesResponses,
		transcodeCLIOptions{
			lossPolicy:          lossPolicy,
			negatedLosses:       lossNegated,
			capabilities:        chatCapabilities,
			negatedCapabilities: capabilityNegated,
			clientQuery:         allowedClientQuery,
			negatedQuery:        queryNegated,
			strictDefaults:      transcodeStrictDefaults,
		},
	)
	if err != nil {
		return err
	}
	transcodeModelMap, err := parseTranscodeModelMap(transcodeModels)
	if err != nil {
		return err
	}
	transcodeAuthPolicy, err := parseTranscodeAuth(
		transcodeAuth,
		transcodeAuthSource,
		transcodeAuthHeader,
		transcodeAnthropicVersion,
	)
	if err != nil {
		return err
	}
	for i := range mappings {
		mappings[i].ModelMap = transcodeModelMap
		mappings[i].Auth = transcodeAuthPolicy
		limits := transcode.BodyLimits{
			AcceptedRequestBytes: transcodeMaxRequestMB << 20,
			// The decoded limit is separate from the accepted raw-body limit
			// (merge gate 19): it bounds the rendered upstream request, with
			// headroom for decode amplification.
			DecodedRequestBytes:     transcodeMaxRequestMB << 22,
			SuccessfulResponseBytes: transcodeMaxResponseMB << 20,
			ErrorResponseBytes:      1 << 20,
			SSELineBytes:            1 << 20,
			SSEFrameBytes:           1 << 20,
		}
		// The retry-replay contract (internal/transcode/limits.go): a
		// declared bound must equal the proxy retry body cap with retries
		// enabled. With retries disabled no replay happens, so no bound is
		// declared and the fail-fast equality check does not apply
		// (review-k finding 8).
		if retryMax != 0 {
			limits.RetryReplayBytes = int64(retryMaxBodyMB) << 20
		}
		mappings[i].BodyLimits = limits
	}

	proxyOpts := []proxy.Option{
		proxy.WithUpstream(upstream),
		proxy.WithMatcher(matcher),
		proxy.WithLimiter(limiter),
		proxy.WithMetrics(met),
		proxy.WithQueueTimeout(queueTimeout),
		proxy.WithGlobalLimiter(globalLimiter),
		proxy.WithRouteLimiters(routeLimiters),
		proxy.WithMaxRetries(retryMax),
		proxy.WithMaxBodyBytes(int64(retryMaxBodyMB) << 20),
		proxy.WithRetryWaitMin(retryWaitMin),
		proxy.WithRetryWaitMax(retryWaitMax),
		proxy.WithRetryMinDelay(retryMinDelay),
		proxy.WithRetrySkipOn429(retrySkipOn429),
		proxy.WithCancelCooldown(cancelCooldown),
		proxy.WithFailureHold(failureHold),
		proxy.WithAdaptiveHeadroom(adaptiveHeadroom),
		proxy.WithAdaptiveHeadroomWindow(adaptiveHeadroomWindow),
		proxy.WithLimitAll(limitAll),
		proxy.WithTransport(transport),
		proxy.WithJournal(j),
		proxy.WithBreaker(breaker),
	}
	if len(mappings) > 0 {
		proxyOpts = append(proxyOpts, proxy.WithTranscodeMapping(mappings...))
		for _, m := range mappings {
			log.Printf(
				"transcoding %s %s (%s) -> %s (%s)",
				m.ClientRoute.Method,
				m.ClientRoute.Path,
				m.ClientProtocol,
				m.UpstreamPath,
				m.UpstreamProtocol,
			)
		}
	}

	p, err := proxy.New(proxyOpts...)
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

	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", bindAddr, err)
	}
	defer ln.Close()
	log.Printf("listening on %s", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tuiProgram *tea.Program
	tuiDone := make(chan struct{})

	if useTUI {
		log.Println("TUI dashboard enabled")
		snapCh := make(chan metrics.Snapshot, 16)
		progCh := make(chan *tea.Program, 1)
		go func() {
			tui.Run(snapCh, concurrency, j, progCh)
			close(tuiDone)
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

// limitFlags implements flag.Value for repeatable -limit flags.
type limitFlags []string

func (f limitFlags) String() string { return strings.Join(f, ", ") }
func (f *limitFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}
