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
	"net/http"
	"net/http/httptest"
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
		StrictLossPolicy(), nil)
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
	policy := permissiveLossPolicy()
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

// TestFieldCaptureClaudeThinkingDisplayUpdatesDecodes replays the exchange the
// proxy refused with `messages thinking display "updates" is invalid`: a
// claude-cli 2.1.328 request carrying manual extended thinking with the
// beta-gated "updates" display (selected with the Anthropic-Beta member
// thinking-display-updates-2026-08-18). The contract documents display on the
// enabled arm as well as adaptive, and lists updates as a member, so the
// production decode must accept it under the STRICT policy — the request
// triggers no loss at all — and the Messages target must reproduce it.
func TestFieldCaptureClaudeThinkingDisplayUpdatesDecodes(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.FieldClaudeThinkingDisplayUpdatesJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("the reported client request was refused: %v", err)
	}
	thinking := result.Request.Thinking
	if thinking == nil || thinking.Type != "enabled" {
		t.Fatalf("thinking = %+v, want the enabled arm", thinking)
	}
	if thinking.Display != "updates" {
		t.Fatalf("thinking display = %q, want the beta member carried rather than destroyed", thinking.Display)
	}
	if thinking.BudgetTokens == nil || *thinking.BudgetTokens != 31999 {
		t.Fatalf("budget_tokens = %v, want 31999", thinking.BudgetTokens)
	}
	if len(result.Report.Losses) != 0 {
		t.Fatalf("the request carries no loss, got %+v", result.Report.Losses)
	}

	// A Messages target carries the whole union natively, so the upstream sees
	// exactly what the client asked for.
	rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	for _, want := range []string{
		`"type":"enabled"`,
		`"budget_tokens":31999`,
		`"display":"updates"`,
	} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("rendered thinking is missing %s: %s", want, rendered)
		}
	}

	// Every other target loses the display preference, and losing it is a
	// policy decision the operator can refuse — never a silent drop.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning: {},
	}}
	// The captures carry tools, and a Messages-sourced tool on a Responses
	// target is its own documented refusal (tool_schema_strictness). Drop them
	// so each sub-test decides the thinking arm and nothing else.
	withoutTools := result.Request
	withoutTools.Tools = nil
	withoutTools.ToolChoice = nil
	for name, render := range map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error){
		"chat": func(r CanonicalRequest, c *ExchangeContext) ([]byte, ConversionReport, error) {
			return RenderChatRequest(r, c, ChatCapabilities{})
		},
		"responses": RenderResponsesRequest,
		"gemini":    RenderGeminiRequest,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := render(withoutTools, testExchangeContext()); err == nil {
				t.Fatal("strict policy accepted losing the display preference; want a keyed refusal")
			} else if !strings.Contains(err.Error(), string(FeatureRequestReasoning)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureRequestReasoning)
			}
			_, report, err := render(withoutTools, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			if !reportHasAt(report, FeatureRequestReasoning, "thinking.display") {
				t.Fatalf("report = %+v, want the display decision at thinking.display", report.Losses)
			}
		})
	}
}

