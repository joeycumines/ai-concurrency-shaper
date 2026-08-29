package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// transcodeRouteFlags implements flag.Value for repeatable -transcode-route
// flags of the form clientProtocol@clientPath=upstreamProtocol@upstreamPath,
// e.g. responses@/v1/responses=chat-completions@/v1/chat/completions.
type transcodeRouteFlags []proxy.TranscodeMapping

func (f transcodeRouteFlags) String() string {
	var parts []string
	for _, m := range f {
		parts = append(parts, fmt.Sprintf(
			"%s@%s=%s@%s",
			m.ClientProtocol,
			m.ClientRoute.Path,
			m.UpstreamProtocol,
			m.UpstreamPath,
		))
	}
	return strings.Join(parts, ", ")
}

func (f *transcodeRouteFlags) Set(v string) error {
	m, err := parseTranscodeRoute(v)
	if err != nil {
		return err
	}
	*f = append(*f, m)
	return nil
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
// the final policy. Without this provisional allowance, a
// messages->responses route could never parse even when the operator
// approves tool_schema_strictness on the command line (review-z commit 6).
func provisionalStrictnessLoss(m transcode.Mapping) transcode.Mapping {
	if m.ClientProtocol == transcode.ClientMessages &&
		m.UpstreamProtocol == transcode.UpstreamResponses {
		m.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureToolSchemaStrictness: {},
		}}
	}
	return m
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
		// Messages is deliberately not accepted: no supported transcode
		// direction targets Messages, so advertising or accepting it would
		// defer a guaranteed startup failure to the first request
		// (review-j finding 14).
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

const httpMethodPost = "POST"

// transcodeCLIOptions carries the repeatable transcode feature flags through
// buildTranscodeMappings: the positive -transcode-allow-loss,
// -transcode-chat-capability, and -transcode-allow-client-query values with
// their `!name` negations, plus -transcode-strict-defaults. The positives
// extend the sensible defaults; the negations withdraw them;
// -transcode-strict-defaults removes every default at once (blank slate)
// while explicit positives still apply.
type transcodeCLIOptions struct {
	lossPolicy          transcode.LossPolicy
	negatedLosses       map[transcode.Feature]struct{}
	capabilities        transcode.ChatCapabilities
	negatedCapabilities map[string]struct{}
	clientQuery         map[string]struct{}
	negatedQuery        map[string]struct{}
	strictDefaults      bool
}

// buildTranscodeMappings combines the repeatable -transcode-route values with
// the preset flags, in that order. Enabling both Messages presets is a
// conflict because they register the same client route. The CLI feature
// flags in opts are merged over the sensible defaults (see the merged*
// helpers) — additive positives, `!name` negations, and the
// -transcode-strict-defaults blank slate — so a minimal invocation works out
// of the box against a modern OpenAI-compatible chat upstream while a strict
// legacy upstream can shed the defaults from the command line alone
// (review-11 finding 3).
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
			// The FINAL loss policy is applied here; a messages->responses
			// mapping under the strict policy cannot serve tool requests
			// and fails startup (review-z commit 6).
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

// mustRouteKey builds a POST route key; construction errors are impossible
// for the constant values used by the presets.
func mustRouteKey(method, path string) transcode.RouteKey {
	key, err := transcode.NewRouteKey(method, path)
	if err != nil {
		panic(err)
	}
	return key
}

// transcodeModelFlags implements flag.Value for repeatable -transcode-model
// values of the form client-model=upstream-model.
type transcodeModelFlags []string

func (f transcodeModelFlags) String() string {
	return strings.Join(f, ", ")
}

func (f *transcodeModelFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
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
	// Identity fallback applies only when NO mappings were supplied: with an
	// explicit mapping, an unknown model must be rejected, never silently
	// passed through as identity (review-08 additional 1).
	return transcode.ModelMap{
		Exact:         exact,
		AllowIdentity: len(exact) == 0,
	}, nil
}

