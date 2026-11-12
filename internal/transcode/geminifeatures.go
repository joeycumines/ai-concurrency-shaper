package transcode

// Gemini sanctioned encodings. A Note records an encoding the conversion
// applied without a policy decision, so these names are not registered in
// lossRegistry and need no reachability scenario: they make the encoding
// visible in the per-request log with its rationale.
const (
	// FeatureGeminiBodyModel marks a request body model that matched the path
	// model and was therefore not forwarded.
	FeatureGeminiBodyModel Feature = "gemini_body_model"
	// FeatureGeminiRoleInferred marks a content with no role, read as a user
	// turn.
	FeatureGeminiRoleInferred Feature = "gemini_role_inferred"
	// FeatureGeminiCallIDSynthesized marks a functionCall without an id that
	// received a proxy-minted id.
	FeatureGeminiCallIDSynthesized Feature = "gemini_call_id_synthesized"
	// FeatureGeminiCallCorrelation marks id-less functionResponse parts that
	// were paired with their calls by order.
	FeatureGeminiCallCorrelation Feature = "gemini_call_correlation"
	// FeatureGeminiFunctionResponseJSON marks a functionResponse object that
	// is not the output or error shape and was carried as compact JSON text.
	FeatureGeminiFunctionResponseJSON Feature = "gemini_function_response_json"
	// FeatureGeminiSchemaConverted marks an OpenAPI-style schema converted to
	// JSON Schema.
	FeatureGeminiSchemaConverted Feature = "gemini_schema_converted"
	// FeatureGeminiRefusalText marks a refusal rendered as ordinary text.
	FeatureGeminiRefusalText Feature = "gemini_refusal_text"
	// FeatureGeminiFinishRefusal marks a refusal finish reason rendered as the
	// SAFETY finish reason.
	FeatureGeminiFinishRefusal Feature = "gemini_finish_refusal"
	// FeatureGeminiThoughtText marks thinking text rendered as a Gemini
	// thought part because the client asked for thoughts.
	FeatureGeminiThoughtText Feature = "gemini_thought_text"
	// FeatureGeminiEmptyContent marks a candidate with no parts, rendered with
	// one empty text part so SDK text accessors do not fail.
	FeatureGeminiEmptyContent Feature = "gemini_empty_content"
	// FeatureGeminiFunctionResponseObject marks a function result carried to
	// Gemini as the {"output"|"error": text} object shape.
	FeatureGeminiFunctionResponseObject Feature = "gemini_function_response_object"
	// FeatureGeminiPromptBlocked marks a blocked prompt (promptFeedback
	// blockReason) rendered as a completed refusal.
	FeatureGeminiPromptBlocked Feature = "gemini_prompt_blocked"
	// FeatureGeminiStructuredOutputEnforced marks a client best-effort
	// structured output that Gemini nevertheless constrains to the schema.
	FeatureGeminiStructuredOutputEnforced Feature = "gemini_structured_output_enforced"
)
