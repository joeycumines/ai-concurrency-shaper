// Package transcode implements HTTP request/response transcoding between the
// OpenAI Responses API, the OpenAI Chat Completions API, and the Anthropic
// Messages API.
//
// The schema types in this file model the well-defined wire formats of those
// three external APIs. Union-typed fields (content as string-or-array,
// tool_choice as string-or-object, tool output as string-or-blocks) mirror the
// JSON semantics of the Bifrost reference implementation
// (github.com/joeycumines/bifrost, core/schemas and
// core/providers/anthropic/types.go) so payloads in the internal test corpus
// round-trip faithfully.
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

// =============================================================================
// OpenAI Responses API
// =============================================================================

// ResponsesMessageType is the type of a responses API input item or output item.
type ResponsesMessageType string

// ResponsesMessageType values.
const (
	ResponsesMessageTypeMessage            ResponsesMessageType = "message"
	ResponsesMessageTypeReasoning          ResponsesMessageType = "reasoning"
	ResponsesMessageTypeFunctionCall       ResponsesMessageType = "function_call"
	ResponsesMessageTypeFunctionCallOutput ResponsesMessageType = "function_call_output"
	ResponsesMessageTypeItemReference      ResponsesMessageType = "item_reference"
	ResponsesMessageTypeRefusal            ResponsesMessageType = "refusal"
)

// ResponsesMessageRoleType is the role of a responses message item.
type ResponsesMessageRoleType string

// ResponsesMessageRoleType values.
const (
	ResponsesMessageRoleAssistant ResponsesMessageRoleType = "assistant"
	ResponsesMessageRoleUser      ResponsesMessageRoleType = "user"
	ResponsesMessageRoleSystem    ResponsesMessageRoleType = "system"
	ResponsesMessageRoleDeveloper ResponsesMessageRoleType = "developer"
)

// ResponsesMessageContentBlockType is the type of a responses content block.
type ResponsesMessageContentBlockType string

// ResponsesMessageContentBlockType values.
const (
	ResponsesMessageContentBlockTypeInputText  ResponsesMessageContentBlockType = "input_text"
	ResponsesMessageContentBlockTypeInputImage ResponsesMessageContentBlockType = "input_image"
	ResponsesMessageContentBlockTypeInputFile  ResponsesMessageContentBlockType = "input_file"
	ResponsesMessageContentBlockTypeOutputText ResponsesMessageContentBlockType = "output_text"
	ResponsesMessageContentBlockTypeRefusal    ResponsesMessageContentBlockType = "refusal"
	ResponsesMessageContentBlockTypeReasoning  ResponsesMessageContentBlockType = "reasoning_text"
)

// ResponsesReasoningSummary is one summary block of a reasoning item.
type ResponsesReasoningSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// ResponsesMessageContentBlock is one content block of a responses message.
type ResponsesMessageContentBlock struct {
	Type      ResponsesMessageContentBlockType `json:"type"`
	Text      *string                          `json:"text,omitempty"`
	Signature *string                          `json:"signature,omitempty"`
	Summary   []ResponsesReasoningSummary      `json:"summary,omitempty"`
}

// ResponsesMessageContent is the content of a responses message item: either
// a plain string or an array of content blocks. Exactly one form is set.
type ResponsesMessageContent struct {
	ContentStr    *string
	ContentBlocks []ResponsesMessageContentBlock
}

// MarshalJSON emits the active form directly, without a wrapper object.
func (c ResponsesMessageContent) MarshalJSON() ([]byte, error) {
	if c.ContentStr != nil && c.ContentBlocks != nil {
		return nil, fmt.Errorf("both ContentStr and ContentBlocks are set; only one should be non-nil")
	}
	if c.ContentStr != nil {
		return json.Marshal(*c.ContentStr)
	}
	if c.ContentBlocks != nil {
		return json.Marshal(c.ContentBlocks)
	}
	// The Responses API requires content to be a string or array, never null.
	return []byte(`""`), nil
}

