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
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// targetTranscodeHandler pairs one transcode mapping with the handler built
// from it. The mapping pointer refers to proxy-owned storage (the option
// slice copied during New), so it cannot alias caller state.
type targetTranscodeHandler struct {
	mapping *TranscodeMapping
	handler http.Handler
}

// dispatcherTarget is one dispatch candidate: the mapping's frozen
// model-resolution state and its handler.
type dispatcherTarget struct {
	profileMap transcode.ProfileMap
	modelMap   transcode.ModelMap
	handler    http.Handler
	// chat marks the chat-completions target, the dispatcher's fallback
	// handler: a claim it makes is preferred when no dialect decides.
	chat bool
}

// targetAnswer is one target's view of the request model, mirroring the
// handler's own resolution order: a profile name resolves to its target model
// first, then the model map.
type targetAnswer struct {
	// via is the target model's declared dialect; explicit reports that one
	// is known. Only an explicit answer decides the route: an identity
	// fallback (no Via) says nothing about which upstream serves the model,
	// and a later target's map may name it.
	via      transcode.NativeProtocol
	explicit bool
	// claimed reports that the name is one of this target's profiles: a
	// claiming handler is the only kind that knows the name (any other would
	// treat the profile as a model name). unmapped reports a claim whose
	// target model this target cannot map.
	claimed  bool
	unmapped bool
}

func (t dispatcherTarget) resolve(model string) targetAnswer {
	if profileModel, _, ok := t.profileMap.ResolveProfile(model); ok {
		resolved, err := t.modelMap.Resolve(profileModel)
		if err != nil {
			return targetAnswer{claimed: true, unmapped: true}
		}
		return targetAnswer{via: resolved.Via, explicit: resolved.Via != "", claimed: true}
	}
	resolved, err := t.modelMap.Resolve(model)
	if err != nil {
		// An unmapped model is not this target's answer to give: a later
		// target's map may know it.
		return targetAnswer{}
	}
	return targetAnswer{via: resolved.Via, explicit: resolved.Via != ""}
}

// multiTranscodeDispatcher dispatches a single client route (such as POST
// /v1/messages) across multiple transcode handlers by the request's target
// model dialect (Via). A single mapping on a route is the operator's blanket
// serving choice (pre-existing fallthrough semantics); multiple mappings
// switch this dispatcher to via-directed selection:
//
//   - A native miss on the same path supplies the model name, so the body is
//     never re-read. A Via the native route resolved decides the dialect; an
//     empty Via (identity fallback) resolved nothing, so the name is still
//     resolved against the targets below.
//   - Otherwise the body is probed once — bounded by the largest
//     accepted-request bound across the targets — and the model is resolved
//     against each target in registration order: the first explicit dialect
//     decides. Conflicting maps across targets therefore resolve
//     deterministically by registration order, never "last mapping wins".
//   - A profile follows its target model's dialect. A target that claims a
//     profile whose target model it cannot map decides the route: its handler
//     renders the profile refusal. A claim with no dialect is served by a
//     claiming handler (the chat target first, matching the fallback below),
//     because only a claiming handler knows the name.
//   - via=responses and via=chat select their matching handler.
//   - via=messages fails closed with a 404 naming the model: no transcode
//     direction serves a messages-native model faithfully, and converting
//     through the wrong upstream would misreport the exchange.
//   - An unresolvable or absent model falls to the chat-first handler so it
//     renders its own dialect-shaped model-resolution error, preserving
//     single-handler semantics.
type multiTranscodeDispatcher struct {
	routeKey transcode.RouteKey
	// client is the shared client dialect of every mapping on the route;
	// construction rejects conflicting client protocols, so dispatcher-level
	// errors always speak the right dialect.
	client transcode.ClientProtocol
	// targets in registration order.
	targets []dispatcherTarget
	// acceptedBytes is the largest accepted-request bound across the
	// targets; a body over it exceeds every target's own bound by
	// construction, so the forwarded acceptedBytes+1 replay is refused 413
	// by whichever handler receives it.
	acceptedBytes int64
	// errorMessageBytes bounds dispatcher-authored error text, which quotes
	// the client-controlled model name.
	errorMessageBytes int64
	chatHandler       http.Handler
	responsesHandler  http.Handler
}

