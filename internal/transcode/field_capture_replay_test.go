package transcode

// Field-capture regression harness (2026-08-25): replays the
// committed field-capture corpus (testcorpus/testdata/field/ — raw wire
// bytes carrying the real providers' extension spellings, reconstructed
// byte-faithfully from the committed reproduce-first tests of the four
// provider-extension regressions) through the EXACT production decode
// functions — chatStreamChunkFromSSE for stream frames,
// DecodeChatResponseWithPolicy for non-streaming bodies,
// DecodeResponsesRequest for client requests. No parallel test-only
// decoder exists here: that would recreate the very synthetic-blindness
// defect this harness exists to fix (four provider-extension regressions —
// usage top-level extensions, reasoning_content, matched_stop, empty-status
// history — all passed the synthetic suite while failing live sessions).
//
// The regression proof: the fixture carries real provider-extension spellings
// (cache_cost, completion_cost, reasoning_content, matched_stop, usage
// extensions) and the production decode accepts them without leaking them to
// the rendered client output.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// streamReplay is the decoded record of one replayed SSE capture.
type streamReplay struct {
	// content accumulates every decoded content delta, in stream order.
	content strings.Builder
	// reasoning accumulates every decoded reasoning_content delta.
	reasoning strings.Builder
	// sawFinishReason reports that a choice-bearing chunk carried a
	// non-null finish_reason — the terminal condition production requires
	// before accepting the [DONE] sentinel (chatToResponsesConverter:
	// errChatDoneBeforeTerminal).
	sawFinishReason bool
	// sawDone reports the [DONE] sentinel was reached; the replay stops
	// there, mirroring the production reader's terminal stop.
	sawDone bool
}

// replayChatStreamFrames runs every data frame of an SSE capture through the
// production chunk decoder, failing with the offending frame on any decode
// error. [DONE] is the expected terminal sentinel; the replay stops at it,
// as the production reader does.
func replayChatStreamFrames(t *testing.T, name string, capture []byte) streamReplay {
	t.Helper()
	frames := testcorpus.ParseSSEFrames(capture)
	if len(frames) < 3 {
		t.Fatalf("%s: only %d frames parsed", name, len(frames))
	}
	var replay streamReplay
	for i, frame := range frames {
		if replay.sawDone {
			t.Fatalf("%s: frame %d follows the [DONE] sentinel", name, i)
		}
		chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(frame)})
		if err != nil {
			// The [DONE] sentinel is the stream's expected terminal: stop
			// here like the production reader, rather than feeding
			// post-terminal bytes to the decoder.
			if errors.Is(err, errChatStreamDone) {
				replay.sawDone = true
				continue
			}
			t.Fatalf("%s: frame %d failed production decode: %v\nframe: %s", name, i, err, frame)
		}
		for j := range chunk.Choices {
			choice := &chunk.Choices[j]
			if choice.FinishReason != nil {
				replay.sawFinishReason = true
			}
			if choice.Delta != nil {
				if choice.Delta.Content != nil {
					replay.content.WriteString(*choice.Delta.Content)
				}
				if choice.Delta.ReasoningContent != nil {
					replay.reasoning.WriteString(*choice.Delta.ReasoningContent)
				}
			}
		}
	}
	if !replay.sawDone {
		t.Fatalf("%s: stream ended without the [DONE] sentinel", name)
	}
	if !replay.sawFinishReason {
		t.Fatalf(
			"%s: no choice-bearing chunk carried a finish_reason; the "+
				"production converter would reject this stream with "+
				"errChatDoneBeforeTerminal at [DONE]",
			name,
		)
	}
	return replay
}