// UnmarshalJSON decodes a string or an array of content blocks.
func (c *ResponsesMessageContent) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		c.ContentStr = &str
		c.ContentBlocks = nil
		return nil
	}
	var blocks []ResponsesMessageContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		c.ContentBlocks = blocks
		c.ContentStr = nil
		return nil
	}
	return fmt.Errorf("content field is neither a string nor an array of content blocks")
}

// ResponsesToolMessageOutput is the output of a tool call item: either a plain
// string or an array of content blocks. Exactly one form is set.
type ResponsesToolMessageOutput struct {
	Str    *string
	Blocks []ResponsesMessageContentBlock
}

// MarshalJSON emits the active form directly.
func (o ResponsesToolMessageOutput) MarshalJSON() ([]byte, error) {
	if o.Str != nil && o.Blocks != nil {
		return nil, fmt.Errorf("both Str and Blocks are set; only one should be non-nil")
	}
	if o.Str != nil {
		return json.Marshal(*o.Str)
	}
	if o.Blocks != nil {
		return json.Marshal(o.Blocks)
	}
	// A tool may legitimately produce no output.
	return []byte(`""`), nil
}

// UnmarshalJSON decodes a string or an array of content blocks.
func (o *ResponsesToolMessageOutput) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		o.Str = &str
		o.Blocks = nil
		return nil
	}
	var blocks []ResponsesMessageContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		o.Blocks = blocks
		o.Str = nil
		return nil
	}
	return fmt.Errorf("output field is neither a string nor an array of content blocks")
}

// ResponsesToolMessage carries the tool-call fields of function_call and
// function_call_output items.
type ResponsesToolMessage struct {
	CallID    *string                     `json:"call_id,omitempty"`
	Name      *string                     `json:"name,omitempty"`
	Arguments *string                     `json:"arguments,omitempty"`
	Output    *ResponsesToolMessageOutput `json:"output,omitempty"`
	Error     *string                     `json:"error,omitempty"`
}

// ResponsesReasoning carries the summary of a reasoning item.
type ResponsesReasoning struct {
	Summary          []ResponsesReasoningSummary `json:"summary"`
	EncryptedContent *string                     `json:"encrypted_content,omitempty"`
}

// ResponsesMessage is a responses API input item or output item. Reasoning
// items flatten summary; function call items flatten call_id/name/arguments.
type ResponsesMessage struct {
	ID      *string                   `json:"id,omitempty"`
	Type    *ResponsesMessageType     `json:"type,omitempty"`
	Status  *string                   `json:"status,omitempty"`
	Role    *ResponsesMessageRoleType `json:"role,omitempty"`
	Content *ResponsesMessageContent  `json:"content,omitempty"`

	*ResponsesToolMessage
	*ResponsesReasoning
}

// ResponsesToolType is the type of a responses tool.
type ResponsesToolType string

// ResponsesToolType values.
const (
	ResponsesToolTypeFunction ResponsesToolType = "function"
)

// ResponsesTool is a tool definition in a responses request.
type ResponsesTool struct {
	Type        ResponsesToolType `json:"type"`
	Name        *string           `json:"name,omitempty"`
	Description *string           `json:"description,omitempty"`
	Parameters  map[string]any    `json:"parameters,omitempty"`
	Strict      *bool             `json:"strict,omitempty"`
}

// ResponsesToolChoiceStruct is the object form of a responses tool choice.
type ResponsesToolChoiceStruct struct {
	Type string  `json:"type"`
	Name *string `json:"name,omitempty"`
}

// ResponsesToolChoice is a union: either a plain string ("none", "auto",
// "required") or an object naming a specific tool.
type ResponsesToolChoice struct {
	Str    *string
	Struct *ResponsesToolChoiceStruct
}

// MarshalJSON emits the active form directly.
func (c ResponsesToolChoice) MarshalJSON() ([]byte, error) {
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
func (c *ResponsesToolChoice) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		c.Str = &str
		c.Struct = nil
		return nil
	}
	var s ResponsesToolChoiceStruct
	if err := json.Unmarshal(data, &s); err == nil {
		c.Struct = &s
		c.Str = nil
		return nil
	}
	return fmt.Errorf("tool_choice field is neither a string nor a tool choice object")
}

