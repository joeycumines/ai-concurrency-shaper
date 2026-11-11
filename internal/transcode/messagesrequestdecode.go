package transcode

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// DecodeMessagesRequest decodes an Anthropic Messages request body into the
// canonical IR.
// anthropicClientFunctionToolType is the explicit spelling of an ordinary
// client-side function tool. It is NOT a server tool: a gateway that sends
// it must still get the tool forwarded, not dropped under a server key.
const anthropicClientFunctionToolType = "function"

func DecodeMessagesRequest(
	body []byte,
	policy LossPolicy,
) (DecodeResult, error) {
	var envelope anthropicmessages.Request
	if err := wire.Decode(body, &envelope); err != nil {
		return DecodeResult{}, fmt.Errorf("messages request: %w", err)
	}
	if envelope.Model == "" {
		return DecodeResult{}, errors.New("messages request model is empty")
	}
	if envelope.MaxTokens <= 0 {
		return DecodeResult{}, errors.New("messages request max_tokens must be positive")
	}

	var result DecodeResult
	result.Request.ClientModel = envelope.Model
	result.Request.MaxOutputTokens = &envelope.MaxTokens
	result.Request.Temperature = envelope.Temperature
	result.Request.TopP = envelope.TopP
	result.Request.StopSequences = envelope.StopSequences
	metadata, err := strMap(envelope.Metadata)
	if err != nil {
		return DecodeResult{}, err
	}
	result.Request.Metadata = metadata
	if envelope.Stream != nil {
		result.StreamSet = true
		result.Request.Stream = *envelope.Stream
	}

	if envelope.TopK != nil {
		if err := result.Report.Lose(
			policy,
			FeatureTopK,
			"top_k",
			"top_k has no portable equivalent in the target protocols",
		); err != nil {
			return DecodeResult{}, err
		}
	}

	// The modern Anthropic envelope controls are client-side semantics with
	// no target representation: output_config.budget_tokens is an output
	// budget whose effective cap is already carried by max_tokens, and
	// context_management directs server-side conversation trimming. They are
	// never silently stripped — each present control is an explicit
	// loss/reject decision under anthropic_controls.
	if envelope.ContextManagement != nil {
		if err := result.Report.Lose(
			policy,
			FeatureAnthropicControls,
			"context_management",
			"context_management is a server-side conversation control the target cannot reproduce",
		); err != nil {
			return DecodeResult{}, err
		}
	}
	if envelope.OutputConfig != nil {
		if err := result.Report.Lose(
			policy,
			FeatureAnthropicControls,
			"output_config",
			"the output_config budget duplicates max_tokens, which the target already carries",
		); err != nil {
			return DecodeResult{}, err
		}
	}

	// cache_control is the Anthropic prompt-cache marker real clients attach
	// to text blocks, tools, and the system prompt. It is a pure performance
	// hint with no semantic content and no portable equivalent, so it is a
	// sanctioned observable elision — one deduped note per exchange, never a
	// policy gate and never a silent drop.
	cacheControlNoted := false
	noteCacheControl := func() error {
		if cacheControlNoted {
			return nil
		}
		if err := result.Report.Note(
			FeatureAnthropicControls,
			"cache_control",
			"cache_control performance hints are not forwarded (chat upstreams cache automatically)",
		); err != nil {
			return err
		}
		cacheControlNoted = true
		return nil
	}
	if envelope.System != nil {
		for _, block := range envelope.System.ContentBlocks {
			if block.CacheControl != nil {
				if err := noteCacheControl(); err != nil {
					return DecodeResult{}, err
				}
			}
		}
	}
	deferNoted := false
	for _, tool := range envelope.Tools {
		if tool.CacheControl != nil {
			if err := noteCacheControl(); err != nil {
				return DecodeResult{}, err
			}
		}
		if tool.DeferLoading != nil && *tool.DeferLoading && !deferNoted {
			deferNoted = true
			if err := result.Report.Note(
				FeatureAnthropicControls,
				"tools[].defer_loading",
				"deferred tool loading needs Anthropic server-side tool search; deferred tools are sent eagerly",
			); err != nil {
				return DecodeResult{}, err
			}
		}
	}
	// Blocks nest one level: tool_result content carries its own block
	// array, and the marker is legal on those nested blocks too, so the
	// scan walks the nested content of every top-level block.
	var contentBlocksCarryCacheControl func(blocks []anthropicmessages.ContentBlock) error
	contentBlocksCarryCacheControl = func(blocks []anthropicmessages.ContentBlock) error {
		for _, block := range blocks {
			if block.CacheControl != nil {
				if err := noteCacheControl(); err != nil {
					return err
				}
			}
			if block.Content != nil {
				if err := contentBlocksCarryCacheControl(block.Content.ContentBlocks); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, message := range envelope.Messages {
		if err := contentBlocksCarryCacheControl(message.Content.ContentBlocks); err != nil {
			return DecodeResult{}, err
		}
	}

	// Citations on text blocks are an Anthropic feature (replayed conversation
	// history, document context, or web search citations) that the target
	// request cannot reproduce natively: an approved loss drops the citations
	// structure observably while preserving the text content losslessly.
	var scanCitations func(blocks []anthropicmessages.ContentBlock, pathPrefix string) error
	scanCitations = func(blocks []anthropicmessages.ContentBlock, pathPrefix string) error {
		for i, block := range blocks {
			blockPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
			if len(block.Citations) > 0 {
				if err := result.Report.Lose(
					policy,
					FeatureRequestCitations,
					blockPath+".citations",
					"request citations on text blocks cannot be reproduced in the target request",
				); err != nil {
					return err
				}
			}
			if block.Content != nil {
				if err := scanCitations(block.Content.ContentBlocks, blockPath+".content"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if envelope.System != nil {
		if err := scanCitations(envelope.System.ContentBlocks, "system"); err != nil {
			return DecodeResult{}, err
		}
	}
	for i, message := range envelope.Messages {
		if err := scanCitations(message.Content.ContentBlocks, fmt.Sprintf("messages[%d].content", i)); err != nil {
			return DecodeResult{}, err
		}
	}

	// Thinking configuration. "enabled" requires an explicit budget; members
	// are validated per type against the official contract (enabled={type,
	// budget_tokens}, disabled={type}, adaptive={type,display}) so a
	// cross-type member is a malformed request, never silently ignored.
	// The strict wire decode already rejected unknown
	// fields inside the object.
	if envelope.Thinking != nil {
		switch envelope.Thinking.Type {
		case "enabled":
			if envelope.Thinking.Display != nil {
				return DecodeResult{}, errors.New(
					"messages thinking type enabled cannot carry display (it belongs to adaptive)",
				)
			}
			if envelope.Thinking.BudgetTokens == nil {
				return DecodeResult{}, errors.New(
					"messages thinking type enabled requires budget_tokens",
				)
			}
			if *envelope.Thinking.BudgetTokens <= 0 {
				return DecodeResult{}, errors.New(
					"messages thinking budget_tokens must be positive",
				)
			}
			result.Request.Thinking = &CanonicalThinking{
				Type:         "enabled",
				BudgetTokens: envelope.Thinking.BudgetTokens,
			}
		case "disabled", "adaptive":
			if envelope.Thinking.BudgetTokens != nil {
				return DecodeResult{}, fmt.Errorf(
					"messages thinking type %s cannot carry budget_tokens (it belongs to enabled)",
					envelope.Thinking.Type,
				)
			}
			result.Request.Thinking = &CanonicalThinking{
				Type: envelope.Thinking.Type,
			}
		default:
			return DecodeResult{}, fmt.Errorf(
				"messages thinking type %q is invalid (want enabled, disabled, or adaptive)",
				envelope.Thinking.Type,
			)
		}
	}

	// System prompt becomes a system turn.
	if envelope.System != nil {
		if err := envelope.System.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages system: %w", err)
		}
		parts, err := anthropicContentToCanonical(*envelope.System, policy, &result.Report, &result.Request.Artifacts)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages system: %w", err)
		}
		result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
			Role:  CanonicalSystem,
			Parts: parts,
		})
	}

	// Conversation messages. The modern Anthropic wire admits inline
	// system-role messages alongside user/assistant turns; they map to the
	// canonical system role like the top-level system field.
	for i, message := range envelope.Messages {
		if err := message.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		var role CanonicalRole
		switch message.Role {
		case AnthropicMessageRoleUser:
			role = CanonicalUser
		case AnthropicMessageRoleAssistant:
			role = CanonicalAssistant
		case AnthropicMessageRoleSystem:
			role = CanonicalSystem
		default:
			return DecodeResult{}, fmt.Errorf(
				"messages[%d]: unknown anthropic message role %q",
				i,
				message.Role,
			)
		}
		parts, err := anthropicContentToCanonical(message.Content, policy, &result.Report, &result.Request.Artifacts)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if len(parts) == 0 && len(message.Content.ContentBlocks) > 0 {
			// When all content blocks in an assistant turn were scrubbed (e.g.
			// synthetic thinking blocks), preserve the turn with an empty text
			// part so canonical validation does not reject an empty turn.
			parts = []CanonicalPart{CanonicalText{Text: ""}}
		}
		result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
			Role:  role,
			Parts: parts,
		})
	}

	// Tools. The input_schema raw bytes are preserved — validated as exactly
	// one JSON object at this boundary, never decoded and remarshaled through
	// a map, so large integers, decimals, and exponents survive byte-exact.
	// Server-side definitions (type-discriminated, e.g. web_search_20250305)
	// are the anthropic_server_tools loss decision — approved, they drop
	// observably; rejected, the request fails with a keyed error — never a
	// silent pass and never forwarded to a chat upstream that executes no
	// server tools.
	for i, tool := range envelope.Tools {
		// A tools[] entry is classified by its `type` discriminant. Only the
		// client-function spellings - absent, or the explicit "function" - are
		// carried through. Everything else takes the anthropic_server_tools
		// LOSS decision: approved, it drops observably; rejected, the request
		// fails with a keyed error the operator can actually clear.
		//
		// This is deliberately a catch-all rather than a whitelist of known
		// server spellings. A whitelist would make every NEW Anthropic server
		// tool a hard error until the proxy is rebuilt, and the error could not
		// be cleared with the documented flag because its feature string was
		// not a registered key. Routing the whole class through the existing
		// policy-gated key keeps new server tools survivable by approving the
		// flag, while never dropping one silently.
		//
		// Admitting EVERY non-empty type would be wrong in the other
		// direction: a client function tool a gateway spells
		// {"type":"function",...} must still be forwarded, not filed as a
		// server tool.
		if tool.Type != "" && tool.Type != anthropicClientFunctionToolType {
			if err := result.Report.Lose(
				policy,
				FeatureAnthropicServerTools,
				fmt.Sprintf("tools[%d]", i),
				fmt.Sprintf(
					"the %q server-side tool cannot be reproduced in a chat request",
					tool.Type,
				),
			); err != nil {
				return DecodeResult{}, err
			}
			continue
		}
		if err := tool.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages tools[%d]: %w", i, err)
		}
		if _, err := decodeJSONObject(string(tool.InputSchema)); err != nil {
			return DecodeResult{}, fmt.Errorf(
				"messages tools[%d] input_schema: %w",
				i,
				err,
			)
		}
		description := ""
		if tool.Description != nil {
			description = *tool.Description
		}
		result.Request.Tools = append(result.Request.Tools, CanonicalTool{
			Name:        tool.Name,
			Description: description,
			JSONSchema:  tool.InputSchema,
		})
	}

	// Tool choice: Anthropic "auto"/"none"/"any"/named tool, reconciled
	// against the surviving tools so a dangling reference never reaches a
	// renderer.
	if envelope.ToolChoice != nil {
		choice, err := canonicalizeAnthropicToolChoice(
			*envelope.ToolChoice,
			&result.Report,
			policy,
		)
		if err != nil {
			return DecodeResult{}, err
		}
		choice, err = reconcileToolChoice(
			choice,
			len(envelope.Tools),
			&result.Request,
			&result.Report,
		)
		if err != nil {
			return DecodeResult{}, err
		}
		result.Request.ToolChoice = choice
	}

	return result, nil
}