// TestFieldCaptureQwenStreamDecodes pins the matched_stop stream regression:
// every frame of the qwen capture — including the choice-level matched_stop
// (null mid-stream, the terminal token on the finish chunk) and the
// usage-extension spellings — decodes through the strict production shadow.
func TestFieldCaptureQwenStreamDecodes(t *testing.T) {
	replay := replayChatStreamFrames(t, "qwen_stream_field", testcorpus.FieldQwenStreamSSE())
	if got := replay.content.String(); got != "The remembered number is 8463." {
		t.Fatalf("decoded stream content = %q, want %q", got, "The remembered number is 8463.")
	}
	if replay.reasoning.Len() != 0 {
		t.Fatalf("plain capture decoded reasoning_content = %q, want none", replay.reasoning.String())
	}
}

// TestFieldCaptureQwenReasoningStreamDecodes pins the reasoning_content
// stream regression on the same production surface.
func TestFieldCaptureQwenReasoningStreamDecodes(t *testing.T) {
	replay := replayChatStreamFrames(t, "qwen_reasoning_stream_field", testcorpus.FieldQwenReasoningStreamSSE())
	if got := replay.content.String(); got != "The number is 8463." {
		t.Fatalf("decoded stream content = %q, want %q", got, "The number is 8463.")
	}
	if !strings.Contains(replay.reasoning.String(), "I recall the number") {
		t.Fatalf("reasoning_content missing from decoded deltas: %q", replay.reasoning.String())
	}
}

// TestFieldCaptureQwenNonstreamDecodes pins the non-stream regressions
// together: message-level reasoning_content (capability-gated), choice-level
// matched_stop, and the usage extensions all decode through
// DecodeChatResponseWithPolicy. The reasoning mapping follows the
// ProviderReasoningText capability; the extension bytes never reach the
// rendered client output.
func TestFieldCaptureQwenNonstreamDecodes(t *testing.T) {
	body := testcorpus.FieldQwenNonstreamJSON()

	response, _, err := DecodeChatResponseWithPolicy(
		body,
		ChatCapabilities{ProviderReasoningText: true},
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("field capture rejected by production decode: %v", err)
	}
	if len(response.Items) == 0 {
		t.Fatal("decoded response has no items")
	}

	// The reasoning text maps to ordinary text under the capability.
	joined := responseSummary(response)
	if !strings.Contains(joined, "let me think") {
		t.Fatalf("reasoning_content text missing from decoded parts: %q", joined)
	}
	if !strings.Contains(joined, "The remembered number is 8463.") {
		t.Fatalf("content missing from decoded parts: %q", joined)
	}

	// The usage extensions map to their canonical homes: the
	// prompt_tokens_details spelling (details precede the top-level
	// extensions, so cached_tokens→cache-read and created_cache_tokens→
	// cache-write) and the top-level reasoning_tokens.
	if !response.Usage.InputKnown || response.Usage.InputTokens != 20 {
		t.Fatalf("usage input tokens = (%d, known=%t), want (20, true)", response.Usage.InputTokens, response.Usage.InputKnown)
	}
	if !response.Usage.OutputKnown || response.Usage.OutputTokens != 12 {
		t.Fatalf("usage output tokens = (%d, known=%t), want (12, true)", response.Usage.OutputTokens, response.Usage.OutputKnown)
	}
	if !response.Usage.ReasoningKnown || response.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage reasoning tokens = (%d, known=%t), want (3, true)", response.Usage.ReasoningTokens, response.Usage.ReasoningKnown)
	}
	if !response.Usage.CacheReadKnown || response.Usage.CacheReadTokens != 5 {
		t.Fatalf("usage cache-read tokens = (%d, known=%t), want (5, true) — prompt_tokens_details.cached_tokens must map to cache-read", response.Usage.CacheReadTokens, response.Usage.CacheReadKnown)
	}
	if !response.Usage.CacheWriteKnown || response.Usage.CacheWriteTokens != 7 {
		t.Fatalf("usage cache-write tokens = (%d, known=%t), want (7, true) — created_cache_tokens must map to cache-write", response.Usage.CacheWriteTokens, response.Usage.CacheWriteKnown)
	}

	// The opaque cache_cost and completion_cost provider extensions (Verboo /
	// LiteLLM billing fields) must never reach the client dialect: they are
	// decoded so the tolerant upstream-response decode accepts them (the
	// regression), but the canonical IR has no model for them, so any
	// rendered client output is clean of them.
	rendered, _, err := RenderMessagesResponse(response, testExchangeContext())
	if err != nil {
		t.Fatalf("render client dialect: %v", err)
	}
	if bytes.Contains(rendered, []byte("cache_cost")) {
		t.Fatalf("cache_cost leaked into client output: %s", rendered)
	}
	if bytes.Contains(rendered, []byte("completion_cost")) {
		t.Fatalf("completion_cost leaked into client output: %s", rendered)
	}
}

