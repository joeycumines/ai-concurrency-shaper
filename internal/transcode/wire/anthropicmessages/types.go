// Package anthropicmessages implements the pinned Anthropic Messages wire
// contract (source: anthropic-api 2023-06-01, see contracts.lock.json):
// distinct strict types for the request, the response, and the stream
// events.
//
// The package is deliberately self-contained: it imports only the shared
// wire package, never the transcode package, so the pinned contract types
// can never depend on transcode internals (no import cycle).
package anthropicmessages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Role is the role of an Anthropic message.
type Role string

// Role values. RoleSystem covers the modern inline system-role message
// (Claude Code 2.1.x emits system turns inside the messages array).
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// ContentBlockType is the type of an Anthropic content block.
type ContentBlockType string

// ContentBlockType values.
const (
	ContentBlockTypeText             ContentBlockType = "text"
	ContentBlockTypeImage            ContentBlockType = "image"
	ContentBlockTypeDocument         ContentBlockType = "document"
	ContentBlockTypeToolUse          ContentBlockType = "tool_use"
	ContentBlockTypeToolResult       ContentBlockType = "tool_result"
	ContentBlockTypeThinking         ContentBlockType = "thinking"
	ContentBlockTypeRedactedThinking ContentBlockType = "redacted_thinking"
)

// SourceType is the type of an image or document source.
type SourceType string

// SourceType values.
const (
	SourceTypeBase64 SourceType = "base64"
	SourceTypeURL    SourceType = "url"
)

// Source is the source of an image or document block.
type Source struct {
	Type      SourceType `json:"type"`
	MediaType string     `json:"media_type"`
	Data      string     `json:"data,omitempty"`
	URL       string     `json:"url,omitempty"`
}

// Validate checks the source shape. media_type is required for base64
// sources; URL sources may omit it (the API derives it, e.g. application/pdf
// for documents), matching the official SDKs' URL source params.
func (s Source) Validate() error {
	switch s.Type {
	case SourceTypeBase64:
		if s.Data == "" {
			return errors.New("base64 source has no data")
		}
		if s.MediaType == "" {
			return errors.New("base64 source has no media_type")
		}
		// The source union is exclusive: a base64 source must not also
		// carry a url, and vice versa — an ambiguous source is rejected
		// instead of silently preferring one arm.
		if s.URL != "" {
			return errors.New("base64 source must not carry a url")
		}
	case SourceTypeURL:
		if s.URL == "" {
			return errors.New("url source has no url")
		}
		if s.Data != "" {
			return errors.New("url source must not carry base64 data")
		}
	default:
		return fmt.Errorf("unknown anthropic source type %q", s.Type)
	}
	return nil
}

// ContentBlock is one content block of an Anthropic message.
type ContentBlock struct {
	Type      ContentBlockType `json:"type"`
	Text      *string          `json:"text,omitempty"`
	Thinking  *string          `json:"thinking,omitempty"`
	Signature *string          `json:"signature,omitempty"`
	Data      *string          `json:"data,omitempty"` // redacted_thinking payload

	ToolUseID *string         `json:"tool_use_id,omitempty"`
	ID        *string         `json:"id,omitempty"`
	Name      *string         `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   *Content        `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
	Source    *Source         `json:"source,omitempty"`

	// CacheControl is the Anthropic prompt-cache marker (e.g.
	// {"type":"ephemeral"}) real clients (Claude Code) attach to text,
	// image, tool_use, and tool_result blocks. It is a caching performance
	// hint with no semantic content; chat upstreams cache automatically,
	// so it is accepted on the wire and not forwarded — the decode path
	// reports the drop observably (one deduped anthropic_controls note per
	// exchange).
	CacheControl any `json:"cache_control,omitempty"`
}

