package transcode

// Review-z commit 2 acceptance tests for the semantic IR: the multimodal
// tool-result matrix, the role-invariant negative matrix, tool-argument
// fidelity (byte-exact to Chat and Responses, unrepresentable to Messages,
// never an upstream failure), and the empty-conversation rejection.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// toolResultPermissivePolicy approves the two tool-result loss keys.
func toolResultPermissivePolicy() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
	}}
}

// TestRenderChatToolResultTextExact proves exact text results stay exact
// text in the Chat tool message (the string arm, never a block array).
func TestRenderChatToolResultTextExact(t *testing.T) {
	var report ConversionReport
	message, err := renderChatToolResult(CanonicalFunctionResult{
		CallID: "call_1",
		Parts:  []CanonicalPart{CanonicalText{Text: "analysis complete"}},
	}, StrictLossPolicy(), &report)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content == nil || message.Content.ContentStr == nil ||
		*message.Content.ContentStr != "analysis complete" {
		t.Fatalf("content = %+v, want the exact text", message.Content)
	}
	if message.Content.ContentBlocks != nil {
		t.Fatal("text result rendered as blocks")
	}
	if message.ToolCallID == nil || *message.ToolCallID != "call_1" {
		t.Fatalf("tool_call_id = %v", message.ToolCallID)
	}
}