// TestFieldCaptureCodexMultiturnRequestDecodes pins the empty-status
// regression on the request surface: the codex resume history item with
// "status": "" decodes through DecodeResponsesRequest without the
// 'invalid previous output status ""' rejection that killed every codex
// multi-turn session.
func TestFieldCaptureCodexMultiturnRequestDecodes(t *testing.T) {
	result, echo, err := DecodeResponsesRequest(testcorpus.FieldCodexMultiturnRequestJSON(), StrictLossPolicy())
	if err != nil {
		t.Fatalf("field capture rejected by production decode: %v", err)
	}
	if len(result.Request.Turns) != 3 {
		t.Fatalf("turns = %d, want 3 (user, assistant history, user)", len(result.Request.Turns))
	}
	if got := result.Request.Turns[1].Role; got != CanonicalAssistant {
		t.Fatalf("history turn role = %q, want %q", got, CanonicalAssistant)
	}
	// The assistant history content must actually decode: an output_text
	// part silently dropped would still yield the right turn count.
	if got := turnSummary(result.Request.Turns[1]); !strings.Contains(got, "Got it — 8463.") {
		t.Fatalf("assistant history content missing from decoded turn: %q", got)
	}
	if echo == nil {
		t.Fatal("request echo missing")
	}
}

// TestFieldCaptureDataOnlyResponsesStreamReplays pins the data-only
// Responses stream regression: the captured camel stream omits the SSE
// event: name on every frame (a leading ": " comment frame then data-only
// frames whose JSON `type` is authoritative, plus the gateway's opaque "p"
// envelope extension, output[].phase, and the created-time null controls).
// Replayed through the EXACT production converter it must complete the full
// Anthropic lifecycle: exactly one message_stop, no error event, the content
// delivered, and the ungated missing_event_name note recorded once.
func TestFieldCaptureDataOnlyResponsesStreamReplays(t *testing.T) {
	// The captured gateway envelope carries the pinned controls (a real
	// safety_identifier, output[].phase, the created-time null fields), so
	// the replay approves the controls loss and the usage components the
	// Messages contract requires in addition to the note's own key.
	policy := j6PermissivePolicy()
	policy.Allowed[FeatureResponsesControls] = struct{}{}
	policy.Allowed[FeatureOutputPhase] = struct{}{}
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		policy,
		ChatCapabilities{},
		"msg_1",
		"gpt-4.1",
		1,
	)
	converter := newResponsesToAnthropicConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(bytes.NewReader(testcorpus.FieldDataOnlyResponsesStreamSSE()), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !reader.SawTerminal() {
		t.Fatal("no terminal on the data-only captured stream")
	}
	if reader.SawErrorEvent() {
		t.Fatal("data-only captured stream reported an error event")
	}
	body := output.String()
	if got := strings.Count(body, "event: message_stop"); got != 1 {
		t.Fatalf("message_stop count = %d, want exactly one: %q", got, body)
	}
	if !strings.Contains(body, "event: message_start") {
		t.Fatalf("missing message_start: %q", body)
	}
	notes := 0
	for _, loss := range state.report.Losses {
		if loss.Feature == FeatureMissingEventName {
			notes++
			if loss.Kind != NoteRecord {
				t.Fatalf("missing_event_name kind = %v, want NoteRecord", loss.Kind)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("missing_event_name note count = %d, want exactly one", notes)
	}
}

// responseSummary renders every canonical part of the response as one string
// for substring assertions (test-only; the production renderers remain the
// authoritative emission path and are exercised by the replay tests).
func responseSummary(response CanonicalResponse) string {
	var b strings.Builder
	for _, item := range response.Items {
		if message, ok := item.(*CanonicalMessageItem); ok {
			for _, part := range message.Parts {
				switch p := part.(type) {
				case CanonicalText:
					b.WriteString(p.Text)
					b.WriteString("\n")
				case CanonicalRefusal:
					b.WriteString(p.Text)
					b.WriteString("\n")
				}
			}
		}
	}
	return b.String()
}

// turnSummary renders every canonical part of one turn as one string
// (test-only helper mirroring responseSummary).
func turnSummary(turn CanonicalTurn) string {
	var b strings.Builder
	for _, part := range turn.Parts {
		switch p := part.(type) {
		case CanonicalText:
			b.WriteString(p.Text)
			b.WriteString("\n")
		case CanonicalRefusal:
			b.WriteString(p.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestFieldCaptureCamelReasoningFormatDecodes replays the captured camel
// native-Responses body whose reasoning output item carries the gateway's
// opaque "format" routing marker (observed live 2026-09-17): the production tolerant
// upstream decode must accept the modeled extension and never leak it to
// the canonical bytes any client dialect renders.
func TestFieldCaptureCamelReasoningFormatDecodes(t *testing.T) {
	body := testcorpus.FieldCamelReasoningFormatJSON()
	response, err := DecodeResponsesResponse(body)
	if err != nil {
		t.Fatalf("field capture rejected by production decode: %v", err)
	}
	if len(response.Items) != 2 {
		t.Fatalf("items = %d, want 2 (reasoning + message)", len(response.Items))
	}
	raw, ok := response.Items[0].(*CanonicalReasoningItem)
	if !ok {
		t.Fatalf("item 0 = %T, want a reasoning item", response.Items[0])
	}
	// Key-absence, not value-absence: any format key leaks regardless of
	// the marker spelling (or a null from a tag change). The render-side
	// echo re-emits Raw verbatim, so pinning the canonical bytes pins the
	// downstream wire for every client dialect.
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(raw.Raw, &rawMap); err != nil {
		t.Fatalf("canonical reasoning bytes do not decode: %v", err)
	}
	if _, leaked := rawMap["format"]; leaked {
		t.Fatalf("provider marker key leaked into canonical bytes: %s", raw.Raw)
	}
	// The message half still decodes: the marker did not disturb the item.
	if _, ok := response.Items[1].(*CanonicalMessageItem); !ok {
		t.Fatalf("item 1 = %T, want a message item", response.Items[1])
	}
	if got := responseSummary(response); !strings.Contains(got, "PONG") {
		t.Fatalf("message text missing from decoded parts: %q", got)
	}
}

// TestFieldCaptureClaudeServerToolDefinitionDecodes replays the captured
// Claude Code web-search request: the type-discriminated server definition
// must decode (admit) and drop under the anthropic_server_tools key — never
// an unattributed unknown-field rejection.
func TestFieldCaptureClaudeServerToolDefinitionDecodes(t *testing.T) {
	body := testcorpus.FieldClaudeServerToolDefinitionJSON()
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	target := &UnsupportedFeatureError{}
	if !errors.As(err, &target) {
		t.Fatalf("err = %T: %v, want keyed rejection", err, err)
	}
	if target.Feature != string(FeatureAnthropicServerTools) {
		t.Fatalf("feature = %q, want %q", target.Feature, FeatureAnthropicServerTools)
	}
	permissive := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicServerTools: {}}}
	result, err := DecodeMessagesRequest(body, permissive)
	if err != nil {
		t.Fatalf("approved drop rejected: %v", err)
	}
	if len(result.Request.Tools) != 0 {
		t.Fatalf("server tool leaked %d tools upstream", len(result.Request.Tools))
	}
}
