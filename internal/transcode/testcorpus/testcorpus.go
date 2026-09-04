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

// Field-capture fixtures (testdata/field/): wire bodies for the four
// provider-extension field regressions (usage top-level extensions, task 15;
// reasoning_content, task 18; matched_stop, task 27; empty-status codex
// multi-turn history, task 30). The extension spellings and their
// null-vs-value placement are byte-faithful to the committed reproduce-first
// tests of those regressions; the envelope ids, model, content narrative,
// and usage composition are reconstructed (no raw capture was retained).
// Unlike the synthetic fixtures above — which are modeled on the official
// contracts and therefore encode the same assumptions as the decoders —
// these carry the real providers' extension spellings and shapes. They are
// replayed through the production decode functions by the field-capture
// regression tests so the next unmodeled provider extension fails `go test`
// with the exact field name, instead of a user session.
//
// Re-capture tooling: `make field-recapture` (credentials-gated, excluded
// from the default CI graph, and documented to never run against another
// user's proxy instance). The committed bytes are sanitized of secrets.
var (
	//go:embed testdata/field/qwen_stream_field.sse
	qwenStreamFieldSSE []byte
	//go:embed testdata/field/qwen_nonstream_field.json
	qwenNonstreamFieldJSON []byte
	//go:embed testdata/field/qwen_reasoning_stream_field.sse
	qwenReasoningStreamFieldSSE []byte
	//go:embed testdata/field/codex_multiturn_request_field.json
	codexMultiturnRequestFieldJSON []byte
)

// FieldQwenStreamSSE returns the qwen-style chat stream capture: choice-level
// matched_stop and stop_reason on every choice-bearing chunk (null
// mid-stream; matched_stop carries the terminal token string on the finish
// chunk; both absent from the choices:[] usage tail) and the top-level
// usage-extension spellings on the final chunks.
func FieldQwenStreamSSE() []byte { return qwenStreamFieldSSE }

// FieldQwenNonstreamJSON returns the qwen-style non-streaming completion
// capture: message-level reasoning_content and stop_reason, choice-level
// matched_stop, the envelope-level prompt_token_ids/prompt_text and
// choice-level token_ids/routed_experts opaque extensions, and the
// prompt_tokens_details usage spelling carrying created_cache_tokens.
func FieldQwenNonstreamJSON() []byte { return qwenNonstreamFieldJSON }

// FieldQwenReasoningStreamSSE returns the DeepSeek/Qwen reasoning_content
// stream capture: delta.reasoning_content chunks preceding the content, with
// the choice-level matched_stop/stop_reason null-vs-value placement.
func FieldQwenReasoningStreamSSE() []byte { return qwenReasoningStreamFieldSSE }

// FieldCodexMultiturnRequestJSON returns the codex resume request capture:
// a previous-output history item carrying "status": "" (the task-30 field
// regression) between two user turns.
func FieldCodexMultiturnRequestJSON() []byte { return codexMultiturnRequestFieldJSON }

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
