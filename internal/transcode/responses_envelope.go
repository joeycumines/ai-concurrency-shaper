package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Source contracts:
//
// Response object:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L670-L900
//
// Response usage:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L10946-L11012
//
// Response error:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L2939-L2990
//
// Response incomplete details:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L885-L910
//
// Response text config:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L10711-L10780
//
// Response tool choice:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L948-L1008

// ResponsesTool is a function tool definition in a Responses request or
// response echo. Only function tools are part of the supported subset;
// built-in tools (web search, file search, code interpreter, computer use,
// and so on) are rejected as unsupported features.
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Validate checks the function-tool shape.
func (t ResponsesTool) Validate() error {
	if t.Type != "function" {
		return fmt.Errorf("responses tool type = %q, want function", t.Type)
	}
	if t.Name == "" {
		return errors.New("responses tool name is empty")
	}
	return nil
}

// ResponsesToolChoiceNamed is the named-function arm of tool_choice.
type ResponsesToolChoiceNamed struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ResponsesToolChoice is the tool_choice union: a plain string ("none",
// "auto", "required") or a named function object. Unknown variants are
// rejected rather than silently defaulted to auto.
type ResponsesToolChoice struct {
	Str   *string
	Named *ResponsesToolChoiceNamed
}

// Validate checks the union invariants.
func (c ResponsesToolChoice) Validate() error {
	if c.Str != nil && c.Named != nil {
		return errors.New("tool choice has both string and named variants")
	}
	if c.Named != nil {
		if c.Named.Type != "function" {
			return fmt.Errorf("tool choice type = %q, want function", c.Named.Type)
		}
		if c.Named.Name == "" {
			return errors.New("named tool choice name is empty")
		}
		return nil
	}
	if c.Str != nil {
		switch *c.Str {
		case "none", "auto", "required":
			return nil
		default:
			return fmt.Errorf("invalid tool choice %q", *c.Str)
		}
	}
	return errors.New("tool choice has no selected variant")
}

// UnmarshalJSON decodes the string or object arm, rejecting unknown variants
// at decode time so a decode-accepted value always validates.
func (c *ResponsesToolChoice) UnmarshalJSON(data []byte) error {
	data = trimJSONSpace(data)
	if len(data) == 0 {
		return errors.New("empty tool choice")
	}
	if data[0] == '"' {
		var str string
		if err := strictDecode(data, &str); err != nil {
			return err
		}
		c.Str = &str
		c.Named = nil
	} else {
		var named ResponsesToolChoiceNamed
		if err := strictDecode(data, &named); err != nil {
			return err
		}
		c.Str = nil
		c.Named = &named
	}
	return c.Validate()
}

// MarshalJSON emits the selected arm.
func (c ResponsesToolChoice) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	return json.Marshal(c.Named)
}

// ResponsesEnvelopeError is the error object of a failed response envelope.
type ResponsesEnvelopeError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ResponsesEnvelopePrompt is the typed shadow of the pinned ResponsePrompt:
// prompt template configuration echoed on the envelope. The transcoder does
// not implement prompt templates; presence enters the explicit loss/reject
// decision (review-j finding 13).
type ResponsesEnvelopePrompt struct {
	ID        string                     `json:"id"`
	Variables map[string]json.RawMessage `json:"variables,omitempty"`
	Version   string                     `json:"version,omitempty"`
}

// ResponsesIncompleteDetails explains why a response is incomplete.
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ResponsesUsage is the token usage of a response envelope. The details
// objects are always present on the wire (zero when unused).
type ResponsesUsage struct {
	InputTokens         int64                     `json:"input_tokens"`
	InputTokensDetails  *UsageInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int64                     `json:"output_tokens"`
	OutputTokensDetails *UsageOutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int64                     `json:"total_tokens"`
}

// UsageInputTokensDetails breaks down input tokens.
type UsageInputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// UsageOutputTokensDetails breaks down output tokens.
type UsageOutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ResponsesEnvelopeReasoning echoes the request reasoning configuration. Both
// effort and summary are string enums on the wire ("low"/"medium"/"high" and
// "auto"/"concise"/"detailed"), matching the official Reasoning type.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L12866
type ResponsesEnvelopeReasoning struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// ResponsesEnvelopeText echoes the request text configuration.
type ResponsesEnvelopeText struct {
	Format *ResponsesEnvelopeTextFormat `json:"format,omitempty"`
}

