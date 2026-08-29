package transcode

// The OpenAI Chat Completions wire definitions live in the pinned wire
// package wire/openaichat (see contracts.lock.json); this file re-exports
// them under the package's historical names so consumers compile unchanged.
// New code should prefer the wire package names.
//
// Chat Completions is deliberately upstream-only.

import "github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"

// ChatMessageRole is the role of a chat message.
type ChatMessageRole = openaichat.MessageRole

// ChatMessageRole values.
const (
	ChatMessageRoleAssistant = openaichat.MessageRoleAssistant
	ChatMessageRoleUser      = openaichat.MessageRoleUser
	ChatMessageRoleSystem    = openaichat.MessageRoleSystem
	ChatMessageRoleTool      = openaichat.MessageRoleTool
	ChatMessageRoleDeveloper = openaichat.MessageRoleDeveloper
)

// ChatContentBlockType is the type of a chat content block.
type ChatContentBlockType = openaichat.ContentBlockType

// ChatContentBlockType values.
const (
	ChatContentBlockTypeText  = openaichat.ContentBlockTypeText
	ChatContentBlockTypeImage = openaichat.ContentBlockTypeImage
)

// ChatInputImage is the image_url payload of an image content block.
type ChatInputImage = openaichat.InputImage

// ChatContentBlock is one element of a content block array.
type ChatContentBlock = openaichat.ContentBlock

// ChatMessageContent is the content of a chat message: either a plain string
// or an array of content blocks. Exactly one form is set.
type ChatMessageContent = openaichat.MessageContent

// ChatToolType is the type of a chat tool.
type ChatToolType = openaichat.ToolType

// ChatToolType values.
const (
	ChatToolTypeFunction = openaichat.ToolTypeFunction
)

// ChatToolFunction is the function definition of a function tool.
type ChatToolFunction = openaichat.ToolFunction

// ChatTool is a tool definition in a chat completion request.
type ChatTool = openaichat.Tool

// ChatToolChoiceFunction names a specific function for a tool choice.
type ChatToolChoiceFunction = openaichat.ToolChoiceFunction

// ChatToolChoiceStruct is the object form of a tool choice.
type ChatToolChoiceStruct = openaichat.ToolChoiceStruct

// ChatToolChoice is a union: either a plain string ("none", "auto",
// "required") or an object naming a specific tool.
type ChatToolChoice = openaichat.ToolChoice

// ChatToolMessage carries the tool_call_id of a tool-role message.
type ChatToolMessage = openaichat.ChatToolMessage

// ChatToolCallFunction is the function payload of a tool call.
type ChatToolCallFunction = openaichat.ToolCallFunction

// ChatMessageToolCall is a non-stream tool call on an assistant message. The
// official request/response shape carries id, function, and type and NO
// index. Type is required on the wire and always emitted ("function").
type ChatMessageToolCall = openaichat.MessageToolCall

// ChatToolCallDelta is a streaming tool-call fragment.
type ChatToolCallDelta = openaichat.ToolCallDelta

// ChatAssistantMessage carries the assistant-only fields of a chat message.
type ChatAssistantMessage = openaichat.ChatAssistantMessage

// ChatMessage is a message in a chat conversation.
type ChatMessage = openaichat.Message

// ChatResponseFormatType is the response_format type tag.
type ChatResponseFormatType = openaichat.ResponseFormatType

// ChatResponseFormatType values.
const (
	ChatResponseFormatText       = openaichat.ResponseFormatText
	ChatResponseFormatJSONObject = openaichat.ResponseFormatJSONObject
	ChatResponseFormatJSONSchema = openaichat.ResponseFormatJSONSchema
)

// ChatJSONSchemaFormat is the json_schema arm payload of response_format.
type ChatJSONSchemaFormat = openaichat.JSONSchemaFormat

// ChatResponseFormat is the response_format union of the official Chat
// contract.
type ChatResponseFormat = openaichat.ResponseFormat

// ChatStreamOptions configures chat completion streaming.
type ChatStreamOptions = openaichat.StreamOptions

// ChatStop is the stop union: a single string or an array of up to four
// sequences.
type ChatStop = openaichat.Stop

// ChatRequest is a chat completions request.
type ChatRequest = openaichat.Request

// ChatPromptTokensDetails breaks down prompt tokens.
type ChatPromptTokensDetails = openaichat.PromptTokensDetails

// ChatCompletionTokensDetails breaks down completion tokens.
type ChatCompletionTokensDetails = openaichat.CompletionTokensDetails

// ChatLLMUsage is the token usage of a chat completion.
type ChatLLMUsage = openaichat.LLMUsage

// ChatStreamDelta is the delta payload of a streaming chat completion chunk.
type ChatStreamDelta = openaichat.StreamDelta

// ChatTopLogprob is one alternative token of a token log-probability entry.
type ChatTopLogprob = openaichat.TopLogprob

// ChatTokenLogprob is one token log-probability entry of a chat response.
type ChatTokenLogprob = openaichat.TokenLogprob

// ChatChoiceLogprobs is the logprobs payload of a chat choice.
type ChatChoiceLogprobs = openaichat.ChoiceLogprobs

// ChatChoice is one choice of a chat completion response.
type ChatChoice = openaichat.Choice

// ChatResponse is a chat completions response.
type ChatResponse = openaichat.Response

// ChatStreamResponse is one SSE frame of a streaming chat completion.
type ChatStreamResponse = openaichat.StreamChunk
