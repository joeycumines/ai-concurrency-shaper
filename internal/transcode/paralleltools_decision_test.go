package transcode

// An explicit parallel-tool-calls setting is a client intent, and each target
// loses it differently:
//
//   - Chat carries it when the capability is granted; with the capability
//     withdrawn the field is omitted, which is a REAL loss for `false` (the
//     dialect default is "parallel allowed") and NOTHING for `true` (omitting it
//     asserts the same thing).
//   - Gemini has no disable at all: it always allows parallel calls, so `false`
//     is a real loss and `true` is already satisfied.
//   - Messages carries either value on the choice.
//
// Two conditions make the setting inert on EVERY target, because no tool call
// can happen at all: the request declares no tools, or its choice forbids every
// call. Those are Notes: a policy-gated loss there would refuse a request whose
// client intent is already satisfied.
//
// The distinction matters operationally: `-transcode-allow-loss
// '!parallel_tool_calls'` must actually refuse a request whose disable the
// upstream cannot honour, and must keep serving a request whose setting is
// already satisfied.

import (
	"strings"
	"testing"
)

// parallelRequest builds a request with an explicit parallel-tools setting and
// the given tools / choice.
func parallelRequest(parallel bool, tools []CanonicalTool, choice *CanonicalToolChoice) CanonicalRequest {
	return CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Tools:         tools,
		ToolChoice:    choice,
		ParallelTools: &parallel,
	}
}

// oneTool is a single portable function tool.
func oneTool() []CanonicalTool {
	return []CanonicalTool{{Name: "f", JSONSchema: []byte(`{"type":"object"}`)}}
}

// parallelTargets maps each target to a renderer with the parallel-tools
// capability WITHDRAWN, which is the case where Chat has to decide.
func parallelTargets() map[string]func(CanonicalRequest, *ExchangeContext) (ConversionReport, error) {
	return map[string]func(CanonicalRequest, *ExchangeContext) (ConversionReport, error){
		"chat": func(request CanonicalRequest, context *ExchangeContext) (ConversionReport, error) {
			_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
			return report, err
		},
		"gemini": func(request CanonicalRequest, context *ExchangeContext) (ConversionReport, error) {
			_, report, err := RenderGeminiRequest(request, context)
			return report, err
		},
	}
}

// TestParallelDisableWithToolsIsAPolicyGatedLoss proves a `false` setting that a
// target cannot honour is a real, refusable loss: the client asked for
// sequential calls and the upstream may emit parallel ones. A Note would leave
// the operator no way to refuse it.
func TestParallelDisableWithToolsIsAPolicyGatedLoss(t *testing.T) {
	for name, render := range parallelTargets() {
		t.Run(name, func(t *testing.T) {
			request := parallelRequest(false, oneTool(), nil)

			// Strict policy refuses: the intent cannot be honoured.
			_, err := render(request, testExchangeContext())
			if err == nil {
				t.Fatal("a lost parallel-call disable was accepted under strict policy")
			}
			if !strings.Contains(err.Error(), string(FeatureParallelToolCalls)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureParallelToolCalls)
			}

			// Approved: one policy-gated loss, not a Note.
			context := testExchangeContext()
			context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureParallelToolCalls: {}}}
			report, err := render(request, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			found := false
			for _, loss := range report.Losses {
				if loss.Feature != FeatureParallelToolCalls {
					continue
				}
				found = true
				if loss.Kind != LossRecord {
					t.Fatalf("a real lost intent recorded a %v, not a policy-gated loss: %+v", loss.Kind, loss)
				}
			}
			if !found {
				t.Fatalf("report = %+v, want the parallel_tool_calls loss", report.Losses)
			}
		})
	}
}

// TestParallelDisableWithoutToolsIsANote proves the inert case is observable
// without being refusable: with no tools declared the model cannot emit a tool
// call at all, so the client's disable is already satisfied.
func TestParallelDisableWithoutToolsIsANote(t *testing.T) {
	for name, render := range parallelTargets() {
		t.Run(name, func(t *testing.T) {
			request := parallelRequest(false, nil, nil)
			// Strict policy: nothing is approved, yet the request is served.
			report, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("a tool-less disable refused a default deployment: %v", err)
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
		})
	}
}