// newMultiTranscodeDispatcher freezes the targets' resolution state (maps
// are cloned, so caller mutation after New cannot change dispatch) and
// rejects a route whose mappings disagree on the client protocol: one route
// is one client dialect, and a dispatcher mixing two would render errors and
// parse requests in the wrong shape.
func newMultiTranscodeDispatcher(routeKey transcode.RouteKey, targets []targetTranscodeHandler) (*multiTranscodeDispatcher, error) {
	d := &multiTranscodeDispatcher{routeKey: routeKey}
	for _, t := range targets {
		if d.client == "" {
			d.client = t.mapping.ClientProtocol
		} else if t.mapping.ClientProtocol != d.client {
			return nil, fmt.Errorf(
				"proxy: transcode mappings for client route %s %s declare conflicting client protocols (%s vs %s)",
				routeKey.Method, routeKey.Path, d.client, t.mapping.ClientProtocol,
			)
		}
		limits := t.mapping.BodyLimits.WithDefaults()
		if int64(limits.AcceptedRequestBytes) > d.acceptedBytes {
			d.acceptedBytes = int64(limits.AcceptedRequestBytes)
		}
		if int64(limits.ErrorMessageBytes) > d.errorMessageBytes {
			d.errorMessageBytes = int64(limits.ErrorMessageBytes)
		}
		switch t.mapping.Mapping.UpstreamProtocol {
		case transcode.UpstreamResponses:
			d.responsesHandler = t.handler
		case transcode.UpstreamChatCompletions:
			d.chatHandler = t.handler
		}
		d.targets = append(d.targets, dispatcherTarget{
			profileMap: cloneProfileMap(t.mapping.ProfileMap),
			modelMap:   cloneModelMap(t.mapping.ModelMap),
			handler:    t.handler,
			chat:       t.mapping.Mapping.UpstreamProtocol == transcode.UpstreamChatCompletions,
		})
	}
	if d.acceptedBytes <= 0 || d.errorMessageBytes <= 0 {
		return nil, fmt.Errorf(
			"proxy: transcode mappings for client route %s %s resolved a non-positive body bound",
			routeKey.Method, routeKey.Path,
		)
	}
	return d, nil
}

// cloneProfileMap deep-copies the profile map so dispatcher dispatch cannot
// race caller mutation of the configured mapping.
func cloneProfileMap(m transcode.ProfileMap) transcode.ProfileMap {
	if m.Profiles == nil {
		return m
	}
	profiles := make(map[string]transcode.ProfileMapping, len(m.Profiles))
	maps.Copy(profiles, m.Profiles)
	return transcode.ProfileMap{Profiles: profiles}
}

// errorBodyReader replays already-consumed body bytes and then reports the
// original transport read error on every subsequent read, so the chosen
// handler observes the failure exactly as if it had read the original body
// itself: a 400 naming the error, or a client-abort classification when the
// request context is cancelled. Replaying the bytes under a clean EOF would
// instead turn a dead connection into an apparently successful exchange.
type errorBodyReader struct {
	rest *bytes.Reader
	err  error
}

func (b *errorBodyReader) Read(p []byte) (int, error) {
	if b.rest.Len() > 0 {
		return b.rest.Read(p)
	}
	return 0, b.err
}

func (b *errorBodyReader) Close() error { return nil }

