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
	"slices"
	"strings"

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
//     against each target: the explicit dialect decides. Conflicting explicit
//     dialects for one model, and conflicting definitions of one profile name,
//     are refused rather than resolved by registration order.
//   - A profile follows its target model's dialect, and is only ever served
//     by a handler whose mapping defines it: any other handler would forward
//     the profile name upstream as a model name. An explicit dialect declared
//     for the target model decides the route symmetrically, regardless of
//     mapping registration order. Claimants of one profile name must decide one
//     dialect and one target model between them, and the claimants that decide
//     it must agree on the reasoning tier; the CLI stamps one profile map per
//     provider, so divergent definitions are real configurations. When the
//     decided dialect's handler does not define the profile as the claimants
//     do, the request is refused. Any other disagreement is refused as well,
//     with a message that names the sorted disagreement. A profile unmapped
//     across the route is refused by the claiming handler (chat preferred as
//     fallback).
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
	// chatTarget and responsesTarget index the targets serving each
	// dialect (-1 when absent), so a profile answer can be checked against
	// the mapping that would serve it.
	chatTarget      int
	responsesTarget int
}

// newMultiTranscodeDispatcher freezes the targets' resolution state (maps
// are cloned, so caller mutation after New cannot change dispatch) and
// rejects a route whose mappings disagree on the client protocol: one route
// is one client dialect, and a dispatcher mixing two would render errors and
// parse requests in the wrong shape.
func newMultiTranscodeDispatcher(routeKey transcode.RouteKey, targets []targetTranscodeHandler) (*multiTranscodeDispatcher, error) {
	d := &multiTranscodeDispatcher{routeKey: routeKey, chatTarget: -1, responsesTarget: -1}
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
			if d.responsesHandler == nil {
				d.responsesHandler = t.handler
				d.responsesTarget = len(d.targets)
			}
		case transcode.UpstreamChatCompletions:
			if d.chatHandler == nil {
				d.chatHandler = t.handler
				d.chatTarget = len(d.targets)
			}
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
			if err != nil {
				return &errorBodyReader{rest: bytes.NewReader(body), err: err}, nil
			}
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		r = &out
		if err == nil && int64(len(body)) <= d.acceptedBytes {
			if model, present, perr := transcode.TopLevelModelPresent(body); perr == nil && present && model != "" {
				clientModel = model
			}
		}
	}
	var sel targetSelection
	if !resolved && clientModel != "" {
		if refusal, ok := d.routeConflict(clientModel); !ok {
			d.writeRouteError(w, r, http.StatusInternalServerError, refusal)
			return
		}
		sel = d.selectTarget(clientModel)
		targetVia = sel.via
	}
	claiming := sel.handler

	if sel.profileModel != "" && targetVia != transcode.NativeMessages && targetVia != "" {
		if refusal, ok := d.profileDialectRefusal(clientModel, sel); !ok {
			d.writeRouteError(w, r, http.StatusInternalServerError, refusal)
			return
		}
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
		d.writeRouteError(w, r, http.StatusNotFound, d.messagesNativeRefusal(clientModel, sel, miss, missed))
	case d.chatHandler != nil:
		d.chatHandler.ServeHTTP(w, r)
	case d.responsesHandler != nil:
		d.responsesHandler.ServeHTTP(w, r)
	default:
		d.writeRouteError(w, r, http.StatusInternalServerError,
			fmt.Sprintf("route %s has no usable transcode handler", d.routeKey.Path))
	}
}

// targetSelection is the dispatcher's decision for a request model: the
// dialect that decided it (empty when none did), the handler of the target
// that decided or claimed it, and, when that answer came from a profile
// claim, the profile's target model and the claiming target.
type targetSelection struct {
	via          transcode.NativeProtocol
	handler      http.Handler
	profileModel string
	target       *dispatcherTarget
}

