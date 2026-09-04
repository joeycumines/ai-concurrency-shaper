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
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const httpMethodPost = "POST"

// maxSecretFileBytes bounds a secret file read at 64 KiB with one-byte
// overflow detection: an oversized file is a construction error, never an
// unbounded read.
const maxSecretFileBytes = 64 << 10

// envSecretSource reads a secret from an environment variable.
type envSecretSource string

func (s envSecretSource) Secret(context.Context) (string, error) {
	value, ok := os.LookupEnv(string(s))
	if !ok {
		return "", fmt.Errorf("environment variable %s is not set", string(s))
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("environment variable %s is empty", string(s))
	}
	return strings.TrimSpace(value), nil
}

// fileSecretSource reads a secret from a file path. The file's trailing
// newline is trimmed.
type fileSecretSource string

func (s fileSecretSource) Secret(_ context.Context) (string, error) {
	if string(s) == "" {
		return "", fmt.Errorf("file secret source has no path")
	}
	file, err := os.Open(string(s))
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	if len(data) > maxSecretFileBytes {
		return "", fmt.Errorf(
			"secret file %q exceeds the %d byte bound",
			string(s),
			maxSecretFileBytes,
		)
	}
	val := strings.TrimSpace(string(data))
	if val == "" {
		return "", fmt.Errorf("secret file %s is empty", string(s))
	}
	return val, nil
}

// secretSource resolves an env:NAME or file:PATH source.
func secretSource(source string) (transcode.SecretSource, error) {
	kind, name, ok := strings.Cut(source, ":")
	if !ok {
		return nil, fmt.Errorf(
			"invalid -transcode-auth-source %q: want inbound, env:NAME, or file:PATH",
			source,
		)
	}
	switch kind {
	case "env":
		if name == "" {
			return nil, fmt.Errorf("invalid -transcode-auth-source %q: env name is empty", source)
		}
		return envSecretSource(name), nil
	case "file":
		if name == "" {
			return nil, fmt.Errorf("invalid -transcode-auth-source %q: file path is empty", source)
		}
		return fileSecretSource(name), nil
	default:
		return nil, fmt.Errorf(
			"invalid -transcode-auth-source %q: want inbound, env:NAME, or file:PATH",
			source,
		)
	}
}

// parseTranscodeEndpoint splits "protocol@path" and validates the protocol
// name. The path may itself contain '@' (e.g. /v1/models@predict), so the
// first '@' separates the protocol.
func parseTranscodeEndpoint(
	value string,
	upstream bool,
) (clientProtocol transcode.ClientProtocol, upstreamProtocol transcode.UpstreamProtocol, path string, err error) {
	at := strings.Index(value, "@")
	if at <= 0 || at == len(value)-1 {
		return "", "", "", fmt.Errorf(
			"want protocol@path, got %q",
			value,
		)
	}
	protocolName := value[:at]
	path = value[at+1:]
	if !strings.HasPrefix(path, "/") {
		return "", "", "", fmt.Errorf("path %q must be absolute", path)
	}

	if upstream {
		switch transcode.UpstreamProtocol(protocolName) {
		case transcode.UpstreamResponses,
			transcode.UpstreamChatCompletions:
			return "", transcode.UpstreamProtocol(protocolName), path, nil
		}
		return "", "", "", fmt.Errorf(
			"unknown upstream protocol %q (want responses or chat-completions)",
			protocolName,
		)
	}

	switch transcode.ClientProtocol(protocolName) {
	case transcode.ClientResponses, transcode.ClientMessages:
		return transcode.ClientProtocol(protocolName), "", path, nil
	case transcode.ClientProtocol(transcode.UpstreamChatCompletions):
		return "", "", "", fmt.Errorf(
			"chat-completions is not a valid client protocol; it is upstream-only",
		)
	}
	return "", "", "", fmt.Errorf(
		"unknown client protocol %q (want responses or messages)",
		protocolName,
	)
}

// parseTranscodeRoute parses one -transcode-route value. Chat Completions is
// rejected as a client protocol at parse time (chat is upstream-only).
func parseTranscodeRoute(value string) (proxy.TranscodeMapping, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return proxy.TranscodeMapping{}, fmt.Errorf(
			"invalid transcode route %q: want clientProtocol@clientPath=upstreamProtocol@upstreamPath",
			value,
		)
	}

	clientProtocol, _, clientPath, err := parseTranscodeEndpoint(parts[0], false)
	if err != nil {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: %w", value, err)
	}
	_, upstreamProtocol, upstreamPath, err := parseTranscodeEndpoint(parts[1], true)
	if err != nil {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: %w", value, err)
	}

	routeKey, err := transcode.NewRouteKey(httpMethodPost, clientPath)
	if err != nil {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: %w", value, err)
	}

	mapping := transcode.Mapping{
		ClientRoute:      routeKey,
		ClientProtocol:   clientProtocol,
		UpstreamProtocol: upstreamProtocol,
		UpstreamPath:     upstreamPath,
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
	}
	if err := provisionalStrictnessLoss(mapping).Validate(); err != nil {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: %w", value, err)
	}
	return proxy.TranscodeMapping{Mapping: mapping}, nil
}

