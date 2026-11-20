package transcode

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

const anthropicListingJSON = `{"type":"mcp_tool_listing","mcp_server_name":"s","tools":[{"name":"echo","input_schema":{"type":"object"}}]}`

const anthropicMessageDeltaSSE = "event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n"

func anthropicBlockStartSSE(index int, block string) string {
	return "event: content_block_start\n" +
		fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":%s}`, index, block) + "\n\n"
}

func anthropicBlockStopSSE(index int) string {
	return "event: content_block_stop\n" +
		fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index) + "\n\n"
}

func anthropicBlockDeltaSSE(index int, delta string) string {
	return "event: content_block_delta\n" +
		fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":%s}`, index, delta) + "\n\n"
}

func newMessagesGeminiStreamForTest(policy LossPolicy) *messagesToGeminiConverter {
	ctx := testStreamContext()
	ctx.LossPolicy = policy
	return newMessagesToGeminiConverter(
		newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", policy),
		newGeminiStreamClientState(ctx, UpstreamMessages, policy, "resp_1", "claude-x"),
	)
}

// A discarded block still participates in the upstream protocol. These tests
// exercise the converter's failure latch and the handler's actual error
// terminal, so suppressing a malformed listing cannot turn it into success.
func TestGeminiStreamMessagesListingRejectsMalformedLifecycle(t *testing.T) {
	start := anthropicBlockStartSSE(0, anthropicListingJSON)
	stop := anthropicBlockStopSSE(0)
	text := strings.ReplaceAll(anthropicTextBlockSSE, `"index":0`, `"index":1`)
	for name, listing := range map[string]string{
		"missing stop":                    start,
		"duplicate stop":                  start + stop + stop,
		"delta after stop":                start + stop + anthropicBlockDeltaSSE(0, `{"type":"text_delta","text":"LOST"}`),
		"unknown delta":                   start + anthropicBlockDeltaSSE(0, `{"type":"unknown_delta","text":"LOST"}`) + stop,
		"incompatible delta":              start + anthropicBlockDeltaSSE(0, `{"type":"input_json_delta","partial_json":"{}"}`) + stop,
		"negative index":                  anthropicBlockStartSSE(-1, anthropicListingJSON) + anthropicBlockStopSSE(-1),
		"reused open index":               start + start + stop,
		"reused closed index":             start + stop + start + stop,
		"stop before start":               stop + start + stop,
		"message delta before block stop": start + anthropicMessageDeltaSSE + stop,
		"start after message delta": start + stop + anthropicMessageDeltaSSE +
			anthropicBlockStartSSE(2, anthropicListingJSON) + anthropicBlockStopSSE(2),
		"delta after message delta": start + stop + anthropicMessageDeltaSSE +
			anthropicBlockDeltaSSE(0, `{"type":"text_delta","text":"LOST"}`),
		"stop after message delta": start + stop + anthropicMessageDeltaSSE + stop,
	} {
		t.Run(name, func(t *testing.T) {
			sse := anthropicStartSSE + listing + text + anthropicStopSSE("end_turn")
			frames, err := readAllEvents(t, sse)
			if err != nil {
				t.Fatal(err)
			}
			converter := newMessagesGeminiStreamForTest(usageOnlyPolicy())
			var firstError error
			for _, frame := range frames {
				batch, err := converter.Convert(frame)
				if firstError != nil {
					if !errors.Is(err, firstError) || batch.Terminal || len(batch.Events) != 0 {
						t.Fatalf("event after failure: batch=%+v err=%v, want latched %v", batch, err, firstError)
					}
					continue
				}
				if err != nil {
					if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
						t.Fatalf("failure = %T %v, want upstream wire error", err, err)
					}
					firstError = err
				}
				if batch.Terminal {
					t.Fatalf("malformed listing produced a successful terminal: %+v", batch)
				}
			}
			if firstError == nil {
				t.Fatal("malformed listing was accepted")
			}
			final, err := converter.FinalizeEOF()
			if !errors.Is(err, firstError) || final.Terminal || len(final.Events) != 0 {
				t.Fatalf("EOF after failure: batch=%+v err=%v, want latched %v", final, err, firstError)
			}

			rec := serveGeminiStreamBody(t,
				geminiMessagesStreamHandler(t, sse, nil),
				geminiMessagesStreamTarget, geminiMessagesBody,
			)
			clientFrames := geminiStreamFrames(t, rec.Body.String())
			errorsSeen := 0
			for _, frame := range clientFrames {
				if geminiFrameFinish(frame) != "" {
					t.Fatalf("malformed listing reached a successful client terminal: %s", rec.Body.String())
				}
				if _, isError := frame["error"]; isError {
					errorsSeen++
				}
			}
			if errorsSeen != 1 {
				t.Fatalf("error terminals = %d, want exactly one: %s", errorsSeen, rec.Body.String())
			}
		})
	}
}

