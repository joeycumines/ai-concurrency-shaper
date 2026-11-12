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
)
