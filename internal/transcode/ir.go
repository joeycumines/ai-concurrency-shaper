package transcode

import (
	"encoding/json"
	"errors"
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

	// StreamSet is true when the request body explicitly carried a stream
	// field (true or false). The handler applies the documented stream-intent
	// precedence: a present body field is authoritative over the client
	// Accept header (review-08 blocker 1).
	StreamSet bool
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

	// StreamIntent records the resolved stream mode of the exchange: the
	// request body's stream field when explicitly present, otherwise the
	// client Accept header's most-preferred acceptable representation
	// (review-08 blocker 1). A stream/JSON mismatch on the upstream response
	// is an error rather than a silent mode change.
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

// ValidateCanonicalRequest checks the IR invariants before rendering:
// every turn role is one of the canonical values, every turn has content,
// every part is non-nil, tool calls carry non-empty identity (item/call id
// and name) with object-shaped arguments, and tool results carry a non-empty
// call id (review-08 additional 10).
func ValidateCanonicalRequest(request CanonicalRequest) error {
	for turnIndex, turn := range request.Turns {
		switch turn.Role {
		case CanonicalUser, CanonicalAssistant, CanonicalSystem, CanonicalDeveloper:
		default:
			return fmt.Errorf("turn %d has invalid role %q", turnIndex, turn.Role)
		}
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
			// Central role semantics (review-z commit 2): function calls only
			// in assistant turns, function results only in user turns, and
			// refusal only on assistant messages — enforced once here, not
			// per-renderer.
			switch part.(type) {
			case CanonicalFunctionCall:
				if turn.Role != CanonicalAssistant {
					return fmt.Errorf(
						"turn %d part %d: function call in %q turn",
						turnIndex,
						partIndex,
						turn.Role,
					)
				}
			case CanonicalFunctionResult:
				if turn.Role != CanonicalUser {
					return fmt.Errorf(
						"turn %d part %d: function result in %q turn",
						turnIndex,
						partIndex,
						turn.Role,
					)
				}
			case CanonicalRefusal:
				if turn.Role != CanonicalAssistant {
					return fmt.Errorf(
						"turn %d part %d: refusal in %q turn",
						turnIndex,
						partIndex,
						turn.Role,
					)
				}
			}
			if err := validateCanonicalPart(part); err != nil {
				return fmt.Errorf("turn %d part %d: %w", turnIndex, partIndex, err)
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

// Optional[T] is a presence-aware value: Set distinguishes an explicit
// value from an absent one.
type Optional[T any] struct {
	Value T
	Set   bool
}

// CanonicalUsage is the token usage of a canonical response. Breakdown
// fields are zero when the source did not provide them; the Known flags
// distinguish a real zero from an unknown value, so a renderer never
// fabricates zeros as fact (review-j finding 9). TotalTokens carries the
// source's own total when provided (TotalKnown); renderers derive the total
// from the parts only when the source total is unknown (review-k finding 6).
type CanonicalUsage struct {
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64

	InputKnown      bool
	CacheReadKnown  bool
	CacheWriteKnown bool
	OutputKnown     bool
	ReasoningKnown  bool
	TotalKnown      bool
}

// Unknown reports whether no usage value was provided by the source at all.
func (u CanonicalUsage) Unknown() bool {
	return !u.InputKnown && !u.OutputKnown &&
		!u.CacheReadKnown && !u.CacheWriteKnown && !u.ReasoningKnown && !u.TotalKnown
}

// CanonicalResponseStatus is the lifecycle status of a canonical response.
type CanonicalResponseStatus string

const (
	CanonicalResponseCompleted  CanonicalResponseStatus = "completed"
	CanonicalResponseIncomplete CanonicalResponseStatus = "incomplete"
	CanonicalResponseFailed     CanonicalResponseStatus = "failed"
)

// CanonicalStop is the normalized terminal condition of a model response.
type CanonicalStop struct {
	Reason CanonicalStopReason
	// Sequence is the custom stop sequence that terminated generation
	// (empty unless Reason is stop_sequence).
	Sequence string
}

// ToolArguments preserves model-generated function-call arguments exactly.
// Model output is not guaranteed to be valid JSON; Raw carries the original
// text byte-exact, Object carries a cloned parse when the raw text is a JSON
// object (IsObject true), and invalid model output is NOT an upstream defect
// (review-z commit 2). Targets whose wire requires a string preserve Raw;
// targets whose wire requires an object (Anthropic tool_use.input) require
// IsObject and otherwise report a local unrepresentable-output error.
type ToolArguments struct {
	Raw      string
	Object   json.RawMessage
	IsObject bool
}

// ParseToolArguments parses raw model-generated arguments: on a successful
// strict object decode the parsed clone is stored and IsObject is true;
// otherwise the raw text is preserved and IsObject stays false.
func ParseToolArguments(raw string) ToolArguments {
	out := ToolArguments{Raw: raw}
	if object, err := decodeJSONObject(raw); err == nil {
		out.Object = mustRawMessage(object)
		out.IsObject = true
	}
	return out
}

// CanonicalResponseItem is one output item of a canonical response. Item
// boundaries, output ordering, phases, reasoning artifacts, function calls,
// and conversation-state items survive until the target renderer; renderers
// may merge items only through an explicit named loss (output_item_boundaries,
// output_phase).
type CanonicalResponseItem interface {
	isCanonicalResponseItem()
}

// CanonicalMessageItem is an assistant output message item.
type CanonicalMessageItem struct {
	// ID is the source item identity (Responses output message id; empty for
	// sources without item identity).
	ID    string
	Role  CanonicalRole
	Phase Optional[string]
	Parts []CanonicalPart
}

func (*CanonicalMessageItem) isCanonicalResponseItem() {}

// CanonicalFunctionCallItem is an output function_call item.
type CanonicalFunctionCallItem struct {
	ItemID    string
	CallID    string
	Name      string
	Arguments ToolArguments
}

func (*CanonicalFunctionCallItem) isCanonicalResponseItem() {}

// CanonicalFunctionResultItem is an output function_call_output item —
// conversation state that belongs to the NEXT request, not the model
// response. It is preserved as its own item so its identity survives until
// the target renderer decides (output_item_boundaries).
type CanonicalFunctionResultItem struct {
	ItemID  string
	CallID  string
	IsError bool
	Parts   []CanonicalPart
}

func (*CanonicalFunctionResultItem) isCanonicalResponseItem() {}

// CanonicalReasoningItem is an output reasoning item, passed through
// unchanged to a Responses target and loss-gated or rejected elsewhere.
type CanonicalReasoningItem struct {
	Raw json.RawMessage
}

func (*CanonicalReasoningItem) isCanonicalResponseItem() {}

// ResponseSourceArtifacts carries source-specific facts that drive
// loss/reject decisions at render time; they never cross protocol
// boundaries silently.
type ResponseSourceArtifacts struct {
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

	// ResponsesControls lists the pinned Responses envelope control fields
	// (background, max_tool_calls, prompt, prompt_cache_key,
	// safety_identifier) present in the decoded envelope. The client
	// dialects cannot reproduce them, so their presence enters the explicit
	// loss/reject decision at render time (review-j finding 13).
	ResponsesControls []string
}

// CanonicalResponse is the decoded model response in canonical form: an
// ordered list of output items (message, function call, function result,
// reasoning) whose boundaries and identities are preserved until the target
// renderer.
type CanonicalResponse struct {
	ID    string
	Model string
	// CreatedAt is float64 end-to-end: the Responses contract's created_at
	// is a float64 and fractional timestamps must survive decoding, stream
	// identity pinning, and rendering without truncation. Chat's integer
	// created converts at the decode boundary.
	CreatedAt float64
	Status    CanonicalResponseStatus
	Stop      CanonicalStop
	Items     []CanonicalResponseItem
	Usage     CanonicalUsage

	// IncompleteReason is the target-dialect reason when Status is
	// incomplete, e.g. the Responses incomplete_details.reason or the Chat
	// finish_reason.
	IncompleteReason string

	// ErrorMessage is a human-readable failure message when Status is failed
	// or the upstream reported an error.
	ErrorMessage string

	// Source carries the source-specific artifacts that drive loss/reject
	// decisions at render time.
	Source ResponseSourceArtifacts
}

// ValidateCanonicalResponse checks the response IR invariants: a non-empty
// model, a valid status and stop reason, role-correct items and parts,
// non-negative and consistent usage, and tool-call identity (review-08
// additional 10; review-z commit 2 central role validation).
func ValidateCanonicalResponse(response CanonicalResponse) error {
	if response.Model == "" {
		return errors.New("response has no model")
	}
	switch response.Status {
	case CanonicalResponseCompleted, CanonicalResponseIncomplete, CanonicalResponseFailed:
	case "":
		return errors.New("response has no status")
	default:
		return fmt.Errorf("response has invalid status %q", response.Status)
	}
	switch response.Stop.Reason {
	case CanonicalStopEndTurn, CanonicalStopMaxTokens, CanonicalStopStopSequence,
		CanonicalStopToolUse, CanonicalStopRefusal:
	case "":
		return errors.New("response has no stop reason")
	default:
		return fmt.Errorf("response has invalid stop reason %q", response.Stop.Reason)
	}

	// Central role semantics, enforced once here (not per-renderer): only
	// assistant output messages carry content; function calls are
	// assistant-output items; function results are conversation-state items
	// whose call identity must reference a known prior call; refusal parts
	// are assistant-only.
	knownCalls := make(map[string]struct{})
	for itemIndex, item := range response.Items {
		switch value := item.(type) {
		case *CanonicalMessageItem:
			if value.Role != CanonicalAssistant {
				return fmt.Errorf(
					"response item %d: message role %q is not assistant",
					itemIndex,
					value.Role,
				)
			}
			if value.Phase.Set {
				switch value.Phase.Value {
				case "commentary", "final_answer":
				default:
					return fmt.Errorf(
						"response item %d: invalid message phase %q",
						itemIndex,
						value.Phase.Value,
					)
				}
			}
			for partIndex, part := range value.Parts {
				if part == nil {
					return fmt.Errorf(
						"response item %d part %d is nil",
						itemIndex,
						partIndex,
					)
				}
				if err := validateResponsePart(part); err != nil {
					return fmt.Errorf(
						"response item %d part %d: %w",
						itemIndex,
						partIndex,
						err,
					)
				}
			}

		case *CanonicalFunctionCallItem:
			if value.CallID == "" {
				return fmt.Errorf("response item %d: function call has empty call id", itemIndex)
			}
			if value.Name == "" {
				return fmt.Errorf("response item %d: function call has empty name", itemIndex)
			}
			knownCalls[value.CallID] = struct{}{}

		case *CanonicalFunctionResultItem:
			if value.CallID == "" {
				return fmt.Errorf("response item %d: function result has empty call id", itemIndex)
			}
			if _, ok := knownCalls[value.CallID]; !ok {
				return fmt.Errorf(
					"response item %d: function result references unknown call %q",
					itemIndex,
					value.CallID,
				)
			}
			for partIndex, part := range value.Parts {
				if part == nil {
					return fmt.Errorf(
						"response item %d result part %d is nil",
						itemIndex,
						partIndex,
					)
				}
				if err := validateCanonicalPart(part); err != nil {
					return fmt.Errorf(
						"response item %d result part %d: %w",
						itemIndex,
						partIndex,
						err,
					)
				}
			}

		case *CanonicalReasoningItem:
			if len(value.Raw) == 0 || !json.Valid(value.Raw) {
				return fmt.Errorf("response item %d: reasoning item is not valid JSON", itemIndex)
			}

		default:
			return fmt.Errorf("response item %d: unknown item type %T", itemIndex, item)
		}
	}

	if err := validateCanonicalUsage(response.Usage); err != nil {
		return fmt.Errorf("response usage: %w", err)
	}
	return nil
}

// validateResponsePart checks the per-part invariants of ASSISTANT output
// message items: only text and refusal parts are model output; function
// calls and results are their own items, and a part smuggling them into a
// message is a role violation.
func validateResponsePart(part CanonicalPart) error {
	switch part.(type) {
	case CanonicalText:
		return nil
	case CanonicalRefusal:
		return nil
	default:
		return fmt.Errorf(
			"assistant output message carries non-output part %T",
			part,
		)
	}
}

// validateCanonicalPart checks the per-part invariants shared by request and
// response IR: tool calls carry a non-empty call id and name with an
// object-shaped arguments payload, and tool results carry a non-empty call id
// (review-08 additional 10). ItemID is Responses-specific and may be empty
// for Chat/Messages-originated calls.
func validateCanonicalPart(part CanonicalPart) error {
	switch p := part.(type) {
	case CanonicalImage:
		// Exactly one image source (review-z commit 2).
		if (p.URL == "") == (p.Base64 == "") {
			return errors.New("image part requires exactly one of url or base64")
		}
	case CanonicalDocument:
		// Exactly one document source (review-z commit 2).
		selected := 0
		if p.URL != "" {
			selected++
		}
		if p.Base64 != "" {
			selected++
		}
		if p.FileID != "" {
			selected++
		}
		if selected != 1 {
			return errors.New("document part requires exactly one of url, base64, or file id")
		}
	case CanonicalFunctionCall:
		if p.CallID == "" {
			return errors.New("function call has empty call id")
		}
		if p.Name == "" {
			return errors.New("function call has empty name")
		}
		if _, err := decodeJSONObject(string(p.Arguments)); err != nil {
			return fmt.Errorf("function call %q arguments: %w", p.Name, err)
		}
	case CanonicalFunctionResult:
		if p.CallID == "" {
			return errors.New("function result has empty call id")
		}
		// Nested function-result parts validate recursively.
		for partIndex, nested := range p.Parts {
			if nested == nil {
				return fmt.Errorf("function result part %d is nil", partIndex)
			}
			if err := validateCanonicalPart(nested); err != nil {
				return fmt.Errorf("function result part %d: %w", partIndex, err)
			}
		}
	}
	return nil
}

// validateCanonicalUsage enforces non-negative token counts and, where a
// total is known, its consistency with the known components (review-08
// additional 10/11). An input + output total must not underflow or exceed a
// known total.
func validateCanonicalUsage(u CanonicalUsage) error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.ReasoningTokens < 0 ||
		u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.TotalTokens < 0 {
		return errors.New("usage has negative token counts")
	}
	// A known total must be the EXACT sum of the known components: the
	// chat and responses contracts both define total_tokens as
	// input + output (review-z commit 5). Only chat and responses sources
	// reach this branch with a known total — anthropic wire has no total —
	// so exact equality cannot reject a legitimate cache-inclusive total.
	if u.InputKnown && u.OutputKnown && u.TotalKnown {
		sum := u.InputTokens + u.OutputTokens
		if sum < u.InputTokens || sum != u.TotalTokens {
			return &UsageArithmeticError{
				Detail: fmt.Sprintf(
					"total %d is not the exact sum of input %d + output %d",
					u.TotalTokens, u.InputTokens, u.OutputTokens,
				),
				SourceMismatch: true,
				Input:          u.InputTokens,
				Output:         u.OutputTokens,
				Total:          u.TotalTokens,
			}
		}
	}
	return nil
}