// provisionalStrictnessLoss is applied to a route mapping during PARSING
// only: the CLI's real loss policy is applied later by
// buildTranscodeMappings, which re-runs the strictness rejection against
// the final policy.
func provisionalStrictnessLoss(m transcode.Mapping) transcode.Mapping {
	if m.ClientProtocol == transcode.ClientMessages &&
		m.UpstreamProtocol == transcode.UpstreamResponses {
		m.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureToolSchemaStrictness: {},
		}}
	}
	return m
}

// parseTranscodeModelMap builds the ModelMap from repeated -transcode-model
// values. With no mappings, identity fallback is used.
func parseTranscodeModelMap(values []string) (transcode.ModelMap, error) {
	exact := make(map[string]transcode.ModelMapping)
	for _, value := range values {
		client, upstream, ok := strings.Cut(value, "=")
		if !ok || client == "" || upstream == "" {
			return transcode.ModelMap{}, fmt.Errorf(
				"invalid -transcode-model %q: want client-model=upstream-model",
				value,
			)
		}
		if _, dup := exact[client]; dup {
			return transcode.ModelMap{}, fmt.Errorf(
				"duplicate -transcode-model client model %q",
				client,
			)
		}
		exact[client] = transcode.ModelMapping{
			ClientModel:         client,
			UpstreamModel:       upstream,
			ClientResponseModel: client,
		}
	}
	return transcode.ModelMap{
		Exact:         exact,
		AllowIdentity: len(exact) == 0,
	}, nil
}

// parseTranscodeAuth builds the AuthPolicy from the CLI contract.
func parseTranscodeAuth(
	mode, source, customHeader, anthropicVersion string,
) (transcode.AuthPolicy, error) {
	policy := transcode.AuthPolicy{
		Mode:             transcode.AuthMode(mode),
		CustomHeader:     customHeader,
		AnthropicVersion: anthropicVersion,
	}

	if policy.Mode == transcode.AuthNone {
		return transcode.AuthPolicy{Mode: transcode.AuthNone}, nil
	}

	switch source {
	case "", "inbound":
		policy.Inbound = true
	default:
		secret, err := secretSource(source)
		if err != nil {
			return transcode.AuthPolicy{}, err
		}
		policy.Secret = secret
	}

	return policy, nil
}

// chatCapabilityNames is the canonical CLI vocabulary for
// -transcode-chat-capability.
var chatCapabilityNames = []struct {
	name  string
	field func(*transcode.ChatCapabilities) *bool
}{
	{"developer_role", func(c *transcode.ChatCapabilities) *bool { return &c.DeveloperRole }},
	{"image_input", func(c *transcode.ChatCapabilities) *bool { return &c.ImageInput }},
	{"structured_outputs", func(c *transcode.ChatCapabilities) *bool { return &c.StructuredOutputs }},
	{"parallel_tool_calls", func(c *transcode.ChatCapabilities) *bool { return &c.ParallelToolCalls }},
	{"stop_sequences", func(c *transcode.ChatCapabilities) *bool { return &c.StopSequences }},
	{"reasoning_effort", func(c *transcode.ChatCapabilities) *bool { return &c.ReasoningEffort }},
	{"provider_reasoning_text", func(c *transcode.ChatCapabilities) *bool { return &c.ProviderReasoningText }},
	{"system_anywhere", func(c *transcode.ChatCapabilities) *bool { return &c.SystemAnywhere }},
}

func splitFlagNegations(
	values []string,
	validate func(name string) error,
) (positives []string, negated map[string]struct{}, err error) {
	positiveSet := make(map[string]struct{})
	negated = make(map[string]struct{})
	for _, value := range values {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ' '
		}) {
			name, negative := strings.CutPrefix(part, "!")
			if name == "" {
				return nil, nil, fmt.Errorf("empty name in %q", part)
			}
			if err := validate(name); err != nil {
				return nil, nil, err
			}
			if negative {
				negated[name] = struct{}{}
			} else {
				positiveSet[name] = struct{}{}
			}
		}
	}
	for name := range negated {
		if _, ok := positiveSet[name]; ok {
			return nil, nil, fmt.Errorf(
				"conflicting values for %q: given both positively and negated (!%s)",
				name, name,
			)
		}
	}
	positives = make([]string, 0, len(positiveSet))
	for name := range positiveSet {
		positives = append(positives, name)
	}
	sort.Strings(positives)
	return positives, negated, nil
}

