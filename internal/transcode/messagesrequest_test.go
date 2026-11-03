package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// renderGeminiToMessages decodes a Gemini body for the Messages upstream and
// renders the Anthropic document, returning it as a generic map.
func renderGeminiToMessages(
	t *testing.T,
	body string,
	context *ExchangeContext,
) (map[string]any, ConversionReport, error) {
	t.Helper()
	result, _, err := DecodeGeminiRequest([]byte(body), LossPolicy{}, UpstreamMessages)
	if err != nil {
		t.Fatalf("DecodeGeminiRequest: %v", err)
	}
	raw, report, err := RenderMessagesRequest(result.Request, context)
	if err != nil {
		return nil, report, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rendered request is not JSON: %v\n%s", err, raw)
	}
	return doc, report, nil
}

func messagesContext() *ExchangeContext {
	return &ExchangeContext{RequestedClientModel: "gemini-x", UpstreamModel: "claude-x"}
}

func TestRenderMessagesRequestMaxTokens(t *testing.T) {
	t.Run("the request's output limit is used as max_tokens", func(t *testing.T) {
		body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":128}}`
		doc, report, err := renderGeminiToMessages(t, body, messagesContext())
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := doc["max_tokens"]; got != float64(128) {
			t.Fatalf("max_tokens = %v, want 128", got)
		}
		if len(report.Losses) != 0 {
			t.Fatalf("losses = %+v, want none", report.Losses)
		}
		if doc["model"] != "claude-x" {
			t.Fatalf("model = %v, want the upstream model", doc["model"])
		}
	})

	t.Run("the model table max_output is a noted fallback", func(t *testing.T) {
		body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		maxOutput := 4096
		context := messagesContext()
		context.ModelMaxOutputTokens = &maxOutput
		doc, report, err := renderGeminiToMessages(t, body, context)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := doc["max_tokens"]; got != float64(4096) {
			t.Fatalf("max_tokens = %v, want 4096", got)
		}
		if len(report.Losses) != 1 {
			t.Fatalf("losses = %+v, want one note", report.Losses)
		}
		note := report.Losses[0]
		if note.Kind != NoteRecord || note.Feature != FeatureGeminiMaxTokensFromModelTable {
			t.Fatalf("note = %+v, want the max_output note", note)
		}
	})

	t.Run("a request with no output limit and no table fact is refused", func(t *testing.T) {
		body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		_, _, err := renderGeminiToMessages(t, body, messagesContext())
		if err == nil || !strings.Contains(err.Error(), "maxOutputTokens is required") {
			t.Fatalf("error = %v, want the required max_tokens refusal", err)
		}
	})
}

func TestRenderMessagesRequestTurnsKeepOrderAndSystem(t *testing.T) {
	body := `{"systemInstruction":{"parts":[{"text":"be brief"}]},` +
		`"contents":[` +
		`{"role":"user","parts":[{"text":"first"}]},` +
		`{"role":"model","parts":[{"text":"reply"}]},` +
		`{"role":"user","parts":[{"text":"second"}]}],` +
		`"generationConfig":{"maxOutputTokens":32}}`
	doc, _, err := renderGeminiToMessages(t, body, messagesContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// A single system text serializes as the string form of the field.
	if system := doc["system"]; system != "be brief" {
		t.Fatalf("system = %v, want the system text", system)
	}
	messages, ok := doc["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("messages = %v, want three turns", doc["messages"])
	}
	wantRoles := []string{"user", "assistant", "user"}
	for i, want := range wantRoles {
		if got := messages[i].(map[string]any)["role"]; got != want {
			t.Fatalf("message %d role = %v, want %s", i, got, want)
		}
	}
}

func TestRenderMessagesRequestToolRoundTrip(t *testing.T) {
	body := `{"contents":[` +
		`{"role":"user","parts":[{"text":"look it up"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"x"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"lookup","response":{"output":"found"}}}]}],` +
		`"generationConfig":{"maxOutputTokens":32}}`
	doc, _, err := renderGeminiToMessages(t, body, messagesContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	messages := doc["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v, want three turns", messages)
	}
	call := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if call["type"] != "tool_use" || call["id"] != "call_1" || call["name"] != "lookup" {
		t.Fatalf("tool_use block = %v", call)
	}
	result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result block = %v", result)
	}
}

func TestRenderMessagesRequestRefusesTextBeforeToolResult(t *testing.T) {
	// Messages requires tool_result blocks before any other block of a user
	// message, so a text part ahead of the result is refused, not reordered.
	body := `{"contents":[` +
		`{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"x"}}}]},` +
		`{"role":"user","parts":[{"text":"context"},{"functionResponse":{"id":"call_1","name":"lookup","response":{"output":"ok"}}}]}],` +
		`"generationConfig":{"maxOutputTokens":32}}`
	_, _, err := renderGeminiToMessages(t, body, messagesContext())
	if err == nil || !strings.Contains(err.Error(), "must precede") {
		t.Fatalf("error = %v, want the tool_result ordering refusal", err)
	}
}