// parseTranscodeAuth builds the AuthPolicy from the CLI contract. The secret
// itself is never accepted as a command-line argument: env/file sources keep
// it out of process listings and shell history.
func parseTranscodeAuth(
	mode, source, customHeader, anthropicVersion string,
) (transcode.AuthPolicy, error) {
	policy := transcode.AuthPolicy{
		Mode:             transcode.AuthMode(mode),
		CustomHeader:     customHeader,
		AnthropicVersion: anthropicVersion,
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

// transcodeLossFlags accumulates repeatable -transcode-allow-loss values.
type transcodeLossFlags []string

func (f transcodeLossFlags) String() string {
	return strings.Join(f, ",")
}

func (f *transcodeLossFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// transcodeCapabilityFlags accumulates repeatable -transcode-chat-capability
// values: the granular chat-provider capabilities a mapping may use.
type transcodeCapabilityFlags []string

func (f transcodeCapabilityFlags) String() string {
	return strings.Join(f, ",")
}

func (f *transcodeCapabilityFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// transcodeClientQueryFlags accumulates repeatable
// -transcode-allow-client-query values: client query parameters forwarded on
// transcoded routes.
type transcodeClientQueryFlags []string

func (f transcodeClientQueryFlags) String() string {
	return strings.Join(f, ",")
}

func (f *transcodeClientQueryFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// chatCapabilityNames is the canonical CLI vocabulary for
// -transcode-chat-capability. Every granular ChatCapabilities field has
// exactly one name; unknown names fail at startup, never on the first
// request (review-j finding 14). The field selector doubles as the negation
// handle: `!name` clears the same field a bare `name` sets.
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

// splitFlagNegations splits repeatable flag values into the positive names
// and the set of `!name` negations. Every name — positive or negated — is
// checked by validate, so an unknown negation fails at startup exactly like
// an unknown positive, never on the first request. A name given both
// positively and negated is a conflict and fails at startup: a negation that
// could be silently overridden by a positive elsewhere on the command line
// would be a trap. The positives are sorted for deterministic behavior.
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

// parseChatCapabilities validates the -transcode-chat-capability names and
// returns the capability set they enable plus the set of `!name` negations
// (unknown names in either direction fail at startup). The negations
// withdraw the sensible default capabilities inside
// buildTranscodeMappings — a CLI positive for a negated name is rejected by
// splitFlagNegations, so the two can never race.
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

// parseNegatedLosses validates the -transcode-allow-loss values into the
// approved feature set plus the set of `!name` negations. Both directions
// validate against the same granular registry: an unknown negation is
// exactly as fatal as an unknown positive.
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

// validClientQueryName reports the CLI-side rule for one
// -transcode-allow-client-query name. Names must be non-empty and may not
// contain '=', '&', '#', '?', or control characters — the exact rule the
// proxy applies to allowed client query names (validQueryName), so a
// CLI-accepted name is never rejected later (review-j finding 14).
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

// parseClientQuery validates the -transcode-allow-client-query values into
// a set of forwarded query parameter names plus the set of `!name`
// negations that withdraw default query forwarding.
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

// The sensible out-of-the-box defaults applied to every CLI transcode
// mapping. Each preset direction is the "common case" configuration: a
// modern OpenAI-compatible chat upstream fronting real Responses/Messages
// clients. Operators adjust per-mapping via -transcode-chat-capability,
// -transcode-allow-client-query, and -transcode-allow-loss; the defaults
// exist so a minimal invocation works without enumerating every feature.

// defaultTranscodeChatCapabilities is the maximally compatible out-of-the-box
// core: parallel tool calls (universally understood since 2023) and provider
// plaintext reasoning mapping (wire-safe in both directions — the extension
// is decoded and either mapped to ordinary text under this capability or
// observably dropped). Two fidelity-only knobs are deliberately OPT-IN
// because generic and open-weights gateways reject them: `reasoning_effort`
// (a 2024-era parameter several servers refuse outright; off by default the
// effort/budget knob drops observably under the request_reasoning loss) and
// developer-role messages (Qwen/Llama/DeepSeek Jinja templates only know
// system/user/assistant; off by default developer turns render as system and
// the distinction drop is observable under the developer_role loss).
var defaultTranscodeChatCapabilities = transcode.ChatCapabilities{
	DeveloperRole:         false,
	ParallelToolCalls:     true,
	ReasoningEffort:       false,
	ProviderReasoningText: true,
}

// defaultTranscodeAllowedQuery are the client query parameters forwarded
// on CLI mappings. Anthropic Messages clients (Claude Code) gate every
// request with ?beta=true, and the chat completions endpoints of real
// providers tolerate the harmless parameter (verified).
var defaultTranscodeAllowedQuery = map[string]struct{}{
	"beta": {},
}

// defaultTranscodeLosses are the non-portable features real
// Responses/Messages client traffic triggers that a CLI mapping approves
// out of the box. Reasoning summaries, Anthropic thinking blocks, system
// turns that cannot keep their position in a chat request (Claude Code
// sends mid-conversation system on every request), envelope cache controls
// (Responses and the modern Anthropic context_management/output_config
// controls), and built-in tools are client-side controls the chat target
// cannot reproduce; usage breakdowns are observability metadata the chat
// upstreams do not always report (the documented loss encodings emit zeros
// rather than failing the exchange). request_reasoning and developer_role
// back the compatibility-first capability defaults: with reasoning_effort
// and developer_role opt-in, the effort/budget knob and the role distinction
// drop observably instead of erroring out of the box.
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

// mergedLossPolicy combines the CLI -transcode-allow-loss values with the
// default loss set: CLI approvals are additive; `!name` negations and
// -transcode-strict-defaults withdraw them. A negated name that is also
// given positively never reaches this function — splitFlagNegations rejects
// the conflict at startup. With negations and strictDefaults together, the
// strict-defaults blank slate wins the default side and the negations are
// redundant no-ops against it (they are kept in the signature for
// uniformity across the three merged* helpers).
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
		// A negation withdraws a DEFAULT: the parse layer rejects a name
		// given both positively and negated, so this guard only matters
		// for direct programmatic construction of overlapping sets.
		if _, positive := lossPolicy.Allowed[feature]; !positive {
			delete(allowed, feature)
		}
	}
	return transcode.LossPolicy{Allowed: allowed}
}

// mergedChatCapabilities combines the CLI -transcode-chat-capability values
// with the default capability set: additive positives, `!name` negations
// that withdraw a default, and the -transcode-strict-defaults blank slate.
// A negated name is guaranteed absent from the CLI positives
// (splitFlagNegations rejects the conflict at parse time), so a negation
// here can only strip a default.
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
		// A negation withdraws a DEFAULT; an explicit CLI positive always
		// survives it (the parse layer rejects the direct conflict).
		*field = (*field || cli[name]) && !(deny && !cli[name])
	}
	return out
}

// mergedAllowedQuery combines the CLI -transcode-allow-client-query values
// with the default query allowlist: additive positives, `!name` negations,
// and the -transcode-strict-defaults blank slate.
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
		// A negation withdraws a DEFAULT; an explicit CLI positive always
		// survives it (the parse layer rejects the direct conflict).
		if _, positive := clientQuery[name]; !positive {
			delete(out, name)
		}
	}
	return out
}