// TestFieldCaptureClaudeThinkingBetweenToolsDecodes replays Claude Sonnet
// 5.5's lowest thinking setting: type between_tools, which takes no other
// field. The proxy must carry it rather than reject the arm, and no target may
// silently drop what it asserts.
func TestFieldCaptureClaudeThinkingBetweenToolsDecodes(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.FieldClaudeThinkingBetweenToolsJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("the between_tools arm was refused: %v", err)
	}
	if thinking := result.Request.Thinking; thinking == nil ||
		thinking.Type != "between_tools" ||
		thinking.BudgetTokens != nil ||
		thinking.Display != "" {
		t.Fatalf("thinking = %+v, want a bare between_tools arm", result.Request.Thinking)
	}
	if len(result.Report.Losses) != 0 {
		t.Fatalf("the request carries no loss, got %+v", result.Report.Losses)
	}

	rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	if !strings.Contains(string(rendered), `"type":"between_tools"`) {
		t.Fatalf("between_tools is missing from the Messages wire: %s", rendered)
	}

	// The arm asserts no up-front thinking plus progress updates between tool
	// calls. No non-Messages dialect can express either half, so the loss is
	// policy-gated: strict refuses, an approving policy records it.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning: {},
	}}
	// The captures carry tools, and a Messages-sourced tool on a Responses
	// target is its own documented refusal (tool_schema_strictness). Drop them
	// so each sub-test decides the thinking arm and nothing else.
	withoutTools := result.Request
	withoutTools.Tools = nil
	withoutTools.ToolChoice = nil
	for name, render := range map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error){
		"chat": func(r CanonicalRequest, c *ExchangeContext) ([]byte, ConversionReport, error) {
			return RenderChatRequest(r, c, ChatCapabilities{})
		},
		"responses": RenderResponsesRequest,
		"gemini":    RenderGeminiRequest,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := render(withoutTools, testExchangeContext()); err == nil {
				t.Fatal("strict policy accepted losing between_tools; want a keyed refusal")
			} else if !strings.Contains(err.Error(), string(FeatureRequestReasoning)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureRequestReasoning)
			}
			_, report, err := render(withoutTools, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			if !reportHasFeature(report, FeatureRequestReasoning) {
				t.Fatalf("report = %+v, want a request_reasoning entry", report.Losses)
			}
		})
	}
}

// TestHandlerServesClaudeThinkingDisplayUpdates is the end-to-end proof of the
// reported regression: the captured claude-cli request, carrying the beta
// "updates" display, is POSTed to a real TranscodeHandler and must reach the
// upstream instead of being answered locally with the 400 the operator saw.
// The upstream the handler talks to is scripted, so this asserts exactly what
// the proxy SENT rather than merely what a decode returned.
func TestHandlerServesClaudeThinkingDisplayUpdates(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	// The capture carries a thinking budget, tools and system blocks; every
	// loss the conversion legitimately takes here is approved so a refusal can
	// only come from the thinking union itself.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		// The capture's manual extended thinking maps to reasoning_effort and
		// its display preference is a Messages-only field: both are the
		// request_reasoning decisions under test, so approve the key.
		FeatureRequestReasoning:       {},
		FeatureReasoningSummary:       {},
		FeatureToolSchemaStrictness:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	var upstreamBody []byte
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	// The capture is a streaming exchange; this test pins the request path, so
	// the JSON response fixture is paired with a non-streaming request.
	body := bytes.Replace(
		testcorpus.FieldClaudeThinkingDisplayUpdatesJSON(),
		[]byte(`"stream":true,`), nil, 1,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Anthropic-Beta", "thinking-display-updates-2026-08-18")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the reported request was answered %d: %s", rec.Code, rec.Body.String())
	}
	// The chat capture's thinking budget maps to reasoning_effort, which is
	// what this mapping's capabilities enable; the assertion that matters is
	// that the request was CONVERTED, not refused.
	if len(upstreamBody) == 0 {
		t.Fatal("the upstream saw no request")
	}
	if bytes.Contains(upstreamBody, []byte("updates")) {
		t.Fatalf("a Messages-only display value leaked into the chat request: %s", upstreamBody)
	}
}

// TestHandlerServesClaudeThinkingBetweenTools is the same end-to-end proof for
// the between_tools arm: it must reach the upstream as a policy-gated loss
// rather than being refused as an unknown type.
func TestHandlerServesClaudeThinkingBetweenTools(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning:       {},
		FeatureReasoningSummary:       {},
		FeatureToolSchemaStrictness:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	var upstreamBody []byte
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	body := bytes.Replace(
		testcorpus.FieldClaudeThinkingBetweenToolsJSON(),
		[]byte(`"stream":true,`), nil, 1,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the between_tools request was answered %d: %s", rec.Code, rec.Body.String())
	}
	if len(upstreamBody) == 0 {
		t.Fatal("the upstream saw no request")
	}
}

