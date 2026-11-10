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

package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// New creates a Proxy from the given options.
// Returns an error if required options are missing or validation fails.
func New(opts ...Option) (*Proxy, error) {
	var cfg proxyConfig
	for _, o := range opts {
		if err := o.applyProxyOption(&cfg); err != nil {
			return nil, err
		}
	}

	// Validate required fields.
	if cfg.upstream == nil {
		return nil, errors.New("proxy: upstream is required")
	}
	if cfg.matcher == nil {
		return nil, errors.New("proxy: matcher is required")
	}
	if cfg.limiter == nil {
		return nil, errors.New("proxy: limiter is required")
	}
	if cfg.metrics == nil {
		return nil, errors.New("proxy: metrics is required")
	}
	if err := validateRetryTransportBreaker(cfg.transport, cfg.breaker, cfg.maxRetries); err != nil {
		return nil, err
	}

	// Freeze the upstream URL: both the passthrough director and the
	// transcode handlers read it on every request, so caller mutation of the
	// original after New must not change live behavior or race with requests.
	cfg.upstream = cloneURL(cfg.upstream)

	// Freeze the auth policy: the credential is resolved once and pinned, so
	// a mutable custom SecretSource or caller mutation of the policy after
	// New cannot change live behavior, and source failures surface here.
	// Validate runs BEFORE the Rewrite hook can
	// rely on it: an invalid programmatic policy (unknown mode, missing
	// secret source, unresolved auto) fails construction instead of
	// degrading per request to strip-only forwarding.
	if cfg.authPolicy != nil {
		if err := cfg.authPolicy.Validate(); err != nil {
			return nil, fmt.Errorf("proxy: auth policy: %w", err)
		}
		frozen, err := auth.FreezeAuthPolicy(context.Background(), cfg.authPolicy)
		if err != nil {
			return nil, fmt.Errorf("proxy: freeze auth policy: %w", err)
		}
		cfg.authPolicy = frozen
	}

	// Copy the caller's route-limiters map and matcher: mutation of the
	// originals after New must not change live routing or limiter behavior.
	// The limiter values stay shared — they are
	// live synchronization objects.
	cfg.routeLimiters = cloneLimiterMap(cfg.routeLimiters)
	cfg.matcher = cloneMatcher(cfg.matcher)

	// Reject duplicate transcode client routes: the first matching handler
	// would silently win. The key is method+path. A mapping that declares a
	// RetryReplayBytes bound must equal the proxy's retry transport body cap
	// with retries enabled: the declared bound is
	// otherwise a silent lie about the actual replay cap.
	type transcodeTargetKey struct {
		routeKey transcode.RouteKey
		upstream transcode.UpstreamProtocol
	}
	seenTranscodeRoutes := make(map[transcodeTargetKey]struct{}, len(cfg.transcodeMappings))
	for i := range cfg.transcodeMappings {
		routeKey := cfg.transcodeMappings[i].Mapping.ClientRoute
		targetKey := transcodeTargetKey{
			routeKey: routeKey,
			upstream: cfg.transcodeMappings[i].Mapping.UpstreamProtocol,
		}
		if _, dup := seenTranscodeRoutes[targetKey]; dup {
			return nil, fmt.Errorf(
				"proxy: duplicate transcode client route %s %s (upstream %s)",
				routeKey.Method, routeKey.Path,
				cfg.transcodeMappings[i].Mapping.UpstreamProtocol,
			)
		}
		seenTranscodeRoutes[targetKey] = struct{}{}

		// An external signer signs EVERY actual attempt AFTER the retry
		// layer rebuilt the body. With retries enabled,
		// that requires the retry transport to buffer and replay bodies: a
		// zero body cap means no GetBody is ever supplied, so the signer
		// contract cannot be met and the configuration cannot possibly work.
		if cfg.transcodeMappings[i].Mapping.Auth.Mode == transcode.AuthExternalSigner &&
			cfg.maxRetries != 0 && cfg.maxBodyBytes <= 0 {
			return nil, fmt.Errorf(
				"proxy: transcode route %s %s uses an external signer with retries enabled but the retry body cap is zero; the signer cannot replay request bodies (set a positive retry body cap)",
				routeKey.Method,
				routeKey.Path,
			)
		}

		if declared := cfg.transcodeMappings[i].BodyLimits.RetryReplayBytes; declared != 0 {
			if cfg.maxRetries == 0 {
				return nil, fmt.Errorf(
					"proxy: transcode route %s %s declares RetryReplayBytes=%d but retries are disabled",
					routeKey.Method,
					routeKey.Path,
					declared,
				)
			}
			if declared != cfg.maxBodyBytes {
				return nil, fmt.Errorf(
					"proxy: transcode route %s %s RetryReplayBytes=%d must equal the proxy retry body cap %d",
					routeKey.Method,
					routeKey.Path,
					declared,
					cfg.maxBodyBytes,
				)
			}
		}
	}

	transport := buildUpstreamTransport(&cfg)

	// Track whether the retry transport handles breaker reporting. Detect direct
	// retry transports and transparent wrappers that expose retry ownership, so
	// wrapper use does not silently split breaker accounting.
	retryInfo := detectRetryTransport(transport)
	retryTracksAttempts := retryInfo.found
	var retryHandlesBreaker bool
	if retryInfo.found {
		if retryInfo.breaker != nil {
			retryHandlesBreaker = true
		}
		// Wire the in-flight retry counter for TUI visibility.
		// The counter is the metrics collector's atomic, so the TUI sees
		// retry pressure through the snapshot cycle.
		if retryInfo.setInFlightRetries != nil {
			retryInfo.setInFlightRetries(cfg.metrics.RetriesInFlightCounter())
		}
	}

	p := &Proxy{
		matcher:                cfg.matcher,
		limiter:                cfg.limiter,
		m:                      cfg.metrics,
		timeout:                cfg.queueTimeout,
		queueDepthLimit:        cfg.queueDepthLimit,
		queueComments:          cfg.queueComments,
		globalLimiter:          cfg.globalLimiter,
		routeLimiters:          cfg.routeLimiters,
		journal:                cfg.journal,
		breaker:                cfg.breaker,
		cancelCooldown:         cfg.cancelCooldown,
		failureHold:            cfg.failureHold,
		retryHandlesBreaker:    retryHandlesBreaker,
		retryTracksAttempts:    retryTracksAttempts,
		adaptiveHeadroom:       cfg.adaptiveHeadroom,
		adaptiveHeadroomWindow: cfg.adaptiveHeadroomWindow,
		limitAll:               cfg.limitAll,
		authPolicy:             cfg.authPolicy,
		opencode:               cfg.opencode,
	}

	// Build one transcode handler per mapping, each forwarding through the
	// proxy engine (the retry/breaker-aware transport). The mutable parts of
	// the configuration are deep-copied so caller mutation after New cannot
	// change live behavior or race with requests.
	handlersByRoute := make(map[transcode.RouteKey][]targetTranscodeHandler)
	for i := range cfg.transcodeMappings {
		m := &cfg.transcodeMappings[i]
		mapping := m.Mapping
		mapping.ModelMap = cloneModelMap(m.ModelMap)
		mapping.ProfileMap = cloneProfileMap(m.ProfileMap)
		mapping.LossPolicy = cloneLossPolicy(m.LossPolicy)
		mapping.AllowedClientQuery = cloneStringSet(m.AllowedClientQuery)
		if mapping.Auth.IsZero() {
			if cfg.authPolicy != nil {
				// Inherited provider auth adapts to the mapping's upstream
				// dialect (bearer<->x-api-key) so a model authenticates
				// identically whether served natively or transcoded.
				mapping.Auth = transcode.FromProviderAuthForUpstream(cfg.authPolicy, mapping.UpstreamProtocol)
			} else {
				mapping.Auth = transcode.AuthPolicy{Mode: transcode.AuthNone}
			}
		}
		if err := mapping.Validate(); err != nil {
			return nil, fmt.Errorf("proxy: invalid transcode mapping: %w", err)
		}
		h := transcode.NewTranscodeHandler(
			transcode.HandlerConfig{
				Mapping:              mapping,
				Upstream:             cfg.upstream,
				BodyLimits:           m.BodyLimits,
				FlowLogDir:           m.FlowLogDir,
				Continuity:           m.Continuity,
				ContinuityKey:        m.ContinuityKey,
				GeminiSignatures:     m.GeminiSignatures,
				GeminiSignatureScope: m.GeminiSignatureScope,
			},
			p.RoundTrip,
			nil,
		)
		p.transcodeHandlers = append(p.transcodeHandlers, h)
		if m.ClientProtocol == transcode.ClientGemini {
			p.geminiRoutes = true
		}
		handlersByRoute[m.ClientRoute] = append(handlersByRoute[m.ClientRoute], targetTranscodeHandler{
			mapping: m,
			handler: h,
		})
	}
	p.transcodeHandlerMap = make(map[transcode.RouteKey]http.Handler, len(handlersByRoute))
	for routeKey, targets := range handlersByRoute {
		if len(targets) == 1 {
			p.transcodeHandlerMap[routeKey] = targets[0].handler
			continue
		}
		dispatcher, err := newMultiTranscodeDispatcher(routeKey, targets)
		if err != nil {
			return nil, err
		}
		p.transcodeHandlerMap[routeKey] = dispatcher
	}

	// Native routes share the transparent engine with model-identifier
	// rewriting. Dispatch tries native first; a model whose dialect differs
	// falls through to the transcode mapping on the same route when one
	// exists (that is how one path serves both natively and by
	// conversion). Duplicate native keys are rejected.
	p.nativeRouteMap = make(map[transcode.RouteKey]*NativeRoute, len(cfg.nativeRoutes))
	for i := range cfg.nativeRoutes {
		// Copy into proxy-owned storage so caller mutation after New
		// cannot change live routing or race with requests.
		p.nativeRoutes = append(p.nativeRoutes, cfg.nativeRoutes[i])
		nr := &p.nativeRoutes[len(p.nativeRoutes)-1]
		nr.ModelMap = cloneModelMap(nr.ModelMap)
		if nr.Protocol == transcode.NativeGemini {
			p.geminiRoutes = true
		}
		if _, dup := p.nativeRouteMap[nr.RouteKey]; dup {
			return nil, fmt.Errorf("proxy: duplicate native route %s %s",
				nr.RouteKey.Method, nr.RouteKey.Path)
		}
		p.nativeRouteMap[nr.RouteKey] = nr
	}

	// The catalog is a local answer, not an upstream route: build its handler
	// once here so an invalid snapshot is a startup error, never a
	// first-request surprise.
	if cfg.modelCatalog != nil {
		catalogHandler, err := transcode.NewCatalogHandler(*cfg.modelCatalog)
		if err != nil {
			return nil, fmt.Errorf("proxy: model catalog: %w", err)
		}
		p.catalog = catalogHandler
	}

	// The native auth variants are precomputed once so the Rewrite hot
	// path only selects a ready pointer by the natively served dialect.
	nativeAuth := newNativeAuthVariants(cfg.authPolicy)

	rp := &httputil.ReverseProxy{
		// Rewrite (not the deprecated Director) is load-bearing for auth
		// safety: stdlib strips hop-by-hop headers from the outbound clone
		// BEFORE this hook runs, so headers injected here cannot be removed
		// by a client Connection list (golang/go#50580). stdlib also deletes
		// Forwarded/X-Forwarded-* before calling us, preserving the stealth
		// posture; we never call SetXForwarded.
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL joins the upstream base path with the client path at
			// the slash boundary only (no dot-segment cleaning, preserving
			// the escaped path) and merges the base query with the client
			// query by raw concatenation — the transparent-proxy contract.
			pr.SetURL(cfg.upstream)
			pr.Out.Host = cfg.upstream.Host
			if nativeAuth.provider != nil {
				effectiveAuth := nativeAuth.provider
				if alias, ok := nativeAliasFromContext(pr.In.Context()); ok {
					effectiveAuth = nativeAuth.forProtocol(alias.protocol)
				}
				// Effectively unreachable for config-built policies: their
				// sources reject blank credentials at construction time, and
				// Validate ran at startup. Kept defensive for direct callers
				// and future SecretSource implementations. On failure, fall
				// back to strip-only rather than forwarding an
				// unauthenticated or client-credentialed request.
				if err := auth.ApplyUpstreamAuthentication(pr.Out.Context(), pr.Out, effectiveAuth); err != nil {
					slog.Error("upstream auth failed; forwarding stripped-only", "error", err)
					auth.StripCredentials(pr.Out.Header)
				}
			}
			if cfg.opencode.Enabled {
				// First-party headers go on after authentication: the mock
				// never clobbers credentials. The conversation key stashed
				// by the native path wins; otherwise the key derives from
				// the client address (this path never inspects the body).
				fallback := transcode.DeriveClientKey(cfg.opencode.Provider, pr.In.RemoteAddr)
				if alias, ok := nativeAliasFromContext(pr.In.Context()); ok && alias.convKey != "" {
					fallback = alias.convKey
				}
				session := cfg.opencode.ResolveSession(pr.In.Header, fallback)
				cfg.opencode.ApplyHeaders(pr.Out.Header, pr.In.Header, session)
			}
		},
		Transport: p,
		ModifyResponse: func(res *http.Response) error {
			if err := validateSwitchingProtocolsResponse(res); err != nil {
				return err
			}
			// Natively served exchanges restore the client-facing model
			// alias on non-streaming JSON responses; everything else
			// passes through untouched.
			return rewriteNativeResponseAlias(res)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if rec, ok := w.(*statusRecorder); ok {
				rec.transportErr = err
				// Record whether the round trip actually started, so a
				// proxy-generated error that never reached the upstream (an
				// unsupported CONNECT, a signer failure) is never classified as
				// an upstream transport failure. The attempt marker inside the
				// transport chain is the authority; it is set before the
				// configured transport is called.
				if state := upstreamAttemptStateFromContext(r.Context()); state != nil {
					rec.upstreamAttempted = state.started.Load()
				}
				if rec.hijacked && rec.status == http.StatusSwitchingProtocols {
					// ReverseProxy.handleUpgradeResponse hijacks first, then writes and
					// flushes the 101 response on the hijacked bufio.Writer. If that
					// post-hijack handshake fails, no 502 can be emitted, but the
					// exchange is not a clean upgrade completion. The write/flush bypasses
					// statusRecorder.Write, so set the downstream-failure fact here as the
					// slot-release signal for cancel-cooldown protection.
					rec.writeFailed = true
					rec.writeErr = err
					rec.aborted = true
					if rec.onSwitchingProtocolsHandshakeFailure != nil {
						rec.onSwitchingProtocolsHandshakeFailure()
					}
					return
				}
				if isLocalSwitchingProtocolsFailure(r, err) {
					if isLocalSwitchingProtocolsNonHijackerFailure(err) {
						closeSwitchingProtocolsResponseBodyFromContext(r.Context())
					}
					// ReverseProxy can fail an otherwise valid upstream 101 locally before
					// or during the downstream upgrade handoff (for example a non-Hijacker
					// ResponseWriter or failed Hijack). This is not upstream-health input,
					// but it is still an incomplete attempted upgrade, so keep it out of
					// breaker failure classification while reporting the exchange as aborted.
					rec.localUpgradeFailure = true
					rec.aborted = true
				}
			}
			if isContextCancellation(r.Context().Err()) && isContextCancellation(err) {
				return
			}
			// A breaker that opened between retries is the same rejection the
			// pre-flight Allow() check produces (503 + IncCircuitRejected),
			// not an upstream transport failure: the exchange never reached
			// the upstream, and isUpstreamFailureStatus already excludes
			// retryCircuitOpen from breaker classification — the 502 left
			// shaper_circuit_rejected_total blind.
			if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
				p.m.IncCircuitRejected()
				if rec, ok := w.(*statusRecorder); ok {
					if rec.commentsCommitted {
						// The streaming representation was already
						// committed: failed close. The transcode handler
						// frames its own dialect error events (via the
						// committed-stream context marker); this
						// transparent-path failure is dialect-agnostic,
						// so the failed close is the comment + EOF (the
						// terminalWritten guard below cannot be relied
						// on for this path — recordImplicitOK satisfied
						// it on the comment write).
						// The same proxy-generated 503 the uncommitted
						// branch writes is recorded behind the committed
						// 200, after the comment (Write swallows bytes once
						// an override is set).
						fmt.Fprintf(rec, ": queue-wait failed: circuit open\n\n")
						_ = rec.FlushError()
						rec.proxyGeneratedError = true
						rec.recordCommittedFailure(http.StatusServiceUnavailable)
					} else if !rec.terminalWritten {
						rec.proxyGeneratedError = true
						http.Error(rec, "circuit open", http.StatusServiceUnavailable)
					}
				}
				return
			}
			slog.Error("proxy transport error", "error", sanitizeTransportError(err))
			if rec, ok := w.(*statusRecorder); ok {
				if rec.commentsCommitted {
					// The same 502 the uncommitted branch writes, recorded
					// behind the committed 200: without it classification
					// reads the commit's 200 and a dead upstream never
					// reaches the breaker.
					fmt.Fprintf(rec, ": queue-wait failed: upstream\n\n")
					_ = rec.FlushError()
					rec.proxyGeneratedError = true
					rec.recordCommittedFailure(http.StatusBadGateway)
				} else if !rec.terminalWritten {
					rec.proxyGeneratedError = true
					http.Error(rec, "bad gateway", http.StatusBadGateway)
				}
			}
		},
		FlushInterval: -1,
	}
	p.inner = rp
	p.transport = transport
	return p, nil
}
