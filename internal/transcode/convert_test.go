package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

func testExchangeContext() *ExchangeContext {
	return &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: StrictLossPolicy(),
	}
}

func TestDecodeResponsesRequestFixture(t *testing.T) {
	result, echo, err := DecodeResponsesRequest(
		testcorpus.ResponsesRequestJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("decode responses request fixture: %v", err)
	}
	if result.Request.ClientModel != "gpt-4.1" {
		t.Fatalf("model = %q", result.Request.ClientModel)
	}
	if echo == nil {
		t.Fatal("echo is nil")
	}
	if echo.MaxOutputTokens == nil || *echo.MaxOutputTokens != 512 {
		t.Fatalf("max_output_tokens = %v", echo.MaxOutputTokens)
	}
	if echo.Temperature != 0.7 {
		t.Fatalf("temperature = %v", echo.Temperature)
	}
	if len(result.Request.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(result.Request.Tools))
	}
	// instructions -> system turn, input user message -> user turn.
	if len(result.Request.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 (instructions + input)", len(result.Request.Turns))
	}
	if result.Request.Turns[0].Role != CanonicalSystem {
		t.Fatalf("turn 0 role = %q", result.Request.Turns[0].Role)
	}
	if result.Request.Turns[1].Role != CanonicalUser {
		t.Fatalf("turn 1 role = %q", result.Request.Turns[1].Role)
	}
	if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != "auto" {
		t.Fatalf("tool choice = %+v", result.Request.ToolChoice)
	}
	if result.Request.ParallelTools == nil || !*result.Request.ParallelTools {
		t.Fatalf("parallel tools = %v", result.Request.ParallelTools)
	}
}

func TestDecodeResponsesRequestInstructionsAndDeveloper(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"instructions":"be helpful",
		"input":[
			{"role":"developer","content":"follow the rules"},
			{"role":"user","content":"hello"}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Turns) != 3 {
		t.Fatalf("turns = %d, want 3 (instructions, developer, user)", len(result.Request.Turns))
	}
	if result.Request.Turns[0].Role != CanonicalSystem {
		t.Fatalf("instructions turn role = %q", result.Request.Turns[0].Role)
	}
	if result.Request.Turns[1].Role != CanonicalDeveloper {
		t.Fatalf("developer turn role = %q", result.Request.Turns[1].Role)
	}
	if result.Request.Turns[2].Role != CanonicalUser {
		t.Fatalf("user turn role = %q", result.Request.Turns[2].Role)
	}
}

func TestDecodeResponsesRequestStructuredOutput(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"text":{"format":{"type":"json_schema","name":"weather","schema":{"type":"object"}}}
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result.Request.StructuredOutput == nil {
		t.Fatal("structured output is nil")
	}
	if result.Request.StructuredOutput.Name != "weather" {
		t.Fatalf("name = %q", result.Request.StructuredOutput.Name)
	}
}

func TestDecodeResponsesRequestRejectsUnsupported(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		// prompt_cache_key is the responses_controls loss decision:
		// rejected under the strict policy, approved by the CLI defaults.
		{"prompt_cache_key", `{"model":"m","input":"x","prompt_cache_key":"k"}`},
		{"background", `{"model":"m","input":"x","background":true}`},
		{"unknown field", `{"model":"m","input":"x","bogus":1}`},
		{"unsupported text format", `{"model":"m","input":"x","text":{"format":{"type":"json_object"}}}`},
		{"missing model", `{"input":"x"}`},
		{"bad truncation", `{"model":"m","input":"x","truncation":"sometimes"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := DecodeResponsesRequest([]byte(tt.body), StrictLossPolicy())
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

// TestDecodeResponsesRequestControlsPermissiveProbe pins the responses_controls
// contract against every policy (review-12 R12-1): the five conversation-state
// request controls are typed unsupported-feature errors even when
// responses_controls is APPROVED — the key cannot un-gate them — while
// prompt_cache_key under the same approval drops observably.
func TestDecodeResponsesRequestControlsPermissiveProbe(t *testing.T) {
	permissive := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureResponsesControls: {},
	}}
	hardRejected := map[string]string{
		"background":        `{"model":"m","input":"x","background":true}`,
		"max_tool_calls":    `{"model":"m","input":"x","max_tool_calls":3}`,
		"prompt":            `{"model":"m","input":"x","prompt":{"id":"p"}}`,
		"safety_identifier": `{"model":"m","input":"x","safety_identifier":"u"}`,
		"status":            `{"model":"m","input":"x","status":"completed"}`,
	}
	for name, body := range hardRejected {
		_, _, err := DecodeResponsesRequest([]byte(body), permissive)
		if err == nil {
			t.Fatalf("responses_controls approval un-gated the hard-rejected %q", name)
		}
		var target *UnsupportedFeatureError
		if !errors.As(err, &target) {
			t.Fatalf("%s: err = %T: %v, want UnsupportedFeatureError", name, err, err)
		}
		if target.Feature != name {
			t.Fatalf("%s: feature = %q", name, target.Feature)
		}
	}

	// prompt_cache_key under the SAME approval is an observable loss.
	result, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"x","prompt_cache_key":"k"}`),
		permissive,
	)
	if err != nil {
		t.Fatalf("prompt_cache_key rejected under an approved responses_controls: %v", err)
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureResponsesControls && loss.Path == "prompt_cache_key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want a responses_controls loss at prompt_cache_key", result.Report.Losses)
	}
}

func TestDecodeResponsesRequestToolChoiceRequired(t *testing.T) {
	// "required" is part of the official Responses tool_choice contract and
	// renders natively into Chat; it must decode, not be rejected.
	result, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"x","tool_choice":"required"}`),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("required tool_choice rejected: %v", err)
	}
	if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != "required" {
		t.Fatalf("tool choice = %+v", result.Request.ToolChoice)
	}
}

// TestDecodeResponsesRequestToolTypeViolations reproduces review-12 finding
// 4 at the decode boundary: a tool with a missing type must be a malformed
// request under EVERY policy (never silently droppable as a pseudo
// built-in under an approved builtin_tools loss), and cross-type fields
// must be rejected, not silently ignored.
func TestDecodeResponsesRequestToolTypeViolations(t *testing.T) {
	bodies := map[string]string{
		"missing type": `{
			"model":"m","input":"x",
			"tools":[{"name":"f","strict":true}]
		}`,
		"null type": `{
			"model":"m","input":"x",
			"tools":[{"type":null,"name":"f","strict":true}]
		}`,
		"function tool with tools": `{
			"model":"m","input":"x",
			"tools":[{"type":"function","name":"f","strict":true,"tools":[{"type":"function","name":"inner","strict":true}]}]
		}`,
		"namespace with parameters": `{
			"model":"m","input":"x",
			"tools":[{"type":"namespace","name":"ns","parameters":{"type":"object"},"tools":[{"type":"function","name":"inner","strict":true}]}]
		}`,
		"namespace with strict": `{
			"model":"m","input":"x",
			"tools":[{"type":"namespace","name":"ns","strict":true,"tools":[{"type":"function","name":"inner","strict":true}]}]
		}`,
	}
	for name, body := range bodies {
		for policyName, policy := range map[string]LossPolicy{
			"strict":     StrictLossPolicy(),
			"permissive": {Allowed: map[Feature]struct{}{FeatureBuiltinTools: {}}},
		} {
			t.Run(name+"/"+policyName, func(t *testing.T) {
				_, _, err := DecodeResponsesRequest([]byte(body), policy)
				if err == nil {
					t.Fatalf("%s policy accepted a tool-union shape violation", policyName)
				}
				if de, ok := errors.AsType[*wire.DecodeError](err); ok {
					if de.Kind != wire.DecodeMissingRequired && de.Kind != wire.DecodeContradictoryUnion {
						t.Fatalf("err kind = %s, want missing_required or contradictory_union", de.Kind)
					}
					return
				}
				// Wrapped decode errors keep their typed kind.
				t.Fatalf("err = %v (%T), want a typed DecodeError", err, err)
			})
		}
	}
}

// TestDecodeResponsesRequestNamespaceNestedBuiltinTools verifies a built-in
// tool nested inside a namespace is the SAME builtin_tools loss decision as a
// top-level one: approved (e.g. by the CLI defaults) it drops observably,
// rejected it fails the request — never a different, harder rule than the
// top-level path (review-11 finding 2).
func TestDecodeResponsesRequestNamespaceNestedBuiltinTools(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[
			{"type":"namespace","name":"ns","tools":[
				{"type":"function","name":"f","parameters":{"type":"object"},"strict":true},
				{"type":"web_search","name":"ws"}
			]}
		]
	}`)

	// Approved: the nested built-in tool drops observably and the nested
	// function tool survives.
	result, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureBuiltinTools: {},
	}})
	if err != nil {
		t.Fatalf("approved policy rejected the nested built-in tool: %v", err)
	}
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "f" {
		t.Fatalf("tools = %+v, want only the nested function tool", result.Request.Tools)
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureBuiltinTools &&
			(strings.Contains(loss.Path, "web_search") || strings.Contains(loss.Detail, "web_search")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want an observable builtin_tools loss naming web_search", result.Report.Losses)
	}

	// Strict: rejected exactly like a top-level built-in tool.
	_, _, err = DecodeResponsesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("strict policy accepted a namespace-nested built-in tool; want rejection")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("error = %T: %v, want UnsupportedFeatureError", err, err)
	}
	if target.Feature != string(FeatureBuiltinTools) {
		t.Fatalf("feature = %q, want builtin_tools", target.Feature)
	}
}