// TestFieldCaptureClaudeToolAdditionDefinitionDecodes replays the request the
// proxy refused with `unknown anthropic content block type "tool_addition"`: a
// mid-conversation role:"system" message mixing text with an inline-tools
// tool_addition carrying a full tool_definition. The tool must become available
// (hoisted into the request's tools) with the availability-timing change
// recorded as exactly one Note — never an unknown-type rejection, and never a
// silent drop.
func TestFieldCaptureClaudeToolAdditionDefinitionDecodes(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.FieldClaudeToolAdditionDefinitionJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("the reported tool_addition request was refused: %v", err)
	}
	var names []string
	for _, tool := range result.Request.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "Read,db_query" {
		t.Fatalf("tools = %v, want the declared Read plus the hoisted db_query", names)
	}
	// The definition survives losslessly: a generic envelope would lose it.
	hoisted := result.Request.Tools[len(result.Request.Tools)-1]
	if hoisted.Description != "Run a read-only SQL query against the analytics database." {
		t.Fatalf("hoisted description = %q", hoisted.Description)
	}
	if !bytes.Contains(hoisted.JSONSchema, []byte(`"sql"`)) {
		t.Fatalf("hoisted schema lost its property: %s", hoisted.JSONSchema)
	}

	// Exactly one Note, under the tool-change key, naming the timing move.
	var notes []ConversionLoss
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicToolChanges {
			notes = append(notes, loss)
		}
		if loss.Kind != NoteRecord {
			t.Fatalf("hoisting a definition is a sanctioned encoding, got %v: %+v", loss.Kind, loss)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("tool-change notes = %d, want exactly one: %+v", len(notes), result.Report.Losses)
	}
	if !strings.Contains(notes[0].Detail, "db_query") {
		t.Fatalf("note does not name the hoisted tool: %q", notes[0].Detail)
	}

	// The fixture's tool_addition carries a block-level cache_control, which the
	// contract allows on the block or in the definition. It is a prompt-cache
	// performance hint with no chat representation, so it takes the existing
	// deduped cache_control note — it must not vanish unrecorded just because
	// the block type is new.
	cacheControlNotes := 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicControls && loss.Path == "cache_control" {
			cacheControlNotes++
		}
	}
	if cacheControlNotes != 1 {
		t.Fatalf("cache_control notes = %d, want exactly one for the block-level hint", cacheControlNotes)
	}

	// The decoder's own output must pass the central validator, and the Chat
	// target must render the hoisted tool. This fixture's system message sits
	// after a user turn (that is what mid-conversation means), so the render
	// needs the position loss approved — the same approval real traffic needs.
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("validator refused the decoder output: %v", err)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem: {},
		FeatureMultipleSystemTurns:   {},
	}}
	rendered, _, err := RenderChatRequest(result.Request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if !bytes.Contains(rendered, []byte(`"db_query"`)) {
		t.Fatalf("the hoisted tool is missing from the chat request: %s", rendered)
	}
}

// TestFieldCaptureClaudeToolRemovalIsPolicyGated replays the withdrawal
// counterpart: a tool_removal naming a declared tool. A chat target carries one
// global tool list, so the timing change cannot be expressed — the strict policy
// must REFUSE it with the registered key and an approving policy must record the
// loss. It is never silently dropped.
func TestFieldCaptureClaudeToolRemovalIsPolicyGated(t *testing.T) {
	body := testcorpus.FieldClaudeToolRemovalReferenceJSON()
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("strict policy accepted a tool_removal; want a keyed refusal")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("err = %T: %v, want UnsupportedFeatureError", err, err)
	}
	if target.Feature != string(FeatureAnthropicToolChanges) {
		t.Fatalf("feature = %q, want %q", target.Feature, FeatureAnthropicToolChanges)
	}

	result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicToolChanges: {},
	}})
	if err != nil {
		t.Fatalf("approved removal refused: %v", err)
	}
	var losses int
	for _, loss := range result.Report.Losses {
		if loss.Feature != FeatureAnthropicToolChanges {
			continue
		}
		losses++
		if loss.Kind != LossRecord {
			t.Fatalf("a withdrawal changes what the model can call, so it is a policy-gated loss: %+v", loss)
		}
	}
	if losses != 1 {
		t.Fatalf("tool-change losses = %d, want exactly one: %+v", losses, result.Report.Losses)
	}
	// The declared tool is still offered for the whole request: that is the
	// sanctioned encoding the loss records.
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "get_weather" {
		t.Fatalf("declared tools = %+v, want get_weather still offered", result.Request.Tools)
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("validator refused the decoder output: %v", err)
	}
}

