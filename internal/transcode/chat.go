package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// =============================================================================
// OpenAI Chat Completions API
// =============================================================================

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
	ChatContentBlockTypeText    ChatContentBlockType = "text"
	ChatContentBlockTypeRefusal ChatContentBlockType = "refusal"
	ChatContentBlockTypeImage   ChatContentBlockType = "image_url"
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
	Refusal  *string              `json:"refusal,omitempty"`
	ImageURL *ChatInputImage      `json:"image_url,omitempty"`
}

// ChatMessageContent is the content of a chat message: either a plain string
// or an array of content blocks. Exactly one form is set.
type ChatMessageContent struct {
	ContentStr    *string
	ContentBlocks []ChatContentBlock
}

// MarshalJSON emits the active form directly, without a wrapper object.
func (c ChatMessageContent) MarshalJSON() ([]byte, error) {
	if c.ContentStr != nil && c.ContentBlocks != nil {
		return nil, fmt.Errorf("both ContentStr and ContentBlocks are set; only one should be non-nil")
	}
	if c.ContentStr != nil {
		return json.Marshal(*c.ContentStr)
	}
	if c.ContentBlocks != nil {
		return json.Marshal(c.ContentBlocks)
	}
	return []byte("null"), nil
}

// UnmarshalJSON decodes a string or an array of content blocks.
func (c *ChatMessageContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.ContentStr = nil
		c.ContentBlocks = nil
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		c.ContentStr = &str
		c.ContentBlocks = nil
		return nil
	}
	var blocks []ChatContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		c.ContentBlocks = blocks
		c.ContentStr = nil
		return nil
	}
	return fmt.Errorf("content field is neither a string nor an array of content blocks")
}

// ChatReasoning configures reasoning output for chat completions.
type ChatReasoning struct {
	Enabled   *bool   `json:"enabled,omitempty"`
	Effort    *string `json:"effort,omitempty"`
	MaxTokens *int    `json:"max_tokens,omitempty"`
	Display   *string `json:"display,omitempty"`
}

// ChatStreamOptions configures chat completion streaming.
type ChatStreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

// ChatToolType is the type of a chat tool.
type ChatToolType string

// ChatToolType values.
const (
	ChatToolTypeFunction ChatToolType = "function"
	ChatToolTypeCustom   ChatToolType = "custom"
)

// ChatToolFunction is the function definition of a function tool.
type ChatToolFunction struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// ChatTool is a tool definition in a chat completion request.
type ChatTool struct {
	Type     ChatToolType      `json:"type"`
	Function *ChatToolFunction `json:"function,omitempty"`
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

// MarshalJSON emits the active form directly.
func (c ChatToolChoice) MarshalJSON() ([]byte, error) {
	if c.Str != nil && c.Struct != nil {
		return nil, fmt.Errorf("both Str and Struct are set; only one should be non-nil")
	}
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.Struct != nil {
		return json.Marshal(c.Struct)
	}
	return []byte("null"), nil
}

// UnmarshalJSON decodes a string or an object.
func (c *ChatToolChoice) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.Str = nil
		c.Struct = nil
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		c.Str = &str
		c.Struct = nil
		return nil
	}
	var s ChatToolChoiceStruct
	if err := json.Unmarshal(data, &s); err == nil {
		c.Struct = &s
		c.Str = nil
		return nil
	}
	return fmt.Errorf("tool_choice field is neither a string nor a tool choice object")
}

// ChatToolMessage carries the tool_call_id of a tool-role message.
type ChatToolMessage struct {
	ToolCallID *string `json:"tool_call_id,omitempty"`
}

// ChatReasoningDetailsType is the type of a reasoning details block.
type ChatReasoningDetailsType string

// ChatReasoningDetailsType values.
const (
	ChatReasoningDetailsTypeSummary       ChatReasoningDetailsType = "reasoning.summary"
	ChatReasoningDetailsTypeEncrypted     ChatReasoningDetailsType = "reasoning.encrypted"
	ChatReasoningDetailsTypeText          ChatReasoningDetailsType = "reasoning.text"
	ChatReasoningDetailsTypeContentBlocks ChatReasoningDetailsType = "reasoning.content_blocks"
)

// ChatReasoningDetails is a reasoning block attached to an assistant message.
type ChatReasoningDetails struct {
	Index     int                      `json:"index"`
	Type      ChatReasoningDetailsType `json:"type"`
	Summary   *string                  `json:"summary,omitempty"`
	Text      *string                  `json:"text,omitempty"`
	Signature *string                  `json:"signature,omitempty"`
	Data      *string                  `json:"data,omitempty"`
}