// TestDecodeResponsesRequestToolChoiceReconciliation reproduces review-12
// finding 5: an approved built-in tool drop must reconcile tool_choice against
// the tools that actually survive, or the converter renders an invalid
// upstream request (tool_choice "required" or a named function with zero
// tools).
func TestDecodeResponsesRequestToolChoiceReconciliation(t *testing.T) {
	builtinBody := `{
		"model":"m","input":"x",
		"tools":[{"type":"web_search"}],
		"tool_choice":%s
	}`
	mixedBody := `{
		"model":"m","input":"x",
		"tools":[
			{"type":"function","name":"f","parameters":{"type":"object"},"strict":true},
			{"type":"web_search"}
		],
		"tool_choice":%s
	}`
	permissive := LossPolicy{Allowed: map[Feature]struct{}{FeatureBuiltinTools: {}}}

	t.Run("required with no surviving tools is an error", func(t *testing.T) {
		_, _, err := DecodeResponsesRequest(
			fmt.Appendf(nil, builtinBody, `"required"`),
			permissive,
		)
		if err == nil {
			t.Fatal("accepted tool_choice required after every tool dropped; want a client-dialect error")
		}
		if !strings.Contains(err.Error(), "tool_choice") {
			t.Fatalf("err = %v, want it to name tool_choice", err)
		}
	})

	t.Run("named choice whose tool was dropped is an error", func(t *testing.T) {
		_, _, err := DecodeResponsesRequest(
			fmt.Appendf(nil, builtinBody, `{"type":"function","name":"web_search"}`),
			permissive,
		)
		if err == nil {
			t.Fatal("accepted a named tool_choice whose tool was dropped; want a client-dialect error")
		}
		if !strings.Contains(err.Error(), "web_search") {
			t.Fatalf("err = %v, want it to name the dropped tool", err)
		}
	})

	t.Run("named choice with no matching function tool is an error", func(t *testing.T) {
		_, _, err := DecodeResponsesRequest(
			fmt.Appendf(nil, mixedBody, `{"type":"function","name":"missing"}`),
			permissive,
		)
		if err == nil {
			t.Fatal("accepted a named tool_choice no surviving tool matches; want a client-dialect error")
		}
		if !strings.Contains(err.Error(), "missing") {
			t.Fatalf("err = %v, want it to name the unmatched tool", err)
		}
	})

	t.Run("auto with no surviving tools drops with a note", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest(
			fmt.Appendf(nil, builtinBody, `"auto"`),
			permissive,
		)
		if err != nil {
			t.Fatalf("auto tool_choice rejected: %v", err)
		}
		if len(result.Request.Tools) != 0 {
			t.Fatalf("tools = %+v, want none to survive", result.Request.Tools)
		}
		if result.Request.ToolChoice != nil {
			t.Fatalf("tool choice = %+v, want nil after the last tool dropped", result.Request.ToolChoice)
		}
		found := false
		for _, loss := range result.Report.Losses {
			if loss.Feature == FeatureBuiltinTools && loss.Path == "tool_choice" {
				found = true
			}
		}
		if !found {
			t.Fatalf("report = %+v, want a builtin_tools note at tool_choice", result.Report.Losses)
		}
	})

	t.Run("none with no surviving tools is preserved", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest(
			fmt.Appendf(nil, builtinBody, `"none"`),
			permissive,
		)
		if err != nil {
			t.Fatalf("none tool_choice rejected: %v", err)
		}
		if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != "none" {
			t.Fatalf("tool choice = %+v, want none preserved", result.Request.ToolChoice)
		}
	})

	t.Run("required and named with surviving tools pass through", func(t *testing.T) {
		for _, choice := range []string{`"required"`, `{"type":"function","name":"f"}`} {
			result, _, err := DecodeResponsesRequest(
				fmt.Appendf(nil, mixedBody, choice),
				permissive,
			)
			if err != nil {
				t.Fatalf("choice %s rejected: %v", choice, err)
			}
			if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "f" {
				t.Fatalf("tools = %+v, want the function tool to survive", result.Request.Tools)
			}
			wantMode := "required"
			if strings.HasPrefix(choice, "{") {
				wantMode = "named"
			}
			if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != wantMode {
				t.Fatalf("tool choice = %+v, want %s preserved", result.Request.ToolChoice, wantMode)
			}
		}
	})

	t.Run("required with no tools requested still decodes", func(t *testing.T) {
		// Pinned pass-through: the reconciliation is about converter-caused
		// emptiness, not client incoherence the upstream can judge.
		result, _, err := DecodeResponsesRequest(
			[]byte(`{"model":"m","input":"x","tool_choice":"required"}`),
			StrictLossPolicy(),
		)
		if err != nil {
			t.Fatalf("required with no tools rejected: %v", err)
		}
		if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != "required" {
			t.Fatalf("tool choice = %+v", result.Request.ToolChoice)
		}
	})
}