// ResponsesEnvelopeTextFormat is the format arm of text config.
type ResponsesEnvelopeTextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponseEnvelope is the strict response object embedded in
// response.created, response.in_progress, and the terminal events, and used
// as the non-streaming Responses response body. Every official field is
// modeled; nullable fields are emitted as null when unknown rather than
// omitted.
type ResponseEnvelope struct {
	ID        string                `json:"id"`
	Object    string                `json:"object"`
	CreatedAt int64                 `json:"created_at"`
	Status    string                `json:"status"`
	Model     string                `json:"model"`
	Output    []ResponsesOutputItem `json:"output"`

	Error             *ResponsesEnvelopeError     `json:"error"`
	IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details"`

	// Typed shadow fields for the pinned envelope controls (review-j
	// finding 13): they decode so strict decoding never fails on a current
	// official response, and their presence enters the explicit loss/reject
	// decision at render time.
	Background       *bool                    `json:"background,omitempty"`
	MaxToolCalls     *int64                   `json:"max_tool_calls,omitempty"`
	Prompt           *ResponsesEnvelopePrompt `json:"prompt,omitempty"`
	PromptCacheKey   string                   `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier string                   `json:"safety_identifier,omitempty"`

	Instructions       *ResponsesInput             `json:"instructions,omitempty"`
	MaxOutputTokens    *int64                      `json:"max_output_tokens,omitempty"`
	ParallelToolCalls  *bool                       `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID *string                     `json:"previous_response_id,omitempty"`
	Reasoning          *ResponsesEnvelopeReasoning `json:"reasoning,omitempty"`
	Store              *bool                       `json:"store,omitempty"`
	Temperature        *float64                    `json:"temperature,omitempty"`
	Text               *ResponsesEnvelopeText      `json:"text,omitempty"`
	ToolChoice         *ResponsesToolChoice        `json:"tool_choice,omitempty"`
	Tools              []ResponsesTool             `json:"tools,omitempty"`
	TopP               *float64                    `json:"top_p,omitempty"`
	TopLogprobs        *int64                      `json:"top_logprobs,omitempty"`
	Truncation         *string                     `json:"truncation,omitempty"`
	User               *string                     `json:"user,omitempty"`
	Metadata           map[string]string           `json:"metadata,omitempty"`
	ServiceTier        *string                     `json:"service_tier,omitempty"`
	Usage              *ResponsesUsage             `json:"usage,omitempty"`
}

// Validate checks the required envelope fields.
func (r ResponseEnvelope) Validate() error {
	if r.ID == "" {
		return errors.New("response id is empty")
	}
	if r.Object != "response" {
		return fmt.Errorf("response object = %q", r.Object)
	}
	if r.Model == "" {
		return errors.New("response model is empty")
	}
	if r.Output == nil {
		return errors.New("response output must be present; use an empty array")
	}
	for i, item := range r.Output {
		if item == nil {
			return fmt.Errorf("response output item %d is nil", i)
		}
		if err := item.Validate(); err != nil {
			return fmt.Errorf("response output item %d: %w", i, err)
		}
	}
	for i, tool := range r.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("response tools item %d: %w", i, err)
		}
	}
	return nil
}

