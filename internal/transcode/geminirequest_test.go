package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// geminiAllowAll permits every registered loss, so loss-recording behavior can
// be observed in the report.
func geminiAllowAll() LossPolicy {
	return LossPolicy{Allowed: allLossKeys()}
}

func geminiDecodeOK(t *testing.T, body string, policy LossPolicy, target UpstreamProtocol) (DecodeResult, GeminiRequestFacts) {
	t.Helper()
	result, facts, err := DecodeGeminiRequest([]byte(body), policy, target)
	if err != nil {
		t.Fatalf("DecodeGeminiRequest error: %v\nbody: %s", err, body)
	}
	return result, facts
}

func geminiDecodeErr(t *testing.T, body string, policy LossPolicy, target UpstreamProtocol) string {
	t.Helper()
	_, _, err := DecodeGeminiRequest([]byte(body), policy, target)
	if err == nil {
		t.Fatalf("DecodeGeminiRequest succeeded, want error\nbody: %s", body)
	}
	return err.Error()
}

// geminiReportHas reports whether the report holds an entry of the given kind
// for the feature.
func geminiReportHas(report ConversionReport, feature Feature, kind LossKind) bool {
	for _, entry := range report.Losses {
		if entry.Feature == feature && entry.Kind == kind {
			return true
		}
	}
	return false
}

func TestDecodeGeminiRequestTurnsAndText(t *testing.T) {
	body := `{
		"systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "hi"}]},
			{"role": "model", "parts": [{"text": "hello"}]},
			{"role": "user", "parts": [{"text": "again"}]}
		]
	}`
	result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
	turns := result.Request.Turns
	if len(turns) != 4 {
		t.Fatalf("got %d turns, want 4 (system then three contents)", len(turns))
	}
	wantRoles := []CanonicalRole{CanonicalSystem, CanonicalUser, CanonicalAssistant, CanonicalUser}
	for i, want := range wantRoles {
		if turns[i].Role != want {
			t.Fatalf("turn %d role %q, want %q", i, turns[i].Role, want)
		}
	}
	if got := turns[2].Parts[0].(CanonicalText).Text; got != "hello" {
		t.Fatalf("model text %q, want hello", got)
	}
}

func TestDecodeGeminiRequestRoleInferenceIsNoted(t *testing.T) {
	body := `{"contents": [{"parts": [{"text": "no role"}]}]}`
	result, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamChatCompletions)
	if result.Request.Turns[0].Role != CanonicalUser {
		t.Fatalf("role-less content read as %q, want user", result.Request.Turns[0].Role)
	}
	if !geminiReportHas(result.Report, FeatureGeminiRoleInferred, NoteRecord) {
		t.Fatalf("role inference was not recorded as a note: %+v", result.Report.Losses)
	}
}

