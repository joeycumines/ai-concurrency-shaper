package transcode

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const anthropicListingBlockJSON = `{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[]}`

func ignoredAnthropicConverter(allowed ...Feature) *messagesToGeminiConverter {
	policy := usageOnlyPolicy(allowed...)
	context := testStreamContext()
	context.LossPolicy = policy
	return newMessagesToGeminiConverter(
		newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", policy),
		newGeminiStreamClientState(context, UpstreamMessages, policy, "resp_1", "claude-x"),
	)
}

func ignoredAnthropicStart() SSEEvent {
	return SSEEvent{Event: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)}
}

func ignoredAnthropicBlockStart(index int, block string) SSEEvent {
	return SSEEvent{Event: "content_block_start", Data: []byte(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, block))}
}

func ignoredAnthropicBlockDelta(index int, delta string) SSEEvent {
	return SSEEvent{Event: "content_block_delta", Data: []byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, index, delta))}
}

func ignoredAnthropicBlockStop(index int) SSEEvent {
	return SSEEvent{Event: "content_block_stop", Data: []byte(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))}
}

func ignoredAnthropicTerminal() []SSEEvent {
	return []SSEEvent{
		{Event: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)},
		{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}
}

type ignoredAnthropicBlockCase struct {
	name   string
	block  string
	deltas []string
	key    Feature
}

func ignoredAnthropicBlockCases() []ignoredAnthropicBlockCase {
	arguments := []string{
		`{"type":"input_json_delta","partial_json":"{\"query\":"}`,
		`{"type":"input_json_delta","partial_json":"\"hello\"}"}`,
	}
	return []ignoredAnthropicBlockCase{
		{"thinking", `{"type":"thinking","thinking":"","signature":""}`, []string{
			`{"type":"thinking_delta","thinking":"private reasoning"}`,
			`{"type":"signature_delta","signature":"opaque signature"}`,
		}, FeatureAuthenticatedThinking},
		{"redacted thinking", `{"type":"redacted_thinking","data":"opaque encrypted data"}`, nil, FeatureAuthenticatedThinking},
		{"server tool use", `{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`, arguments, FeatureAnthropicServerTools},
		{"MCP tool use", `{"type":"mcp_tool_use","id":"mcp_1","server_name":"calendar","name":"search","input":{}}`, arguments, FeatureAnthropicServerTools},
		{"web search result", `{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]}`, nil, FeatureAnthropicServerTools},
		{"MCP tool result", `{"type":"mcp_tool_result","tool_use_id":"mcp_1","content":[]}`, nil, FeatureAnthropicServerTools},
		{"code execution", `{"type":"code_execution","id":"srv_1","name":"python","input":{}}`, arguments, FeatureAnthropicServerTools},
		{"code execution result", `{"type":"code_execution_tool_result","tool_use_id":"srv_1","content":{"type":"code_execution_result","stdout":"","stderr":"","return_code":0,"content":[]}}`, nil, FeatureAnthropicServerTools},
		{"container upload", `{"type":"container_upload","file_id":"file_1"}`, nil, FeatureAnthropicServerTools},
		{"MCP listing", anthropicListingBlockJSON, nil, FeatureAnthropicToolChanges},
	}
}

func TestAnthropicIgnoredBlocksKeepValidStreams(t *testing.T) {
	for _, test := range ignoredAnthropicBlockCases() {
		t.Run(test.name, func(t *testing.T) {
			c := ignoredAnthropicConverter(FeatureAuthenticatedThinking, FeatureAnthropicServerTools)
			frames := []SSEEvent{ignoredAnthropicStart()}
			// Two occurrences prove both that indices remain distinct and that
			// the shared report decision is still emitted once per stream.
			for index := range 2 {
				frames = append(frames, ignoredAnthropicBlockStart(index, test.block))
				for _, delta := range test.deltas {
					frames = append(frames, ignoredAnthropicBlockDelta(index, delta))
				}
				frames = append(frames, ignoredAnthropicBlockStop(index))
			}
			frames = append(frames,
				ignoredAnthropicBlockStart(2, `{"type":"text","text":"answer"}`),
				ignoredAnthropicBlockStop(2),
			)
			frames = append(frames, ignoredAnthropicTerminal()...)
			var output strings.Builder
			terminals := 0
			for _, frame := range frames {
				batch, err := c.Convert(frame)
				if err != nil {
					t.Fatalf("valid %s: %v", frame.Data, err)
				}
				for _, event := range batch.Events {
					output.Write(event.Data)
				}
				if batch.Terminal {
					terminals++
				}
			}
			if terminals != 1 || !strings.Contains(output.String(), `"text":"answer"`) {
				t.Fatalf("valid stream lost its answer or terminal: terminals=%d, output=%s", terminals, output.String())
			}
			for _, private := range []string{"private reasoning", "opaque signature", "opaque encrypted data", "calendar", "query"} {
				if strings.Contains(output.String(), private) {
					t.Fatalf("discarded content %q leaked into %s", private, output.String())
				}
			}
			if batch, err := c.FinalizeEOF(); err != nil || !batch.Terminal {
				t.Fatalf("valid EOF: batch=%+v, error=%v", batch, err)
			}
			if n := countReportFeature(c.upstream.report, test.key); n != 1 {
				t.Fatalf("report key %s recorded %d times, want once: %+v", test.key, n, c.upstream.report.Losses)
			}
		})
	}
}