// UnmarshalJSON strictly decodes the envelope. Because the official Responses
// API returns many request-derived echo fields, the full envelope is accepted
// and validated field-by-field; unknown fields are rejected.
func (r *ResponseEnvelope) UnmarshalJSON(data []byte) error {
	// Decode into a shadow struct whose output field is raw so the tagged
	// union can be dispatched through DecodeResponsesOutputItem.
	var shadow struct {
		ID                 string                      `json:"id"`
		Object             string                      `json:"object"`
		CreatedAt          int64                       `json:"created_at"`
		Status             string                      `json:"status"`
		Model              string                      `json:"model"`
		Output             json.RawMessage             `json:"output"`
		Error              *ResponsesEnvelopeError     `json:"error"`
		IncompleteDetails  *ResponsesIncompleteDetails `json:"incomplete_details"`
		Background         *bool                       `json:"background"`
		MaxToolCalls       *int64                      `json:"max_tool_calls"`
		Prompt             *ResponsesEnvelopePrompt    `json:"prompt"`
		PromptCacheKey     string                      `json:"prompt_cache_key"`
		SafetyIdentifier   string                      `json:"safety_identifier"`
		Instructions       *ResponsesInput             `json:"instructions"`
		MaxOutputTokens    *int64                      `json:"max_output_tokens"`
		ParallelToolCalls  *bool                       `json:"parallel_tool_calls"`
		PreviousResponseID *string                     `json:"previous_response_id"`
		Reasoning          *ResponsesEnvelopeReasoning `json:"reasoning"`
		Store              *bool                       `json:"store"`
		Temperature        *float64                    `json:"temperature"`
		Text               *ResponsesEnvelopeText      `json:"text"`
		ToolChoice         *ResponsesToolChoice        `json:"tool_choice"`
		Tools              []ResponsesTool             `json:"tools"`
		TopP               *float64                    `json:"top_p"`
		TopLogprobs        *int64                      `json:"top_logprobs"`
		Truncation         *string                     `json:"truncation"`
		User               *string                     `json:"user"`
		Metadata           map[string]string           `json:"metadata"`
		ServiceTier        *string                     `json:"service_tier"`
		Usage              *ResponsesUsage             `json:"usage"`
	}
	if err := strictDecode(data, &shadow); err != nil {
		return err
	}

	var items []ResponsesOutputItem
	if len(shadow.Output) != 0 && string(shadow.Output) != "null" {
		var rawItems []json.RawMessage
		if err := json.Unmarshal(shadow.Output, &rawItems); err != nil {
			return err
		}
		items = make([]ResponsesOutputItem, 0, len(rawItems))
		for i, rawItem := range rawItems {
			item, err := DecodeResponsesOutputItem(rawItem)
			if err != nil {
				return fmt.Errorf("output item %d: %w", i, err)
			}
			items = append(items, item)
		}
	}

	*r = ResponseEnvelope{
		ID:                 shadow.ID,
		Object:             shadow.Object,
		CreatedAt:          shadow.CreatedAt,
		Status:             shadow.Status,
		Model:              shadow.Model,
		Output:             items,
		Error:              shadow.Error,
		IncompleteDetails:  shadow.IncompleteDetails,
		Background:         shadow.Background,
		MaxToolCalls:       shadow.MaxToolCalls,
		Prompt:             shadow.Prompt,
		PromptCacheKey:     shadow.PromptCacheKey,
		SafetyIdentifier:   shadow.SafetyIdentifier,
		Instructions:       shadow.Instructions,
		MaxOutputTokens:    shadow.MaxOutputTokens,
		ParallelToolCalls:  shadow.ParallelToolCalls,
		PreviousResponseID: shadow.PreviousResponseID,
		Reasoning:          shadow.Reasoning,
		Store:              shadow.Store,
		Temperature:        shadow.Temperature,
		Text:               shadow.Text,
		ToolChoice:         shadow.ToolChoice,
		Tools:              shadow.Tools,
		TopP:               shadow.TopP,
		TopLogprobs:        shadow.TopLogprobs,
		Truncation:         shadow.Truncation,
		User:               shadow.User,
		Metadata:           shadow.Metadata,
		ServiceTier:        shadow.ServiceTier,
		Usage:              shadow.Usage,
	}
	return r.Validate()
}

// MarshalJSON validates and emits the envelope.
func (r ResponseEnvelope) MarshalJSON() ([]byte, error) {
	type envelopeAlias ResponseEnvelope
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal((*envelopeAlias)(&r))
}

// ResponsesRequestEcho captures request-derived state required to reconstruct
// the client Responses response envelope. A stateless convertResponse(body)
// cannot faithfully construct a Responses response; the exchange context
// carries the original request so the envelope can be rebuilt.
type ResponsesRequestEcho struct {
	Instructions       *ResponsesInput
	MaxOutputTokens    *int
	ParallelToolCalls  *bool
	PreviousResponseID *string
	Store              *bool
	Temperature        *float64
	TopP               *float64
	Truncation         *string
	User               *string
	Metadata           map[string]string
	Tools              []ResponsesTool
	ToolChoice         *ResponsesToolChoice
	Reasoning          *ResponsesEnvelopeReasoning
	Text               *ResponsesEnvelopeText
	ServiceTier        *string
	TopLogprobs        *int64
	Stream             bool
}

// MessagesRequestContext captures the request-derived state of a Messages
// source request needed to reconstruct the client response envelope.
type MessagesRequestContext struct {
	MaxTokens int
	Metadata  map[string]string
}