func TestGeminiStreamMessagesListingOmissionPreservesAnswer(t *testing.T) {
	// Two listings prove one omission note per stream, with neither listing
	// taking an output index or requiring a loss-policy approval.
	listing := anthropicBlockStartSSE(0, anthropicListingJSON) + anthropicBlockStopSSE(0) +
		anthropicBlockStartSSE(1, anthropicListingJSON) + anthropicBlockStopSSE(1)
	text := strings.ReplaceAll(anthropicTextBlockSSE, `"index":0`, `"index":2`)
	// A provider may emit more than one message_delta, with cumulative usage.
	// The content phase ends at the first; subsequent usage updates still work.
	sse := anthropicStartSSE + listing + text + anthropicMessageDeltaSSE + anthropicStopSSE("end_turn")
	frames, err := readAllEvents(t, sse)
	if err != nil {
		t.Fatal(err)
	}
	converter := newMessagesGeminiStreamForTest(usageOnlyPolicy())
	for _, frame := range frames {
		if _, err := converter.Convert(frame); err != nil {
			t.Fatalf("valid listing stream: %v", err)
		}
	}
	final, err := converter.FinalizeEOF()
	if err != nil || !final.Terminal {
		t.Fatalf("valid listing EOF: batch=%+v err=%v", final, err)
	}
	if usage := converter.upstream.responsesUsage(); usage == nil || usage.OutputTokens != 2 {
		t.Fatalf("repeated message_delta lost cumulative usage: %+v", usage)
	}
	notes := 0
	for _, entry := range converter.ConversionReport().Losses {
		if entry.Feature == FeatureAnthropicToolChanges {
			notes++
			if entry.Kind != NoteRecord {
				t.Fatalf("listing omission must remain a note: %+v", entry)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("listing omission notes = %d, want one", notes)
	}
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, sse, nil),
		geminiMessagesStreamTarget, geminiMessagesBody,
	)
	clientFrames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, clientFrames)
	if got := geminiFrameText(clientFrames); got != "hello" {
		t.Fatalf("answer = %q, want hello: %s", got, rec.Body.String())
	}
	if finish := geminiFrameFinish(clientFrames[len(clientFrames)-1]); finish != "STOP" {
		t.Fatalf("finish = %q, want STOP: %s", finish, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "mcp_tool_listing") || strings.Contains(rec.Body.String(), "echo") {
		t.Fatalf("provider listing leaked into client stream: %s", rec.Body.String())
	}
}

