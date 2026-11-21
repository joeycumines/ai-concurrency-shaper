package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/history"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
)

type gatewayRuntime struct {
	lifecycleMu    sync.Mutex
	closing        bool
	activeHandlers int
	handlersDone   chan struct{}
	handlerContext context.Context
	cancelHandlers context.CancelFunc
	hub            *observation.Hub
	controls       *control.Controller
	captureBytes   int
	ingressJournal *journal.Journal
}

func newGatewayRuntime(cfg *config.Config) (*gatewayRuntime, error) {
	var store observation.Sink
	if cfg.Server.HistoryDB != "" {
		s, err := history.Open(cfg.Server.HistoryDB, history.Options{Retention: cfg.Server.HistoryRetention, Durability: cfg.Server.HistoryDurability})
		if err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		store = s
	}
	providers := make([]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		providers[i] = p.EffectiveName()
	}
	hub := observation.New(observation.Config{Providers: providers, History: store})
	c := control.New()
	c.SetProviderCanceller(hub.CancelProvider)
	c.SetOnChange(func(a control.Audit) {
		hub.Marker(observation.Marker{At: a.At, Provider: a.Command.Provider, Kind: "control_" + a.Kind, Summary: a.Summary, RequestID: a.Command.RequestID})
		// %q neutralizes terminal/log control characters in configured names.
		log.Printf("runtime-control action=%q provider=%q pool=%q limit=%d ttl=%s revision=%d", a.Command.Action, a.Command.Provider, a.Command.Pool, a.Command.Limit, a.Command.TTL, a.Revision)
	})
	handlerContext, cancelHandlers := context.WithCancel(context.Background())
	rt := &gatewayRuntime{hub: hub, controls: c, captureBytes: cfg.Server.DiagnosticCaptureBytes, ingressJournal: journal.New(128, int64(cfg.Server.DiagnosticCaptureBytes)), handlersDone: make(chan struct{}), handlerContext: handlerContext, cancelHandlers: cancelHandlers}
	for _, p := range cfg.Providers {
		if err := rt.register(p); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = rt.Close(ctx)
			return nil, err
		}
	}
	return rt, nil
}
func (rt *gatewayRuntime) register(p *config.Provider) error {
	name := p.EffectiveName()
	pools := make(map[string]*queue.Limiter)
	if l := p.DefaultLimiter(); l != nil && l.Limit() > 0 {
		pools["default"] = l
	}
	if l := p.GlobalLimiter(); l != nil && l.Limit() > 0 {
		pools["global"] = l
	} else {
		rt.hub.SetResource(observation.Resource{Provider: name, ID: "global", Kind: "global", Known: false})
	}
	routes := p.RouteLimiters()
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	seen := make(map[*queue.Limiter]bool)
	for _, l := range pools {
		seen[l] = true
	}
	for _, k := range keys {
		l := routes[k]
		if l != nil && l.Limit() > 0 && !seen[l] {
			pools["route:"+k] = l
			seen[l] = true
		}
	}
	resourceCount := len(pools)
	if _, ok := pools["global"]; !ok {
		resourceCount++ // explicit unknown global capacity is also a resource.
	}
	if resourceCount > observation.MaxResourcePools {
		return fmt.Errorf("provider %q exceeds %d observable admission pools", name, observation.MaxResourcePools)
	}
	if err := rt.controls.RegisterProvider(name, pools); err != nil {
		return err
	}
	for id, l := range pools {
		kind := id
		if strings.HasPrefix(id, "route:") {
			kind = "route"
		}
		l.SetObserverWithClock(func(at time.Time, s queue.LimiterStats) {
			rt.hub.SetResourceAt(at, observation.Resource{Provider: name, ID: id, Kind: kind, Limit: int64(s.Effective), Occupied: s.Occupied, Cooldown: s.Cooldown, Queued: s.Waiters, Known: true})
		}, rt.hub.Now)
	}
	return nil
}