func TestDecodeGeminiRequestRejections(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "system instruction with model role",
			body: `{"systemInstruction":{"role":"model","parts":[{"text":"x"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "must not be model",
		},
		{
			name: "system instruction carrying inline data",
			body: `{"systemInstruction":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "must be a text part",
		},
		{
			name: "empty contents",
			body: `{"contents":[]}`,
			want: "contents is required",
		},
		{
			name: "unknown role",
			body: `{"contents":[{"role":"tool","parts":[{"text":"x"}]}]}`,
			want: `role "tool" is not supported`,
		},
		{
			name: "file data",
			body: `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://b/o"}}]}]}`,
			want: "fileData is not supported",
		},
		{
			name: "inline data of an unsupported media type",
			body: `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"text/csv","data":"AAAA"}}]}]}`,
			want: `mimeType "text/csv" is not supported`,
		},
		{
			name: "function call in a user turn",
			body: `{"contents":[{"role":"user","parts":[{"functionCall":{"name":"f","args":{}}}]}]}`,
			want: "only valid in a model turn",
		},
		{
			name: "function call with non-object arguments",
			body: `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":[1]}}]}]}`,
			want: "not a JSON object",
		},
		{
			name: "function response in a model turn",
			body: `{"contents":[{"role":"model","parts":[{"functionResponse":{"name":"f","response":{"output":"x"}}}]}]}`,
			want: "only valid in a user turn",
		},
		{
			name: "function response scheduling is rejected",
			body: `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f"}}]},{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"f","response":{"output":"x"},"scheduling":"SILENT"}}]}]}`,
			want: "scheduling is not supported",
		},
		{
			name: "function response id matching no call",
			body: `{"contents":[{"role":"user","parts":[{"functionResponse":{"id":"ghost","name":"f","response":{"output":"x"}}}]}]}`,
			want: `matches no open functionCall`,
		},
		{
			name: "function response naming a different function than its call",
			body: `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f"}}]},{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"g","response":{"output":"x"}}}]}]}`,
			want: `but its functionCall is "f"`,
		},
		{
			name: "duplicate function call id",
			body: `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f"}},{"functionCall":{"id":"c1","name":"f"}}]}]}`,
			want: "used by more than one functionCall",
		},
		{
			name: "id-less response with no open call",
			body: `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"x"}}}]}]}`,
			want: "send ids",
		},
		{
			name: "NON_BLOCKING behavior is not accepted here",
			body: `{"tools":[{"functionDeclarations":[{"name":"f","behavior":"NON_BLOCKING"}]}],"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "NON_BLOCKING",
		},
		{
			name: "parameters and parametersJsonSchema together",
			body: `{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT"},"parametersJsonSchema":{"type":"object"}}]}],"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "mutually exclusive",
		},
		{
			name: "allowedFunctionNames without mode ANY",
			body: `{"toolConfig":{"functionCallingConfig":{"mode":"AUTO","allowedFunctionNames":["f"]}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "requires mode ANY",
		},
		{
			name: "ANY with several allowed names",
			body: `{"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["f","g"]}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "at most one allowedFunctionNames entry",
		},
		{
			name: "VALIDATED mode",
			body: `{"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "VALIDATED is not supported",
		},
		{
			name: "zero maxOutputTokens",
			body: `{"generationConfig":{"maxOutputTokens":0},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "must be positive",
		},
		{
			name: "several candidates",
			body: `{"generationConfig":{"candidateCount":2},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "only one candidate",
		},
		{
			name: "non-text response modality",
			body: `{"generationConfig":{"responseModalities":["IMAGE"]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "text output only",
		},
		{
			name: "JSON mime without a schema",
			body: `{"generationConfig":{"responseMimeType":"application/json"},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "without a response schema",
		},
		{
			name: "schema without JSON output",
			body: `{"generationConfig":{"responseJsonSchema":{"type":"object"}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "requires application/json",
		},
		{
			name: "two response schema forms",
			body: `{"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT"},"responseJsonSchema":{"type":"object"}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "more than one response schema form",
		},
		{
			name: "cached content",
			body: `{"cachedContent":"cache/1","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "cachedContent is not supported",
		},
		{
			name: "snake_case field name gets the camelCase hint",
			body: `{"safety_settings":[],"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: `"safetySettings"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := geminiDecodeErr(t, tt.body, geminiAllowAll(), UpstreamChatCompletions)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("error %q does not contain %q", got, tt.want)
			}
		})
	}
}

func TestDecodeGeminiRequestInlineDataAndDocuments(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"inlineData":{"mimeType":"image/png","data":"AAAA"}},
		{"inlineData":{"mimeType":"application/pdf","data":"BBBB","displayName":"spec.pdf"}}
	]}]}`
	result, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamChatCompletions)
	parts := result.Request.Turns[0].Parts
	image, ok := parts[0].(CanonicalImage)
	if !ok || image.MediaType != "image/png" || image.Base64 != "AAAA" {
		t.Fatalf("image part = %#v", parts[0])
	}
	document, ok := parts[1].(CanonicalDocument)
	if !ok || document.MediaType != "application/pdf" || document.Base64 != "BBBB" || document.Filename != "spec.pdf" {
		t.Fatalf("document part = %#v", parts[1])
	}
}

func TestDecodeGeminiRequestThoughtLossesAreGated(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"reasoning","thought":true},
		{"text":"answer","thoughtSignature":"c2ln"}
	]},{"role":"user","parts":[{"text":"next"}]}]}`

	result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
	if !geminiReportHas(result.Report, FeatureGeminiThoughtSignature, LossRecord) {
		t.Fatalf("thought loss not recorded: %+v", result.Report.Losses)
	}
	if got := result.Request.Turns[0].Parts; len(got) != 1 || got[0].(CanonicalText).Text != "answer" {
		t.Fatalf("assistant parts = %#v, want only the answer text", got)
	}

	if msg := geminiDecodeErr(t, body, StrictLossPolicy(), UpstreamChatCompletions); !strings.Contains(msg, "gemini_thought_signature") {
		t.Fatalf("strict policy error %q does not name the gated loss", msg)
	}
}