func (d *multiTranscodeDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var (
		targetVia   transcode.NativeProtocol
		clientModel string
		resolved    bool
	)
	miss, missed := nativeMissFromContext(r.Context())
	if missed {
		// The native route's model resolution answers for the models its own
		// map knows. A Via it resolved decides the dialect; an identity
		// fallback resolved nothing, so the model still has to be resolved
		// here against the mappings on the route.
		clientModel = miss.clientModel
		if miss.via != "" {
			targetVia, resolved = miss.via, true
		}
	}
	if !resolved && clientModel == "" && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, d.acceptedBytes+1))
		// Close the consumed original exactly like the native path does:
		// the journal's request-body capture has seen every byte read
		// here, and the connection is handed back cleanly.
		_ = r.Body.Close()
		out := *r
		if err != nil {
			out.Body = &errorBodyReader{rest: bytes.NewReader(body), err: err}
		} else {
			out.Body = io.NopCloser(bytes.NewReader(body))
		}
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		r = &out
		if err == nil && int64(len(body)) <= d.acceptedBytes {
			if model, present, perr := transcode.TopLevelModelPresent(body); perr == nil && present && model != "" {
				clientModel = model
			}
		}
	}
	var claiming http.Handler
	if !resolved && clientModel != "" {
		targetVia, claiming = d.selectTarget(clientModel)
	}

	switch {
	case targetVia == "" && claiming != nil:
		// A target claimed the name as a profile and no target named a
		// dialect: the profile's target model resolves by identity there
		// (the claimant serves it), or cannot be mapped (the claimant renders
		// its own profile refusal in the mapping's dialect). Either way the
		// request reaches a handler that knows the name, never one that would
		// forward the profile name as a model.
		claiming.ServeHTTP(w, r)
	case targetVia == transcode.NativeResponses && d.responsesHandler != nil:
		d.responsesHandler.ServeHTTP(w, r)
	case targetVia == transcode.NativeChat && d.chatHandler != nil:
		d.chatHandler.ServeHTTP(w, r)
	case targetVia == transcode.NativeMessages:
		// Faithful to the native route's own refusal vocabulary: a
		// messages-native model cannot be served by any configured
		// transcode target on this route.
		d.writeRouteError(w, r, http.StatusNotFound, d.messagesNativeRefusal(clientModel, miss, missed))
	case d.chatHandler != nil:
		d.chatHandler.ServeHTTP(w, r)
	case d.responsesHandler != nil:
		d.responsesHandler.ServeHTTP(w, r)
	default:
		d.writeRouteError(w, r, http.StatusInternalServerError,
			fmt.Sprintf("route %s has no usable transcode handler", d.routeKey.Path))
	}
}

// selectTarget walks the targets in registration order and stops at the first
// decisive answer: an explicit dialect (returned with the handler that answered
// it), or a profile claim whose target model the claiming target cannot map
// (returned with no dialect, so that handler renders its own "profile %q
// targets unmapped model %q" refusal instead of the request reaching another
// upstream under a name the operator mapped to this one).
//
// A profile claim that resolves with no dialect decides nothing: the search
// continues, and if no target names a dialect the claimant is returned with no
// dialect — the chat target if it claims, else the first claimant — so the
// profile lands where its target model would when requested by name, on a
// handler that knows the profile. No claim and no dialect: ("", nil), and the
// dispatcher's fallback decides.
func (d *multiTranscodeDispatcher) selectTarget(clientModel string) (transcode.NativeProtocol, http.Handler) {
	var claimant *dispatcherTarget
	for i := range d.targets {
		target := &d.targets[i]
		answer := target.resolve(clientModel)
		if answer.explicit {
			return answer.via, target.handler
		}
		if answer.unmapped {
			return "", target.handler
		}
		if answer.claimed && (claimant == nil || (target.chat && !claimant.chat)) {
			claimant = target
		}
	}
	if claimant != nil {
		return "", claimant.handler
	}
	return "", nil
}

// messagesNativeRefusal words the 404 for a messages-native model so its
// remedy is one the operator can apply. A path carries at most one native
// route, and a native miss proves this one has it: adding another would be a
// duplicate-route startup error. Native serving resolves model names only,
// never -transcode-profile names, so a profile is pointed at its target model.
func (d *multiTranscodeDispatcher) messagesNativeRefusal(clientModel string, miss nativeMissInfo, missed bool) string {
	for _, target := range d.targets {
		profileModel, _, ok := target.profileMap.ResolveProfile(clientModel)
		if !ok {
			continue
		}
		where := " on a path whose -native-route serves " + string(transcode.NativeMessages)
		switch {
		case !missed:
			where += fmt.Sprintf(" (or add a -native-route %s@%s)", transcode.NativeMessages, d.routeKey.Path)
		case miss.nativeProtocol == transcode.NativeMessages:
			where = ""
		}
		return fmt.Sprintf(
			"%q is a -transcode-profile whose target model %q is served natively as %s; native serving resolves model names only, and the transcode mappings on %s reach the chat-completions and responses upstreams only — request %q by name%s",
			clientModel, profileModel, transcode.NativeMessages, d.routeKey.Path, profileModel, where,
		)
	}
	switch {
	case !missed:
		return fmt.Sprintf(
			"model %q is served natively as %s; this route transcodes to the chat-completions and responses upstreams only, so no transcode mapping on %s can serve it — request it on its own native path, or add a -native-route %s@%s for it",
			clientModel, transcode.NativeMessages, d.routeKey.Path, transcode.NativeMessages, d.routeKey.Path,
		)
	case miss.nativeProtocol == transcode.NativeMessages:
		return fmt.Sprintf(
			"model %q is served natively as %s by the transcode mappings' model map, but this path's %s native route does not map it, and the transcode mappings reach the chat-completions and responses upstreams only",
			clientModel, transcode.NativeMessages, transcode.NativeMessages,
		)
	default:
		return fmt.Sprintf(
			"model %q is served natively as %s; %s already carries a %s native route and its transcode mappings reach the chat-completions and responses upstreams only — request it on a path whose -native-route serves %s",
			clientModel, transcode.NativeMessages, d.routeKey.Path, miss.nativeProtocol, transcode.NativeMessages,
		)
	}
}

