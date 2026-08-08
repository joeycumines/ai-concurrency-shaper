package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// =============================================================================
// OpenAI Chat Completions API (upstream-only, official subset)
// =============================================================================
//
// Chat Completions is deliberately upstream-only. The schema models the
// official OpenAI Chat Completions wire contract; provider extensions are
// opt-in capabilities and never masquerade as official fields.
//
// https://platform.openai.com/docs/api-reference/chat
// https://github.com/openai/openai-go/blob/main/chatcompletion.go

// ChatMessageRole is the role of a chat message.
type ChatMessageRole string

// ChatMessageRole values.
const (
	ChatMessageRoleAssistant ChatMessageRole = "assistant"
	ChatMessageRoleUser      ChatMessageRole = "user"
	ChatMessageRoleSystem    ChatMessageRole = "system"
	ChatMessageRoleTool      ChatMessageRole = "tool"
	ChatMessageRoleDeveloper ChatMessageRole = "developer"
)

// ChatContentBlockType is the type of a chat content block.
type ChatContentBlockType string

// ChatContentBlockType values.
const (
	ChatContentBlockTypeText  ChatContentBlockType = "text"
	ChatContentBlockTypeImage ChatContentBlockType = "image_url"
)

// ChatInputImage is the image_url payload of an image content block.
type ChatInputImage struct {
	URL    string  `json:"url"`
	Detail *string `json:"detail,omitempty"`
}

// ChatContentBlock is one element of a content block array.
type ChatContentBlock struct {
	Type     ChatContentBlockType `json:"type"`
	Text     *string              `json:"text,omitempty"`
	ImageURL *ChatInputImage      `json:"image_url,omitempty"`
}

// Validate checks the block shape.
func (b ChatContentBlock) Validate() error {
	switch b.Type {
	case ChatContentBlockTypeText:
		if b.Text == nil {
			return errors.New("text content block has no text")
		}
	case ChatContentBlockTypeImage:
		if b.ImageURL == nil || b.ImageURL.URL == "" {
			return errors.New("image content block has no image_url")
		}
	default:
		return fmt.Errorf("unknown chat content block type %q", b.Type)
	}
	return nil
}

// ChatMessageContent is the content of a chat message: either a plain string
// or an array of content blocks. Exactly one form is set.
type ChatMessageContent struct {
	ContentStr    *string
	ContentBlocks []ChatContentBlock
}

