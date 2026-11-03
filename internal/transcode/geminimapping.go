package transcode

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// validateGeminiShape enforces the Gemini-specific mapping rules that apply
// before any direction is considered: the client route template (only Gemini
// routes may contain the {model} placeholder, which every other route rejects
// below), the Gemini upstream path and bare model ids, and the
// credential-free client query allowlist.
func (m Mapping) validateGeminiShape() error {
	if m.ClientProtocol == ClientGemini {
		if _, err := ParseGeminiClientRoutePath(m.ClientRoute.Path); err != nil {
			return err
		}
	} else if strings.ContainsAny(m.ClientRoute.Path, "{}") {
		return fmt.Errorf(
			"route path %q contains a template placeholder; only gemini routes accept {model}",
			m.ClientRoute.Path,
		)
	}

	if m.UpstreamProtocol == UpstreamGemini {
		if canonicalizeRoutePath(m.UpstreamPath) != m.UpstreamPath ||
			strings.ContainsAny(m.UpstreamPath, "{}:?#%") {
			return fmt.Errorf(
				"gemini upstream path %q must be canonical and must not contain '{', '}', ':', '?', '#', or '%%'",
				m.UpstreamPath,
			)
		}
	}
	// A via=gemini entry names a model served natively by Gemini, so its
	// upstream model must be a bare id whatever protocol this mapping uses.
	// Entries with another or an empty Via are not checked here: the model
	// map is provider-wide.
	for _, clientModel := range slices.Sorted(maps.Keys(m.ModelMap.Exact)) {
		mapping := m.ModelMap.Exact[clientModel]
		if mapping.Via == NativeGemini && !ValidCatalogIdent(mapping.UpstreamModel) {
			return fmt.Errorf(
				"gemini upstream model %q must be a bare model id such as gemini-2.5-pro (no models/ prefix, no '/' or ':')",
				mapping.UpstreamModel,
			)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(m.AllowedClientQuery)) {
		switch strings.ToLower(name) {
		case "key", "access_token", "oauth_token":
			return fmt.Errorf("client query parameter %q carries a credential and cannot be allowed", name)
		}
	}
	return nil
}
