// Package openairesponses implements the pinned OpenAI Responses wire
// contract (source: openai-go v1.12.0, see contracts.lock.json): distinct
// strict types for the create request, the response object, and each SSE
// event, with presence-aware decoding and required-field emission.
//
// The package is deliberately self-contained: it imports only the shared
// wire package, never the transcode package, so the pinned contract types
// can never depend on transcode internals (no import cycle). The transcode
// layer maps the typed errors (wire.DecodeError, wire.UnsupportedTypeError)
// into its classification taxonomy at its boundaries.
package openairesponses

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Tool is a function tool definition. Only function tools are part of the
// supported subset; built-in tools (web search, file search, code
// interpreter, computer use, and so on) are rejected as unsupported features.
//
// Strict is required on the wire for both the create request (the pinned
// FunctionToolParam) and the response echo (the pinned ResponseFunctionTool):
// a tool whose strict key is missing or null is malformed under the pin.
type Tool struct {
	Type        string           `json:"type"`
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Parameters  json.RawMessage  `json:"parameters,omitempty"`
	Strict      wire.Field[bool] `json:"strict"`
}

// Validate checks the function-tool shape and the required strict field.
func (t Tool) Validate() error {
	if t.Type != "function" {
		return fmt.Errorf("responses tool type = %q, want function", t.Type)
	}
	if t.Name == "" {
		return errors.New("responses tool name is empty")
	}
	if !t.Strict.Present || t.Strict.Null {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "strict",
			Message: "responses function tool requires an explicit strict value",
		}
	}
	return nil
}

// ToolChoiceNamed is the named-function arm of tool_choice.
type ToolChoiceNamed struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ToolChoice is the tool_choice union: a plain string ("none", "auto",
// "required") or a named function object. Unknown variants are rejected
// rather than silently defaulted to auto.
type ToolChoice struct {
	Str   *string
	Named *ToolChoiceNamed
}

// Validate checks the union invariants.
func (c ToolChoice) Validate() error {
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
func (c *ToolChoice) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty tool choice")
	}
	if data[0] == '"' {
		var str string
		if err := wire.Decode(data, &str); err != nil {
			return err
		}
		c.Str = &str
		c.Named = nil
	} else {
		var named ToolChoiceNamed
		if err := wire.Decode(data, &named); err != nil {
			return err
		}
		c.Str = nil
		c.Named = &named
	}
	return c.Validate()
}

// MarshalJSON emits the selected arm.
func (c ToolChoice) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	return json.Marshal(c.Named)
}

// EnvelopeError is the error object of a failed response envelope. Code is
// required on the wire per the pinned ResponseError contract: it is always
// emitted (empty string when the transcoder has no code to report).
type EnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Prompt is the typed shadow of the pinned ResponsePrompt: prompt template
// configuration echoed on the envelope. The transcoder does not implement
// prompt templates; presence enters the explicit loss/reject decision.
type Prompt struct {
	ID        string                     `json:"id"`
	Variables map[string]json.RawMessage `json:"variables,omitempty"`
	Version   string                     `json:"version,omitempty"`
}

// IncompleteDetails explains why a response is incomplete.
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Usage is the token usage of a response envelope. The details objects are
// required on the wire per the pinned ResponseUsage contract: they are
// always emitted (explicit null when the transcoder has no breakdown to
// report).
type Usage struct {
	InputTokens         int64                     `json:"input_tokens"`
	InputTokensDetails  *UsageInputTokensDetails  `json:"input_tokens_details"`
	OutputTokens        int64                     `json:"output_tokens"`
	OutputTokensDetails *UsageOutputTokensDetails `json:"output_tokens_details"`
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

// Reasoning echoes the request reasoning configuration. Both effort and
// summary are string enums on the wire ("low"/"medium"/"high" and
// "auto"/"concise"/"detailed"), matching the official Reasoning type.
type Reasoning struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// Text echoes the request text configuration.
type Text struct {
	Format *TextFormat `json:"format,omitempty"`
}

// TextFormat is the format arm of text config.
type TextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// InputRole is the role of an easy input message.
type InputRole string

// InputRole values.
const (
	InputRoleUser      InputRole = "user"
	InputRoleAssistant InputRole = "assistant"
	InputRoleSystem    InputRole = "system"
	InputRoleDeveloper InputRole = "developer"
)

// ItemStatus is the lifecycle status of an input or output item.
type ItemStatus string

// ItemStatus values.
const (
	ItemInProgress ItemStatus = "in_progress"
	ItemCompleted  ItemStatus = "completed"
	ItemIncomplete ItemStatus = "incomplete"
)

// ValidStatus reports whether s is a legal ItemStatus value.
func ValidStatus(s ItemStatus) bool {
	switch s {
	case ItemInProgress, ItemCompleted, ItemIncomplete:
		return true
	default:
		return false
	}
}
