package transcode

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// A discriminator probe must use the wire spelling before selecting a union
// arm. Otherwise an unknown TYPE extension can select a tolerant server arm
// and discard members of an ordinary client function.
func TestMessagesToolDiscriminatorUsesExactWireName(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicServerTools: {}}}
	for _, definition := range []string{
		`{"TYPE":"web_search_20250305","name":"db","input_schema":{"type":"object"},"defer_loading":true}`,
		`{"TYPE":"future_server_tool","name":"db","input_schema":{"type":"object"},"unknown_client_option":true}`,
		`{"type":"custom","TYPE":"web_search_20250305","name":"db","input_schema":{"type":"object"}}`,
	} {
		t.Run(definition, func(t *testing.T) {
			body := []byte(`{"model":"m","max_tokens":16,"tools":[` + definition + `],"messages":[{"role":"user","content":"hi"}]}`)
			_, err := DecodeMessagesRequest(body, policy)
			var malformed *wire.DecodeError
			if !errors.As(err, &malformed) || (malformed.Kind != wire.DecodeUnknownField && malformed.Kind != wire.DecodeDuplicateKey) {
				t.Fatalf("client function with unknown TYPE = %T: %v, want a field-name rejection before any server-tool loss", err, err)
			}
		})
	}
	// The canonical server type owns dispatch. A differently spelled provider
	// extension is ignored under the server definition's tolerant contract.
	body := []byte(`{"model":"m","max_tokens":16,"tools":[{"type":"web_search_20250305","TYPE":"custom","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`)
	result, err := DecodeMessagesRequest(body, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 0 || len(result.Report.Losses) != 1 || result.Report.Losses[0].Feature != FeatureAnthropicServerTools {
		t.Fatalf("canonical server tool was reclassified: tools=%+v report=%+v", result.Request.Tools, result.Report.Losses)
	}
}

func TestResponsesToolDiscriminatorUsesExactWireName(t *testing.T) {
	for _, definition := range []string{
		`{"TYPE":"web_search","name":"db"}`,
		`{"type":"function","TYPE":"web_search","name":"db","parameters":{"type":"object"},"strict":false}`,
	} {
		t.Run(definition, func(t *testing.T) {
			var tool openairesponses.Tool
			if err := json.Unmarshal([]byte(definition), &tool); err == nil {
				t.Fatalf("noncanonical discriminator selected a tolerant built-in arm: %+v", tool)
			}
		})
	}
	var tool openairesponses.Tool
	if err := json.Unmarshal([]byte(`{"type":"web_search","TYPE":"function","name":"search"}`), &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Type != "web_search" || tool.Name != "search" {
		t.Fatalf("canonical built-in type lost to an unknown extension: %+v", tool)
	}
}

func TestMessagesStrictUnionsRejectDiscriminatorAliases(t *testing.T) {
	for _, raw := range []string{
		`{"TYPE":"adaptive"}`,
		`{"type":"disabled","TYPE":"adaptive","display":null}`,
	} {
		var thinking anthropicmessages.ThinkingConfig
		if err := json.Unmarshal([]byte(raw), &thinking); err == nil {
			t.Fatalf("thinking union accepted %s as %+v", raw, thinking)
		}
	}
	for _, raw := range []string{
		`{"TYPE":"text","text":"hello"}`,
		`{"type":"text","TYPE":"mcp_tool_listing","text":"hello","mcp_server_name":"s","tools":[]}`,
	} {
		var block anthropicmessages.ContentBlock
		if err := json.Unmarshal([]byte(raw), &block); err == nil {
			t.Fatalf("content union accepted %s as %+v", raw, block)
		}
	}
}

func TestResponsesEventNameUsesCanonicalTypeAfterTolerantDecode(t *testing.T) {
	data := []byte(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"kept","logprobs":[],"TYPE":"response.failed"}`)
	if _, err := decodeResponsesSSEEvent(data); err != nil {
		t.Fatalf("canonical Responses event was rejected because of an unknown extension: %v", err)
	}
	if err := validateEventNameMatchesJSONType(SSEEvent{Event: "response.output_text.delta", Data: data}); err != nil {
		t.Fatalf("matching canonical event name was rejected: %v", err)
	}
	if err := validateEventNameMatchesJSONType(SSEEvent{Event: "response.failed", Data: data}); err == nil {
		t.Fatal("a name matching only the unknown TYPE extension was accepted")
	}
}