// TestRenderChatToolResultMultimodalMatrix proves image/document/mixed
// results are rejected under strict policy with a local UnrepresentableError
// and, under the approved permissions, encoded as ONE deterministic
// tool_result_json_envelope text — never invalid image_url content inside a
// tool-role message.
func TestRenderChatToolResultMultimodalMatrix(t *testing.T) {
	multimodal := func() CanonicalFunctionResult {
		return CanonicalFunctionResult{
			CallID: "call_1",
			Parts: []CanonicalPart{
				CanonicalText{Text: "analysis complete"},
				CanonicalImage{
					MediaType: "image/png",
					URL:       "https://example.test/x.png",
				},
			},
		}
	}

	// Strict policy: rejected with a LOCAL unrepresentable error.
	var strictReport ConversionReport
	_, err := renderChatToolResult(multimodal(), StrictLossPolicy(), &strictReport)
	if err == nil {
		t.Fatal("strict policy accepted multimodal tool-result content")
	}
	var unrepresentable *UnrepresentableError
	if !errors.As(err, &unrepresentable) {
		t.Fatalf("err = %T %v, want *UnrepresentableError", err, err)
	}
	var wireErr *UpstreamWireError
	if errors.As(err, &wireErr) {
		t.Fatal("invalid model output must never classify as corrupt upstream wire")
	}

	// Permissive: the deterministic envelope encoding.
	var report ConversionReport
	message, err := renderChatToolResult(multimodal(), toolResultPermissivePolicy(), &report)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content == nil || message.Content.ContentStr == nil {
		t.Fatalf("content = %+v, want the envelope text", message.Content)
	}
	if message.Content.ContentBlocks != nil {
		t.Fatal("envelope encoded as blocks, not a text string")
	}
	envelopeText := *message.Content.ContentStr
	var envelope struct {
		TranscodeVersion int `json:"transcode_version"`
		Content          []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			MediaType string `json:"media_type"`
			URL       string `json:"url"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(envelopeText), &envelope); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\n%s", err, envelopeText)
	}
	if envelope.TranscodeVersion != 1 {
		t.Fatalf("transcode_version = %d, want 1", envelope.TranscodeVersion)
	}
	if len(envelope.Content) != 2 {
		t.Fatalf("content entries = %d, want 2", len(envelope.Content))
	}
	if envelope.Content[0].Type != "text" || envelope.Content[0].Text != "analysis complete" {
		t.Fatalf("entry 0 = %+v", envelope.Content[0])
	}
	if envelope.Content[1].Type != "image" || envelope.Content[1].MediaType != "image/png" ||
		envelope.Content[1].URL != "https://example.test/x.png" {
		t.Fatalf("entry 1 = %+v", envelope.Content[1])
	}
	if !reportHasFeature(report, FeatureToolResultMultimodalContent) ||
		!reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("report lacks the tool-result losses: %+v", report)
	}

	// A base64 image encodes as a data URL in the envelope.
	var report2 ConversionReport
	base64Result := CanonicalFunctionResult{
		CallID: "call_2",
		Parts: []CanonicalPart{CanonicalImage{
			MediaType: "image/png",
			Base64:    "aGk=",
		}},
	}
	message2, err := renderChatToolResult(base64Result, toolResultPermissivePolicy(), &report2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*message2.Content.ContentStr, "data:image/png;base64,aGk=") {
		t.Fatalf("base64 image not encoded as a data URL: %s", *message2.Content.ContentStr)
	}

	// A document result encodes as a document entry.
	var report3 ConversionReport
	documentResult := CanonicalFunctionResult{
		CallID: "call_3",
		Parts: []CanonicalPart{CanonicalDocument{
			MediaType: "application/pdf",
			URL:       "https://example.test/d.pdf",
		}},
	}
	message3, err := renderChatToolResult(documentResult, toolResultPermissivePolicy(), &report3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*message3.Content.ContentStr, `"type":"document"`) ||
		!strings.Contains(*message3.Content.ContentStr, "https://example.test/d.pdf") {
		t.Fatalf("document envelope = %s", *message3.Content.ContentStr)
	}

	// A base64 document encodes as a data URL.
	var report4 ConversionReport
	documentBase64 := CanonicalFunctionResult{
		CallID: "call_4",
		Parts: []CanonicalPart{CanonicalDocument{
			MediaType: "application/pdf",
			Base64:    "JVBERi0x",
		}},
	}
	message4, err := renderChatToolResult(documentBase64, toolResultPermissivePolicy(), &report4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*message4.Content.ContentStr, "data:application/pdf;base64,JVBERi0x") {
		t.Fatalf("base64 document envelope = %s", *message4.Content.ContentStr)
	}

	// A base64 image with an unsupported media type is an encoding error.
	var report5 ConversionReport
	badMedia := CanonicalFunctionResult{
		CallID: "call_5",
		Parts: []CanonicalPart{CanonicalImage{
			MediaType: "application/pdf",
			Base64:    "aGk=",
		}},
	}
	if _, err := renderChatToolResult(badMedia, toolResultPermissivePolicy(), &report5); err == nil {
		t.Fatal("unsupported image media type accepted")
	}
}

// TestRoleInvariantNegativeMatrix proves the central role validation rejects
// every role violation before any renderer sees the IR.
func TestRoleInvariantNegativeMatrix(t *testing.T) {
	valid := func() CanonicalResponse {
		return CanonicalResponse{
			ID:     "resp_1",
			Model:  "m",
			Status: CanonicalResponseCompleted,
			Stop:   CanonicalStop{Reason: CanonicalStopToolUse},
			Items: []CanonicalResponseItem{
				&CanonicalFunctionCallItem{
					CallID: "call_1",
					Name:   "f",
					Arguments: ToolArguments{
						Raw:      `{}`,
						Object:   json.RawMessage(`{}`),
						IsObject: true,
					},
				},
				&CanonicalFunctionResultItem{
					CallID: "call_1",
					Parts:  []CanonicalPart{CanonicalText{Text: "ok"}},
				},
				&CanonicalMessageItem{
					Role:  CanonicalAssistant,
					Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
				},
			},
		}
	}
	cases := []struct {
		name   string
		mutate func(*CanonicalResponse)
	}{
		{"function call item without call id", func(r *CanonicalResponse) {
			r.Items[0].(*CanonicalFunctionCallItem).CallID = ""
		}},
		{"function call item without name", func(r *CanonicalResponse) {
			r.Items[0].(*CanonicalFunctionCallItem).Name = ""
		}},
		{"result references unknown call", func(r *CanonicalResponse) {
			r.Items[1].(*CanonicalFunctionResultItem).CallID = "nope"
		}},
		{"result without call id", func(r *CanonicalResponse) {
			r.Items[1].(*CanonicalFunctionResultItem).CallID = ""
		}},
		{"message item with user role", func(r *CanonicalResponse) {
			r.Items[2].(*CanonicalMessageItem).Role = CanonicalUser
		}},
		{"message item with function call part", func(r *CanonicalResponse) {
			r.Items[2].(*CanonicalMessageItem).Parts = []CanonicalPart{
				CanonicalFunctionCall{CallID: "c", Name: "f", Arguments: json.RawMessage(`{}`)},
			}
		}},
		{"message item with invalid phase", func(r *CanonicalResponse) {
			r.Items[2].(*CanonicalMessageItem).Phase = Optional[string]{Value: "bogus", Set: true}
		}},
		{"reasoning item with invalid JSON", func(r *CanonicalResponse) {
			r.Items = append(r.Items, &CanonicalReasoningItem{Raw: json.RawMessage(`{`)})
		}},
		{"message item with image part", func(r *CanonicalResponse) {
			r.Items[2].(*CanonicalMessageItem).Parts = []CanonicalPart{
				CanonicalImage{MediaType: "image/png", URL: "https://example.test/x.png"},
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response := valid()
			c.mutate(&response)
			if err := ValidateCanonicalResponse(response); err == nil {
				t.Fatal("role violation accepted by the IR validator")
			}
		})
	}
	// The valid base passes.
	if err := ValidateCanonicalResponse(valid()); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
}

// TestRequestRoleInvariantNegativeMatrix proves the request-side role rules:
// function calls only in assistant turns, results only in user turns,
// refusal only on assistant messages, and exactly one image/document source.
func TestRequestRoleInvariantNegativeMatrix(t *testing.T) {
	valid := func() CanonicalRequest {
		return CanonicalRequest{
			ClientModel: "m",
			Turns: []CanonicalTurn{
				{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
				{Role: CanonicalAssistant, Parts: []CanonicalPart{
					CanonicalText{Text: "sure"},
					CanonicalFunctionCall{
						CallID:    "call_1",
						Name:      "f",
						Arguments: json.RawMessage(`{}`),
					},
				}},
				{Role: CanonicalUser, Parts: []CanonicalPart{
					CanonicalFunctionResult{
						CallID: "call_1",
						Parts:  []CanonicalPart{CanonicalText{Text: "ok"}},
					},
				}},
			},
		}
	}
	cases := []struct {
		name   string
		mutate func(*CanonicalRequest)
	}{
		{"call in user turn", func(r *CanonicalRequest) {
			r.Turns[0].Parts = append(r.Turns[0].Parts, CanonicalFunctionCall{
				CallID: "c", Name: "f", Arguments: json.RawMessage(`{}`),
			})
		}},
		{"result in assistant turn", func(r *CanonicalRequest) {
			r.Turns[1].Parts = append(r.Turns[1].Parts, CanonicalFunctionResult{
				CallID: "call_1", Parts: []CanonicalPart{CanonicalText{Text: "x"}},
			})
		}},
		{"refusal in user turn", func(r *CanonicalRequest) {
			r.Turns[0].Parts = append(r.Turns[0].Parts, CanonicalRefusal{Text: "no"})
		}},
		{"image with two sources", func(r *CanonicalRequest) {
			r.Turns[0].Parts = append(r.Turns[0].Parts, CanonicalImage{
				MediaType: "image/png", URL: "u", Base64: "b",
			})
		}},
		{"image with no sources", func(r *CanonicalRequest) {
			r.Turns[0].Parts = append(r.Turns[0].Parts, CanonicalImage{MediaType: "image/png"})
		}},
		{"document with two sources", func(r *CanonicalRequest) {
			r.Turns[0].Parts = append(r.Turns[0].Parts, CanonicalDocument{
				MediaType: "application/pdf", URL: "u", FileID: "f",
			})
		}},
		{"document with no sources", func(r *CanonicalRequest) {
			r.Turns[0].Parts = append(r.Turns[0].Parts, CanonicalDocument{MediaType: "application/pdf"})
		}},
		{"call with invalid arguments", func(r *CanonicalRequest) {
			r.Turns[1].Parts = append(r.Turns[1].Parts, CanonicalFunctionCall{
				CallID: "c", Name: "f", Arguments: json.RawMessage(`not json`),
			})
		}},
		{"call with empty call id", func(r *CanonicalRequest) {
			r.Turns[1].Parts = append(r.Turns[1].Parts, CanonicalFunctionCall{
				Name: "f", Arguments: json.RawMessage(`{}`),
			})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			request := valid()
			c.mutate(&request)
			if err := ValidateCanonicalRequest(request); err == nil {
				t.Fatal("role violation accepted by the request validator")
			}
		})
	}
	if err := ValidateCanonicalRequest(valid()); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

// TestToolArgumentsFidelityAcrossTargets proves invalid model-generated tool
// arguments convert byte-exact to Chat and Responses, produce a local
// unrepresentable error to Messages, and never classify as upstream failure.
func TestToolArgumentsFidelityAcrossTargets(t *testing.T) {
	// The model-generated arguments are NOT valid JSON: the wire carries
	// them as a JSON-escaped string ({\"city\":), and the decoded Raw is
	// the unescaped original ({"city":) preserved byte-exact.
	const invalidRaw = `{"city":`
	const invalidRawEscaped = `{\"city\":`
	response, _, err := DecodeChatResponseWithPolicy([]byte(
		`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"`+invalidRawEscaped+`"}}]}}]}`,
	), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	call, ok := response.Items[0].(*CanonicalFunctionCallItem)
	if !ok {
		t.Fatalf("item = %T", response.Items[0])
	}
	if call.Arguments.Raw != invalidRaw || call.Arguments.IsObject {
		t.Fatalf("arguments = %+v, want the raw string preserved unparsed", call.Arguments)
	}

	// Responses target: byte-exact in the function_call arguments field.
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
	item, ok := envelope.Output[0].(*ResponsesFunctionCallOutputItem)
	if !ok {
		t.Fatalf("output item = %T", envelope.Output[0])
	}
	if item.Arguments != invalidRaw {
		t.Fatalf("responses arguments = %q, want the raw %q preserved byte-exact", item.Arguments, invalidRaw)
	}

	// Messages target: local unrepresentable output, never corrupt wire.
	context = testExchangeContext()
	context.RequestedClientModel = "m"
	_, _, err = RenderMessagesResponse(response, context)
	if err == nil {
		t.Fatal("messages render accepted non-object tool arguments")
	}
	var unrepresentable *UnrepresentableError
	if !errors.As(err, &unrepresentable) {
		t.Fatalf("err = %T %v, want *UnrepresentableError", err, err)
	}
	var wireErr *UpstreamWireError
	if errors.As(err, &wireErr) {
		t.Fatal("invalid model arguments must never classify as upstream wire failure")
	}
}

// TestHandlerInvalidToolArgumentsNeverUpstreamFailure proves the outcome
// record: an exchange whose only defect is invalid model-generated tool
// arguments records a local failure, never an upstream failure (the circuit
// breaker stays neutral).
func TestHandlerInvalidToolArgumentsNeverUpstreamFailure(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}

	var outcomes []Outcome
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"city\":"}}]}}]}`,
				)),
			}, nil
		},
		func(o Outcome) { outcomes = append(outcomes, o) },
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d", len(outcomes))
	}
	if outcomes[0].UpstreamFailure {
		t.Fatal("invalid model-generated arguments recorded as an upstream failure")
	}
	if outcomes[0].Provenance == ProvenanceUpstreamBodyError {
		t.Fatalf("provenance = %s, want a local provenance", outcomes[0].Provenance)
	}
}

// TestRenderChatRequestEmptyConversationRejected proves a source request with
// no Chat-representable messages fails client-dialect before the upstream
// call — messages:null or an invented empty user prompt are never emitted.
func TestRenderChatRequestEmptyConversationRejected(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	if _, _, err := RenderChatRequest(request, context, ChatCapabilities{}); err == nil {
		t.Fatal("empty conversation rendered")
	} else if !strings.Contains(err.Error(), "no Chat-representable messages") {
		t.Fatalf("err = %v", err)
	}

	// A conversation whose turns render to no messages is equally rejected.
	request = CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalImage{MediaType: "image/png", URL: "u"}},
		}},
	}
	if _, _, err := RenderChatRequest(request, context, ChatCapabilities{}); err == nil {
		t.Fatal("image-only conversation rendered under strict policy")
	}
}