// ResponsesReasoningConfig configures reasoning for a responses request.
type ResponsesReasoningConfig struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// ResponsesTextConfigFormat is the format of response text.
type ResponsesTextConfigFormat struct {
	Type string `json:"type"`
}

// ResponsesTextConfig configures response text.
type ResponsesTextConfig struct {
	Format *ResponsesTextConfigFormat `json:"format,omitempty"`
}

// ResponsesStreamOptions configures responses streaming.
type ResponsesStreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

// ResponsesRequest is a responses API request.
type ResponsesRequest struct {
	Model              string                    `json:"model"`
	Instructions       *string                   `json:"instructions,omitempty"`
	Input              []ResponsesMessage        `json:"input,omitempty"`
	Tools              []ResponsesTool           `json:"tools,omitempty"`
	ToolChoice         *ResponsesToolChoice      `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                     `json:"parallel_tool_calls,omitempty"`
	Reasoning          *ResponsesReasoningConfig `json:"reasoning,omitempty"`
	MaxOutputTokens    *int                      `json:"max_output_tokens,omitempty"`
	Temperature        *float64                  `json:"temperature,omitempty"`
	TopP               *float64                  `json:"top_p,omitempty"`
	Stream             *bool                     `json:"stream,omitempty"`
	StreamOptions      *ResponsesStreamOptions   `json:"stream_options,omitempty"`
	Store              *bool                     `json:"store,omitempty"`
	Metadata           map[string]any            `json:"metadata,omitempty"`
	User               *string                   `json:"user,omitempty"`
	PreviousResponseID *string                   `json:"previous_response_id,omitempty"`
	Text               *ResponsesTextConfig      `json:"text,omitempty"`
	Truncation         *string                   `json:"truncation,omitempty"`
	Include            []string                  `json:"include,omitempty"`
}

// ResponsesResponseInputTokens breaks down input tokens. The cached token
// field is always present on the wire (zero when unused), matching the real
// API.
type ResponsesResponseInputTokens struct {
	CachedTokens int `json:"cached_tokens"`
}

// ResponsesResponseOutputTokens breaks down output tokens.
type ResponsesResponseOutputTokens struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ResponsesResponseUsage is the token usage of a responses response.
type ResponsesResponseUsage struct {
	InputTokens         int                            `json:"input_tokens"`
	InputTokensDetails  *ResponsesResponseInputTokens  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                            `json:"output_tokens"`
	OutputTokensDetails *ResponsesResponseOutputTokens `json:"output_tokens_details,omitempty"`
	TotalTokens         int                            `json:"total_tokens"`
}

// ResponsesStopDetails carries the refusal stop details of a response.
type ResponsesStopDetails struct {
	Type             string  `json:"type"`
	Category         *string `json:"category,omitempty"`
	Explanation      *string `json:"explanation,omitempty"`
	RecommendedModel *string `json:"recommended_model,omitempty"`
}

// ResponsesResponseError is the error payload of a responses response or of a
// stream error event.
type ResponsesResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResponsesResponse is a responses API response.
type ResponsesResponse struct {
	ID                 string                    `json:"id"`
	Object             string                    `json:"object"`
	CreatedAt          int64                     `json:"created_at"`
	Status             *string                   `json:"status,omitempty"`
	Error              json.RawMessage           `json:"error,omitempty"`
	IncompleteDetails  json.RawMessage           `json:"incomplete_details,omitempty"`
	Instructions       *string                   `json:"instructions,omitempty"`
	MaxOutputTokens    *int                      `json:"max_output_tokens,omitempty"`
	Model              string                    `json:"model"`
	Output             []ResponsesMessage        `json:"output,omitempty"`
	ParallelToolCalls  *bool                     `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID *string                   `json:"previous_response_id"`
	Reasoning          *ResponsesReasoningConfig `json:"reasoning,omitempty"`
	Store              *bool                     `json:"store,omitempty"`
	Temperature        *float64                  `json:"temperature,omitempty"`
	Text               *ResponsesTextConfig      `json:"text,omitempty"`
	ToolChoice         *ResponsesToolChoice      `json:"tool_choice,omitempty"`
	Tools              []ResponsesTool           `json:"tools,omitempty"`
	TopP               *float64                  `json:"top_p,omitempty"`
	Truncation         *string                   `json:"truncation,omitempty"`
	Usage              *ResponsesResponseUsage   `json:"usage,omitempty"`
	User               *string                   `json:"user,omitempty"`
	Metadata           map[string]any            `json:"metadata,omitempty"`
	StopDetails        *ResponsesStopDetails     `json:"stop_details,omitempty"`
}