// UnmarshalJSON decodes the tagged union per-arm: each type admits exactly
// its own fields with DisallowUnknownFields, so a block carrying another
// arm's fields is rejected at decode instead of having those fields silently
// discarded. The public struct keeps its broad shape for rendering; a
// decode-accepted block always validates. The type probe is a lenient
// unmarshal — the arm decode applies the strictness.
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
			Type         ContentBlockType `json:"type"`
			Text         *string          `json:"text"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("text block: %w", err)
		}
		block.Type = shadow.Type
		block.Text = shadow.Text
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeImage, ContentBlockTypeDocument:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			Source       *Source          `json:"source"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("%s block: %w", probe.Type, err)
		}
		block.Type = shadow.Type
		block.Source = shadow.Source
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeToolUse:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			ID           *string          `json:"id"`
			Name         *string          `json:"name"`
			Input        json.RawMessage  `json:"input"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("tool_use block: %w", err)
		}
		block.Type = shadow.Type
		block.ID = shadow.ID
		block.Name = shadow.Name
		block.Input = shadow.Input
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeToolResult:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			ToolUseID    *string          `json:"tool_use_id"`
			Content      *Content         `json:"content"`
			IsError      *bool            `json:"is_error"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("tool_result block: %w", err)
		}
		block.Type = shadow.Type
		block.ToolUseID = shadow.ToolUseID
		block.Content = shadow.Content
		block.IsError = shadow.IsError
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeThinking:
		// cache_control is intentionally NOT admitted on this arm (and the
		// redacted_thinking arm below), unlike text/image/document/tool_use/
		// tool_result: the official prompt-caching contract states "Thinking
		// blocks cannot be cached directly with cache_control" (Anthropic,
		// Build with Claude → Prompt caching, "What cannot be cached"), so a
		// block carrying it is a contract violation on the client side and
		// the typed unknown-field rejection below is the correct strict-side
		// behavior — pinned by TestThinkingBlockCacheControlRejected (gate
		// run 1 informational note 1).
		var shadow struct {
			Type      ContentBlockType `json:"type"`
			Thinking  *string          `json:"thinking"`
			Signature *string          `json:"signature"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("thinking block: %w", err)
		}
		block.Type = shadow.Type
		block.Thinking = shadow.Thinking
		block.Signature = shadow.Signature

	case ContentBlockTypeRedactedThinking:
		// Same contract exclusion as the thinking arm above:
		// thinking-family blocks are never directly cacheable, so
		// cache_control is rejected here too.
		var shadow struct {
			Type ContentBlockType `json:"type"`
			Data *string          `json:"data"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("redacted_thinking block: %w", err)
		}
		block.Type = shadow.Type
		block.Data = shadow.Data

	default:
		return fmt.Errorf("unknown anthropic content block type %q", probe.Type)
	}

	*b = block
	return b.Validate()
}

// Validate checks the block shape.
func (b ContentBlock) Validate() error {
	switch b.Type {
	case ContentBlockTypeText:
		if b.Text == nil {
			return errors.New("text block has no text")
		}
	case ContentBlockTypeImage:
		if b.Source == nil {
			return errors.New("image block has no source")
		}
		return b.Source.Validate()
	case ContentBlockTypeDocument:
		if b.Source == nil {
			return errors.New("document block has no source")
		}
		return b.Source.Validate()
	case ContentBlockTypeToolUse:
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
	case ContentBlockTypeToolResult:
		if b.ToolUseID == nil || *b.ToolUseID == "" {
			return errors.New("tool_result block has no tool_use_id")
		}
		if b.Content == nil {
			return errors.New("tool_result block has no content")
		}
	case ContentBlockTypeThinking:
		if b.Thinking == nil || b.Signature == nil {
			return errors.New("thinking block requires thinking and signature")
		}
	case ContentBlockTypeRedactedThinking:
		if b.Data == nil {
			return errors.New("redacted_thinking block has no data")
		}
	default:
		return fmt.Errorf("unknown anthropic content block type %q", b.Type)
	}
	return nil
}

// Content is the content of an Anthropic message: either a plain string or
// an array of content blocks. Exactly one form is set.
type Content struct {
	ContentStr    *string
	ContentBlocks []ContentBlock
}

// Validate checks the union invariants and each block.
func (c Content) Validate() error {
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
func (c Content) MarshalJSON() ([]byte, error) {
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
func (c *Content) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("anthropic content is null")
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

// Message is a message in an Anthropic conversation.
type Message struct {
	Role    Role    `json:"role"`
	Content Content `json:"content"`
}

// Validate checks the message shape.
func (m Message) Validate() error {
	switch m.Role {
	case RoleUser, RoleAssistant, RoleSystem:
	default:
		return fmt.Errorf("unknown anthropic message role %q", m.Role)
	}
	return m.Content.Validate()
}

// Tool is a tool definition in an Anthropic request. InputSchema is the raw
// schema JSON: it is validated as exactly one JSON object at the
// canonical-IR boundary and passed through byte-exact, so numbers are never
// decoded and remarshaled through a map.
type Tool struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	// CacheControl is the Anthropic prompt-cache marker real clients
	// (Claude Code) attach to tool definitions; a caching performance hint
	// accepted on the wire and not forwarded — the decode path reports the
	// drop observably (one deduped anthropic_controls note per exchange).
	CacheControl any `json:"cache_control,omitempty"`
}

// Validate checks the tool shape.
func (t Tool) Validate() error {
	if t.Name == "" {
		return errors.New("anthropic tool name is empty")
	}
	if t.InputSchema == nil {
		return errors.New("anthropic tool has no input_schema")
	}
	return nil
}

// ToolChoice configures tool selection.
type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// StopReason is the reason the model stopped generating.
type StopReason string

// StopReason values.
const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonStopSequence StopReason = "stop_sequence"
	StopReasonToolUse      StopReason = "tool_use"
	StopReasonPauseTurn    StopReason = "pause_turn"
	StopReasonRefusal      StopReason = "refusal"
)

// StopDetails carries the refusal stop details of a response.
type StopDetails struct {
	Type             string  `json:"type"`
	Category         *string `json:"category,omitempty"`
	Explanation      *string `json:"explanation,omitempty"`
	RecommendedModel *string `json:"recommended_model,omitempty"`
}

