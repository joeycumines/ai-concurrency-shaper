package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
)

// geminiResponse is the rendered GenerateContent response. Field order is the
// order the Gemini SDKs emit.
type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion"`
	ResponseID    string            `json:"responseId"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is one rendered output part. Text is a pointer so an empty text
// part is still emitted.
type geminiPart struct {
	Text         *string             `json:"text,omitempty"`
	Thought      *bool               `json:"thought,omitempty"`
	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
}

type geminiFunctionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// geminiUsage carries each count as a pointer so an unknown component is
// omitted, while a known zero is still emitted.
type geminiUsage struct {
	PromptTokenCount        *int64 `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount,omitempty"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount,omitempty"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount,omitempty"`
	TotalTokenCount         *int64 `json:"totalTokenCount,omitempty"`
}

// RenderGeminiResponse renders the canonical response as a Gemini
// GenerateContent response with one candidate. A failed exchange never renders
// a success body: it is an upstream semantic failure. Thinking text is a
// thought part only when the client asked for thoughts; otherwise it is an
// approved loss.
func RenderGeminiResponse(
	response CanonicalResponse,
	context *ExchangeContext,
) ([]byte, ConversionReport, error) {
	var report ConversionReport
	if err := ValidateCanonicalResponse(response); err != nil {
		return nil, report, err
	}
	if response.Status == CanonicalResponseFailed {
		return nil, report, &UpstreamSemanticFailureError{Message: response.ErrorMessage}
	}
	if context == nil || context.IDs == nil {
		return nil, report, errors.New("render gemini response requires an exchange context")
	}

	parts, finishReason, err := geminiResponseParts(response, context, &report)
	if err != nil {
		return nil, report, err
	}
	if len(parts) == 0 {
		empty := ""
		parts = []geminiPart{{Text: &empty}}
		if err := report.Note(
			FeatureGeminiEmptyContent, "candidates[0].content.parts",
			"the response had no content; one empty text part keeps SDK text accessors working",
		); err != nil {
			return nil, report, err
		}
	}

	usage, err := geminiResponseUsage(response, context, &report)
	if err != nil {
		return nil, report, err
	}

	out := geminiResponse{
		Candidates: []geminiCandidate{{
			Content:      geminiContent{Role: "model", Parts: parts},
			FinishReason: finishReason,
			Index:        0,
		}},
		UsageMetadata: usage,
		ModelVersion:  requestedClientModelAlias(response, context),
		ResponseID:    context.IDs.New("resp_"),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return encoded, report, nil
}

// geminiResponseParts renders the items in order and returns the finish
// reason for the response's stop reason.
func geminiResponseParts(
	response CanonicalResponse,
	context *ExchangeContext,
	report *ConversionReport,
) ([]geminiPart, string, error) {
	var parts []geminiPart
	phaseRecorded := false
	reasoningRecorded := false
	for _, item := range response.Items {
		switch value := item.(type) {
		case *CanonicalMessageItem:
			if value.Phase.Set && !phaseRecorded {
				phaseRecorded = true
				if err := report.Lose(
					context.lossPolicy(), FeatureOutputPhase, "output[].phase",
					"the output message phase cannot be reproduced in a Gemini response",
				); err != nil {
					return nil, "", err
				}
			}
			for _, part := range value.Parts {
				rendered, err := geminiOutputPart(part, context, report)
				if err != nil {
					return nil, "", err
				}
				parts = append(parts, rendered...)
			}
		case *CanonicalFunctionCallItem:
			if !value.Arguments.IsObject {
				return nil, "", &UnrepresentableError{
					Protocol: "gemini",
					Path:     "candidates[0].content.parts[].functionCall.args",
					Detail:   "the model-generated tool arguments are not a JSON object and cannot be represented as functionCall.args",
				}
			}
			// The canonical item carries the BARE name + namespace; the
			// Gemini wire carries only the flat name, re-derived from both.
			name := value.Name
			if value.Namespace != "" {
				flat, ok := context.ToolNames.flatName(ToolNameRef{Namespace: value.Namespace, Name: value.Name})
				if !ok {
					return nil, "", fmt.Errorf(
						"output[].function_call: call %q references namespace %q, which this request does not declare",
						value.Name, value.Namespace,
					)
				}
				name = flat
			}
			parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
				ID:   value.CallID,
				Name: name,
				Args: json.RawMessage(value.Arguments.Raw),
			}})
		case *CanonicalFunctionResultItem:
			if err := report.Lose(
				context.lossPolicy(), FeatureOutputItemBoundaries, "output[].function_call_output",
				"tool results are conversation-state output items, not part of the model response",
			); err != nil {
				return nil, "", err
			}
		case *CanonicalReasoningItem:
			if !reasoningRecorded {
				reasoningRecorded = true
				if err := report.Lose(
					context.lossPolicy(), FeatureReasoningSummary, "output[].reasoning",
					"Responses reasoning output cannot be reproduced in a Gemini response",
				); err != nil {
					return nil, "", err
				}
			}
		default:
			return nil, "", fmt.Errorf("response item: unknown canonical item type %T", item)
		}
	}
	finish, err := geminiFinishReason(response, report)
	if err != nil {
		return nil, "", err
	}
	return parts, finish, nil
}

