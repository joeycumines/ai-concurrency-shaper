package transcode

import (
	"encoding/json"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// geminiEmptyObjectSchema is the schema of a function that declares no
// parameters.
var geminiEmptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// tools converts the request's tool list. Function declarations become
// canonical tools; provider tools have no canonical form and are an approved
// loss or a rejection under the policy.
func (d *geminiDecoder) tools(tools []geminigeneratecontent.Tool, out *CanonicalRequest) error {
	for i, tool := range tools {
		path := fmt.Sprintf("tools[%d]", i)
		for _, member := range []struct {
			name string
			raw  json.RawMessage
		}{
			{"codeExecution", tool.CodeExecution},
			{"computerUse", tool.ComputerUse},
			{"fileSearch", tool.FileSearch},
			{"googleMaps", tool.GoogleMaps},
			{"googleSearch", tool.GoogleSearch},
			{"googleSearchRetrieval", tool.GoogleSearchRetrieval},
			{"mcpServers", tool.MCPServers},
			{"urlContext", tool.URLContext},
		} {
			if !geminigeneratecontent.RawMemberPresent(member.raw) {
				continue
			}
			if err := d.report.Lose(
				d.policy, FeatureGeminiProviderTools, path+"."+member.name,
				"provider tool is not forwarded",
			); err != nil {
				return err
			}
		}
		for j, decl := range tool.FunctionDeclarations {
			converted, err := d.functionDeclaration(fmt.Sprintf("%s.functionDeclarations[%d]", path, j), decl)
			if err != nil {
				return err
			}
			out.Tools = append(out.Tools, converted)
		}
	}
	return nil
}

// functionDeclaration converts one function declaration. The declared
// parameters become the tool's JSON Schema: the JSON Schema member verbatim,
// or the OpenAPI-style member converted. Both at once is ambiguous.
func (d *geminiDecoder) functionDeclaration(
	path string,
	decl geminigeneratecontent.FunctionDeclaration,
) (CanonicalTool, error) {
	if decl.Name == "" {
		return CanonicalTool{}, fmt.Errorf("%s.name is required", path)
	}
	if decl.Behavior != nil {
		switch *decl.Behavior {
		case "UNSPECIFIED", "BLOCKING":
		case "NON_BLOCKING":
			return CanonicalTool{}, fmt.Errorf("%s.behavior NON_BLOCKING is not supported: the call must block", path)
		default:
			return CanonicalTool{}, fmt.Errorf("%s.behavior %q is not supported", path, *decl.Behavior)
		}
	}
	if geminigeneratecontent.RawMemberPresent(decl.Response) || geminigeneratecontent.RawMemberPresent(decl.ResponseJSONSchema) {
		if err := d.report.Lose(
			d.policy, FeatureGeminiGenerationControls, path+".response",
			"the declared response schema is not forwarded",
		); err != nil {
			return CanonicalTool{}, err
		}
	}

	var schema json.RawMessage
	switch {
	case geminigeneratecontent.RawMemberPresent(decl.Parameters) && geminigeneratecontent.RawMemberPresent(decl.ParametersJSONSchema):
		return CanonicalTool{}, fmt.Errorf("%s: parameters and parametersJsonSchema are mutually exclusive", path)
	case geminigeneratecontent.RawMemberPresent(decl.ParametersJSONSchema):
		if _, err := wire.JSONObject(string(decl.ParametersJSONSchema)); err != nil {
			return CanonicalTool{}, fmt.Errorf("%s.parametersJsonSchema must be a JSON object", path)
		}
		schema = decl.ParametersJSONSchema
	case geminigeneratecontent.RawMemberPresent(decl.Parameters):
		converted, err := ConvertGeminiSchema(decl.Parameters)
		if err != nil {
			return CanonicalTool{}, fmt.Errorf("%s.parameters: %w", path, err)
		}
		if err := d.report.Note(
			FeatureGeminiSchemaConverted, path+".parameters",
			"the OpenAPI-style parameters schema was converted to JSON Schema",
		); err != nil {
			return CanonicalTool{}, err
		}
		schema = converted
	default:
		schema = geminiEmptyObjectSchema
	}
	return CanonicalTool{Name: decl.Name, Description: decl.Description, JSONSchema: schema}, nil
}

// toolConfig maps the function-calling mode onto the canonical tool choice.
// An unset or MODE_UNSPECIFIED mode leaves the choice unset. allowedFunctionNames
// is valid only with ANY, and ANY narrows to a named choice only for exactly
// one name.
func (d *geminiDecoder) toolConfig(cfg *geminigeneratecontent.ToolConfig, out *CanonicalRequest) error {
	if cfg == nil {
		return nil
	}
	if geminigeneratecontent.RawMemberPresent(cfg.RetrievalConfig) || cfg.IncludeServerSideToolInvocations != nil {
		if err := d.report.Lose(
			d.policy, FeatureGeminiProviderTools, "toolConfig",
			"retrieval and server-side tool invocation settings are not forwarded",
		); err != nil {
			return err
		}
	}
	fcc := cfg.FunctionCallingConfig
	if fcc == nil {
		return nil
	}
	mode := ""
	if fcc.Mode != nil {
		mode = *fcc.Mode
	}
	names := fcc.AllowedFunctionNames
	if len(names) > 0 && mode != "ANY" {
		return fmt.Errorf("toolConfig.functionCallingConfig.allowedFunctionNames requires mode ANY")
	}
	switch mode {
	case "", "MODE_UNSPECIFIED":
		return nil
	case "AUTO":
		out.ToolChoice = &CanonicalToolChoice{Mode: "auto"}
	case "NONE":
		out.ToolChoice = &CanonicalToolChoice{Mode: "none"}
	case "ANY":
		switch len(names) {
		case 0:
			out.ToolChoice = &CanonicalToolChoice{Mode: "required"}
		case 1:
			out.ToolChoice = &CanonicalToolChoice{Mode: "named", Name: names[0]}
		default:
			return fmt.Errorf("toolConfig.functionCallingConfig mode ANY allows at most one allowedFunctionNames entry")
		}
	case "VALIDATED":
		return fmt.Errorf("toolConfig.functionCallingConfig mode VALIDATED is not supported")
	default:
		return fmt.Errorf("toolConfig.functionCallingConfig.mode %q is not supported", mode)
	}
	return nil
}

// geminiField reads a presence-aware numeric member. Absent reports false; an
// explicit null is rejected because the modeled member has no null form.
func geminiField[T any](field wire.Field[T], path string) (T, bool, error) {
	var zero T
	if !field.Present {
		return zero, false, nil
	}
	if field.Null {
		return zero, false, fmt.Errorf("%s must not be null", path)
	}
	return field.Value, true, nil
}

// generationConfig converts the sampling and output members. Members with
// no canonical form are an approved generation-controls loss or a rejection,
// and thinkingConfig records whether the client asked for thought text.
func (d *geminiDecoder) generationConfig(
	cfg *geminigeneratecontent.GenerationConfig,
	out *CanonicalRequest,
	facts *GeminiRequestFacts,
) error {
	if cfg == nil {
		return nil
	}
	if v, ok, err := geminiField(cfg.Temperature, "generationConfig.temperature"); err != nil {
		return err
	} else if ok {
		out.Temperature = &v
	}
	if v, ok, err := geminiField(cfg.TopP, "generationConfig.topP"); err != nil {
		return err
	} else if ok {
		out.TopP = &v
	}
	if v, ok, err := geminiField(cfg.TopK, "generationConfig.topK"); err != nil {
		return err
	} else if ok {
		if d.target == UpstreamMessages {
			n := int(v)
			out.TopK = &n
		} else if err := d.report.Lose(
			d.policy, FeatureTopK, "generationConfig.topK",
			"top-k sampling is not supported by this upstream",
		); err != nil {
			return err
		}
	}
	if v, ok, err := geminiField(cfg.MaxOutputTokens, "generationConfig.maxOutputTokens"); err != nil {
		return err
	} else if ok {
		if v <= 0 {
			return fmt.Errorf("generationConfig.maxOutputTokens must be positive")
		}
		n := int(v)
		out.MaxOutputTokens = &n
	}
	if v, ok, err := geminiField(cfg.CandidateCount, "generationConfig.candidateCount"); err != nil {
		return err
	} else if ok && v != 1 {
		return fmt.Errorf("generationConfig.candidateCount %d is not supported: only one candidate is returned", v)
	}
	if len(cfg.StopSequences) > 0 {
		out.StopSequences = append([]string(nil), cfg.StopSequences...)
	}

	for _, name := range []string{"speechConfig", "imageConfig", "translationConfig", "audioTranscriptionConfig"} {
		var raw json.RawMessage
		switch name {
		case "speechConfig":
			raw = cfg.SpeechConfig
		case "imageConfig":
			raw = cfg.ImageConfig
		case "translationConfig":
			raw = cfg.TranslationConfig
		case "audioTranscriptionConfig":
			raw = cfg.AudioTranscriptionConfig
		}
		if geminigeneratecontent.RawMemberPresent(raw) {
			return fmt.Errorf("generationConfig.%s is not supported", name)
		}
	}
	if err := geminiModalities(cfg.ResponseModalities); err != nil {
		return err
	}

	for _, control := range []struct {
		name    string
		present bool
	}{
		{"frequencyPenalty", cfg.FrequencyPenalty.Present},
		{"presencePenalty", cfg.PresencePenalty.Present},
		{"seed", cfg.Seed.Present},
		{"logprobs", cfg.Logprobs.Present},
		{"responseLogprobs", cfg.ResponseLogprobs != nil && *cfg.ResponseLogprobs},
		{"enableAffectiveDialog", cfg.EnableAffectiveDialog != nil && *cfg.EnableAffectiveDialog},
		{"enableEnhancedCivicAnswers", cfg.EnableEnhancedCivicAnswers != nil && *cfg.EnableEnhancedCivicAnswers},
		{"mediaResolution", cfg.MediaResolution != nil},
	} {
		if !control.present {
			continue
		}
		if err := d.report.Lose(
			d.policy, FeatureGeminiGenerationControls, "generationConfig."+control.name,
			"generation control is not forwarded",
		); err != nil {
			return err
		}
	}

	if cfg.ThinkingConfig != nil {
		if err := d.report.Lose(
			d.policy, FeatureRequestReasoning, "generationConfig.thinkingConfig",
			"thinking configuration is not forwarded",
		); err != nil {
			return err
		}
		facts.IncludeThoughts = cfg.ThinkingConfig.IncludeThoughts != nil && *cfg.ThinkingConfig.IncludeThoughts
	}

	schema, err := d.responseSchema(cfg)
	if err != nil {
		return err
	}
	if schema != nil {
		out.StructuredOutput = &CanonicalStructuredOutput{Name: "response", Schema: schema, Strict: false}
	}
	return nil
}

// geminiModalities accepts only the text modality, which is the only output
// this gateway produces.
func geminiModalities(modalities []string) error {
	if len(modalities) == 0 {
		return nil
	}
	if len(modalities) == 1 && modalities[0] == "TEXT" {
		return nil
	}
	return fmt.Errorf("generationConfig.responseModalities is not supported: text output only")
}

// responseSchema resolves the structured-output schema from the response MIME
// fields. JSON output selected without a schema is rejected, a schema without
// JSON output is rejected, and more than one schema form is rejected because
// their meaning would have to be guessed.
func (d *geminiDecoder) responseSchema(cfg *geminigeneratecontent.GenerationConfig) (json.RawMessage, error) {
	jsonMode := false
	if cfg.ResponseMimeType != nil {
		switch *cfg.ResponseMimeType {
		case "text/plain":
		case "application/json":
			jsonMode = true
		default:
			return nil, fmt.Errorf("generationConfig.responseMimeType %q is not supported", *cfg.ResponseMimeType)
		}
	}

	var formatSchema json.RawMessage
	if format := cfg.ResponseFormat; format != nil {
		if geminigeneratecontent.RawMemberPresent(format.Audio) || geminigeneratecontent.RawMemberPresent(format.Image) {
			return nil, fmt.Errorf("generationConfig.responseFormat audio and image output is not supported")
		}
		if text := format.Text; text != nil {
			if text.MimeType != nil {
				switch *text.MimeType {
				case "MIME_TYPE_UNSPECIFIED", "TEXT_PLAIN":
				case "APPLICATION_JSON":
					jsonMode = true
				default:
					return nil, fmt.Errorf("generationConfig.responseFormat.text.mimeType %q is not supported", *text.MimeType)
				}
			}
			formatSchema = text.Schema
		}
	}

	forms := 0
	for _, raw := range []json.RawMessage{
		cfg.ResponseSchema, cfg.ResponseJSONSchema, cfg.ResponseJSONSchemaUnderscore, formatSchema,
	} {
		if geminigeneratecontent.RawMemberPresent(raw) {
			forms++
		}
	}
	if forms > 1 {
		return nil, fmt.Errorf("generationConfig sets more than one response schema form")
	}
	if forms == 0 {
		if jsonMode {
			return nil, fmt.Errorf("generationConfig requests JSON output without a response schema")
		}
		return nil, nil
	}
	if !jsonMode {
		return nil, fmt.Errorf("a response schema requires application/json output")
	}

	switch {
	case geminigeneratecontent.RawMemberPresent(cfg.ResponseSchema):
		converted, err := ConvertGeminiSchema(cfg.ResponseSchema)
		if err != nil {
			return nil, fmt.Errorf("generationConfig.responseSchema: %w", err)
		}
		if err := d.report.Note(
			FeatureGeminiSchemaConverted, "generationConfig.responseSchema",
			"the OpenAPI-style response schema was converted to JSON Schema",
		); err != nil {
			return nil, err
		}
		return converted, nil
	case geminigeneratecontent.RawMemberPresent(cfg.ResponseJSONSchema):
		return geminiObjectSchema("generationConfig.responseJsonSchema", cfg.ResponseJSONSchema)
	case geminigeneratecontent.RawMemberPresent(cfg.ResponseJSONSchemaUnderscore):
		return geminiObjectSchema("generationConfig._responseJsonSchema", cfg.ResponseJSONSchemaUnderscore)
	default:
		return geminiObjectSchema("generationConfig.responseFormat.text.schema", formatSchema)
	}
}

// geminiObjectSchema validates that a JSON Schema member is an object and
// carries it verbatim.
func geminiObjectSchema(path string, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := wire.JSONObject(string(raw)); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object", path)
	}
	return raw, nil
}

// requestControls handles the request-level members outside the contents,
// tools, and generation settings.
func (d *geminiDecoder) requestControls(req geminigeneratecontent.GenerateContentRequest, out *CanonicalRequest) error {
	if len(req.SafetySettings) > 0 {
		if err := d.report.Lose(
			d.policy, FeatureGeminiSafetySettings, "safetySettings",
			"safety settings are not forwarded",
		); err != nil {
			return err
		}
	}
	if req.CachedContent != nil {
		return fmt.Errorf("cachedContent is not supported")
	}
	if req.ContinuationToken != nil {
		return fmt.Errorf("continuationToken is not supported")
	}
	for _, control := range []struct {
		name    string
		present bool
	}{
		{"labels", geminigeneratecontent.RawMemberPresent(req.Labels)},
		{"serviceTier", req.ServiceTier != nil},
		{"store", req.Store != nil},
	} {
		if !control.present {
			continue
		}
		if err := d.report.Lose(
			d.policy, FeatureGeminiRequestControls, control.name,
			"request control is not forwarded",
		); err != nil {
			return err
		}
	}
	return nil
}
