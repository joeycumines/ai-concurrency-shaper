package transcode

// derefStr returns the value of s, or "" when s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nonEmpty reports whether a presence-aware string pointer carries a
// non-empty value: the provider reasoning spellings (reasoning and
// reasoning_content) treat present-but-empty exactly like absent, matching
// the stream delta idiom.
func nonEmpty(s *string) bool {
	return s != nil && *s != ""
}

// chatProviderReasoningDroppedDetail is the single loss detail reported when
// provider plaintext reasoning must be dropped because the
// ProviderReasoningText capability is not enabled. One constant, shared
// verbatim by the non-stream and stream chat surfaces, so a capability-off
// exchange produces observably identical loss text through both conversion
// paths (de-asymmetry).
const chatProviderReasoningDroppedDetail = "provider reasoning is dropped because the provider_reasoning_text capability is not enabled"

// chatProviderReasoningMappedDetail is the single note detail reported when
// provider plaintext reasoning maps to ordinary text under the enabled
// ProviderReasoningText capability. One constant, shared verbatim by the
// non-stream and stream chat surfaces, so a capability-on exchange produces
// observably identical note text through both conversion paths (de-asymmetry).
const chatProviderReasoningMappedDetail = "provider reasoning maps to ordinary text (provider_reasoning_text encoding)"

// chatProviderReasoningBothDetail is the single loss detail reported when a
// chat response carries two provider reasoning spellings at once (a non-stream
// message with both, or one stream delta carrying both) — the contradiction
// that is never an ordered merge. One constant, shared verbatim by the
// non-stream and stream chat surfaces, so the both-spellings disposition has
// exactly one spelling across both conversion paths (de-asymmetry).
const chatProviderReasoningBothDetail = "chat response carries both reasoning and reasoning_content"

// resolveChatReasoningSpelling resolves the two provider spelling of chat
// plaintext reasoning — `reasoning_content` (the DeepSeek/Qwen convention)
// and `reasoning` (OpenRouter style) — into a single text and its wire path.
// They are one logical field shared by the non-stream and stream chat
// surfaces, so the resolution invariant has exactly one implementation
// A present-but-empty value counts as absent; two
// non-empty spellings at once are contradictory upstream wire and reported by
// returning empty text with both set. The caller supplies the per-surface
// report paths and owns the surface-specific rejection/encoding decision.
func resolveChatReasoningSpelling(
	reasoning, reasoningContent *string,
	reasoningPath, reasoningContentPath string,
) (text, path string, both bool) {
	switch {
	case nonEmpty(reasoning) && nonEmpty(reasoningContent):
		return "", "", true
	case nonEmpty(reasoningContent):
		return *reasoningContent, reasoningContentPath, false
	case nonEmpty(reasoning):
		return *reasoning, reasoningPath, false
	}
	return "", "", false
}
