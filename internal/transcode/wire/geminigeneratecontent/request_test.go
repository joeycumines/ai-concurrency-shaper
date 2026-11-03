package geminigeneratecontent

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestDecodeRequestFullBody(t *testing.T) {
	body := []byte(`{
		"model": "models/gemini-2.5-pro",
		"contents": [
			{
				"role": "user",
				"parts": [
					{"text": "hello", "thought": true},
					{"thoughtSignature": "c2ln"},
					{"inlineData": {"mimeType": "image/png", "data": "aGk=", "displayName": "pic"}, "videoMetadata": {"startOffset": "1s"}, "mediaResolution": "MEDIA_RESOLUTION_HIGH"},
					{"fileData": {"mimeType": "application/pdf", "fileUri": "gs://bucket/doc", "displayName": "doc"}},
					{"functionCall": {"id": "call-1", "name": "lookup", "args": {"q": "x"}}},
					{"functionResponse": {"id": "call-1", "name": "lookup", "response": {"output": "ok"}, "parts": [], "scheduling": "WHEN_IDLE", "willContinue": true}},
					{"executableCode": {"language": "PYTHON", "code": "print(1)"}},
					{"codeExecutionResult": {"outcome": "OUTCOME_OK", "output": "1"}},
					{"toolCall": {"toolType": "X"}},
					{"toolResponse": {"toolType": "X"}},
					{"audioTranscription": {"text": "hi"}},
					{"text": "speech", "speechMetadata": {"voice": "x"}, "partMetadata": {"k": "v"}, "mediaProcessing": "STATIC"}
				]
			}
		],
		"systemInstruction": {"role": "system", "parts": [{"text": "be brief"}]},
		"tools": [
			{
				"functionDeclarations": [
					{"name": "lookup", "description": "d", "behavior": "BLOCKING", "parameters": {"type": "OBJECT"}, "parametersJsonSchema": {"type": "object"}, "response": {"type": "OBJECT"}, "responseJsonSchema": {"type": "object"}}
				],
				"codeExecution": {},
				"computerUse": {},
				"fileSearch": {},
				"googleMaps": {},
				"googleSearch": {},
				"googleSearchRetrieval": {},
				"mcpServers": [],
				"urlContext": {}
			}
		],
		"toolConfig": {
			"functionCallingConfig": {"mode": "AUTO", "allowedFunctionNames": ["lookup"]},
			"retrievalConfig": {},
			"includeServerSideToolInvocations": true
		},
		"safetySettings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"}],
		"generationConfig": {
			"temperature": 0.5,
			"topP": 0.9,
			"topK": 40,
			"candidateCount": 1,
			"maxOutputTokens": 256,
			"stopSequences": ["STOP"],
			"responseMimeType": "application/json",
			"responseSchema": {"type": "OBJECT"},
			"responseJsonSchema": {"type": "object"},
			"_responseJsonSchema": {"type": "object"},
			"responseModalities": ["TEXT"],
			"responseFormat": {"text": {"mimeType": "APPLICATION_JSON", "schema": {"type": "object"}}, "audio": {}, "image": {}},
			"thinkingConfig": {"includeThoughts": true, "thinkingBudget": 1024, "thinkingLevel": "LOW"},
			"speechConfig": {},
			"imageConfig": {},
			"translationConfig": {},
			"audioTranscriptionConfig": {},
			"frequencyPenalty": 0.1,
			"presencePenalty": 0.2,
			"responseLogprobs": true,
			"seed": 7,
			"logprobs": 5,
			"enableAffectiveDialog": true,
			"enableEnhancedCivicAnswers": true,
			"mediaResolution": "MEDIA_RESOLUTION_HIGH"
		},
		"cachedContent": "cachedContents/x",
		"continuationToken": "token",
		"labels": {"team": "x"},
		"serviceTier": "standard",
		"store": true
	}`)

	request, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.Model == nil || *request.Model != "models/gemini-2.5-pro" {
		t.Fatalf("model = %v", request.Model)
	}
	if len(request.Contents) != 1 || request.Contents[0].Role != "user" {
		t.Fatalf("contents = %+v", request.Contents)
	}
	parts := request.Contents[0].Parts
	if len(parts) != 12 {
		t.Fatalf("parts = %d", len(parts))
	}
	if parts[0].Text == nil || *parts[0].Text != "hello" || parts[0].Thought == nil || !*parts[0].Thought {
		t.Fatalf("text part = %+v", parts[0])
	}
	if parts[1].ThoughtSignature == nil || *parts[1].ThoughtSignature != "c2ln" {
		t.Fatalf("signature part = %+v", parts[1])
	}
	if parts[2].InlineData == nil || parts[2].InlineData.MimeType != "image/png" || parts[2].InlineData.Data != "aGk=" || parts[2].InlineData.DisplayName != "pic" {
		t.Fatalf("inlineData part = %+v", parts[2])
	}
	if len(parts[2].VideoMetadata) == 0 || len(parts[2].MediaResolution) == 0 {
		t.Fatalf("inlineData modifiers = %+v", parts[2])
	}
	if parts[3].FileData == nil || parts[3].FileData.FileURI != "gs://bucket/doc" {
		t.Fatalf("fileData part = %+v", parts[3])
	}
	if parts[4].FunctionCall == nil || parts[4].FunctionCall.ID != "call-1" || parts[4].FunctionCall.Name != "lookup" || string(parts[4].FunctionCall.Args) != `{"q": "x"}` {
		t.Fatalf("functionCall part = %+v", parts[4])
	}
	if parts[5].FunctionResponse == nil || parts[5].FunctionResponse.Scheduling == nil || *parts[5].FunctionResponse.Scheduling != "WHEN_IDLE" || parts[5].FunctionResponse.WillContinue == nil || !*parts[5].FunctionResponse.WillContinue {
		t.Fatalf("functionResponse part = %+v", parts[5])
	}
	if len(parts[6].ExecutableCode) == 0 || len(parts[7].CodeExecutionResult) == 0 || len(parts[8].ToolCall) == 0 || len(parts[9].ToolResponse) == 0 || len(parts[10].AudioTranscription) == 0 {
		t.Fatalf("raw payload parts = %+v", parts[6:11])
	}
	if parts[11].MediaProcessing == nil || *parts[11].MediaProcessing != "STATIC" || len(parts[11].SpeechMetadata) == 0 || len(parts[11].PartMetadata) == 0 {
		t.Fatalf("modifier part = %+v", parts[11])
	}

	if request.SystemInstruction == nil || request.SystemInstruction.Role != "system" || len(request.SystemInstruction.Parts) != 1 {
		t.Fatalf("systemInstruction = %+v", request.SystemInstruction)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("tools = %+v", request.Tools)
	}
	tool := request.Tools[0]
	if len(tool.FunctionDeclarations) != 1 || tool.FunctionDeclarations[0].Name != "lookup" || tool.FunctionDeclarations[0].Behavior == nil || *tool.FunctionDeclarations[0].Behavior != "BLOCKING" {
		t.Fatalf("function declarations = %+v", tool.FunctionDeclarations)
	}
	if len(tool.FunctionDeclarations[0].Parameters) == 0 || len(tool.FunctionDeclarations[0].ParametersJSONSchema) == 0 || len(tool.FunctionDeclarations[0].Response) == 0 || len(tool.FunctionDeclarations[0].ResponseJSONSchema) == 0 {
		t.Fatalf("function declaration raw members = %+v", tool.FunctionDeclarations[0])
	}
	if len(tool.CodeExecution) == 0 || len(tool.ComputerUse) == 0 || len(tool.FileSearch) == 0 || len(tool.GoogleMaps) == 0 || len(tool.GoogleSearch) == 0 || len(tool.GoogleSearchRetrieval) == 0 || len(tool.MCPServers) == 0 || len(tool.URLContext) == 0 {
		t.Fatalf("tool members = %+v", tool)
	}

	if request.ToolConfig == nil || request.ToolConfig.FunctionCallingConfig == nil {
		t.Fatalf("toolConfig = %+v", request.ToolConfig)
	}
	config := request.ToolConfig.FunctionCallingConfig
	if config.Mode == nil || *config.Mode != "AUTO" || len(config.AllowedFunctionNames) != 1 || config.AllowedFunctionNames[0] != "lookup" {
		t.Fatalf("functionCallingConfig = %+v", config)
	}
	if request.ToolConfig.IncludeServerSideToolInvocations == nil || !*request.ToolConfig.IncludeServerSideToolInvocations || len(request.ToolConfig.RetrievalConfig) == 0 {
		t.Fatalf("toolConfig modifiers = %+v", request.ToolConfig)
	}

	if len(request.SafetySettings) == 0 || len(request.Labels) == 0 {
		t.Fatalf("raw request members = %+v", request)
	}

	generation := request.GenerationConfig
	if generation == nil {
		t.Fatal("generationConfig missing")
	}
	if !generation.Temperature.Present || generation.Temperature.Value != 0.5 {
		t.Fatalf("temperature = %+v", generation.Temperature)
	}
	if !generation.TopP.Present || generation.TopP.Value != 0.9 {
		t.Fatalf("topP = %+v", generation.TopP)
	}
	if !generation.TopK.Present || generation.TopK.Value != 40 {
		t.Fatalf("topK = %+v", generation.TopK)
	}
	if !generation.CandidateCount.Present || generation.CandidateCount.Value != 1 {
		t.Fatalf("candidateCount = %+v", generation.CandidateCount)
	}
	if !generation.MaxOutputTokens.Present || generation.MaxOutputTokens.Value != 256 {
		t.Fatalf("maxOutputTokens = %+v", generation.MaxOutputTokens)
	}
	if !generation.FrequencyPenalty.Present || generation.FrequencyPenalty.Value != 0.1 || !generation.PresencePenalty.Present || generation.PresencePenalty.Value != 0.2 {
		t.Fatalf("penalties = %+v / %+v", generation.FrequencyPenalty, generation.PresencePenalty)
	}
	if !generation.Seed.Present || generation.Seed.Value != 7 || !generation.Logprobs.Present || generation.Logprobs.Value != 5 {
		t.Fatalf("seed/logprobs = %+v / %+v", generation.Seed, generation.Logprobs)
	}
	if len(generation.StopSequences) != 1 || generation.StopSequences[0] != "STOP" || len(generation.ResponseModalities) != 1 || generation.ResponseModalities[0] != "TEXT" {
		t.Fatalf("string lists = %+v / %+v", generation.StopSequences, generation.ResponseModalities)
	}
	if generation.ResponseMimeType == nil || *generation.ResponseMimeType != "application/json" || generation.MediaResolution == nil || *generation.MediaResolution != "MEDIA_RESOLUTION_HIGH" {
		t.Fatalf("response mime/media resolution = %+v", generation)
	}
	if len(generation.ResponseSchema) == 0 || len(generation.ResponseJSONSchema) == 0 || len(generation.ResponseJSONSchemaUnderscore) == 0 {
		t.Fatalf("response schemas = %+v", generation)
	}
	if generation.ResponseFormat == nil || generation.ResponseFormat.Text == nil || generation.ResponseFormat.Text.MimeType == nil || *generation.ResponseFormat.Text.MimeType != "APPLICATION_JSON" {
		t.Fatalf("responseFormat = %+v", generation.ResponseFormat)
	}
	if len(generation.ResponseFormat.Text.Schema) == 0 || len(generation.ResponseFormat.Audio) == 0 || len(generation.ResponseFormat.Image) == 0 {
		t.Fatalf("responseFormat members = %+v", generation.ResponseFormat)
	}
	if generation.ThinkingConfig == nil || generation.ThinkingConfig.IncludeThoughts == nil || !*generation.ThinkingConfig.IncludeThoughts {
		t.Fatalf("thinkingConfig = %+v", generation.ThinkingConfig)
	}
	if !generation.ThinkingConfig.ThinkingBudget.Present || generation.ThinkingConfig.ThinkingBudget.Value != 1024 || generation.ThinkingConfig.ThinkingLevel == nil || *generation.ThinkingConfig.ThinkingLevel != "LOW" {
		t.Fatalf("thinkingConfig values = %+v", generation.ThinkingConfig)
	}
	if len(generation.SpeechConfig) == 0 || len(generation.ImageConfig) == 0 || len(generation.TranslationConfig) == 0 || len(generation.AudioTranscriptionConfig) == 0 {
		t.Fatalf("provider config members = %+v", generation)
	}
	if generation.ResponseLogprobs == nil || !*generation.ResponseLogprobs || generation.EnableAffectiveDialog == nil || !*generation.EnableAffectiveDialog || generation.EnableEnhancedCivicAnswers == nil || !*generation.EnableEnhancedCivicAnswers {
		t.Fatalf("boolean controls = %+v", generation)
	}
	if request.CachedContent == nil || *request.CachedContent != "cachedContents/x" || request.ContinuationToken == nil || *request.ContinuationToken != "token" {
		t.Fatalf("cachedContent/continuationToken = %+v", request)
	}
	if request.ServiceTier == nil || *request.ServiceTier != "standard" || request.Store == nil || !*request.Store {
		t.Fatalf("serviceTier/store = %+v", request)
	}
}

