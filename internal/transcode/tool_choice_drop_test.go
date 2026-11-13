package transcode

// A tool choice and the tool list must agree at the wire, on every target:
// OpenAI Chat, OpenAI Responses and Anthropic Messages all reject
// tool_choice (and parallel_tool_calls) when no tool is declared. Locally
// preventable incompatibilities are exactly what reconciliation exists to stop,
// so a choice or a parallel-tools field that outlives the tools it refers to is
// a defect, not a pass-through.

import (
	"strings"
	"testing"
)

// decodeBuiltinOnlyChoice decodes a Responses request whose only tool is a
// built-in (dropped under an approved loss) with the given tool_choice, and
// returns the canonical request.
func decodeBuiltinOnlyChoice(t *testing.T, choice string) CanonicalRequest {
	t.Helper()
	body := []byte(`{"model":"m","input":"hi","max_output_tokens":64,"tool_choice":` + choice + `,` +
		`"tools":[{"type":"web_search"}]}`)
	result, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureBuiltinTools: {},
	}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Request.Tools) != 0 {
		t.Fatalf("tools = %d, want the built-in dropped", len(result.Request.Tools))
	}
	return result.Request
}

// TestNoneChoiceIsDroppedWithTheLastTool proves a ""none"" choice does not
// outlive its tools. The premise — an empty tool list already means "call no
// tool" — is right; the conclusion is backwards, because the upstreams reject
// the FIELD, not the meaning.
func TestNoneChoiceIsDroppedWithTheLastTool(t *testing.T) {
	request := decodeBuiltinOnlyChoice(t, `"none"`)
	if request.ToolChoice != nil {
		t.Fatalf("tool_choice = %+v, want it dropped with the tools", request.ToolChoice)
	}

	// Every renderer must then emit no tool_choice field at all.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureBuiltinTools: {}}}
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			rendered, _, err := render(request, context)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(string(rendered), "tool_choice") {
				t.Fatalf("a tool_choice was emitted with zero tools: %s", rendered)
			}
		})
	}
}

// TestDroppedChoiceIsNotedUnderTheFeatureThatRemovedTheTools proves the drop is
// observable under the decision that actually happened.
func TestDroppedChoiceIsNotedUnderTheFeatureThatRemovedTheTools(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","max_output_tokens":64,"tool_choice":"none",` +
		`"tools":[{"type":"web_search"}]}`)
	result, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureBuiltinTools: {},
	}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasAt(result.Report, FeatureBuiltinTools, "tool_choice") {
		t.Fatalf("report = %+v, want the tool_choice drop noted under builtin_tools",
			result.Report.Losses)
	}
}

// TestRequiredChoiceIsStillRefused proves the genuinely incoherent shape still
// fails: "required" asserts a call that no remaining tool can satisfy.
func TestRequiredChoiceIsStillRefused(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","max_output_tokens":64,"tool_choice":"required",` +
		`"tools":[{"type":"web_search"}]}`)
	_, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureBuiltinTools: {},
	}})
	if err == nil {
		t.Fatal("required with zero surviving tools was accepted")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("err = %q, want it to name the required choice", err)
	}
}

// TestNoneChoicePassesThroughWhenTheClientSentNoTools proves the pass-through
// scope is unchanged: a client that declares no tools is judged by the upstream.
func TestNoneChoicePassesThroughWhenTheClientSentNoTools(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tool_choice":"none"}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != "none" {
		t.Fatalf("tool_choice = %+v, want the client's own none choice preserved", result.Request.ToolChoice)
	}
}
