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

	// top_k is a sampling cutoff the Messages target carries natively, so the
	// decoder (which has no target) stores it: deciding the loss here would
	// destroy a value another target can reproduce, and top_k is not a default
	// loss, so a default deployment would refuse the request outright. The
	// Chat and Responses renderers record the loss because THEIR wire has no
	// such field. A NEGATIVE cutoff is malformed by the API's own contract, so
	// it is rejected at this boundary rather than forwarded (zero is the
	// documented "no cutoff" value and is carried).
	if envelope.TopK != nil && *envelope.TopK < 0 {
		return DecodeResult{}, fmt.Errorf("messages top_k must not be negative, got %d", *envelope.TopK)
	}
	result.Request.TopK = envelope.TopK

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

	// Thinking configuration. The contract has four arms —
	// enabled={type,budget_tokens,display?}, disabled={type},
	// between_tools={type}, adaptive={type,display?} — and display's enum is
	// summarized|omitted|updates, the last gated upstream by the
	// Anthropic-Beta member thinking-display-updates-2026-08-18 that
	// claude-cli 2.x sends. Every member is validated per arm, so a
	// cross-type member is a malformed request rather than silently ignored,
	// and no spelling outside the enum is treated as a legal "absent"
	// (absence is the field being omitted). The strict wire decode already
	// rejected unknown fields inside the object.
	if envelope.Thinking != nil {
		thinking := &CanonicalThinking{Type: envelope.Thinking.Type}
		switch envelope.Thinking.Type {
		case "enabled", "disabled", "between_tools", "adaptive":
		default:
			return DecodeResult{}, fmt.Errorf(
				"messages thinking type %q is invalid (want enabled, disabled, between_tools, or adaptive)",
				envelope.Thinking.Type,
			)
		}
		if envelope.Thinking.Display != nil {
			switch display := *envelope.Thinking.Display; display {
			case "summarized", "omitted", "updates":
				thinking.Display = display
			default:
				return DecodeResult{}, fmt.Errorf(
					"messages thinking display %q is invalid (want summarized, omitted, or updates)",
					display,
				)
			}
		}
		switch envelope.Thinking.Type {
		case "enabled":
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
			thinking.BudgetTokens = envelope.Thinking.BudgetTokens
		case "disabled":
			// disabled turns thinking off outright, so there is nothing to
			// display.
			if thinking.Display != "" {
				return DecodeResult{}, errors.New(
					"messages thinking type disabled cannot carry display (there is nothing to display)",
				)
			}
		case "between_tools":
			// The lowest setting on Claude Sonnet 5.5: it turns off up-front
			// thinking and accepts no other field at all.
			if thinking.Display != "" {
				return DecodeResult{}, errors.New(
					"messages thinking type between_tools cannot carry display (it accepts no other field)",
				)
			}
		}
		// budget_tokens belongs to the enabled arm alone; accepting it on
		// another arm would silently tolerate a contradictory union member.
		if envelope.Thinking.Type != "enabled" && envelope.Thinking.BudgetTokens != nil {
			return DecodeResult{}, fmt.Errorf(
				"messages thinking type %s cannot carry budget_tokens (it belongs to enabled)",
				envelope.Thinking.Type,
			)
		}
		// The Messages target carries display natively, so the decoder (which
		// has no target) stores it; each other target records the
		// request_reasoning loss for the preference it cannot reproduce.
		result.Request.Thinking = thinking
	}

	// Mid-conversation tool definitions (inline-tools-2026-09-15). A
	// tool_addition carrying `tool_definition` offers a NEW or CHANGED tool
	// from that point onward; a chat target has one global tool list, so the
	// only representable form is the tool being present for the whole request.
	// Hoisting it preserves the tool's availability and its definition
	// losslessly, and the timing change is recorded as a Note. This runs BEFORE
	// the message walks so a hoisted definition is never also charged as a
	// withdrawal-style loss below.
	hoisted, hoistedOut, err := hoistToolDefinitions(&envelope, &result)
	if err != nil {
		return DecodeResult{}, err
	}

	// System prompt becomes a system turn. The empty-part synthesis below is
	// shared with the messages[] loop: a system prompt whose blocks were all
	// scrubbed (marker-signature thinking), moved to artifacts (authenticated
	// thinking), or dropped under an approved server-tool loss leaves zero
	// parts, and canonical validation refuses a turn with no content. Keeping
	// the turn with an empty text part preserves the boundary while leaving the
	// artifact and loss decisions to the gates that own them.
	if envelope.System != nil {
		if err := envelope.System.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages system: %w", err)
		}
		parts, err := anthropicContentToCanonical(*envelope.System, policy, &result.Report, &result.Request.Artifacts)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages system: %w", err)
		}
		if len(parts) == 0 && len(envelope.System.ContentBlocks) > 0 {
			parts = []CanonicalPart{CanonicalText{Text: ""}}
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
			// A tool-change-only message is the one shape that is not a turn: a
			// mid-conversation tool_addition / tool_removal carries availability
			// semantics, never conversation content, so synthesizing an empty
			// text part would invent prompt text the client never wrote and
			// charge the client a mid-conversation-system move for a turn that
			// exists only because of a hoist. Every OTHER all-scrubbed turn
			// (proxy-synthesized marker thinking, an approved server-tool drop)
			// keeps its boundary with an empty part, so canonical validation
			// does not reject it.
			if !carriesOnlyToolChanges(message.Content.ContentBlocks) {
				parts = []CanonicalPart{CanonicalText{Text: ""}}
			}
		} else if len(parts) == 0 && len(message.Content.ContentBlocks) == 0 &&
			message.Content.ContentStr == nil {
			if _, wasHoisted := hoistedOut[i]; wasHoisted {
				// All of this message's blocks became request tools.
				continue
			}
			// An explicit empty content ARRAY is not a legal Messages message
			// (the union selects a variant, and an empty array selects none), so
			// it is a client-dialect error rather than a silently dropped turn.
			return DecodeResult{}, fmt.Errorf(
				"messages[%d]: content array is empty", i,
			)
		}
		if len(parts) > 0 {
			result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
				Role:  role,
				Parts: parts,
			})
		}
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

	// Tools defined mid-conversation (hoisted above) join the declared list.
	// A definition that reuses an earlier name is a SCHEMA CHANGE from its
	// position onward — the contract's normal mechanism for shipping a tool
	// whose schema changed — while a chat request carries ONE schema per name
	// for the whole conversation. The only representable encoding is
	// last-definition-wins: the newest definition serves every turn, and the
	// turns authored against the earlier one are served the newer schema. That
	// is a sanctioned encoding, not a policy decision (no policy could restore
	// per-turn schemas), so it is recorded as a Note per superseded
	// declaration. It is NOT a hard error: refusing it would 400 exactly the
	// traffic this path exists to serve.
	for _, hoist := range hoisted {
		previous := -1
		for j, existing := range result.Request.Tools {
			if existing.Name == hoist.tool.Name {
				previous = j
				break
			}
		}
		if previous >= 0 {
			if err := result.Report.Note(
				FeatureAnthropicToolChanges,
				hoist.path,
				fmt.Sprintf(
					"tool %q is redefined mid-conversation; the newest definition serves the whole request, so turns authored against the earlier schema see the updated one",
					hoist.tool.Name,
				),
			); err != nil {
				return DecodeResult{}, err
			}
			// Last-definition-wins: replace in place so the tool list never
			// carries two entries under one name.
			result.Request.Tools[previous] = hoist.tool
			continue
		}
		result.Request.Tools = append(result.Request.Tools, hoist.tool)
	}

	// Tool choice: Anthropic "auto"/"none"/"any"/named tool, reconciled
	// against the surviving tools so a dangling reference never reaches a
	// renderer.
	if envelope.ToolChoice != nil {
		choice, err := canonicalizeAnthropicToolChoice(
			*envelope.ToolChoice,
			&result.Request,
		)
		if err != nil {
			return DecodeResult{}, err
		}
		choice, err = reconcileToolChoice(
			choice,
			len(envelope.Tools),
			&result.Request,
			&result.Report,
			FeatureAnthropicServerTools,
		)
		if err != nil {
			return DecodeResult{}, err
		}
		result.Request.ToolChoice = choice
	}

	return result, nil
}

