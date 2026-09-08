package transcode

import (
	"strings"
	"testing"
)

// TestChatUsageTotalMismatchRelayed pins the disposition of a gateway total
// that is not the exact sum of prompt + completion (293640 vs 293360 + 221 on
// a real glm exchange): it must NOT fail the decode — the source values are
// relayed as-is — and the render records the mismatch as a
// usage_total_mismatch note naming the emitted counts.
func TestChatUsageTotalMismatchRelayed(t *testing.T) {
	body := []byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],` +
		`"usage":{"prompt_tokens":293360,"completion_tokens":221,"total_tokens":293640,` +
		`"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`)
	response, report, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, j6PermissivePolicy())
	if err != nil {
		t.Fatalf("inconsistent usage totals must not fail the decode: %v", err)
	}
	if !response.Usage.TotalKnown || response.Usage.TotalTokens != 293640 {
		t.Fatalf("source total must be relayed as-is: %+v", response.Usage)
	}
	if reportHasFeature(report, FeatureUsageTotalMismatch) {
		t.Fatalf("decode recorded the mismatch before the counts were emitted: %+v", report.Losses)
	}
	context := testExchangeContext()
	context.LossPolicy = j6PermissivePolicy()
	context.RequestedClientModel = "m"
	_, renderReport, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatalf("render must not reject the mismatch: %v", err)
	}
	mustMismatchNote(t, &renderReport, "chat usage total mismatch render")
}

func TestChatResponseToolCallsDuplicateKeysAndEmptyArguments(t *testing.T) {
	context := &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: j6PermissivePolicy(),
	}

	// Case 1: Duplicate keys in tool arguments
	bodyDup := []byte(`{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"query\":\"hello\",\"query\":\"world\"}"}}]}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)

	resDup, _, err := DecodeChatResponseWithPolicy(bodyDup, ChatCapabilities{}, j6PermissivePolicy())
	if err != nil {
		t.Fatalf("decode chat response with duplicate tool arguments failed: %v", err)
	}
	msgBytesDup, _, err := RenderMessagesResponse(resDup, context)
	if err != nil {
		t.Fatalf("render messages response failed on duplicate tool arguments: %v", err)
	}
	if !strings.Contains(string(msgBytesDup), `"world"`) {
		t.Fatalf("expected last-key-wins world in rendered messages: %s", string(msgBytesDup))
	}
	respBytesDup, _, err := RenderResponsesResponse(resDup, context)
	if err != nil {
		t.Fatalf("render responses response failed on duplicate tool arguments: %v", err)
	}
	if !strings.Contains(string(respBytesDup), `"arguments":"{\"query\":\"hello\",\"query\":\"world\"}"`) {
		t.Fatalf("expected raw string preserved in rendered responses: %s", string(respBytesDup))
	}

	// Case 2: Empty string arguments (no-arg tool call from model)
	bodyEmpty := []byte(`{"id":"c2","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_2","type":"function","function":{"name":"get_time","arguments":""}}]}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)

	resEmpty, _, err := DecodeChatResponseWithPolicy(bodyEmpty, ChatCapabilities{}, j6PermissivePolicy())
	if err != nil {
		t.Fatalf("decode chat response with empty tool arguments failed: %v", err)
	}
	msgBytesEmpty, _, err := RenderMessagesResponse(resEmpty, context)
	if err != nil {
		t.Fatalf("render messages response failed on empty tool arguments: %v", err)
	}
	if !strings.Contains(string(msgBytesEmpty), `"input":{}`) {
		t.Fatalf("expected empty object input in rendered messages: %s", string(msgBytesEmpty))
	}
	respBytesEmpty, _, err := RenderResponsesResponse(resEmpty, context)
	if err != nil {
		t.Fatalf("render responses response failed on empty tool arguments: %v", err)
	}
	if !strings.Contains(string(respBytesEmpty), `"arguments":""`) {
		t.Fatalf("expected empty string arguments preserved in rendered responses: %s", string(respBytesEmpty))
	}
}
