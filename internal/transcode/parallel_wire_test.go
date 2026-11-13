package transcode

// parallel_tool_calls (Chat, Responses) and disable_parallel_tool_use (Messages)
// describe HOW calls may be made, so they are only meaningful when a call CAN be
// made. Emitting the field with no tool declared is an upstream 400 on every
// dialect that carries it, and an explicit "allowed" asserts the dialect default
// so it needs no field at all.

import (
	"encoding/json"
	"strings"
	"testing"
)

// chatWireParallel reads the rendered Chat body's parallel_tool_calls field.
func chatWireParallel(t *testing.T, rendered []byte) (present bool, value bool) {
	t.Helper()
	var raw struct {
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		Tools             []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(rendered, &raw); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	if raw.ParallelToolCalls == nil {
		return false, false
	}
	return true, *raw.ParallelToolCalls
}

// responsesWireParallel reads the rendered Responses body's parallel_tool_calls.
func responsesWireParallel(t *testing.T, rendered []byte) (present bool, value bool) {
	t.Helper()
	var raw struct {
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		Tools             []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(rendered, &raw); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	if raw.ParallelToolCalls == nil {
		return false, false
	}
	return true, *raw.ParallelToolCalls
}

// TestParallelToolCallsIsNeverEmittedWithoutTools proves both OpenAI targets
// omit the field when the tool list is empty: the field alone is rejected.
func TestParallelToolCallsIsNeverEmittedWithoutTools(t *testing.T) {
	for _, value := range []bool{true, false} {
		request := parallelRequest(value, nil, nil)
		t.Run(parallelName(value), func(t *testing.T) {
			rendered, report, err := RenderChatRequest(request, testExchangeContext(),
				ChatCapabilities{ParallelToolCalls: true})
			if err != nil {
				t.Fatalf("chat render: %v", err)
			}
			if present, _ := chatWireParallel(t, rendered); present {
				t.Fatalf("chat emitted parallel_tool_calls with zero tools: %s", rendered)
			}
			if !reportHasFeature(report, FeatureParallelToolCalls) {
				t.Fatalf("chat recorded no Note for the inert setting: %+v", report.Losses)
			}

			rendered, report, err = RenderResponsesRequest(request, testExchangeContext())
			if err != nil {
				t.Fatalf("responses render: %v", err)
			}
			if present, _ := responsesWireParallel(t, rendered); present {
				t.Fatalf("responses emitted parallel_tool_calls with zero tools: %s", rendered)
			}
			if !reportHasFeature(report, FeatureParallelToolCalls) {
				t.Fatalf("responses recorded no Note for the inert setting: %+v", report.Losses)
			}
		})
	}
}

// TestParallelToolCallsIsNeverEmittedForANoneChoice proves the second inert
// shape: a choice that forbids every call already satisfies the setting.
func TestParallelToolCallsIsNeverEmittedForANoneChoice(t *testing.T) {
	none := &CanonicalToolChoice{Mode: "none"}
	// The Responses target requires an explicit strict on its function tools,
	// which a hand-built CanonicalTool does not carry.
	responsesContext := testExchangeContext()
	responsesContext.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolSchemaStrictness: {},
	}}
	for _, value := range []bool{true, false} {
		request := parallelRequest(value, oneTool(), none)
		t.Run(parallelName(value), func(t *testing.T) {
			rendered, _, err := RenderChatRequest(request, testExchangeContext(),
				ChatCapabilities{ParallelToolCalls: true})
			if err != nil {
				t.Fatalf("chat render: %v", err)
			}
			if present, _ := chatWireParallel(t, rendered); present {
				t.Fatalf("chat emitted parallel_tool_calls for a none choice: %s", rendered)
			}
			rendered, _, err = RenderResponsesRequest(request, responsesContext)
			if err != nil {
				t.Fatalf("responses render: %v", err)
			}
			if present, _ := responsesWireParallel(t, rendered); present {
				t.Fatalf("responses emitted parallel_tool_calls for a none choice: %s", rendered)
			}
		})
	}
}

// TestParallelToolCallsIsCarriedWhenACallCanHappen proves the field survives
// where it means something: a disable with a real tool, and an explicit allowed
// with a real tool.
func TestParallelToolCallsIsCarriedWhenACallCanHappen(t *testing.T) {
	request := parallelRequest(false, oneTool(), nil)
	rendered, _, err := RenderChatRequest(request, testExchangeContext(),
		ChatCapabilities{ParallelToolCalls: true})
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if present, value := chatWireParallel(t, rendered); !present || value {
		t.Fatalf("chat parallel_tool_calls = (present %v, %v), want an explicit false: %s",
			present, value, rendered)
	}

	request = parallelRequest(true, oneTool(), nil)
	rendered, _, err = RenderChatRequest(request, testExchangeContext(),
		ChatCapabilities{ParallelToolCalls: true})
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if present, value := chatWireParallel(t, rendered); !present || !value {
		t.Fatalf("chat parallel_tool_calls = (present %v, %v), want an explicit true: %s",
			present, value, rendered)
	}

	// The Responses target requires an explicit strict on its function tools,
	// which a hand-built CanonicalTool does not carry: approve that decision so
	// this test stays about the parallel-tools field.
	request = parallelRequest(false, oneTool(), nil)
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureToolSchemaStrictness: {}}}
	rendered, _, err = RenderResponsesRequest(request, context)
	if err != nil {
		t.Fatalf("responses render: %v", err)
	}
	if present, value := responsesWireParallel(t, rendered); !present || value {
		t.Fatalf("responses parallel_tool_calls = (present %v, %v), want an explicit false: %s",
			present, value, rendered)
	}
}

// TestChatWithdrawnCapabilityOmitsTheField proves the capability still decides
// chat carriage: withdrawn, a real disable is the policy-gated loss and no field
// is emitted.
func TestChatWithdrawnCapabilityOmitsTheField(t *testing.T) {
	request := parallelRequest(false, oneTool(), nil)
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureParallelToolCalls: {}}}
	rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if present, _ := chatWireParallel(t, rendered); present {
		t.Fatalf("chat emitted the field with the capability withdrawn: %s", rendered)
	}
	var losses int
	for _, loss := range report.Losses {
		if loss.Feature == FeatureParallelToolCalls && loss.Kind == LossRecord {
			losses++
		}
	}
	if losses != 1 {
		t.Fatalf("policy-gated parallel_tool_calls losses = %d, want 1: %+v", losses, report.Losses)
	}
}

// TestMessagesInventsNoToolChoiceForAnAllowedSetting proves the Messages
// renderer does not synthesize a tool_choice the client never sent just to carry
// an "allowed" setting: that value asserts the dialect default.
func TestMessagesInventsNoToolChoiceForAnAllowedSetting(t *testing.T) {
	request := parallelRequest(true, oneTool(), nil)
	rendered, _, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	if strings.Contains(string(rendered), "tool_choice") {
		t.Fatalf("messages invented a tool_choice for an allowed setting: %s", rendered)
	}
}

// parallelName names a parallel-tools subtest.
func parallelName(value bool) string {
	if value {
		return "allowed"
	}
	return "disabled"
}
