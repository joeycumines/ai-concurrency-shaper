package main

import (
	"fmt"
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
	if err := mapping.Validate(); err != nil {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: %w", value, err)
	}
	return proxy.TranscodeMapping{Mapping: mapping}, nil
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

// buildTranscodeMappings combines the repeatable -transcode-route values with
// the preset flags, in that order. Enabling both Messages presets is a
// conflict because they register the same client route.
func buildTranscodeMappings(
	routes []proxy.TranscodeMapping,
	responsesChat, messagesChat, messagesResponses bool,
) ([]proxy.TranscodeMapping, error) {
	if messagesChat && messagesResponses {
		return nil, fmt.Errorf(
			"-transcode-messages-chat and -transcode-messages-responses both map /v1/messages; enable only one",
		)
	}

	var mappings []proxy.TranscodeMapping
	mappings = append(mappings, routes...)
	if responsesChat {
		mappings = append(mappings, proxy.TranscodeMapping{Mapping: transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/responses"),
			ClientProtocol:   transcode.ClientResponses,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}})
	}
	if messagesChat {
		mappings = append(mappings, proxy.TranscodeMapping{Mapping: transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/messages"),
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}})
	}
	if messagesResponses {
		mappings = append(mappings, proxy.TranscodeMapping{Mapping: transcode.Mapping{
			ClientRoute:      mustRouteKey(httpMethodPost, "/v1/messages"),
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		}})
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
	modelMap := transcode.ModelMap{AllowIdentity: true}
	exact := make(map[string]transcode.ModelMapping)
	for _, value := range values {
		client, upstream, ok := strings.Cut(value, "=")
		if !ok || client == "" || upstream == "" {
			return transcode.ModelMap{}, fmt.Errorf(
				"invalid -transcode-model %q: want client-model=upstream-model",
				value,
			)
		}
		exact[client] = transcode.ModelMapping{
			ClientModel:         client,
			UpstreamModel:       upstream,
			ClientResponseModel: client,
		}
	}
	modelMap.Exact = exact
	return modelMap, nil
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
