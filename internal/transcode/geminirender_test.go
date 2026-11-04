package transcode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// geminiX2GContext returns an exchange context for a Gemini-upstream render
// (R2G/M2G) with the given loss policy and signature store.
func geminiX2GContext(policy LossPolicy, store *GeminiSignatureStore) *ExchangeContext {
	context := testExchangeContext()
	context.LossPolicy = policy
	context.GeminiSignatures = store
	context.GeminiSignatureScope = "prov"
	return context
}

// permissivePolicy allows every key.
func permissivePolicy() LossPolicy {
	allowed := make(map[Feature]struct{})
	for _, key := range RegisteredLossKeys() {
		allowed[key] = struct{}{}
	}
	return LossPolicy{Allowed: allowed}
}

// renderGemini decodes body as a JSON string into a CanonicalRequest render
// input: the helper builds a minimal valid request around the given turns.
func renderGemini(t *testing.T, request CanonicalRequest, context *ExchangeContext) (string, ConversionReport, error) {
	t.Helper()
	body, report, err := RenderGeminiRequest(request, context)
	return string(body), report, err
}

func baseUserRequest() CanonicalRequest {
	return CanonicalRequest{
		ClientModel: "gemini-2.5-flash",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
	}
}

func TestRenderGeminiRequestBasic(t *testing.T) {
	body, report, err := renderGemini(t, baseUserRequest(), geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	contents, ok := doc["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %v, want one entry", doc["contents"])
	}
	content := contents[0].(map[string]any)
	if content["role"] != "user" {
		t.Fatalf("role = %v, want user", content["role"])
	}
	if len(report.Losses) != 0 {
		t.Fatalf("unexpected losses: %+v", report.Losses)
	}
}

func TestRenderGeminiRequestSystemInstruction(t *testing.T) {
	request := baseUserRequest()
	request.Turns = append([]CanonicalTurn{{
		Role:  CanonicalSystem,
		Parts: []CanonicalPart{CanonicalText{Text: "be brief"}},
	}}, request.Turns...)
	body, _, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys, ok := doc["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %s", body)
	}
	parts := sys["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("systemInstruction parts = %v", parts)
	}
}

func TestRenderGeminiRequestMidConversationSystemLoss(t *testing.T) {
	request := baseUserRequest()
	request.Turns = append(request.Turns,
		CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{CanonicalText{Text: "answer"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "late system"}}},
	)
	// The late system turn comes after a dialog turn, so its position loss
	// is recorded; the turn itself still moves into systemInstruction.
	body, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("report lacks mid_conversation_system: %+v", report.Losses)
	}
	if !strings.Contains(body, "late system") {
		t.Fatalf("late system text missing from systemInstruction: %s", body)
	}
	// Strict policy rejects.
	if _, _, err := renderGemini(t, request, geminiX2GContext(StrictLossPolicy(), nil)); err == nil {
		t.Fatal("strict policy accepted mid-conversation system turn")
	}
}

func TestRenderGeminiRequestTwoUserTurnsTwoContents(t *testing.T) {
	request := baseUserRequest()
	request.Turns = append(request.Turns, CanonicalTurn{
		Role:  CanonicalUser,
		Parts: []CanonicalPart{CanonicalText{Text: "again"}},
	})
	body, _, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Contents) != 2 || doc.Contents[0].Role != "user" || doc.Contents[1].Role != "user" {
		t.Fatalf("contents = %+v, want two user contents", doc.Contents)
	}
}

