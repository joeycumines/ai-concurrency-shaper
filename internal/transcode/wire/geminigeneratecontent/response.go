package geminigeneratecontent

import (
	"encoding/json"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// FinishReasons is the pinned Candidate.finishReason enum.
var FinishReasons = []string{
	"FINISH_REASON_UNSPECIFIED",
	"STOP",
	"MAX_TOKENS",
	"SAFETY",
	"RECITATION",
	"LANGUAGE",
	"OTHER",
	"BLOCKLIST",
	"PROHIBITED_CONTENT",
	"SPII",
	"MALFORMED_FUNCTION_CALL",
	"IMAGE_SAFETY",
	"IMAGE_PROHIBITED_CONTENT",
	"IMAGE_OTHER",
	"NO_IMAGE",
	"IMAGE_RECITATION",
	"UNEXPECTED_TOOL_CALL",
	"TOO_MANY_TOOL_CALLS",
	"MISSING_THOUGHT_SIGNATURE",
	"MALFORMED_RESPONSE",
	"ESCALATION",
	"PUP_LIMITED_DISABLED",
	"CONTINUATION",
}

// BlockReasons is the pinned PromptFeedback.blockReason enum.
var BlockReasons = []string{
	"BLOCK_REASON_UNSPECIFIED",
	"SAFETY",
	"OTHER",
	"BLOCKLIST",
	"PROHIBITED_CONTENT",
	"IMAGE_SAFETY",
}

// GenerateContentResponse is the successful response body and, unchanged,
// one stream chunk. Error is the in-stream error frame that Discovery's
// response schema omits but both official SDKs parse.
type GenerateContentResponse struct {
	Candidates     []Candidate     `json:"candidates,omitempty"`
	PromptFeedback *PromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *UsageMetadata  `json:"usageMetadata,omitempty"`
	ModelVersion   string          `json:"modelVersion,omitempty"`
	ResponseID     string          `json:"responseId,omitempty"`
	ModelStatus    *ModelStatus    `json:"modelStatus,omitempty"`
	Error          *ErrorBody      `json:"error,omitempty"`
}

// Candidate is one generated response.
type Candidate struct {
	Content               *Content        `json:"content,omitempty"`
	FinishReason          *string         `json:"finishReason,omitempty"`
	FinishMessage         *string         `json:"finishMessage,omitempty"`
	Index                 *int64          `json:"index,omitempty"`
	SafetyRatings         []SafetyRating  `json:"safetyRatings,omitempty"`
	CitationMetadata      json.RawMessage `json:"citationMetadata,omitempty"`
	GroundingMetadata     json.RawMessage `json:"groundingMetadata,omitempty"`
	GroundingAttributions json.RawMessage `json:"groundingAttributions,omitempty"`
	URLContextMetadata    json.RawMessage `json:"urlContextMetadata,omitempty"`
	LogprobsResult        json.RawMessage `json:"logprobsResult,omitempty"`
	AvgLogprobs           *float64        `json:"avgLogprobs,omitempty"`
	TokenCount            *int64          `json:"tokenCount,omitempty"`
	ContinuationToken     *string         `json:"continuationToken,omitempty"`
}

// PromptFeedback reports why the prompt itself was blocked.
type PromptFeedback struct {
	BlockReason   *string        `json:"blockReason,omitempty"`
	SafetyRatings []SafetyRating `json:"safetyRatings,omitempty"`
}

// UsageMetadata reports token counts. Each details list names the modalities
// of one count and their token numbers.
type UsageMetadata struct {
	PromptTokenCount           *int64               `json:"promptTokenCount,omitempty"`
	CachedContentTokenCount    *int64               `json:"cachedContentTokenCount,omitempty"`
	CandidatesTokenCount       *int64               `json:"candidatesTokenCount,omitempty"`
	ToolUsePromptTokenCount    *int64               `json:"toolUsePromptTokenCount,omitempty"`
	ThoughtsTokenCount         *int64               `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount            *int64               `json:"totalTokenCount,omitempty"`
	PromptTokensDetails        []ModalityTokenCount `json:"promptTokensDetails,omitempty"`
	CacheTokensDetails         []ModalityTokenCount `json:"cacheTokensDetails,omitempty"`
	CandidatesTokensDetails    []ModalityTokenCount `json:"candidatesTokensDetails,omitempty"`
	ToolUsePromptTokensDetails []ModalityTokenCount `json:"toolUsePromptTokensDetails,omitempty"`
	ServiceTier                *string              `json:"serviceTier,omitempty"`
}

// SafetyRating is the safety classification of one harm category.
type SafetyRating struct {
	Category    string `json:"category,omitempty"`
	Probability string `json:"probability,omitempty"`
	Blocked     *bool  `json:"blocked,omitempty"`
}

// ModalityTokenCount is the token count of one modality in a usage breakdown.
type ModalityTokenCount struct {
	Modality   string `json:"modality,omitempty"`
	TokenCount *int64 `json:"tokenCount,omitempty"`
}

// ModelStatus reports the lifecycle stage of the model that served a response.
type ModelStatus struct {
	Message        string `json:"message,omitempty"`
	ModelStage     string `json:"modelStage,omitempty"`
	RetirementTime string `json:"retirementTime,omitempty"`
}

// DecodeResponse tolerantly decodes a response envelope or stream chunk.
// Unknown envelope and candidate members are skipped; Part.UnmarshalJSON
// keeps every content part strict.
func DecodeResponse(body []byte) (GenerateContentResponse, error) {
	var response GenerateContentResponse
	if err := wire.DecodeTolerant(body, &response); err != nil {
		return GenerateContentResponse{}, err
	}
	return response, nil
}
