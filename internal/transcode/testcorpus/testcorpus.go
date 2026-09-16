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
// provider-extension field regressions (usage top-level extensions,
// reasoning_content, matched_stop, empty-status codex
// multi-turn history). The extension spellings and their
// null-vs-value placement are byte-faithful to the committed reproduce-first
// tests of those regressions; the envelope ids, model, content narrative,
// and usage composition are reconstructed (no raw capture was retained).
// Unlike the synthetic fixtures above — which are modeled on the official
// contracts and therefore encode the same assumptions as the decoders —
// these carry the real providers' extension spellings and shapes. They are
// replayed through the production decode functions by the field-capture
// regression tests: the production tolerant upstream decode must accept each
// modeled extension and never leak it to the rendered client output.
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
	//go:embed testdata/field/codex_namespace_request_field.json
	codexNamespaceRequestFieldJSON []byte
	//go:embed testdata/field/repeated_terminal_tail_field.sse
	repeatedTerminalTailFieldSSE []byte
	//go:embed testdata/field/data_only_responses_stream_field.sse
	dataOnlyResponsesStreamFieldSSE []byte
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

// FieldCodexNamespaceRequestJSON returns the captured Codex CLI (0.154.0)
// Responses request carrying namespace tools (multi_agent_v1 and an MCP
// namespace) alongside ordinary function tools and a web_search built-in.
// The tools, their child schemas, and tool_choice are byte-verbatim from the
// capture; the envelope is trimmed to the fields the replay test needs.
func FieldCodexNamespaceRequestJSON() []byte { return codexNamespaceRequestFieldJSON }

// FieldRepeatedTerminalTailSSE returns the captured dialagram
// meta-muse-spark-1.3 stream tail in which the gateway redelivers the
// terminal chunk — the same single choice, the same finish reason, an
// insubstantial delta — with the usage accounting piggybacked, followed by
// the [DONE] sentinel. The frames are shape-verbatim from the capture; the
// tool-call argument payload is replaced with a neutral value.
func FieldRepeatedTerminalTailSSE() []byte { return repeatedTerminalTailFieldSSE }

// FieldDataOnlyResponsesStreamSSE returns the captured camel native-Responses
// stream in which the gateway omits the SSE event: name on every frame: a
// leading ": " comment frame then data-only frames whose JSON `type` is the
// authoritative discriminator (the payloads also carry the gateway's opaque
// "p" envelope extension and the pinned envelope controls, e.g.
// output[].phase and table/prompt-cache nulls at created time). Sanitized:
// the account id, generation id, and per-stream "p" token are replaced with
// stable placeholders; the shape (key presence, null-vs-value, frame order)
// is byte-faithful to the capture.
func FieldDataOnlyResponsesStreamSSE() []byte { return dataOnlyResponsesStreamFieldSSE }

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