// selectTarget decides which handler serves clientModel.
//
// When clientModel is claimed as a profile:
//   - Any claiming target whose model map assigns the target model an explicit
//     dialect decides the route.
//   - If no claiming target has an explicit dialect, but another target on the
//     route assigns the target model an explicit dialect, that dialect decides
//     (and profileDialectRefusal enforces that the serving handler defines the
//     profile).
//   - If no explicit dialect is declared anywhere on the route, a claiming
//     target that resolves the target model via identity serves it (chat
//     claimant preferred as fallback, matching single-target fallback rules).
//   - If no target can map the target model at all, the profile is unmapped
//     across the entire route; the fallback claimant (chat preferred, else the
//     first claimant) renders its profile refusal.
//
// In every case, profile dispatch is registration-order independent, and a
// non-claiming target's raw model match on the literal profile name is never
// allowed to steal the dispatch or forward the profile name upstream.
//
// When clientModel is not claimed as a profile, the targets' model maps are
// searched in registration order for an explicit dialect; if none is found,
// the zero selection is returned and the fallback decides.
func (d *multiTranscodeDispatcher) selectTarget(clientModel string) targetSelection {
	type claim struct {
		target   *dispatcherTarget
		model    string
		resolved transcode.ModelMapping
		err      error
	}
	var claims []claim
	for i := range d.targets {
		target := &d.targets[i]
		pModel, _, ok := target.profileMap.ResolveProfile(clientModel)
		if !ok {
			continue
		}
		resolved, err := target.modelMap.Resolve(pModel)
		claims = append(claims, claim{target: target, model: pModel, resolved: resolved, err: err})
	}

	if len(claims) > 0 {
		for _, c := range claims {
			if c.err == nil && c.resolved.Via != "" {
				return targetSelection{via: c.resolved.Via, handler: c.target.handler, profileModel: c.model, target: c.target}
			}
		}
		for _, c := range claims {
			for i := range d.targets {
				target := &d.targets[i]
				if resolved, err := target.modelMap.Resolve(c.model); err == nil && resolved.Via != "" {
					return targetSelection{via: resolved.Via, handler: c.target.handler, profileModel: c.model, target: c.target}
				}
			}
		}
		for _, c := range claims {
			if c.err == nil && c.target.chat {
				return targetSelection{handler: c.target.handler, profileModel: c.model, target: c.target}
			}
		}
		for _, c := range claims {
			if c.err == nil {
				return targetSelection{handler: c.target.handler, profileModel: c.model, target: c.target}
			}
		}
		for _, c := range claims {
			if c.target.chat {
				return targetSelection{handler: c.target.handler, profileModel: c.model, target: c.target}
			}
		}
		first := claims[0]
		return targetSelection{handler: first.target.handler, profileModel: first.model, target: first.target}
	}

	for i := range d.targets {
		target := &d.targets[i]
		if resolved, err := target.modelMap.Resolve(clientModel); err == nil && resolved.Via != "" {
			return targetSelection{
				via:     resolved.Via,
				handler: target.handler,
				target:  target,
			}
		}
	}
	return targetSelection{}
}

// routeConflict reports whether the route can decide clientModel's dialect
// without a registration order choosing it.
//
// For a profile name, each claimant reaches a dialect through the explicit
// dialects its target model carries. A target model with conflicting explicit
// dialects is undecidable by itself. Across the claimants, the decisions must
// name one dialect and one target model, and the claimants that decide them
// must agree on the reasoning tier. Claimants that carry no explicit dialect
// fall back to identity resolution and never conflict with a decision. A raw
// model name is undecidable when its explicit dialects conflict. Every refusal
// names the sorted disagreement, so it is identical in every registration
// order.
func (d *multiTranscodeDispatcher) routeConflict(clientModel string) (string, bool) {
	type decision struct {
		dialect transcode.NativeProtocol
		model   string
		tier    string
	}
	var decisions []decision
	claimed := false
	for i := range d.targets {
		model, tier, ok := d.targets[i].profileMap.ResolveProfile(clientModel)
		if !ok {
			continue
		}
		claimed = true
		dialects := d.explicitDialects(model)
		if len(dialects) > 1 {
			return fmt.Sprintf(
				"%q is a -transcode-profile whose target model %q is mapped to conflicting dialects (%s) by the mappings on %s, so no transcode mapping on this route can serve it",
				clientModel, model, joinDialects(dialects), d.routeKey.Path,
			), false
		}
		for _, dialect := range dialects {
			decisions = append(decisions, decision{dialect: dialect, model: model, tier: tier})
		}
	}
	if !claimed {
		if dialects := d.explicitDialects(clientModel); len(dialects) > 1 {
			return fmt.Sprintf(
				"model %q is mapped to conflicting dialects (%s) by the mappings on %s, so no transcode mapping on this route can serve it",
				clientModel, joinDialects(dialects), d.routeKey.Path,
			), false
		}
		return "", true
	}
	if len(decisions) == 0 {
		return "", true
	}
	slices.SortFunc(decisions, func(a, b decision) int {
		if c := strings.Compare(string(a.dialect), string(b.dialect)); c != 0 {
			return c
		}
		return strings.Compare(a.model, b.model)
	})
	var pairs []string
	var tiers []string
	for i, dec := range decisions {
		if i == 0 || dec.dialect != decisions[i-1].dialect || dec.model != decisions[i-1].model {
			pairs = append(pairs, fmt.Sprintf("%s via %s", dec.model, dec.dialect))
		}
		if !slices.Contains(tiers, dec.tier) {
			tiers = append(tiers, dec.tier)
		}
	}
	if len(pairs) > 1 {
		return fmt.Sprintf(
			"%q is a -transcode-profile that the mappings on %s decide differently (%s), so no transcode mapping on this route can serve it",
			clientModel, d.routeKey.Path, strings.Join(pairs, " and "),
		), false
	}
	if len(tiers) > 1 {
		slices.Sort(tiers)
		for i, tier := range tiers {
			if tier == "" {
				tiers[i] = "none"
			}
		}
		return fmt.Sprintf(
			"%q is a -transcode-profile whose target model %q is defined with conflicting reasoning tiers (%s) by the mappings on %s, so no transcode mapping on this route can serve it",
			clientModel, decisions[0].model, strings.Join(tiers, " and "), d.routeKey.Path,
		), false
	}
	return "", true
}