// TestRenderChatRequestNoDanglingToolChoice proves the chat body carries no
// tool_choice when the built-in drop left no tools to choose among
// (review-12 finding 5, render level).
func TestRenderChatRequestNoDanglingToolChoice(t *testing.T) {
	body := []byte(`{
		"model":"m","input":"x",
		"tools":[{"type":"web_search"}],
		"tool_choice":"auto"
	}`)
	result, _, err := DecodeResponsesRequest(
		body,
		LossPolicy{Allowed: map[Feature]struct{}{FeatureBuiltinTools: {}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Request.ToolChoice != nil {
		t.Fatalf("decode left a dangling tool choice: %+v", result.Request.ToolChoice)
	}
	rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	if chat.ToolChoice != nil {
		t.Fatalf("rendered tool_choice = %+v, want none with zero tools", chat.ToolChoice)
	}
	if len(chat.Tools) != 0 {
		t.Fatalf("rendered tools = %+v, want none", chat.Tools)
	}
}

// TestDecodeMessagesRequestToolChoiceNamedMissingTool pins the Messages side
// of review-12 finding 5: Messages has no built-in tools (every tool is a
// function and always survives), so only the dangling named reference needs
// guarding.
func TestDecodeMessagesRequestToolChoiceNamedMissingTool(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":10,
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"f","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"g"}
	}`)
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("accepted a named tool_choice no tool matches; want a client-dialect error")
	}
	if !strings.Contains(err.Error(), "g") {
		t.Fatalf("err = %v, want it to name the unmatched tool", err)
	}

	// A matching name decodes untouched.
	good := []byte(`{
		"model":"m","max_tokens":10,
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"f","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"f"}
	}`)
	result, err := DecodeMessagesRequest(good, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result.Request.ToolChoice == nil ||
		result.Request.ToolChoice.Mode != "named" ||
		result.Request.ToolChoice.Name != "f" {
		t.Fatalf("tool choice = %+v, want named f", result.Request.ToolChoice)
	}
}

func TestDecodeResponsesRequestFunctionCallIdentity(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"role":"assistant","content":[{"type":"input_text","text":"ok"}]},
			{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"x\":1}"},
			{"type":"function_call_output","call_id":"call_1","output":"done"}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// assistant text turn with the function call folded into it, then a user
	// turn holding the function result.
	if len(result.Request.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(result.Request.Turns))
	}
	assistant := result.Request.Turns[0]
	if assistant.Role != CanonicalAssistant || len(assistant.Parts) != 2 {
		t.Fatalf("assistant turn = %+v", assistant)
	}
	call, ok := assistant.Parts[1].(CanonicalFunctionCall)
	if !ok {
		t.Fatalf("part 1 = %T", assistant.Parts[1])
	}
	if call.CallID != "call_1" || call.Name != "f" {
		t.Fatalf("call = %+v", call)
	}
	user := result.Request.Turns[1]
	if user.Role != CanonicalUser || len(user.Parts) != 1 {
		t.Fatalf("user turn = %+v", user)
	}
	resultPart, ok := user.Parts[0].(CanonicalFunctionResult)
	if !ok {
		t.Fatalf("part = %T", user.Parts[0])
	}
	if resultPart.CallID != "call_1" {
		t.Fatalf("result = %+v", resultPart)
	}
}

func TestDecodeResponsesRequestReasoningArtifact(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
			{"role":"user","content":"hi"}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Artifacts.ResponsesReasoningItems) != 1 {
		t.Fatalf("reasoning items = %d", len(result.Request.Artifacts.ResponsesReasoningItems))
	}
}

func TestDecodeMessagesRequestFixture(t *testing.T) {
	// The fixture carries top_k, which is rejected under the strict policy.
	_, err := DecodeMessagesRequest(
		testcorpus.AnthropicMessagesRequestJSON(),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("expected top_k rejection under strict policy")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("error = %T: %v", err, err)
	}
	if target.Feature != string(FeatureTopK) {
		t.Fatalf("feature = %q, want top_k", target.Feature)
	}

	// With top_k allowed, the fixture decodes.
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureTopK: {}}}
	result, err := DecodeMessagesRequest(testcorpus.AnthropicMessagesRequestJSON(), policy)
	if err != nil {
		t.Fatalf("decode with top_k loss: %v", err)
	}
	if result.Request.ClientModel != "claude-sonnet-4-20250514" {
		t.Fatalf("model = %q", result.Request.ClientModel)
	}
	if result.Request.MaxOutputTokens == nil || *result.Request.MaxOutputTokens != 1024 {
		t.Fatalf("max tokens = %v", result.Request.MaxOutputTokens)
	}
	if len(result.Request.Turns) != 2 { // system + user
		t.Fatalf("turns = %d", len(result.Request.Turns))
	}
	if result.Request.Turns[0].Role != CanonicalSystem {
		t.Fatalf("system turn = %+v", result.Request.Turns[0])
	}
	if len(result.Request.Tools) != 1 {
		t.Fatalf("tools = %d", len(result.Request.Tools))
	}
	if result.Request.ToolChoice == nil || result.Request.ToolChoice.Mode != "auto" {
		t.Fatalf("tool choice = %+v", result.Request.ToolChoice)
	}
}

func TestDecodeMessagesRequestThinkingArtifact(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"hmm","signature":"sig123"},
				{"type":"text","text":"answer"}
			]}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Artifacts.AnthropicThinkingBlocks) != 1 {
		t.Fatalf("thinking blocks = %d", len(result.Request.Artifacts.AnthropicThinkingBlocks))
	}
	// The thinking block is preserved raw, never reinterpreted.
	var block AnthropicContentBlock
	if err := json.Unmarshal(result.Request.Artifacts.AnthropicThinkingBlocks[0], &block); err != nil {
		t.Fatal(err)
	}
	if block.Type != AnthropicContentBlockTypeThinking || *block.Signature != "sig123" {
		t.Fatalf("block = %+v", block)
	}
	// The text part follows.
	assistant := result.Request.Turns[1]
	text, ok := assistant.Parts[0].(CanonicalText)
	if !ok || text.Text != "answer" {
		t.Fatalf("assistant parts = %+v", assistant.Parts)
	}
}

func TestRequirePortableArtifacts(t *testing.T) {
	request := CanonicalRequest{
		Artifacts: SourceArtifacts{
			AnthropicThinkingBlocks: []json.RawMessage{json.RawMessage(`{}`)},
		},
	}
	var report ConversionReport
	// Crossing to Chat requires a loss.
	err := RequirePortableArtifacts(request, UpstreamChatCompletions, StrictLossPolicy(), &report)
	if err == nil {
		t.Fatal("expected thinking rejection crossing to chat")
	}
	// Staying on Messages is fine.
	err = RequirePortableArtifacts(request, UpstreamMessages, StrictLossPolicy(), &report)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderChatRequestFromResponses(t *testing.T) {
	result, _, err := DecodeResponsesRequest(
		testcorpus.ResponsesRequestJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	if chat.Model != "gpt-4.1" {
		t.Fatalf("model = %q", chat.Model)
	}
	if chat.N == nil || *chat.N != 1 {
		t.Fatalf("n = %v, want 1", chat.N)
	}
	if len(chat.Messages) == 0 {
		t.Fatal("no messages")
	}
	// Instructions became a system message.
	if chat.Messages[0].Role != ChatMessageRoleSystem {
		t.Fatalf("first message role = %q", chat.Messages[0].Role)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("tools = %d", len(chat.Tools))
	}
	if chat.ToolChoice == nil || chat.ToolChoice.Str == nil || *chat.ToolChoice.Str != "auto" {
		t.Fatalf("tool choice = %+v", chat.ToolChoice)
	}
	if chat.Stream == nil || *chat.Stream {
		t.Fatalf("stream = %v, want false (fixture streams=false)", chat.Stream)
	}
}

func TestRenderChatRequestDeveloperRoleLoss(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[{"role":"developer","content":"rules"}]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Strict policy + no capability: developer role is a rejection.
	if _, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{}); err == nil {
		t.Fatal("expected developer role rejection")
	}
	// With the capability, developer is preserved.
	_, _, err = RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{DeveloperRole: true})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderChatRequestStructuredOutputCapability(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object"}}}
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Without the capability: rejection under strict policy.
	if _, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{}); err == nil {
		t.Fatal("expected structured output rejection")
	}
	// With the capability: response_format json_schema.
	rendered, _, err := RenderChatRequest(
		result.Request,
		testExchangeContext(),
		ChatCapabilities{StructuredOutputs: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.ResponseFormat == nil || chat.ResponseFormat.Type != ChatResponseFormatJSONSchema {
		t.Fatalf("response format = %+v", chat.ResponseFormat)
	}
}

func TestRenderResponsesRequestFromMessages(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureTopK: {}}}
	result, err := DecodeMessagesRequest(testcorpus.AnthropicMessagesRequestJSON(), policy)
	if err != nil {
		t.Fatal(err)
	}
	// The Messages source tool carries no strictness semantic: under strict
	// policy the conversion is rejected client-dialect (review-z commit 1).
	if _, _, err := RenderResponsesRequest(
		result.Request,
		testExchangeContext(),
	); err == nil {
		t.Fatal("expected strict-policy rejection for missing tool strictness")
	}
	// Under the tool_schema_strictness permission the render emits explicit
	// strict:false and succeeds.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureTopK:                 {},
		FeatureToolSchemaStrictness: {},
	}}
	rendered, _, err := RenderResponsesRequest(result.Request, context)
	if err != nil {
		t.Fatal(err)
	}
	var envelope openairesponses.Request
	if err := strictDecode(rendered, &envelope); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	if envelope.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("model = %q", envelope.Model)
	}
	if envelope.Instructions == nil {
		t.Fatal("instructions is nil")
	}
	if envelope.Input == nil {
		t.Fatal("input is nil")
	}
	if err := envelope.Input.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Tools) != 1 {
		t.Fatalf("tools = %d", len(envelope.Tools))
	}
	if !envelope.Tools[0].Strict.Present || envelope.Tools[0].Strict.Null ||
		envelope.Tools[0].Strict.Value {
		t.Fatalf("rendered strict = %+v, want explicit false", envelope.Tools[0].Strict)
	}
}

func TestRenderResponsesRequestStopSequencesLoss(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"stop_sequences":["STOP"]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Stop sequences have no Responses representation; strict policy rejects.
	if _, _, err := RenderResponsesRequest(result.Request, testExchangeContext()); err == nil {
		t.Fatal("expected stop sequence rejection")
	}
}

func TestDecodeChatResponseFixture(t *testing.T) {
	response, _, err := DecodeChatResponseWithPolicy(testcorpus.ChatCompletionsResponseJSON(), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "chatcmpl_8xyz" {
		t.Fatalf("id = %q", response.ID)
	}
	if response.Status != CanonicalResponseCompleted {
		t.Fatalf("status = %q", response.Status)
	}
	if response.Stop.Reason != CanonicalStopEndTurn {
		t.Fatalf("stop reason = %q", response.Stop.Reason)
	}
	if response.Usage.InputTokens != 42 || response.Usage.OutputTokens != 18 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if len(response.Items) != 1 {
		t.Fatalf("items = %d", len(response.Items))
	}
	message, ok := response.Items[0].(*CanonicalMessageItem)
	if !ok || len(message.Parts) == 0 {
		t.Fatalf("item = %T", response.Items[0])
	}
	text, ok := message.Parts[0].(CanonicalText)
	if !ok {
		t.Fatalf("part = %T", message.Parts[0])
	}
	if !strings.Contains(text.Text, "21°C") {
		t.Fatalf("text = %q", text.Text)
	}
}

func TestDecodeChatResponseProviderReasoning(t *testing.T) {
	body := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning":"think"}}]
	}`)
	// Without the capability: rejection.
	if _, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy()); err == nil {
		t.Fatal("expected provider reasoning rejection")
	}
	// With the capability: mapped to ordinary text.
	response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{ProviderReasoningText: true}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	message, ok := response.Items[0].(*CanonicalMessageItem)
	if !ok {
		t.Fatalf("item = %T", response.Items[0])
	}
	found := false
	for _, part := range message.Parts {
		if text, ok := part.(CanonicalText); ok && text.Text == "think" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning text missing: %+v", message.Parts)
	}
}