// recorderFrom finds the proxy's statusRecorder behind any Unwrap-capable
// diagnostic wrappers, the same traversal transcode.WriteCommittedStreamError
// performs, so classification facts land on the recorder whatever wraps it.
func recorderFrom(w http.ResponseWriter) *statusRecorder {
	for cur := w; cur != nil; {
		if rec, ok := cur.(*statusRecorder); ok {
			return rec
		}
		unwrapper, ok := cur.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		cur = unwrapper.Unwrap()
	}
	return nil
}

// writeRouteError renders a dispatcher-level refusal in the route's client
// dialect and records the local outcome so the proxy's exactly-one-outcome
// invariant holds even though no handler ran. The message is bounded because
// it quotes the client-controlled model name; 5xx marks the exchange as a
// proxy-generated error (never an upstream health signal), mirroring the
// native path's error writer.
//
// When the streaming representation was already committed before admission
// (SSE queue comments), the status is fixed and the exchange is locked to the
// stream: the refusal becomes the dialect's SSE error terminal, exactly as a
// handler's own post-commit error path renders it.
//
// DownstreamComplete means the response was delivered, not that the exchange
// succeeded — the same meaning every handler error writer gives it. A
// delivered refusal therefore completes downstream while remaining a local
// failure (Provenance + LocalFailure), which is exactly how the identical
// refusal is counted when a handler renders it.
func (d *multiTranscodeDispatcher) writeRouteError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if status >= 500 {
		// A 5xx refusal is a proxy-local defect (no upstream was seen), so
		// the recorder's proxyGeneratedError fact keeps the exchange out of
		// upstream-health classification. The queue-comment commit already
		// wrote a 200 through the recorder, which marks it terminal; the fact
		// is a classification input, not a WriteHeader guard, so it is set
		// regardless of that earlier write.
		if rec := recorderFrom(w); rec != nil {
			rec.proxyGeneratedError = true
		}
	}
	message = transcode.BoundErrorMessage(message, int(d.errorMessageBytes))
	apiErr := transcode.CanonicalAPIError{
		Status:  status,
		Message: message,
	}
	committed := transcode.CommittedStreamFromContext(r.Context())
	var writeErr error
	if committed {
		writeErr = transcode.WriteCommittedStreamError(w, d.client, apiErr)
	} else {
		writeErr = transcode.WriteDialectHTTPError(w, d.client, apiErr)
	}
	outcome := transcode.Outcome{
		UpstreamAttempted: false,
		UpstreamStatus:    transcode.Optional[int]{Value: status, Set: true},
		Provenance:        transcode.ProvenanceLocalRequestConversionError,
		LocalFailure:      true,
	}
	if writeErr != nil {
		// The refusal was never delivered: the exchange did not complete
		// cleanly — a client abort when the context is cancelled, else a
		// downstream write failure.
		if r.Context().Err() != nil {
			outcome.Provenance = transcode.ProvenanceClientAbort
			outcome.ClientAborted = true
		} else {
			outcome.Provenance = transcode.ProvenanceDownstreamWriteError
		}
	} else {
		outcome.DownstreamComplete = true
	}
	if sink := transcode.OutcomeSinkFromContext(r.Context()); sink != nil {
		sink.Record(outcome)
	}
}