func assertIgnoredAnthropicFailure(t *testing.T, c *messagesToGeminiConverter, frames []SSEEvent) {
	t.Helper()
	for _, frame := range frames {
		batch, err := c.Convert(frame)
		if err != nil {
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("wrong failure class: %T: %v", err, err)
			}
			assertLatchedBatch(t, "failed call", batch, err, err)
			after, afterErr := c.Convert(SSEEvent{Event: "ping", Data: []byte(`{"type":"ping"}`)})
			assertLatchedBatch(t, "event after failure", after, afterErr, err)
			eof, eofErr := c.FinalizeEOF()
			assertLatchedBatch(t, "EOF after failure", eof, eofErr, err)
			return
		}
		if batch.Terminal {
			t.Fatal("malformed block lifecycle produced a successful terminal")
		}
	}
	t.Fatal("malformed block lifecycle was not refused")
}

func TestAnthropicIgnoredBlocksValidateLifecycle(t *testing.T) {
	for _, block := range ignoredAnthropicBlockCases() {
		t.Run(block.name, func(t *testing.T) {
			start := ignoredAnthropicBlockStart(0, block.block)
			stop := ignoredAnthropicBlockStop(0)
			delta := ignoredAnthropicBlockDelta(0, `{"type":"text_delta","text":"not valid on this block"}`)
			for name, middle := range map[string][]SSEEvent{
				"unclosed block":           {start},
				"duplicate stop":           {start, stop, stop},
				"delta after stop":         {start, stop, delta},
				"wrong delta kind":         {start, delta, stop},
				"reused open index":        {start, start},
				"reused closed index":      {start, stop, start},
				"stop before start":        {stop},
				"delta before start":       {delta},
				"negative block index":     {ignoredAnthropicBlockStart(-1, block.block)},
				"stop after final delta":   {start, ignoredAnthropicTerminal()[0], stop},
				"start after final delta":  {ignoredAnthropicTerminal()[0], start},
				"missing block stop index": {start, {Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop"}`)}},
				"missing content delta":    {start, {Event: "content_block_delta", Data: []byte(`{"type":"content_block_delta","index":0}`)}},
			} {
				t.Run(name, func(t *testing.T) {
					frames := append([]SSEEvent{ignoredAnthropicStart()}, middle...)
					frames = append(frames, ignoredAnthropicTerminal()...)
					assertIgnoredAnthropicFailure(t, ignoredAnthropicConverter(FeatureAuthenticatedThinking, FeatureAnthropicServerTools), frames)
				})
			}
		})
	}
}

func TestAnthropicIgnoredDeltasRequireTheirOwnPayload(t *testing.T) {
	for _, test := range []struct {
		name, block, delta string
	}{
		{"missing thinking", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta"}`},
		{"missing signature", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"signature_delta"}`},
		{"missing arguments", `{"type":"server_tool_use","id":"srv_1","name":"search","input":{}}`, `{"type":"input_json_delta"}`},
		{"foreign field", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta","thinking":"private","text":"cannot be silently dropped"}`},
		{"foreign null field", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"thinking_delta","thinking":"private","text":null}`},
		{"unknown delta type", `{"type":"thinking","thinking":"","signature":""}`, `{"type":"unknown_delta"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertIgnoredAnthropicFailure(t, ignoredAnthropicConverter(FeatureAuthenticatedThinking, FeatureAnthropicServerTools), []SSEEvent{
				ignoredAnthropicStart(), ignoredAnthropicBlockStart(0, test.block), ignoredAnthropicBlockDelta(0, test.delta),
			})
		})
	}
}

func TestAnthropicIgnoredBlockEOFStaysFailed(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("block closed=%t", closed), func(t *testing.T) {
			c := ignoredAnthropicConverter()
			frames := []SSEEvent{ignoredAnthropicStart(), ignoredAnthropicBlockStart(0, anthropicListingBlockJSON)}
			if closed {
				frames = append(frames, ignoredAnthropicBlockStop(0))
			}
			assertEOFLatched(t, c, frames...)
			batch, err := c.Convert(ignoredAnthropicTerminal()[0])
			assertLatchedBatch(t, "event after truncated EOF", batch, err, c.streamError)
		})
	}
}