func TestDecodeChatResponseMultipleChoicesRejected(t *testing.T) {
	body := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[
			{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"a"}},
			{"index":1,"finish_reason":"stop","message":{"role":"assistant","content":"b"}}
		]
	}`)
	if _, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy()); err == nil {
		t.Fatal("expected multiple-choice rejection")
	}
}

func TestDecodeChatResponseToolCalls(t *testing.T) {
	body := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{
			"role":"assistant","content":"",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]
		}}]
	}`)
	response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if response.Stop.Reason != CanonicalStopToolUse {
		t.Fatalf("stop reason = %q", response.Stop.Reason)
	}
	var found *CanonicalFunctionCallItem
	for _, item := range response.Items {
		if call, ok := item.(*CanonicalFunctionCallItem); ok {
			found = call
			break
		}
	}
	if found == nil {
		t.Fatalf("no function call item: %+v", response.Items)
	}
	if found.CallID != "call_1" || found.Name != "f" {
		t.Fatalf("call = %+v", found)
	}
	if !found.Arguments.IsObject || found.Arguments.Raw != `{"x":1}` {
		t.Fatalf("arguments = %+v", found.Arguments)
	}
}

func TestDecodeResponsesResponseFixture(t *testing.T) {
	response, err := DecodeResponsesResponse(testcorpus.ResponsesResponseJSON())
	if err != nil {
		t.Fatalf("decode responses response: %v", err)
	}
	if response.ID != "resp_8xyz" {
		t.Fatalf("id = %q", response.ID)
	}
	if response.Status != CanonicalResponseCompleted {
		t.Fatalf("status = %q", response.Status)
	}
	if response.Stop.Reason != CanonicalStopToolUse {
		t.Fatalf("stop reason = %q (fixture has function_call)", response.Stop.Reason)
	}
	if response.Usage.InputTokens != 45 || response.Usage.OutputTokens != 25 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	// The fixture output: reasoning item, function call, function call
	// output, message — each preserved as its own canonical item.
	if len(response.Items) != 4 {
		t.Fatalf("items = %d, want 4: %+v", len(response.Items), response.Items)
	}
	call, ok := response.Items[1].(*CanonicalFunctionCallItem)
	if !ok {
		t.Fatalf("item 1 = %T", response.Items[1])
	}
	if call.CallID != "call_abc123" || call.Name != "get_weather" {
		t.Fatalf("call = %+v", call)
	}
	if _, ok := response.Items[2].(*CanonicalFunctionResultItem); !ok {
		t.Fatalf("item 2 = %T, want a function result item", response.Items[2])
	}
	if _, ok := response.Items[3].(*CanonicalMessageItem); !ok {
		t.Fatalf("item 3 = %T, want a message item", response.Items[3])
	}
	if _, ok := response.Items[0].(*CanonicalReasoningItem); !ok {
		t.Fatalf("item 0 = %T, want a reasoning item", response.Items[0])
	}
}

func TestRenderMessagesResponseReasoningLoss(t *testing.T) {
	// The fixture has a reasoning item; rendering to Messages requires a loss,
	// so under the strict policy rendering fails.
	response, err := DecodeResponsesResponse(testcorpus.ResponsesResponseJSON())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Items) != 4 {
		t.Fatalf("items = %d", len(response.Items))
	}
	if _, ok := response.Items[0].(*CanonicalReasoningItem); !ok {
		t.Fatalf("item 0 = %T, want a reasoning item", response.Items[0])
	}
	if _, _, err := RenderMessagesResponse(response, testExchangeContext()); err == nil {
		t.Fatal("expected reasoning loss rejection under strict policy")
	}
	// With the losses approved, rendering succeeds.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary:       {},
		FeatureOutputItemBoundaries:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	if _, _, err := RenderMessagesResponse(response, context); err != nil {
		t.Fatalf("render with approved loss: %v", err)
	}
}

