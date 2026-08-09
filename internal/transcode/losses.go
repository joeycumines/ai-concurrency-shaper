package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Feature names a semantic that is not portable between two dialects. Whether
// losing it is acceptable is decided by the LossPolicy in effect for the
// mapping.
type Feature string

const (
	FeatureDeveloperRole         Feature = "developer_role"
	FeatureStructuredOutput      Feature = "structured_output"
	FeatureParallelToolCalls     Feature = "parallel_tool_calls"
	FeatureStopSequences         Feature = "stop_sequences"
	FeatureImageInput            Feature = "image_input"
	FeatureDocumentInput         Feature = "document_input"
	FeatureConversationState     Feature = "conversation_state"
	FeatureReasoningSummary      Feature = "reasoning_summary"
	FeatureProviderReasoning     Feature = "provider_reasoning"
	FeatureAuthenticatedThinking Feature = "authenticated_thinking"
	FeatureTopK                  Feature = "top_k"
	// FeatureLogprobs covers chat response token log-probabilities: the
	// client dialects cannot reproduce them, so their presence is a
	// loss/reject decision.
	FeatureLogprobs Feature = "logprobs"
	// FeatureServiceTier covers an upstream chat response's service tier:
	// the client dialects cannot represent the tier actually served, so its
	// presence is a loss/reject decision.
	FeatureServiceTier Feature = "service_tier"
	// FeatureUsageTiming covers Anthropic's required usage fields when the
	// source cannot provide them at the required point (the early
	// message_start usage) or at all: emitting zeros would fabricate facts,
	// so the omission is an explicit loss/reject decision (review-j finding
	// 9).
	FeatureUsageTiming Feature = "usage_timing"
	// FeatureToolResultError covers a tool result marked as an error
	// (Anthropic tool_result.is_error): the target dialects cannot carry the
	// error status, so it is rejected by default. A permissive policy may
	// encode the status into visible content only with the named
	// "error_status_prefix" encoding, which is reported (review-j finding
	// 10).
	FeatureToolResultError Feature = "tool_result_error"
	// FeaturePhase covers a Responses output-message phase (commentary vs
	// final_answer): the Messages dialect has no phase, so the distinction
	// is a loss/reject decision (review-j finding 10).
	FeaturePhase Feature = "phase"
	// FeatureReasoningSummaryRequest covers the Responses request's
	// reasoning.summary style when rendering to a Chat upstream: Chat has
	// reasoning_effort but no summary style, so the request is a
	// loss/reject decision (review-j finding 10).
	FeatureReasoningSummaryRequest Feature = "reasoning_summary_request"
	// FeatureResponsesControls covers the pinned Responses envelope control
	// fields (background, max_tool_calls, prompt, prompt_cache_key,
	// safety_identifier): they decode as typed shadows so strict decoding
	// never fails on a current official response, and their presence is a
	// loss/reject decision because the client dialects cannot reproduce
	// them (review-j finding 13).
	FeatureResponsesControls Feature = "responses_controls"
)

// LossPolicy decides whether a non-portable feature may be dropped during a
// conversion. The zero value (nil map) is the strictest policy: no loss is
// allowed and every non-portable feature produces an error.
type LossPolicy struct {
	Allowed map[Feature]struct{}
}

// StrictLossPolicy returns a policy that allows no losses.
func StrictLossPolicy() LossPolicy {
	return LossPolicy{Allowed: make(map[Feature]struct{})}
}