// anthropicContentToCanonical maps Anthropic message content (string or
// blocks) to canonical parts.
func anthropicContentToCanonical(
	content AnthropicContent,
	policy LossPolicy,
	report *ConversionReport,
	artifacts *SourceArtifacts,
) ([]CanonicalPart, error) {
	if content.ContentStr != nil {
		return []CanonicalPart{CanonicalText{Text: *content.ContentStr}}, nil
	}
	var parts []CanonicalPart
	for i, block := range content.ContentBlocks {
		if err := block.Validate(); err != nil {
			return nil, fmt.Errorf("content block %d: %w", i, err)
		}
		switch block.Type {
		case AnthropicContentBlockTypeText:
			parts = append(parts, CanonicalText{Text: *block.Text})

		case AnthropicContentBlockTypeImage:
			if block.Source == nil {
				return nil, fmt.Errorf("content block %d: image has no source", i)
			}
			parts = append(parts, CanonicalImage{
				MediaType: block.Source.MediaType,
				URL:       block.Source.URL,
				Base64:    block.Source.Data,
			})

		case AnthropicContentBlockTypeDocument:
			if block.Source == nil {
				return nil, fmt.Errorf("content block %d: document has no source", i)
			}
			parts = append(parts, CanonicalDocument{
				MediaType: block.Source.MediaType,
				URL:       block.Source.URL,
				Base64:    block.Source.Data,
			})

		case AnthropicContentBlockTypeToolUse:
			arguments, err := decodeJSONObject(string(block.Input))
			if err != nil {
				return nil, fmt.Errorf("content block %d: tool_use input: %w", i, err)
			}
			raw, err := rawMessage(arguments)
			if err != nil {
				return nil, fmt.Errorf("content block %d: tool_use input: %w", i, err)
			}
			parts = append(parts, CanonicalFunctionCall{
				CallID:    *block.ID,
				Name:      *block.Name,
				Arguments: raw,
			})

		case AnthropicContentBlockTypeToolResult:
			resultParts, err := anthropicContentToCanonical(
				*block.Content,
				policy,
				report,
				artifacts,
			)
			if err != nil {
				return nil, fmt.Errorf("content block %d: tool_result: %w", i, err)
			}
			isError := block.IsError != nil && *block.IsError
			parts = append(parts, CanonicalFunctionResult{
				CallID:  *block.ToolUseID,
				IsError: isError,
				Parts:   resultParts,
			})

		case AnthropicContentBlockTypeThinking,
			AnthropicContentBlockTypeRedactedThinking:
			// REQUEST SCRUBBING (operator-adjudicated design, 2026-09-07):
			// a thinking block whose signature is the proxy's own marker
			// (SyntheticThinkingSignature) is a block this proxy synthesized
			// on a previous response. It is scrubbed here — dropped before
			// artifact capture, never counted under authenticated_thinking,
			// and never forwarded to any upstream — so the synthetic
			// signature can never reach a provider that validates
			// signatures. Non-marker thinking blocks are source-authenticated
			// artifacts and keep the existing behavior below.
			if block.Type == AnthropicContentBlockTypeThinking &&
				block.Signature != nil &&
				*block.Signature == SyntheticThinkingSignature {
				continue
			}
			// Thinking blocks are source-authenticated artifacts. They may
			// pass through unchanged only to a Messages target; crossing
			// protocols is decided by RequirePortableArtifacts.
			raw, err := json.Marshal(block)
			if err != nil {
				return nil, fmt.Errorf("content block %d: %w", i, err)
			}
			artifacts.AnthropicThinkingBlocks = append(
				artifacts.AnthropicThinkingBlocks,
				raw,
			)

		case AnthropicContentBlockTypeMCPToolUse:
			// MCP tools are client-side tools under a server spelling:
			// the fields are identical to tool_use, so the mapping is 1:1
			// with no loss key.
			arguments, err := decodeJSONObject(string(block.Input))
			if err != nil {
				return nil, fmt.Errorf("content block %d: mcp_tool_use input: %w", i, err)
			}
			raw, err := rawMessage(arguments)
			if err != nil {
				return nil, fmt.Errorf("content block %d: mcp_tool_use input: %w", i, err)
			}
			parts = append(parts, CanonicalFunctionCall{
				CallID:    *block.ID,
				Name:      *block.Name,
				Arguments: raw,
			})

		case AnthropicContentBlockTypeMCPToolResult:
			resultParts, err := anthropicContentToCanonical(
				*block.Content,
				policy,
				report,
				artifacts,
			)
			if err != nil {
				return nil, fmt.Errorf("content block %d: mcp_tool_result: %w", i, err)
			}
			isError := block.IsError != nil && *block.IsError
			parts = append(parts, CanonicalFunctionResult{
				CallID:  *block.ToolUseID,
				IsError: isError,
				Parts:   resultParts,
			})

		case AnthropicContentBlockTypeServerToolUse,
			AnthropicContentBlockTypeWebSearchToolResult,
			AnthropicContentBlockTypeCodeExecution,
			AnthropicContentBlockTypeCodeExecutionResult,
			AnthropicContentBlockTypeContainerUpload:
			// Server-executed content a chat upstream cannot express: a
			// fabricated CanonicalFunctionCall would dangle with no
			// upstream executor and corrupt tool pairing, so the only
			// honest outcomes are a keyed drop or a keyed rejection.
			if err := report.Lose(
				policy,
				FeatureAnthropicServerTools,
				fmt.Sprintf("messages[].content[%d]", i),
				fmt.Sprintf(
					"the %q server-side block cannot be reproduced in a chat request",
					string(block.Type),
				),
			); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("content block %d: unknown type %q", i, block.Type)
		}
	}
	return parts, nil
}

// strMap converts a map with any value type to a string map. Messages
// metadata values are strings on the wire; any other value is an error
// (never silently dropped).
func strMap(m map[string]any) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for key, value := range m {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("metadata field %q must be a string", key)
		}
		out[key] = s
	}
	return out, nil
}