// Validate checks the union invariants and each block.
func (c ChatMessageContent) Validate() error {
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
func (c ChatMessageContent) MarshalJSON() ([]byte, error) {
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
func (c *ChatMessageContent) UnmarshalJSON(data []byte) error {
	data = trimJSONSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		c.ContentStr = nil
		c.ContentBlocks = nil
		return errors.New("chat message content is null")
	}
	if data[0] == '"' {
		var str string
		if err := strictDecode(data, &str); err != nil {
			return err
		}
		c.ContentStr = &str
		c.ContentBlocks = nil
		return c.Validate()
	}
	var blocks []ChatContentBlock
	if err := strictDecode(data, &blocks); err != nil {
		return err
	}
	c.ContentBlocks = blocks
	c.ContentStr = nil
	return c.Validate()
}

// ChatToolType is the type of a chat tool.
type ChatToolType string

// ChatToolType values.
const (
	ChatToolTypeFunction ChatToolType = "function"
)

// ChatToolFunction is the function definition of a function tool. Parameters
// is the raw schema JSON: it is validated as exactly one JSON object at the
// canonical-IR boundary and passed through byte-exact, so numbers are never
// decoded and remarshaled through a map (review-k finding 2).
type ChatToolFunction struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatTool is a tool definition in a chat completion request.
type ChatTool struct {
	Type     ChatToolType      `json:"type"`
	Function *ChatToolFunction `json:"function,omitempty"`
}

// Validate checks the tool shape.
func (t ChatTool) Validate() error {
	if t.Type != ChatToolTypeFunction {
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

// ChatToolChoiceFunction names a specific function for a tool choice.
type ChatToolChoiceFunction struct {
	Name string `json:"name"`
}

// ChatToolChoiceStruct is the object form of a tool choice.
type ChatToolChoiceStruct struct {
	Type     string                  `json:"type"`
	Function *ChatToolChoiceFunction `json:"function,omitempty"`
}

// ChatToolChoice is a union: either a plain string ("none", "auto",
// "required") or an object naming a specific tool.
type ChatToolChoice struct {
	Str    *string
	Struct *ChatToolChoiceStruct
}

// Validate checks the union invariants.
func (c ChatToolChoice) Validate() error {
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
func (c ChatToolChoice) MarshalJSON() ([]byte, error) {
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
func (c *ChatToolChoice) UnmarshalJSON(data []byte) error {
	data = trimJSONSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("chat tool choice is null")
	}
	if data[0] == '"' {
		var str string
		if err := strictDecode(data, &str); err != nil {
			return err
		}
		c.Str = &str
		c.Struct = nil
	} else {
		var s ChatToolChoiceStruct
		if err := strictDecode(data, &s); err != nil {
			return err
		}
		c.Struct = &s
		c.Str = nil
	}
	return c.Validate()
}

// ChatToolMessage carries the tool_call_id of a tool-role message.
type ChatToolMessage struct {
	ToolCallID *string `json:"tool_call_id,omitempty"`
}

// ChatToolCallFunction is the function payload of a tool call. The
// non-stream shape carries complete arguments; streaming deltas carry
// partial fragments.
type ChatToolCallFunction struct {
	Name      *string `json:"name"`
	Arguments string  `json:"arguments"` // stringified JSON, may be partial while streaming
}

// ChatMessageToolCall is a non-stream tool call on an assistant message: the
// official request/response shape carries id, function, and type and NO index
// (review-j finding 5). Index belongs to the streaming delta type.
type ChatMessageToolCall struct {
	Type     *string              `json:"type,omitempty"`
	ID       *string              `json:"id,omitempty"`
	Function ChatToolCallFunction `json:"function"`
}

// ChatToolCallDelta is a streaming tool-call fragment: the official chunk
// delta shape carries index (required on the wire), id, function, and type.
// It is used only by ChatStreamDelta.ToolCalls.
type ChatToolCallDelta struct {
	Index    *int                 `json:"index,omitempty"`
	Type     *string              `json:"type,omitempty"`
	ID       *string              `json:"id,omitempty"`
	Function ChatToolCallFunction `json:"function"`
}

// ChatAssistantMessage carries the assistant-only fields of a chat message.
type ChatAssistantMessage struct {
	Refusal   *string               `json:"refusal,omitempty"`
	ToolCalls []ChatMessageToolCall `json:"tool_calls,omitempty"`
	// Reasoning is a provider extension: an explicitly configured plaintext
	// reasoning response field. It is only read when
	// ChatCapabilities.ProviderReasoningText is enabled and may map only to
	// ordinary text, an approved loss, or a rejection.
	Reasoning *string `json:"reasoning,omitempty"`
}

// ChatMessage is a message in a chat conversation. Assistant messages flatten
// tool_calls and refusal fields; tool messages flatten tool_call_id.
type ChatMessage struct {
	Name    *string             `json:"name,omitempty"`
	Role    ChatMessageRole     `json:"role,omitempty"`
	Content *ChatMessageContent `json:"content,omitempty"`

	*ChatToolMessage
	*ChatAssistantMessage
}

// Validate checks the message shape.
func (m ChatMessage) Validate() error {
	if m.Role == "" {
		return errors.New("chat message role is empty")
	}
	if m.Content != nil {
		if err := m.Content.Validate(); err != nil {
			return err
		}
	}
	if m.Role == ChatMessageRoleTool && (m.ToolCallID == nil || *m.ToolCallID == "") {
		return errors.New("tool message has no tool_call_id")
	}
	if m.ChatAssistantMessage != nil {
		for i, call := range m.ToolCalls {
			if call.ID == nil || *call.ID == "" {
				return fmt.Errorf("tool call %d has no id", i)
			}
		}
	}
	return nil
}

// ChatResponseFormatType is the response_format type tag.
type ChatResponseFormatType string

// ChatResponseFormatType values.
const (
	ChatResponseFormatText       ChatResponseFormatType = "text"
	ChatResponseFormatJSONObject ChatResponseFormatType = "json_object"
	ChatResponseFormatJSONSchema ChatResponseFormatType = "json_schema"
)

// ChatJSONSchemaFormat is the json_schema arm payload of response_format.
// Schema is the raw schema JSON: it is validated as exactly one JSON object
// at the canonical-IR boundary and passed through byte-exact, so numbers are
// never decoded and remarshaled through a map (review-k finding 2).
type ChatJSONSchemaFormat struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatResponseFormat is the response_format union of the official Chat
// contract: text, json_object, or json_schema.
type ChatResponseFormat struct {
	Type       ChatResponseFormatType `json:"type"`
	JSONSchema *ChatJSONSchemaFormat  `json:"json_schema,omitempty"`
}

// ChatStreamOptions configures chat completion streaming.
type ChatStreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

// ChatStop is the stop union: a single string or an array of up to four
// sequences. The official contract permits both forms.
type ChatStop struct {
	Str  *string
	Strs []string
}

// Validate checks the union invariants.
func (s ChatStop) Validate() error {
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
func (s ChatStop) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.Str != nil {
		return json.Marshal(*s.Str)
	}
	return json.Marshal(s.Strs)
}

// UnmarshalJSON decodes a string or an array.
func (s *ChatStop) UnmarshalJSON(data []byte) error {
	data = trimJSONSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("chat stop is null")
	}
	if data[0] == '"' {
		var str string
		if err := strictDecode(data, &str); err != nil {
			return err
		}
		s.Str = &str
		s.Strs = nil
		return nil
	}
	var strs []string
	if err := strictDecode(data, &strs); err != nil {
		return err
	}
	s.Strs = strs
	s.Str = nil
	return nil
}

// ChatRequest is a chat completions request. The official subset covers the
// fields that transcode actually maps; unsupported fields are rejected by
// strict decoding.
type ChatRequest struct {
	Model               string              `json:"model"`
	Messages            []ChatMessage       `json:"messages"`
	MaxCompletionTokens *int                `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int                `json:"max_tokens,omitempty"`
	Temperature         *float64            `json:"temperature,omitempty"`
	TopP                *float64            `json:"top_p,omitempty"`
	Stop                *ChatStop           `json:"stop,omitempty"`
	Stream              *bool               `json:"stream,omitempty"`
	StreamOptions       *ChatStreamOptions  `json:"stream_options,omitempty"`
	Tools               []ChatTool          `json:"tools,omitempty"`
	ToolChoice          *ChatToolChoice     `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool               `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     *string             `json:"reasoning_effort,omitempty"`
	ResponseFormat      *ChatResponseFormat `json:"response_format,omitempty"`
	Metadata            map[string]any      `json:"metadata,omitempty"`
	User                *string             `json:"user,omitempty"`
	Store               *bool               `json:"store,omitempty"`
	FrequencyPenalty    *float64            `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64            `json:"presence_penalty,omitempty"`
	Seed                *int                `json:"seed,omitempty"`
	N                   *int                `json:"n,omitempty"`
}

// ChatPromptTokensDetails breaks down prompt tokens. The cached token field
// is always present on the wire (zero when unused), matching the real API.
type ChatPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	AudioTokens  int `json:"audio_tokens"`
}

// ChatCompletionTokensDetails breaks down completion tokens. The reasoning
// token field is always present on the wire (zero when unused), matching the
// real API.
type ChatCompletionTokensDetails struct {
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	AudioTokens              int `json:"audio_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
}

// ChatLLMUsage is the token usage of a chat completion.
// PromptTokens and CompletionTokens are omitted when zero;
// the always-on-wire fields are the sub-fields
// (CachedTokens/ReasoningTokens).
type ChatLLMUsage struct {
	PromptTokens            int                          `json:"prompt_tokens,omitempty"`
	CompletionTokens        int                          `json:"completion_tokens,omitempty"`
	TotalTokens             int                          `json:"total_tokens"`
	PromptTokensDetails     *ChatPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *ChatCompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// ChatStreamDelta is the delta payload of a streaming chat completion chunk.
type ChatStreamDelta struct {
	Role      *string             `json:"role,omitempty"`
	Content   *string             `json:"content,omitempty"`
	Refusal   *string             `json:"refusal,omitempty"`
	Reasoning *string             `json:"reasoning,omitempty"`
	ToolCalls []ChatToolCallDelta `json:"tool_calls,omitempty"`
}

// ChatTopLogprob is one alternative token of a token log-probability entry.
type ChatTopLogprob struct {
	Token   string  `json:"token"`
	Bytes   []int64 `json:"bytes"`
	Logprob float64 `json:"logprob"`
}

// ChatTokenLogprob is one token log-probability entry of a chat response.
type ChatTokenLogprob struct {
	Token       string           `json:"token"`
	Bytes       []int64          `json:"bytes"`
	Logprob     float64          `json:"logprob"`
	TopLogprobs []ChatTopLogprob `json:"top_logprobs"`
}

// ChatChoiceLogprobs is the logprobs payload of a chat choice: token
// log-probabilities for content and refusal tokens. It is required on the
// wire (null when not requested); the transcoder never requests logprobs, so
// it is decode-only and its presence enters the explicit loss/reject decision.
type ChatChoiceLogprobs struct {
	Content []ChatTokenLogprob `json:"content"`
	Refusal []ChatTokenLogprob `json:"refusal"`
}

// ChatChoice is one choice of a chat completion response, in either the
// non-streaming (Message) or streaming (Delta) form.
type ChatChoice struct {
	Index        int                 `json:"index"`
	FinishReason *string             `json:"finish_reason,omitempty"`
	LogProbs     *ChatChoiceLogprobs `json:"logprobs"`
	Message      *ChatMessage        `json:"message,omitempty"`
	Delta        *ChatStreamDelta    `json:"delta,omitempty"`
}

// ChatResponse is a chat completions response.
type ChatResponse struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	ServiceTier       *string       `json:"service_tier,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
	Choices           []ChatChoice  `json:"choices"`
	Usage             *ChatLLMUsage `json:"usage,omitempty"`
}

// ChatStreamResponse is one SSE frame of a streaming chat completion: the
// chunk envelope (id, object, created, model, choices, usage) around the
// per-choice deltas.
type ChatStreamResponse struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	ServiceTier       *string       `json:"service_tier,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
	Choices           []ChatChoice  `json:"choices"`
	Usage             *ChatLLMUsage `json:"usage,omitempty"`
}
