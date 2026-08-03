package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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
