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
	"bytes"
	"log"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// mustParseURL parses raw as a URL or fails the test.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// TestResolveAndValidate_LegacySingle exercises the good single-provider legacy
// path: one provider at bare root with default tuning resolves to fully wired
// runtime objects.
func TestResolveAndValidate_LegacySingle(t *testing.T) {
	cfg, err := Parse([]string{"-upstream", "https://x", "-bind", ":8080", "-tui"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}

	// Server fields preserved.
	if cfg.Server.Bind != ":8080" {
		t.Errorf("Server.Bind = %q, want %q", cfg.Server.Bind, ":8080")
	}
	if !cfg.Server.TUI {
		t.Error("Server.TUI = false, want true")
	}

	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.Name != "" {
		t.Errorf("Name = %q, want empty (single unnamed provider)", p.Name)
	}
	if p.Prefix != "" {
		t.Errorf("Prefix = %q, want empty (bare root)", p.Prefix)
	}
	if p.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want default 4", p.Concurrency)
	}

	// Resolved state.
	if p.UpstreamURL() == nil {
		t.Fatal("UpstreamURL() = nil, want parsed URL")
	}
	if p.UpstreamURL().String() != "https://x" {
		t.Errorf("UpstreamURL() = %q, want %q", p.UpstreamURL().String(), "https://x")
	}
	wantPatterns := route.DefaultPatterns()
	if got := p.Patterns(); !reflect.DeepEqual(got, wantPatterns) {
		t.Errorf("Patterns() = %v, want DefaultPatterns %v", got, wantPatterns)
	}
	if p.Matcher() == nil {
		t.Error("Matcher() = nil, want non-nil")
	}
	if p.DefaultLimiter() == nil || p.DefaultLimiter().Limit() != 4 {
		t.Errorf("DefaultLimiter() = nil or limit != 4, got %v", p.DefaultLimiter())
	}
	if p.GlobalLimiter() != nil {
		t.Error("GlobalLimiter() = non-nil, want nil (disabled by default)")
	}
	// idempotency: resolving twice yields the same effect without error.
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Errorf("second ResolveAndValidate: %v", err)
	}
}

// TestResolveAndValidate_Multi exercises the two-section path: derived names, mount
// prefixes, per-provider limiters, and the transport's per-host idle pool.
func TestResolveAndValidate_Multi(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream", "https://acme.example.com",
		"-prefix", "/acme",
		"-concurrency", "8",
		"--provider",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/claude",
		"-global-concurrency", "24",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}

	// Provider 0: explicit name acme, concurrency 8, no global limiter.
	p0 := cfg.Providers[0]
	if p0.Name != "acme" {
		t.Errorf("Providers[0].Name = %q, want %q", p0.Name, "acme")
	}
	if p0.Prefix != "/acme" {
		t.Errorf("Providers[0].Prefix = %q, want %q", p0.Prefix, "/acme")
	}
	if p0.DefaultLimiter() == nil || p0.DefaultLimiter().Limit() != 8 {
		t.Errorf("Providers[0].DefaultLimiter() = nil or limit != 8, got %v", p0.DefaultLimiter())
	}
	if p0.GlobalLimiter() != nil {
		t.Errorf("Providers[0].GlobalLimiter() = non-nil, want nil")
	}

	// Provider 1: name derives from api.anthropic.com -> anthropic.
	p1 := cfg.Providers[1]
	if p1.Name != "anthropic" {
		t.Errorf("Providers[1].Name = %q, want %q", p1.Name, "anthropic")
	}
	if p1.Prefix != "/claude" {
		t.Errorf("Providers[1].Prefix = %q, want %q", p1.Prefix, "/claude")
	}
	if p1.GlobalLimiter() == nil || p1.GlobalLimiter().Limit() != 24 {
		t.Errorf("Providers[1].GlobalLimiter() = nil or limit != 24, got %v", p1.GlobalLimiter())
	}

	// Neither provider configured -limit, so routeLimiters stays empty and no
	// pattern uses a group pool; default pools take the whole concurrency. Both
	// floor at 20 (probe an exact value only via the group case below).
	if len(p0.RouteLimiters()) != 0 {
		t.Errorf("Providers[0].RouteLimiters() = %v, want empty", p0.RouteLimiters())
	}
	if p0.MaxIdleConnsPerHost() < 20 {
		t.Errorf("Providers[0].MaxIdleConnsPerHost() = %d, want >= 20", p0.MaxIdleConnsPerHost())
	}
	if p1.MaxIdleConnsPerHost() < 20 {
		t.Errorf("Providers[1].MaxIdleConnsPerHost() = %d, want >= 20", p1.MaxIdleConnsPerHost())
	}
}

