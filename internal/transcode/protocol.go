package transcode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Authoritative protocol contracts:
//
// OpenAI Responses:
// https://platform.openai.com/docs/api-reference/responses
// https://github.com/openai/openai-go/blob/main/responses/response.go
//
// OpenAI Chat Completions:
// https://platform.openai.com/docs/api-reference/chat
// https://github.com/openai/openai-go/blob/main/chatcompletion.go
//
// Anthropic Messages:
// https://platform.claude.com/docs/en/api/messages
//
// Chat is deliberately upstream-only. There must be no ClientChatCompletions
// value and no client-facing /v1/chat/completions transcoding route.
type ClientProtocol string

const (
	ClientResponses ClientProtocol = "responses"
	ClientMessages  ClientProtocol = "messages"
)

type UpstreamProtocol string

const (
	UpstreamResponses       UpstreamProtocol = "responses"
	UpstreamMessages        UpstreamProtocol = "messages"
	UpstreamChatCompletions UpstreamProtocol = "chat-completions"
)

// RouteKey fixes the path-only dispatch bug. Method is normalized once at
// construction and is part of the lookup key.
type RouteKey struct {
	Method string
	Path   string
}

// NewRouteKey normalizes the method and validates the path.
func NewRouteKey(method, path string) (RouteKey, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return RouteKey{}, errors.New("transcode route method is empty")
	}
	if !strings.HasPrefix(path, "/") {
		return RouteKey{}, fmt.Errorf("transcode route path %q must be absolute", path)
	}
	return RouteKey{Method: method, Path: path}, nil
}

// ChatCapabilities describes only independently verified upstream behavior.
// A capability must be enabled from configuration or a maintained provider
// profile; conversion code must never infer it from a successful response.
type ChatCapabilities struct {
	DeveloperRole     bool
	ImageInput        bool
	StructuredOutputs bool
	ParallelToolCalls bool
	StopSequences     bool
	ReasoningEffort   bool

	// ProviderReasoningText permits an explicitly configured provider extension
	// such as a plaintext "reasoning" response field. It may be mapped only to
	// ordinary text, an approved loss, or a rejection. It must never become an
	// Anthropic thinking or redacted_thinking block.
	ProviderReasoningText bool
}

// Mapping declares one transcoded route: a POST client route in one client
// protocol rendered into one upstream protocol.
type Mapping struct {
	ClientRoute RouteKey

	ClientProtocol   ClientProtocol
	UpstreamProtocol UpstreamProtocol
	UpstreamPath     string

	ChatCapabilities ChatCapabilities
	LossPolicy       LossPolicy
	ModelMap         ModelMap
	Auth             AuthPolicy
}

// Validate checks the whole immutable route configuration so a misconfigured
// mapping fails at startup, never on the first request (review-j finding 14):
// the route shape, the direction, the authentication policy, and the model
// map.
func (m Mapping) Validate() error {
	if m.ClientRoute.Method != http.MethodPost {
		return fmt.Errorf(
			"transcoding is supported only for POST create routes, got %s %s",
			m.ClientRoute.Method,
			m.ClientRoute.Path,
		)
	}
	if !strings.HasPrefix(m.UpstreamPath, "/") {
		return fmt.Errorf("upstream path %q must be absolute", m.UpstreamPath)
	}

	switch {
	case m.ClientProtocol == ClientResponses &&
		m.UpstreamProtocol == UpstreamChatCompletions:

	case m.ClientProtocol == ClientMessages &&
		m.UpstreamProtocol == UpstreamResponses:

	case m.ClientProtocol == ClientMessages &&
		m.UpstreamProtocol == UpstreamChatCompletions:

	default:
		return fmt.Errorf(
			"unsupported transcode direction %q -> %q",
			m.ClientProtocol,
			m.UpstreamProtocol,
		)
	}

	if err := m.Auth.Validate(m.UpstreamProtocol); err != nil {
		return fmt.Errorf("auth policy: %w", err)
	}
	if m.Auth.Mode == AuthCustomHeader {
		// The custom header name must be a valid HTTP field name and must
		// not collide with a header the proxy pipeline manages (auth
		// stripping, hop-by-hop removal, representation sanitization).
		if !ValidHTTPFieldName(m.Auth.CustomHeader) {
			return fmt.Errorf(
				"auth policy: custom header name %q is not a valid HTTP field name",
				m.Auth.CustomHeader,
			)
		}
		if reservedTranscodeHeaderName(m.Auth.CustomHeader) {
			return fmt.Errorf(
				"auth policy: custom header name %q is reserved by the proxy pipeline",
				m.Auth.CustomHeader,
			)
		}
	}

	// Model map: every explicit entry must resolve to a real upstream model.
	for clientModel, mapping := range m.ModelMap.Exact {
		if clientModel == "" {
			return errors.New("model map: empty client model key")
		}
		if mapping.UpstreamModel == "" {
			return fmt.Errorf(
				"model map: client model %q maps to an empty upstream model",
				clientModel,
			)
		}
	}

	return nil
}

// reservedTranscodeHeaderName reports whether the header name is managed by
// the proxy pipeline and therefore cannot be a custom authentication header:
// auth stripping (including the x-amz-*/x-goog-* cloud-signature prefixes),
// hop-by-hop removal, representation sanitization, forwarded-header
// deletion, and anti-compression would remove or rewrite it — the secret
// would be stripped, clobbered, or leaked (review-j finding 14).
func reservedTranscodeHeaderName(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "x-api-key", "api-key", "x-goog-api-key", "host",
		"content-type", "anthropic-version", "anthropic-beta",
		"accept-encoding", "x-forwarded-for", "x-forwarded-host",
		"x-forwarded-proto",
		"content-length", "content-encoding", "content-md5", "content-range",
		"digest", "content-digest", "repr-digest", "signature",
		"signature-input", "etag", "last-modified", "accept-ranges",
		"if-match", "if-none-match", "if-modified-since",
		"if-unmodified-since", "if-range",
		"connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return strings.HasPrefix(lower, "x-amz-") ||
		strings.HasPrefix(lower, "x-goog-")
}
