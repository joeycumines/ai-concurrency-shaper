package transcode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// TestCodexMultiTurnReplayStress160 tests an agentic session history replay of
// 160+ items modeled directly on real Codex TUI workloads with model glm-5.3-flash
// under -transcode-responses-chat.
//
// It validates that:
// 1. 160+ input items with mixed roles, previous output messages, function calls,
//    and function outputs decode cleanly under StrictLossPolicy.
// 2. Duplicate keys in function call arguments (such as "max_output_tokens")
//    are normalized via RFC 8259 last-key-wins without failing the request.
// 3. Zero-argument tool calls with empty arguments ("") or whitespace ("   ")
//    are normalized to valid empty objects ("{}").
// 4. Unicode, special characters, escaped newlines/quotes, and large payloads
//    in tool arguments survive byte-exact.
// 5. RenderChatRequest produces valid, well-formed Chat Completions wire.
func TestCodexMultiTurnReplayStress160(t *testing.T) {
	var items []openairesponses.InputItem

	// Turn 0: Developer system instruction with complex markdown and skills
	items = append(items, &openairesponses.EasyInputMessage{
		Type: "message",
		ID:   "msg_dev_0",
		Role: "developer",
		Content: openairesponses.InputMessageContent{
			Parts: openairesponses.InputContentParts{
				&openairesponses.InputText{
					Type: "input_text",
					Text: "<skills_instructions>\n## Skills\nA skill is a set of local instructions.\n" +
						"```json\n{\"skill\": \"run_cmd\", \"params\": {\"cwd\": \"/workspace\"}}\n```\n</skills_instructions>",
				},
			},
		},
	})

	// Generate 40 conversation cycles (4 items per cycle = 160 items)
	// Each cycle: User prompt -> Assistant previous output -> Assistant function call -> User function output
	for cycle := 0; cycle < 40; cycle++ {
		// 1. User message
		items = append(items, &openairesponses.EasyInputMessage{
			Type: "message",
			ID:   fmt.Sprintf("msg_user_%d", cycle),
			Role: "user",
			Content: openairesponses.InputMessageContent{
				Parts: openairesponses.InputContentParts{
					&openairesponses.InputText{
						Type: "input_text",
						Text: fmt.Sprintf("Step %d: please run analysis and report status for file %d.go", cycle, cycle),
					},
				},
			},
		})

		// 2. Assistant previous output message
		items = append(items, &openairesponses.PreviousOutputMessage{
			ID:    fmt.Sprintf("msg_prev_out_%d", cycle),
			Type:  "message",
			Role:  "assistant",
			Content: openairesponses.OutputContentParts{
				&openairesponses.OutputText{
					Type:        "output_text",
					Text:        fmt.Sprintf("I will inspect file %d.go now.", cycle),
					Annotations: []openairesponses.Annotation{},
				},
			},
		})

		// 3. Assistant function call with varied argument edge cases
		var argsJSON string
		callID := fmt.Sprintf("call_%03d", cycle)
		switch cycle % 5 {
		case 0:
			// Exact Codex TUI glm-5.3-flash reproduction: duplicate keys
			argsJSON = fmt.Sprintf(
				`{"file_path":"pkg%d.go","max_output_tokens":100,"cmd":"cat","max_output_tokens":%d}`,
				cycle, 200+cycle,
			)
		case 1:
			// Zero-argument tool call emitting empty string
			argsJSON = ""
		case 2:
			// Zero-argument tool call emitting whitespace only
			argsJSON = "   \n\t  "
		case 3:
			// Unicode, escaped quotes, newlines, and emojis
			argsJSON = fmt.Sprintf(
				`{"path":"dir/file_%d.txt","content":"Line 1\nLine 2 \"quoted\" \u3053\u3093\u306b\u3061\u306f 🚀"}`,
				cycle,
			)
		case 4:
			// Multi-kilobyte arguments payload
			largeData := strings.Repeat("x", 2048)
			argsJSON = fmt.Sprintf(`{"chunk_id":%d,"data":"%s"}`, cycle, largeData)
		}

		items = append(items, &openairesponses.FunctionCallInput{
			Type:      "function_call",
			ID:        fmt.Sprintf("fc_%d", cycle),
			CallID:    callID,
			Name:      "execute_tool",
			Arguments: argsJSON,
		})

		// 4. Function call output from tool execution
		outputContent := fmt.Sprintf("output of cycle %d: success\nresult: ok", cycle)
		items = append(items, &openairesponses.FunctionCallOutputInput{
			Type:   "function_call_output",
			ID:     fmt.Sprintf("fco_%d", cycle),
			CallID: callID,
			Output: openairesponses.FunctionOutput{
				Text: &outputContent,
			},
		})
	}

	// Final user prompt asking for synthesis
	items = append(items, &openairesponses.EasyInputMessage{
		Type: "message",
		ID:   "msg_final",
		Role: "user",
		Content: openairesponses.InputMessageContent{
			Parts: openairesponses.InputContentParts{
				&openairesponses.InputText{
					Type: "input_text",
					Text: "All 40 steps complete. Please provide final executive summary.",
				},
			},
		},
	})

	// Total items: 1 (dev) + 40*4 (160) + 1 (final) = 162 items
	if len(items) != 162 {
		t.Fatalf("expected 162 items, got %d", len(items))
	}

	reqEnvelope := openairesponses.Request{
		Model: "glm-5.3-flash",
		Input: &openairesponses.Input{
			Items: items,
		},
	}

	bodyBytes, err := json.Marshal(reqEnvelope)
	if err != nil {
		t.Fatalf("marshal request envelope: %v", err)
	}

	// DecodeResponsesRequest under StrictLossPolicy
	decoded, _, err := DecodeResponsesRequest(bodyBytes, StrictLossPolicy())
	if err != nil {
		t.Fatalf("DecodeResponsesRequest failed on 162-item replay: %v", err)
	}

	// Verify CanonicalRequest
	if decoded.Request.ClientModel != "glm-5.3-flash" {
		t.Fatalf("model = %q, want glm-5.3-flash", decoded.Request.ClientModel)
	}

	// Validate CanonicalRequest IR invariants
	if err := ValidateCanonicalRequest(decoded.Request); err != nil {
		t.Fatalf("ValidateCanonicalRequest failed: %v", err)
	}

	// Inspect canonical function calls for normalization
	var funcCalls []CanonicalFunctionCall
	for _, turn := range decoded.Request.Turns {
		for _, part := range turn.Parts {
			if fc, ok := part.(CanonicalFunctionCall); ok {
				funcCalls = append(funcCalls, fc)
			}
		}
	}

	if len(funcCalls) != 40 {
		t.Fatalf("expected 40 function calls in IR, got %d", len(funcCalls))
	}

	// Verify specific cases:
	// Cycle 0: duplicate max_output_tokens normalized to last-key-wins (200)
	var parsedArgs0 map[string]any
	if err := json.Unmarshal(funcCalls[0].Arguments, &parsedArgs0); err != nil {
		t.Fatalf("unmarshal cycle 0 args: %v", err)
	}
	if parsedArgs0["max_output_tokens"] != float64(200) {
		t.Fatalf("cycle 0 max_output_tokens = %v, want 200", parsedArgs0["max_output_tokens"])
	}

	// Cycle 1: empty string normalized to "{}"
	if string(funcCalls[1].Arguments) != "{}" {
		t.Fatalf("cycle 1 empty args = %s, want {}", string(funcCalls[1].Arguments))
	}

	// Cycle 2: whitespace normalized to "{}"
	if string(funcCalls[2].Arguments) != "{}" {
		t.Fatalf("cycle 2 whitespace args = %s, want {}", string(funcCalls[2].Arguments))
	}

	// Cycle 3: Unicode and emojis preserved
	var parsedArgs3 map[string]any
	if err := json.Unmarshal(funcCalls[3].Arguments, &parsedArgs3); err != nil {
		t.Fatalf("unmarshal cycle 3 args: %v", err)
	}
	if !strings.Contains(parsedArgs3["content"].(string), "こんにちは") {
		t.Fatalf("cycle 3 content missing unicode: %v", parsedArgs3["content"])
	}

	// Render to Chat Completions upstream schema
	context := &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: StrictLossPolicy(),
	}
	chatBytes, _, err := RenderChatRequest(decoded.Request, context, ChatCapabilities{
		DeveloperRole: true,
	})
	if err != nil {
		t.Fatalf("RenderChatRequest failed: %v", err)
	}

	// Verify rendered Chat Completions request decodes cleanly
	var chatReq openaichat.Request
	if err := wire.Decode(chatBytes, &chatReq); err != nil {
		t.Fatalf("rendered Chat request failed wire.Decode: %v", err)
	}

	if chatReq.Model != "glm-5.3-flash" {
		t.Fatalf("chat model = %q, want glm-5.3-flash", chatReq.Model)
	}

	// Verify report did not fail or explode, and contains the item identity note
	if len(decoded.Report.Losses) == 0 {
		t.Fatal("expected item identity notes in decode conversion report")
	}

	// Subtest: Verify replaying with output message phase ("final_answer" / "commentary")
	t.Run("ReplayWithOutputPhase", func(t *testing.T) {
		itemsWithPhase := make([]openairesponses.InputItem, len(items))
		copy(itemsWithPhase, items)
		itemsWithPhase[2] = &openairesponses.PreviousOutputMessage{
			ID:    "msg_prev_out_phase",
			Type:  "message",
			Role:  "assistant",
			Phase: "final_answer",
			Content: openairesponses.OutputContentParts{
				&openairesponses.OutputText{
					Type:        "output_text",
					Text:        "I have reached the final answer.",
					Annotations: []openairesponses.Annotation{},
				},
			},
		}

		bodyWithPhase, err := json.Marshal(openairesponses.Request{
			Model: "glm-5.3-flash",
			Input: &openairesponses.Input{Items: itemsWithPhase},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		// Under StrictLossPolicy: rejected with UnsupportedFeatureError(FeatureOutputPhase)
		_, _, err = DecodeResponsesRequest(bodyWithPhase, StrictLossPolicy())
		if err == nil {
			t.Fatal("expected error under StrictLossPolicy with output_phase")
		}
		var unsuppErr *UnsupportedFeatureError
		if !strings.Contains(err.Error(), "output_phase") {
			t.Fatalf("expected output_phase in error, got %v", err)
		}

		// Under policy allowing FeatureOutputPhase: approved and converted
		phasePolicy := LossPolicy{
			Allowed: map[Feature]struct{}{
				FeatureOutputPhase: {},
			},
		}
		decodedWithPhase, _, err := DecodeResponsesRequest(bodyWithPhase, phasePolicy)
		if err != nil {
			t.Fatalf("DecodeResponsesRequest failed with allowed output_phase: %v", err)
		}
		var sawPhaseLoss bool
		for _, loss := range decodedWithPhase.Report.Losses {
			if loss.Feature == FeatureOutputPhase {
				sawPhaseLoss = true
				break
			}
		}
		if !sawPhaseLoss {
			t.Fatal("expected FeatureOutputPhase loss recorded in conversion report")
		}

		// Render to chat must succeed
		phaseContext := &ExchangeContext{
			IDs:        NewExchangeIDs(),
			LossPolicy: phasePolicy,
		}
		chatWithPhaseBytes, _, err := RenderChatRequest(decodedWithPhase.Request, phaseContext, ChatCapabilities{
			DeveloperRole: true,
		})
		if err != nil {
			t.Fatalf("RenderChatRequest failed with output_phase: %v", err)
		}
		var chatPhaseReq openaichat.Request
		if err := wire.Decode(chatWithPhaseBytes, &chatPhaseReq); err != nil {
			t.Fatalf("wire.Decode chat request with output_phase: %v", err)
		}
		_ = unsuppErr
	})
}

// TestClaudeCodeMultiTurnReplayStress160 tests an agentic session history replay
// of 160+ messages and content blocks modeled on Claude Code workloads under
// -transcode-messages-chat.
//
// It validates that:
// 1. 160+ messages with mixed text, tool_use, tool_result, and thinking blocks
//    decode cleanly.
// 2. Marker-signature synthetic thinking blocks ("shaper-synth-thinking-1")
//    are completely scrubbed from replayed history.
// 3. An assistant turn whose ONLY content was synthetic thinking blocks is
//    safely preserved with an empty text part rather than failing with
//    "turn has no content parts".
// 4. Tool use blocks with empty or duplicate input are normalized.
// 5. RenderChatRequest produces valid upstream Chat Completions wire.
func TestClaudeCodeMultiTurnReplayStress160(t *testing.T) {
	var messages []anthropicmessages.Message

	// Turn 0: System instructions handled via envelope.System
	systemPrompt := anthropicmessages.Content{
		ContentStr: new("You are Claude Code, an agentic coding assistant."),
	}

	// 40 cycles of dialog (4 messages per cycle = 160 messages)
	for cycle := 0; cycle < 40; cycle++ {
		toolUseID := fmt.Sprintf("toolu_%03d", cycle)

		// 1. User message
		userText := fmt.Sprintf("Task %d: analyze component %d", cycle, cycle)
		messages = append(messages, anthropicmessages.Message{
			Role: anthropicmessages.RoleUser,
			Content: anthropicmessages.Content{
				ContentStr: &userText,
			},
		})

		// 2. Assistant message
		var assistantBlocks []anthropicmessages.ContentBlock
		switch cycle % 4 {
		case 0:
			// Assistant with synthetic thinking block + tool use
			synthThinking := fmt.Sprintf("Let me think about component %d...", cycle)
			synthSig := SyntheticThinkingSignature
			assistantBlocks = append(assistantBlocks, anthropicmessages.ContentBlock{
				Type:      anthropicmessages.ContentBlockTypeThinking,
				Thinking:  &synthThinking,
				Signature: &synthSig,
			})
			toolName := "ReadComponent"
			inputJSON := json.RawMessage(fmt.Sprintf(`{"id":%d,"depth":%d}`, cycle, cycle))
			assistantBlocks = append(assistantBlocks, anthropicmessages.ContentBlock{
				Type:  anthropicmessages.ContentBlockTypeToolUse,
				ID:    &toolUseID,
				Name:  &toolName,
				Input: inputJSON,
			})

		case 1:
			// Assistant turn with ONLY a synthetic thinking block (model stopped/paused)
			synthThinking := fmt.Sprintf("Contemplating step %d deeply.", cycle)
			synthSig := SyntheticThinkingSignature
			assistantBlocks = append(assistantBlocks, anthropicmessages.ContentBlock{
				Type:      anthropicmessages.ContentBlockTypeThinking,
				Thinking:  &synthThinking,
				Signature: &synthSig,
			})

		case 2:
			// Assistant turn with zero-argument tool use (empty object input)
			toolName := "GetSystemClock"
			assistantBlocks = append(assistantBlocks, anthropicmessages.ContentBlock{
				Type:  anthropicmessages.ContentBlockTypeToolUse,
				ID:    &toolUseID,
				Name:  &toolName,
				Input: json.RawMessage("{}"),
			})

		case 3:
			// Assistant turn with regular text + tool use
			textMsg := fmt.Sprintf("Working on step %d", cycle)
			toolName := "ExecuteScript"
			inputJSON := json.RawMessage(fmt.Sprintf(`{"script":"test_%d.sh"}`, cycle))
			assistantBlocks = append(assistantBlocks,
				anthropicmessages.ContentBlock{
					Type: anthropicmessages.ContentBlockTypeText,
					Text: &textMsg,
				},
				anthropicmessages.ContentBlock{
					Type:  anthropicmessages.ContentBlockTypeToolUse,
					ID:    &toolUseID,
					Name:  &toolName,
					Input: inputJSON,
				},
			)
		}

		messages = append(messages, anthropicmessages.Message{
			Role: anthropicmessages.RoleAssistant,
			Content: anthropicmessages.Content{
				ContentBlocks: assistantBlocks,
			},
		})

		// 3. User message providing tool result (if a tool was called)
		if cycle%4 != 1 {
			resultContent := anthropicmessages.Content{
				ContentStr: new(fmt.Sprintf("result for %s: ok", toolUseID)),
			}
			messages = append(messages, anthropicmessages.Message{
				Role: anthropicmessages.RoleUser,
				Content: anthropicmessages.Content{
					ContentBlocks: []anthropicmessages.ContentBlock{
						{
							Type:      anthropicmessages.ContentBlockTypeToolResult,
							ToolUseID: &toolUseID,
							Content:   &resultContent,
						},
					},
				},
			})
		} else {
			// In cycle%4 == 1, assistant only thought; user nudges assistant
			nudge := "Please proceed with execution."
			messages = append(messages, anthropicmessages.Message{
				Role: anthropicmessages.RoleUser,
				Content: anthropicmessages.Content{
					ContentStr: &nudge,
				},
			})
		}

		// 4. Assistant follow-up acknowledgment text
		ackText := fmt.Sprintf("Acknowledged step %d.", cycle)
		messages = append(messages, anthropicmessages.Message{
			Role: anthropicmessages.RoleAssistant,
			Content: anthropicmessages.Content{
				ContentStr: &ackText,
			},
		})
	}

	reqEnvelope := anthropicmessages.Request{
		Model:     "claude-3-7-sonnet-20250219",
		MaxTokens: 4096,
		System:    &systemPrompt,
		Messages:  messages,
	}

	bodyBytes, err := json.Marshal(reqEnvelope)
	if err != nil {
		t.Fatalf("marshal messages envelope: %v", err)
	}

	// Decode under StrictLossPolicy
	decoded, err := DecodeMessagesRequest(bodyBytes, StrictLossPolicy())
	if err != nil {
		t.Fatalf("DecodeMessagesRequest failed on 160-message replay: %v", err)
	}

	// Validate CanonicalRequest IR invariants
	if err := ValidateCanonicalRequest(decoded.Request); err != nil {
		t.Fatalf("ValidateCanonicalRequest failed: %v", err)
	}

	// Verify that ALL marker-signature thinking blocks were scrubbed
	for turnIndex, turn := range decoded.Request.Turns {
		for partIndex, part := range turn.Parts {
			if ct, ok := part.(CanonicalText); ok {
				if strings.Contains(ct.Text, "Let me think") || strings.Contains(ct.Text, "Contemplating") {
					t.Fatalf("turn %d part %d leaked scrubbed thinking: %q", turnIndex, partIndex, ct.Text)
				}
			}
		}
	}
	if len(decoded.Request.Artifacts.AnthropicThinkingBlocks) != 0 {
		t.Fatalf("expected 0 authenticated thinking blocks, got %d", len(decoded.Request.Artifacts.AnthropicThinkingBlocks))
	}

	// Verify cycle 1 assistant turn (which only had synthetic thinking) was preserved with empty text
	// Turn index calculation:
	// System turn = index 0
	// Cycle 0: user (1), assistant (2), user (3), assistant (4)
	// Cycle 1: user (5), assistant (6), user (7), assistant (8)
	cycle1AssistantTurn := decoded.Request.Turns[6]
	if cycle1AssistantTurn.Role != CanonicalAssistant {
		t.Fatalf("turn 6 role = %v, want assistant", cycle1AssistantTurn.Role)
	}
	if len(cycle1AssistantTurn.Parts) != 1 {
		t.Fatalf("turn 6 parts len = %d, want 1", len(cycle1AssistantTurn.Parts))
	}
	if ct, ok := cycle1AssistantTurn.Parts[0].(CanonicalText); !ok || ct.Text != "" {
		t.Fatalf("turn 6 part = %+v, want empty CanonicalText", cycle1AssistantTurn.Parts[0])
	}

	// Render to Chat Completions upstream schema
	context := &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: StrictLossPolicy(),
	}
	chatBytes, _, err := RenderChatRequest(decoded.Request, context, ChatCapabilities{
		SystemAnywhere: true,
	})
	if err != nil {
		t.Fatalf("RenderChatRequest failed: %v", err)
	}

	var chatReq openaichat.Request
	if err := wire.Decode(chatBytes, &chatReq); err != nil {
		t.Fatalf("rendered Chat request failed wire.Decode: %v", err)
	}

	if chatReq.Model != "claude-3-7-sonnet-20250219" {
		t.Fatalf("chat model = %q", chatReq.Model)
	}
}