// TestResolveAndValidate_GroupLimiters exercises the route/group limiter and
// transport-pool math with explicit -limit patterns, including a shared group pool.
func TestResolveAndValidate_GroupLimiters(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://one",
		"-limit", "POST /v1/messages:30@anthropic",
		"-limit", "POST /completions:8@anthropic",
		"-limit", "POST /completions:8@group2",
		"-limit", "POST /free:5",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}

	p := cfg.Providers[0]
	rl := p.RouteLimiters()
	// The two @anthropic patterns share one limiter (first wins: 30), plus the
	// 8 for @group2 and the lone 5. All patterns have nonzero limits, so no
	// pattern falls through to the default pool.
	if len(rl) != 3 {
		t.Fatalf("len(RouteLimiters()) = %d, want 3", len(rl))
	}
	if l := rl["anthropic"]; l == nil || l.Limit() != 30 {
		t.Errorf(`RouteLimiters()["anthropic"] = %v, want limit 30`, l)
	}
	if l := rl["group2"]; l == nil || l.Limit() != 8 {
		t.Errorf(`RouteLimiters()["group2"] = %v, want limit 8`, l)
	}
	if l := rl["POST /free:5"]; l == nil || l.Limit() != 5 {
		t.Errorf(`RouteLimiters()["POST /free:5"] = %v, want limit 5`, l)
	}

	// Pool math: route pools 30 + 8 + 5 = 43, no global cap, no
	// default pool used (all limits nonzero), above the 20 floor -> 43.
	if got := p.MaxIdleConnsPerHost(); got != 43 {
		t.Errorf("MaxIdleConnsPerHost() = %d, want 43", got)
	}
}

// TestResolveAndValidate_Overlap exercises the prefix overlap table: segments-wise
// prefix relationships are rejected, disjoint prefixes pass.
func TestResolveAndValidate_Overlap(t *testing.T) {
	// Each case builds two providers with valid upstreams and the given prefixes.
	cases := []struct {
		name        string
		a, b        string
		wantErr     bool
		errContains string
	}{
		{name: "nested overlap", a: "/acme", b: "/acme/v1", wantErr: true, errContains: "overlap"},
		{name: "disjoint", a: "/acme", b: "/acme2", wantErr: false},
		{name: "equal", a: "/api", b: "/api", wantErr: true, errContains: "overlap"},
		{name: "one-char prefix", a: "/a", b: "/", wantErr: true, errContains: "requires -prefix"},
		{name: "empty vs any", a: "", b: "/acme", wantErr: true, errContains: "requires -prefix"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Providers: []*Provider{
					{Name: "a", Upstream: "https://a.example.com", Prefix: tc.a, Concurrency: 4},
					{Name: "b", Upstream: "https://b.example.com", Prefix: tc.b, Concurrency: 4},
				},
			}
			err := cfg.ResolveAndValidate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("ResolveAndValidate: nil error, want error")
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error = %q, want it to contain %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveAndValidate: %v, want nil", err)
			}
		})
	}
}

// TestResolveAndValidate_OverlapDerivedName pins the overlap error's labels to
// each provider's (possibly derived) name, not its raw upstream host. This
// matches the plan's specified error string verbatim:
// `provider prefixes overlap: "/acme" (acme) and "/acme/v1" (anthropic)`.
func TestResolveAndValidate_OverlapDerivedName(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Name: "acme", Upstream: "https://acme.example.com", Prefix: "/acme", Concurrency: 4},
			{Upstream: "https://api.anthropic.com", Prefix: "/acme/v1", Concurrency: 4},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want overlap error")
	}
	want := `provider prefixes overlap: "/acme" (acme) and "/acme/v1" (anthropic)`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestResolveAndValidate_DuplicateName rejects two providers that derive the same