// geminiOutputPart renders one message part. Refusal text is ordinary text
// with a note; thinking is a thought part only when the client asked for
// thoughts.
func geminiOutputPart(part CanonicalPart, context *ExchangeContext, report *ConversionReport) ([]geminiPart, error) {
	switch value := part.(type) {
	case CanonicalText:
		text := value.Text
		return []geminiPart{{Text: &text}}, nil
	case CanonicalRefusal:
		if err := report.Note(
			FeatureGeminiRefusalText, "candidates[0].content.parts[].text",
			"the refusal is rendered as ordinary text",
		); err != nil {
			return nil, err
		}
		text := value.Text
		return []geminiPart{{Text: &text}}, nil
	case CanonicalThinkingPart:
		if !context.GeminiIncludeThoughts {
			if err := report.Lose(
				context.lossPolicy(), FeatureProviderReasoningText, "candidates[0].content.parts[].thought",
				"provider reasoning text is not returned unless the client requests thoughts",
			); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if err := report.Note(
			FeatureGeminiThoughtText, "candidates[0].content.parts[].thought",
			"reasoning text is rendered as a thought part because the client requested thoughts",
		); err != nil {
			return nil, err
		}
		text := value.Text
		thought := true
		return []geminiPart{{Text: &text, Thought: &thought}}, nil
	default:
		return nil, &UnrepresentableError{
			Protocol: "gemini",
			Path:     "candidates[0].content.parts[]",
			Detail:   fmt.Sprintf("the response part %T has no Gemini output form", part),
		}
	}
}

// geminiFinishReason maps the stop reason. A refusal finishes with SAFETY,
// recorded as a note, because that is the closest Gemini reason.
func geminiFinishReason(response CanonicalResponse, report *ConversionReport) (string, error) {
	switch response.Stop.Reason {
	case CanonicalStopMaxTokens:
		return "MAX_TOKENS", nil
	case CanonicalStopRefusal:
		if err := report.Note(
			FeatureGeminiFinishRefusal, "candidates[0].finishReason",
			"the refusal stop reason is rendered as the SAFETY finish reason",
		); err != nil {
			return "", err
		}
		return "SAFETY", nil
	default:
		return "STOP", nil
	}
}

// geminiResponseUsage renders usageMetadata. Unknown components are omitted
// under an approved loss; totals that do not add up are clamped and recorded.
func geminiResponseUsage(
	response CanonicalResponse,
	context *ExchangeContext,
	report *ConversionReport,
) (*geminiUsage, error) {
	usage := response.Usage
	if usage.Unknown() {
		if err := report.Lose(
			context.lossPolicy(), FeatureUsageUnknown, "usage",
			"the upstream response did not provide token usage; usageMetadata is omitted",
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	clamp := clampCanonicalUsage(&usage)
	if err := clamp.record(report, "usage"); err != nil {
		return nil, err
	}

	out := &geminiUsage{}
	if usage.InputKnown {
		out.PromptTokenCount = new(usage.InputTokens)
	} else if err := report.Lose(
		context.lossPolicy(), FeatureUsageUnknown, "usage.promptTokenCount",
		"the input token count is unknown; promptTokenCount is omitted",
	); err != nil {
		return nil, err
	}

	if usage.OutputKnown {
		candidates := usage.OutputTokens
		if usage.ReasoningKnown {
			candidates -= usage.ReasoningTokens
		}
		if candidates < 0 {
			candidates = 0
			if err := report.Note(
				FeatureUsageNegativeCounts, "usage.candidatesTokenCount",
				"reasoning tokens exceed output tokens; candidatesTokenCount is clamped to zero",
			); err != nil {
				return nil, err
			}
		}
		out.CandidatesTokenCount = new(candidates)
	} else if err := report.Lose(
		context.lossPolicy(), FeatureUsageUnknown, "usage.candidatesTokenCount",
		"the output token count is unknown; candidatesTokenCount is omitted",
	); err != nil {
		return nil, err
	}

	if usage.ReasoningKnown {
		if usage.ReasoningTokens > 0 {
			out.ThoughtsTokenCount = new(usage.ReasoningTokens)
		}
	} else if err := report.Lose(
		context.lossPolicy(), FeatureUsageReasoningUnknown, "usage.thoughtsTokenCount",
		"the reasoning token count is unknown; thoughtsTokenCount is omitted",
	); err != nil {
		return nil, err
	}

	if usage.CacheReadKnown && usage.CacheReadTokens > 0 {
		out.CachedContentTokenCount = new(usage.CacheReadTokens)
	} else if !usage.CacheReadKnown {
		if err := report.Lose(
			context.lossPolicy(), FeatureUsageCacheReadUnknown, "usage.cachedContentTokenCount",
			"the cache read token count is unknown; cachedContentTokenCount is omitted",
		); err != nil {
			return nil, err
		}
	}

	if usage.TotalKnown {
		out.TotalTokenCount = new(usage.TotalTokens)
	} else {
		total := int64(0)
		for _, count := range []*int64{out.PromptTokenCount, out.CandidatesTokenCount, out.ThoughtsTokenCount} {
			if count != nil {
				total += *count
			}
		}
		out.TotalTokenCount = new(total)
		if err := report.Note(
			FeatureUsageTotalDerived, "usage.totalTokenCount",
			"the total is the sum of the reported counts",
		); err != nil {
			return nil, err
		}
	}
	return out, nil
}
