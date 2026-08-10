package transcode

// The OpenAI Responses wire definitions live in the pinned wire package
// wire/openairesponses (see contracts.lock.json); this file re-exports them
// under the package's historical names so consumers compile unchanged. New
// code should prefer the wire package names.

import "github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"

// ResponsesTool is a function tool definition in a Responses request or
// response echo. Strict is required on the wire: a tool whose strict key is
// missing or null is malformed under the pin.
type ResponsesTool = openairesponses.Tool

// ResponsesToolChoiceNamed is the named-function arm of tool_choice.
type ResponsesToolChoiceNamed = openairesponses.ToolChoiceNamed

// ResponsesToolChoice is the tool_choice union: a plain string ("none",
// "auto", "required") or a named function object.
type ResponsesToolChoice = openairesponses.ToolChoice

// ResponsesEnvelopeError is the error object of a failed response envelope.
type ResponsesEnvelopeError = openairesponses.EnvelopeError

// ResponsesEnvelopePrompt is the typed shadow of the pinned ResponsePrompt.
type ResponsesEnvelopePrompt = openairesponses.Prompt

// ResponsesIncompleteDetails explains why a response is incomplete.
type ResponsesIncompleteDetails = openairesponses.IncompleteDetails

// ResponsesUsage is the token usage of a response envelope.
type ResponsesUsage = openairesponses.Usage

// UsageInputTokensDetails breaks down input tokens.
type UsageInputTokensDetails = openairesponses.UsageInputTokensDetails

// UsageOutputTokensDetails breaks down output tokens.
type UsageOutputTokensDetails = openairesponses.UsageOutputTokensDetails

// ResponsesEnvelopeReasoning echoes the request reasoning configuration.
type ResponsesEnvelopeReasoning = openairesponses.Reasoning

// ResponsesEnvelopeText echoes the request text configuration.
type ResponsesEnvelopeText = openairesponses.Text

// ResponsesEnvelopeTextFormat is the format arm of text config.
type ResponsesEnvelopeTextFormat = openairesponses.TextFormat

// ResponseEnvelope is the strict response object embedded in
// response.created, response.in_progress, and the terminal events, and used
// as the non-streaming Responses response body. The 14 pinned required
// fields are ALWAYS emitted — with explicit null, {}, or [] where the
// contract requires the key but has no value. created_at is float64.
type ResponseEnvelope = openairesponses.Response

// ResponsesRequestEcho captures request-derived state required to
// reconstruct the client Responses response envelope. A stateless
// convertResponse(body) cannot faithfully construct a Responses response;
// the exchange context carries the original request so the envelope can be
// rebuilt.
//
// The effective values (ParallelToolCalls, Temperature, ToolChoice, TopP)
// carry the pinned API defaults applied during request decoding, so the
// renderer can emit a complete response without guessing later.
type ResponsesRequestEcho struct {
	Instructions *ResponsesInput
	Metadata     map[string]string

	ParallelToolCalls bool
	Temperature       float64
	ToolChoice        ResponsesToolChoice
	Tools             []ResponsesTool
	TopP              float64

	// Nullable or genuinely optional echo fields remain pointers.
	MaxOutputTokens    *int
	PreviousResponseID *string
	Store              *bool
	Truncation         *string
	User               *string
	Reasoning          *ResponsesEnvelopeReasoning
	Text               *ResponsesEnvelopeText
	ServiceTier        *string
	TopLogprobs        *int64
}

// MessagesRequestContext captures the request-derived state of a Messages
// source request needed to reconstruct the client response envelope.
type MessagesRequestContext struct {
	MaxTokens int
	Metadata  map[string]string
}