// ResponsesStreamResponseType is the type of a responses SSE event.
type ResponsesStreamResponseType string

// ResponsesStreamResponseType values.
const (
	ResponsesStreamResponseTypePing       ResponsesStreamResponseType = "response.ping"
	ResponsesStreamResponseTypeCreated    ResponsesStreamResponseType = "response.created"
	ResponsesStreamResponseTypeInProgress ResponsesStreamResponseType = "response.in_progress"
	ResponsesStreamResponseTypeCompleted  ResponsesStreamResponseType = "response.completed"
	ResponsesStreamResponseTypeFailed     ResponsesStreamResponseType = "response.failed"
	ResponsesStreamResponseTypeIncomplete ResponsesStreamResponseType = "response.incomplete"

	ResponsesStreamResponseTypeOutputItemAdded ResponsesStreamResponseType = "response.output_item.added"
	ResponsesStreamResponseTypeOutputItemDone  ResponsesStreamResponseType = "response.output_item.done"

	ResponsesStreamResponseTypeContentPartAdded ResponsesStreamResponseType = "response.content_part.added"
	ResponsesStreamResponseTypeContentPartDone  ResponsesStreamResponseType = "response.content_part.done"

	ResponsesStreamResponseTypeOutputTextDelta ResponsesStreamResponseType = "response.output_text.delta"
	ResponsesStreamResponseTypeOutputTextDone  ResponsesStreamResponseType = "response.output_text.done"

	ResponsesStreamResponseTypeReasoningSummaryPartAdded ResponsesStreamResponseType = "response.reasoning_summary_part.added"
	ResponsesStreamResponseTypeReasoningSummaryPartDone  ResponsesStreamResponseType = "response.reasoning_summary_part.done"
	ResponsesStreamResponseTypeReasoningSummaryTextDelta ResponsesStreamResponseType = "response.reasoning_summary_text.delta"
	ResponsesStreamResponseTypeReasoningSummaryTextDone  ResponsesStreamResponseType = "response.reasoning_summary_text.done"

	ResponsesStreamResponseTypeFunctionCallArgumentsDelta ResponsesStreamResponseType = "response.function_call_arguments.delta"
	ResponsesStreamResponseTypeFunctionCallArgumentsDone  ResponsesStreamResponseType = "response.function_call_arguments.done"

	ResponsesStreamResponseTypeError ResponsesStreamResponseType = "error"
)

// ResponsesStreamResponse is one SSE event of a responses stream.
type ResponsesStreamResponse struct {
	Type           ResponsesStreamResponseType `json:"type"`
	SequenceNumber int                         `json:"sequence_number,omitempty"`

	Response *ResponsesResponse `json:"response,omitempty"`

	OutputIndex  *int                          `json:"output_index,omitempty"`
	SummaryIndex *int                          `json:"summary_index,omitempty"`
	Item         *ResponsesMessage             `json:"item,omitempty"`
	ContentIndex *int                          `json:"content_index,omitempty"`
	ItemID       *string                       `json:"item_id,omitempty"`
	Part         *ResponsesMessageContentBlock `json:"part,omitempty"`

	Delta     *string                 `json:"delta,omitempty"`
	Text      *string                 `json:"text,omitempty"`
	Arguments *string                 `json:"arguments,omitempty"`
	Error     *ResponsesResponseError `json:"error,omitempty"`
}

// =============================================================================
// Anthropic Messages API
// =============================================================================

// AnthropicMessageRole is the role of an Anthropic message.
type AnthropicMessageRole string

// AnthropicMessageRole values.
const (
	AnthropicMessageRoleUser      AnthropicMessageRole = "user"
	AnthropicMessageRoleAssistant AnthropicMessageRole = "assistant"
)