// Close rejects late ingress, cancels active handlers, and joins their final
// journals/facts before sealing history. When the join exceeds its deadline the
// caller still receives the timeout and may retry Close after a non-cooperative
// handler exits: the recorder is left open so no handler can publish behind a
// sealed sink. A separate bounded attempt still seals the last tick and drains
// already-queued observations, so slow ingress cannot discard committed facts.
func (rt *gatewayRuntime) Close(ctx context.Context) error {
	rt.lifecycleMu.Lock()
	if !rt.closing {
		rt.closing = true
		if rt.activeHandlers == 0 {
			close(rt.handlersDone)
		}
	}
	done := rt.handlersDone
	rt.lifecycleMu.Unlock()
	rt.cancelHandlers()
	rt.controls.Close()

	// A retry may arrive with an already-expired context. If all handlers have
	// joined, complete the seal regardless: the caller's deadline governed the
	// earlier join, not the independent final tick and sink drain.
	var joinErr error
	select {
	case <-done:
	default:
		select {
		case <-done:
		case <-ctx.Done():
			rt.lifecycleMu.Lock()
			active := rt.activeHandlers
			rt.lifecycleMu.Unlock()
			joinErr = fmt.Errorf("runtime shutdown waiting for %d ingress handlers before sealing history: %w", active, ctx.Err())
		}
	}

	// On a join timeout the recorder must stay open so a handler that finally
	// exits can publish its terminal fact; the retrying caller performs the
	// seal once the join succeeds.
	if joinErr != nil {
		return joinErr
	}
	// Once all handlers have joined, give the final tick and SQLite drain
	// their own budget. Reusing a nearly-expired ingress deadline would skip
	// the seal and silently lose the queued tail precisely when handlers were
	// slow to finish.
	sealCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return rt.hub.Close(sealCtx)
}
func (rt *gatewayRuntime) enterHandler() bool {
	rt.lifecycleMu.Lock()
	defer rt.lifecycleMu.Unlock()
	if rt.closing {
		return false
	}
	rt.activeHandlers++
	return true
}
func (rt *gatewayRuntime) leaveHandler() {
	rt.lifecycleMu.Lock()
	defer rt.lifecycleMu.Unlock()
	rt.activeHandlers--
	if rt.closing && rt.activeHandlers == 0 {
		close(rt.handlersDone)
	}
}