// name (both api.anthropic.com -> anthropic).
func TestResolveAndValidate_DuplicateName(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Upstream: "https://api.anthropic.com", Prefix: "/a", Concurrency: 4},
			{Upstream: "https://api.anthropic.com", Prefix: "/b", Concurrency: 4},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want duplicate-name error")
	}
	if !strings.Contains(err.Error(), `duplicate provider name "anthropic"`) {
		t.Errorf("error = %q, want duplicate provider name %q", err, `"anthropic"`)
	}
}

// TestResolveAndValidate_PerProviderUpstreamRequired checks the per-provider
// -upstream required error with the section index.
func TestResolveAndValidate_PerProviderUpstreamRequired(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Upstream: "https://a", Prefix: "/a", Concurrency: 4},
			{Upstream: "", Prefix: "/b", Concurrency: 4},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want upstream-required error")
	}
	want := "provider section 2: -upstream is required"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestResolveAndValidate_NoProviders checks the empty-provider error.
func TestResolveAndValidate_NoProviders(t *testing.T) {
	cfg := &Config{}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want 'no providers configured'")
	}
	if err.Error() != "no providers configured" {
		t.Errorf("error = %q, want %q", err, "no providers configured")
	}
}

// TestResolveAndValidate_BadScheme checks the http/https scheme requirement.
func TestResolveAndValidate_BadScheme(t *testing.T) {
	cfg := &Config{Providers: []*Provider{{Name: "a", Upstream: "ftp://x"}}}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want scheme error")
	}
	want := `-upstream URL scheme must be http or https, got "ftp"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestResolveAndValidate_BadSchemeMulti checks that the scheme error is
// prefixed with the section index when more than one provider is configured.
func TestResolveAndValidate_BadSchemeMulti(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Upstream: "https://a", Prefix: "/a", Concurrency: 4},
			{Upstream: "ftp://b", Prefix: "/b", Concurrency: 4},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want scheme error")
	}
	want := `provider section 2: -upstream URL scheme must be http or https, got "ftp"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestResolveAndValidate_ConcurrencyLt1 checks the concurrency >= 1 rule.
func TestResolveAndValidate_ConcurrencyLt1(t *testing.T) {
	cfg := &Config{Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 0}}}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want concurrency error")
	}
	if !strings.Contains(err.Error(), "concurrency must be >= 1") {
		t.Errorf("error = %q, want concurrency >= 1 text", err)
	}
}

// TestResolveAndValidate_NegativeReleaseCooldown checks the cooldown >= 0 rule.
func TestResolveAndValidate_NegativeReleaseCooldown(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, ReleaseCooldown: -1 * time.Second}},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want cooldown error")
	}
	if !strings.Contains(err.Error(), "release-cooldown must be >= 0") {
		t.Errorf("error = %q, want release-cooldown >= 0 text", err)
	}
}

// TestResolveAndValidate_NegativeDurationFlags checks that every provider-scoped
// duration flag, given a negative value, is rejected by ResolveAndValidate
// directly — before resolve() can reach queue.NewLimiterWithCooldown (which
// panics on invalid input) or proxy.New (whose validation this replaces as the
// first line of defense).
func TestResolveAndValidate_NegativeDurationFlags(t *testing.T) {
	cases := []struct {
		name  string
		field func(*Provider) *time.Duration
	}{
		{"queue-timeout", func(p *Provider) *time.Duration { return &p.QueueTimeout }},
		{"retry-wait-min", func(p *Provider) *time.Duration { return &p.RetryWaitMin }},
		{"retry-wait-max", func(p *Provider) *time.Duration { return &p.RetryWaitMax }},
		{"retry-min-delay", func(p *Provider) *time.Duration { return &p.RetryMinDelay }},
		{"release-cooldown", func(p *Provider) *time.Duration { return &p.ReleaseCooldown }},
		{"cancel-cooldown", func(p *Provider) *time.Duration { return &p.CancelCooldown }},
		{"failure-hold", func(p *Provider) *time.Duration { return &p.FailureHold }},
		{"adaptive-headroom-window", func(p *Provider) *time.Duration { return &p.AdaptiveHeadroomWindow }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{Name: "a", Upstream: "https://x", Concurrency: 4}
			*tc.field(p) = -time.Second
			err := (&Config{Providers: []*Provider{p}}).ResolveAndValidate()
			if err == nil {
				t.Fatalf("ResolveAndValidate: nil error, want %s rejection", tc.name)
			}
			want := tc.name + " must be >= 0"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want %q", err, want)
			}
		})
	}
}