// OutputTokensDetails breaks down output tokens.
type OutputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

// Usage is the token usage of an Anthropic response. The cache token fields
// are always present on the wire (zero when unused), matching the real API.
type Usage struct {
	InputTokens              int                  `json:"input_tokens"`
	CacheCreationInputTokens int                  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                  `json:"cache_read_input_tokens"`
	OutputTokens             int                  `json:"output_tokens"`
	OutputTokensDetails      *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// Response is an Anthropic messages API response.
//
// StopReason and StopSequence are nullable fields that are ALWAYS present on
// the wire: they serialize as explicit null before generation completes (the
// message_start payload) and carry their real values in message_delta or the
// completed non-stream response. They are pointers WITHOUT omitempty for
// exactly this reason.
type Response struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *StopReason    `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        *Usage         `json:"usage,omitempty"`
	StopDetails  *StopDetails   `json:"stop_details,omitempty"`
}

// Request is an Anthropic messages API request (the pinned subset the
// transcoder maps; unsupported fields are rejected by strict decoding).
type Request struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []Message       `json:"messages"`
	System        *Content        `json:"system,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        *bool           `json:"stream,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`

	// ContextManagement and OutputConfig are the newest Anthropic Messages
	// request controls (sent by Claude Code 2.1.x: context_management with
	// clear_thinking_20251015 edits, output_config with the output budget).
	// They are client-side controls with no target equivalent: accepted on
	// the wire and gated as an observable loss/reject decision
	// (anthropic_controls) by the transcode layer — never silently dropped.
	ContextManagement any `json:"context_management,omitempty"`
	OutputConfig      any `json:"output_config,omitempty"`
}

// ThinkingConfig is the Anthropic Messages thinking request configuration.
// The official contract is:
//
//	{"type": "enabled", "budget_tokens": N}
//	{"type": "disabled"}
//	{"type": "adaptive", "display": "omitted"}
//
// "adaptive" (used by Claude Code 2.1.x effort picker) delegates the
// thinking decision to the model/server. Unknown fields are rejected by the
// strict wire decode; the Type value and the per-type member set (a
// budget_tokens on disabled/adaptive or a display on enabled is malformed,
// never silently ignored — review-12 R12-L1) are validated by the transcode
// layer.
type ThinkingConfig struct {
	Type         string  `json:"type"`
	BudgetTokens *int    `json:"budget_tokens,omitempty"`
	Display      *string `json:"display,omitempty"`
}

// StreamEventType is the type of an Anthropic SSE event.
type StreamEventType string

// StreamEventType values.
const (
	StreamEventTypeMessageStart      StreamEventType = "message_start"
	StreamEventTypeMessageStop       StreamEventType = "message_stop"
	StreamEventTypeContentBlockStart StreamEventType = "content_block_start"
	StreamEventTypeContentBlockDelta StreamEventType = "content_block_delta"
	StreamEventTypeContentBlockStop  StreamEventType = "content_block_stop"
	StreamEventTypeMessageDelta      StreamEventType = "message_delta"
	StreamEventTypePing              StreamEventType = "ping"
	StreamEventTypeError             StreamEventType = "error"
)

// StreamDeltaType is the type of an Anthropic stream delta.
type StreamDeltaType string

// StreamDeltaType values.
const (
	StreamDeltaTypeTextDelta      StreamDeltaType = "text_delta"
	StreamDeltaTypeInputJSONDelta StreamDeltaType = "input_json_delta"
	StreamDeltaTypeThinkingDelta  StreamDeltaType = "thinking_delta"
	StreamDeltaTypeSignatureDelta StreamDeltaType = "signature_delta"
	StreamDeltaTypeCitationsDelta StreamDeltaType = "citations_delta"
)

// StreamDelta is the delta payload of an Anthropic SSE event.
type StreamDelta struct {
	Type         StreamDeltaType `json:"type,omitempty"`
	Text         *string         `json:"text,omitempty"`
	PartialJSON  *string         `json:"partial_json,omitempty"`
	Thinking     *string         `json:"thinking,omitempty"`
	Signature    *string         `json:"signature,omitempty"`
	StopReason   *StopReason     `json:"stop_reason,omitempty"`
	StopSequence *string         `json:"stop_sequence,omitempty"`
}

// StreamError is the nested error object of an Anthropic error event. The
// official stream error contract requires it.
type StreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// StreamEvent is one SSE event of an Anthropic messages stream. The tagged
// event shapes share one struct because every field is optional on the wire;
// per-event requiredness is enforced by the consuming stream state machine.
type StreamEvent struct {
	Type         StreamEventType `json:"type"`
	Message      *Response       `json:"message,omitempty"`
	Index        *int            `json:"index,omitempty"`
	ContentBlock *ContentBlock   `json:"content_block,omitempty"`
	Delta        *StreamDelta    `json:"delta,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
	Error        *StreamError    `json:"error,omitempty"`
}
