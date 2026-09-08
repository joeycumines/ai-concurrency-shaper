package transcode

import (
	"strings"
	"testing"
)

// TestDecodeResponsesRequestNullEncryptedContent pins the CODEX-NULL fix
// (operator-observed 2026-09-08): codex-tui sends reasoning input items with
// encrypted_content and reasoning_text.signature explicitly null (JSON null,
// not absent). The strict client decode rejected the null on the modeled
// string fields and 400'd the exchange. Null on a provider-opaque optional
// pass-through field means "absent": the decode must accept it and the
// pass-through must omit the key.
func TestDecodeResponsesRequestNullEncryptedContent(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"t","signature":null}],"encrypted_content":null},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"stream":false}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("explicit null on optional pass-through fields must decode: %v", err)
	}
	// The user turn must survive behind the reasoning item.
	userTurns := 0
	for _, turn := range result.Request.Turns {
		if turn.Role == CanonicalUser {
			userTurns++
		}
	}
	if userTurns == 0 {
		t.Fatal("user turn missing after the reasoning item")
	}
	// The reasoning artifact re-marshal must omit the null keys entirely.
	found := false
	for _, raw := range result.Request.Artifacts.ResponsesReasoningItems {
		if strings.Contains(string(raw), `"encrypted_content"`) || strings.Contains(string(raw), `"signature"`) {
			t.Fatalf("null optional keys must be omitted from the pass-through artifact: %s", raw)
		}
		if strings.Contains(string(raw), `"type":"reasoning"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("reasoning artifact item missing")
	}
}