func TestRenderMessagesResponseFromResponses(t *testing.T) {
	response, err := DecodeResponsesResponse(testcorpus.ResponsesResponseJSON())
	if err != nil {
		t.Fatal(err)
	}
	// The fixture contains reasoning items and a conversation-state result
	// echo; allow both losses so rendering proceeds and the tool_use/text
	// block shapes can be asserted.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary:       {},
		FeatureOutputItemBoundaries:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	rendered, _, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	var message AnthropicMessageResponse
	if err := json.Unmarshal(rendered, &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "message" || message.Role != "assistant" {
		t.Fatalf("message = %+v", message)
	}
	if message.StopReason == nil || *message.StopReason != AnthropicStopReasonToolUse {
		t.Fatalf("stop reason = %v", message.StopReason)
	}
	// The function call became a tool_use block; the text followed.
	if len(message.Content) != 2 {
		t.Fatalf("content = %d blocks: %s", len(message.Content), rendered)
	}
	if message.Content[0].Type != AnthropicContentBlockTypeToolUse {
		t.Fatalf("block 0 = %q", message.Content[0].Type)
	}
	if message.Content[1].Type != AnthropicContentBlockTypeText {
		t.Fatalf("block 1 = %q", message.Content[1].Type)
	}
	if message.Usage == nil || message.Usage.InputTokens != 45 || message.Usage.OutputTokens != 25 {
		t.Fatalf("usage = %+v", message.Usage)
	}
}

func TestRenderResponsesResponseFromChat(t *testing.T) {
	response, _, err := DecodeChatResponseWithPolicy(testcorpus.ChatCompletionsResponseJSON(), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "gpt-4.1"
	context.UpstreamModel = "gpt-4.1"
	rendered, _, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	var envelope ResponseEnvelope
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Object != "response" || envelope.Status != "completed" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Model != "gpt-4.1" {
		t.Fatalf("model = %q", envelope.Model)
	}
	if len(envelope.Output) != 1 {
		t.Fatalf("output = %d", len(envelope.Output))
	}
	message, ok := envelope.Output[0].(*ResponsesOutputMessage)
	if !ok {
		t.Fatalf("output[0] = %T", envelope.Output[0])
	}
	if len(message.Content) != 1 {
		t.Fatalf("content = %d", len(message.Content))
	}
	text, ok := message.Content[0].(*ResponsesOutputText)
	if !ok {
		t.Fatalf("content[0] = %T", message.Content[0])
	}
	if text.Annotations == nil {
		t.Fatal("annotations must be present")
	}
	if envelope.Usage == nil || envelope.Usage.TotalTokens != 60 {
		t.Fatalf("usage = %+v", envelope.Usage)
	}
}

func TestRenderResponsesResponseEcho(t *testing.T) {
	result, echo, err := DecodeResponsesRequest(
		testcorpus.ResponsesRequestJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = result
	response := CanonicalResponse{
		ID:        "resp_upstream",
		Model:     "gpt-4.1",
		CreatedAt: 1710000000,
		Status:    CanonicalResponseCompleted,
		Stop:      CanonicalStop{Reason: CanonicalStopEndTurn},
		Items: []CanonicalResponseItem{&CanonicalMessageItem{
			Role:  CanonicalAssistant,
			Parts: []CanonicalPart{CanonicalText{Text: "The weather is 21°C."}},
		}},
	}
	context := testExchangeContext()
	context.OriginalResponsesRequest = echo
	context.RequestedClientModel = "gpt-4.1"
	context.UpstreamModel = "gpt-4.1"
	rendered, _, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	var envelope ResponseEnvelope
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Instructions == nil {
		t.Fatal("instructions echo missing")
	}
	if envelope.MaxOutputTokens == nil || *envelope.MaxOutputTokens != 512 {
		t.Fatalf("max_output_tokens echo = %v", envelope.MaxOutputTokens)
	}
	if envelope.Temperature == nil || *envelope.Temperature != 0.7 {
		t.Fatalf("temperature echo = %v", envelope.Temperature)
	}
	if len(envelope.Tools) != 1 {
		t.Fatalf("tools echo = %d", len(envelope.Tools))
	}
	if envelope.ToolChoice == nil || envelope.ToolChoice.Str == nil || *envelope.ToolChoice.Str != "auto" {
		t.Fatalf("tool choice echo = %+v", envelope.ToolChoice)
	}
}

func TestRenderResponsesResponseFailed(t *testing.T) {
	response := CanonicalResponse{
		ID:           "r",
		Model:        "m",
		CreatedAt:    1,
		Status:       CanonicalResponseFailed,
		Stop:         CanonicalStop{Reason: CanonicalStopEndTurn},
		ErrorMessage: "upstream exploded",
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	rendered, _, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	var envelope ResponseEnvelope
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "failed" {
		t.Fatalf("status = %q", envelope.Status)
	}
	if envelope.Error == nil || envelope.Error.Message != "upstream exploded" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestModelMapResolve(t *testing.T) {
	m := ModelMap{
		Exact: map[string]ModelMapping{
			"claude-3": {ClientModel: "claude-3", UpstreamModel: "gpt-4o", ClientResponseModel: "claude-3"},
		},
		AllowIdentity: true,
	}
	mapping, err := m.Resolve("claude-3")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.UpstreamModel != "gpt-4o" || mapping.ClientResponseModel != "claude-3" {
		t.Fatalf("mapping = %+v", mapping)
	}
	mapping, err = m.Resolve("unknown-model")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.UpstreamModel != "unknown-model" {
		t.Fatalf("identity mapping = %+v", mapping)
	}

	strict := ModelMap{AllowIdentity: false, RequireExplicitMap: true}
	if _, err := strict.Resolve("nope"); err == nil {
		t.Fatal("expected mapping error")
	}

	// ClientResponseModel defaults to the client model.
	mapping, err = (ModelMap{Exact: map[string]ModelMapping{
		"a": {ClientModel: "a", UpstreamModel: "b"},
	}}).Resolve("a")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.ClientResponseModel != "a" {
		t.Fatalf("alias = %q", mapping.ClientResponseModel)
	}
}

func TestChatSchemaOfficialShapes(t *testing.T) {
	// reasoning_effort is a top-level string field, not a reasoning object.
	var request ChatRequest
	if err := strictDecode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`), &request); err != nil {
		t.Fatal(err)
	}
	if request.ReasoningEffort == nil || *request.ReasoningEffort != "medium" {
		t.Fatalf("reasoning_effort = %v", request.ReasoningEffort)
	}
	// A gateway-style reasoning object is rejected.
	if err := strictDecode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"medium"}}`), &request); err == nil {
		t.Fatal("expected rejection of reasoning object")
	}
	// Developer role is a first-class role.
	var dev ChatRequest
	if err := strictDecode([]byte(`{"model":"m","messages":[{"role":"developer","content":"rules"}]}`), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.Messages[0].Role != ChatMessageRoleDeveloper {
		t.Fatalf("role = %q", dev.Messages[0].Role)
	}
	// stop accepts string or array.
	var stop ChatRequest
	if err := strictDecode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`), &stop); err != nil {
		t.Fatal(err)
	}
	if stop.Stop == nil || stop.Stop.Str == nil || *stop.Stop.Str != "END" {
		t.Fatalf("stop = %+v", stop.Stop)
	}
	if err := strictDecode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`), &stop); err != nil {
		t.Fatal(err)
	}
	if stop.Stop.Strs == nil || len(stop.Stop.Strs) != 2 {
		t.Fatalf("stop = %+v", stop.Stop)
	}
}

func TestAnthropicSchemaStrict(t *testing.T) {
	var request anthropicmessages.Request
	// Unknown top-level fields are rejected.
	if err := strictDecode([]byte(`{"model":"m","max_tokens":10,"messages":[],"bogus":1}`), &request); err == nil {
		t.Fatal("expected unknown field rejection")
	}
	// String content is accepted.
	if err := strictDecode([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || request.Messages[0].Content.ContentStr == nil {
		t.Fatalf("messages = %+v", request.Messages)
	}
	// Thinking blocks are modeled (for preservation), never synthesized.
	var block AnthropicContentBlock
	if err := strictDecode([]byte(`{"type":"thinking","thinking":"x","signature":"s"}`), &block); err != nil {
		t.Fatal(err)
	}
	if err := block.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedFeatureErrorFormat(t *testing.T) {
	err := &UnsupportedFeatureError{Protocol: "responses", Path: "input[].type", Feature: "web_search_call"}
	msg := err.Error()
	if !strings.Contains(msg, "responses") || !strings.Contains(msg, "input[].type") || !strings.Contains(msg, "web_search_call") {
		t.Fatalf("message = %q", msg)
	}
}

func TestRenderChatImageInputCapability(t *testing.T) {
	body := testcorpus.AnthropicMessagesRequestJSON()
	// Image content is not in the stock fixture; build a request with one.
	var envelope struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					URL       string `json:"url"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	var withImage = struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}{Model: "m", MaxTokens: 10}
	withImage.Messages = append(withImage.Messages,
		struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]`)},
	)
	raw, err := json.Marshal(withImage)
	if err != nil {
		t.Fatal(err)
	}

	result, err := DecodeMessagesRequest(raw, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}

	// Without the capability the image is rejected (strict policy).
	if _, _, err := RenderChatRequest(
		result.Request,
		testExchangeContext(),
		ChatCapabilities{ParallelToolCalls: true},
	); err == nil {
		t.Fatal("image input accepted without the capability")
	}

	// With the capability the image renders as an image_url block.
	rendered, _, err := RenderChatRequest(
		result.Request,
		testExchangeContext(),
		ChatCapabilities{ParallelToolCalls: true, ImageInput: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "image_url") {
		t.Fatalf("rendered chat request lacks image_url: %s", rendered)
	}
	if !strings.Contains(string(rendered), `"detail":"auto"`) {
		t.Fatalf("rendered chat image lacks the auto detail default: %s", rendered)
	}
}

func TestRenderResponsesRequestImageDetailDefaultsAuto(t *testing.T) {
	// A canonical image without a detail must render with the API default
	// (auto) on the Responses wire.
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.com/a.png"}
	rendered, err := canonicalImageToResponsesInputImage(image)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Detail != "auto" {
		t.Fatalf("detail = %q, want auto", rendered.Detail)
	}
}

func TestDecodeMessagesRequestStrictness(t *testing.T) {
	// disable_parallel_tool_use: true is rejected under the strict policy.
	_, err := DecodeMessagesRequest(
		[]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("disable_parallel_tool_use accepted under the strict policy")
	}

	// Non-string metadata values are rejected, not dropped.
	_, err = DecodeMessagesRequest(
		[]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}],"metadata":{"when":123}}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("non-string metadata accepted")
	}
}

