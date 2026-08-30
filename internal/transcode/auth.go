package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
)

// Direct OpenAI clients use bearer authentication:
// https://github.com/openai/openai-go/blob/main/chatcompletion.go#L66-L70
//
// Direct Anthropic authentication and required version header:
// https://platform.claude.com/docs/en/api/overview#headers
//
// Azure OpenAI authentication:
// https://learn.microsoft.com/en-us/azure/ai-services/openai/reference#authentication
//
// Amazon Bedrock API keys:
// https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html

// CredentialKind identifies the kind of an inbound credential.
type CredentialKind uint8

// CredentialKind values.
const (
	CredentialUnknown CredentialKind = iota
	CredentialBearer
	CredentialXAPIKey
	CredentialAPIKey
)

// Credential is one extracted inbound credential.
type Credential struct {
	Kind  CredentialKind
	Value string
}

// errAuthInboundCredential marks client-fault authentication errors: the
// request is rejected (4xx) rather than treated as an internal failure.
var errAuthInboundCredential = errors.New("invalid inbound credential")

// ExtractInboundCredential extracts exactly one unambiguous credential from
// the client headers. Multiple different credentials are an error; equal
// duplicate representations collapse to one.
func ExtractInboundCredential(h http.Header) (Credential, error) {
	var credentials []Credential

	for _, value := range h.Values("Authorization") {
		scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") ||
			strings.TrimSpace(token) == "" {
			return Credential{}, fmt.Errorf(
				"%w: authorization must use a non-empty Bearer credential",
				errAuthInboundCredential,
			)
		}
		credentials = append(credentials, Credential{
			Kind:  CredentialBearer,
			Value: strings.TrimSpace(token),
		})
	}

	for _, value := range h.Values("X-Api-Key") {
		if value = strings.TrimSpace(value); value != "" {
			credentials = append(credentials, Credential{
				Kind:  CredentialXAPIKey,
				Value: value,
			})
		}
	}

	for _, value := range h.Values("Api-Key") {
		if value = strings.TrimSpace(value); value != "" {
			credentials = append(credentials, Credential{
				Kind:  CredentialAPIKey,
				Value: value,
			})
		}
	}

	if len(credentials) == 0 {
		return Credential{}, nil
	}

	first := credentials[0]
	for _, credential := range credentials[1:] {
		if credential.Value != first.Value {
			return Credential{}, fmt.Errorf(
				"%w: multiple different inbound credentials were supplied",
				errAuthInboundCredential,
			)
		}
	}

	// Equal duplicate representations collapse to one credential.
	return first, nil
}

// AuthMode is the configured target authentication mode.
type AuthMode string

// AuthMode values.
const (
	AuthAuto           AuthMode = "auto"
	AuthNone           AuthMode = "none"
	AuthBearer         AuthMode = "bearer"
	AuthXAPIKey        AuthMode = "x-api-key"
	AuthAPIKey         AuthMode = "api-key"
	AuthCustomHeader   AuthMode = "header"
	AuthExternalSigner AuthMode = "external-signer"
)

// SecretSource supplies an upstream secret on demand.
type SecretSource interface {
	Secret(context.Context) (string, error)
}

// RequestSigner signs a final request. Sign runs last, after the final URL,
// query, body, Host, and representation headers are set.
type RequestSigner interface {
	Sign(context.Context, *http.Request) error
}

// AuthPolicy describes how the target request is authenticated.
type AuthPolicy struct {
	Mode AuthMode

	// Inbound=true means use the single credential extracted from the client.
	// Otherwise Secret must be configured.
	Inbound bool
	Secret  SecretSource

	CustomHeader     string
	AnthropicVersion string

	Signer RequestSigner
}

// IsZero reports whether the AuthPolicy is unconfigured (all zero values).
func (p AuthPolicy) IsZero() bool {
	return p.Mode == "" && !p.Inbound && p.Secret == nil && p.Signer == nil && p.CustomHeader == "" && p.AnthropicVersion == ""
}

// FromProviderAuth converts an internal/auth.AuthPolicy into a transcode.AuthPolicy.
// When policy is nil or Mode is auth.AuthNone, it returns AuthPolicy{Mode: AuthNone}.
// When policy is non-nil with a secret source, the resulting transcode.AuthPolicy
// has Inbound=false and inherits the provider's resolved mode, secret source,
// custom header, and anthropic version.
func FromProviderAuth(policy *auth.AuthPolicy) AuthPolicy {
	if policy == nil || policy.Mode == auth.AuthNone {
		return AuthPolicy{Mode: AuthNone}
	}
	var mode AuthMode
	switch policy.Mode {
	case auth.AuthBearer:
		mode = AuthBearer
	case auth.AuthXAPIKey:
		mode = AuthXAPIKey
	case auth.AuthAPIKey:
		mode = AuthAPIKey
	case auth.AuthCustomHeader:
		mode = AuthCustomHeader
	default:
		mode = AuthMode(policy.Mode)
	}
	return AuthPolicy{
		Mode:             mode,
		Secret:           policy.Secret,
		CustomHeader:     policy.CustomHeader,
		AnthropicVersion: policy.AnthropicVersion,
	}
}

