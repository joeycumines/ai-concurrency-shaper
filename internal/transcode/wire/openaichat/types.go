// Package openaichat implements the pinned OpenAI Chat Completions wire
// contract (source: openai-go v1.12.0, see contracts.lock.json): distinct
// strict types for the create request, the non-stream response, and the
// stream chunk.
//
// Chat Completions is deliberately upstream-only. The schema models the
// official OpenAI Chat Completions wire contract; provider extensions are
// opt-in capabilities and never masquerade as official fields.
//
// The package is deliberately self-contained: it imports only the shared
// wire package, never the transcode package, so the pinned contract types
// can never depend on transcode internals (no import cycle).
package openaichat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// MessageRole is the role of a chat message.
type MessageRole string

// MessageRole values.
const (
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleUser      MessageRole = "user"
	MessageRoleSystem    MessageRole = "system"
	MessageRoleTool      MessageRole = "tool"
	MessageRoleDeveloper MessageRole = "developer"
)

// ContentBlockType is the type of a chat content block.
type ContentBlockType string

// ContentBlockType values.
const (
	ContentBlockTypeText  ContentBlockType = "text"
	ContentBlockTypeImage ContentBlockType = "image_url"
)

// InputImage is the image_url payload of an image content block.
type InputImage struct {
	URL    string  `json:"url"`
	Detail *string `json:"detail,omitempty"`
}

// ContentBlock is one element of a content block array.
type ContentBlock struct {
	Type     ContentBlockType `json:"type"`
	Text     *string          `json:"text,omitempty"`
	ImageURL *InputImage      `json:"image_url,omitempty"`
}

// UnmarshalJSON decodes the tagged union per-arm: a text block admits only
// text and an image_url block only image_url (DisallowUnknownFields), so a
// block carrying another arm's fields is rejected at decode instead of
// having those fields silently discarded. The type probe is a lenient
// unmarshal; the arm decode applies the strictness.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type ContentBlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}

	var block ContentBlock
	switch probe.Type {
	case ContentBlockTypeText:
		var shadow struct {
			Type ContentBlockType `json:"type"`
			Text *string          `json:"text"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("text block: %w", err)
		}
		block.Type = shadow.Type
		block.Text = shadow.Text

	case ContentBlockTypeImage:
		var shadow struct {
			Type     ContentBlockType `json:"type"`
			ImageURL *InputImage      `json:"image_url"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("image block: %w", err)
		}
		block.Type = shadow.Type
		block.ImageURL = shadow.ImageURL

	default:
		return fmt.Errorf("unknown chat content block type %q", probe.Type)
	}

	*b = block
	return b.Validate()
}

// Validate checks the block shape.
func (b ContentBlock) Validate() error {
	switch b.Type {
	case ContentBlockTypeText:
		if b.Text == nil {
			return errors.New("text content block has no text")
		}
	case ContentBlockTypeImage:
		if b.ImageURL == nil || b.ImageURL.URL == "" {
			return errors.New("image content block has no image_url")
		}
	default:
		return fmt.Errorf("unknown chat content block type %q", b.Type)
	}
	return nil
}

// MessageContent is the content of a chat message: either a plain string or
// an array of content blocks. Exactly one form is set.
type MessageContent struct {
	ContentStr    *string
	ContentBlocks []ContentBlock
}

// Validate checks the union invariants and each block.
func (c MessageContent) Validate() error {
	if c.ContentStr != nil && c.ContentBlocks != nil {
		return errors.New("chat message content has both string and block variants")
	}
	if c.ContentStr == nil && c.ContentBlocks == nil {
		return errors.New("chat message content has no selected variant")
	}
	for i, block := range c.ContentBlocks {
		if err := block.Validate(); err != nil {
			return fmt.Errorf("chat content block %d: %w", i, err)
		}
	}
	return nil
}

// MarshalJSON emits the active form directly, without a wrapper object.
func (c MessageContent) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.ContentStr != nil {
		return json.Marshal(*c.ContentStr)
	}
	return json.Marshal(c.ContentBlocks)
}

// UnmarshalJSON decodes a string or an array of content blocks, rejecting
// invalid blocks at decode time so a decode-accepted value always validates.
func (c *MessageContent) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		c.ContentStr = nil
		c.ContentBlocks = nil
		return errors.New("chat message content is null")
	}
	if data[0] == '"' {
		var str string
		if err := wire.Decode(data, &str); err != nil {
			return err
		}
		c.ContentStr = &str
		c.ContentBlocks = nil
		return c.Validate()
	}
	var blocks []ContentBlock
	if err := wire.Decode(data, &blocks); err != nil {
		return err
	}
	c.ContentBlocks = blocks
	c.ContentStr = nil
	return c.Validate()
}

// ToolType is the type of a chat tool.
type ToolType string

