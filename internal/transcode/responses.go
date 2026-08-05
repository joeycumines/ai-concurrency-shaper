package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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
	// ImageURL is the remote URL of an input_image block.
	ImageURL *string `json:"image_url,omitempty"`
	// ImageData is the raw image_data payload of an input_image block.
	ImageData json.RawMessage `json:"image_data,omitempty"`
	// FileID is the ID of an input_file block.
	FileID *string `json:"file_id,omitempty"`
	// Filename is the filename of an input_file block.
	Filename *string `json:"filename,omitempty"`
	// FileData is the base64-encoded file_data payload of an input_file block.
	FileData json.RawMessage `json:"file_data,omitempty"`
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
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		o.Str = nil
		o.Blocks = nil
		return nil
	}
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

// ResponsesReasoning carries the summary of a reasoning item. The summary
// field is omitted when empty, matching the wire shape of the real API.
type ResponsesReasoning struct {
	Summary          []ResponsesReasoningSummary `json:"summary,omitempty"`
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
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            *string            `json:"status,omitempty"`
	Error             json.RawMessage    `json:"error,omitempty"`
	IncompleteDetails json.RawMessage    `json:"incomplete_details,omitempty"`
	Instructions      *string            `json:"instructions,omitempty"`
	MaxOutputTokens   *int               `json:"max_output_tokens,omitempty"`
	Model             string             `json:"model"`
	Output            []ResponsesMessage `json:"output,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	// PreviousResponseID is always emitted on the wire, mirroring
	// the Bifrost reference implementation. omitempty is omitted
	// intentionally so the field is present even when null.
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

	ResponsesStreamResponseTypeRefusalDelta ResponsesStreamResponseType = "response.refusal.delta"

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