func TestRenderGeminiRequestParallelCallsPlaceholderOnlyFirst(t *testing.T) {
	request := baseUserRequest()
	request.Turns = append(request.Turns, CanonicalTurn{
		Role: CanonicalAssistant,
		Parts: []CanonicalPart{
			CanonicalFunctionCall{CallID: "call_1", Name: "f", Arguments: json.RawMessage(`{"a":1}`)},
			CanonicalFunctionCall{CallID: "call_2", Name: "g", Arguments: json.RawMessage(`{}`)},
		},
	})
	body, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureGeminiThoughtSignaturePlaceholder) {
		t.Fatalf("report lacks placeholder loss: %+v", report.Losses)
	}
	var doc struct {
		Contents []struct {
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	model := doc.Contents[1]
	if len(model.Parts) != 2 {
		t.Fatalf("model parts = %v", model.Parts)
	}
	fc1 := model.Parts[0]["functionCall"].(map[string]any)
	fc2 := model.Parts[1]["functionCall"].(map[string]any)
	if model.Parts[0]["thoughtSignature"] != skipThoughtSignatureValidator {
		t.Fatalf("first call thoughtSignature = %v, want placeholder", model.Parts[0]["thoughtSignature"])
	}
	if _, present := model.Parts[1]["thoughtSignature"]; present {
		t.Fatalf("second call carries a thoughtSignature: %v", model.Parts[1])
	}
	if fc1["id"] != "call_1" || fc2["id"] != "call_2" {
		t.Fatalf("ids = %v %v", fc1["id"], fc2["id"])
	}
	// Strict rejects the placeholder loss.
	if _, _, err := renderGemini(t, request, geminiX2GContext(StrictLossPolicy(), nil)); err == nil {
		t.Fatal("strict policy accepted the placeholder loss")
	}
}

func TestRenderGeminiRequestStoredSignatureRestored(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 8, TTL: time.Hour})
	store.Put("prov", "call_1", "real-sig")
	store.Put("prov", "call_2", "real-sig-2")
	request := baseUserRequest()
	// Earlier turn: model call with a stored signature must be restored even
	// though it is not in the current turn.
	request.Turns = append(request.Turns,
		CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{
			CanonicalFunctionCall{CallID: "call_1", Name: "f", Arguments: json.RawMessage(`{}`)},
		}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "again"}}},
		CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{
			CanonicalFunctionCall{CallID: "call_2", Name: "g", Arguments: json.RawMessage(`{}`)},
		}},
	)
	body, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), store))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if reportHasFeature(report, FeatureGeminiThoughtSignaturePlaceholder) {
		t.Fatalf("placeholder recorded although both calls have stored signatures: %+v", report.Losses)
	}
	var doc struct {
		Contents []struct {
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := doc.Contents[1].Parts[0]["thoughtSignature"]; got != "real-sig" {
		t.Fatalf("earlier-turn signature = %v, want real-sig", got)
	}
	if got := doc.Contents[3].Parts[0]["thoughtSignature"]; got != "real-sig-2" {
		t.Fatalf("current-turn signature = %v, want real-sig-2", got)
	}
}

func TestRenderGeminiRequestGcallIDsOmitted(t *testing.T) {
	request := baseUserRequest()
	request.Turns = append(request.Turns,
		CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{
			CanonicalFunctionCall{CallID: "gcall_abc", Name: "f", Arguments: json.RawMessage(`{}`)},
		}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{
			CanonicalFunctionResult{CallID: "gcall_abc", Parts: []CanonicalPart{CanonicalText{Text: "ok"}}},
		}},
	)
	body, _, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, "gcall_") {
		t.Fatalf("gcall_ id leaked into the render: %s", body)
	}
	var doc struct {
		Contents []struct {
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fc := doc.Contents[1].Parts[0]["functionCall"].(map[string]any)
	if _, present := fc["id"]; present {
		t.Fatalf("functionCall carries an id: %v", fc)
	}
	fr := doc.Contents[2].Parts[0]["functionResponse"].(map[string]any)
	if _, present := fr["id"]; present {
		t.Fatalf("functionResponse carries an id: %v", fr)
	}
	if fr["name"] != "f" {
		t.Fatalf("functionResponse name = %v, want f (resolved from the earlier call)", fr["name"])
	}
	resp, _ := fr["response"].(map[string]any)
	if resp["output"] != "ok" {
		t.Fatalf("functionResponse response = %v", fr["response"])
	}
}

func TestRenderGeminiRequestFunctionResultUnknownCallRejects(t *testing.T) {
	request := baseUserRequest()
	request.Turns = append(request.Turns, CanonicalTurn{
		Role:  CanonicalUser,
		Parts: []CanonicalPart{CanonicalFunctionResult{CallID: "call_x", Parts: []CanonicalPart{CanonicalText{Text: "ok"}}}},
	})
	_, _, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err == nil || !strings.Contains(err.Error(), "no earlier call") {
		t.Fatalf("err = %v, want no-earlier-call rejection", err)
	}
}

func TestRenderGeminiRequestToolStrictnessLoss(t *testing.T) {
	strict := true
	request := baseUserRequest()
	request.Tools = []CanonicalTool{{
		Name:       "f",
		JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"$schema":"https://json-schema.org/draft/2020-12/schema"}`),
		Strict:     &strict,
	}}
	body, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureGeminiToolStrictness) {
		t.Fatalf("report lacks gemini_tool_strictness: %+v", report.Losses)
	}
	// The raw schema is carried unchanged, including additionalProperties and $schema.
	var doc struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Name                 string          `json:"name"`
				ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	schema := string(doc.Tools[0].FunctionDeclarations[0].ParametersJSONSchema)
	if !strings.Contains(schema, "additionalProperties") || !strings.Contains(schema, "$schema") {
		t.Fatalf("parametersJsonSchema = %s, want raw schema unchanged", schema)
	}
	if _, _, err := renderGemini(t, request, geminiX2GContext(StrictLossPolicy(), nil)); err == nil {
		t.Fatal("strict policy accepted tool strictness")
	}
}

func TestRenderGeminiRequestStructuredOutputStrictFalseNote(t *testing.T) {
	request := baseUserRequest()
	request.StructuredOutput = &CanonicalStructuredOutput{
		Name:   "s",
		Schema: json.RawMessage(`{"type":"object"}`),
		Strict: false,
	}
	body, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureGeminiStructuredOutputEnforced) {
		t.Fatalf("report lacks the structured-output note: %+v", report.Losses)
	}
	var doc struct {
		GenerationConfig struct {
			ResponseMimeType   string          `json:"responseMimeType"`
			ResponseJSONSchema json.RawMessage `json:"responseJsonSchema"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.GenerationConfig.ResponseMimeType != "application/json" {
		t.Fatalf("responseMimeType = %q", doc.GenerationConfig.ResponseMimeType)
	}
	if string(doc.GenerationConfig.ResponseJSONSchema) != `{"type":"object"}` {
		t.Fatalf("responseJsonSchema = %s", doc.GenerationConfig.ResponseJSONSchema)
	}
}

func TestRenderGeminiRequestImageDetailLoss(t *testing.T) {
	request := baseUserRequest()
	request.Turns[0].Parts = []CanonicalPart{
		CanonicalImage{MediaType: "image/png", Base64: "aGk=", Detail: "high"},
		CanonicalText{Text: "what is this"},
	}
	body, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureGeminiImageDetail) {
		t.Fatalf("report lacks gemini_image_detail: %+v", report.Losses)
	}
	var doc struct {
		Contents []struct {
			Parts []struct {
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inline := doc.Contents[0].Parts[0].InlineData
	if inline == nil || inline.MimeType != "image/png" || inline.Data != "aGk=" {
		t.Fatalf("inlineData = %+v", doc.Contents[0].Parts[0])
	}
	if _, _, err := renderGemini(t, request, geminiX2GContext(StrictLossPolicy(), nil)); err == nil {
		t.Fatal("strict policy accepted image detail")
	}
}

func TestRenderGeminiRequestImageURLLoss(t *testing.T) {
	request := baseUserRequest()
	request.Turns[0].Parts = []CanonicalPart{
		CanonicalImage{MediaType: "image/png", URL: "https://example.com/x.png"},
		CanonicalText{Text: "what is this"},
	}
	_, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureImageInput) {
		t.Fatalf("report lacks image_input: %+v", report.Losses)
	}
}

func TestRenderGeminiRequestToolChoice(t *testing.T) {
	for _, tt := range []struct {
		choice CanonicalToolChoice
		mode   string
		names  []string
	}{
		{CanonicalToolChoice{Mode: "auto"}, "AUTO", nil},
		{CanonicalToolChoice{Mode: "none"}, "NONE", nil},
		{CanonicalToolChoice{Mode: "required"}, "ANY", nil},
		{CanonicalToolChoice{Mode: "named", Name: "f"}, "ANY", []string{"f"}},
	} {
		request := baseUserRequest()
		request.ToolChoice = &tt.choice
		body, _, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
		if err != nil {
			t.Fatalf("render %s: %v", tt.choice.Mode, err)
		}
		var doc struct {
			ToolConfig struct {
				FunctionCallingConfig struct {
					Mode                 string   `json:"mode"`
					AllowedFunctionNames []string `json:"allowedFunctionNames"`
				} `json:"functionCallingConfig"`
			} `json:"toolConfig"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		cfg := doc.ToolConfig.FunctionCallingConfig
		if cfg.Mode != tt.mode {
			t.Fatalf("mode = %q, want %q", cfg.Mode, tt.mode)
		}
		if len(cfg.AllowedFunctionNames) != len(tt.names) {
			t.Fatalf("allowedFunctionNames = %v, want %v", cfg.AllowedFunctionNames, tt.names)
		}
	}
}

func TestRenderGeminiRequestParallelToolsFalseLoss(t *testing.T) {
	parallel := false
	request := baseUserRequest()
	request.ParallelTools = &parallel
	_, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureParallelToolCalls) {
		t.Fatalf("report lacks parallel_tool_calls: %+v", report.Losses)
	}
}

func TestRenderGeminiRequestMetadataLoss(t *testing.T) {
	request := baseUserRequest()
	request.Metadata = map[string]string{"user_id": "u-1"}
	_, report, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureMetadata) {
		t.Fatalf("report lacks metadata: %+v", report.Losses)
	}
}

func TestRenderGeminiRequestEmptyContentRejects(t *testing.T) {
	request := baseUserRequest()
	request.Turns[0].Parts = []CanonicalPart{CanonicalImage{MediaType: "image/png", URL: "https://example.com/x.png"}}
	// Under a permissive policy the URL image is dropped, leaving the turn
	// with no renderable content.
	_, _, err := renderGemini(t, request, geminiX2GContext(permissivePolicy(), nil))
	if err == nil || !strings.Contains(err.Error(), "no content") {
		t.Fatalf("err = %v, want no-content rejection", err)
	}
}
