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

package policy

import (
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/client"
)

// Behavior defines an action that can be applied to a request
// when a CEL policy matches. Behaviors are switchable — they
// can be enabled or disabled at runtime.
type Behavior interface {
	// Name returns the behavior's unique identifier.
	Name() string
	// IsEnabled returns whether this behavior is currently active.
	IsEnabled() bool
	// SetEnabled toggles the behavior on or off.
	SetEnabled(bool)
	// AppliesTo returns true if this behavior should be evaluated
	// for the given client state. When false, the behavior defers
	// to the default retry policy.
	AppliesTo(*client.ClientState) bool
	// CheckRetry determines whether a failed request should be
	// retried based on this behavior's policy. When this returns
	// true, the retry transport will attempt another request.
	// When it returns false, the default retry policy applies.
	CheckRetry(req *http.Request, resp *http.Response, err error) bool
}

// DefaultBehavior is a no-op behavior that always defers to
// the default retry policy.
type DefaultBehavior struct{}

// Name returns "default".
func (b *DefaultBehavior) Name() string { return "default" }

// IsEnabled returns false — the default behavior is never the
// active policy match; it is the fallback when no policy matches.
func (b *DefaultBehavior) IsEnabled() bool { return false }

// SetEnabled is a no-op for the default behavior.
func (b *DefaultBehavior) SetEnabled(bool) {}

// AppliesTo returns true so the default retry policy is always
// evaluated as a fallback when no policy-specific behavior matches.
func (b *DefaultBehavior) AppliesTo(*client.ClientState) bool { return true }

// CheckRetry always returns false so the default retry policy
// is used when no policy matches.
func (b *DefaultBehavior) CheckRetry(req *http.Request, resp *http.Response, err error) bool {
	return false
}

// RetryOn403Behavior retries requests that receive a 403 response,
// treating client errors as retryable for specific originators.
type RetryOn403Behavior struct {
	enabled bool
}

// NewRetryOn403Behavior creates a RetryOn403Behavior that is initially enabled.
func NewRetryOn403Behavior() *RetryOn403Behavior {
	return &RetryOn403Behavior{enabled: true}
}

// Name returns "retry-on-403".
func (b *RetryOn403Behavior) Name() string { return "retry-on-403" }

// IsEnabled returns whether this behavior is currently active.
func (b *RetryOn403Behavior) IsEnabled() bool { return b.enabled }

// SetEnabled toggles the behavior on or off.
func (b *RetryOn403Behavior) SetEnabled(enabled bool) { b.enabled = enabled }

// AppliesTo returns true for all client states.
// RetryOn403Behavior does not require the client to be in
// any particular state; it applies whenever the policy matches.
func (b *RetryOn403Behavior) AppliesTo(*client.ClientState) bool { return true }

// CheckRetry returns true when enabled and the response is a 403,
// causing the retry transport to retry 403 responses for matched clients.
// Transport errors are also retried (returned as true).
// Non-403 responses return false, deferring to the default retry policy.
// The req parameter is reserved for future originator-aware retry decisions.
func (b *RetryOn403Behavior) CheckRetry(req *http.Request, resp *http.Response, err error) bool {
	if !b.enabled {
		return false
	}
	_ = req // reserved for originator-aware decisions
	if err != nil {
		return true
	}
	return resp != nil && resp.StatusCode == http.StatusForbidden
}

// CrushedClientBehavior retries requests for clients that have been
// identified as "crushed" (experiencing repeated failures). When active
// and the client is crushed, 403, 429, and 5xx responses are all
// considered retryable regardless of the default retry policy.
type CrushedClientBehavior struct {
	enabled bool
}

// NewCrushedClientBehavior creates a CrushedClientBehavior that is initially enabled.
func NewCrushedClientBehavior() *CrushedClientBehavior {
	return &CrushedClientBehavior{enabled: true}
}

// Name returns "crushed-client-retry".
func (b *CrushedClientBehavior) Name() string { return "crushed-client-retry" }

// IsEnabled returns whether this behavior is currently active.
func (b *CrushedClientBehavior) IsEnabled() bool { return b.enabled }

// SetEnabled toggles the behavior on or off.
func (b *CrushedClientBehavior) SetEnabled(enabled bool) { b.enabled = enabled }

// AppliesTo returns true only when the client is known to be
// crushed (failure count exceeds the threshold within the window).
// When the client is not crushed or no state is recorded, the
// behavior defers to the default retry policy.
func (b *CrushedClientBehavior) AppliesTo(s *client.ClientState) bool {
	return s != nil && s.IsCrushed
}

// CheckRetry returns true when enabled and the client is crushed
// and the response indicates a failure (403, 429, or 5xx).
// Transport errors are also retried (returned as true).
// When this behavior is disabled or the client is not crushed,
// it returns false so the default retry policy applies.
// The req parameter is reserved for future originator-aware retry decisions.
func (b *CrushedClientBehavior) CheckRetry(req *http.Request, resp *http.Response, err error) bool {
	if !b.enabled {
		return false
	}
	_ = req // reserved for originator-aware decisions
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= 500
}