// carriesOnlyToolChanges reports whether every block in content is a
// mid-conversation tool change. Such a message carries availability semantics
// and no conversation content, so it is not a conversational turn: keeping it
// as an empty text turn would put proxy-invented prompt text on the wire and
// charge the client a position it never occupied.
func carriesOnlyToolChanges(blocks []anthropicmessages.ContentBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		switch block.Type {
		case AnthropicContentBlockTypeToolAddition, AnthropicContentBlockTypeToolRemoval:
		default:
			return false
		}
	}
	return true
}

// hoistToolDefinitions extracts every tool_addition carrying a tool_definition
// and returns the tools it declares. The definition is a tools[] entry (a custom
// tool, an Anthropic-defined client or server tool, or an mcp_toolset), so it is
// decoded exactly like one.
//
// A chat target carries ONE global tool list, so the only representable form of
// "offer this tool from here onward" is the tool being present for the whole
// request. Hoisting preserves the definition losslessly and records the timing
// change as a Note. The extracted block is REMOVED from the envelope so the
// content walk cannot also charge it as a withdrawal-style loss.
//
// The reference and removal arms carry no definition and are left in place for
// the content walk to decide.
func hoistToolDefinitions(
	envelope *anthropicmessages.Request,
	result *DecodeResult,
) ([]hoistedTool, map[int]struct{}, error) {
	var hoisted []hoistedTool
	// emptied records the message indices that contained ONLY hoisted
	// definitions: after the tool is lifted into the request's tools list those
	// messages hold no content, and they are not turns. Distinguishing them
	// from a client-sent empty array is what keeps a genuinely empty array a
	// typed rejection.
	emptied := make(map[int]struct{})
	for i := range envelope.Messages {
		message := &envelope.Messages[i]
		if message.Content.ContentBlocks == nil {
			continue
		}
		blocks := message.Content.ContentBlocks[:0]
		for j := range message.Content.ContentBlocks {
			block := message.Content.ContentBlocks[j]
			if block.Type != AnthropicContentBlockTypeToolAddition ||
				block.Tool == nil ||
				block.Tool.Type != anthropicmessages.ToolChangeTypeDefinition {
				blocks = append(blocks, block)
				continue
			}
			tool, err := decodeHoistedTool(*block.Tool)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"messages[%d].content[%d]: tool_addition: %w", i, j, err,
				)
			}
			hoisted = append(hoisted, hoistedTool{
				tool: tool,
				path: fmt.Sprintf("messages[%d].content[%d]", i, j),
			})
			// The tool becomes available for the whole request, so its earlier
			// availability threshold is gone. Recorded once per block, naming
			// the REAL message and block index: a bare `messages[]` collides
			// across messages, and the report dedupes by (feature, path), so
			// two hoists in different messages would be logged as one.
			if err := result.Report.Note(
				FeatureAnthropicToolChanges,
				fmt.Sprintf("messages[%d].content[%d]", i, j),
				fmt.Sprintf(
					"tool %q defined mid-conversation is offered for the whole request (a chat target has one global tool list, so the availability point cannot be expressed)",
					tool.Name,
				),
			); err != nil {
				return nil, nil, err
			}
		}
		if len(blocks) == 0 && len(message.Content.ContentBlocks) > 0 {
			emptied[i] = struct{}{}
		}
		// Keep the two states distinguishable: an emptied content array must
		// stay empty rather than become a nil-block sentinel.
		message.Content.ContentBlocks = blocks
	}
	return hoisted, emptied, nil
}

