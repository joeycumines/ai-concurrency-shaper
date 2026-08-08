package transcode

import (
	"encoding/json"
	"fmt"
)

// Canonical IR contains only semantics genuinely shared by the supported
// dialects. Provider-authenticated or provider-opaque data is not generalized.
//
// Protocol references:
// https://platform.openai.com/docs/api-reference/responses
// https://platform.openai.com/docs/api-reference/chat
// https://platform.claude.com/docs/en/api/messages

// CanonicalRole is a conversation role in the canonical IR.
type CanonicalRole string

const (
	CanonicalUser      CanonicalRole = "user"
	CanonicalAssistant CanonicalRole = "assistant"
	CanonicalSystem    CanonicalRole = "system"
	CanonicalDeveloper CanonicalRole = "developer"
)

// CanonicalPart is one content part of a canonical turn.
type CanonicalPart interface {
	isCanonicalPart()
}

// CanonicalText is ordinary text.
type CanonicalText struct {
	Text string
}

func (CanonicalText) isCanonicalPart() {}

// CanonicalRefusal is refusal content. It is a distinct part type because
// refusal has different rendering requirements than ordinary text in every
// target dialect.
type CanonicalRefusal struct {
	Text string
}

func (CanonicalRefusal) isCanonicalPart() {}

// CanonicalImage is an image input. Exactly one of URL or Base64 is set.
type CanonicalImage struct {
	MediaType string
	URL       string
	Base64    string
	Detail    string
}

func (CanonicalImage) isCanonicalPart() {}

// CanonicalDocument is a document input. Exactly one of URL, Base64, or
// FileID is set.
type CanonicalDocument struct {
	MediaType string
	URL       string
	Base64    string
	FileID    string
	Filename  string
}

func (CanonicalDocument) isCanonicalPart() {}

// CanonicalFunctionCall is a model-initiated function call. Arguments is a
// validated complete JSON object.
type CanonicalFunctionCall struct {
	ItemID    string
	CallID    string
	Name      string
	Arguments json.RawMessage // validated complete JSON object
}

func (CanonicalFunctionCall) isCanonicalPart() {}

// CanonicalFunctionResult is the result of a function call.
type CanonicalFunctionResult struct {
	CallID  string
	IsError bool
	Parts   []CanonicalPart
}

func (CanonicalFunctionResult) isCanonicalPart() {}

// CanonicalTurn is one conversation turn. An assistant turn may contain
// multiple parts; turn boundaries are preserved across conversion.
type CanonicalTurn struct {
	Role  CanonicalRole
	Parts []CanonicalPart
}

// CanonicalTool is a function tool definition.
type CanonicalTool struct {
	Name        string
	Description string
	JSONSchema  json.RawMessage
	Strict      *bool
}

// CanonicalToolChoice is the normalized tool selection mode.
type CanonicalToolChoice struct {
	Mode string // none, auto, required, named
	Name string
}

// CanonicalStructuredOutput is a structured-output schema.
type CanonicalStructuredOutput struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      bool
}

// SourceArtifacts carries provider-specific data that is not portable. These
// values may only be passed through unchanged to the same protocol or handled
// through an explicit loss/rejection path.
type SourceArtifacts struct {
	ResponsesReasoningItems []json.RawMessage
	AnthropicThinkingBlocks []json.RawMessage
}

// CanonicalRequest is the decoded request in canonical form.
type CanonicalRequest struct {
	ClientModel string

	Turns []CanonicalTurn
	Tools []CanonicalTool

	ToolChoice       *CanonicalToolChoice
	ParallelTools    *bool
	MaxOutputTokens  *int
	Temperature      *float64
	TopP             *float64
	StopSequences    []string
	StructuredOutput *CanonicalStructuredOutput
	Stream           bool
	Metadata         map[string]string

	Artifacts SourceArtifacts
}

// DecodeResult is the output of a source decode.
type DecodeResult struct {
	Request CanonicalRequest
	Report  ConversionReport
}

// ExchangeContext carries per-exchange state required to render the client
// response envelope and preserve identity across the conversion.
type ExchangeContext struct {
	RequestedClientModel string
	UpstreamModel        string

	IDs *ExchangeIDs

	// LossPolicy decides whether a non-portable feature may be dropped during
	// rendering. It is copied from the mapping.
	LossPolicy LossPolicy

	// Request-derived state required to reconstruct the client response
	// envelope. A stateless convertResponse(body) cannot faithfully construct
	// a Responses response.
	OriginalResponsesRequest *ResponsesRequestEcho
	OriginalMessagesRequest  *MessagesRequestContext

	// StreamIntent records the source request's stream value so a stream/JSON
	// mismatch on the upstream response is an error rather than a silent mode
	// change.
	StreamIntent bool
}

// lossPolicy returns the exchange loss policy, or the strictest policy when
// the context is nil.
func (c *ExchangeContext) lossPolicy() LossPolicy {
	if c == nil {
		return StrictLossPolicy()
	}
	return c.LossPolicy
}

