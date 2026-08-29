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

// Tool is a function tool definition, or a namespace tool that groups
// nested tools (the Responses namespace-tool contract used by agents such as
// the Codex CLI's multi-agent surface). Function tools are part of the
// supported subset; built-in tools (web search, file search, code
// interpreter, computer use, and so on) decode but are rejected as
// unsupported features at the transcode boundary; namespace tools are
// flattened by the converters (the nested function tools are fully
// portable; the grouping itself is not).
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
	// Tools carries the nested tools of a namespace tool.
	Tools []Tool `json:"tools,omitempty"`
}

// Validate checks the tool shape and the required strict field. Namespace
// tools validate their nested tools and do not require strict themselves
// (the strict contract applies to function tools). A missing type is a
// typed malformed rejection under every policy: a tool with no type is
// never a built-in (built-ins are known non-empty strings), so it must
// never reach the builtin_tools loss path (review-12 finding 4).
func (t Tool) Validate() error {
	if t.Type == "" {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "tools[].type",
			Message: "responses tool requires an explicit non-empty type",
		}
	}
	switch t.Type {
	case "namespace":
		if t.Name == "" {
			return errors.New("namespace tool name is empty")
		}
		if len(t.Tools) == 0 {
			return errors.New("namespace tool has no nested tools")
		}
		if len(t.Parameters) != 0 {
			return &wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    "tools[].parameters",
				Message: "namespace tool cannot carry parameters (they belong to function tools)",
			}
		}
		if t.Strict.Present {
			return &wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    "tools[].strict",
				Message: "namespace tool cannot carry strict (the strict contract applies to function tools)",
			}
		}
		for i, nested := range t.Tools {
			if err := nested.Validate(); err != nil {
				return fmt.Errorf("namespace tool %d: %w", i, err)
			}
		}
		return nil
	case "function":
		if t.Name == "" {
			return errors.New("responses tool name is empty")
		}
		if len(t.Tools) != 0 {
			return &wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    "tools[].tools",
				Message: "function tool cannot carry nested tools (they belong to namespace tools)",
			}
		}
		if !t.Strict.Present || t.Strict.Null {
			return &wire.DecodeError{
				Kind:    wire.DecodeMissingRequired,
				Path:    "tools[].strict",
				Message: "responses function tool requires an explicit strict value",
			}
		}
		return nil
	default:
		// Built-in tools (web_search, file_search, code_interpreter,
		// computer_use, ...) decode; the transcode boundary decides the
		// loss/reject path. Unknown types are the same classification.
		return nil
	}
}

// UnmarshalJSON decodes function and namespace tools strictly. Built-in
// tool types (web_search, file_search, ...) are accepted wholesale — their
// payloads are provider-specific (e.g. the Codex CLI's web_search tool
// carries external_web_access) and the transcode boundary decides the
// loss/reject path, so strictness here would only reject real clients. A
// missing or empty type is rejected here, at decode time, so it can never
// masquerade as a built-in under an approved loss (review-12 finding 4);
// cross-type fields (tools on a function tool, parameters/strict on a
// namespace tool) are rejected rather than silently swallowed into the
// shared struct.
func (t *Tool) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.Type != "function" && probe.Type != "namespace" {
		if probe.Type == "" {
			return &wire.DecodeError{
				Kind:    wire.DecodeMissingRequired,
				Path:    "tools[].type",
				Message: "responses tool requires an explicit non-empty type",
			}
		}
		// Lenient: built-in tool payloads are opaque and never forwarded.
		var builtin struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		}
		if err := json.Unmarshal(data, &builtin); err != nil {
			return err
		}
		t.Type = builtin.Type
		t.Name = builtin.Name
		return nil
	}
	var strict struct {
		Type        string           `json:"type"`
		Name        string           `json:"name,omitempty"`
		Description string           `json:"description,omitempty"`
		Parameters  json.RawMessage  `json:"parameters,omitempty"`
		Strict      wire.Field[bool] `json:"strict"`
		Tools       []Tool           `json:"tools,omitempty"`
	}
	if err := wire.Decode(data, &strict); err != nil {
		return err
	}
	t.Type = strict.Type
	t.Name = strict.Name
	t.Description = strict.Description
	t.Parameters = strict.Parameters
	t.Strict = strict.Strict
	t.Tools = strict.Tools
	// Cross-type shape violations are rejected at decode so the shared
	// struct can never silently swallow a field the tool's type does not
	// own (review-12 finding 4).
	return t.Validate()
}

// MarshalJSON emits the tool in its decode-union shape: function tools
// always carry strict (the pinned contract marks it required on both the
// create request and the response echo), namespace and built-in tools
// never do. A blanket struct marshal would invent strict:false on echoed
// built-in and namespace tools — bytes the strict decoder classifies as a
// contradictory union for namespaces — so the marshal mirrors the union.
func (t Tool) MarshalJSON() ([]byte, error) {
	type toolFields struct {
		Type        string          `json:"type"`
		Name        string          `json:"name,omitempty"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
		Tools       []Tool          `json:"tools,omitempty"`
	}
	fields := toolFields{
		Type:        t.Type,
		Name:        t.Name,
		Description: t.Description,
		Parameters:  t.Parameters,
		Tools:       t.Tools,
	}
	if t.Type == "function" {
		value := t.Strict.Value
		fields.Strict = &value
	}
	return json.Marshal(fields)
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

	// CreatedCacheTokens is NOT part of the pinned Responses wire contract:
	// encoding/json never emits or accepts it (json:"-"), and a wire
	// input_tokens_details.created_cache_tokens key is still rejected as an
	// unknown field by strict decode. It is an in-memory composition carrier
	// for the Chat provider extension of the same name, so the composed
	// Messages←Chat stream can know the cache-write component without
	// inventing wire bytes the Responses contract does not define.
	CreatedCacheTokens *int64 `json:"-"`
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