// TestParallelDisableWithNoneChoiceIsANote proves the second inert condition:
// a choice that forbids every call already satisfies the disable.
func TestParallelDisableWithNoneChoiceIsANote(t *testing.T) {
	none := &CanonicalToolChoice{Mode: "none"}
	for name, render := range parallelTargets() {
		t.Run(name, func(t *testing.T) {
			request := parallelRequest(false, oneTool(), none)
			report, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("a none-choice disable refused a default deployment: %v", err)
			}
			found := false
			for _, loss := range report.Losses {
				if loss.Feature != FeatureParallelToolCalls {
					continue
				}
				found = true
				if loss.Kind != NoteRecord {
					t.Fatalf("the inert none-choice disable recorded a %v: %+v", loss.Kind, loss)
				}
			}
			if !found {
				t.Fatalf("report = %+v, want the parallel_tool_calls Note", report.Losses)
			}
		})
	}
}

// TestExplicitParallelAllowedIsNotALoss proves `parallel_tool_calls: true`
// costs nothing on a target that cannot carry it: every dialect's default is
// "parallel allowed", so omitting the field asserts the same thing. Charging it
// would refuse a request that loses nothing.
func TestExplicitParallelAllowedIsNotALoss(t *testing.T) {
	for name, render := range parallelTargets() {
		t.Run(name, func(t *testing.T) {
			request := parallelRequest(true, oneTool(), nil)
			_, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("an explicit parallel-allowed setting refused a strict render: %v", err)
			}
			report, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if reportHasFeature(report, FeatureParallelToolCalls) {
				t.Fatalf("an explicit true recorded a decision though nothing was lost: %+v", report.Losses)
			}
		})
	}
}

// TestParallelDenialRefusesOnlyWhereIntentIsLost is the operational proof: an
// operator who denies the key refuses traffic whose disable cannot be honoured,
// and keeps serving traffic whose setting is inert or already satisfied.
func TestParallelDenialRefusesOnlyWhereIntentIsLost(t *testing.T) {
	denied := LossPolicy{Allowed: map[Feature]struct{}{}}
	for name, render := range parallelTargets() {
		t.Run(name, func(t *testing.T) {
			// A real loss: refused.
			if _, err := render(parallelRequest(false, oneTool(), nil), &ExchangeContext{
				IDs: NewExchangeIDs(), LossPolicy: denied,
			}); err == nil {
				t.Fatal("a denied parallel_tool_calls served a request whose disable is lost")
			}
			// Inert and already-satisfied shapes: served.
			for _, request := range []CanonicalRequest{
				parallelRequest(false, nil, nil),
				parallelRequest(false, oneTool(), &CanonicalToolChoice{Mode: "none"}),
				parallelRequest(true, oneTool(), nil),
			} {
				if _, err := render(request, &ExchangeContext{
					IDs: NewExchangeIDs(), LossPolicy: denied,
				}); err != nil {
					t.Fatalf("a denied parallel_tool_calls refused a request that loses nothing: %v", err)
				}
			}
		})
	}
}

// TestMessagesParallelCarriageIsUnchanged is the scope guard: the Messages
// target expresses the setting natively, so a tools-declared request records no
// policy decision. Only a real DISABLE needs the flag — an explicit "allowed"
// asserts the dialect default, so carrying it would invent a tool_choice the
// client never wrote for a value that changes nothing.
func TestMessagesParallelCarriageIsUnchanged(t *testing.T) {
	disabled := parallelRequest(false, oneTool(), nil)
	rendered, report, err := RenderMessagesRequest(disabled, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if reportHasFeature(report, FeatureParallelToolCalls) {
		t.Fatalf("the Messages target recorded a decision it can express natively: %+v", report.Losses)
	}
	if !strings.Contains(string(rendered), "disable_parallel_tool_use") {
		t.Fatalf("the disable is missing from the Messages wire: %s", rendered)
	}

	allowed := parallelRequest(true, oneTool(), nil)
	rendered, report, err = RenderMessagesRequest(allowed, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if reportHasFeature(report, FeatureParallelToolCalls) {
		t.Fatalf("the Messages target recorded a decision it can express natively: %+v", report.Losses)
	}
	if strings.Contains(string(rendered), "tool_choice") {
		t.Fatalf("an allowed setting invented a tool_choice the client never wrote: %s", rendered)
	}
}