// hoistedTool pairs a tool hoisted out of a mid-conversation definition with the
// canonical path it came from, so every decision about it names real
// coordinates rather than a report-slice index.
type hoistedTool struct {
	tool CanonicalTool
	path string
}

// decodeHoistedTool decodes a tool_definition payload as a canonical tool.
//
// The definition is a tools[] entry, so it is decoded through the SAME
// anthropicmessages.Tool type the declared list uses. That is load-bearing:
// Tool.UnmarshalJSON routes a client-function spelling through the STRICT
// client decode (unknown fields rejected) and a type-tagged server tool through
// the tolerant capture the loss report needs. Reimplementing the decode here
// with a bare json.Unmarshal silently discarded every documented tools[] member
// outside a five-field probe (input_examples, strict, allowed_callers,
// defer_loading, cache_control, max_uses, ...) — the exact client-contract hole
// the strict decode exists to close.
func decodeHoistedTool(change anthropicmessages.ToolChange) (CanonicalTool, error) {
	var tool anthropicmessages.Tool
	if err := json.Unmarshal(change.Definition, &tool); err != nil {
		return CanonicalTool{}, err
	}
	// A definition carrying a type discriminator is a server-side tool. This
	// loop runs before the declared-tools walk (which files server tools under
	// anthropic_server_tools), so hoisting one here would smuggle a server tool
	// past that gate; it is refused instead.
	if tool.Type != "" && tool.Type != anthropicClientFunctionToolType {
		return CanonicalTool{}, fmt.Errorf(
			"the %q server-side tool definition cannot be reproduced in a chat request",
			tool.Type,
		)
	}
	if err := tool.Validate(); err != nil {
		return CanonicalTool{}, err
	}
	schema := tool.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	if _, err := decodeJSONObject(string(schema)); err != nil {
		return CanonicalTool{}, fmt.Errorf("input_schema: %w", err)
	}
	description := ""
	if tool.Description != nil {
		description = *tool.Description
	}
	return CanonicalTool{
		Name:        tool.Name,
		Description: description,
		JSONSchema:  schema,
	}, nil
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

		case AnthropicContentBlockTypeToolAddition,
			AnthropicContentBlockTypeToolRemoval:
			// A mid-conversation tool change: the block alters which tools the
			// model is offered from this point onward. It contributes no
			// conversation content, so it produces no canonical part; its only
			// honest fate on a target with one global tool list is the
			// anthropic_tool_changes decision. A tool_definition is handled
			// separately by hoistToolDefinitions before this walk, so a
			// surviving block here is a withdrawal or a reference.
			if err := report.Lose(
				policy,
				FeatureAnthropicToolChanges,
				fmt.Sprintf("messages[].content[%d]", i),
				fmt.Sprintf(
					"the %q mid-conversation tool change cannot keep its availability timing in a chat request",
					string(block.Type),
				),
			); err != nil {
				return nil, err
			}

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