func TestDecodeGeminiRequestSignatureOnlyPartIsDropped(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[{"thoughtSignature":"c2ln"},{"text":"ok"}]},{"role":"user","parts":[{"text":"next"}]}]}`
	result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
	if got := result.Request.Turns[0].Parts; len(got) != 1 {
		t.Fatalf("assistant parts = %#v, want the signature-only part dropped", got)
	}
}

func TestDecodeGeminiRequestCallIdentity(t *testing.T) {
	t.Run("explicit ids pair the response and carry the error flag", func(t *testing.T) {
		body := `{"contents":[
			{"role":"model","parts":[{"functionCall":{"id":"c1","name":"lookup","args":{"q":"x"}}}]},
			{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"lookup","response":{"error":"down"}}}]}
		]}`
		result, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamChatCompletions)
		call := result.Request.Turns[0].Parts[0].(CanonicalFunctionCall)
		if call.CallID != "c1" || call.Name != "lookup" || string(call.Arguments) != `{"q":"x"}` {
			t.Fatalf("call = %#v", call)
		}
		result2 := result.Request.Turns[1].Parts[0].(CanonicalFunctionResult)
		if result2.CallID != "c1" || !result2.IsError {
			t.Fatalf("result = %#v", result2)
		}
		if text := result2.Parts[0].(CanonicalText).Text; text != "down" {
			t.Fatalf("result text %q, want down", text)
		}
	})

	t.Run("missing call id is minted once with a note", func(t *testing.T) {
		body := `{"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"a"}},{"functionCall":{"name":"b"}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"a","response":{"output":"1"}}},{"functionResponse":{"name":"b","response":{"output":"2"}}}]}
		]}`
		result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
		first := result.Request.Turns[0].Parts[0].(CanonicalFunctionCall)
		second := result.Request.Turns[0].Parts[1].(CanonicalFunctionCall)
		if first.CallID != "gcall_1" || second.CallID != "gcall_2" {
			t.Fatalf("minted ids %q, %q", first.CallID, second.CallID)
		}
		if got := result.Request.Turns[1].Parts[1].(CanonicalFunctionResult).CallID; got != "gcall_2" {
			t.Fatalf("second result paired with %q, want gcall_2", got)
		}
		noted := 0
		for _, entry := range result.Report.Losses {
			if entry.Feature == FeatureGeminiCallIDSynthesized {
				noted++
			}
		}
		if noted != 1 {
			t.Fatalf("call id note recorded %d times, want once", noted)
		}
	})

	t.Run("id-less responses pair by order within the preceding model turn", func(t *testing.T) {
		body := `{"contents":[
			{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f"}},{"functionCall":{"id":"c2","name":"f"}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"one"}}},{"functionResponse":{"name":"f","response":{"output":"two"}}}]}
		]}`
		result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
		if got := result.Request.Turns[1].Parts[0].(CanonicalFunctionResult).CallID; got != "c1" {
			t.Fatalf("first id-less result paired with %q, want c1", got)
		}
		if got := result.Request.Turns[1].Parts[1].(CanonicalFunctionResult).CallID; got != "c2" {
			t.Fatalf("second id-less result paired with %q, want c2", got)
		}
		if !geminiReportHas(result.Report, FeatureGeminiCallCorrelation, NoteRecord) {
			t.Fatalf("correlation not noted: %+v", result.Report.Losses)
		}
	})

	t.Run("several same-name calls from an earlier turn cannot be paired by order", func(t *testing.T) {
		body := `{"contents":[
			{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f"}},{"functionCall":{"id":"c2","name":"f"}}]},
			{"role":"user","parts":[{"text":"interrupt"}]},
			{"role":"model","parts":[{"text":"ok"}]},
			{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"one"}}},{"functionResponse":{"name":"f","response":{"output":"two"}}}]}
		]}`
		if msg := geminiDecodeErr(t, body, geminiAllowAll(), UpstreamChatCompletions); !strings.Contains(msg, "send ids") {
			t.Fatalf("error %q does not ask for ids", msg)
		}
	})

	t.Run("object response that is not output or error is carried as compact JSON", func(t *testing.T) {
		body := `{"contents":[
			{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f"}}]},
			{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"f","response":{"rows": 2, "ok": true}}}]}
		]}`
		result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
		text := result.Request.Turns[1].Parts[0].(CanonicalFunctionResult).Parts[0].(CanonicalText).Text
		if text != `{"rows":2,"ok":true}` {
			t.Fatalf("compact text %q", text)
		}
		if !geminiReportHas(result.Report, FeatureGeminiFunctionResponseJSON, NoteRecord) {
			t.Fatalf("compact JSON not noted")
		}
	})
}

func TestDecodeGeminiRequestToolsAndSchemas(t *testing.T) {
	body := `{"tools":[{"functionDeclarations":[
		{"name":"legacy","parameters":{"type":"OBJECT","properties":{"n":{"type":"INTEGER"}}}},
		{"name":"modern","parametersJsonSchema":{"type":"object","properties":{"s":{"type":"string"}}}},
		{"name":"bare"}
	]}],"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
	if len(result.Request.Tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(result.Request.Tools))
	}
	legacy := string(result.Request.Tools[0].JSONSchema)
	if legacy != `{"properties":{"n":{"type":"integer"}},"type":"object"}` {
		t.Fatalf("legacy schema %s", legacy)
	}
	if got := string(result.Request.Tools[1].JSONSchema); got != `{"type":"object","properties":{"s":{"type":"string"}}}` {
		t.Fatalf("modern schema %s", got)
	}
	if got := string(result.Request.Tools[2].JSONSchema); got != `{"type":"object","properties":{}}` {
		t.Fatalf("bare schema %s", got)
	}
	if !geminiReportHas(result.Report, FeatureGeminiSchemaConverted, NoteRecord) {
		t.Fatalf("schema conversion not noted")
	}
}

