// Package testcorpus provides self-contained test fixtures for the three API
// schemas supported by the transcode package. All fixtures are embedded at
// compile time — no live external connections are required.
//
// The fixture payloads are modeled on the wire shapes of the Bifrost reference
// implementation (github.com/joeycumines/bifrost), exercising the shapes
// documented in its schema packages: developer-role messages, tool calls and
// tool results, reasoning blocks, token usage, and representative SSE stream
// sequences for each streaming API.
package testcorpus

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

//go:embed testdata/chat_completions_request.json
var chatCompletionsRequestJSON []byte

//go:embed testdata/chat_completions_response.json
var chatCompletionsResponseJSON []byte

//go:embed testdata/chat_completions_stream.sse
var chatCompletionsStreamSSE []byte

//go:embed testdata/responses_request.json
var responsesRequestJSON []byte

//go:embed testdata/responses_response.json
var responsesResponseJSON []byte

//go:embed testdata/responses_stream.sse
var responsesStreamSSE []byte

//go:embed testdata/anthropic_messages_request.json
var anthropicMessagesRequestJSON []byte

//go:embed testdata/anthropic_messages_response.json
var anthropicMessagesResponseJSON []byte

//go:embed testdata/anthropic_messages_stream.sse
var anthropicMessagesStreamSSE []byte

// ChatCompletionsRequestJSON returns the raw chat completions request fixture
// bytes.
func ChatCompletionsRequestJSON() []byte { return chatCompletionsRequestJSON }

// ChatCompletionsResponseJSON returns the raw chat completions response
// fixture bytes.
func ChatCompletionsResponseJSON() []byte { return chatCompletionsResponseJSON }

// ResponsesRequestJSON returns the raw responses request fixture bytes.
func ResponsesRequestJSON() []byte { return responsesRequestJSON }

// ResponsesResponseJSON returns the raw responses response fixture bytes.
func ResponsesResponseJSON() []byte { return responsesResponseJSON }

// AnthropicMessagesRequestJSON returns the raw anthropic messages request
// fixture bytes.
func AnthropicMessagesRequestJSON() []byte { return anthropicMessagesRequestJSON }

// AnthropicMessagesResponseJSON returns the raw anthropic messages response
// fixture bytes.
func AnthropicMessagesResponseJSON() []byte { return anthropicMessagesResponseJSON }

// ChatCompletionsFixtureSet is a request/response pair and an SSE stream
// for the OpenAI Chat Completions API, exercising a developer-role message, an
// assistant tool call with a tool-role result, reasoning blocks, and usage.
type ChatCompletionsFixtureSet struct {
	// Request is a chat completions request with a developer message, a
	// function tool, an assistant tool call, and a tool result.
	Request transcode.ChatRequest
	// Response is a chat completions response with a reasoning block and
	// token usage details.
	Response transcode.ChatResponse
	// Stream is the raw SSE text of a streaming chat completion: a role and
	// reasoning start chunk, reasoning and content deltas, a terminal chunk
	// carrying usage, and the [DONE] sentinel.
	Stream string
}

// ResponsesFixtureSet is a request/response pair and an SSE stream for the
// OpenAI Responses API, exercising reasoning items, function calls and their
// outputs, message items, and usage.
type ResponsesFixtureSet struct {
	// Request is a responses request with instructions, a function tool, and
	// reasoning configuration.
	Request transcode.ResponsesRequest
	// Response is a responses response with reasoning, function call, tool
	// output, and message output items.
	Response transcode.ResponsesResponse
	// Stream is the raw SSE text of a streaming responses session:
	// response.created, response.in_progress, output item and content part
	// lifecycle events, output_text deltas, reasoning summary deltas, and
	// response.completed with final usage.
	Stream string
}

// AnthropicMessagesFixtureSet is a request/response pair and an SSE stream for
// the Anthropic Messages API, exercising a system prompt, a tool definition,
// thinking blocks, text content, and usage.
type AnthropicMessagesFixtureSet struct {
	// Request is an Anthropic messages request with a system prompt, a tool
	// definition, and a user message.
	Request transcode.AnthropicMessageRequest
	// Response is an Anthropic messages response with a text content block
	// and token usage.
	Response transcode.AnthropicMessageResponse
	// Stream is the raw SSE text of a streaming Anthropic session:
	// message_start, thinking block start/deltas (thinking_delta and
	// signature_delta)/stop, text block start/deltas/stop, message_delta,
	// and message_stop.
	Stream string
}

// OpenAIChatCompletionsFixtures returns the chat completions fixture set.
func OpenAIChatCompletionsFixtures() (ChatCompletionsFixtureSet, error) {
	var f ChatCompletionsFixtureSet
	if err := json.Unmarshal(chatCompletionsRequestJSON, &f.Request); err != nil {
		return f, fmt.Errorf("unmarshal chat completions request fixture: %w", err)
	}
	if err := json.Unmarshal(chatCompletionsResponseJSON, &f.Response); err != nil {
		return f, fmt.Errorf("unmarshal chat completions response fixture: %w", err)
	}
	f.Stream = string(chatCompletionsStreamSSE)
	return f, nil
}

// OpenAIResponsesFixtures returns the responses API fixture set.
func OpenAIResponsesFixtures() (ResponsesFixtureSet, error) {
	var f ResponsesFixtureSet
	if err := json.Unmarshal(responsesRequestJSON, &f.Request); err != nil {
		return f, fmt.Errorf("unmarshal responses request fixture: %w", err)
	}
	if err := json.Unmarshal(responsesResponseJSON, &f.Response); err != nil {
		return f, fmt.Errorf("unmarshal responses response fixture: %w", err)
	}
	f.Stream = string(responsesStreamSSE)
	return f, nil
}

// AnthropicMessagesFixtures returns the Anthropic messages fixture set.
func AnthropicMessagesFixtures() (AnthropicMessagesFixtureSet, error) {
	var f AnthropicMessagesFixtureSet
	if err := json.Unmarshal(anthropicMessagesRequestJSON, &f.Request); err != nil {
		return f, fmt.Errorf("unmarshal anthropic messages request fixture: %w", err)
	}
	if err := json.Unmarshal(anthropicMessagesResponseJSON, &f.Response); err != nil {
		return f, fmt.Errorf("unmarshal anthropic messages response fixture: %w", err)
	}
	f.Stream = string(anthropicMessagesStreamSSE)
	return f, nil
}

// ParseSSEFrames splits raw SSE text into its data payloads, one per event,
// skipping blank lines and event: lines. It is used by tests to validate
// fixture streams and by the streaming handlers to decode upstream frames.
// Malformed events are skipped rather than returned as errors, mirroring the
// tolerant behavior required of the transcode handler.
func ParseSSEFrames(data []byte) []string {
	var frames []string
	var current []byte
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r")
		switch {
		case len(trimmed) == 0:
			if len(current) > 0 {
				frames = append(frames, string(current))
				current = nil
			}
		case bytes.HasPrefix(trimmed, []byte("data:")):
			payload := trimmed[len("data:"):]
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			if len(current) > 0 {
				current = append(current, '\n')
			}
			current = append(current, payload...)
		default:
			// event:, id:, retry:, comments, and malformed lines are ignored.
		}
	}
	if len(current) > 0 {
		frames = append(frames, string(current))
	}
	return frames
}
