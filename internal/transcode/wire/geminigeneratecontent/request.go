package geminigeneratecontent

import (
	"encoding/json"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// GenerateContentRequest is the client-visible GenerateContent request.
// Unknown members are rejected by DecodeRequest.
type GenerateContentRequest struct {
	Model             *string           `json:"model,omitempty"`
	Contents          []Content         `json:"contents,omitempty"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolConfig        *ToolConfig       `json:"toolConfig,omitempty"`
	SafetySettings    []SafetySetting   `json:"safetySettings,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	CachedContent     *string           `json:"cachedContent,omitempty"`
	ContinuationToken *string           `json:"continuationToken,omitempty"`
	Labels            json.RawMessage   `json:"labels,omitempty"`
	ServiceTier       *string           `json:"serviceTier,omitempty"`
	Store             *bool             `json:"store,omitempty"`
}

// Tool is one entry of GenerateContentRequest.Tools. Only function
// declarations are converted; the provider tool members stay raw so their
// presence can be reported without guessing their shape.
type Tool struct {
	FunctionDeclarations  []FunctionDeclaration `json:"functionDeclarations,omitempty"`
	CodeExecution         json.RawMessage       `json:"codeExecution,omitempty"`
	ComputerUse           json.RawMessage       `json:"computerUse,omitempty"`
	FileSearch            json.RawMessage       `json:"fileSearch,omitempty"`
	GoogleMaps            json.RawMessage       `json:"googleMaps,omitempty"`
	GoogleSearch          json.RawMessage       `json:"googleSearch,omitempty"`
	GoogleSearchRetrieval json.RawMessage       `json:"googleSearchRetrieval,omitempty"`
	MCPServers            json.RawMessage       `json:"mcpServers,omitempty"`
	URLContext            json.RawMessage       `json:"urlContext,omitempty"`
}

// FunctionDeclaration is a function tool definition. Parameters carries the
// legacy OpenAPI schema object; ParametersJSONSchema carries JSON Schema.
type FunctionDeclaration struct {
	Name                 string          `json:"name,omitempty"`
	Description          string          `json:"description,omitempty"`
	Behavior             *string         `json:"behavior,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
	Response             json.RawMessage `json:"response,omitempty"`
	ResponseJSONSchema   json.RawMessage `json:"responseJsonSchema,omitempty"`
}

// ToolConfig is the request's tool-selection configuration.
type ToolConfig struct {
	FunctionCallingConfig            *FunctionCallingConfig `json:"functionCallingConfig,omitempty"`
	RetrievalConfig                  json.RawMessage        `json:"retrievalConfig,omitempty"`
	IncludeServerSideToolInvocations *bool                  `json:"includeServerSideToolInvocations,omitempty"`
}

// FunctionCallingConfig selects how the model may call functions.
type FunctionCallingConfig struct {
	Mode                 *string  `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// GenerationConfig models every GenerationConfig member in Discovery
// revision 20261028. Presence-aware numerics let the converter distinguish a
// zero the client sent from an absent member.
type GenerationConfig struct {
	Temperature                  wire.Field[float64] `json:"temperature,omitzero"`
	TopP                         wire.Field[float64] `json:"topP,omitzero"`
	TopK                         wire.Field[int64]   `json:"topK,omitzero"`
	CandidateCount               wire.Field[int64]   `json:"candidateCount,omitzero"`
	MaxOutputTokens              wire.Field[int64]   `json:"maxOutputTokens,omitzero"`
	StopSequences                []string            `json:"stopSequences,omitempty"`
	ResponseMimeType             *string             `json:"responseMimeType,omitempty"`
	ResponseSchema               json.RawMessage     `json:"responseSchema,omitempty"`
	ResponseJSONSchema           json.RawMessage     `json:"responseJsonSchema,omitempty"`
	ResponseJSONSchemaUnderscore json.RawMessage     `json:"_responseJsonSchema,omitempty"`
	ResponseModalities           []string            `json:"responseModalities,omitempty"`
	ResponseFormat               *ResponseFormat     `json:"responseFormat,omitempty"`
	ThinkingConfig               *ThinkingConfig     `json:"thinkingConfig,omitempty"`
	SpeechConfig                 json.RawMessage     `json:"speechConfig,omitempty"`
	ImageConfig                  json.RawMessage     `json:"imageConfig,omitempty"`
	TranslationConfig            json.RawMessage     `json:"translationConfig,omitempty"`
	AudioTranscriptionConfig     json.RawMessage     `json:"audioTranscriptionConfig,omitempty"`
	FrequencyPenalty             wire.Field[float64] `json:"frequencyPenalty,omitzero"`
	PresencePenalty              wire.Field[float64] `json:"presencePenalty,omitzero"`
	ResponseLogprobs             *bool               `json:"responseLogprobs,omitempty"`
	Seed                         wire.Field[int64]   `json:"seed,omitzero"`
	Logprobs                     wire.Field[int64]   `json:"logprobs,omitzero"`
	EnableAffectiveDialog        *bool               `json:"enableAffectiveDialog,omitempty"`
	EnableEnhancedCivicAnswers   *bool               `json:"enableEnhancedCivicAnswers,omitempty"`
	MediaResolution              *string             `json:"mediaResolution,omitempty"`
}

// ResponseFormat configures each output modality independently. Only the
// text member is converted; audio and image stay raw so their presence is an
// explicit loss or rejection.
type ResponseFormat struct {
	Text  *ResponseFormatText `json:"text,omitempty"`
	Audio json.RawMessage     `json:"audio,omitempty"`
	Image json.RawMessage     `json:"image,omitempty"`
}

// ResponseFormatText is the text output format. MimeType is pinned to
// MIME_TYPE_UNSPECIFIED, APPLICATION_JSON, or TEXT_PLAIN.
type ResponseFormatText struct {
	MimeType *string         `json:"mimeType,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

// ThinkingConfig controls thought summaries and the thinking budget.
type ThinkingConfig struct {
	IncludeThoughts *bool             `json:"includeThoughts,omitempty"`
	ThinkingBudget  wire.Field[int64] `json:"thinkingBudget,omitzero"`
	ThinkingLevel   *string           `json:"thinkingLevel,omitempty"`
}

// SafetySetting is one harm-category threshold of the request's safety
// policy. Enum members are carried as strings.
type SafetySetting struct {
	Category  string `json:"category,omitempty"`
	Threshold string `json:"threshold,omitempty"`
}

// DecodeRequest strictly decodes a GenerateContent request. Every content and
// part is validated while it decodes (Part.UnmarshalJSON).
func DecodeRequest(body []byte) (GenerateContentRequest, error) {
	var request GenerateContentRequest
	if err := wire.Decode(body, &request); err != nil {
		return GenerateContentRequest{}, err
	}
	return request, nil
}