// observeIngress owns the original client identity, including routing failures
// before a provider is resolved. The selected proxy supplies its authoritative
// terminal result after journaling; Finish's once-only fallback covers routes
// that do not enter a proxy. Body reads still happen only in the existing path.
func (rt *gatewayRuntime) observeIngress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil {
			http.Error(w, "invalid request URL", http.StatusBadRequest)
			return
		}
		if !rt.enterHandler() {
			http.Error(w, "gateway is shutting down", http.StatusServiceUnavailable)
			return
		}
		// Registered first, so this runs after observation finalization and after the
		// read-interruption callback has joined. The mutex closes the Add/Wait race
		// for handlers scheduled just as shutdown begins.
		defer rt.leaveHandler()
		handlerContext, cancelHandler := context.WithCancel(r.Context())
		stopHandlerCancel := context.AfterFunc(rt.handlerContext, cancelHandler)
		defer func() { stopHandlerCancel(); cancelHandler() }()
		r = r.WithContext(handlerContext)
		parentContext, originalBody := r.Context(), r.Body
		started := time.Now()
		provider := observation.UnresolvedProvider
		if dispatcher, ok := next.(*router.Handler); ok {
			if selected, ok := dispatcher.ProviderForPath(r.URL.Path); ok {
				if _, ok := selected.Proxy.(*proxy.Proxy); ok {
					provider = selected.Name
				}
			}
		}
		ctx, request := rt.hub.Begin(r.Context(), observation.BeginInfo{Provider: provider, Method: r.Method, Path: r.URL.Path})
		capture := diagnostic.New(request.ID(), rt.captureBytes)
		capture.Versions(r.Header)
		ctx = diagnostic.WithContext(ctx, capture)
		r = r.WithContext(ctx)
		if r.Body == nil || r.Body == http.NoBody {
			capture.Metadata(diagnostic.ClientRequest, r.Header.Get("Content-Type"), 0)
			capture.Complete(diagnostic.ClientRequest, true)
		} else {
			r.Body = capture.WrapBody(diagnostic.ClientRequest, r.Body, r.Header.Get("Content-Type"), 0)
		}
		writer := diagnostic.WrapWriter(w, capture)
		// Cancellation must also interrupt a slow inbound body before routing or
		// conversion. A context alone does not unblock net/http's Body.Read. The
		// deadline is only armed for an operator/client cancellation, never for
		// Finish's ordinary cleanup cancellation. Serialize deadline changes with
		// handler return so this callback cannot affect a later keepalive request.
		deadlineSet := false
		interruptDone := make(chan struct{})
		controller := http.NewResponseController(w)
		stopInterrupt := context.AfterFunc(ctx, func() {
			defer close(interruptDone)
			if parentContext.Err() == nil && !request.Snapshot().Cancelled {
				return
			}
			deadlineSet = controller.SetReadDeadline(time.Now()) == nil
			if originalBody != nil {
				_ = originalBody.Close()
			}
		})
		defer func() {
			failure := recover()
			if !stopInterrupt() {
				<-interruptDone
			}
			if deadlineSet {
				_ = controller.SetReadDeadline(time.Time{})
			}
			if request.IsFinished() {
				capture.Seal()
				if failure != nil {
					panic(failure)
				}
				if request.Cancelled() {
					// A local operator cancellation is not a successful empty 200
					// or a successfully terminated chunked body for the client.
					panic(http.ErrAbortHandler)
				}
				return
			}
			status := writer.Status
			if status == 0 && !writer.Hijacked && failure == nil && writer.Err == nil && ctx.Err() == nil {
				status = http.StatusOK
			}
			outcome, stage := observation.OutcomeCompleted, observation.StageUnknown
			switch {
			case failure != nil:
				outcome = observation.OutcomeFailed
				stage = observation.StageProxy
				capture.Failure("proxy", "panic", "", "", "handler aborted")
			case writer.Err != nil:
				outcome = observation.OutcomeFailed
				stage = observation.StageDownstream
			case ctx.Err() != nil:
				outcome = observation.OutcomeAborted
			case status >= 500:
				outcome = observation.OutcomeFailed
				stage = observation.StageProxy
			case status >= 400:
				outcome = observation.OutcomeRejected
				stage = observation.StageAdmission
			}
			if outcome != observation.OutcomeCompleted && capture.FirstFailureFingerprint() == "" {
				capture.Failure(string(stage), string(outcome), "", "", "request ended before provider dispatch")
			}
			if status > 0 {
				capture.Metadata(diagnostic.DownstreamResponse, writer.Header().Get("Content-Type"), status)
			}
			capture.Complete(diagnostic.DownstreamResponse, !writer.Hijacked && writer.Err == nil && failure == nil && ctx.Err() == nil)
			evidence := capture.Seal()
			request.SetFingerprint(capture.FirstFailureFingerprint())
			urlCopy := *r.URL
			e := &journal.Entry{RequestID: request.ID(), Diagnostic: evidence, Method: r.Method, URL: &urlCopy, RequestHeaders: r.Header.Clone(), RequestBody: evidence.Legs[0].Body, RequestBodyTruncated: evidence.Legs[0].Truncated || !evidence.Legs[0].Complete, StatusCode: status, ResponseHeaders: writer.Header().Clone(), ResponseBody: evidence.Legs[3].Body, ResponseSize: evidence.Legs[3].ObservedBytes, ContentType: evidence.Legs[3].ContentType, Aborted: !evidence.Legs[3].Complete, Timing: journal.Timing{QueueStart: started, QueueEnd: started, ResponseHeaders: writer.HeadersAt}}
			if evidence.Legs[3].Complete {
				e.Timing.ResponseComplete = time.Now()
			}
			if !evidence.Legs[3].BytesKnown {
				e.ResponseSize = -1
			}
			rt.ingressJournal.Record(e)
			request.SetJournalID(e.ID)
			request.Finish(observation.FinishInfo{Status: status, Outcome: outcome, Stage: stage, Cancelled: ctx.Err() != nil})
			if failure != nil {
				panic(failure)
			}
			if request.Cancelled() {
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(writer, r)
	})
}
