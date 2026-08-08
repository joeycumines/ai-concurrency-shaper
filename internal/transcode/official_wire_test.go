package transcode

import (
	"testing"
)

// TestOfficialFunctionCallWire verifies the messages->responses stream
// direction accepts the official OpenAI function-call wire shapes: an
// output_item.added without a status and with empty arguments, and a
// function_call_arguments.done without a name (per the published function-
// calling guide). The exchange must convert to a tool_use block with the
// accumulated arguments and a tool_use stop reason.
func TestOfficialFunctionCallWire(t *testing.T) {
	state := newAnthropicResponsesStreamState(testStreamContext(), j6PermissivePolicy(), "resp_1", "m", 1)
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_1","output_index":0,"delta":"{\"city\":\"Tokyo\"}"}`,
		`{"type":"response.function_call_arguments.done","sequence_number":3,"item_id":"fc_1","output_index":0,"arguments":"{\"city\":\"Tokyo\"}"}`,
		`{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}`,
		`{"type":"response.completed","sequence_number":5,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}}`,
	}
	var all []AnthropicStreamEvent
	for _, raw := range events {
		event, err := decodeResponsesSSEEvent([]byte(raw))
		if err != nil {
			t.Fatalf("decode %q: %v", raw[:40], err)
		}
		batch, err := state.Convert(event)
		if err != nil {
			t.Fatalf("convert %q: %v", raw[:40], err)
		}
		all = append(all, batch...)
	}
	stop := ""
	for _, e := range all {
		if e.Delta != nil && e.Delta.StopReason != nil {
			stop = string(*e.Delta.StopReason)
		}
	}
	if stop != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", stop)
	}
	var sawToolUseBlock bool
	var accumulated string
	for _, e := range all {
		if e.ContentBlock != nil && e.ContentBlock.Type == AnthropicContentBlockTypeToolUse {
			sawToolUseBlock = true
		}
		if e.Delta != nil && e.Delta.PartialJSON != nil {
			accumulated += *e.Delta.PartialJSON
		}
	}
	if !sawToolUseBlock {
		t.Fatal("no tool_use block emitted")
	}
	if accumulated != `{"city":"Tokyo"}` {
		t.Fatalf("accumulated arguments = %q", accumulated)
	}
}
