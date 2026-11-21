package transcode

// sourceChatUsage preserves absence of a component the decoder derived for the
// target dialect. A displayed/forwarded derived total is not a reported total.
func sourceChatUsage(chunk ChatStreamResponse) CanonicalUsage {
	if chunk.Usage == nil {
		return CanonicalUsage{}
	}
	u := chunk.Usage
	return CanonicalUsage{InputTokens: int64(u.PromptTokens), OutputTokens: int64(u.CompletionTokens), TotalTokens: int64(u.TotalTokens), InputKnown: chunk.DerivedUsageTotal != "prompt_tokens", OutputKnown: chunk.DerivedUsageTotal != "completion_tokens", TotalKnown: chunk.DerivedUsageTotal != "total_tokens"}
}
func sourceGeminiUsage(s *geminiResponsesStreamState) CanonicalUsage {
	if s == nil || s.usage == nil {
		return CanonicalUsage{}
	}
	return CanonicalUsage{InputTokens: s.usage.InputTokens, OutputTokens: s.usage.OutputTokens, TotalTokens: s.usage.TotalTokens, InputKnown: s.usagePresence.input, OutputKnown: s.usagePresence.output, TotalKnown: s.usagePresence.total}
}
func sourceMessagesUsage(s *anthropicUpstreamResponsesStreamState) CanonicalUsage {
	if s == nil || s.startUsage == nil || s.finalUsage == nil {
		return CanonicalUsage{}
	}
	u := messagesUsage(s.startUsage)
	u.OutputTokens = int64(s.finalUsage.OutputTokens)
	u.OutputKnown = true
	// Messages does not report a total; the Responses intermediate derives it.
	u.TotalKnown = false
	u.TotalTokens = 0
	return u
}

// sourceStreamUsage is called only after a valid terminal was converted,
// admitted by the output bounds and delivered. None of these paths parse bytes
// again: they read the source state already accepted by the codec.
func sourceStreamUsage(converter frameConverter) CanonicalUsage {
	switch c := converter.(type) {
	case *chatToResponsesConverter:
		return c.state.observedUsage
	case *chatToAnthropicConverter:
		return c.chat.observedUsage
	case *chatToGeminiConverter:
		return c.chat.observedUsage
	case *responsesToAnthropicConverter:
		return c.state.observedUsage
	case *responsesToGeminiConverter:
		return c.state.observedUsage
	case *geminiToResponsesConverter:
		return sourceGeminiUsage(c.state)
	case *geminiToAnthropicConverter:
		return sourceGeminiUsage(c.gemini)
	case *messagesToGeminiConverter:
		return sourceMessagesUsage(c.upstream)
	default:
		return CanonicalUsage{}
	}
}