func TestDecodeRequestRejectsStrictViolations(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantKind wire.DecodeErrorKind
	}{
		{
			name:     "unknown top-level member",
			body:     `{"contents":[],"bogus":1}`,
			wantKind: wire.DecodeUnknownField,
		},
		{
			name:     "unknown part member",
			body:     `{"contents":[{"parts":[{"text":"x","bogus":1}]}]}`,
			wantKind: wire.DecodeUnknownField,
		},
		{
			name:     "snake case field",
			body:     `{"system_instruction":{"parts":[{"text":"x"}]}}`,
			wantKind: wire.DecodeUnknownField,
		},
		{
			name:     "two payload members",
			body:     `{"contents":[{"parts":[{"text":"x","functionCall":{"name":"lookup"}}]}]}`,
			wantKind: wire.DecodeContradictoryUnion,
		},
		{
			name:     "empty part",
			body:     `{"contents":[{"parts":[{}]}]}`,
			wantKind: wire.DecodeMissingRequired,
		},
		{
			name:     "duplicate top-level key",
			body:     `{"contents":[],"contents":[]}`,
			wantKind: wire.DecodeDuplicateKey,
		},
		{
			name:     "trailing value",
			body:     `{"contents":[]}{}`,
			wantKind: wire.DecodeTrailingValue,
		},
		{
			name:     "unknown safety setting member",
			body:     `{"contents":[],"safetySettings":[{"category":"HARM_CATEGORY_HATE_SPEECH","bogus":1}]}`,
			wantKind: wire.DecodeUnknownField,
		},
		{
			name:     "malformed JSON",
			body:     `{"contents":`,
			wantKind: wire.DecodeMalformed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(test.body))
			requireDecodeErrorKind(t, err, test.wantKind)
		})
	}
}

func TestDecodeRequestUnknownSnakeCaseFieldNamesRejectedKey(t *testing.T) {
	_, err := DecodeRequest([]byte(`{"system_instruction":{"parts":[{"text":"x"}]}}`))
	requireDecodeErrorKind(t, err, wire.DecodeUnknownField)
	if !strings.Contains(err.Error(), "system_instruction") {
		t.Fatalf("error does not name the rejected field: %v", err)
	}
}

func TestDecodeRequestNullPresenceField(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"generationConfig":{"temperature":null}}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if request.GenerationConfig == nil {
		t.Fatal("generationConfig missing")
	}
	temperature := request.GenerationConfig.Temperature
	if !temperature.Present || !temperature.Null || temperature.Value != 0 {
		t.Fatalf("temperature = %+v", temperature)
	}
}
