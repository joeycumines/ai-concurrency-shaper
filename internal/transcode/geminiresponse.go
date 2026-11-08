// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// refusalFinishReasons are the Gemini finish reasons that mean the provider
// declined the answer: the response is incomplete with reason content_filter,
// never a failure.
var refusalFinishReasons = map[string]bool{
	"SAFETY":                   true,
	"RECITATION":               true,
	"BLOCKLIST":                true,
	"PROHIBITED_CONTENT":       true,
	"SPII":                     true,
	"IMAGE_SAFETY":             true,
	"IMAGE_PROHIBITED_CONTENT": true,
	"IMAGE_RECITATION":         true,
	"LANGUAGE":                 true,
}

// DecodeGeminiResponse decodes a non-streaming Gemini GenerateContent
// response into the canonical response (R2G, M2G). The envelope decode is
// tolerant (unknown envelope, candidate, and usage members are skipped and
// never forwarded); content parts stay strict through Part.UnmarshalJSON.
// Corrupt wire (type errors, duplicate keys, trailing data, malformed JSON)
// is an *UpstreamWireError; valid-but-unrepresentable content is an
// *UnrepresentableError (local, breaker-neutral).
func DecodeGeminiResponse(
	body []byte,
	capabilities ChatCapabilities,
	policy LossPolicy,
	ids *ExchangeIDs,
) (CanonicalResponse, ConversionReport, error) {
	var report ConversionReport
	resp, err := geminigeneratecontent.DecodeResponse(body)
	if err != nil {
		return CanonicalResponse{}, report, upstreamWireError(
			UpstreamGemini, 0, fmt.Errorf("gemini response: %w", err),
		)
	}

	var response CanonicalResponse
	response.ID = resp.ResponseID
	response.Model = resp.ModelVersion

	// Prompt feedback: a blocked prompt is incomplete with reason
	// content_filter; its safety ratings are an observable loss. The API
	// contract says a blocked prompt returns no candidates, so a response
	// carrying both contradicts itself and is corrupt wire (the stream
	// rejects the same combination).
	if resp.PromptFeedback != nil {
		if resp.PromptFeedback.BlockReason != nil &&
			*resp.PromptFeedback.BlockReason != "" &&
			*resp.PromptFeedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
			if len(resp.Candidates) > 0 {
				return CanonicalResponse{}, report, upstreamWireError(
					UpstreamGemini, 0,
					errors.New("gemini response carries both promptFeedback.blockReason and candidates"),
				)
			}
			// The Responses contract represents a filtered response as
			// incomplete with reason content_filter; the Anthropic dialect
			// renders the refusal stop reason. The stream renders the same.
			response.Status = CanonicalResponseIncomplete
			response.IncompleteReason = "content_filter"
			response.Stop = CanonicalStop{Reason: CanonicalStopRefusal}
			response.Items = append(response.Items, &CanonicalMessageItem{
				Role: CanonicalAssistant,
				Parts: []CanonicalPart{CanonicalRefusal{
					Text: fmt.Sprintf("the prompt was blocked by the provider (blockReason %s)", *resp.PromptFeedback.BlockReason),
				}},
			})
			if err := report.Note(
				FeatureGeminiPromptBlocked,
				"promptFeedback.blockReason",
				fmt.Sprintf("the prompt was blocked (blockReason %s) and the response is rendered as a refusal", *resp.PromptFeedback.BlockReason),
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		} else if len(resp.Candidates) == 0 {
			return CanonicalResponse{}, report, upstreamWireError(
				UpstreamGemini, 0, errors.New("gemini response has no candidates"),
			)
		}
		if len(resp.PromptFeedback.SafetyRatings) > 0 {
			if err := report.Lose(
				policy,
				FeatureGeminiSafetyMetadata,
				"promptFeedback.safetyRatings",
				"prompt safety ratings cannot be reproduced in the target response",
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
	}
	if len(resp.Candidates) == 0 && response.Status == "" {
		return CanonicalResponse{}, report, upstreamWireError(
			UpstreamGemini, 0, errors.New("gemini response has no candidates"),
		)
	}

	// Candidate selection: more than one candidate collapses to the lowest
	// index under gemini_candidate_collapse; duplicate indexes are corrupt
	// wire.
	var candidate *geminigeneratecontent.Candidate
	if len(resp.Candidates) > 0 {
		indexes := make(map[int64]int, len(resp.Candidates))
		for i := range resp.Candidates {
			idx := int64(0)
			if resp.Candidates[i].Index != nil {
				idx = *resp.Candidates[i].Index
			}
			indexes[idx]++
			if indexes[idx] > 1 {
				return CanonicalResponse{}, report, upstreamWireError(
					UpstreamGemini, 0,
					fmt.Errorf("gemini response has duplicate candidate index %d", idx),
				)
			}
		}
		if len(resp.Candidates) > 1 {
			if err := report.Lose(
				policy,
				FeatureGeminiCandidateCollapse,
				"candidates",
				fmt.Sprintf("the response carried %d candidates; the lowest index is kept (the gateway never requests more than one)", len(resp.Candidates)),
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
		best := 0
		bestIndex := int64(0)
		if resp.Candidates[0].Index != nil {
			bestIndex = *resp.Candidates[0].Index
		}
		for i := range resp.Candidates {
			idx := int64(0)
			if resp.Candidates[i].Index != nil {
				idx = *resp.Candidates[i].Index
			}
			if i == 0 || idx < bestIndex {
				best = i
				bestIndex = idx
			}
		}
		candidate = &resp.Candidates[best]
	}

	if candidate != nil {
		if len(candidate.SafetyRatings) > 0 {
			if err := report.Lose(
				policy,
				FeatureGeminiSafetyMetadata,
				"candidates[].safetyRatings",
				"candidate safety ratings cannot be reproduced in the target response",
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
		for _, raw := range []struct {
			name string
			val  json.RawMessage
		}{
			{"candidates[].citationMetadata", candidate.CitationMetadata},
			{"candidates[].groundingMetadata", candidate.GroundingMetadata},
			{"candidates[].groundingAttributions", candidate.GroundingAttributions},
			{"candidates[].urlContextMetadata", candidate.URLContextMetadata},
		} {
			if geminigeneratecontent.RawMemberPresent(raw.val) {
				if err := report.Lose(
					policy,
					FeatureGeminiGroundingMetadata,
					raw.name,
					"citation/grounding metadata cannot be reproduced in the target response",
				); err != nil {
					return CanonicalResponse{}, report, err
				}
			}
		}
		if candidate.AvgLogprobs != nil || geminigeneratecontent.RawMemberPresent(candidate.LogprobsResult) {
			if err := report.Lose(
				policy,
				FeatureLogprobs,
				"candidates[].avgLogprobs",
				"token log-probabilities cannot be reproduced in the target",
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
		if candidate.TokenCount != nil {
			if err := report.Lose(
				policy,
				FeatureGeminiUsageBreakdown,
				"candidates[].tokenCount",
				"the candidate token count breakdown cannot be reproduced in the target response",
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
		if candidate.ContinuationToken != nil && *candidate.ContinuationToken != "" {
			return CanonicalResponse{}, report, &UnrepresentableError{
				Protocol: string(UpstreamGemini),
				Path:     "candidates[].continuationToken",
				Detail:   "a continuation token cannot be represented in the target dialect",
			}
		}
	}
	if resp.ModelStatus != nil {
		if err := report.Lose(
			policy,
			FeatureGeminiModelStatus,
			"modelStatus",
			"the model status (lifecycle stage/retirement) cannot be reproduced in the target response",
		); err != nil {
			return CanonicalResponse{}, report, err
		}
	}

	// Content parts. Consecutive text parts concatenate into one CanonicalText.
	if candidate != nil && candidate.Content != nil {
		if candidate.Content.Role != "" && candidate.Content.Role != "model" {
			return CanonicalResponse{}, report, upstreamWireError(
				UpstreamGemini, 0,
				fmt.Errorf("candidate content role = %q, want model or absent", candidate.Content.Role),
			)
		}
		message := &CanonicalMessageItem{Role: CanonicalAssistant}
		var pendingText strings.Builder
		flushText := func() {
			if pendingText.Len() > 0 {
				message.Parts = append(message.Parts, CanonicalText{Text: pendingText.String()})
				pendingText.Reset()
			}
		}
		for j, part := range candidate.Content.Parts {
			partPath := fmt.Sprintf("candidates[].content.parts[%d]", j)
			switch {
			case part.Text != nil:
				if part.Thought != nil && *part.Thought {
					// An empty thought part renders nothing, so it records
					// no mapping and splits no text run, matching the
					// streaming empty-delta short circuit. A signature
					// attached to it is still gated below.
					if text := *part.Text; text != "" {
						switch {
						case capabilities.ProviderReasoningThinking:
							// Only a mapped thought opens a new part: the
							// pending text run ends here.
							flushText()
							message.Parts = append(message.Parts, CanonicalThinkingPart{
								Text:      text,
								Signature: SyntheticThinkingSignature,
							})
							if err := report.Note(
								FeatureProviderReasoningText,
								partPath,
								"provider reasoning mapped to a native thinking block (provider_reasoning_thinking encoding)",
							); err != nil {
								return CanonicalResponse{}, report, err
							}
						case capabilities.ProviderReasoningText:
							// Reasoning mapped to text joins the pending run.
							pendingText.WriteString(text)
							if err := report.Note(
								FeatureProviderReasoningText,
								partPath,
								chatProviderReasoningMappedDetail,
							); err != nil {
								return CanonicalResponse{}, report, err
							}
						default:
							// A dropped thought renders nothing, so the
							// pending text run stays un-split: [a, thought,
							// c] concatenates to one "ac" text part, the
							// same result as a stream that never carried
							// the thought.
							if err := report.Lose(
								policy,
								FeatureProviderReasoningText,
								partPath,
								chatProviderReasoningDroppedDetail,
							); err != nil {
								return CanonicalResponse{}, report, err
							}
						}
					}
				} else {
					pendingText.WriteString(*part.Text)
				}
				if part.ThoughtSignature != nil && *part.ThoughtSignature != "" {
					if err := report.Lose(
						policy,
						FeatureGeminiResponseThoughtSignature,
						partPath+".thoughtSignature",
						"a thought signature on a non-function-call part cannot be reproduced in the target response",
					); err != nil {
						return CanonicalResponse{}, report, err
					}
				}
			case part.FunctionCall != nil:
				flushText()
				call := part.FunctionCall
				callID := call.ID
				if callID == "" {
					if ids == nil {
						return CanonicalResponse{}, report, upstreamWireError(
							UpstreamGemini, 0,
							errors.New("gemini functionCall has no id and no id allocator is available"),
						)
					}
					callID = ids.New(gcallPrefix)
				}
				// An absent or explicit-null args member is no arguments,
				// matching the wire model (RawMemberPresent) and the stream.
				args := call.Args
				if !geminigeneratecontent.RawMemberPresent(args) {
					args = json.RawMessage("{}")
				} else if !jsonObjectShaped(args) {
					return CanonicalResponse{}, report, upstreamWireError(
						UpstreamGemini, 0,
						fmt.Errorf("gemini functionCall %q args must be a JSON object", call.Name),
					)
				}
				item := &CanonicalFunctionCallItem{
					CallID:    callID,
					Name:      call.Name,
					Arguments: ParseToolArguments(string(args)),
				}
				response.Items = append(response.Items, item)
				// The note describes a minted id the response carries, so it
				// follows the committed call. A later refusal of the response
				// (a part modifier, IR validation) can still leave it in the
				// returned report; callers discard the report on error.
				if callID != call.ID {
					if err := report.Note(
						FeatureGeminiCallIDSynthesized,
						partPath+".functionCall",
						"a functionCall without an id was given a proxy-minted id",
					); err != nil {
						return CanonicalResponse{}, report, err
					}
				}
				if part.ThoughtSignature != nil && *part.ThoughtSignature != "" {
					if response.Source.GeminiCallSignatures == nil {
						response.Source.GeminiCallSignatures = make(map[string]string)
					}
					response.Source.GeminiCallSignatures[callID] = *part.ThoughtSignature
				}
			case part.FunctionResponse != nil:
				return CanonicalResponse{}, report, &UnrepresentableError{
					Protocol: string(UpstreamGemini),
					Path:     partPath,
					Detail:   "a functionResponse part cannot appear in a model response",
				}
			case part.InlineData != nil, part.FileData != nil,
				geminigeneratecontent.RawMemberPresent(part.ExecutableCode),
				geminigeneratecontent.RawMemberPresent(part.CodeExecutionResult),
				geminigeneratecontent.RawMemberPresent(part.ToolCall),
				geminigeneratecontent.RawMemberPresent(part.ToolResponse),
				geminigeneratecontent.RawMemberPresent(part.AudioTranscription):
				return CanonicalResponse{}, report, &UnrepresentableError{
					Protocol: string(UpstreamGemini),
					Path:     partPath,
					Detail:   "this part kind has no representation in any target dialect",
				}
			default:
				// A signature-only part (no payload) or an empty part.
				if part.ThoughtSignature != nil && *part.ThoughtSignature != "" {
					if err := report.Lose(
						policy,
						FeatureGeminiResponseThoughtSignature,
						partPath+".thoughtSignature",
						"a thought signature on a non-function-call part cannot be reproduced in the target response",
					); err != nil {
						return CanonicalResponse{}, report, err
					}
				}
			}
			// Part modifiers the target cannot carry.
			for _, mod := range []struct {
				name string
				raw  json.RawMessage
			}{
				{"mediaResolution", part.MediaResolution},
				{"partMetadata", part.PartMetadata},
				{"videoMetadata", part.VideoMetadata},
				{"speechMetadata", part.SpeechMetadata},
			} {
				if geminigeneratecontent.RawMemberPresent(mod.raw) {
					if err := report.Lose(
						policy,
						FeatureGeminiPartMetadata,
						partPath+"."+mod.name,
						"Gemini per-part metadata cannot be reproduced in the target response",
					); err != nil {
						return CanonicalResponse{}, report, err
					}
				}
			}
			if part.MediaProcessing != nil {
				if err := report.Lose(
					policy,
					FeatureGeminiPartMetadata,
					partPath+".mediaProcessing",
					"Gemini per-part media processing cannot be reproduced in the target response",
				); err != nil {
					return CanonicalResponse{}, report, err
				}
			}
		}
		flushText()
		if len(message.Parts) > 0 {
			response.Items = append(response.Items, message)
		}
	}

	// Finish reason.
	if candidate != nil {
		if candidate.FinishReason == nil || *candidate.FinishReason == "" ||
			*candidate.FinishReason == "FINISH_REASON_UNSPECIFIED" {
			return CanonicalResponse{}, report, upstreamWireError(
				UpstreamGemini, 0, errors.New("gemini response has no finishReason"),
			)
		}
		hasCalls := false
		for _, item := range response.Items {
			if _, ok := item.(*CanonicalFunctionCallItem); ok {
				hasCalls = true
				break
			}
		}
		switch reason := *candidate.FinishReason; {
		case reason == "STOP":
			response.Status = CanonicalResponseCompleted
			if hasCalls {
				response.Stop = CanonicalStop{Reason: CanonicalStopToolUse}
			} else {
				response.Stop = CanonicalStop{Reason: CanonicalStopEndTurn}
			}
		case reason == "MAX_TOKENS":
			response.Status = CanonicalResponseIncomplete
			response.Stop = CanonicalStop{Reason: CanonicalStopMaxTokens}
			response.IncompleteReason = "max_output_tokens"
		case refusalFinishReasons[reason]:
			response.Status = CanonicalResponseIncomplete
			response.IncompleteReason = "content_filter"
			response.Stop = CanonicalStop{Reason: CanonicalStopRefusal}
			text := fmt.Sprintf("the provider stopped the response (finishReason %s)", reason)
			if candidate.FinishMessage != nil && *candidate.FinishMessage != "" {
				text = *candidate.FinishMessage
			}
			// Append to the open message item when one exists, else open one.
			appended := false
			if n := len(response.Items); n > 0 {
				if msg, ok := response.Items[n-1].(*CanonicalMessageItem); ok {
					msg.Parts = append(msg.Parts, CanonicalRefusal{Text: text})
					appended = true
				}
			}
			if !appended {
				response.Items = append(response.Items, &CanonicalMessageItem{
					Role:  CanonicalAssistant,
					Parts: []CanonicalPart{CanonicalRefusal{Text: text}},
				})
			}
			if err := report.Note(
				FeatureGeminiFinishRefusal,
				"candidates[].finishReason",
				fmt.Sprintf("finishReason %s rendered as a refusal (gemini_finish_refusal encoding)", reason),
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		default:
			return CanonicalResponse{}, report, &UnrepresentableError{
				Protocol: string(UpstreamGemini),
				Path:     "candidates[].finishReason",
				Detail:   fmt.Sprintf("the provider ended the response with finishReason %s", reason),
			}
		}
	} else {
		// Blocked-prompt refusal already set the status/stop above.
		if response.Status == "" {
			return CanonicalResponse{}, report, upstreamWireError(
				UpstreamGemini, 0, errors.New("gemini response has no candidates and no block reason"),
			)
		}
	}

	// Usage.
	if resp.UsageMetadata != nil {
		u := resp.UsageMetadata
		deref := func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		}
		response.Usage = CanonicalUsage{
			InputTokens:     deref(u.PromptTokenCount) + deref(u.ToolUsePromptTokenCount),
			CacheReadTokens: deref(u.CachedContentTokenCount),
			OutputTokens:    deref(u.CandidatesTokenCount) + deref(u.ThoughtsTokenCount),
			ReasoningTokens: deref(u.ThoughtsTokenCount),
			TotalTokens:     deref(u.TotalTokenCount),
			InputKnown:      true,
			CacheReadKnown:  true,
			OutputKnown:     true,
			ReasoningKnown:  true,
			TotalKnown:      true,
			CacheWriteKnown: false,
		}
		clamp := clampCanonicalUsage(&response.Usage)
		if err := clamp.record(&report, "usageMetadata"); err != nil {
			return CanonicalResponse{}, report, err
		}
		if len(u.PromptTokensDetails) > 0 || len(u.CacheTokensDetails) > 0 ||
			len(u.CandidatesTokensDetails) > 0 || len(u.ToolUsePromptTokensDetails) > 0 {
			if err := report.Lose(
				policy,
				FeatureGeminiUsageBreakdown,
				"usageMetadata.*TokensDetails",
				"the per-modality token breakdowns cannot be reproduced in the target response",
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
		if u.ServiceTier != nil && *u.ServiceTier != "" {
			if err := report.Lose(
				policy,
				FeatureResponseServiceTier,
				"usageMetadata.serviceTier",
				"the upstream service tier actually served cannot be reproduced in the target",
			); err != nil {
				return CanonicalResponse{}, report, err
			}
		}
	}

	if err := ValidateCanonicalResponse(response); err != nil {
		return CanonicalResponse{}, report, err
	}
	return response, report, nil
}

// jsonObjectShaped reports whether raw is a JSON object.
func jsonObjectShaped(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