// TestResolveAndValidate_NegativeRetryMaxBodyMB checks the size backstop.
func TestResolveAndValidate_NegativeRetryMaxBodyMB(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, RetryMaxBodyMB: -1}},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want retry-max-body-mb rejection")
	}
	if !strings.Contains(err.Error(), "retry-max-body-mb must be >= 0") {
		t.Errorf("error = %q, want retry-max-body-mb >= 0 text", err)
	}
}

// TestResolveAndValidate_CBZeroDurations checks that every circuit-breaker
// value is rejected as non-positive when the breaker is enabled, and that the
// disabled-breaker zero state stays legal (CBEnabled=false with all-zero CB
// fields must pass — that is the "breaker off" configuration).
func TestResolveAndValidate_CBZeroDurations(t *testing.T) {
	cases := []struct {
		name  string
		field func(*Provider)
	}{
		{"cb-threshold", func(p *Provider) { p.CBThreshold = 0 }},
		{"cb-window", func(p *Provider) { p.CBWindow = 0 }},
		{"cb-open-timeout", func(p *Provider) { p.CBOpenTimeout = 0 }},
		{"cb-max-open-timeout", func(p *Provider) { p.CBMaxOpen = 0 }},
		{"cb-penalty", func(p *Provider) { p.CBPenalty = 0 }},
		{"cb-max-penalty", func(p *Provider) { p.CBMaxPenalty = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a fully valid enabled-breaker provider (the flag
			// defaults) so only the field under test is invalid; the checks are
			// first-error-wins in flag-definition order.
			p := &Provider{
				Name: "a", Upstream: "https://x", Concurrency: 4,
				CBEnabled: true, CBThreshold: 5,
				CBWindow: 30 * time.Second, CBOpenTimeout: 10 * time.Second,
				CBMaxOpen: 120 * time.Second, CBPenalty: 2 * time.Second,
				CBMaxPenalty: 60 * time.Second,
			}
			tc.field(p)
			err := (&Config{Providers: []*Provider{p}}).ResolveAndValidate()
			if err == nil {
				t.Fatalf("ResolveAndValidate: nil error, want %s rejection", tc.name)
			}
			want := tc.name + " must be > 0"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want %q", err, want)
			}
		})
	}

	// Disabled breaker with all-zero CB fields is legal.
	cfg := &Config{Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4}}}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("disabled-breaker zero state should validate, got %v", err)
	}
}

// TestResolveAndValidate_BadRoutePattern checks that an unparseable -limit is
// rejected with the section context.
func TestResolveAndValidate_BadRoutePattern(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, Limits: []string{"not a pattern"}}},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want bad-pattern error")
	}
	if !strings.Contains(err.Error(), "invalid -limit") {
		t.Errorf("error = %q, want invalid -limit text", err)
	}
}

// TestResolveAndValidate_BadRoutePatternMulti checks that a bad -limit in a
// multi-provider config carries the section context.
func TestResolveAndValidate_BadRoutePatternMulti(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Upstream: "https://a", Prefix: "/a", Concurrency: 4},
			{Upstream: "https://b", Prefix: "/b", Concurrency: 4, Limits: []string{"not a pattern"}},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want bad-pattern error")
	}
	if !strings.Contains(err.Error(), "provider section 2") || !strings.Contains(err.Error(), "invalid -limit") {
		t.Errorf("error = %q, want section-scoped invalid -limit text", err)
	}
}