func TestDecodeGeminiRequestToolChoiceModes(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want *CanonicalToolChoice
	}{
		{"AUTO maps to auto", `"mode":"AUTO"`, &CanonicalToolChoice{Mode: "auto"}},
		{"NONE maps to none", `"mode":"NONE"`, &CanonicalToolChoice{Mode: "none"}},
		{"ANY maps to required", `"mode":"ANY"`, &CanonicalToolChoice{Mode: "required"}},
		{"ANY with one name maps to named", `"mode":"ANY","allowedFunctionNames":["f"]`, &CanonicalToolChoice{Mode: "named", Name: "f"}},
		{"MODE_UNSPECIFIED leaves the choice unset", `"mode":"MODE_UNSPECIFIED"`, nil},
		{"absent mode leaves the choice unset", `"allowedFunctionNames":[]`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"toolConfig":{"functionCallingConfig":{` + tt.mode + `}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
			result, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamChatCompletions)
			got := result.Request.ToolChoice
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ToolChoice = %+v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("ToolChoice = %+v, want %+v", got, *tt.want)
			}
		})
	}
}

func TestDecodeGeminiRequestGenerationAndControls(t *testing.T) {
	t.Run("sampling members map and topK is gated by the target", func(t *testing.T) {
		body := `{"generationConfig":{"temperature":0.5,"topP":0.9,"topK":40,"maxOutputTokens":256,"stopSequences":["END"]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		messages, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamMessages)
		req := messages.Request
		if req.Temperature == nil || *req.Temperature != 0.5 || req.TopP == nil || *req.TopP != 0.9 {
			t.Fatalf("sampling not mapped: %+v", req)
		}
		if req.TopK == nil || *req.TopK != 40 || req.MaxOutputTokens == nil || *req.MaxOutputTokens != 256 {
			t.Fatalf("topK/maxOutputTokens not mapped: %+v", req)
		}
		if len(req.StopSequences) != 1 || req.StopSequences[0] != "END" {
			t.Fatalf("stop sequences %v", req.StopSequences)
		}

		chat, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
		if chat.Request.TopK != nil {
			t.Fatalf("topK carried to a target that cannot take it")
		}
		if !geminiReportHas(chat.Report, FeatureTopK, LossRecord) {
			t.Fatalf("topK loss not recorded for chat completions")
		}
		if msg := geminiDecodeErr(t, body, StrictLossPolicy(), UpstreamChatCompletions); !strings.Contains(msg, "top_k") {
			t.Fatalf("strict error %q does not name top_k", msg)
		}
	})

	t.Run("structured output from responseSchema is converted and noted", func(t *testing.T) {
		body := `{"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"a":{"type":"STRING"}}}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		result, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamChatCompletions)
		out := result.Request.StructuredOutput
		if out == nil || out.Name != "response" || out.Strict {
			t.Fatalf("structured output = %+v", out)
		}
		var schema map[string]any
		if err := json.Unmarshal(out.Schema, &schema); err != nil || schema["type"] != "object" {
			t.Fatalf("schema %s", out.Schema)
		}
		if !geminiReportHas(result.Report, FeatureGeminiSchemaConverted, NoteRecord) {
			t.Fatalf("conversion not noted")
		}
	})

	t.Run("JSON-schema structured output is verbatim", func(t *testing.T) {
		body := `{"generationConfig":{"responseMimeType":"application/json","responseJsonSchema":{"type":"object","x-keep":true}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		result, _ := geminiDecodeOK(t, body, StrictLossPolicy(), UpstreamChatCompletions)
		if got := string(result.Request.StructuredOutput.Schema); got != `{"type":"object","x-keep":true}` {
			t.Fatalf("schema %s", got)
		}
	})

	t.Run("thinking config is a gated loss and sets includeThoughts", func(t *testing.T) {
		body := `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":512}},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		result, facts := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
		if !facts.IncludeThoughts {
			t.Fatalf("includeThoughts not carried into facts")
		}
		if !geminiReportHas(result.Report, FeatureRequestReasoning, LossRecord) {
			t.Fatalf("reasoning loss not recorded")
		}
		if _, _, err := DecodeGeminiRequest([]byte(body), StrictLossPolicy(), UpstreamChatCompletions); err == nil {
			t.Fatalf("strict policy accepted a thinking config")
		}
	})

	t.Run("safety settings and penalties are gated losses", func(t *testing.T) {
		body := `{"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}],"generationConfig":{"frequencyPenalty":0.2},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
		if !geminiReportHas(result.Report, FeatureGeminiSafetySettings, LossRecord) {
			t.Fatalf("safety loss not recorded")
		}
		if !geminiReportHas(result.Report, FeatureGeminiGenerationControls, LossRecord) {
			t.Fatalf("generation control loss not recorded")
		}
		if _, _, err := DecodeGeminiRequest([]byte(body), StrictLossPolicy(), UpstreamChatCompletions); err == nil {
			t.Fatalf("strict policy accepted safety settings")
		}
	})
}

func TestDecodeGeminiRequestSkipsTurnsWhoseEveryPartIsDropped(t *testing.T) {
	body := `{"contents":[
		{"role":"model","parts":[{"text":"reasoning","thought":true}]},
		{"role":"user","parts":[{"text":"next"}]}
	]}`
	result, _ := geminiDecodeOK(t, body, geminiAllowAll(), UpstreamChatCompletions)
	if len(result.Request.Turns) != 1 || result.Request.Turns[0].Role != CanonicalUser {
		t.Fatalf("turns = %+v, want only the user turn", result.Request.Turns)
	}
	if !geminiReportHas(result.Report, FeatureGeminiThoughtSignature, LossRecord) {
		t.Fatalf("thought loss not recorded")
	}
}
