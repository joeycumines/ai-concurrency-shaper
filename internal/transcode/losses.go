package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	FeatureUnsupportedTool       Feature = "unsupported_tool"
	FeatureTopK                  Feature = "top_k"
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
// when the policy does not allow it.
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
	r.Losses = append(r.Losses, ConversionLoss{
		Feature: feature,
		Path:    path,
		Detail:  detail,
	})
	return nil
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
