package transcode

import (
	"fmt"
)

// ModelMapping maps a client model identifier to the upstream model identifier
// and the stable client-facing alias returned in the converted response.
//
// https://platform.openai.com/docs/api-reference/responses
// https://platform.claude.com/docs/en/api/messages
type ModelMapping struct {
	ClientModel   string
	UpstreamModel string

	// ClientResponseModel is the stable client-facing alias returned in the
	// converted response. It should normally equal ClientModel.
	ClientResponseModel string

	// ReasoningTier pins the reasoning effort tier for this model ("low",
	// "medium", "high", or "" when unset). Empty means no tier override.
	ReasoningTier string
}

// ModelMap resolves client model identifiers to upstream model identifiers.
type ModelMap struct {
	Exact              map[string]ModelMapping
	AllowIdentity      bool
	RequireExplicitMap bool
}

// ProfileMapping maps a profile name to a model and reasoning tier. Profiles
// are used by MultiAgent V2 to dispatch child agents with distinct
// capabilities.
type ProfileMapping struct {
	Model         string
	ReasoningTier string // "low", "medium", "high", or "" (unset)
}

// ProfileMap resolves profile names to model+tier pairs. When a profile is
// resolved, its target model is resolved through the route's ModelMap.
type ProfileMap struct {
	Profiles map[string]ProfileMapping
}

// ResolveProfile returns the mapping for the profile name. If the profile
// is not in the map, ok is false and the caller should fall through to
// ModelMap.Resolve. The profile's model is returned as the client model to
// resolve; the tier is returned separately so the caller can apply it to
// the rendered request.
func (p ProfileMap) ResolveProfile(profileName string) (clientModel string, tier string, ok bool) {
	if p.Profiles == nil {
		return "", "", false
	}
	mapping, found := p.Profiles[profileName]
	if !found {
		return "", "", false
	}
	return mapping.Model, mapping.ReasoningTier, true
}

// Resolve returns the mapping for the client model. With identity fallback,
// an unmapped model is passed through unchanged; otherwise it is an error.
// The actual upstream model is never leaked into the client response: the
// client-facing alias is returned instead.
func (m ModelMap) Resolve(clientModel string) (ModelMapping, error) {
	if mapping, ok := m.Exact[clientModel]; ok {
		if mapping.ClientResponseModel == "" {
			mapping.ClientResponseModel = clientModel
		}
		return mapping, nil
	}
	if m.AllowIdentity && !m.RequireExplicitMap {
		return ModelMapping{
			ClientModel:         clientModel,
			UpstreamModel:       clientModel,
			ClientResponseModel: clientModel,
		}, nil
	}
	return ModelMapping{}, fmt.Errorf(
		"no upstream model mapping for client model %q",
		clientModel,
	)
}