// Validate checks the policy against the target protocol.
func (p AuthPolicy) Validate(target UpstreamProtocol) error {
	switch p.Mode {
	case "", AuthAuto, AuthNone, AuthBearer, AuthXAPIKey, AuthAPIKey:
	case AuthCustomHeader:
		if strings.TrimSpace(p.CustomHeader) == "" {
			return errors.New("custom auth header is empty")
		}
	case AuthExternalSigner:
		if p.Signer == nil {
			return errors.New("external-signer mode requires a signer")
		}
	default:
		return fmt.Errorf("unknown auth mode %q", p.Mode)
	}

	// A secret-requiring mode must have a way to obtain the secret:
	// inbound forwarding or a configured source. A missing source would
	// otherwise pass startup and fail every request (review-j finding 14).
	if p.Mode != AuthNone && p.Mode != AuthExternalSigner && !p.Inbound && p.Secret == nil {
		return errors.New("auth mode requires a secret source or inbound credentials")
	}

	if target == UpstreamMessages &&
		p.AnthropicVersion == "" &&
		p.Mode != AuthNone {
		return errors.New("messages authentication requires anthropic-version")
	}
	return nil
}

// ApplyTargetAuthentication strips every source credential and applies the
// target policy. It must run after the request is otherwise final and before
// sending.
func ApplyTargetAuthentication(
	ctx context.Context,
	req *http.Request,
	target UpstreamProtocol,
	policy AuthPolicy,
	inbound Credential,
) error {
	if err := policy.Validate(target); err != nil {
		return err
	}

	stripAuthentication(req.Header)

	if target != UpstreamMessages {
		req.Header.Del("Anthropic-Version")
		req.Header.Del("Anthropic-Beta")
	}

	if policy.Mode == AuthNone {
		return nil
	}

	if policy.Mode == AuthExternalSigner {
		// The signer is attached to the request context; the signing
		// transport inside the retry chain signs EVERY actual attempt after
		// body reconstruction (review-z commit 4).
		*req = *req.WithContext(WithRequestSigner(req.Context(), policy.Signer))
		return nil
	}

	secret, err := resolveSecret(ctx, policy, inbound)
	if err != nil {
		return err
	}

	mode := policy.Mode
	if mode == AuthAuto || mode == "" {
		switch target {
		case UpstreamMessages:
			// This default makes an OpenAI-compatible client carrying a
			// static upstream API key through Authorization work with the
			// direct Messages API. Workload-identity bearer tokens require
			// the explicit AuthBearer profile.
			mode = AuthXAPIKey
		case UpstreamResponses, UpstreamChatCompletions:
			mode = AuthBearer
		default:
			return fmt.Errorf("auto auth has no rule for %q", target)
		}
	}

	switch mode {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+secret)

	case AuthXAPIKey:
		req.Header.Set("X-Api-Key", secret)

	case AuthAPIKey:
		req.Header.Set("Api-Key", secret)

	case AuthCustomHeader:
		req.Header.Set(policy.CustomHeader, secret)

	default:
		return fmt.Errorf("unsupported resolved auth mode %q", mode)
	}

	if target == UpstreamMessages {
		req.Header.Set("Anthropic-Version", policy.AnthropicVersion)
	}
	return nil
}

// resolveSecret returns the upstream secret: the inbound credential when
// Inbound is set, otherwise the configured SecretSource.
func resolveSecret(
	ctx context.Context,
	policy AuthPolicy,
	inbound Credential,
) (string, error) {
	if policy.Inbound {
		if inbound.Value == "" {
			return "", fmt.Errorf(
				"%w: no inbound credential was supplied",
				errAuthInboundCredential,
			)
		}
		return inbound.Value, nil
	}
	if policy.Secret == nil {
		return "", errors.New("no configured upstream secret source")
	}
	secret, err := policy.Secret.Secret(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("configured upstream secret is empty")
	}
	return strings.TrimSpace(secret), nil
}

// stripAuthentication removes every source credential and protocol header so
// no credential crosses a provider boundary. Cloud signature headers are
// removed before the target signer runs.
func stripAuthentication(h http.Header) {
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Api-Key",
		"Api-Key",
		"X-Goog-Api-Key",
		"Anthropic-Version",
		"Anthropic-Beta",
	} {
		h.Del(name)
	}

	// Remove any source cloud signature before applying the target signer.
	for name := range h {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") ||
			strings.HasPrefix(lower, "x-goog-") {
			h.Del(name)
		}
	}
}

// BuildConvertedRequest ensures GetBody exists for retries and signers.
func BuildConvertedRequest(
	ctx context.Context,
	method string,
	targetURL string,
	body []byte,
	headers http.Header,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		targetURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}

	req.Header = headers.Clone()
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return req, nil
}
