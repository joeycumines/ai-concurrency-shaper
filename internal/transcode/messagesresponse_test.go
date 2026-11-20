package transcode

import (
	"errors"
	"testing"
)

func decodeMessagesBody(t *testing.T, body string, policy LossPolicy) (CanonicalResponse, ConversionReport, error) {
	t.Helper()
	return DecodeMessagesResponse([]byte(body), policy)
}

func TestDecodeMessagesResponseKeepsBlockOrder(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[` +
		`{"type":"text","text":"a"},` +
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}},` +
		`{"type":"text","text":"b"}],` +
		`"stop_reason":"tool_use","stop_sequence":null,` +
		`"usage":{"input_tokens":3,"output_tokens":2},` +
		`"cost":0.01}`
	resp, _, err := decodeMessagesBody(t, body, LossPolicy{})
	if err != nil {
		t.Fatalf("DecodeMessagesResponse: %v", err)
	}
	if resp.Status != CanonicalResponseCompleted || resp.Stop.Reason != CanonicalStopToolUse {
		t.Fatalf("status %q stop %+v", resp.Status, resp.Stop)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want 3 in block order", len(resp.Items))
	}
	if first, ok := resp.Items[0].(*CanonicalMessageItem); !ok || len(first.Parts) != 1 {
		t.Fatalf("item 0 = %#v, want a text message", resp.Items[0])
	}
	if _, ok := resp.Items[1].(*CanonicalFunctionCallItem); !ok {
		t.Fatalf("item 1 = %#v, want a function call", resp.Items[1])
	}
	if last, ok := resp.Items[2].(*CanonicalMessageItem); !ok || len(last.Parts) != 1 {
		t.Fatalf("item 2 = %#v, want a text message", resp.Items[2])
	}
}

// TestDecodeMessagesResponseListingKeepsTurnBoundary proves a dropped MCP
// tool listing does not split adjacent text blocks into separate messages.
func TestDecodeMessagesResponseListingKeepsTurnBoundary(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[` +
		`{"type":"text","text":"Hello"},` +
		`{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[]},` +
		`{"type":"text","text":" world"}],` +
		`"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
	resp, report, err := decodeMessagesBody(t, body, LossPolicy{})
	if err != nil {
		t.Fatalf("DecodeMessagesResponse: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want one message item containing both text parts", len(resp.Items))
	}
	item, ok := resp.Items[0].(*CanonicalMessageItem)
	if !ok || len(item.Parts) != 2 {
		t.Fatalf("item 0 = %#v, want one message with two text parts", resp.Items[0])
	}
	if !reportHasFeature(report, FeatureAnthropicToolChanges) {
		t.Fatalf("dropped listing produced no note: %+v", report.Losses)
	}
}

// TestDecodeMessagesResponseListingToleratesProviderExtensions proves an
// unmodeled field in a dropped MCP tool listing does not fail the exchange.
// The listing carries no client-visible content, so a provider extension on
// this subject-to-change response surface must be skipped, not fatal.
func TestDecodeMessagesResponseListingToleratesProviderExtensions(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[` +
		`{"type":"text","text":"Hello"},` +
		`{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":` +
		`[{"name":"echo","input_schema":{"type":"object"},"provider_extension":42}]},` +
		`{"type":"text","text":" world"}],` +
		`"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
	resp, report, err := decodeMessagesBody(t, body, LossPolicy{})
	if err != nil {
		t.Fatalf("DecodeMessagesResponse rejected a provider extension on a dropped listing: %v", err)
	}
	item, ok := resp.Items[0].(*CanonicalMessageItem)
	if !ok || len(item.Parts) != 2 {
		t.Fatalf("item 0 = %#v, want one message with two text parts", resp.Items[0])
	}
	if !reportHasFeature(report, FeatureAnthropicToolChanges) {
		t.Fatalf("dropped listing produced no note: %+v", report.Losses)
	}
}

func TestDecodeMessagesResponseStopReasons(t *testing.T) {
	tests := []struct {
		name       string
		stop       string
		status     CanonicalResponseStatus
		reason     CanonicalStopReason
		incomplete string
	}{
		{name: "end_turn completes", stop: `"end_turn"`, status: CanonicalResponseCompleted, reason: CanonicalStopEndTurn},
		{name: "max_tokens is incomplete", stop: `"max_tokens"`, status: CanonicalResponseIncomplete, reason: CanonicalStopMaxTokens, incomplete: "max_output_tokens"},
		{name: "refusal is incomplete content_filter", stop: `"refusal"`, status: CanonicalResponseIncomplete, reason: CanonicalStopRefusal, incomplete: "content_filter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
				`"content":[{"type":"text","text":"hi"}],"stop_reason":` + tt.stop + `,"usage":{"input_tokens":1,"output_tokens":1}}`
			resp, _, err := decodeMessagesBody(t, body, LossPolicy{})
			if err != nil {
				t.Fatalf("DecodeMessagesResponse: %v", err)
			}
			if resp.Status != tt.status || resp.Stop.Reason != tt.reason || resp.IncompleteReason != tt.incomplete {
				t.Fatalf("status %q stop %q incomplete %q", resp.Status, resp.Stop.Reason, resp.IncompleteReason)
			}
		})
	}

	t.Run("pause_turn has no single-response form", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"pause_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		_, _, err := decodeMessagesBody(t, body, LossPolicy{})
		if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
			t.Fatalf("error = %v, want an unrepresentable refusal", err)
		}
	})

	t.Run("a missing stop_reason never succeeds", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
			`"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		if _, _, err := decodeMessagesBody(t, body, LossPolicy{}); err == nil {
			t.Fatal("response without stop_reason was accepted")
		}
	})
}

func TestDecodeMessagesResponseThinkingNeedsApproval(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"text","text":"answer"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	t.Run("refused when the loss is not approved", func(t *testing.T) {
		if _, _, err := decodeMessagesBody(t, body, LossPolicy{}); err == nil {
			t.Fatal("authenticated thinking passed without approval")
		}
	})

	t.Run("approved loss drops the block and records it", func(t *testing.T) {
		policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureAuthenticatedThinking: {}}}
		resp, report, err := decodeMessagesBody(t, body, policy)
		if err != nil {
			t.Fatalf("DecodeMessagesResponse: %v", err)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("items = %d, want only the answer", len(resp.Items))
		}
		if len(report.Losses) != 1 || report.Losses[0].Feature != FeatureAuthenticatedThinking {
			t.Fatalf("losses = %+v", report.Losses)
		}
	})
}

// geminiMessagesBody is the Gemini generateContent request used by the
// Gemini-client-over-Messages handler tests (messagesstreamupstream_test.go).
const geminiMessagesBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":64}}`