// TestResolveAndValidate_GlobalConcurrencyCapsPool checks that a global
// concurrency less than the summed route pools caps the transport pool.
func TestResolveAndValidate_GlobalConcurrencyCapsPool(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://one",
		"-limit", "POST /v1/messages:30@anthropic",
		"-global-concurrency", "20",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	// routePoolMax = 30; capped by global 20 -> 20.
	if got := cfg.Providers[0].MaxIdleConnsPerHost(); got != 20 {
		t.Errorf("MaxIdleConnsPerHost() = %d, want 20", got)
	}
}

// TestResolveAndValidate_Breaker exercises the circuit breaker accessor and
// its own configuration error path, plus the disabled (nil) state.
func TestResolveAndValidate_Breaker(t *testing.T) {
	// Default: circuit breaker enabled -> non-nil breaker.
	cfg, err := Parse([]string{"-upstream", "https://x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if cfg.Providers[0].Breaker() == nil {
		t.Error("Breaker() = nil, want non-nil (enabled by default)")
	}

	// Disabled -> nil.
	cfg, err = Parse([]string{"-upstream", "https://x", "-circuit-breaker=false"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if cfg.Providers[0].Breaker() != nil {
		t.Error("Breaker() = non-nil, want nil when disabled")
	}

	// Invalid breaker config -> rejected by validateBasic's fail-closed check
	// (before resolve() could reach circuitbreaker.New).
	cfg = &Config{Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, CBEnabled: true, CBThreshold: 0}}}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want cb-threshold fail-closed error")
	}
	if !strings.Contains(err.Error(), "-cb-threshold must be > 0") {
		t.Errorf("error = %q, want cb-threshold fail-closed text", err)
	}
}

// TestScope_StringFallback exercises the Scope string switch fallthrough.
func TestScope_StringFallback(t *testing.T) {
	if got := Scope(42).String(); !strings.Contains(got, "scope(42)") {
		t.Errorf("Scope(42).String() = %q, want scope(42)", got)
	}
	if Scope(0).String() != "server" || Scope(1).String() != "provider" || Scope(2).String() != "account" {
		t.Error("Scope known values did not render their names")
	}
}

// TestValidateBasic_UrlErrors exercises the parse-error and missing-scheme paths.
func TestValidateBasic_UrlErrors(t *testing.T) {
	// Unparseable URL.
	cfg := &Config{Providers: []*Provider{{Name: "a", Upstream: "://bad", Concurrency: 4}}}
	err := cfg.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "invalid -upstream URL") {
		t.Fatalf("malformed upstream error = %v, want 'invalid -upstream URL'", err)
	}

	// Missing scheme.
	cfg = &Config{Providers: []*Provider{{Name: "a", Upstream: "example.com", Concurrency: 4}}}
	err = cfg.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "must include scheme") {
		t.Fatalf("missing-scheme error = %v, want 'must include scheme'", err)
	}
}

// TestNormalizePrefix exercises the leading-slash rule and trailing-slash trim.
func TestNormalizePrefix(t *testing.T) {
	// Missing leading slash rejected.
	cfg := &Config{Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, Prefix: "acme"}}}
	err := cfg.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "-prefix must start with /") {
		t.Fatalf("leading-slash error = %v, want '-prefix must start with /'", err)
	}

	// Trailing slash is trimmed; "/" becomes bare.
	cfg = &Config{Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, Prefix: "/acme/"}}}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if cfg.Providers[0].Prefix != "/acme" {
		t.Errorf("Prefix after trim = %q, want %q", cfg.Providers[0].Prefix, "/acme")
	}
}

// TestPrefixSegments exercises traversal resolution in the overlap helper.
func TestPrefixSegments(t *testing.T) {
	if got := prefixSegments("/a/./b/../c/"); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("prefixSegments(/a/./b/../c/) = %v, want [a c]", got)
	}
	if got := prefixSegments("/../x"); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("prefixSegments(/../x) = %v, want [x]", got)
	}
	if got := prefixSegments("/a/b"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("prefixSegments(/a/b) = %v, want [a b]", got)
	}
}