func TestAnthropicIgnoredBlocksRespectBudgetsAndRollback(t *testing.T) {
	t.Run("state bound precedes listing note", func(t *testing.T) {
		c := ignoredAnthropicConverter()
		c.upstream.budget.stateEntriesBudget = 0
		assertIgnoredAnthropicFailure(t, c, []SSEEvent{ignoredAnthropicStart(), ignoredAnthropicBlockStart(0, anthropicListingBlockJSON)})
		if len(c.upstream.blocks) != 0 || reportHasFeature(c.upstream.report, FeatureAnthropicToolChanges) {
			t.Fatalf("refused block allocated state or recorded a note: blocks=%d, report=%+v", len(c.upstream.blocks), c.upstream.report.Losses)
		}
	})
	t.Run("closed entries remain bounded", func(t *testing.T) {
		c := ignoredAnthropicConverter()
		c.upstream.budget.stateEntriesBudget = 1
		assertIgnoredAnthropicFailure(t, c, []SSEEvent{
			ignoredAnthropicStart(), ignoredAnthropicBlockStart(0, anthropicListingBlockJSON), ignoredAnthropicBlockStop(0),
			ignoredAnthropicBlockStart(1, anthropicListingBlockJSON),
		})
		if len(c.upstream.blocks) != 1 {
			t.Fatalf("state bound retained %d blocks, want one", len(c.upstream.blocks))
		}
	})
	t.Run("discarded deltas consume the event budget", func(t *testing.T) {
		c := ignoredAnthropicConverter(FeatureAuthenticatedThinking)
		c.upstream.budget.eventsBudget = 2
		assertIgnoredAnthropicFailure(t, c, []SSEEvent{
			ignoredAnthropicStart(), ignoredAnthropicBlockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
			ignoredAnthropicBlockDelta(0, `{"type":"thinking_delta","thinking":"private"}`),
		})
	})
	t.Run("failed delivery rolls back the listing note and latches", func(t *testing.T) {
		c := ignoredAnthropicConverter()
		if _, err := c.Convert(ignoredAnthropicStart()); err != nil {
			t.Fatal(err)
		}
		refused := errors.New("delivery refused")
		c.installDelivery(func(convertedBatch) ([]byte, error) { return nil, refused })
		batch, err := c.Convert(ignoredAnthropicBlockStart(0, anthropicListingBlockJSON))
		assertLatchedBatch(t, "refused listing", batch, err, refused)
		if reportHasFeature(*c.ConversionReport(), FeatureAnthropicToolChanges) {
			t.Fatalf("refused listing left a note: %+v", c.ConversionReport().Losses)
		}
		batch, err = c.Convert(ignoredAnthropicBlockStop(0))
		assertLatchedBatch(t, "stop after delivery failure", batch, err, refused)
		batch, err = c.FinalizeEOF()
		assertLatchedBatch(t, "EOF after delivery failure", batch, err, refused)
	})
}

func TestGeminiStreamMessagesListingLifecycle(t *testing.T) {
	listing := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":` + anthropicListingBlockJSON + `}` + "\n\n"
	stop := "event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n"
	delta := "event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"unexpected"}}` + "\n\n"
	text := strings.ReplaceAll(anthropicTextBlockSSE, `"index":0`, `"index":1`)
	for _, test := range []struct {
		name, content string
		valid         bool
	}{
		{"complete listing", listing + stop + text, true},
		{"unclosed listing", listing + text, false},
		{"repeated listing stop", listing + stop + stop + text, false},
		{"delta after listing stop", listing + stop + delta + text, false},
		{"text delta on listing", listing + delta + stop + text, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := serveGeminiStreamBody(t,
				geminiMessagesStreamHandler(t, anthropicStartSSE+test.content+anthropicStopSSE("end_turn"), nil),
				geminiMessagesStreamTarget, geminiMessagesBody,
			)
			frames := geminiStreamFrames(t, rec.Body.String())
			if test.valid {
				assertSingleTerminal(t, frames)
				if geminiFrameText(frames) != "hello" || geminiFrameFinish(frames[len(frames)-1]) != "STOP" {
					t.Fatalf("valid listing lost the answer or finish: %s", rec.Body.String())
				}
				return
			}
			if len(frames) == 0 {
				t.Fatalf("failed listing has no error frame: HTTP %d, %s", rec.Code, rec.Body.String())
			}
			for _, frame := range frames {
				if geminiFrameFinish(frame) != "" {
					t.Fatalf("malformed listing reported success: HTTP %d, %s", rec.Code, rec.Body.String())
				}
			}
			if _, ok := frames[len(frames)-1]["error"]; !ok {
				t.Fatalf("failed listing has no error terminal: %s", rec.Body.String())
			}
		})
	}
}