// AnthropicContentBlockType is the type of an Anthropic content block.
type AnthropicContentBlockType string

// AnthropicContentBlockType values.
const (
	AnthropicContentBlockTypeText             AnthropicContentBlockType = "text"
	AnthropicContentBlockTypeImage            AnthropicContentBlockType = "image"
	AnthropicContentBlockTypeDocument         AnthropicContentBlockType = "document"
	AnthropicContentBlockTypeToolUse          AnthropicContentBlockType = "tool_use"
	AnthropicContentBlockTypeToolResult       AnthropicContentBlockType = "tool_result"
	AnthropicContentBlockTypeThinking         AnthropicContentBlockType = "thinking"
	AnthropicContentBlockTypeRedactedThinking AnthropicContentBlockType = "redacted_thinking"
)

// AnthropicSource is the source of an image or document block.
type AnthropicSource struct {
	Type      string  `json:"type"`
	MediaType *string `json:"media_type,omitempty"`
	Data      *string `json:"data,omitempty"`
	URL       *string `json:"url,omitempty"`
}

// AnthropicContentBlock is one content block of an Anthropic message.
type AnthropicContentBlock struct {
	Type      AnthropicContentBlockType `json:"type"`
	Text      *string                   `json:"text,omitempty"`
	Thinking  *string                   `json:"thinking,omitempty"`
	Signature *string                   `json:"signature,omitempty"`
	Data      *string                   `json:"data,omitempty"` // redacted_thinking payload

	ToolUseID *string           `json:"tool_use_id,omitempty"`
	ID        *string           `json:"id,omitempty"`
	Name      *string           `json:"name,omitempty"`
	Input     json.RawMessage   `json:"input,omitempty"`
	Content   *AnthropicContent `json:"content,omitempty"`
	IsError   *bool             `json:"is_error,omitempty"`
	Source    *AnthropicSource  `json:"source,omitempty"`
}

// AnthropicContent is the content of an Anthropic message: either a plain
// string or an array of content blocks. Exactly one form is set.
type AnthropicContent struct {
	ContentStr    *string
	ContentBlocks []AnthropicContentBlock
}

// MarshalJSON emits the active form directly, without a wrapper object.
func (c AnthropicContent) MarshalJSON() ([]byte, error) {
	if c.ContentStr != nil && c.ContentBlocks != nil {
		return nil, fmt.Errorf("both ContentStr and ContentBlocks are set; only one should be non-nil")
	}
	if c.ContentStr != nil {
		return json.Marshal(*c.ContentStr)
	}
	if c.ContentBlocks != nil {
		return json.Marshal(c.ContentBlocks)
	}
	// Anthropic requires content to be an array, never null.
	return []byte("[]"), nil
}

// UnmarshalJSON decodes a string or an array of content blocks.
func (c *AnthropicContent) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		c.ContentStr = &str
		c.ContentBlocks = nil
		return nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		c.ContentBlocks = blocks
		c.ContentStr = nil
		return nil
	}
	return fmt.Errorf("content field is neither a string nor an array of content blocks")
}

// AnthropicMessage is a message in an Anthropic conversation.
type AnthropicMessage struct {
	Role    AnthropicMessageRole `json:"role"`
	Content AnthropicContent     `json:"content"`
}

// AnthropicTool is a tool definition in an Anthropic request.
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// AnthropicToolChoice configures tool selection.
type AnthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// AnthropicMessageRequest is an Anthropic messages API request.
type AnthropicMessageRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        *AnthropicContent    `json:"system,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        *bool                `json:"stream,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
}

// AnthropicStopReason is the reason the model stopped generating.
type AnthropicStopReason string

// AnthropicStopReason values.
const (
	AnthropicStopReasonEndTurn      AnthropicStopReason = "end_turn"
	AnthropicStopReasonMaxTokens    AnthropicStopReason = "max_tokens"
	AnthropicStopReasonStopSequence AnthropicStopReason = "stop_sequence"
	AnthropicStopReasonToolUse      AnthropicStopReason = "tool_use"
	AnthropicStopReasonPauseTurn    AnthropicStopReason = "pause_turn"
	AnthropicStopReasonRefusal      AnthropicStopReason = "refusal"
)

