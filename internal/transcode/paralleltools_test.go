package transcode

// disable_parallel_tool_use is the Anthropic spelling of the canonical
// parallel-tools setting, and it is a BOOLEAN, not a flag: an explicit false
// asserts that parallel calls ARE allowed. Decoding it only when true destroys
// the false case, which then bypasses a target's parallel-tool-calls gate —
// the same "decide before the target is known" class as decoding top_k.
//
// The IR therefore carries the setting in both directions, and the Messages
// target re-emits it in both. The two INERT cases (a request declaring no tools,
// and a {type:"none"} choice that forbids every call already) are Notes rather
// than default-deny losses: nothing is lost, and a key that is not in the
// default set would refuse a request whose client setting is already satisfied.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// messagesBodyWithChoice builds a Messages request body whose tool_choice and
// tools are supplied by the caller.
func messagesBodyWithChoice(choice, tools string) []byte {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"tool_choice":` + choice
	if tools != "" {
		body += `,"tools":` + tools
	}
	return []byte(body + `}`)
}

// TestDisableParallelToolUseFalseIsCarried proves both values reach the IR, so a
// target that cannot carry the setting still sees it and can take its own
// decision.
func TestDisableParallelToolUseFalseIsCarried(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "false", want: true}, // parallel calls ALLOWED
		{value: "true", want: false}, // disabled
	} {
		t.Run(tc.value, func(t *testing.T) {
			request := decodeMessagesForTest(t, string(messagesBodyWithChoice(
				`{"type":"auto","disable_parallel_tool_use":`+tc.value+`}`, "")))
			if request.ParallelTools == nil {
				t.Fatalf("disable_parallel_tool_use:%s was dropped at decode", tc.value)
			}
			if *request.ParallelTools != tc.want {
				t.Fatalf("ParallelTools = %v, want %v", *request.ParallelTools, tc.want)
			}
		})
	}

	// An absent flag stays absent: the client asserted nothing.
	request := decodeMessagesForTest(t, string(messagesBodyWithChoice(`{"type":"auto"}`, "")))
	if request.ParallelTools != nil {
		t.Fatalf("an absent disable_parallel_tool_use invented ParallelTools = %v", *request.ParallelTools)
	}
}

// TestMessagesReEmitsBothParallelSettings proves the Messages target carries the
// setting in both directions on a choice that can hold it.
func TestMessagesReEmitsBothParallelSettings(t *testing.T) {
	const tools = `[{"name":"f","input_schema":{"type":"object"}}]`
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "true", want: true},
		{value: "false", want: false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			request := decodeMessagesForTest(t, string(messagesBodyWithChoice(
				`{"type":"auto","disable_parallel_tool_use":`+tc.value+`}`, tools)))
			rendered, _, err := RenderMessagesRequest(request, testExchangeContext())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var out anthropicmessages.Request
			if err := strictDecode(rendered, &out); err != nil {
				t.Fatalf("rendered: %v\n%s", err, rendered)
			}
			if out.ToolChoice == nil || out.ToolChoice.DisableParallelToolUse == nil {
				t.Fatalf("tool_choice = %+v, want disable_parallel_tool_use carried", out.ToolChoice)
			}
			if *out.ToolChoice.DisableParallelToolUse != tc.want {
				t.Fatalf("disable_parallel_tool_use = %v, want %v",
					*out.ToolChoice.DisableParallelToolUse, tc.want)
			}
		})
	}
}

// TestMessagesParallelFlagIsNotAttachedToNoneChoice proves the flag is not put on
// a union arm that forbids every call already: a "none" choice is a complete
// statement, and adding a parallel-call modifier to it would be a union the
// upstream never asked for.
func TestMessagesParallelFlagIsNotAttachedToNoneChoice(t *testing.T) {
	const tools = `[{"name":"f","input_schema":{"type":"object"}}]`
	request := decodeMessagesForTest(t, string(messagesBodyWithChoice(
		`{"type":"none","disable_parallel_tool_use":true}`, tools)))
	rendered, _, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var out anthropicmessages.Request
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered: %v\n%s", err, rendered)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "none" {
		t.Fatalf("tool_choice = %+v, want the none choice", out.ToolChoice)
	}
	if out.ToolChoice.DisableParallelToolUse != nil {
		t.Fatalf("a none choice carried disable_parallel_tool_use = %v",
			*out.ToolChoice.DisableParallelToolUse)
	}
}

// TestMessagesToolLessDisableIsANote proves the inert case is observable without
// being a policy decision. With no tools declared the model cannot emit a tool
// call at all, so the client's disable is already satisfied; a default-deny key
// here would refuse a request that loses nothing.
func TestMessagesToolLessDisableIsANote(t *testing.T) {
	request := decodeMessagesForTest(t, string(messagesBodyWithChoice(
		`{"type":"auto","disable_parallel_tool_use":true}`, "")))
	// Strict policy: no approval is possible, yet the request is served.
	_, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("a tool-less disable must not refuse a default deployment: %v", err)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature != FeatureParallelToolCalls {
			continue
		}
		found = true
		if loss.Kind != NoteRecord {
			t.Fatalf("the inert tool-less disable recorded a %v: %+v", loss.Kind, loss)
		}
	}
	if !found {
		t.Fatalf("report = %+v, want the parallel_tool_calls Note", report.Losses)
	}
}

// TestGeminiToolLessDisableIsANote proves the same inertness argument holds for
// the Gemini target, where function-calling mode always allows parallel calls.
func TestGeminiToolLessDisableIsANote(t *testing.T) {
	disabled := false
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		ParallelTools: &disabled,
	}
	rendered, report, err := RenderGeminiRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("a disabled parallel setting must not refuse a default deployment: %v", err)
	}
	if !json.Valid(rendered) {
		t.Fatalf("rendered body is not JSON: %s", rendered)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature != FeatureParallelToolCalls {
			continue
		}
		found = true
		if loss.Kind != NoteRecord {
			t.Fatalf("the unreproducible disable recorded a %v: %+v", loss.Kind, loss)
		}
	}
	if !found {
		t.Fatalf("report = %+v, want the parallel_tool_calls Note", report.Losses)
	}
}

// TestChatParallelGateStillUsesTheCarriedValue proves the Chat target's gate sees
// a carried setting and distinguishes the cases that matter: an explicit
// parallel-allowed value costs nothing, an inert disable costs nothing, and a
// real disable on a mount that cannot do parallel calls is the policy-gated
// loss. The gate must not refuse traffic that loses nothing — that was the
// defect, not the behaviour.
func TestChatParallelGateStillUsesTheCarriedValue(t *testing.T) {
	// An explicit false (parallel ALLOWED) asserts the dialect default, so
	// omitting the field is truthful and nothing is lost.
	allowed := decodeMessagesForTest(t, string(messagesBodyWithChoice(
		`{"type":"auto","disable_parallel_tool_use":false}`, "")))
	if _, _, err := RenderChatRequest(allowed, testExchangeContext(), ChatCapabilities{}); err != nil {
		t.Fatalf("an explicit parallel-allowed setting was refused though omitting it means the same: %v", err)
	}

	// A disable with no tools is inert: the model cannot emit a call at all.
	inert := decodeMessagesForTest(t, string(messagesBodyWithChoice(
		`{"type":"auto","disable_parallel_tool_use":true}`, "")))
	if _, report, err := RenderChatRequest(inert, testExchangeContext(), ChatCapabilities{}); err != nil {
		t.Fatalf("a tool-less disable was refused though it is already satisfied: %v", err)
	} else if !reportHasFeature(report, FeatureParallelToolCalls) {
		t.Fatalf("report = %+v, want the inert parallel_tool_calls Note", report.Losses)
	}

	// A disable WITH tools is the real loss the operator can refuse.
	real := decodeMessagesForTest(t, string(messagesBodyWithChoice(
		`{"type":"auto","disable_parallel_tool_use":true}`,
		`[{"name":"f","input_schema":{"type":"object"}}]`)))
	_, _, err := RenderChatRequest(real, testExchangeContext(), ChatCapabilities{})
	if err == nil {
		t.Fatal("a real parallel-tools disable bypassed the chat capability gate")
	}
	if !strings.Contains(err.Error(), string(FeatureParallelToolCalls)) {
		t.Fatalf("err = %q, want it to name %s", err, FeatureParallelToolCalls)
	}
}