// TestFieldCaptureClaudeToolChangeStrictnessHolds proves the widened union did
// not become blanket tolerance: an UNDOCUMENTED block type is still a typed
// rejection, and the tool-change arm still rejects a member from another arm, a
// nameless reference, and a tool_definition on a removal.
func TestFieldCaptureClaudeToolChangeStrictnessHolds(t *testing.T) {
	envelope := func(block string) []byte {
		return []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"system","content":[` + block + `]}]}`)
	}
	cases := map[string]string{
		"undocumented block type": `{"type":"quantum_compute"}`,
		"unknown arm":             `{"type":"tool_addition","tool":{"type":"teleport","name":"x"}}`,
		"cross-arm member":        `{"type":"tool_addition","tool":{"type":"tool_reference","name":"x","server_name":"s"}}`,
		"nameless reference":      `{"type":"tool_removal","tool":{"type":"tool_reference"}}`,
		"definition on removal": `{"type":"tool_removal","tool":{"type":"tool_definition",` +
			`"definition":{"name":"x","input_schema":{"type":"object"}}}}`,
		"non-object definition": `{"type":"tool_addition","tool":{"type":"tool_definition","definition":[]}}`,
	}
	approve := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMessagesRequest(envelope(block), approve); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// TestHandlerServesClaudeToolAddition is the end-to-end proof: the captured
// tool_addition request is POSTed to a real TranscodeHandler and must reach the
// upstream with the hoisted tool rather than being answered locally.
func TestHandlerServesClaudeToolAddition(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	// Approve what this captured traffic legitimately takes: a mid-conversation
	// system message (Claude Code appends one per tool-change batch) and the
	// usual usage/reasoning defaults. The tool-change decision itself is a Note
	// here, so it needs no approval.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem:  {},
		FeatureMultipleSystemTurns:    {},
		FeatureRequestReasoning:       {},
		FeatureReasoningSummary:       {},
		FeatureToolSchemaStrictness:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	var upstreamBody []byte
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	body := bytes.Replace(
		testcorpus.FieldClaudeToolAdditionDefinitionJSON(),
		[]byte(`"stream":true,`), nil, 1,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Anthropic-Beta", "inline-tools-2026-09-15")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the tool_addition request was answered %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"db_query"`, `"Read"`} {
		if !bytes.Contains(upstreamBody, []byte(want)) {
			t.Fatalf("upstream request is missing %s: %s", want, upstreamBody)
		}
	}
	if bytes.Contains(upstreamBody, []byte("tool_addition")) {
		t.Fatalf("a Messages-only block leaked into the chat request: %s", upstreamBody)
	}
}

