package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
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
	if echo.Temperature == nil || *echo.Temperature != 0.7 {
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
		{"include", `{"model":"m","input":"x","include":["reasoning"]}`},
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

func TestDecodeResponsesRequestToolChoiceRequiredRejected(t *testing.T) {
	// Responses tool_choice "required" is not representable; the strict
	// contract rejects it rather than silently weakening to auto.
	_, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"x","tool_choice":"required"}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("expected rejection of required tool_choice")
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
	rendered, _, err := RenderResponsesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatal(err)
	}
	var envelope responsesRequestEnvelope
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
	response, err := DecodeChatResponse(testcorpus.ChatCompletionsResponseJSON(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "chatcmpl_8xyz" {
		t.Fatalf("id = %q", response.ID)
	}
	if response.Status != CanonicalResponseCompleted {
		t.Fatalf("status = %q", response.Status)
	}
	if response.StopReason != CanonicalStopEndTurn {
		t.Fatalf("stop reason = %q", response.StopReason)
	}
	if response.Usage.InputTokens != 42 || response.Usage.OutputTokens != 18 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if len(response.Turns) != 1 {
		t.Fatalf("turns = %d", len(response.Turns))
	}
	text, ok := response.Turns[0].Parts[0].(CanonicalText)
	if !ok {
		t.Fatalf("part = %T", response.Turns[0].Parts[0])
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
	if _, err := DecodeChatResponse(body, ChatCapabilities{}); err == nil {
		t.Fatal("expected provider reasoning rejection")
	}
	// With the capability: mapped to ordinary text.
	response, err := DecodeChatResponse(body, ChatCapabilities{ProviderReasoningText: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, part := range response.Turns[0].Parts {
		if text, ok := part.(CanonicalText); ok && text.Text == "think" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning text missing: %+v", response.Turns[0].Parts)
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
	if _, err := DecodeChatResponse(body, ChatCapabilities{}); err == nil {
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
	response, err := DecodeChatResponse(body, ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != CanonicalStopToolUse {
		t.Fatalf("stop reason = %q", response.StopReason)
	}
	var found *CanonicalFunctionCall
	for _, part := range response.Turns[0].Parts {
		if call, ok := part.(CanonicalFunctionCall); ok {
			found = &call
			break
		}
	}
	if found == nil {
		t.Fatalf("no function call in parts: %+v", response.Turns[0].Parts)
	}
	if found.CallID != "call_1" || found.Name != "f" {
		t.Fatalf("call = %+v", found)
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
	if response.StopReason != CanonicalStopToolUse {
		t.Fatalf("stop reason = %q (fixture has function_call)", response.StopReason)
	}
	if response.Usage.InputTokens != 45 || response.Usage.OutputTokens != 25 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	// The fixture output: reasoning item, function call, function call
	// output, message. The function call is an assistant part, the result a
	// user part, the message text an assistant part.
	if len(response.Turns) != 3 {
		t.Fatalf("turns = %d, want 3: %+v", len(response.Turns), response.Turns)
	}
	call, ok := response.Turns[0].Parts[0].(CanonicalFunctionCall)
	if !ok {
		t.Fatalf("turn 0 part = %T", response.Turns[0].Parts[0])
	}
	if call.CallID != "call_abc123" || call.Name != "get_weather" {
		t.Fatalf("call = %+v", call)
	}
	if response.Turns[1].Role != CanonicalUser {
		t.Fatalf("turn 1 role = %q", response.Turns[1].Role)
	}
	if response.Turns[2].Role != CanonicalAssistant {
		t.Fatalf("turn 2 role = %q", response.Turns[2].Role)
	}
	if len(response.ReasoningItems) != 1 {
		t.Fatalf("reasoning items = %d", len(response.ReasoningItems))
	}
}

func TestRenderMessagesResponseReasoningLoss(t *testing.T) {
	// The fixture has a reasoning item; rendering to Messages requires a loss,
	// so under the strict policy rendering fails.
	response, err := DecodeResponsesResponse(testcorpus.ResponsesResponseJSON())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.ReasoningItems) != 1 {
		t.Fatalf("reasoning items = %d", len(response.ReasoningItems))
	}
	if _, err := RenderMessagesResponse(response, testExchangeContext()); err == nil {
		t.Fatal("expected reasoning loss rejection under strict policy")
	}
	// With the losses approved, rendering succeeds.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary:  {},
		FeatureConversationState: {},
	}}
	if _, err := RenderMessagesResponse(response, context); err != nil {
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
		FeatureReasoningSummary:  {},
		FeatureConversationState: {},
	}}
	rendered, err := RenderMessagesResponse(response, context)
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
	if message.StopReason != AnthropicStopReasonToolUse {
		t.Fatalf("stop reason = %q", message.StopReason)
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
	response, err := DecodeChatResponse(testcorpus.ChatCompletionsResponseJSON(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "gpt-4.1"
	context.UpstreamModel = "gpt-4.1"
	rendered, err := RenderResponsesResponse(response, context)
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
		ID:         "resp_upstream",
		Model:      "gpt-4.1",
		CreatedAt:  1710000000,
		Status:     CanonicalResponseCompleted,
		StopReason: CanonicalStopEndTurn,
		Turns: []CanonicalTurn{{
			Role:  CanonicalAssistant,
			Parts: []CanonicalPart{CanonicalText{Text: "The weather is 21°C."}},
		}},
	}
	context := testExchangeContext()
	context.OriginalResponsesRequest = echo
	context.RequestedClientModel = "gpt-4.1"
	context.UpstreamModel = "gpt-4.1"
	rendered, err := RenderResponsesResponse(response, context)
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
		StopReason:   CanonicalStopEndTurn,
		ErrorMessage: "upstream exploded",
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	rendered, err := RenderResponsesResponse(response, context)
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
	var request messagesRequestEnvelope
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
}
