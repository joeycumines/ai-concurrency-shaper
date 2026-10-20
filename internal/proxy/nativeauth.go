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

package proxy

import (
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// nativeAuthVariants holds the provider's authentication policy together
// with the dialect-adapted variants the native serving path can select,
// precomputed once at construction so the Rewrite hot path selects a
// ready pointer instead of building a policy per request. The variants
// mirror what transcode.FromProviderAuthForUpstream applies on the
// transcode path, so one mount authenticates a model identically whether
// served natively or converted.
type nativeAuthVariants struct {
	// provider is the mount's policy, applied verbatim for dialects with
	// no adaptation and for non-alias requests.
	provider *auth.AuthPolicy
	// messages adapts a bearer mount for NativeMessages: X-Api-Key plus
	// the default Anthropic version when the policy carries none.
	messages *auth.AuthPolicy
	// openai adapts an x-api-key mount for NativeChat and
	// NativeResponses: Bearer.
	openai *auth.AuthPolicy
}

// newNativeAuthVariants precomputes the adapted variants of a provider
// policy: faithful field copies overriding only Mode and, for the Messages
// variant, AnthropicVersion. A nil policy yields an empty variant set.
func newNativeAuthVariants(policy *auth.AuthPolicy) nativeAuthVariants {
	variants := nativeAuthVariants{provider: policy}
	if policy == nil {
		return variants
	}
	switch policy.Mode {
	case auth.AuthBearer:
		version := policy.AnthropicVersion
		if strings.TrimSpace(version) == "" {
			version = auth.DefaultAnthropicVersion
		}
		messages := *policy
		messages.Mode = auth.AuthXAPIKey
		messages.AnthropicVersion = version
		variants.messages = &messages
	case auth.AuthXAPIKey:
		openai := *policy
		openai.Mode = auth.AuthBearer
		variants.openai = &openai
	}
	return variants
}

// forProtocol selects the precomputed policy for a native dialect: the
// adapted variant when this policy mode has one for the dialect, else the
// provider policy verbatim.
func (v nativeAuthVariants) forProtocol(protocol transcode.NativeProtocol) *auth.AuthPolicy {
	switch protocol {
	case transcode.NativeMessages:
		if v.messages != nil {
			return v.messages
		}
	case transcode.NativeChat, transcode.NativeResponses:
		if v.openai != nil {
			return v.openai
		}
	}
	return v.provider
}
