package config

import (
	"fmt"
	"net/url"

	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// validateBasic validates a single provider's identity/connection values and
// normalizes its prefix. It does not construct runtime objects. When multi is
// false (the single legacy provider) errors are not prefixed with a section
// label, keeping the error text byte-identical to the legacy binary.
func (p *Provider) validateBasic(index int, multi bool) error {
	// When multi is false (the single legacy provider) the context prefix is
	// empty so error text stays byte-identical to the legacy binary.
	ctx := func() string {
		if !multi {
			return ""
		}
		return fmt.Sprintf("provider section %d: ", index+1)
	}

	if p.Upstream == "" {
		return fmt.Errorf("%s-upstream is required", ctx())
	}
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return fmt.Errorf("%sinvalid -upstream URL: %w", ctx(), err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("%s-upstream URL must include scheme (http or https)", ctx())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s-upstream URL scheme must be http or https, got %q", ctx(), u.Scheme)
	}
	if u.Hostname() == "" {
		if multi && p.Name == "" {
			return fmt.Errorf("%s-upstream URL must include a hostname; unable to derive a name", ctx())
		}
		return fmt.Errorf("%s-upstream URL must include a hostname", ctx())
	}
	p.upstream = u

	if p.Concurrency < 1 {
		return fmt.Errorf("%s-concurrency must be >= 1, got %d", ctx(), p.Concurrency)
	}
	if p.GlobalConcurrency < 0 {
		return fmt.Errorf("%s-global-concurrency must be >= 0, got %d", ctx(), p.GlobalConcurrency)
	}
	if p.ReleaseCooldown < 0 {
		return fmt.Errorf("%s-release-cooldown must be >= 0, got %v", ctx(), p.ReleaseCooldown)
	}

	// Fail-closed backstops for every remaining value flag, in flag-definition
	// order (see registerProviderFlags). queue.NewLimiterWithCooldown panics on
	// invalid input and stdlib flag accepts negative durations, so each value is
	// rejected here before resolve() can reach any panicking constructor. The
	// messages mirror the downstream proxy/circuitbreaker validation that this
	// replaces as the first line of defense.
	if p.QueueTimeout < 0 {
		return fmt.Errorf("%s-queue-timeout must be >= 0, got %v", ctx(), p.QueueTimeout)
	}
	if p.QueueDepth < 0 {
		return fmt.Errorf("%s-queue-depth must be >= 0, got %d", ctx(), p.QueueDepth)
	}
	if p.QueueComments < 0 {
		return fmt.Errorf("%s-queue-comments must be >= 0, got %v", ctx(), p.QueueComments)
	}
	if p.RetryMaxBodyMB < 0 {
		return fmt.Errorf("%s-retry-max-body-mb must be >= 0, got %d", ctx(), p.RetryMaxBodyMB)
	}
	if p.RetryWaitMin < 0 {
		return fmt.Errorf("%s-retry-wait-min must be >= 0, got %v", ctx(), p.RetryWaitMin)
	}
	if p.RetryWaitMax < 0 {
		return fmt.Errorf("%s-retry-wait-max must be >= 0, got %v", ctx(), p.RetryWaitMax)
	}
	if p.RetryMinDelay < 0 {
		return fmt.Errorf("%s-retry-min-delay must be >= 0, got %v", ctx(), p.RetryMinDelay)
	}
	if p.CancelCooldown < 0 {
		return fmt.Errorf("%s-cancel-cooldown must be >= 0, got %v", ctx(), p.CancelCooldown)
	}
	if p.FailureHold < 0 {
		return fmt.Errorf("%s-failure-hold must be >= 0, got %v", ctx(), p.FailureHold)
	}
	if p.AdaptiveHeadroomWindow < 0 {
		return fmt.Errorf("%s-adaptive-headroom-window must be >= 0, got %v", ctx(), p.AdaptiveHeadroomWindow)
	}
	if p.CBEnabled {
		// The breaker rejects zero/negative values, but only when enabled —
		// the disabled-breaker zero state is legal.
		if p.CBThreshold < 1 {
			return fmt.Errorf("%s-cb-threshold must be > 0, got %d", ctx(), p.CBThreshold)
		}
		if p.CBWindow <= 0 {
			return fmt.Errorf("%s-cb-window must be > 0, got %v", ctx(), p.CBWindow)
		}
		if p.CBOpenTimeout <= 0 {
			return fmt.Errorf("%s-cb-open-timeout must be > 0, got %v", ctx(), p.CBOpenTimeout)
		}
		if p.CBMaxOpen <= 0 {
			return fmt.Errorf("%s-cb-max-open-timeout must be > 0, got %v", ctx(), p.CBMaxOpen)
		}
		if p.CBPenalty <= 0 {
			return fmt.Errorf("%s-cb-penalty must be > 0, got %v", ctx(), p.CBPenalty)
		}
		if p.CBMaxPenalty <= 0 {
			return fmt.Errorf("%s-cb-max-penalty must be > 0, got %v", ctx(), p.CBMaxPenalty)
		}
	}

	if err := p.validateAuth(); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}
	// Grammar-only (no side effects): the variable is read during resolve,
	// after every provider's grammar has been checked, mirroring validateAuth.
	if p.SesameTokenEnv != "" && !validEnvVarName(p.SesameTokenEnv) {
		return fmt.Errorf(
			"%s-sesame-token-env %q: environment variable name must match [A-Za-z_][A-Za-z0-9_]*",
			ctx(), p.SesameTokenEnv,
		)
	}
	// The local-handshake trust flags configure the handshake the proxy
	// performs itself. Under remote termination the proxy never handshakes,
	// so accepting them would silently ignore the operator's CA — refuse
	// here in validation, before any material is decoded, so the error names
	// the meaningless combination rather than a missing file.
	if p.RCTerminateTLS && (p.UpstreamCA != "" || p.UpstreamInsecure || p.UpstreamServerName != "") {
		return fmt.Errorf("%supstream TLS trust options (-upstream-ca, -upstream-insecure, -upstream-server-name) do not apply with -rc-terminate-tls; use the -rc-target-* options", ctx())
	}
	// On a plaintext upstream the transport never consults TLSClientConfig,
	// so the same trio would be accepted but inert there too.
	if p.upstream.Scheme == "http" && (p.UpstreamCA != "" || p.UpstreamInsecure || p.UpstreamServerName != "") {
		return fmt.Errorf("%supstream TLS trust options (-upstream-ca, -upstream-insecure, -upstream-server-name) do not apply to a plaintext http upstream", ctx())
	}
	// A plaintext upstream has no TLS handshake for remote termination.
	if p.upstream.Scheme == "http" && p.RCTerminateTLS {
		return fmt.Errorf("%s-rc-terminate-tls does not apply to a plaintext http upstream: there is no upstream TLS handshake to terminate", ctx())
	}

	for _, s := range p.Limits {
		if _, err := route.Parse(s); err != nil {
			return fmt.Errorf("%sinvalid -limit %q: %w", ctx(), s, err)
		}
	}
	if err := normalizePrefix(p); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}
	return nil
}