// explicitDialects returns the distinct explicit dialects that route targets
// declare for model, sorted. Identity resolution declares none.
func (d *multiTranscodeDispatcher) explicitDialects(model string) []transcode.NativeProtocol {
	var dialects []transcode.NativeProtocol
	for i := range d.targets {
		resolved, err := d.targets[i].modelMap.Resolve(model)
		if err == nil && resolved.Via != "" && !slices.Contains(dialects, resolved.Via) {
			dialects = append(dialects, resolved.Via)
		}
	}
	slices.Sort(dialects)
	return dialects
}

// joinDialects renders explicit dialects for an error message.
func joinDialects(dialects []transcode.NativeProtocol) string {
	parts := make([]string, 0, len(dialects))
	for _, dialect := range dialects {
		parts = append(parts, string(dialect))
	}
	return strings.Join(parts, " and ")
}

// profileDialectRefusal checks that a profile answer naming a dialect can be
// served by that dialect's handler, i.e. that its mapping defines the
// profile with the identical target model and does not map that target model to
// a conflicting dialect. When the route lacks a handler for that dialect
// (idx < 0) or the handler cannot serve the profile, the returned refusal
// names it.
func (d *multiTranscodeDispatcher) profileDialectRefusal(clientModel string, sel targetSelection) (string, bool) {
	idx := d.chatTarget
	if sel.via == transcode.NativeResponses {
		idx = d.responsesTarget
	}
	if idx < 0 {
		return fmt.Sprintf(
			"%q is a -transcode-profile whose target model %q is served as %s, but this route has no %s mapping, so no transcode mapping on this route can serve it",
			clientModel, sel.profileModel, sel.via, sel.via,
		), false
	}
	served := &d.targets[idx]
	pm, _, ok := served.profileMap.ResolveProfile(clientModel)
	if !ok || pm != sel.profileModel {
		return fmt.Sprintf(
			"%q is a -transcode-profile whose target model %q is served as %s, but the %s mapping on %s does not define the profile, so no transcode mapping on this route can serve it",
			clientModel, sel.profileModel, sel.via, sel.via, d.routeKey.Path,
		), false
	}
	res, err := served.modelMap.Resolve(sel.profileModel)
	if err != nil {
		return fmt.Sprintf(
			"%q is a -transcode-profile whose target model %q is served as %s, but the %s mapping on %s does not map target model %q, so no transcode mapping on this route can serve it",
			clientModel, sel.profileModel, sel.via, sel.via, d.routeKey.Path, sel.profileModel,
		), false
	}
	if res.Via != "" && res.Via != sel.via {
		return fmt.Sprintf(
			"%q is a -transcode-profile whose target model %q is served as %s, but the %s mapping on %s does not map target model %q to %s",
			clientModel, sel.profileModel, sel.via, sel.via, d.routeKey.Path, sel.profileModel, sel.via,
		), false
	}
	return "", true
}

// messagesNativeRefusal words the 404 for a messages-native model so its
// remedy is one the operator can apply. A path carries at most one native
// route, and a native miss proves this one has it: adding another would be a
// duplicate-route startup error. Native serving resolves model names only,
// never -transcode-profile names, so a profile is pointed at its target model.
//
// The wording follows what decided the route: a profile claim (sel carries its
// target model) gets the profile remedy; a model the native route or a model
// map resolved gets the model remedy.
func (d *multiTranscodeDispatcher) messagesNativeRefusal(clientModel string, sel targetSelection, miss nativeMissInfo, missed bool) string {
	if profileModel := sel.profileModel; profileModel != "" {
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