// ChatAssistantMessageToolCallFunction is the function payload of a tool call.
type ChatAssistantMessageToolCallFunction struct {
	Name      *string `json:"name"`
	Arguments string  `json:"arguments"` // stringified JSON, may be partial while streaming
}

// ChatAssistantMessageToolCall is a tool call emitted by the assistant.
type ChatAssistantMessageToolCall struct {
	Index    *int                                 `json:"index,omitempty"`
	Type     *string                              `json:"type,omitempty"`
	ID       *string                              `json:"id,omitempty"`
	Function ChatAssistantMessageToolCallFunction `json:"function"`
}

// ChatAssistantMessage carries the assistant-only fields of a chat message.
type ChatAssistantMessage struct {
	Refusal          *string                        `json:"refusal,omitempty"`
	Reasoning        *string                        `json:"reasoning,omitempty"`
	ReasoningDetails []ChatReasoningDetails         `json:"reasoning_details,omitempty"`
	ToolCalls        []ChatAssistantMessageToolCall `json:"tool_calls,omitempty"`
}

// ChatMessage is a message in a chat conversation. Assistant messages flatten
// tool_calls and reasoning fields; tool messages flatten tool_call_id.
type ChatMessage struct {
	Name    *string             `json:"name,omitempty"`
	Role    ChatMessageRole     `json:"role,omitempty"`
	Content *ChatMessageContent `json:"content,omitempty"`

	*ChatToolMessage
	*ChatAssistantMessage
}

// ChatRequest is a chat completions request.
type ChatRequest struct {
	Model               string             `json:"model"`
	Messages            []ChatMessage      `json:"messages"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	Stop                []string           `json:"stop,omitempty"`
	Stream              *bool              `json:"stream,omitempty"`
	StreamOptions       *ChatStreamOptions `json:"stream_options,omitempty"`
	Tools               []ChatTool         `json:"tools,omitempty"`
	ToolChoice          *ChatToolChoice    `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool              `json:"parallel_tool_calls,omitempty"`
	Reasoning           *ChatReasoning     `json:"reasoning,omitempty"`
	Metadata            map[string]any     `json:"metadata,omitempty"`
	User                *string            `json:"user,omitempty"`
	Store               *bool              `json:"store,omitempty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64           `json:"presence_penalty,omitempty"`
	Seed                *int               `json:"seed,omitempty"`
	N                   *int               `json:"n,omitempty"`
}

// ChatPromptTokensDetails breaks down prompt tokens. The cached token field is
// always present on the wire (zero when unused), matching the real API.
type ChatPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ChatCompletionTokensDetails breaks down completion tokens. The reasoning
// token field is always present on the wire (zero when unused), matching the
// real API.
type ChatCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ChatLLMUsage is the token usage of a chat completion.
type ChatLLMUsage struct {
	PromptTokens            int                          `json:"prompt_tokens,omitempty"`
	CompletionTokens        int                          `json:"completion_tokens,omitempty"`
	TotalTokens             int                          `json:"total_tokens"`
	PromptTokensDetails     *ChatPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *ChatCompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// ChatStreamDelta is the delta payload of a streaming chat completion chunk.
type ChatStreamDelta struct {
	Role             *string                        `json:"role,omitempty"`
	Content          *string                        `json:"content,omitempty"`
	Refusal          *string                        `json:"refusal,omitempty"`
	Reasoning        *string                        `json:"reasoning,omitempty"`
	ReasoningDetails []ChatReasoningDetails         `json:"reasoning_details,omitempty"`
	ToolCalls        []ChatAssistantMessageToolCall `json:"tool_calls,omitempty"`
}

// ChatChoice is one choice of a chat completion response, in either the
// non-streaming (Message) or streaming (Delta) form.
type ChatChoice struct {
	Index        int              `json:"index"`
	FinishReason *string          `json:"finish_reason,omitempty"`
	Message      *ChatMessage     `json:"message,omitempty"`
	Delta        *ChatStreamDelta `json:"delta,omitempty"`
}

// ChatResponse is a chat completions response.
type ChatResponse struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
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
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
	Choices           []ChatChoice  `json:"choices"`
	Usage             *ChatLLMUsage `json:"usage,omitempty"`
}
