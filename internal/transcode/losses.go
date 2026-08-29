package transcode

//go:generate go run ./gen/lossmatrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// The granular loss registry (review-z commit 2). Every non-portable feature
// is gated by exactly one granular, direction-specific loss key; the
// registry below is the SINGLE source of truth for the CLI, the converters,
// and the generated LOSS_MATRIX.md (drift-tested). The legacy broad
// permission names that are NOT granular in their own right are REMOVED —
// the feature is unreleased, so there are no deprecated aliases and no
// startup expansion log (plan.md commit 6; replanLog entry 3). Names that
// survived as granular keys keep their string (e.g. reasoning_summary,
// image_input, top_k) with narrowed, single-semantic meaning. MIGRATION.md
// documents the old-to-new mapping for pre-release users.

// Feature names a semantic that is not portable between two dialects.
// Whether losing it is acceptable is decided by the LossPolicy in effect for
// the mapping.
type Feature string

// The granular loss keys. The order below is the canonical registry order
// used by the generated LOSS_MATRIX.md.
const (
	// PreviousResponseID covers the Responses previous_response_id request
	// field (and Responses-specific conversation-state references such as
	// item_reference input items): the target request cannot reproduce them.
	// Input item ids (the id of an easy message, a previous output message, a
	// function call, or a function call output) are also conversation-state
	// references; they cannot cross either and their unconditional drop is
	// noted observably rather than loss-gated.
	FeaturePreviousResponseID Feature = "previous_response_id"
	// RequestTopLogprobs covers the Responses top_logprobs request field.
	FeatureRequestTopLogprobs Feature = "request_top_logprobs"
	// RequestServiceTier covers the Responses service_tier request field.
	FeatureRequestServiceTier Feature = "request_service_tier"
	// RequestTruncation covers the Responses truncation request field.
	FeatureRequestTruncation Feature = "request_truncation"
	// MultipleSystemTurns covers multiple system turns that cannot be
	// expressed in the target's single system/instructions shape.
	FeatureMultipleSystemTurns Feature = "multiple_system_turns"
	// SystemNonTextContent covers non-text system prompt content that cannot
	// be expressed in the target's system/instructions shape.
	FeatureSystemNonTextContent Feature = "system_non_text_content"
	// MidConversationSystem covers system-channel turns that cannot keep
	// their position in a chat request: open-weights chat templates reject
	// any system-role message after index 0, including a second leading
	// one. Under this permission system-channel turns consolidate into one
	// leading system message (position/timing lost, content and authority
	// preserved); leading-only consolidation of multiple system turns is a
	// sanctioned note under the same key. The system_anywhere chat
	// capability restores positional rendering for upstreams that accept
	// system messages anywhere.
	FeatureMidConversationSystem Feature = "mid_conversation_system"
	// FeatureToolSchemaStrictness covers a source tool schema that carries
	// no strictness semantic (Messages tools have none). Responses function
	// tools require an explicit strict on the wire; when the source cannot
	// provide one, the conversion emits explicit strict:false (the
	// non-tightening value) only under this loss decision, and is rejected
	// under strict policy.
	FeatureToolSchemaStrictness Feature = "tool_schema_strictness"
	// ToolResultErrorStatus covers a tool result marked as an error
	// (Anthropic tool_result.is_error): the target dialects cannot carry the
	// error status, so it is rejected by default. A permissive policy may
	// encode the status into visible content only with the named
	// error_status_prefix encoding, which is reported.
	FeatureToolResultErrorStatus Feature = "tool_result_error_status"
	// ToolResultMultimodalContent covers multimodal tool-result content
	// (images, documents, mixed results) that a Chat tool message cannot
	// carry: rejected under strict policy, deterministically encoded as the
	// tool_result_json_envelope text under its own loss key when approved.
	FeatureToolResultMultimodalContent Feature = "tool_result_multimodal_content"
	// OutputItemBoundaries covers output item boundaries (and
	// conversation-state output items such as function_call_output) that the
	// target cannot reproduce: renderers may merge or drop items only under
	// this named loss.
	FeatureOutputItemBoundaries Feature = "output_item_boundaries"
	// OutputPhase covers an output message phase (commentary vs
	// final_answer), including the phase of a previous output message reused
	// as input: the target dialect has no phase, so the distinction is a
	// loss/reject decision.
	FeatureOutputPhase Feature = "output_phase"
	// UsageUnknown covers a source that provided no token usage at all: the
	// required target usage cannot be reproduced.
	FeatureUsageUnknown Feature = "usage_unknown"
	// UsageCacheReadUnknown covers a source that provided no cache-read
	// token breakdown.
	FeatureUsageCacheReadUnknown Feature = "usage_cache_read_unknown"
	// UsageCacheWriteUnknown covers a source that provided no cache-write
	// token breakdown.
	FeatureUsageCacheWriteUnknown Feature = "usage_cache_write_unknown"
	// UsageReasoningUnknown covers a source that provided no reasoning-token
	// breakdown.
	FeatureUsageReasoningUnknown Feature = "usage_reasoning_unknown"
	// ProviderReasoningText covers provider reasoning text in a RESPONSE —
	// the Chat provider extension spelled `reasoning` (OpenRouter style) or
	// `reasoning_content` (the DeepSeek/Qwen convention real open-weights
	// gateways stream) — that cannot be reproduced in the target: it may map
	// only to ordinary text, an approved loss, or a rejection. Request-side
	// reasoning controls are a separate semantic — see RequestReasoning —
	// and must never reuse this key (review-11 finding 4).
	FeatureProviderReasoningText Feature = "provider_reasoning_text"
	// RequestReasoning covers request-side reasoning controls — the Anthropic
	// Messages thinking budget (an explicit enabled budget_tokens) and the
	// Responses reasoning.effort — that the target request cannot reproduce.
	// It is deliberately distinct from ProviderReasoningText: an operator
	// must never have to approve losing response reasoning text just to
	// strip a request knob, or vice versa.
	FeatureRequestReasoning Feature = "request_reasoning"
	// ReasoningSummary covers reasoning summaries (Responses reasoning
	// output and the request-side reasoning.summary style) that cannot be
	// reproduced in the target.
	FeatureReasoningSummary Feature = "reasoning_summary"
	// ToolResultJSONEnvelope covers the sanctioned deterministic encoding of
	// multimodal tool results as a JSON text envelope inside a Chat tool
	// message (transcode_version 1).
	FeatureToolResultJSONEnvelope Feature = "tool_result_json_envelope"

	// The remaining keys name single, specific semantics that the granular
	// split above does not cover; they are granular in their own right.
	//
	// DeveloperRole covers a developer-role message that the target cannot
	// express distinctly.
	FeatureDeveloperRole Feature = "developer_role"
	// StructuredOutput covers a structured-output format that the target
	// cannot reproduce.
	FeatureStructuredOutput Feature = "structured_output"
	// ParallelToolCalls covers the parallel-tool-calls setting that the
	// target cannot reproduce.
	FeatureParallelToolCalls Feature = "parallel_tool_calls"
	// StopSequences covers stop sequences that the target cannot reproduce.
	FeatureStopSequences Feature = "stop_sequences"
	// ImageInput covers image input that the target cannot reproduce.
	FeatureImageInput Feature = "image_input"
	// DocumentInput covers document input that the target cannot reproduce.
	FeatureDocumentInput Feature = "document_input"
	// AuthenticatedThinking covers Anthropic authenticated thinking blocks
	// (thinking + signature, redacted_thinking): they may only pass through
	// unchanged to the same protocol; crossing protocols is a loss/reject
	// decision.
	FeatureAuthenticatedThinking Feature = "authenticated_thinking"
	// TopK covers the top_k setting that the target cannot reproduce.
	FeatureTopK Feature = "top_k"
	// Logprobs covers response token log-probabilities that the client
	// dialects cannot reproduce.
	FeatureLogprobs Feature = "logprobs"
	// ResponsesControls covers the Responses envelope controls that are
	// tolerated observably. Request-side, include and client_metadata are
	// noted (sanctioned encodings) and prompt_cache_key is dropped under
	// this permission; the conversation-state request controls (background,
	// max_tool_calls, prompt, safety_identifier, status) are typed
	// unsupported-feature errors under every policy — this key cannot
	// un-gate them (review-12 R12-1). Response-side, Responses envelope
	// controls echoed on an upstream response (background, max_tool_calls,
	// prompt, prompt_cache_key, safety_identifier) are dropped under this
	// permission.
	FeatureResponsesControls Feature = "responses_controls"
	// AnthropicControls covers the Anthropic Messages client-side envelope
	// controls (context_management, output_config): they are client/server
	// conversation controls with no representation in the target request —
	// output_config.budget_tokens duplicates the max_tokens output budget
	// already carried by max_tokens, and context_management edits direct
	// server-side context trimming. An approved loss drops them observably.
	FeatureAnthropicControls Feature = "anthropic_controls"
	// BuiltinTools covers Responses built-in tools (web_search, file_search,
	// code_interpreter, computer_use, and other non-function tool types) that
	// a chat request cannot express: an approved loss drops them from the
	// upstream request, a rejection refuses the request.
	FeatureBuiltinTools Feature = "builtin_tools"
	// ResponseServiceTier covers the upstream chat service tier ACTUALLY
	// SERVED (distinct from the requested tier): the client dialects cannot
	// represent the tier served.
	FeatureResponseServiceTier Feature = "response_service_tier"
)

