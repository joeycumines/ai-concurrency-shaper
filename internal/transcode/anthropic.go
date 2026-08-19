package transcode

// The Anthropic Messages wire definitions live in the pinned wire package
// wire/anthropicmessages (see contracts.lock.json); this file re-exports
// them under the package's historical names so consumers compile unchanged.
// New code should prefer the wire package names.

import "github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"

// AnthropicMessageRole is the role of an Anthropic message.
type AnthropicMessageRole = anthropicmessages.Role

// AnthropicMessageRole values.
const (
	AnthropicMessageRoleUser      = anthropicmessages.RoleUser
	AnthropicMessageRoleAssistant = anthropicmessages.RoleAssistant
	AnthropicMessageRoleSystem    = anthropicmessages.RoleSystem
)

// AnthropicContentBlockType is the type of an Anthropic content block.
type AnthropicContentBlockType = anthropicmessages.ContentBlockType

// AnthropicContentBlockType values.
const (
	AnthropicContentBlockTypeText             = anthropicmessages.ContentBlockTypeText
	AnthropicContentBlockTypeImage            = anthropicmessages.ContentBlockTypeImage
	AnthropicContentBlockTypeDocument         = anthropicmessages.ContentBlockTypeDocument
	AnthropicContentBlockTypeToolUse          = anthropicmessages.ContentBlockTypeToolUse
	AnthropicContentBlockTypeToolResult       = anthropicmessages.ContentBlockTypeToolResult
	AnthropicContentBlockTypeThinking         = anthropicmessages.ContentBlockTypeThinking
	AnthropicContentBlockTypeRedactedThinking = anthropicmessages.ContentBlockTypeRedactedThinking
)

// AnthropicSourceType is the type of an image or document source.
type AnthropicSourceType = anthropicmessages.SourceType

// AnthropicSourceType values.
const (
	AnthropicSourceTypeBase64 = anthropicmessages.SourceTypeBase64
	AnthropicSourceTypeURL    = anthropicmessages.SourceTypeURL
)

// AnthropicSource is the source of an image or document block.
type AnthropicSource = anthropicmessages.Source

// AnthropicContentBlock is one content block of an Anthropic message.
type AnthropicContentBlock = anthropicmessages.ContentBlock

// AnthropicContent is the content of an Anthropic message: either a plain
// string or an array of content blocks. Exactly one form is set.
type AnthropicContent = anthropicmessages.Content

// AnthropicMessage is a message in an Anthropic conversation.
type AnthropicMessage = anthropicmessages.Message

// AnthropicTool is a tool definition in an Anthropic request.
type AnthropicTool = anthropicmessages.Tool

// AnthropicToolChoice configures tool selection.
type AnthropicToolChoice = anthropicmessages.ToolChoice

// AnthropicStopReason is the reason the model stopped generating.
type AnthropicStopReason = anthropicmessages.StopReason

// AnthropicStopReason values.
const (
	AnthropicStopReasonEndTurn      = anthropicmessages.StopReasonEndTurn
	AnthropicStopReasonMaxTokens    = anthropicmessages.StopReasonMaxTokens
	AnthropicStopReasonStopSequence = anthropicmessages.StopReasonStopSequence
	AnthropicStopReasonToolUse      = anthropicmessages.StopReasonToolUse
	AnthropicStopReasonPauseTurn    = anthropicmessages.StopReasonPauseTurn
	AnthropicStopReasonRefusal      = anthropicmessages.StopReasonRefusal
)

// AnthropicStopDetails carries the refusal stop details of a response.
type AnthropicStopDetails = anthropicmessages.StopDetails

// AnthropicOutputTokensDetails breaks down output tokens.
type AnthropicOutputTokensDetails = anthropicmessages.OutputTokensDetails

// AnthropicUsage is the token usage of an Anthropic response.
type AnthropicUsage = anthropicmessages.Usage

// AnthropicMessageResponse is an Anthropic messages API response.
type AnthropicMessageResponse = anthropicmessages.Response

// AnthropicStreamEventType is the type of an Anthropic SSE event.
type AnthropicStreamEventType = anthropicmessages.StreamEventType

// AnthropicStreamEventType values.
const (
	AnthropicStreamEventTypeMessageStart      = anthropicmessages.StreamEventTypeMessageStart
	AnthropicStreamEventTypeMessageStop       = anthropicmessages.StreamEventTypeMessageStop
	AnthropicStreamEventTypeContentBlockStart = anthropicmessages.StreamEventTypeContentBlockStart
	AnthropicStreamEventTypeContentBlockDelta = anthropicmessages.StreamEventTypeContentBlockDelta
	AnthropicStreamEventTypeContentBlockStop  = anthropicmessages.StreamEventTypeContentBlockStop
	AnthropicStreamEventTypeMessageDelta      = anthropicmessages.StreamEventTypeMessageDelta
	AnthropicStreamEventTypePing              = anthropicmessages.StreamEventTypePing
	AnthropicStreamEventTypeError             = anthropicmessages.StreamEventTypeError
)

// AnthropicStreamDeltaType is the type of an Anthropic stream delta.
type AnthropicStreamDeltaType = anthropicmessages.StreamDeltaType

// AnthropicStreamDeltaType values.
const (
	AnthropicStreamDeltaTypeTextDelta      = anthropicmessages.StreamDeltaTypeTextDelta
	AnthropicStreamDeltaTypeInputJSONDelta = anthropicmessages.StreamDeltaTypeInputJSONDelta
	AnthropicStreamDeltaTypeThinkingDelta  = anthropicmessages.StreamDeltaTypeThinkingDelta
	AnthropicStreamDeltaTypeSignatureDelta = anthropicmessages.StreamDeltaTypeSignatureDelta
	AnthropicStreamDeltaTypeCitationsDelta = anthropicmessages.StreamDeltaTypeCitationsDelta
)

// AnthropicStreamDelta is the delta payload of an Anthropic SSE event.
type AnthropicStreamDelta = anthropicmessages.StreamDelta

// AnthropicStreamError is the nested error object of an Anthropic error
// event.
type AnthropicStreamError = anthropicmessages.StreamError

// AnthropicStreamEvent is one SSE event of an Anthropic messages stream.
type AnthropicStreamEvent = anthropicmessages.StreamEvent