// ToolType values.
const (
	ToolTypeFunction ToolType = "function"
)

// ToolFunction is the function definition of a function tool. Parameters is
// the raw schema JSON: it is validated as exactly one JSON object at the
// canonical-IR boundary and passed through byte-exact, so numbers are never
// decoded and remarshaled through a map.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Tool is a tool definition in a chat completion request.
type Tool struct {
	Type     ToolType      `json:"type"`
	Function *ToolFunction `json:"function,omitempty"`
}

// Validate checks the tool shape.
func (t Tool) Validate() error {
	if t.Type != ToolTypeFunction {
		return fmt.Errorf("unknown chat tool type %q", t.Type)
	}
	if t.Function == nil {
		return errors.New("chat function tool has no function")
	}
	if t.Function.Name == "" {
		return errors.New("chat function tool name is empty")
	}
	return nil
}

// ToolChoiceFunction names a specific function for a tool choice.
type ToolChoiceFunction struct {
	Name string `json:"name"`
}

// ToolChoiceStruct is the object form of a tool choice.
type ToolChoiceStruct struct {
	Type     string              `json:"type"`
	Function *ToolChoiceFunction `json:"function,omitempty"`
}

// ToolChoice is a union: either a plain string ("none", "auto", "required")
// or an object naming a specific tool.
type ToolChoice struct {
	Str    *string
	Struct *ToolChoiceStruct
}

// Validate checks the union invariants.
func (c ToolChoice) Validate() error {
	if c.Str != nil && c.Struct != nil {
		return errors.New("chat tool choice has both string and struct variants")
	}
	if c.Struct != nil {
		if c.Struct.Type != "function" {
			return fmt.Errorf("chat tool choice type = %q", c.Struct.Type)
		}
		if c.Struct.Function == nil || c.Struct.Function.Name == "" {
			return errors.New("named chat tool choice has no function name")
		}
		return nil
	}
	if c.Str != nil {
		switch *c.Str {
		case "none", "auto", "required":
			return nil
		default:
			return fmt.Errorf("invalid chat tool choice %q", *c.Str)
		}
	}
	return errors.New("chat tool choice has no selected variant")
}

// MarshalJSON emits the active form directly.
func (c ToolChoice) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	return json.Marshal(c.Struct)
}

// UnmarshalJSON decodes a string or an object, rejecting unknown variants at
// decode time so a decode-accepted value always validates.
func (c *ToolChoice) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("chat tool choice is null")
	}
	if data[0] == '"' {
		var str string
		if err := wire.Decode(data, &str); err != nil {
			return err
		}
		c.Str = &str
		c.Struct = nil
	} else {
		var s ToolChoiceStruct
		if err := wire.Decode(data, &s); err != nil {
			return err
		}
		c.Struct = &s
		c.Str = nil
	}
	return c.Validate()
}

// ToolMessage carries the tool_call_id of a tool-role message.
type ChatToolMessage struct {
	ToolCallID *string `json:"tool_call_id,omitempty"`
}

// ToolCallFunction is the function payload of a tool call. The non-stream
// shape carries complete arguments; streaming deltas carry partial
// fragments.
type ToolCallFunction struct {
	Name      *string `json:"name"`
	Arguments string  `json:"arguments"` // stringified JSON, may be partial while streaming
}