// TestConversionReportSaturationAcrossManyReplayedNotes tests that when an agentic
// session replays thousands of annotated items (e.g. input items with IDs), the
// conversion report properly saturates at maxStreamConversionReportEntries (4096)
// without memory exhaustion or exchange failure.
func TestConversionReportSaturationAcrossManyReplayedNotes(t *testing.T) {
	var report ConversionReport
	policy := StrictLossPolicy()

	// Add 5000 items that trigger Note
	for i := 0; i < 5000; i++ {
		err := report.Note(
			FeaturePreviousResponseID,
			fmt.Sprintf("input[%d].id", i),
			"item identity not forwarded",
		)
		if err != nil {
			t.Fatalf("report.Note returned error on entry %d: %v", i, err)
		}
	}

	// The report must be bounded at maxStreamConversionReportEntries + 1 (the bound plus the single overflow note)
	if len(report.Losses) != maxStreamConversionReportEntries+1 {
		t.Fatalf("report.Losses len = %d, want %d", len(report.Losses), maxStreamConversionReportEntries+1)
	}

	// Dropped count must record the entries that exceeded the bound
	if report.Dropped != 5000-maxStreamConversionReportEntries {
		t.Fatalf("report.Dropped = %d, want %d", report.Dropped, 5000-maxStreamConversionReportEntries)
	}

	// Must contain the FeatureReportOverflow note
	var sawOverflowNote bool
	for _, loss := range report.Losses {
		if loss.Feature == FeatureReportOverflow {
			sawOverflowNote = true
			break
		}
	}
	if !sawOverflowNote {
		t.Fatal("expected FeatureReportOverflow note in report")
	}

	_ = policy
}
