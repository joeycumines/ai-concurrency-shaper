package transcode

import (
	"bytes"
	"testing"
)

func TestMessagesCaseDistinctSchemaPropertiesSurvive(t *testing.T) {
	const body = `{"model":"m","max_tokens":16,"tools":[{"name":"case_sensitive","input_schema":{"type":"object","properties":{"ID":{"type":"string"},"id":{"type":"number"}}}}],"messages":[{"role":"user","content":"hi"}]}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	for _, property := range []string{`"ID":{"type":"string"}`, `"id":{"type":"number"}`} {
		if !bytes.Contains(encoded, []byte(property)) {
			t.Fatalf("schema property lost: %s", encoded)
		}
	}
}

func TestMessagesResponseCaseDistinctToolArgumentsSurvive(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"tool_1","name":"run","input":{"PATH":"/usr/bin","Path":"/tmp"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`
	response, _, err := DecodeMessagesResponse([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = usageOnlyPolicy()
	encoded, _, err := RenderGeminiResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"args":{"PATH":"/usr/bin","Path":"/tmp"}`)) {
		t.Fatalf("case-distinct tool arguments changed: %s", encoded)
	}
}