// AnthropicStopDetails carries the refusal stop details of a response.
type AnthropicStopDetails struct {
	Type             string  `json:"type"`
	Category         *string `json:"category,omitempty"`
	Explanation      *string `json:"explanation,omitempty"`
	RecommendedModel *string `json:"recommended_model,omitempty"`
}

// AnthropicOutputTokensDetails breaks down output tokens.
type AnthropicOutputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

// AnthropicUsage is the token usage of an Anthropic response. The cache token
// fields are always present on the wire (zero when unused), matching the real
// API.
type AnthropicUsage struct {
	InputTokens              int                           `json:"input_tokens"`
	CacheCreationInputTokens int                           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                           `json:"cache_read_input_tokens"`
	OutputTokens             int                           `json:"output_tokens"`
	OutputTokensDetails      *AnthropicOutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// AnthropicMessageResponse is an Anthropic messages API response.
type AnthropicMessageResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   AnthropicStopReason     `json:"stop_reason,omitempty"`
	StopSequence *string                 `json:"stop_sequence,omitempty"`
	Usage        *AnthropicUsage         `json:"usage,omitempty"`
	StopDetails  *AnthropicStopDetails   `json:"stop_details,omitempty"`
}

// AnthropicStreamEventType is the type of an Anthropic SSE event.
type AnthropicStreamEventType string

// AnthropicStreamEventType values.
const (
	AnthropicStreamEventTypeMessageStart      AnthropicStreamEventType = "message_start"
	AnthropicStreamEventTypeMessageStop       AnthropicStreamEventType = "message_stop"
	AnthropicStreamEventTypeContentBlockStart AnthropicStreamEventType = "content_block_start"
	AnthropicStreamEventTypeContentBlockDelta AnthropicStreamEventType = "content_block_delta"
	AnthropicStreamEventTypeContentBlockStop  AnthropicStreamEventType = "content_block_stop"
	AnthropicStreamEventTypeMessageDelta      AnthropicStreamEventType = "message_delta"
	AnthropicStreamEventTypePing              AnthropicStreamEventType = "ping"
	AnthropicStreamEventTypeError             AnthropicStreamEventType = "error"
)

// AnthropicStreamDeltaType is the type of an Anthropic stream delta.
type AnthropicStreamDeltaType string

// AnthropicStreamDeltaType values.
const (
	AnthropicStreamDeltaTypeTextDelta      AnthropicStreamDeltaType = "text_delta"
	AnthropicStreamDeltaTypeInputJSONDelta AnthropicStreamDeltaType = "input_json_delta"
	AnthropicStreamDeltaTypeThinkingDelta  AnthropicStreamDeltaType = "thinking_delta"
	AnthropicStreamDeltaTypeSignatureDelta AnthropicStreamDeltaType = "signature_delta"
	AnthropicStreamDeltaTypeCitationsDelta AnthropicStreamDeltaType = "citations_delta"
)

// AnthropicStreamDelta is the delta payload of an Anthropic SSE event.
type AnthropicStreamDelta struct {
	Type         AnthropicStreamDeltaType `json:"type,omitempty"`
	Text         *string                  `json:"text,omitempty"`
	PartialJSON  *string                  `json:"partial_json,omitempty"`
	Thinking     *string                  `json:"thinking,omitempty"`
	Signature    *string                  `json:"signature,omitempty"`
	StopReason   *AnthropicStopReason     `json:"stop_reason,omitempty"`
	StopSequence *string                  `json:"stop_sequence,omitempty"`
}

// AnthropicStreamEvent is one SSE event of an Anthropic messages stream.
type AnthropicStreamEvent struct {
	Type         AnthropicStreamEventType  `json:"type"`
	Message      *AnthropicMessageResponse `json:"message,omitempty"`
	Index        *int                      `json:"index,omitempty"`
	ContentBlock *AnthropicContentBlock    `json:"content_block,omitempty"`
	Delta        *AnthropicStreamDelta     `json:"delta,omitempty"`
	Usage        *AnthropicUsage           `json:"usage,omitempty"`
}