// TestGetNameLabelFallback exercises nameLabel's unnamed fallthrough paths.
func TestGetNameLabelFallback(t *testing.T) {
	if got := nameLabel(&Provider{Name: "x"}); got != "x" {
		t.Errorf("nameLabel with Name = %q, want %q", got, "x")
	}
	if got := nameLabel(&Provider{upstream: mustParseURL(t, "https://h.example")}); got != "h.example" {
		t.Errorf("nameLabel derived = %q, want %q", got, "h.example")
	}
	if got := nameLabel(&Provider{}); got != "(unnamed)" {
		t.Errorf("nameLabel empty = %q, want %q", got, "(unnamed)")
	}
}

// TestDeriveName exercises the host-name derivation including IPv6 and api- forms.
// Inputs are port-free Hostname() forms (see validateMulti): IPv6 literals arrive
// bare, and deriveName never needs to strip a port or brackets itself.
func TestDeriveName(t *testing.T) {
	cases := []struct{ host, want string }{
		{"api.anthropic.com", "anthropic"},
		{"api-test.example.com", "test"},
		{"acme.example.com", "acme"},
		{"::1", "::1"},
		{"localhost", "localhost"},
	}
	for _, tc := range cases {
		if got := deriveName(tc.host); got != tc.want {
			t.Errorf("deriveName(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// TestSegmentsOverlap_Asymmetric exercises the swap that makes the longer of the
// two prefixes the iteration upper bound.
func TestSegmentsOverlap_Asymmetric(t *testing.T) {
	// "/a/b" is strictly longer than "/a"; they alias (overlap).
	if !segmentsOverlap("/a/b", "/a") {
		t.Error("segmentsOverlap(/a/b, /a) = false, want true")
	}
	// Disjoint at first differing segment.
	if segmentsOverlap("/a/b", "/a/c") {
		t.Error("segmentsOverlap(/a/b, /a/c) = true, want false")
	}
}

// TestRouteLimiters_ZeroLimitSkip checks that a pattern with Limit 0 contributes
// no limiter (it falls through to the default pool).
func TestResolveAndValidate_ZeroLimitPattern(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-limit", "POST /v1/messages:0",
		"-limit", "POST /free:@pool",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	// Both patterns have Limit 0 -> no route limiters at all.
	if got := cfg.Providers[0].RouteLimiters(); len(got) != 0 {
		t.Errorf("RouteLimiters() = %v, want empty (both patterns have limit 0)", got)
	}
}

// TestStringList_String exercises the flag.Value String() accessor.
func TestStringList_String(t *testing.T) {
	var sl []string
	if got := (&stringList{slice: &sl}).String(); got != "" {
		t.Errorf("empty stringList.String() = %q, want empty", got)
	}
	sl = []string{"a", "b"}
	if got := (&stringList{slice: &sl}).String(); got != "a, b" {
		t.Errorf("stringList.String() = %q, want %q", got, "a, b")
	}
}

// TestResolveAndValidate_NegativeGlobalConcurrency checks the global-concurrency
// lower bound.
func TestResolveAndValidate_NegativeGlobalConcurrency(t *testing.T) {
	cfg := &Config{Providers: []*Provider{{Name: "a", Upstream: "https://x", Concurrency: 4, GlobalConcurrency: -1}}}
	err := cfg.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "global-concurrency must be >= 0") {
		t.Fatalf("error = %v, want 'global-concurrency must be >= 0'", err)
	}
}

// TestResolveAndValidate_MultiEmptyNameNoPrefix checks the anonymous-provider
// (empty name) missing-prefix message.
func TestResolveAndValidate_MultiEmptyNameNoPrefix(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Upstream: "https://a", Prefix: "/a", Concurrency: 4},
			{Name: "", Upstream: "https://b", Prefix: "", Concurrency: 4},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want requires-prefix error")
	}
	if want := "provider section 2: requires -prefix when multiple providers are configured"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestResolveAndValidate_NoDerivableName exercises the unable-to-derive error for
// an upstream whose host yields an empty name.
func TestResolveAndValidate_NoDerivableName(t *testing.T) {
	cfg := &Config{
		Providers: []*Provider{
			{Upstream: "https://", Prefix: "/a", Concurrency: 4},
			{Upstream: "https://a", Prefix: "/b", Concurrency: 4},
		},
	}
	err := cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want unable-to-derive error")
	}
	// Provider 2 is the one with an empty host; it fails when its name is
	// derived after the first resolves.
	if !strings.Contains(err.Error(), "unable to derive a name") {
		t.Errorf("error = %q, want 'unable to derive a name'", err)
	}
}

// TestParse_FlagNeedsArgument checks the missing-value error (tokenize catches it
// before flag parsing).
func TestParse_FlagNeedsArgument(t *testing.T) {
	if _, err := Parse([]string{"-upstream"}); err == nil {
		t.Fatal("Parse(-upstream) = nil error, want 'flag needs an argument'")
	}
}

// TestParse_ProviderSectionFlagParseError exercises the flag-package error inside a
// provider section (a value that fails type parsing).
func TestParse_ProviderSectionFlagParseError(t *testing.T) {
	_, err := Parse([]string{"--provider=acme", "-upstream", "https://x", "-concurrency", "notanint"})
	if err == nil {
		t.Fatal("Parse with invalid -concurrency: nil error, want provider section parse error")
	}
	if !strings.Contains(err.Error(), "provider section 1") {
		t.Errorf("error = %q, want provider section context", err)
	}
}

// TestParse_LegacyFlagParseError exercises the flag-package error in legacy mode.
func TestParse_LegacyFlagParseError(t *testing.T) {
	_, err := Parse([]string{"-upstream", "https://x", "-concurrency", "notanint"})
	if err == nil {
		t.Fatal("Parse with invalid -concurrency: nil error, want error")
	}
}

// TestParsePatterns_Error exercises parsePatterns' direct error path with an
// unparseable pattern (reachable only by direct call, since validateBasic gates it).
func TestParsePatterns_Error(t *testing.T) {
	p := &Provider{Limits: []string{"bad"}}
	if _, err := p.parsePatterns(); err == nil {
		t.Fatal("parsePatterns: nil error, want error")
	}
}

// TestStringList_Nil exercises the nil-receiver String() branch.
func TestStringList_Nil(t *testing.T) {
	if got := (*stringList)(nil).String(); got != "" {
		t.Errorf("nil stringList.String() = %q, want empty", got)
	}
}

// TestParse_ServerSectionFlagParseError exercises the flag-package error inside
// the server (pre-marker) region of a sectioned invocation.
func TestParse_ServerSectionFlagParseError(t *testing.T) {
	_, err := Parse([]string{"-tui=xyz", "--provider=acme", "-upstream", "https://x"})
	if err == nil {
		t.Fatal("Parse with invalid server bool: nil error, want server section parse error")
	}
	if !strings.Contains(err.Error(), "server section") {
		t.Errorf("error = %q, want server section context", err)
	}
}

// TestRouteLimiters_GroupConflictWarn exercises the legacy group-conflict
// warning: when two -limit patterns share a group with different limits, the
// first limiter wins and the disagreement is reported on stderr via log.Printf
// (matching the legacy frontend binary's stream and format).
func TestRouteLimiters_GroupConflictWarn(t *testing.T) {
	prev := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer func() {
		log.SetOutput(prev)
	}()

	limiters := routeLimiters([]string{
		"POST /a:30@anthropic",
		"POST /b:8@anthropic",
	}, 200*time.Millisecond)

	if l := limiters["anthropic"]; l == nil || l.Limit() != 30 {
		t.Errorf(`RouteLimiters()["anthropic"] = %v, want limit 30 (first wins)`, l)
	}

	got := buf.String()
	wantSub := `group "anthropic" with limit 8, but group already has limit 30. Using 30.`
	if !strings.Contains(got, wantSub) {
		t.Errorf("stderr log = %q, want it to contain %q", got, wantSub)
	}
}