func parseChatCapabilities(
	values []string,
) (transcode.ChatCapabilities, map[string]struct{}, error) {
	validate := func(name string) error {
		for _, entry := range chatCapabilityNames {
			if entry.name == name {
				return nil
			}
		}
		return fmt.Errorf("unknown chat capability %q", name)
	}
	positives, negated, err := splitFlagNegations(values, validate)
	if err != nil {
		return transcode.ChatCapabilities{}, nil, err
	}
	var out transcode.ChatCapabilities
	for _, name := range positives {
		for _, entry := range chatCapabilityNames {
			if entry.name == name {
				*entry.field(&out) = true
				break
			}
		}
	}
	return out, negated, nil
}

func parseNegatedLosses(
	values ...string,
) (map[transcode.Feature]struct{}, map[transcode.Feature]struct{}, error) {
	validate := func(name string) error {
		_, err := transcode.ParseLossFeatures(name)
		return err
	}
	positives, negatedNames, err := splitFlagNegations(values, validate)
	if err != nil {
		return nil, nil, err
	}
	allowed, err := transcode.ParseLossFeatures(positives...)
	if err != nil {
		return nil, nil, err
	}
	negated := make(map[transcode.Feature]struct{}, len(negatedNames))
	for name := range negatedNames {
		negated[transcode.Feature(name)] = struct{}{}
	}
	return allowed, negated, nil
}

func validClientQueryName(name string) error {
	if name == "" {
		return fmt.Errorf("empty client query parameter name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x21 || c == 0x7f || strings.ContainsRune("=%&#?", rune(c)) {
			return fmt.Errorf(
				"invalid client query parameter name %q",
				name,
			)
		}
	}
	return nil
}

func parseClientQuery(
	values []string,
) (map[string]struct{}, map[string]struct{}, error) {
	positives, negated, err := splitFlagNegations(values, validClientQueryName)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]struct{}, len(positives))
	for _, name := range positives {
		out[name] = struct{}{}
	}
	return out, negated, nil
}

var defaultTranscodeChatCapabilities = transcode.ChatCapabilities{
	DeveloperRole:         false,
	ParallelToolCalls:     true,
	ReasoningEffort:       false,
	ProviderReasoningText: true,
}

var defaultTranscodeAllowedQuery = map[string]struct{}{
	"beta": {},
}

var defaultTranscodeLosses = map[transcode.Feature]struct{}{
	transcode.FeatureReasoningSummary:       {},
	transcode.FeatureAuthenticatedThinking:  {},
	transcode.FeatureMidConversationSystem:  {},
	transcode.FeatureResponsesControls:      {},
	transcode.FeatureAnthropicControls:      {},
	transcode.FeatureBuiltinTools:           {},
	transcode.FeatureUsageUnknown:           {},
	transcode.FeatureUsageCacheReadUnknown:  {},
	transcode.FeatureUsageCacheWriteUnknown: {},
	transcode.FeatureUsageReasoningUnknown:  {},
	transcode.FeatureRequestReasoning:       {},
	transcode.FeatureDeveloperRole:          {},
}

func mergedLossPolicy(
	lossPolicy transcode.LossPolicy,
	negated map[transcode.Feature]struct{},
	strictDefaults bool,
) transcode.LossPolicy {
	allowed := make(
		map[transcode.Feature]struct{},
		len(defaultTranscodeLosses)+len(lossPolicy.Allowed),
	)
	if !strictDefaults {
		for feature := range defaultTranscodeLosses {
			allowed[feature] = struct{}{}
		}
	}
	for feature := range lossPolicy.Allowed {
		allowed[feature] = struct{}{}
	}
	for feature := range negated {
		if _, positive := lossPolicy.Allowed[feature]; !positive {
			delete(allowed, feature)
		}
	}
	return transcode.LossPolicy{Allowed: allowed}
}

func mergedChatCapabilities(
	capabilities transcode.ChatCapabilities,
	negated map[string]struct{},
	strictDefaults bool,
) transcode.ChatCapabilities {
	out := transcode.ChatCapabilities{}
	if !strictDefaults {
		out = defaultTranscodeChatCapabilities
	}
	fields := map[string]*bool{
		"developer_role":          &out.DeveloperRole,
		"image_input":             &out.ImageInput,
		"structured_outputs":      &out.StructuredOutputs,
		"parallel_tool_calls":     &out.ParallelToolCalls,
		"stop_sequences":          &out.StopSequences,
		"reasoning_effort":        &out.ReasoningEffort,
		"provider_reasoning_text": &out.ProviderReasoningText,
		"system_anywhere":         &out.SystemAnywhere,
	}
	cli := map[string]bool{
		"developer_role":          capabilities.DeveloperRole,
		"image_input":             capabilities.ImageInput,
		"structured_outputs":      capabilities.StructuredOutputs,
		"parallel_tool_calls":     capabilities.ParallelToolCalls,
		"stop_sequences":          capabilities.StopSequences,
		"reasoning_effort":        capabilities.ReasoningEffort,
		"provider_reasoning_text": capabilities.ProviderReasoningText,
		"system_anywhere":         capabilities.SystemAnywhere,
	}
	for name, field := range fields {
		_, deny := negated[name]
		*field = (*field || cli[name]) && !(deny && !cli[name])
	}
	return out
}