func TestDecodeChatResponseContentFilterIncomplete(t *testing.T) {
	// The non-streaming chat decode records content_filter as an incomplete
	// response with the official reason, plus the refusal stop reason.
	response, _, err := DecodeChatResponseWithPolicy(
		[]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != CanonicalResponseIncomplete {
		t.Fatalf("status = %v, want incomplete", response.Status)
	}
	if response.IncompleteReason != "content_filter" {
		t.Fatalf("incomplete reason = %q", response.IncompleteReason)
	}
	if response.Stop.Reason != CanonicalStopRefusal {
		t.Fatalf("stop reason = %v, want refusal", response.Stop.Reason)
	}
}

func TestChatStreamErrorFrameSurfaced(t *testing.T) {
	// An in-band chat error frame must be surfaced as a conversion error,
	// never ignored while the stream continues.
	converter := newChatToResponsesConverter(newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	))
	_, err := converter.Convert(SSEEvent{Data: []byte(
		`{"error":{"message":"upstream exploded"}}`,
	)})
	if err == nil {
		t.Fatal("chat error frame accepted")
	}
}

func TestDecodeResponsesResponseContentFilterRefusal(t *testing.T) {
	// The Responses-source non-streaming decode must render content_filter
	// as a refusal stop reason (parity with the streaming and Chat-source
	// paths), never clobbered back to max_tokens.
	body := `{"id":"resp_f","object":"response","created_at":1,"status":"incomplete","model":"m","incomplete_details":{"reason":"content_filter"},"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"filtered","annotations":[]}]}],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}`
	response, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != CanonicalResponseIncomplete {
		t.Fatalf("status = %v", response.Status)
	}
	if response.IncompleteReason != "content_filter" {
		t.Fatalf("incomplete reason = %q", response.IncompleteReason)
	}
	if response.Stop.Reason != CanonicalStopRefusal {
		t.Fatalf("stop reason = %v, want refusal", response.Stop.Reason)
	}
}

// TestResponsesToolMarshalUnionShape pins the response-echo marshal shape of
// the tool union (review-gate task-11 finding 1): a function tool always
// carries strict (the pinned contract marks it required on both the create
// request and the response echo), while built-in and namespace tools never
// do — a blanket struct marshal would invent strict:false on echoed
// built-in and namespace tools, bytes the strict decoder classifies as a
// contradictory union for namespaces. Every echoed shape must round-trip
// through the package's own strict decoder.
func TestResponsesToolMarshalUnionShape(t *testing.T) {
	marshal := func(t *testing.T, tool openairesponses.Tool) []byte {
		t.Helper()
		data, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return data
	}

	t.Run("function tool carries strict", func(t *testing.T) {
		strict := true
		data := marshal(t, openairesponses.Tool{
			Type: "function", Name: "f",
			Parameters: json.RawMessage(`{"type":"object"}`), Strict: wire.Field[bool]{Present: true, Value: strict},
		})
		if !strings.Contains(string(data), `"strict":true`) {
			t.Fatalf("function tool marshal = %s, want strict:true", data)
		}
		var decoded openairesponses.Tool
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("round-trip decode: %v", err)
		}
		if decoded.Type != "function" || decoded.Name != "f" || !decoded.Strict.Present || decoded.Strict.Value != true {
			t.Fatalf("round trip = %+v", decoded)
		}
	})

	t.Run("built-in tool never carries strict", func(t *testing.T) {
		data := marshal(t, openairesponses.Tool{Type: "web_search", Name: "ws"})
		if strings.Contains(string(data), "strict") {
			t.Fatalf("built-in tool marshal = %s, want no strict key", data)
		}
		var decoded openairesponses.Tool
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("round-trip decode: %v", err)
		}
		if decoded.Type != "web_search" || decoded.Name != "ws" {
			t.Fatalf("round trip = %+v", decoded)
		}
	})

	t.Run("namespace tool never carries strict", func(t *testing.T) {
		inner := true
		data := marshal(t, openairesponses.Tool{
			Type: "namespace", Name: "ns",
			Tools: []openairesponses.Tool{{
				Type: "function", Name: "f",
				Parameters: json.RawMessage(`{"type":"object"}`),
				Strict:     wire.Field[bool]{Present: true, Value: inner},
			}},
		})
		if strings.Count(string(data), "strict") != 1 {
			t.Fatalf("namespace tool marshal = %s, want strict only on the nested function tool", data)
		}
		// The echo must survive the package's own strict decoder: the old
		// blanket marshal invented strict:false here, which decodes as a
		// contradictory union.
		var decoded openairesponses.Tool
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("namespace echo failed its own strict decoder: %v", err)
		}
		if decoded.Type != "namespace" || len(decoded.Tools) != 1 || !decoded.Tools[0].Strict.Present {
			t.Fatalf("round trip = %+v", decoded)
		}
	})

	t.Run("echoed envelope tools round-trip through the strict decoder", func(t *testing.T) {
		// The client-facing response envelope echoes the original tools;
		// the full envelope path must not invent union-inconsistent bytes.
		strict := true
		envelope := openairesponses.Response{
			ID: "resp_1", Object: "response", CreatedAt: 1,
			Status: "completed", Model: "m",
			Output: []openairesponses.OutputItem{},
			Tools: []openairesponses.Tool{
				{Type: "web_search", Name: "ws"},
				{Type: "namespace", Name: "ns", Tools: []openairesponses.Tool{{
					Type: "function", Name: "f",
					Parameters: json.RawMessage(`{"type":"object"}`),
					Strict:     wire.Field[bool]{Present: true, Value: strict},
				}}},
			},
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		var decoded openairesponses.Response
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("envelope echo failed its own strict decoder: %v", err)
		}
		if len(decoded.Tools) != 2 || decoded.Tools[0].Type != "web_search" || decoded.Tools[1].Type != "namespace" {
			t.Fatalf("round trip tools = %+v", decoded.Tools)
		}
	})
}

