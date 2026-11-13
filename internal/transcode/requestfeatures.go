package transcode

// Sanctioned-encoding names that are not Gemini-specific.
//
// A Note records an encoding the conversion applied without a policy decision,
// so these names are not registered in lossRegistry and need no reachability
// scenario: they make the encoding visible in the per-request log with its
// rationale. Keeping them out of the dialect namespaces matters because
// operators grep the log by dialect, and a Messages event filed under a gemini_*
// key sends them to the wrong place.
const (
	// FeatureMessagesMaxTokensFromModelTable marks an Anthropic Messages
	// render whose required max_tokens came from the model table's max_output
	// fact because the source request carried no output limit (a Gemini client
	// may omit maxOutputTokens, and the Messages API rejects a request without
	// max_tokens).
	FeatureMessagesMaxTokensFromModelTable Feature = "messages_max_tokens_from_model_table"
	// FeatureToolNamespaceFlatten marks a namespace tool whose nested function
	// tools flattened into ordinary function tools. The grouping is client-side
	// structure the target cannot express and the functions themselves are
	// fully portable, so the flatten is a sanctioned encoding: observable, but
	// never a policy decision, and never filed under builtin_tools (which means
	// a tool was DROPPED).
	FeatureToolNamespaceFlatten Feature = "tool_namespace_flatten"
	// FeatureToolChoiceNamespaceBind marks a named tool_choice the CLIENT sent
	// as a bare child name that a plain function owns, while a namespace child
	// of the same bare name exists under a qualified flat name. The plain
	// function is what the wire carries, and that is a plausible reading of the
	// client's intent — but the client may have meant the child, so the bind is
	// recorded rather than left silent. It is its OWN key, not
	// namespace_replay_undeclared: that key means a REPLAYED CALL named a
	// namespace this exchange does not declare, and filing a choice bind there
	// sends an operator grepping either meaning to the other.
	FeatureToolChoiceNamespaceBind Feature = "tool_choice_namespace_bind"
)