// lossEntry pairs a loss key with the documentation emitted in
// LOSS_MATRIX.md. The registry order is canonical.
type lossEntry struct {
	Key         Feature
	Description string
}

// lossRegistry is the ordered granular registry — the single source for the
// CLI, the converters, and the generated LOSS_MATRIX.md.
var lossRegistry = []lossEntry{
	{FeaturePreviousResponseID, "the Responses previous_response_id request field and item_reference conversation-state references cannot be reproduced in the target request; input item ids are also conversation-state references and their unconditional drop is noted observably"},
	{FeatureRequestTopLogprobs, "the Responses top_logprobs request field cannot be reproduced in the target request"},
	{FeatureRequestServiceTier, "the Responses service_tier request field cannot be reproduced in the target request"},
	{FeatureRequestTruncation, "the Responses truncation request field cannot be reproduced in the target request"},
	{FeatureMultipleSystemTurns, "multiple system turns cannot be expressed in the target's single system/instructions shape"},
	{FeatureSystemNonTextContent, "non-text system prompt content cannot be expressed in the target's system/instructions shape"},
	{FeatureMidConversationSystem, "mid-conversation system turns cannot keep their position in a chat request; under this permission system-channel turns consolidate into one leading system message (position/timing lost, content and authority preserved), and leading-only consolidation of multiple system turns is a sanctioned note under the same key"},
	{FeatureToolSchemaStrictness, "the source tool schema has no strictness semantic; the Responses function-tool contract requires explicit strict, emitted as strict:false under this permission"},
	{FeatureToolResultErrorStatus, "the tool result error status cannot be reproduced in the target; the permissive encoding is the visible error_status_prefix text"},
	{FeatureToolResultMultimodalContent, "multimodal tool-result content cannot be carried by a Chat tool message; under this permission it is encoded as the tool_result_json_envelope text"},
	{FeatureOutputItemBoundaries, "output item boundaries and conversation-state output items (function_call_output) cannot be reproduced in the target"},
	{FeatureOutputPhase, "the output message phase (commentary vs final_answer) cannot be reproduced in the target"},
	{FeatureUsageUnknown, "the source provided no token usage; the required target usage cannot be reproduced"},
	{FeatureUsageCacheReadUnknown, "the source provided no cache-read token breakdown; the required target usage breakdown cannot be reproduced"},
	{FeatureUsageCacheWriteUnknown, "the source provided no cache-write token breakdown; the required target usage breakdown cannot be reproduced"},
	{FeatureUsageReasoningUnknown, "the source provided no reasoning-token breakdown; the required target usage breakdown cannot be reproduced"},
	{FeatureProviderReasoningText, "provider reasoning text in a RESPONSE (the chat extension spelled `reasoning` or, in the DeepSeek/Qwen convention real open-weights gateways emit, `reasoning_content`) cannot be reproduced in the target; it may map only to ordinary text, an approved loss, or a rejection (request-side reasoning controls are the separate request_reasoning key)"},
	{FeatureRequestReasoning, "request-side reasoning controls (the Anthropic thinking budget and the Responses reasoning.effort) cannot be reproduced in the target request"},
	{FeatureReasoningSummary, "reasoning summaries (output and request-side summary style) cannot be reproduced in the target"},
	{FeatureToolResultJSONEnvelope, "multimodal tool results are encoded as the deterministic transcode JSON text envelope (transcode_version 1) inside a Chat tool message"},
	{FeatureDeveloperRole, "the developer-role distinction cannot be reproduced in the target"},
	{FeatureStructuredOutput, "the structured-output format cannot be reproduced in the target"},
	{FeatureParallelToolCalls, "the parallel-tool-calls setting cannot be reproduced in the target"},
	{FeatureStopSequences, "stop sequences cannot be reproduced in the target"},
	{FeatureImageInput, "image input cannot be reproduced in the target"},
	{FeatureDocumentInput, "document input cannot be reproduced in the target"},
	{FeatureAuthenticatedThinking, "Anthropic authenticated thinking blocks cannot cross protocol boundaries"},
	{FeatureTopK, "the top_k setting cannot be reproduced in the target"},
	{FeatureLogprobs, "token log-probabilities cannot be reproduced in the target"},
	{FeatureResponsesControls, "the Responses envelope controls that are tolerated observably: include and client_metadata are noted and prompt_cache_key is dropped under this permission, and Responses envelope controls echoed on an upstream response are dropped under this permission; the request-side conversation-state controls (background, max_tool_calls, prompt, safety_identifier, status) remain typed unsupported-feature errors under every policy"},
	{FeatureAnthropicControls, "the Anthropic Messages client-side envelope controls (context_management, output_config) have no representation in the target request; an approved loss drops them observably"},
	{FeatureBuiltinTools, "Responses built-in tools (web_search, file_search, code_interpreter, computer_use, and other non-function tool types) cannot be reproduced in a chat request; an approved loss drops them, and a tool_choice the drop leaves dangling is reconciled (auto drops with a note, required and named references reject)"},
	{FeatureResponseServiceTier, "the upstream chat service tier actually served cannot be reproduced in the target"},
}

