package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// =============================================================================
// Anthropic Messages API
// =============================================================================
//
// https://platform.claude.com/docs/en/api/messages
// https://platform.claude.com/docs/en/build-with-claude/streaming

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

// AnthropicSourceType is the type of an image or document source.
type AnthropicSourceType string

// AnthropicSourceType values.
const (
	AnthropicSourceTypeBase64 AnthropicSourceType = "base64"
	AnthropicSourceTypeURL    AnthropicSourceType = "url"
)

// AnthropicSource is the source of an image or document block.
type AnthropicSource struct {
	Type      AnthropicSourceType `json:"type"`
	MediaType string              `json:"media_type"`
	Data      string              `json:"data,omitempty"`
	URL       string              `json:"url,omitempty"`
}

// Validate checks the source shape. media_type is required for base64
// sources; URL sources may omit it (the API derives it, e.g. application/pdf
// for documents), matching the official SDKs' URL source params.
func (s AnthropicSource) Validate() error {
	switch s.Type {
	case AnthropicSourceTypeBase64:
		if s.Data == "" {
			return errors.New("base64 source has no data")
		}
		if s.MediaType == "" {
			return errors.New("base64 source has no media_type")
		}
	case AnthropicSourceTypeURL:
		if s.URL == "" {
			return errors.New("url source has no url")
		}
	default:
		return fmt.Errorf("unknown anthropic source type %q", s.Type)
	}
	return nil
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

// Validate checks the block shape.
func (b AnthropicContentBlock) Validate() error {
	switch b.Type {
	case AnthropicContentBlockTypeText:
		if b.Text == nil {
			return errors.New("text block has no text")
		}
	case AnthropicContentBlockTypeImage:
		if b.Source == nil {
			return errors.New("image block has no source")
		}
		return b.Source.Validate()
	case AnthropicContentBlockTypeDocument:
		if b.Source == nil {
			return errors.New("document block has no source")
		}
		return b.Source.Validate()
	case AnthropicContentBlockTypeToolUse:
		if b.ID == nil || *b.ID == "" {
			return errors.New("tool_use block has no id")
		}
		if b.Name == nil || *b.Name == "" {
			return errors.New("tool_use block has no name")
		}
		if len(b.Input) == 0 {
			return errors.New("tool_use block has no input")
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b.Input, &probe); err != nil {
			return errors.New("tool_use input is not a JSON object")
		}
	case AnthropicContentBlockTypeToolResult:
		if b.ToolUseID == nil || *b.ToolUseID == "" {
			return errors.New("tool_result block has no tool_use_id")
		}
		if b.Content == nil {
			return errors.New("tool_result block has no content")
		}
	case AnthropicContentBlockTypeThinking:
		if b.Thinking == nil || b.Signature == nil {
			return errors.New("thinking block requires thinking and signature")
		}
	case AnthropicContentBlockTypeRedactedThinking:
		if b.Data == nil {
			return errors.New("redacted_thinking block has no data")
		}
	default:
		return fmt.Errorf("unknown anthropic content block type %q", b.Type)
	}
	return nil
}

// AnthropicContent is the content of an Anthropic message: either a plain
// string or an array of content blocks. Exactly one form is set.
type AnthropicContent struct {
	ContentStr    *string
	ContentBlocks []AnthropicContentBlock
}

// Validate checks the union invariants and each block.
func (c AnthropicContent) Validate() error {
	if c.ContentStr != nil && c.ContentBlocks != nil {
		return errors.New("anthropic content has both string and block variants")
	}
	if c.ContentStr == nil && c.ContentBlocks == nil {
		return errors.New("anthropic content has no selected variant")
	}
	for i, block := range c.ContentBlocks {
		if err := block.Validate(); err != nil {
			return fmt.Errorf("anthropic content block %d: %w", i, err)
		}
	}
	return nil
}

// MarshalJSON emits the active form, without a wrapper object.
func (c AnthropicContent) MarshalJSON() ([]byte, error) {
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
func (c *AnthropicContent) UnmarshalJSON(data []byte) error {
	data = trimJSONSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("anthropic content is null")
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
	var blocks []AnthropicContentBlock
	if err := strictDecode(data, &blocks); err != nil {
		return err
	}
	c.ContentBlocks = blocks
	c.ContentStr = nil
	return c.Validate()
}

// AnthropicMessage is a message in an Anthropic conversation.
type AnthropicMessage struct {
	Role    AnthropicMessageRole `json:"role"`
	Content AnthropicContent     `json:"content"`
}

// Validate checks the message shape.
func (m AnthropicMessage) Validate() error {
	switch m.Role {
	case AnthropicMessageRoleUser, AnthropicMessageRoleAssistant:
	default:
		return fmt.Errorf("unknown anthropic message role %q", m.Role)
	}
	return m.Content.Validate()
}

// AnthropicTool is a tool definition in an Anthropic request.
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// Validate checks the tool shape.
func (t AnthropicTool) Validate() error {
	if t.Name == "" {
		return errors.New("anthropic tool name is empty")
	}
	if t.InputSchema == nil {
		return errors.New("anthropic tool has no input_schema")
	}
	return nil
}

// AnthropicToolChoice configures tool selection.
type AnthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
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

// AnthropicStreamError is the nested error object of an Anthropic error
// event. The official stream error contract requires it.
//
// https://platform.claude.com/docs/en/build-with-claude/streaming#error-events
type AnthropicStreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// AnthropicStreamEvent is one SSE event of an Anthropic messages stream.
type AnthropicStreamEvent struct {
	Type         AnthropicStreamEventType  `json:"type"`
	Message      *AnthropicMessageResponse `json:"message,omitempty"`
	Index        *int                      `json:"index,omitempty"`
	ContentBlock *AnthropicContentBlock    `json:"content_block,omitempty"`
	Delta        *AnthropicStreamDelta     `json:"delta,omitempty"`
	Usage        *AnthropicUsage           `json:"usage,omitempty"`
	Error        *AnthropicStreamError     `json:"error,omitempty"`
}