func mergedAllowedQuery(
	clientQuery map[string]struct{},
	negated map[string]struct{},
	strictDefaults bool,
) map[string]struct{} {
	out := make(map[string]struct{}, len(defaultTranscodeAllowedQuery)+len(clientQuery))
	if !strictDefaults {
		for name := range defaultTranscodeAllowedQuery {
			out[name] = struct{}{}
		}
	}
	for name := range clientQuery {
		out[name] = struct{}{}
	}
	for name := range negated {
		if _, positive := clientQuery[name]; !positive {
			delete(out, name)
		}
	}
	return out
}

type transcodeCLIOptions struct {
	lossPolicy          transcode.LossPolicy
	negatedLosses       map[transcode.Feature]struct{}
	capabilities        transcode.ChatCapabilities
	negatedCapabilities map[string]struct{}
	clientQuery         map[string]struct{}
	negatedQuery        map[string]struct{}
	strictDefaults      bool
}

func buildTranscodeMappings(
	routes []proxy.TranscodeMapping,
	responsesChat, messagesChat, messagesResponses bool,
	opts transcodeCLIOptions,
) ([]proxy.TranscodeMapping, error) {
	if messagesChat && messagesResponses {
		return nil, fmt.Errorf(
			"-transcode-messages-chat and -transcode-messages-responses both map /v1/messages; enable only one",
		)
	}

	lossPolicy := mergedLossPolicy(opts.lossPolicy, opts.negatedLosses, opts.strictDefaults)
	capabilities := mergedChatCapabilities(opts.capabilities, opts.negatedCapabilities, opts.strictDefaults)
	allowedQuery := mergedAllowedQuery(opts.clientQuery, opts.negatedQuery, opts.strictDefaults)

	lossMapping := func(m transcode.Mapping) proxy.TranscodeMapping {
		m.LossPolicy = lossPolicy
		m.ChatCapabilities = capabilities
		m.AllowedClientQuery = allowedQuery
		return proxy.TranscodeMapping{Mapping: m}
	}

	var mappings []proxy.TranscodeMapping
	for _, route := range routes {
		route.Mapping.LossPolicy = lossPolicy
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
	return mappings, nil
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
func (p *Provider) resolveTranscode() error {
	if err := validateMBFlag("-transcode-max-request-mb", p.TranscodeMaxRequestMB, 20); err != nil {
		return err
	}
	if err := validateMBFlag("-transcode-max-response-mb", p.TranscodeMaxResponseMB, 20); err != nil {
		return err
	}

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
	if p.TranscodeAuth != "" || p.TranscodeAuthSource != "" || p.TranscodeAuthHeader != "" {
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
		hasExplicitTranscodeAuth = true
	} else if p.authPolicy != nil {
		authPolicy = transcode.FromProviderAuth(p.authPolicy)
	} else {
		authPolicy = transcode.AuthPolicy{Mode: transcode.AuthNone}
	}

	modelMap, err := parseTranscodeModelMap(p.TranscodeModelMap)
	if err != nil {
		return err
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
	seen := make(map[transcode.RouteKey]struct{})
	for i := range mappings {
		if _, dup := seen[mappings[i].ClientRoute]; dup {
			return fmt.Errorf(
				"duplicate transcode mapping for client route %s %s",
				mappings[i].ClientRoute.Method,
				mappings[i].ClientRoute.Path,
			)
		}
		seen[mappings[i].ClientRoute] = struct{}{}

		if len(modelMap.Exact) > 0 || !modelMap.AllowIdentity {
			mappings[i].Mapping.ModelMap = modelMap
		}
		if hasExplicitTranscodeAuth || p.authPolicy != nil {
			mappings[i].Mapping.Auth = authPolicy
		} else if mappings[i].Mapping.Auth.IsZero() {
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

		if err := mappings[i].Mapping.Validate(); err != nil {
			return fmt.Errorf("transcode mapping %s %s: %w", mappings[i].ClientRoute.Method, mappings[i].ClientRoute.Path, err)
		}
	}

	p.transcodeMappings = mappings
	return nil
}