// TestAnthropicNestedOnlyCacheControlNoted pins the recursive cache_control
// scan (review-gate task-11 finding 2): a marker that appears ONLY inside
// nested tool_result content (no top-level block carries it) still produces
// exactly one deduped anthropic_controls note per exchange.
func TestAnthropicNestedOnlyCacheControlNoted(t *testing.T) {
	body := []byte(`{
		"model": "claude-x",
		"max_tokens": 16,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [
					{"type": "text", "text": "nested", "cache_control": {"type": "ephemeral"}}
				]}
			]}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("strict decode rejected a nested-only cache_control marker: %v", err)
	}
	count := 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicControls && loss.Path == "cache_control" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cache_control notes = %d, want exactly 1: %+v", count, result.Report.Losses)
	}

	// Multiple nested markers still dedupe to one note.
	bodyMulti := []byte(`{
		"model": "claude-x",
		"max_tokens": 16,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [
					{"type": "text", "text": "a", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "b", "cache_control": {"type": "ephemeral"}}
				]},
				{"type": "tool_result", "tool_use_id": "call_2", "content": [
					{"type": "text", "text": "c", "cache_control": {"type": "ephemeral"}}
				]}
			]}
		]
	}`)
	result, err = DecodeMessagesRequest(bodyMulti, StrictLossPolicy())
	if err != nil {
		t.Fatalf("strict decode rejected multiple nested markers: %v", err)
	}
	count = 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicControls && loss.Path == "cache_control" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cache_control notes = %d, want exactly 1 (deduped): %+v", count, result.Report.Losses)
	}
}

// TestDecodeResponsesRequestAllBuiltinNamespace pins the top-level parity of
// a namespace whose nested tools are ALL built-ins (review-gate task-11
// finding 6): under an approved builtin_tools loss the namespace drops
// observably exactly like a top-level all-built-in tools list (tool_choice
// reconciliation owns the no-tools-left case), and under a strict policy it
// is the same typed builtin_tools rejection — never a different, harder
// error.
func TestDecodeResponsesRequestAllBuiltinNamespace(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"input": "hi",
		"tools": [
			{"type": "namespace", "name": "ns", "tools": [
				{"type": "web_search", "name": "ws"},
				{"type": "file_search", "name": "fs"}
			]}
		]
	}`)

	// Approved: accept-and-drop, identical to the top-level all-built-in
	// rule; a tool-less request renders (tool_choice defaults to none set).
	result, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureBuiltinTools: {},
	}})
	if err != nil {
		t.Fatalf("approved policy rejected an all-built-in namespace: %v", err)
	}
	if len(result.Request.Tools) != 0 {
		t.Fatalf("tools = %+v, want none to survive", result.Request.Tools)
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureBuiltinTools &&
			(strings.Contains(loss.Detail, "web_search") || strings.Contains(loss.Detail, "file_search")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want an observable builtin_tools loss naming the dropped built-ins", result.Report.Losses)
	}

	// The zero-flatten itself is observable too: the namespace produced no
	// portable function tools, so the report carries an explicit note saying
	// so rather than an unremarked empty tools list (review-gate task-12
	// finding 4).
	foundZeroFlatten := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureBuiltinTools &&
			loss.Path == "tools[]" &&
			strings.Contains(loss.Detail, "no portable function tools") {
			foundZeroFlatten = true
		}
	}
	if !foundZeroFlatten {
		t.Fatalf("report = %+v, want a builtin_tools note on tools[] recording the zero-flatten", result.Report.Losses)
	}

	// Strict: the same typed builtin_tools rejection as a top-level
	// built-in tool.
	_, _, err = DecodeResponsesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("strict policy accepted an all-built-in namespace; want rejection")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("error = %T: %v, want UnsupportedFeatureError", err, err)
	}
	if target.Feature != string(FeatureBuiltinTools) {
		t.Fatalf("feature = %q, want builtin_tools", target.Feature)
	}
}

func TestDecodeResponsesRequestCodexTurnTwoHistory(t *testing.T) {
	// Byte-faithful replay of the field-observed Codex turn-2 shape
	// (autopsy 01): the assistant history item carries output_text with NO
	// annotations key. Pre-fix this failed locally in 14ms with
	// "output_text annotations must be present; use an empty array".
	body := []byte(`{
		"model":"gpt-5.1",
		"instructions":"You are helpful.",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 1"}]},
			{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 2"}]}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// instructions + three input items.
	if len(result.Request.Turns) != 4 {
		t.Fatalf("turns = %d", len(result.Request.Turns))
	}
	assistant := result.Request.Turns[2]
	if assistant.Role != CanonicalAssistant {
		t.Fatalf("turn 2 role = %q", assistant.Role)
	}
	text, ok := assistant.Parts[0].(CanonicalText)
	if !ok || text.Text != "hi" {
		t.Fatalf("assistant parts = %+v", assistant.Parts)
	}

	// The decoded history renders a chat request carrying the converted
	// assistant turn — Codex sessions survive turn 2+.
	rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("render chat: %v", err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	found := false
	for _, message := range chat.Messages {
		if message.Role != ChatMessageRoleAssistant || message.Content == nil {
			continue
		}
		for _, block := range message.Content.ContentBlocks {
			if block.Text != nil && *block.Text == "hi" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("converted assistant history missing from %s", rendered)
	}
}

func TestDecodeResponsesRequestIdStrippedAssistantHistory(t *testing.T) {
	// An assistant history turn stripped of id/status routes to the easy
	// message arm (autopsy 01 §3.3) and carries output-type parts.
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"q"},
			{"role":"assistant","content":[{"type":"output_text","text":"a"}]},
			{"type":"message","role":"user","content":"follow-up"}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Request.Turns) != 3 {
		t.Fatalf("turns = %d", len(result.Request.Turns))
	}
	assistant := result.Request.Turns[1]
	if assistant.Role != CanonicalAssistant {
		t.Fatalf("turn 1 role = %q", assistant.Role)
	}
	text, ok := assistant.Parts[0].(CanonicalText)
	if !ok || text.Text != "a" {
		t.Fatalf("assistant parts = %+v", assistant.Parts)
	}

	if _, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{}); err != nil {
		t.Fatalf("render chat: %v", err)
	}
}

func TestDecodeResponsesRequestFunctionOutputOutputPartRejected(t *testing.T) {
	// Review F1 pin (decode level): output-type parts stay rejected inside
	// function_call_output payloads. Decode precedes loss-policy
	// evaluation, so StrictLossPolicy() here covers every policy: no policy
	// can reach this rejection at all.
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"q"},
			{"type":"function_call_output","call_id":"c","output":[{"type":"output_text","text":"x"}]}
		]
	}`)
	if _, _, err := DecodeResponsesRequest(body, StrictLossPolicy()); err == nil {
		t.Fatal("strict policy accepted an output_text function-output part")
	}
}

