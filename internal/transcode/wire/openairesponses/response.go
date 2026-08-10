package openairesponses

import (
	"encoding/json"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Response is the strict response object embedded in response.created,
// response.in_progress, and the terminal events, and used as the
// non-streaming Responses response body. Every official field is modeled;
// the 14 pinned required fields are ALWAYS emitted — with explicit null,
// {}, or [] where the contract requires the key but has no value — so a
// strict decoder can never fail on a missing required key in emitted wire.
//
// created_at is float64: valid fractional timestamps survive decoding,
// stream identity pinning, and rendering without truncation.
type Response struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	CreatedAt float64      `json:"created_at"`
	Status    string       `json:"status"`
	Model     string       `json:"model"`
	Output    []OutputItem `json:"output"`

	// Required envelope fields: always emitted, null when absent.
	Error             *EnvelopeError     `json:"error"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details"`
	Instructions      *Input             `json:"instructions"`
	Metadata          map[string]string  `json:"metadata"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls"`
	Temperature       *float64           `json:"temperature"`
	ToolChoice        *ToolChoice        `json:"tool_choice"`
	Tools             []Tool             `json:"tools"`
	TopP              *float64           `json:"top_p"`

	// Typed shadow fields for the pinned envelope controls: they decode so
	// strict decoding never fails on a current official response, and their
	// presence enters the explicit loss/reject decision at render time.
	Background       *bool   `json:"background,omitempty"`
	MaxToolCalls     *int64  `json:"max_tool_calls,omitempty"`
	Prompt           *Prompt `json:"prompt,omitempty"`
	PromptCacheKey   string  `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier string  `json:"safety_identifier,omitempty"`

	MaxOutputTokens    *int64     `json:"max_output_tokens,omitempty"`
	PreviousResponseID *string    `json:"previous_response_id,omitempty"`
	Reasoning          *Reasoning `json:"reasoning,omitempty"`
	Store              *bool      `json:"store,omitempty"`
	Text               *Text      `json:"text,omitempty"`
	TopLogprobs        *int64     `json:"top_logprobs,omitempty"`
	Truncation         *string    `json:"truncation,omitempty"`
	User               *string    `json:"user,omitempty"`
	ServiceTier        *string    `json:"service_tier,omitempty"`
	Usage              *Usage     `json:"usage,omitempty"`
}

// Validate checks the required envelope fields. Missing required fields are
// reported as typed wire.DecodeError with the missing_required category, so
// every rejection of the six malformed-JSON categories surfaces as a typed
// decode error at the boundary.
func (r Response) Validate() error {
	if r.ID == "" {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "id",
			Message: "response id is empty",
		}
	}
	if r.Object != "response" {
		return fmt.Errorf("response object = %q", r.Object)
	}
	if r.Model == "" {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "model",
			Message: "response model is empty",
		}
	}
	if r.Output == nil {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "output",
			Message: "response output must be present; use an empty array",
		}
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
func (r *Response) UnmarshalJSON(data []byte) error {
	// Decode into a shadow struct whose output field is raw so the tagged
	// union can be dispatched through DecodeOutputItem.
	var shadow struct {
		ID                 string             `json:"id"`
		Object             string             `json:"object"`
		CreatedAt          float64            `json:"created_at"`
		Status             string             `json:"status"`
		Model              string             `json:"model"`
		Output             json.RawMessage    `json:"output"`
		Error              *EnvelopeError     `json:"error"`
		IncompleteDetails  *IncompleteDetails `json:"incomplete_details"`
		Background         *bool              `json:"background"`
		MaxToolCalls       *int64             `json:"max_tool_calls"`
		Prompt             *Prompt            `json:"prompt"`
		PromptCacheKey     string             `json:"prompt_cache_key"`
		SafetyIdentifier   string             `json:"safety_identifier"`
		Instructions       *Input             `json:"instructions"`
		MaxOutputTokens    *int64             `json:"max_output_tokens"`
		ParallelToolCalls  *bool              `json:"parallel_tool_calls"`
		PreviousResponseID *string            `json:"previous_response_id"`
		Reasoning          *Reasoning         `json:"reasoning"`
		Store              *bool              `json:"store"`
		Temperature        *float64           `json:"temperature"`
		Text               *Text              `json:"text"`
		ToolChoice         *ToolChoice        `json:"tool_choice"`
		Tools              []Tool             `json:"tools"`
		TopP               *float64           `json:"top_p"`
		TopLogprobs        *int64             `json:"top_logprobs"`
		Truncation         *string            `json:"truncation"`
		User               *string            `json:"user"`
		Metadata           map[string]string  `json:"metadata"`
		ServiceTier        *string            `json:"service_tier"`
		Usage              *Usage             `json:"usage"`
	}
	if err := wire.Decode(data, &shadow); err != nil {
		return err
	}

	var items []OutputItem
	if len(shadow.Output) != 0 && string(shadow.Output) != "null" {
		var rawItems []json.RawMessage
		if err := json.Unmarshal(shadow.Output, &rawItems); err != nil {
			return err
		}
		items = make([]OutputItem, 0, len(rawItems))
		for i, rawItem := range rawItems {
			item, err := DecodeOutputItem(rawItem)
			if err != nil {
				return fmt.Errorf("output item %d: %w", i, err)
			}
			items = append(items, item)
		}
	}

	*r = Response{
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
func (r Response) MarshalJSON() ([]byte, error) {
	type responseAlias Response
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal((*responseAlias)(&r))
}