// allLossKeys returns the set of every registered loss key.
func allLossKeys() map[Feature]struct{} {
	known := make(map[Feature]struct{}, len(lossRegistry))
	for _, entry := range lossRegistry {
		known[entry.Key] = struct{}{}
	}
	return known
}

// LossKeyDescription returns the documented description of a loss key, or ""
// when the key is not registered.
func LossKeyDescription(feature Feature) string {
	for _, entry := range lossRegistry {
		if entry.Key == feature {
			return entry.Description
		}
	}
	return ""
}

// RegisteredLossKeys returns the canonical ordered loss keys.
func RegisteredLossKeys() []Feature {
	keys := make([]Feature, 0, len(lossRegistry))
	for _, entry := range lossRegistry {
		keys = append(keys, entry.Key)
	}
	return keys
}

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
// Only the granular registry names are accepted; the legacy broad names are
// removed, not aliased (review-z commit 2).
func ParseLossFeatures(names ...string) (map[Feature]struct{}, error) {
	known := allLossKeys()
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

// reserve enforces the shared report bound for both entry paths: an exchange
// that accumulates more entries than the exchange budget is corrupt
// (review-08 blocker 7). Notes and losses accumulate into the same slice, so
// an unbounded Note path could grow the report past the bound without either
// path noticing (review-gate task-12 finding 5).
func (r *ConversionReport) reserve() error {
	if len(r.Losses) >= maxStreamConversionReportEntries {
		// A report overflow is corrupt upstream data on the response side
		// (the stream path derives upstream provenance from this typed
		// error) while the request path records its own explicit local
		// provenance (review-08 blocker 7).
		return &UpstreamWireError{
			Cause: fmt.Errorf("conversion report exceeds the exchange bound of %d entries", maxStreamConversionReportEntries),
		}
	}
	return nil
}

// Lose records a loss of the feature, or returns an UnsupportedFeatureError
// when the policy does not allow it. The report is bounded: see reserve.
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
	if err := r.reserve(); err != nil {
		return err
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
// The report is bounded exactly like Lose: see reserve.
func (r *ConversionReport) Note(feature Feature, path string, detail string) error {
	if err := r.reserve(); err != nil {
		return err
	}
	r.Losses = append(r.Losses, ConversionLoss{
		Feature: feature,
		Path:    path,
		Detail:  detail,
	})
	return nil
}

// decodeJSONObject ensures function-call arguments are objects before
// converting them to Anthropic tool_use.input, whose final input must be an
// object. It delegates to the wire layer's shared strict object decoder so
// the object rule has exactly one implementation.
func decodeJSONObject(raw string) (map[string]json.RawMessage, error) {
	return wire.JSONObject(raw)
}

// trimJSONSpace returns data with surrounding whitespace removed.
func trimJSONSpace(data []byte) []byte {
	return bytes.TrimSpace(data)
}
