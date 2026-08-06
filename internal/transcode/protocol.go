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

// Validate checks the route shape and the direction. The client route must be
// a POST create route, the upstream path must be absolute, and the direction
// must be one of the supported pairs. Chat is valid only as an upstream.
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
		return nil

	case m.ClientProtocol == ClientMessages &&
		m.UpstreamProtocol == UpstreamResponses:
		return nil

	case m.ClientProtocol == ClientMessages &&
		m.UpstreamProtocol == UpstreamChatCompletions:
		return nil

	default:
		return fmt.Errorf(
			"unsupported transcode direction %q -> %q",
			m.ClientProtocol,
			m.UpstreamProtocol,
		)
	}
}
