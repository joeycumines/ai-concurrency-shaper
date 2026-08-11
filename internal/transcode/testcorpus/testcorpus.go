// Package testcorpus provides self-contained wire fixtures for the three API
// schemas supported by the transcode package. All fixtures are embedded at
// compile time — no live external connections are required.
//
// The fixture payloads are raw JSON/SSE wire data modeled on the official
// contracts (OpenAI Responses, OpenAI Chat Completions, Anthropic Messages)
// and are used by tests to exercise request/response conversion and stream
// translation. They are independent of the package's Go types so the wire
// shapes stay authoritative.
package testcorpus

import (
	"bytes"
	_ "embed"
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

// ChatCompletionsStreamSSE returns the raw streaming chat completions SSE
// fixture bytes.
func ChatCompletionsStreamSSE() []byte { return chatCompletionsStreamSSE }

// ResponsesRequestJSON returns the raw responses request fixture bytes.
func ResponsesRequestJSON() []byte { return responsesRequestJSON }

// ResponsesResponseJSON returns the raw responses response fixture bytes.
func ResponsesResponseJSON() []byte { return responsesResponseJSON }

// ResponsesStreamSSE returns the raw streaming responses SSE fixture bytes.
func ResponsesStreamSSE() []byte { return responsesStreamSSE }

// AnthropicMessagesRequestJSON returns the raw anthropic messages request
// fixture bytes.
func AnthropicMessagesRequestJSON() []byte { return anthropicMessagesRequestJSON }

// AnthropicMessagesResponseJSON returns the raw anthropic messages response
// fixture bytes.
func AnthropicMessagesResponseJSON() []byte { return anthropicMessagesResponseJSON }

// AnthropicMessagesStreamSSE returns the raw streaming anthropic messages SSE
// fixture bytes.
func AnthropicMessagesStreamSSE() []byte { return anthropicMessagesStreamSSE }

// ParseSSEFrames splits raw SSE text into its data payloads, one per event,
// skipping blank lines and event: lines. It is used by tests to validate
// fixture streams. Malformed events are skipped rather than returned as
// errors, mirroring the tolerant behavior of the SSE frame parser.
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
