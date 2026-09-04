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
}

// ModelMap resolves client model identifiers to upstream model identifiers.
type ModelMap struct {
	Exact              map[string]ModelMapping
	AllowIdentity      bool
	RequireExplicitMap bool
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