// MessageToolCall is a non-stream tool call on an assistant message: the
// official request/response shape carries id, function, and type and NO
// index. Type is required on the wire and always emitted ("function").
type MessageToolCall struct {
	Type     string           `json:"type"`
	ID       *string          `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallDelta is a streaming tool-call fragment: the official chunk delta
// shape carries index (required on the wire), id, function, and type. It is
// used only by StreamDelta.ToolCalls.
type ToolCallDelta struct {
	Index    *int             `json:"index,omitempty"`
	Type     *string          `json:"type,omitempty"`
	ID       *string          `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// AssistantMessage carries the assistant-only fields of a chat message.
type ChatAssistantMessage struct {
	Refusal   *string           `json:"refusal,omitempty"`
	ToolCalls []MessageToolCall `json:"tool_calls,omitempty"`
	// Reasoning is a provider extension: an explicitly configured plaintext
	// reasoning response field. It is only read when
	// ChatCapabilities.ProviderReasoningText is enabled and may map only to
	// ordinary text, an approved loss, or a rejection.
	Reasoning *string `json:"reasoning,omitempty"`

	// Opaque provider-extension fields present on real assistant messages
	// (e.g. the yolo gateway's token_ids, routed_experts, stop_reason):
	// decoded so strict wire decoding never fails on a current provider,
	// never forwarded — mirroring Choice so the message and choice surfaces
	// agree.
	TokenIDs      any     `json:"token_ids,omitempty"`
	RoutedExperts any     `json:"routed_experts,omitempty"`
	StopReason    *string `json:"stop_reason,omitempty"`
}

// Message is a message in a chat conversation. Assistant messages flatten
// tool_calls and refusal fields; tool messages flatten tool_call_id.
type Message struct {
	Name    *string         `json:"name,omitempty"`
	Role    MessageRole     `json:"role,omitempty"`
	Content *MessageContent `json:"content,omitempty"`

	*ChatToolMessage
	*ChatAssistantMessage
}

// Validate checks the message shape.
func (m Message) Validate() error {
	if m.Role == "" {
		return errors.New("chat message role is empty")
	}
	if m.Content != nil {
		if err := m.Content.Validate(); err != nil {
			return err
		}
	}
	// Role-conditional fields: refusal, tool_calls, and reasoning are
	// assistant-only, and tool_call_id is tool-only. A message carrying
	// another role's fields is a contradictory union — a typed decode
	// rejection — instead of being silently discarded or relabeled. The
	// embedded struct pointers may be nil, so every promoted access is
	// guarded.
	if m.Role != MessageRoleAssistant && m.ChatAssistantMessage != nil {
		return &wire.DecodeError{
			Kind:    wire.DecodeContradictoryUnion,
			Path:    "role",
			Message: fmt.Sprintf("chat message role %q carries assistant-only fields", m.Role),
		}
	}
	if m.Role != MessageRoleTool && m.ChatToolMessage != nil {
		return &wire.DecodeError{
			Kind:    wire.DecodeContradictoryUnion,
			Path:    "role",
			Message: fmt.Sprintf("chat message role %q carries tool_call_id", m.Role),
		}
	}
	if m.Role == MessageRoleTool &&
		(m.ChatToolMessage == nil || m.ToolCallID == nil || *m.ToolCallID == "") {
		return errors.New("tool message has no tool_call_id")
	}
	if m.ChatAssistantMessage != nil {
		for i, call := range m.ToolCalls {
			if call.ID == nil || *call.ID == "" {
				return fmt.Errorf("tool call %d has no id", i)
			}
			if call.Type != "function" {
				return fmt.Errorf("tool call %d type = %q, want function", i, call.Type)
			}
		}
	}
	return nil
}

// ResponseFormatType is the response_format type tag.
type ResponseFormatType string

// ResponseFormatType values.
const (
	ResponseFormatText       ResponseFormatType = "text"
	ResponseFormatJSONObject ResponseFormatType = "json_object"
	ResponseFormatJSONSchema ResponseFormatType = "json_schema"
)

// JSONSchemaFormat is the json_schema arm payload of response_format. Schema
// is the raw schema JSON: it is validated as exactly one JSON object at the
// canonical-IR boundary and passed through byte-exact, so numbers are never
// decoded and remarshaled through a map.
type JSONSchemaFormat struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponseFormat is the response_format union of the official Chat contract:
// text, json_object, or json_schema.
type ResponseFormat struct {
	Type       ResponseFormatType `json:"type"`
	JSONSchema *JSONSchemaFormat  `json:"json_schema,omitempty"`
}

// StreamOptions configures chat completion streaming.
type StreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

// Stop is the stop union: a single string or an array of up to four
// sequences. The official contract permits both forms.
type Stop struct {
	Str  *string
	Strs []string
}

// Validate checks the union invariants.
func (s Stop) Validate() error {
	if s.Str != nil && s.Strs != nil {
		return errors.New("chat stop has both string and array variants")
	}
	if s.Str == nil && s.Strs == nil {
		return errors.New("chat stop has no selected variant")
	}
	if len(s.Strs) > 4 {
		return errors.New("chat stop array exceeds 4 sequences")
	}
	return nil
}

// MarshalJSON emits the active form.
func (s Stop) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.Str != nil {
		return json.Marshal(*s.Str)
	}
	return json.Marshal(s.Strs)
}

// UnmarshalJSON decodes a string or an array.
func (s *Stop) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("chat stop is null")
	}
	if data[0] == '"' {
		var str string
		if err := wire.Decode(data, &str); err != nil {
			return err
		}
		s.Str = &str
		s.Strs = nil
		return nil
	}
	var strs []string
	if err := wire.Decode(data, &strs); err != nil {
		return err
	}
	s.Strs = strs
	s.Str = nil
	return nil
}

// Request is a chat completions request. The official subset covers the
// fields that transcode actually maps; unsupported fields are rejected by
// strict decoding.
type Request struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                *Stop           `json:"stop,omitempty"`
	Stream              *bool           `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          *ToolChoice     `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     *string         `json:"reasoning_effort,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	Metadata            map[string]any  `json:"metadata,omitempty"`
	User                *string         `json:"user,omitempty"`
	Store               *bool           `json:"store,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
	N                   *int            `json:"n,omitempty"`
}

