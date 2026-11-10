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
	"context"
	"fmt"
	"log"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const httpMethodPost = "POST"

func buildTranscodeMappings(
	routes []proxy.TranscodeMapping,
	responsesChat, messagesChat, messagesResponses bool,
	opts transcodeCLIOptions,
) ([]proxy.TranscodeMapping, error) {
	lossPolicy := mergedLossPolicy(opts.lossPolicy, opts.negatedLosses, opts.strictDefaults)
	capabilities := mergedChatCapabilities(opts.capabilities, opts.negatedCapabilities, opts.strictDefaults)
	allowedQuery := mergedAllowedQuery(opts.clientQuery, opts.negatedQuery, opts.strictDefaults)

	// mappingLossPolicy derives one mapping's effective loss policy from the
	// merged CLI policy: a fresh copy plus the directional injections, unless
	// defaults are strict or the operator negated the feature. The injections
	// are what make enabling a direction work out of the box, and they apply
	// to EVERY spelling of the direction, preset and explicit route alike, so
	// the two cannot drift:
	//   - Messages->Responses and Gemini->Responses: Messages/Gemini tools
	//     carry no strictness semantic and the Responses function-tool
	//     contract demands explicit strict, so strict:false is emitted under
	//     tool_schema_strictness.
	//   - Any mapping whose upstream is Gemini: metadata (Claude Code always
	//     sends metadata.user_id and the Messages decoder always fills
	//     CanonicalRequest.Metadata) and parallel_tool_calls (Codex commonly
	//     sends parallel_tool_calls: false), which a Gemini request cannot
	//     reproduce.
	mappingLossPolicy := func(m transcode.Mapping) transcode.LossPolicy {
		lp := transcode.LossPolicy{Allowed: make(map[transcode.Feature]struct{}, len(lossPolicy.Allowed)+3)}
		maps.Copy(lp.Allowed, lossPolicy.Allowed)
		if !opts.strictDefaults && (m.ClientProtocol == transcode.ClientMessages || m.ClientProtocol == transcode.ClientGemini) && m.UpstreamProtocol == transcode.UpstreamResponses {
			if _, negated := opts.negatedLosses[transcode.FeatureToolSchemaStrictness]; !negated {
				lp.Allowed[transcode.FeatureToolSchemaStrictness] = struct{}{}
			}
		}
		if !opts.strictDefaults && m.UpstreamProtocol == transcode.UpstreamGemini {
			for _, feature := range []transcode.Feature{transcode.FeatureMetadata, transcode.FeatureParallelToolCalls} {
				if _, negated := opts.negatedLosses[feature]; !negated {
					lp.Allowed[feature] = struct{}{}
				}
			}
		}
		return lp
	}

	lossMapping := func(m transcode.Mapping) proxy.TranscodeMapping {
		m.LossPolicy = mappingLossPolicy(m)
		m.ChatCapabilities = capabilities
		m.AllowedClientQuery = allowedQuery
		return proxy.TranscodeMapping{Mapping: m}
	}

	var mappings []proxy.TranscodeMapping
	for _, route := range routes {
		route.Mapping.LossPolicy = mappingLossPolicy(route.Mapping)
		route.Mapping.ChatCapabilities = capabilities
		route.Mapping.AllowedClientQuery = allowedQuery
		if err := route.Mapping.Validate(); err != nil {
			return nil, fmt.Errorf(
				"invalid transcode route %s %s: %w",
				route.ClientRoute.Method,
				route.ClientRoute.Path,
				err,
			)
		}
		mappings = append(mappings, route)
	}
	if responsesChat {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/responses"),
			ClientProtocol:   transcode.ClientResponses,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if messagesChat {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/messages"),
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if messagesResponses {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/messages"),
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if opts.geminiPresets.GeminiChat {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, geminiGenerateRoute),
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if opts.geminiPresets.GeminiResponses {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, geminiGenerateRoute),
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if opts.geminiPresets.GeminiMessages {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, geminiGenerateRoute),
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamMessages,
			UpstreamPath:     "/v1/messages",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if opts.geminiPresets.ResponsesGemini {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/responses"),
			ClientProtocol:   transcode.ClientResponses,
			UpstreamProtocol: transcode.UpstreamGemini,
			UpstreamPath:     "/v1beta",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	if opts.geminiPresets.MessagesGemini {
		mappings = append(mappings, lossMapping(transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/messages"),
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamGemini,
			UpstreamPath:     "/v1beta",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}))
	}
	return mappings, nil
}

// sortedNativeProtocols returns the keys of a dialect-keyed map in sorted
// order, so startup refusals are deterministic.
func sortedNativeProtocols[V any](m map[transcode.NativeProtocol]V) []transcode.NativeProtocol {
	out := make([]transcode.NativeProtocol, 0, len(m))
	for dialect := range m {
		out = append(out, dialect)
	}
	slices.Sort(out)
	return out
}

func mustRouteKey(method, path string) transcode.RouteKey {
	key, err := transcode.NewRouteKey(method, path)
	if err != nil {
		panic(err)
	}
	return key
}

func validateMBFlag(name string, value int64, shift uint) error {
	if value < 0 {
		return fmt.Errorf("%s must be nonnegative, got %d", name, value)
	}
	if shift >= 63 || value > int64(math.MaxInt64)>>uint(shift) {
		return fmt.Errorf("%s is too large to convert to bytes: %d MiB", name, value)
	}
	return nil
}

// resolveTranscode resolves and validates all transcode configuration for a Provider.
// modelTable is this provider's frozen subset of the global -model-table: nil or
// empty means the table is not configured and the provider-scope -transcode-model
// values are the only model-mapping source.
func (p *Provider) resolveTranscode(modelTable []modelTableEntry) error {
	if err := validateMBFlag("-transcode-max-request-mb", p.TranscodeMaxRequestMB, 20); err != nil {
		return err
	}
	if err := validateMBFlag("-transcode-max-response-mb", p.TranscodeMaxResponseMB, 20); err != nil {
		return err
	}
	if p.TranscodeContinuityCap < 0 {
		return fmt.Errorf("-transcode-continuity-capacity must be nonnegative, got %d", p.TranscodeContinuityCap)
	}
	if p.TranscodeContinuityTTL < 0 {
		return fmt.Errorf("-transcode-continuity-ttl must be nonnegative, got %s", p.TranscodeContinuityTTL)
	}
	if p.TranscodeSignatureCap < 0 {
		return fmt.Errorf("-transcode-gemini-signature-capacity must be nonnegative, got %d", p.TranscodeSignatureCap)
	}
	if p.TranscodeSignatureTTL < 0 {
		return fmt.Errorf("-transcode-gemini-signature-ttl must be nonnegative, got %s", p.TranscodeSignatureTTL)
	}
	// The continuity store is shared by this resolve's mappings only: one an
	// earlier resolve built reflects its capacity and TTL, not the current
	// ones.
	p.continuityStore = nil
	// The signature store is likewise rebuilt per resolve; it is created only
	// when a mapping targets a Gemini upstream (below).
	p.geminiSignatureStore = nil

	var parsedRoutes []proxy.TranscodeMapping
	for _, r := range p.TranscodeRoutes {
		tm, err := parseTranscodeRoute(r)
		if err != nil {
			return err
		}
		parsedRoutes = append(parsedRoutes, tm)
	}

	var authPolicy transcode.AuthPolicy
	var hasExplicitTranscodeAuth bool
	var authFromProvider bool
	if p.TranscodeAuth != "" || p.TranscodeAuthSource != "" || p.TranscodeAuthHeader != "" {
		if p.TranscodeAuthSource == "provider" {
			// Explicit opt-in to provider-auth inheritance, distinct from
			// the implicit inheritance below so -transcode-auth-source=inbound
			// never silently forwards a client credential.
			if p.TranscodeAuth != "" || p.TranscodeAuthHeader != "" {
				return fmt.Errorf("-transcode-auth-source provider cannot be combined with -transcode-auth or -transcode-auth-header")
			}
			if p.authPolicy == nil {
				return fmt.Errorf("-transcode-auth-source provider requires a configured -auth-source on the provider")
			}
			authFromProvider = true
		} else {
			var err error
			authPolicy, err = parseTranscodeAuth(
				p.TranscodeAuth,
				p.TranscodeAuthSource,
				p.TranscodeAuthHeader,
				p.TranscodeAnthropicVersion,
			)
			if err != nil {
				return err
			}
			if authPolicy.Secret != nil {
				val, err := authPolicy.Secret.Secret(context.Background())
				if err != nil {
					return fmt.Errorf("-transcode-auth-source %s: %w", p.TranscodeAuthSource, err)
				}
				authPolicy.Secret = auth.NewStaticSecretSource(strings.TrimSpace(val))
			}
		}
		hasExplicitTranscodeAuth = true
	} else if p.authPolicy != nil {
		authFromProvider = true
	} else {
		authPolicy = transcode.AuthPolicy{Mode: transcode.AuthNone}
	}

	modelMap, err := parseTranscodeModelMap(p.TranscodeModelMap)
	if err != nil {
		return err
	}
	tableProjected := len(modelTable) > 0
	if tableProjected {
		modelMap = modelMapFromTable(modelTable)
	}

	profileMap, err := parseTranscodeProfiles(p.TranscodeProfiles)
	if err != nil {
		return err
	}
	if len(profileMap.Profiles) > 0 && (tableProjected || len(modelMap.Exact) > 0 || !modelMap.AllowIdentity || modelMap.RequireExplicitMap) {
		for name, prof := range profileMap.Profiles {
			// A profile is resolved BEFORE the model map, so a profile whose
			// name is also a mapped model would silently shadow that mapping
			// and the client would be told the mapped name while being served
			// the profile's target. That is a false identity, not a routing
			// choice, so it is rejected at startup where it is still cheap.
			if _, clash := modelMap.Exact[name]; clash {
				return fmt.Errorf("invalid -transcode-profile %q: profile name collides with a mapped model; rename the profile or the model", name)
			}
			if _, err := modelMap.Resolve(prof.Model); err != nil {
				return fmt.Errorf("invalid -transcode-profile %q: target model %q cannot be resolved: %w", name, prof.Model, err)
			}
		}
	}

	allowedLosses, negatedLosses, err := parseNegatedLosses(p.TranscodeAllowLosses...)
	if err != nil {
		return err
	}

	chatCaps, negatedCaps, err := parseChatCapabilities(p.TranscodeChatCapabilities)
	if err != nil {
		return err
	}

	allowedQuery, negatedQuery, err := parseClientQuery(p.TranscodeAllowClientQuery)
	if err != nil {
		return err
	}

	opts := transcodeCLIOptions{
		lossPolicy:          transcode.LossPolicy{Allowed: allowedLosses},
		negatedLosses:       negatedLosses,
		capabilities:        chatCaps,
		negatedCapabilities: negatedCaps,
		clientQuery:         allowedQuery,
		negatedQuery:        negatedQuery,
		strictDefaults:      p.TranscodeStrictDefaults,
		geminiPresets: geminiPresetFlags{
			GeminiChat:      p.TranscodeGeminiChat,
			GeminiResponses: p.TranscodeGeminiResponses,
			GeminiMessages:  p.TranscodeGeminiMessages,
			ResponsesGemini: p.TranscodeResponsesGemini,
			MessagesGemini:  p.TranscodeMessagesGemini,
		},
	}

	mappings, err := buildTranscodeMappings(
		parsedRoutes,
		p.TranscodeResponsesChat,
		p.TranscodeMessagesChat,
		p.TranscodeMessagesResponses,
		opts,
	)
	if err != nil {
		return err
	}

	// Apply model map, auth, body limits, and check duplicate client routes within this provider.
	type transcodeTargetKey struct {
		routeKey transcode.RouteKey
		upstream transcode.UpstreamProtocol
	}
	seen := make(map[transcodeTargetKey]struct{})
	// routeClientProtocol tracks the client dialect already claimed by each
	// client route: one route is one client dialect, and a second mapping in
	// a different dialect would parse requests and render errors in the
	// wrong shape behind one dispatcher.
	routeClientProtocol := make(map[transcode.RouteKey]transcode.ClientProtocol)
	// The preset is built and stamped before the mappings are validated:
	// the mapping's reserved-name rule consults Mapping.Opencode, so
	// validating first left that predicate dead and deferred the refusal to
	// proxy.New, where the operator saw a "proxy config:" prefix instead of
	// the config error.
	p.opencodePreset = transcode.OpencodePreset{
		Enabled:   p.Opencode,
		UserAgent: p.OpencodeUserAgent,
		Client:    p.OpencodeClient,
		Provider:  effectiveName(p),
	}
	for i := range mappings {
		mappings[i].Mapping.Opencode = p.opencodePreset
		targetKey := transcodeTargetKey{
			routeKey: mappings[i].ClientRoute,
			upstream: mappings[i].UpstreamProtocol,
		}
		if _, dup := seen[targetKey]; dup {
			return fmt.Errorf(
				"duplicate transcode mapping for client route %s %s (upstream %s)",
				mappings[i].ClientRoute.Method,
				mappings[i].ClientRoute.Path,
				mappings[i].UpstreamProtocol,
			)
		}
		seen[targetKey] = struct{}{}
		if claimed, ok := routeClientProtocol[mappings[i].ClientRoute]; ok && claimed != mappings[i].ClientProtocol {
			return fmt.Errorf(
				"transcode mappings for client route %s %s declare conflicting client protocols (%s vs %s)",
				mappings[i].ClientRoute.Method,
				mappings[i].ClientRoute.Path,
				claimed,
				mappings[i].ClientProtocol,
			)
		}
		routeClientProtocol[mappings[i].ClientRoute] = mappings[i].ClientProtocol

		if tableProjected {
			// The global table owns model resolution for every transcode-enabled
			// provider: the projected map is stamped unconditionally, identity
			// fallback is off, and unlisted surrogates fail as local client errors.
			mappings[i].Mapping.ModelMap = modelMap
		} else if len(modelMap.Exact) > 0 || !modelMap.AllowIdentity {
			mappings[i].Mapping.ModelMap = modelMap
		}
		if len(profileMap.Profiles) > 0 {
			mappings[i].Mapping.ProfileMap = profileMap
		}
		switch {
		case authFromProvider:
			// Inherited provider auth — implicit or -transcode-auth-source
			// provider — adapts to each mapping's upstream dialect, so a
			// model authenticates identically whether served natively or
			// transcoded. Explicit transcode auth (env:/file:/inbound) is
			// applied verbatim below: the operator's spelling wins.
			mappings[i].Mapping.Auth = transcode.FromProviderAuthForUpstream(p.authPolicy, mappings[i].UpstreamProtocol)
		case hasExplicitTranscodeAuth:
			mappings[i].Mapping.Auth = authPolicy
		case mappings[i].Mapping.Auth.IsZero():
			mappings[i].Mapping.Auth = authPolicy
		}
		if p.RetryMax != 0 && p.RetryMaxBodyMB > 0 && mappings[i].BodyLimits.RetryReplayBytes == 0 {
			mappings[i].BodyLimits.RetryReplayBytes = p.RetryMaxBodyMB << 20
		}
		if p.TranscodeMaxRequestMB > 0 {
			mappings[i].BodyLimits.DecodedRequestBytes = p.TranscodeMaxRequestMB << 20
		}
		if p.TranscodeMaxResponseMB > 0 {
			mappings[i].BodyLimits.SuccessfulResponseBytes = p.TranscodeMaxResponseMB << 20
		}
		mappings[i].FlowLogDir = p.TranscodeFlowLogDir

		if p.TranscodeContinuity {
			if p.continuityStore == nil {
				p.continuityStore = transcode.NewContinuityStore(transcode.ContinuityConfig{
					Capacity: p.TranscodeContinuityCap,
					TTL:      p.TranscodeContinuityTTL,
				})
				log.Printf(
					"transcode: continuity store enabled (capacity %d, ttl %s); previous_response_id resolves against retained chains, a miss degrades to the existing observable loss",
					p.continuityStore.Capacity(),
					p.continuityStore.TTL(),
				)
			}
			mappings[i].Continuity = p.continuityStore
			mappings[i].ContinuityKey = p.Name
		}

		if mappings[i].UpstreamProtocol == transcode.UpstreamGemini {
			if p.geminiSignatureStore == nil {
				p.geminiSignatureStore = transcode.NewGeminiSignatureStore(transcode.GeminiSignatureConfig{
					Capacity: p.TranscodeSignatureCap,
					TTL:      p.TranscodeSignatureTTL,
				})
				log.Printf(
					"transcode: gemini signature store enabled (capacity %d, ttl %s)",
					p.geminiSignatureStore.Capacity(),
					p.geminiSignatureStore.TTL(),
				)
			}
			mappings[i].GeminiSignatures = p.geminiSignatureStore
			mappings[i].GeminiSignatureScope = effectiveName(p)
		}

		if err := mappings[i].Mapping.Validate(); err != nil {
			return fmt.Errorf("transcode mapping %s %s: %w", mappings[i].ClientRoute.Method, mappings[i].ClientRoute.Path, err)
		}
	}

	// A client route carrying several transcode mappings is dispatched by the
	// request model's native dialect, so a target no model can select is dead
	// configuration: the operator's flags promise a route that cannot happen.
	// Any non-chat dialect is selected only by a model declaring via=<dialect>
	// (a -model-table fact; -transcode-model cannot carry one). The chat
	// target is also the dispatcher's fallback, so any model that resolves
	// with no declared dialect reaches it: a via-less map entry or identity
	// resolution counts, not only via=chat. Routes are checked in
	// configuration order, so the refusal is deterministic.
	type dialectReach struct {
		present    bool
		selectable bool
	}
	type routeReach struct {
		mappings int
		dialects map[transcode.NativeProtocol]*dialectReach
	}
	reach := make(map[transcode.RouteKey]*routeReach, len(mappings))
	var routeOrder []transcode.RouteKey
	upstreamDialect := func(p transcode.UpstreamProtocol) (transcode.NativeProtocol, bool) {
		switch p {
		case transcode.UpstreamChatCompletions:
			return transcode.NativeChat, true
		case transcode.UpstreamResponses:
			return transcode.NativeResponses, true
		case transcode.UpstreamMessages:
			return transcode.NativeMessages, true
		case transcode.UpstreamGemini:
			return transcode.NativeGemini, true
		default:
			return "", false
		}
	}
	for _, m := range mappings {
		r := reach[m.ClientRoute]
		if r == nil {
			r = &routeReach{dialects: map[transcode.NativeProtocol]*dialectReach{}}
			reach[m.ClientRoute] = r
			routeOrder = append(routeOrder, m.ClientRoute)
		}
		r.mappings++
		dialect, ok := upstreamDialect(m.UpstreamProtocol)
		if !ok {
			continue
		}
		d := r.dialects[dialect]
		if d == nil {
			d = &dialectReach{}
			r.dialects[dialect] = d
		}
		d.present = true
		modelMap := m.Mapping.ModelMap
		if dialect == transcode.NativeChat && modelMap.AllowIdentity && !modelMap.RequireExplicitMap {
			d.selectable = true
		}
		for _, mapping := range modelMap.Exact {
			switch {
			case dialect == transcode.NativeChat && (mapping.Via == "" || mapping.Via == transcode.NativeChat):
				d.selectable = true
			case mapping.Via == dialect:
				d.selectable = true
			}
		}
	}
	for _, routeKey := range routeOrder {
		r := reach[routeKey]
		if r.mappings < 2 {
			continue
		}
		var missing []string
		for _, dialect := range sortedNativeProtocols(r.dialects) {
			d := r.dialects[dialect]
			if d.present && !d.selectable {
				missing = append(missing, string(dialect))
			}
		}
		if len(missing) == 0 {
			continue
		}
		return fmt.Errorf(
			"transcode route %s %s carries %d mappings but no model can select the %s dialect, so that target is unreachable; declare it with -model-table (surrogate@provider=wire;via=%s) or configure a single mapping for the route",
			routeKey.Method, routeKey.Path, r.mappings, strings.Join(missing, " or "), missing[0],
		)
	}

	var nativeRoutes []proxy.NativeRoute
	nativeSeen := make(map[transcode.RouteKey]struct{})
	for _, raw := range p.NativeRouteFlags {
		nr, err := parseNativeRoute(raw)
		if err != nil {
			return err
		}
		// A native route MAY share its client route with a transcode
		// mapping: dispatch tries native first and falls through to the
		// mapping when the model's dialect differs, so one path can serve
		// both natively and by conversion.
		if _, dup := nativeSeen[nr.RouteKey]; dup {
			return fmt.Errorf(
				"duplicate native route for client route %s %s",
				nr.RouteKey.Method,
				nr.RouteKey.Path,
			)
		}
		nativeSeen[nr.RouteKey] = struct{}{}
		nativeRoutes = append(nativeRoutes, nr)
	}
	for i := range nativeRoutes {
		// Native routes share the provider's model resolution: the
		// projected table (with Via dialects) or the explicit map. With
		// neither, identity fallback applies and Via stays unknown, so
		// every request declines native and falls through.
		if tableProjected {
			nativeRoutes[i].ModelMap = modelMap
		} else if len(modelMap.Exact) > 0 || !modelMap.AllowIdentity {
			nativeRoutes[i].ModelMap = modelMap
		} else {
			nativeRoutes[i].ModelMap = transcode.ModelMap{AllowIdentity: true}
		}
		if p.TranscodeMaxRequestMB > 0 {
			// The native path reads the whole body to rewrite the model
			// identifier, so both its read cap and its rewritten-body cap
			// follow the provider's declared request bound.
			b := p.TranscodeMaxRequestMB << 20
			nativeRoutes[i].BodyLimits.DecodedRequestBytes = b
			nativeRoutes[i].BodyLimits.AcceptedRequestBytes = b
		}
		if p.TranscodeMaxResponseMB > 0 {
			nativeRoutes[i].BodyLimits.SuccessfulResponseBytes = p.TranscodeMaxResponseMB << 20
		}
		if err := nativeRoutes[i].Validate(); err != nil {
			return fmt.Errorf("native route %s %s: %w",
				nativeRoutes[i].RouteKey.Method, nativeRoutes[i].RouteKey.Path, err)
		}
	}

	p.transcodeMappings = mappings
	p.nativeRoutes = nativeRoutes
	for i := range p.nativeRoutes {
		p.nativeRoutes[i].Provider = effectiveName(p)
	}
	return nil
}