// ParseLossFeatures parses comma- or space-separated feature names into a
// map for a LossPolicy, rejecting unknown names (review-j finding 14: a
// misconfigured loss policy fails at startup, never on the first request).
func ParseLossFeatures(names ...string) (map[Feature]struct{}, error) {
	known := map[Feature]struct{}{}
	for _, feature := range []Feature{
		FeatureDeveloperRole, FeatureStructuredOutput, FeatureParallelToolCalls,
		FeatureStopSequences, FeatureImageInput, FeatureDocumentInput,
		FeatureConversationState, FeatureReasoningSummary,
		FeatureProviderReasoning, FeatureAuthenticatedThinking, FeatureTopK,
		FeatureLogprobs, FeatureServiceTier, FeatureUsageTiming,
		FeatureToolResultError, FeaturePhase, FeatureReasoningSummaryRequest,
		FeatureResponsesControls,
	} {
		known[feature] = struct{}{}
	}
	allowed := make(map[Feature]struct{})
	for _, name := range names {
		for _, part := range strings.FieldsFunc(name, func(r rune) bool {
			return r == ',' || r == ' '
		}) {
			feature := Feature(part)
			if _, ok := known[feature]; !ok {
				return nil, fmt.Errorf("unknown loss feature %q", part)
			}
			allowed[feature] = struct{}{}
		}
	}
	return allowed, nil
}

// Allows reports whether the policy permits losing the feature.
func (p LossPolicy) Allows(feature Feature) bool {
	_, ok := p.Allowed[feature]
	return ok
}

// UnsupportedFeatureError reports a request or response field that uses a
// feature this transcoder does not support in the target dialect.
type UnsupportedFeatureError struct {
	Protocol string
	Path     string
	Feature  string
}

func (e *UnsupportedFeatureError) Error() string {
	return fmt.Sprintf(
		"%s field %s uses unsupported feature %q",
		e.Protocol,
		e.Path,
		e.Feature,
	)
}

// ConversionLoss records a feature that was dropped under an approved loss
// policy.
type ConversionLoss struct {
	Feature Feature
	Path    string
	Detail  string
}

// ConversionReport accumulates approved losses for one conversion.
type ConversionReport struct {
	Losses []ConversionLoss
}

// Lose records a loss of the feature, or returns an UnsupportedFeatureError
// when the policy does not allow it. The report is bounded: an exchange that
// accumulates more entries than the exchange budget is corrupt (review-08
// blocker 7).
func (r *ConversionReport) Lose(
	policy LossPolicy,
	feature Feature,
	path string,
	detail string,
) error {
	if !policy.Allows(feature) {
		return &UnsupportedFeatureError{
			Protocol: "transcode",
			Path:     path,
			Feature:  string(feature),
		}
	}
	if len(r.Losses) >= maxStreamConversionReportEntries {
		// A report overflow is corrupt upstream data on the response side
		// (the stream path derives upstream provenance from this typed
		// error) while the request path records its own explicit local
		// provenance (review-08 blocker 7).
		return &UpstreamWireError{
			Cause: errors.New("conversion report exceeds the exchange bound of " +
				strconv.Itoa(maxStreamConversionReportEntries) + " entries"),
		}
	}
	r.Losses = append(r.Losses, ConversionLoss{
		Feature: feature,
		Path:    path,
		Detail:  detail,
	})
	return nil
}

// Note records a named encoding that the conversion applied without a policy
// decision — the encoding is sanctioned by a capability or an approved loss
// — so it is observable even though no loss occurred (review-j finding 10:
// encodings that invent or reinterpret content must be named and reported).
func (r *ConversionReport) Note(feature Feature, path string, detail string) {
	r.Losses = append(r.Losses, ConversionLoss{
		Feature: feature,
		Path:    path,
		Detail:  detail,
	})
}

// strictDecode decodes exactly one JSON value into dst with unknown fields
// rejected and trailing values rejected. It is the standard entry point for
// every wire union and strict request/response schema in this package.
func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return err
	}

	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// trimJSONSpace returns data with surrounding whitespace removed.
func trimJSONSpace(data []byte) []byte {
	return bytes.TrimSpace(data)
}

// decodeJSONObject ensures function-call arguments are objects before
// converting them to Anthropic tool_use.input, whose final input must be an
// object.
func decodeJSONObject(raw string) (map[string]json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("empty JSON object")
	}
	var value map[string]json.RawMessage
	if err := strictDecode([]byte(raw), &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	return value, nil
}
