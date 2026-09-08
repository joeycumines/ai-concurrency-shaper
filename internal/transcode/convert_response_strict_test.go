package transcode

import (
	"testing"
)

// TestChatUsageTotalMismatchRelayed pins the CC-USAGE-ARITHMETIC disposition
// (operator-observed 2026-09-08): a gateway total that is not the exact sum
// of prompt + completion (293640 vs 293360 + 221 on a real glm exchange)
// must NOT fail the decode — the source values are relayed as-is and the
// mismatch is recorded as a usage_total_mismatch note.
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
	found := false
	for _, l := range report.Losses {
		if l.Feature == FeatureUsageTotalMismatch {
			found = true
		}
	}
	if !found {
		t.Fatalf("usage_total_mismatch note missing: %+v", report.Losses)
	}
}