func TestAnthropicStreamOmittedBlockKinds(t *testing.T) {
	policy := usageOnlyPolicy()
	policy.Allowed[FeatureAuthenticatedThinking] = struct{}{}
	policy.Allowed[FeatureAnthropicServerTools] = struct{}{}
	for _, tc := range []struct {
		name, block string
		deltas      []string
	}{
		{"listing", anthropicListingJSON, nil},
		{"thinking", `{"type":"thinking","thinking":"","signature":""}`, []string{
			`{"type":"thinking_delta","thinking":"private"}`,
			`{"type":"signature_delta","signature":"sig"}`,
		}},
		{"redacted thinking", `{"type":"redacted_thinking","data":"opaque"}`, nil},
		{"server tool call", `{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`, []string{
			`{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}`,
		}},
		{"MCP tool call", `{"type":"mcp_tool_use","id":"mcp_1","server_name":"s","name":"echo","input":{}}`, []string{
			`{"type":"input_json_delta","partial_json":"{}"}`,
		}},
		{"legacy code execution call", `{"type":"code_execution","id":"code_1","input":{}}`, []string{
			`{"type":"input_json_delta","partial_json":"{}"}`,
		}},
		{"web result", `{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]}`, nil},
		{"MCP result", `{"type":"mcp_tool_result","tool_use_id":"mcp_1","content":"result"}`, nil},
		{"code execution result", `{"type":"code_execution_tool_result","tool_use_id":"code_1","content":{}}`, nil},
		{"container upload", `{"type":"container_upload","file_id":"file_1"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := anthropicBlockStartSSE(0, tc.block)
			for _, delta := range tc.deltas {
				block += anthropicBlockDeltaSSE(0, delta)
			}
			for _, closed := range []bool{false, true} {
				t.Run(fmt.Sprintf("closed=%t", closed), func(t *testing.T) {
					sse := anthropicStartSSE + block
					if closed {
						sse += anthropicBlockStopSSE(0)
					}
					sse += anthropicStopSSE("end_turn")
					frames, err := readAllEvents(t, sse)
					if err != nil {
						t.Fatal(err)
					}
					converter := newMessagesGeminiStreamForTest(policy)
					var failure error
					for _, frame := range frames {
						if _, err := converter.Convert(frame); err != nil {
							failure = err
							break
						}
					}
					final, err := converter.FinalizeEOF()
					if closed {
						if failure != nil || err != nil || !final.Terminal {
							t.Fatalf("valid omitted block failed: convert=%v EOF=%v batch=%+v", failure, err, final)
						}
					} else if failure == nil || !errors.Is(err, failure) || final.Terminal {
						t.Fatalf("open omitted block did not latch a failure: convert=%v EOF=%v batch=%+v", failure, err, final)
					}
					// Omitted payloads must not accumulate merely to validate their
					// lifecycle; the one bookkeeping entry remains bounded.
					if converter.upstream.budget.semanticBytes != 0 || converter.upstream.budget.stateEntries != 1 {
						t.Fatalf("omitted block retained payload or mischarged state: %+v", converter.upstream.budget)
					}
				})
			}
		})
	}
}

func TestAnthropicStreamContentDeltaArms(t *testing.T) {
	policy := usageOnlyPolicy()
	policy.Allowed[FeatureAuthenticatedThinking] = struct{}{}
	policy.Allowed[FeatureAnthropicServerTools] = struct{}{}
	for _, tc := range []struct {
		name, block, delta string
	}{
		{"missing thinking", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta"}`},
		{"null signature", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"signature_delta","signature":null}`},
		{"thinking and text", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta","thinking":"x","text":"hidden"}`},
		{"thinking and null text", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta","thinking":"x","text":null}`},
		{"thinking and stop", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta","thinking":"x","stop_reason":"end_turn"}`},
		{"thinking and null stop", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta","thinking":"x","stop_sequence":null}`},
		{"redacted thinking delta", `{"type":"redacted_thinking","data":"opaque"}`, `{"type":"thinking_delta","thinking":"x"}`},
		{"missing tool input", `{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`, `{"type":"input_json_delta"}`},
		{"tool input and text", `{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`, `{"type":"input_json_delta","partial_json":"{}","text":"hidden"}`},
		{"tool call text delta", `{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`, `{"type":"text_delta","text":"hidden"}`},
		{"missing visible text", `{"type":"text","text":""}`, `{"type":"text_delta"}`},
		{"visible text and signature", `{"type":"text","text":""}`, `{"type":"text_delta","text":"x","signature":"hidden"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames, err := readAllEvents(t, anthropicStartSSE+anthropicBlockStartSSE(0, tc.block)+anthropicBlockDeltaSSE(0, tc.delta))
			if err != nil {
				t.Fatal(err)
			}
			converter := newMessagesGeminiStreamForTest(policy)
			for _, frame := range frames[:len(frames)-1] {
				if _, err := converter.Convert(frame); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			batch, err := converter.Convert(frames[len(frames)-1])
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok || batch.Terminal || len(batch.Events) != 0 {
				t.Fatalf("invalid delta accepted: err=%v batch=%+v", err, batch)
			}
			final, eofErr := converter.FinalizeEOF()
			if !errors.Is(eofErr, err) || final.Terminal {
				t.Fatalf("delta failure not latched: err=%v EOF=%v batch=%+v", err, eofErr, final)
			}
		})
	}
}

func TestAnthropicStreamListingStateBudget(t *testing.T) {
	state := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", StrictLossPolicy())
	state.budget.stateEntriesBudget = 0
	frames, err := readAllEvents(t, anthropicStartSSE+anthropicBlockStartSSE(0, anthropicListingJSON))
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		event, err := decodeAnthropicStreamFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		_, err = state.Convert(event)
		if event.Type == anthropicmessages.StreamEventTypeContentBlockStart {
			if !isUpstreamConversionError(err) {
				t.Fatalf("listing over state bound: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if len(state.blocks) != 0 || reportHasFeature(state.report, FeatureAnthropicToolChanges) {
		t.Fatalf("refused listing committed state or a note: blocks=%+v report=%+v", state.blocks, state.report)
	}
}