func TestRenderChatRequestMidConversationSystemConsolidates(t *testing.T) {
	// Field-observed Claude Code shape (autopsy 02): envelope.system plus an
	// inline role:system message AFTER dialog turns. Positional rendering
	// puts role:system at index >0 — the exact shape Qwen/Llama/DeepSeek
	// Jinja templates reject with "System message must be at the
	// beginning." The chat render must never emit a system message after
	// index 0: system-channel turns consolidate into one leading system
	// message, and the position loss is policy-gated.
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"system":[{"type":"text","text":"top"}],
		"messages":[
			{"role":"user","content":"first"},
			{"role":"system","content":"inline"},
			{"role":"user","content":"second"}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}

	// Strict programmatic policy: the position loss is a client-dialect
	// error.
	if _, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{}); err == nil {
		t.Fatal("strict render accepted a mid-conversation system turn; want rejection")
	}

	// Approved policy: exactly one leading system message carrying all
	// system-channel text in order, and exactly one recorded loss.
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem: {},
	}}
	rendered, report, err := RenderChatRequest(result.Request, permissive, ChatCapabilities{})
	if err != nil {
		t.Fatalf("permissive render: %v", err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	// The Jinja property: no system-role message after index 0.
	for i, message := range chat.Messages {
		if i > 0 && message.Role == ChatMessageRoleSystem {
			t.Fatalf(
				"system message at index %d violates the single-leading-system property: %s",
				i, rendered,
			)
		}
	}
	if len(chat.Messages) != 3 ||
		chat.Messages[0].Role != ChatMessageRoleSystem ||
		chat.Messages[1].Role != ChatMessageRoleUser ||
		chat.Messages[2].Role != ChatMessageRoleUser {
		t.Fatalf("messages = %+v, want [system user user]", chat.Messages)
	}
	var texts []string
	for _, block := range chat.Messages[0].Content.ContentBlocks {
		if block.Text != nil {
			texts = append(texts, *block.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "top\n\ninline" {
		t.Fatalf("consolidated system content = %q, want [\"top\\n\\ninline\"]", texts)
	}
	found := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("mid_conversation_system entries = %d, want exactly 1: %+v", found, report.Losses)
	}
}

func TestRenderChatRequestLeadingSystemPairMergesWithNote(t *testing.T) {
	// Two LEADING system turns (envelope.system + an inline system before
	// any dialog) also consolidate: open-weights Jinja templates reject a
	// second system message even at index 1. Leading-only consolidation is
	// a sanctioned note under the same key — observable without a policy
	// approval, so the strict render succeeds with the note recorded.
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"system":[{"type":"text","text":"top"}],
		"messages":[
			{"role":"system","content":"inline"},
			{"role":"user","content":"hi"}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rendered, report, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("strict render rejected a leading-only system merge: %v", err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	for i, message := range chat.Messages {
		if i > 0 && message.Role == ChatMessageRoleSystem {
			t.Fatalf("system message at index %d: %s", i, rendered)
		}
	}
	if len(chat.Messages) != 2 ||
		chat.Messages[0].Role != ChatMessageRoleSystem ||
		chat.Messages[1].Role != ChatMessageRoleUser {
		t.Fatalf("messages = %+v, want [system user]", chat.Messages)
	}
	var texts []string
	for _, block := range chat.Messages[0].Content.ContentBlocks {
		if block.Text != nil {
			texts = append(texts, *block.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "top\n\ninline" {
		t.Fatalf("merged system content = %q, want [\"top\\n\\ninline\"]", texts)
	}
	found := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("mid_conversation_system entries = %d, want exactly 1: %+v", found, report.Losses)
	}
}

func TestRenderChatRequestSingleLeadingSystemUnchanged(t *testing.T) {
	// The common case — exactly one leading system turn — renders exactly
	// as before, with zero report entries: consolidation never adds noise
	// to requests that were already Jinja-safe.
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"system":[{"type":"text","text":"top"}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rendered, report, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	if len(chat.Messages) != 2 ||
		chat.Messages[0].Role != ChatMessageRoleSystem ||
		chat.Messages[1].Role != ChatMessageRoleUser {
		t.Fatalf("messages = %+v, want [system user]", chat.Messages)
	}
	if len(chat.Messages[0].Content.ContentBlocks) != 1 ||
		*chat.Messages[0].Content.ContentBlocks[0].Text != "top" {
		t.Fatalf("system content = %+v, want the original single block", chat.Messages[0].Content)
	}
	if len(report.Losses) != 0 {
		t.Fatalf("report entries = %+v, want none", report.Losses)
	}
}

func TestRenderChatRequestSystemAnywhereCapability(t *testing.T) {
	// The system_anywhere capability restores positional rendering for
	// upstreams that accept system messages anywhere (e.g. genuine
	// OpenAI), with zero report entries.
	body := []byte(`{
		"model":"m",
		"max_tokens":100,
		"system":[{"type":"text","text":"top"}],
		"messages":[
			{"role":"user","content":"first"},
			{"role":"system","content":"inline"},
			{"role":"user","content":"second"}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rendered, report, err := RenderChatRequest(
		result.Request,
		testExchangeContext(),
		ChatCapabilities{SystemAnywhere: true},
	)
	if err != nil {
		t.Fatalf("system-anywhere render: %v", err)
	}
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	if len(chat.Messages) != 4 ||
		chat.Messages[0].Role != ChatMessageRoleSystem ||
		chat.Messages[1].Role != ChatMessageRoleUser ||
		chat.Messages[2].Role != ChatMessageRoleSystem ||
		chat.Messages[3].Role != ChatMessageRoleUser {
		t.Fatalf("messages = %+v, want positional [system user system user]", chat.Messages)
	}
	if len(report.Losses) != 0 {
		t.Fatalf("report entries = %+v, want none", report.Losses)
	}
}

// TestDecodeResponsesRequestPreviousOutputEmptyStatus (field regression
// 2026-08-24, task 30): codex resume traffic carries previous-output history
// items with "status": "" (observed live against the yolo gateway: 'convert
// request: responses request: wire: malformed: input item 3: invalid previous
// output status ""'). The sibling input items treat an absent status as
// optional, and the item identity is carried by the id; status is not read
// downstream. An empty status must decode; a bogus non-empty status still
// rejects.
func TestDecodeResponsesRequestPreviousOutputEmptyStatus(t *testing.T) {
	body := []byte(`{
		"model":"qwen3.8-27b",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 1"}]},
			{"type":"message","id":"msg_9","role":"assistant","status":"","content":[{"type":"output_text","text":"hi","annotations":[]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 2"}]}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Request.Turns) != 3 {
		t.Fatalf("turns = %d", len(result.Request.Turns))
	}
	if result.Request.Turns[1].Role != CanonicalAssistant {
		t.Fatalf("turn 1 role = %q", result.Request.Turns[1].Role)
	}

	// Strictness pin: a malformed non-empty status is still a typed
	// rejection on the same surface.
	bogus := []byte(`{
		"model":"qwen3.8-27b",
		"input":[
			{"type":"message","id":"msg_9","role":"assistant","status":"bogus","content":[{"type":"output_text","text":"hi","annotations":[]}]}
		]
	}`)
	if _, _, err := DecodeResponsesRequest(bogus, StrictLossPolicy()); err == nil {
		t.Fatal("bogus status accepted")
	}
}
