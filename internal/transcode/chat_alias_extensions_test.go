package transcode

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestChatStreamUnknownCaseAliasesDoNotOverrideCanonicalFields(t *testing.T) {
	for _, suffix := range []string{
		`,"ID":"other","MODEL":42`,
		`,"ERROR":{"message":"provider extension"}`,
		`,"USAGE":{"prompt_tokens":null}`,
	} {
		body := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"kept","CONTENT":"discarded"},"finish_reason":"stop","FINISH_REASON":"function_call"}]` + suffix + `}`
		chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)})
		if err != nil {
			t.Fatalf("unknown provider extensions caused failure: %v", err)
		}
		if chunk.ID != "chatcmpl-1" || chunk.Model != "m" || chunk.Usage != nil || len(chunk.Choices) != 1 {
			t.Fatalf("canonical envelope was overwritten: %+v", chunk)
		}
		choice := chunk.Choices[0]
		if choice.Delta == nil || choice.Delta.Content == nil || *choice.Delta.Content != "kept" || choice.FinishReason == nil || *choice.FinishReason != "stop" {
			t.Fatalf("canonical content/terminal overwritten in the second decode: %+v", choice)
		}
	}
	_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(`{"error":{"message":"real failure"}}`)})
	var failure *StreamConversionError
	if !errors.As(err, &failure) || failure.Provenance != ProvenanceUpstreamBodyError {
		t.Fatalf("canonical error did not fail as an upstream error: %T: %v", err, err)
	}
}

func TestChatStreamUsageNullProbeUsesExactWireName(t *testing.T) {
	for _, tc := range []struct {
		name, usage, extension string
		wantError              bool
		wantDerived            string
	}{
		{"unknown null cannot corrupt totals", `{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`, `{"prompt_tokens":null}`, false, ""},
		{"unknown null cannot prevent derivation", `{"completion_tokens":3,"total_tokens":5}`, `{"prompt_tokens":null}`, false, "prompt_tokens"},
		{"unknown valid cannot hide null", `{"prompt_tokens":null,"completion_tokens":3,"total_tokens":5}`, `{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"object":"chat.completion.chunk","choices":[],"usage":` + tc.usage + `,"USAGE":` + tc.extension + `}`
			chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)})
			if tc.wantError {
				var corrupt *UpstreamWireError
				if !errors.As(err, &corrupt) {
					t.Fatalf("illegal canonical null was hidden by an extension: chunk=%+v error=%v", chunk, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if chunk.Usage == nil || chunk.Usage.PromptTokens != 2 || chunk.Usage.CompletionTokens != 3 || chunk.Usage.TotalTokens != 5 || chunk.DerivedUsageTotal != tc.wantDerived {
				t.Fatalf("usage or derivation changed: %+v; derived=%q", chunk.Usage, chunk.DerivedUsageTotal)
			}
		})
	}
}

func TestLegacyFunctionCallUnknownCaseAliasesAreIgnored(t *testing.T) {
	for _, raw := range []string{
		`{"name":"db","arguments":"{\"x\":1}","NAME":"other","ARGUMENTS":"{}"}`,
		`{"name":"db","arguments":"{\"x\":1}","NAME":null,"ARGUMENTS":42}`,
	} {
		name, arguments, hasContent, empty, err := parseLegacyFunctionCallFragment(json.RawMessage(raw))
		if err != nil || name == nil || *name != "db" || arguments == nil || *arguments != `{"x":1}` || !hasContent || empty {
			t.Fatalf("legacy fragment canonical fields changed: name=%v args=%v content=%v empty=%v error=%v", name, arguments, hasContent, empty, err)
		}
		stream := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","function_call":` + raw + `},"finish_reason":"function_call"}]}`
		chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(stream)})
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Choices) != 1 || chunk.Choices[0].Delta == nil || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
			t.Fatalf("legacy stream did not produce one call: %+v", chunk)
		}
		call := chunk.Choices[0].Delta.ToolCalls[0]
		if call.Function.Name == nil || *call.Function.Name != "db" || call.Function.Arguments != `{"x":1}` {
			t.Fatalf("legacy stream call was overwritten: %+v", call)
		}
		body := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"function_call":` + raw + `},"finish_reason":"function_call"}]}`
		response, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var found *CanonicalFunctionCallItem
		for _, item := range response.Items {
			if call, ok := item.(*CanonicalFunctionCallItem); ok {
				if found != nil {
					t.Fatal("legacy nonstream response produced more than one call")
				}
				found = call
			}
		}
		if found == nil || found.Name != "db" || found.Arguments.Raw != `{"x":1}` {
			t.Fatalf("legacy nonstream call was overwritten: %+v", found)
		}
	}
	name, arguments, content, _, err := parseLegacyFunctionCallFragment(json.RawMessage(`{"NAME":"not_a_name","arguments":"{}"}`))
	if err != nil || name != nil || arguments == nil || *arguments != "{}" || !content {
		t.Fatalf("alias supplied a missing name: name=%v args=%v content=%v error=%v", name, arguments, content, err)
	}
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"function_call":{"NAME":"forged","arguments":"{}"}},"finish_reason":"function_call"}]}`)
	if _, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy(), nil); err == nil {
		t.Fatal("unknown NAME satisfied the nonstream required function name")
	}
}