// TestToolDefinitionUsesTheStrictClientContract proves a hoisted definition is
// decoded through the SAME strict tools[] path as a declared one, not a private
// probe. A private probe silently discarded every modelled tools[] member it did
// not happen to list — the client-contract hole the strict decode exists to
// close — and let a type-tagged server tool through as a plain chat function.
func TestToolDefinitionUsesTheStrictClientContract(t *testing.T) {
	envelope := func(definition string) []byte {
		return []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[{"type":"tool_addition","tool":` +
			`{"type":"tool_definition","definition":` + definition + `}}]}]}`)
	}
	// An unknown definition field is a client-dialect error, never a silent drop.
	_, err := DecodeMessagesRequest(envelope(
		`{"name":"db","input_schema":{"type":"object"},"totally_bogus":1}`,
	), StrictLossPolicy())
	if err == nil {
		t.Fatal("an unknown definition field was silently accepted")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %q, want an unknown-field rejection", err)
	}

	// A type-tagged server tool must not be smuggled past the server-tools gate.
	_, err = DecodeMessagesRequest(envelope(
		`{"type":"web_search_20250305","name":"ws"}`,
	), StrictLossPolicy())
	if err == nil {
		t.Fatal("a server-tool definition was hoisted past the server-tools gate")
	}
	if !strings.Contains(err.Error(), "server-side tool definition") {
		t.Fatalf("err = %q, want the server-tool refusal", err)
	}
}

// TestToolDefinitionSchemaChangeIsLastDefinitionWins proves a mid-conversation
// schema change is served rather than refused. The contract makes a later
// definition REPLACE the earlier one from its position onward, and it is the
// normal way a client ships a tool whose schema changed, so refusing it would
// 400 the very traffic this path exists to serve. A chat request carries one
// schema per name, so the only honest encoding is the newest definition serving
// the whole request — recorded as a Note, never a silent rewrite, and never two
// entries under one name.
func TestToolDefinitionSchemaChangeIsLastDefinitionWins(t *testing.T) {
	def := func(prop string) string {
		return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":` +
			`{"name":"db_query","input_schema":{"type":"object","properties":{"` + prop + `":{"type":"string"}}}}}}`
	}
	cases := map[string]string{
		"shadows a declared tool": `{"model":"m","max_tokens":16,` +
			`"tools":[{"name":"db_query","input_schema":{"type":"object"}}],` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def("a") + `]}]}`,
		"two definitions, one name": `{"model":"m","max_tokens":16,` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def("a") + `]},` +
			`{"role":"system","content":[` + def("b") + `]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
			if err != nil {
				t.Fatalf("a mid-conversation schema change was refused: %v", err)
			}
			// Exactly one entry per name, carrying the NEWEST schema.
			count := 0
			for _, tool := range result.Request.Tools {
				if tool.Name != "db_query" {
					continue
				}
				count++
				if !bytes.Contains(tool.JSONSchema, []byte(`"`+lastProp(name)+`"`)) {
					t.Fatalf("tool schema = %s, want the newest definition", tool.JSONSchema)
				}
			}
			if count != 1 {
				t.Fatalf("%q appears %d times in the tool list, want exactly one", "db_query", count)
			}
			// The supersession is observable, not silent.
			redefined := 0
			for _, loss := range result.Report.Losses {
				if loss.Feature == FeatureAnthropicToolChanges && strings.Contains(loss.Detail, "redefined") {
					redefined++
					if loss.Kind != NoteRecord {
						t.Fatalf("a supersession is a sanctioned encoding, got %v: %+v", loss.Kind, loss)
					}
				}
			}
			if redefined != 1 {
				t.Fatalf("supersession notes = %d, want exactly one: %+v", redefined, result.Report.Losses)
			}
			if err := ValidateCanonicalRequest(result.Request); err != nil {
				t.Fatalf("validator refused the decoder output: %v", err)
			}
		})
	}
}

// lastProp is the property the newest definition in each case declares.
func lastProp(caseName string) string {
	if caseName == "shadows a declared tool" {
		return "a"
	}
	return "b"
}

// TestToolChangeOnlySystemMessageIsNotATurn proves a system message that exists
// only to change tool availability does not become a conversation turn. Turning
// it into an empty-text system turn would (a) put proxy-invented prompt text on
// the wire, (b) charge the client a mid_conversation_system move for a message
// the client never wrote as content, and (c) refuse ordinary claude-cli traffic
// under a strict policy. The message contributes its tool change and nothing
// else, so the surrounding turns stay adjacent.
func TestToolChangeOnlySystemMessageIsNotATurn(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"x"}}]},` +
		`{"role":"user","content":"go"}]}`)
	result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicToolChanges: {},
	}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	requireTurnSequence(t, result.Request.Turns, CanonicalUser, CanonicalUser)
	for i, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if text, ok := part.(CanonicalText); ok && text.Text == "" {
				t.Fatalf("turn %d carries proxy-invented empty text: %+v", i, turn.Parts)
			}
		}
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("validator refused the decoder output: %v", err)
	}
	// With the tool change gone the request is a plain two-turn conversation,
	// which a strict Chat render must accept with no position loss at all.
	if _, report, err := RenderChatRequest(
		result.Request, testExchangeContext(), ChatCapabilities{},
	); err != nil {
		t.Fatalf("strict chat render refused a tool-change-only system message: %v", err)
	} else if reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("a message that is not a turn was charged a position: %+v", report.Losses)
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