// RequirePortableArtifacts enforces that provider-authenticated artifacts
// never cross protocol boundaries. Thinking blocks and reasoning items may
// only be passed through unchanged to the same protocol; crossing protocols
// requires an approved loss or a rejection.
func RequirePortableArtifacts(
	request CanonicalRequest,
	target UpstreamProtocol,
	policy LossPolicy,
	report *ConversionReport,
) error {
	if target != UpstreamMessages &&
		len(request.Artifacts.AnthropicThinkingBlocks) != 0 {
		if err := report.Lose(
			policy,
			FeatureAuthenticatedThinking,
			"messages[].content[]",
			"Anthropic thinking blocks cannot be translated across protocols",
		); err != nil {
			return err
		}
	}

	if target != UpstreamResponses &&
		len(request.Artifacts.ResponsesReasoningItems) != 0 {
		if err := report.Lose(
			policy,
			FeatureReasoningSummary,
			"input[]",
			"Responses reasoning items cannot be reproduced in the target protocol",
		); err != nil {
			return err
		}
	}

	return nil
}

// ValidateCanonicalRequest checks the IR invariants before rendering.
func ValidateCanonicalRequest(request CanonicalRequest) error {
	for turnIndex, turn := range request.Turns {
		if len(turn.Parts) == 0 {
			return fmt.Errorf("turn %d has no content parts", turnIndex)
		}
		for partIndex, part := range turn.Parts {
			if part == nil {
				return fmt.Errorf(
					"turn %d part %d is nil",
					turnIndex,
					partIndex,
				)
			}
		}
	}
	return nil
}

// CanonicalStopReason is the normalized terminal reason of a model response.
type CanonicalStopReason string

const (
	CanonicalStopEndTurn      CanonicalStopReason = "end_turn"
	CanonicalStopMaxTokens    CanonicalStopReason = "max_tokens"
	CanonicalStopStopSequence CanonicalStopReason = "stop_sequence"
	CanonicalStopToolUse      CanonicalStopReason = "tool_use"
	CanonicalStopRefusal      CanonicalStopReason = "refusal"
)

// CanonicalUsage is the token usage of a canonical response. Breakdown
// fields are zero when the source did not provide them; the Known flags
// distinguish a real zero from an unknown value, so a renderer never
// fabricates zeros as fact (review-j finding 9).
type CanonicalUsage struct {
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64

	InputKnown      bool
	CacheReadKnown  bool
	CacheWriteKnown bool
	OutputKnown     bool
	ReasoningKnown  bool
}

// Unknown reports whether no usage value was provided by the source at all.
func (u CanonicalUsage) Unknown() bool {
	return !u.InputKnown && !u.OutputKnown &&
		!u.CacheReadKnown && !u.CacheWriteKnown && !u.ReasoningKnown
}

// CanonicalResponseStatus is the lifecycle status of a canonical response.
type CanonicalResponseStatus string

const (
	CanonicalResponseCompleted  CanonicalResponseStatus = "completed"
	CanonicalResponseIncomplete CanonicalResponseStatus = "incomplete"
	CanonicalResponseFailed     CanonicalResponseStatus = "failed"
)

// CanonicalResponse is the decoded model response in canonical form. It
// carries the same part vocabulary as the request IR: assistant turns contain
// text, refusal, and function-call parts; tool results appear in subsequent
// user turns.
type CanonicalResponse struct {
	ID           string
	Model        string
	CreatedAt    int64
	Status       CanonicalResponseStatus
	StopReason   CanonicalStopReason
	StopSequence string
	Turns        []CanonicalTurn
	Usage        CanonicalUsage

	// IncompleteReason is the target-dialect reason when Status is
	// incomplete, e.g. the Responses incomplete_details.reason or the Chat
	// finish_reason.
	IncompleteReason string

	// ErrorMessage is a human-readable failure message when Status is failed
	// or the upstream reported an error.
	ErrorMessage string

	// ReasoningItems carries Responses reasoning output items as source
	// artifacts. They may pass through unchanged only to a Responses target;
	// crossing protocols is a loss or a rejection.
	ReasoningItems []json.RawMessage

	// ChatLogProbs reports that the upstream chat response carried token log
	// probabilities. The client dialects cannot reproduce them; the fact is
	// carried so rendering enters the explicit loss/reject decision instead
	// of silently dropping it.
	ChatLogProbs bool

	// ChatServiceTier is the upstream chat response's service tier (empty
	// when absent). The client dialects cannot represent the tier actually
	// served; a non-empty value enters the explicit loss/reject decision at
	// render time.
	ChatServiceTier string

	// ResponsesPhase reports that an output message carried a phase
	// (commentary vs final_answer). The Messages dialect has no phase, so
	// the distinction enters the explicit loss/reject decision at render
	// time (review-j finding 10).
	ResponsesPhase bool

	// ResponsesControls lists the pinned Responses envelope control fields
	// (background, max_tool_calls, prompt, prompt_cache_key,
	// safety_identifier) present in the decoded envelope. The client
	// dialects cannot reproduce them, so their presence enters the explicit
	// loss/reject decision at render time (review-j finding 13).
	ResponsesControls []string
}

// ValidateCanonicalResponse checks the response IR invariants.
func ValidateCanonicalResponse(response CanonicalResponse) error {
	for turnIndex, turn := range response.Turns {
		if len(turn.Parts) == 0 {
			return fmt.Errorf("response turn %d has no content parts", turnIndex)
		}
		for partIndex, part := range turn.Parts {
			if part == nil {
				return fmt.Errorf("response turn %d part %d is nil", turnIndex, partIndex)
			}
		}
	}
	return nil
}
