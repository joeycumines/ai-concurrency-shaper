package openairesponses

import (
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Request is the Responses create-request wire contract (the pinned
// ResponseNewParams subset the transcoder supports). It is strictly
// request-shaped: request optionality never leaks into the response object
// (see Response, whose required fields are always emitted).
//
// Stream is presence-aware (wire.Field): the request body's stream field
// must be distinguishable from absent so the handler can apply the
// documented stream-intent precedence — an explicit stream:false is
// authoritative over the Accept header.
//
// Instructions is a plain string on the create request (the pinned
// ResponseNewParams shape); the string-or-item-list union exists only on the
// response echo.
type Request struct {
	Model string `json:"model"`
	Input *Input `json:"input,omitempty"`

	Instructions       *string           `json:"instructions,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID *string           `json:"previous_response_id,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Truncation         *string           `json:"truncation,omitempty"`
	User               *string           `json:"user,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Tools              []Tool            `json:"tools,omitempty"`
	ToolChoice         *ToolChoice       `json:"tool_choice,omitempty"`
	Reasoning          *Reasoning        `json:"reasoning,omitempty"`
	Text               *Text             `json:"text,omitempty"`
	ServiceTier        *string           `json:"service_tier,omitempty"`
	TopLogprobs        *int64            `json:"top_logprobs,omitempty"`
	// Stream is presence-aware (wire.Field) and omitted when absent via
	// omitzero (which consults Field.IsZero): a non-streaming exchange
	// carries no stream key, while an explicit stream:true or stream:false
	// is emitted as a bare boolean.
	Stream wire.Field[bool] `json:"stream,omitzero"`

	// Typed shadow fields for the pinned create-request controls the
	// transcoder deliberately does not implement: they decode so strict
	// decoding never fails on a current official request, and their presence
	// is a typed unsupported-feature rejection at the request boundary
	// (probeUnsupportedResponsesFields), never a silent drop. include and
	// client_metadata are accepted (best-effort / telemetry, reported at the
	// transcode boundary); prompt_cache_key is the responses_controls loss
	// decision.
	Include          []string       `json:"include,omitempty"`
	Prompt           *Prompt        `json:"prompt,omitempty"`
	Background       *bool          `json:"background,omitempty"`
	MaxToolCalls     *int64         `json:"max_tool_calls,omitempty"`
	SafetyIdentifier string         `json:"safety_identifier,omitempty"`
	PromptCacheKey   string         `json:"prompt_cache_key,omitempty"`
	Status           string         `json:"status,omitempty"`
	ClientMetadata   map[string]any `json:"client_metadata,omitempty"`
}
