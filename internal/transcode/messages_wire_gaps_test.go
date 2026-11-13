package transcode

// Three places where a renderer's wire contract was narrower than the IR it
// serves: an Anthropic tool requires input_schema but a Responses function tool
// may omit its parameters; an enabled thinking config without a budget is a Note
// everywhere except on this wire, which rejects the union; and the
// structured-output decision is a canonical feature, not the Responses field
// name that happens to carry it in one dialect.

import (
	"encoding/json"
	"strings"
	"testing"
)

// messagesToolWire decodes the rendered Messages body's tools and thinking.
func messagesWire(t *testing.T, rendered []byte) (tools []map[string]json.RawMessage, thinking json.RawMessage) {
	t.Helper()
	var out struct {
		Tools    []map[string]json.RawMessage `json:"tools"`
		Thinking json.RawMessage              `json:"thinking"`
	}
	if err := json.Unmarshal(rendered, &out); err != nil {
		t.Fatalf("rendered messages: %v\n%s", err, rendered)
	}
	return out.Tools, out.Thinking
}

// TestMessagesRendersAnObjectSchemaForASchemalessTool proves a tool decoded
// without parameters still renders a valid Anthropic tool: input_schema is
// required, and an absent one is malformed rather than "no arguments allowed".
func TestMessagesRendersAnObjectSchemaForASchemalessTool(t *testing.T) {
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Tools: []CanonicalTool{{Name: "no_args"}},
	}
	rendered, _, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	tools, _ := messagesWire(t, rendered)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1: %s", len(tools), rendered)
	}
	schema, ok := tools[0]["input_schema"]
	if !ok || len(schema) == 0 || string(schema) == "null" {
		t.Fatalf("input_schema = %s, want an object schema: %s", schema, rendered)
	}
	var probe map[string]any
	if err := json.Unmarshal(schema, &probe); err != nil {
		t.Fatalf("input_schema is not a JSON object: %v", err)
	}
	if probe["type"] != "object" {
		t.Fatalf("input_schema = %s, want type object", schema)
	}
}

// TestMessagesOmitsBudgetlessThinkingAsANote proves the enabled-without-a-budget
// shape is handled the way Chat and Responses handle it: noted and omitted,
// because the Anthropic wire rejects an enabled thinking config with no budget.
func TestMessagesOmitsBudgetlessThinkingAsANote(t *testing.T) {
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Thinking: &CanonicalThinking{Type: "enabled"},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureRequestReasoning: {}}}
	rendered, report, err := RenderMessagesRequest(request, context)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, thinking := messagesWire(t, rendered)
	if len(thinking) > 0 && string(thinking) != "null" {
		t.Fatalf("thinking = %s, want it omitted rather than an invalid union", thinking)
	}
	if !reportHasFeature(report, FeatureRequestReasoning) {
		t.Fatalf("report = %+v, want the request_reasoning Note", report.Losses)
	}
	// A real budget still renders the enabled union.
	budget := 2048
	request.Thinking = &CanonicalThinking{Type: "enabled", BudgetTokens: &budget}
	rendered, _, err = RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, thinking = messagesWire(t, rendered)
	var config struct {
		Type         string `json:"type"`
		BudgetTokens *int   `json:"budget_tokens"`
	}
	if err := json.Unmarshal(thinking, &config); err != nil {
		t.Fatalf("thinking decode: %v", err)
	}
	if config.Type != "enabled" || config.BudgetTokens == nil || *config.BudgetTokens != budget {
		t.Fatalf("thinking = %s, want enabled with the budget", thinking)
	}
}

// TestStructuredOutputLossNamesTheCanonicalPath proves the decision names the
// canonical feature rather than the Responses wire field that carries it: a
// Gemini-sourced schema is not a "text.format".
func TestStructuredOutputLossNamesTheCanonicalPath(t *testing.T) {
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		StructuredOutput: &CanonicalStructuredOutput{
			Name:   "s",
			Schema: json.RawMessage(`{"type":"object"}`),
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureStructuredOutput: {}}}
	for name, render := range map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error){
		"messages": RenderMessagesRequest,
		"chat": func(r CanonicalRequest, c *ExchangeContext) ([]byte, ConversionReport, error) {
			return RenderChatRequest(r, c, ChatCapabilities{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, report, err := render(request, context)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !reportHasAt(report, FeatureStructuredOutput, "structured_output") {
				t.Fatalf("report = %+v, want the loss at the canonical path structured_output",
					report.Losses)
			}
			for _, loss := range report.Losses {
				if loss.Feature == FeatureStructuredOutput && loss.Path == "text.format" {
					t.Fatalf("the loss still names the Responses wire field: %+v", loss)
				}
			}
		})
	}
}

// TestStructuredOutputIsCarriedWhereTheTargetSupportsIt is the scope guard: the
// path change must not disturb an actual carriage.
func TestStructuredOutputIsCarriedWhereTheTargetSupportsIt(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		StructuredOutput: &CanonicalStructuredOutput{
			Name:   "s",
			Schema: json.RawMessage(`{"type":"object"}`),
		},
	}
	rendered, report, err := RenderChatRequest(request, testExchangeContext(),
		ChatCapabilities{StructuredOutputs: true})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(rendered), "json_schema") {
		t.Fatalf("structured output was not carried: %s", rendered)
	}
	if reportHasFeature(report, FeatureStructuredOutput) {
		t.Fatalf("a carried structured output recorded a loss: %+v", report.Losses)
	}
}