// PromptTokensDetails breaks down prompt tokens. The cached token field is
// always present on the wire (zero when unused), matching the real API.
// Provider-extension fields (created_cache_tokens, multimodal_tokens) are
// decoded and never forwarded.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	AudioTokens  int `json:"audio_tokens"`

	CreatedCacheTokens *int `json:"created_cache_tokens,omitempty"`
	MultimodalTokens   *int `json:"multimodal_tokens,omitempty"`
}

// CompletionTokensDetails breaks down completion tokens. The reasoning token
// field is always present on the wire (zero when unused), matching the real
// API.
type CompletionTokensDetails struct {
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	AudioTokens              int `json:"audio_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
}

// LLMUsage is the token usage of a chat completion. PromptTokens and
// CompletionTokens are omitted when zero; the always-on-wire fields are the
// sub-fields (CachedTokens/ReasoningTokens).
type LLMUsage struct {
	PromptTokens            int                      `json:"prompt_tokens,omitempty"`
	CompletionTokens        int                      `json:"completion_tokens,omitempty"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// StreamDelta is the delta payload of a streaming chat completion chunk.
type StreamDelta struct {
	Role      *string         `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	Refusal   *string         `json:"refusal,omitempty"`
	Reasoning *string         `json:"reasoning,omitempty"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

// TopLogprob is one alternative token of a token log-probability entry.
type TopLogprob struct {
	Token   string  `json:"token"`
	Bytes   []int64 `json:"bytes"`
	Logprob float64 `json:"logprob"`
}

// TokenLogprob is one token log-probability entry of a chat response.
type TokenLogprob struct {
	Token       string       `json:"token"`
	Bytes       []int64      `json:"bytes"`
	Logprob     float64      `json:"logprob"`
	TopLogprobs []TopLogprob `json:"top_logprobs"`
}

// ChoiceLogprobs is the logprobs payload of a chat choice: token
// log-probabilities for content and refusal tokens. It is required on the
// wire (null when not requested); the transcoder never requests logprobs, so
// it is decode-only and its presence enters the explicit loss/reject
// decision.
type ChoiceLogprobs struct {
	Content []TokenLogprob `json:"content"`
	Refusal []TokenLogprob `json:"refusal"`
}

// Choice is one choice of a chat completion response, in either the
// non-streaming (Message) or streaming (Delta) form.
type Choice struct {
	Index        int             `json:"index"`
	FinishReason *string         `json:"finish_reason,omitempty"`
	LogProbs     *ChoiceLogprobs `json:"logprobs"`
	Message      *Message        `json:"message,omitempty"`
	Delta        *StreamDelta    `json:"delta,omitempty"`

	// Opaque provider-extension fields present on real streaming and
	// non-streaming chat responses (e.g. the yolo gateway's token_ids,
	// routed_experts, stop_reason): decoded so strict wire decoding never
	// fails on a current provider, never forwarded.
	TokenIDs      any     `json:"token_ids,omitempty"`
	RoutedExperts any     `json:"routed_experts,omitempty"`
	StopReason    *string `json:"stop_reason,omitempty"`
}

// Response is a chat completions response (the non-streaming shape). Opaque
// provider-extension fields (prompt_token_ids, prompt_text) are decoded so
// strict wire decoding never fails on a current provider, never forwarded —
// mirroring StreamChunk so the non-streaming and streaming surfaces agree.
type Response struct {
	ID                string    `json:"id"`
	Object            string    `json:"object"`
	Created           int64     `json:"created"`
	Model             string    `json:"model"`
	ServiceTier       *string   `json:"service_tier,omitempty"`
	SystemFingerprint string    `json:"system_fingerprint,omitempty"`
	Choices           []Choice  `json:"choices"`
	Usage             *LLMUsage `json:"usage,omitempty"`

	PromptTokenIDs any     `json:"prompt_token_ids,omitempty"`
	PromptText     *string `json:"prompt_text,omitempty"`
}

// StreamChunk is one SSE frame of a streaming chat completion: the chunk
// envelope (id, object, created, model, choices, usage) around the
// per-choice deltas. Opaque provider-extension fields (prompt_token_ids,
// prompt_text) are decoded and never forwarded.
type StreamChunk struct {
	ID                string    `json:"id"`
	Object            string    `json:"object"`
	Created           int64     `json:"created"`
	Model             string    `json:"model"`
	ServiceTier       *string   `json:"service_tier,omitempty"`
	SystemFingerprint string    `json:"system_fingerprint,omitempty"`
	Choices           []Choice  `json:"choices"`
	Usage             *LLMUsage `json:"usage,omitempty"`

	PromptTokenIDs any     `json:"prompt_token_ids,omitempty"`
	PromptText     *string `json:"prompt_text,omitempty"`
}
