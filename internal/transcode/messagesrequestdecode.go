package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// DecodeMessagesRequest decodes an Anthropic Messages request body into the
// canonical IR.
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
	// Validate source relationships before any loss can discard the evidence:
	// tool kind conflicts, role/placement, and inline cache controls belong to
	// the client contract. The source slices remain intact, so every later
	// decision retains its original message and block coordinates.
	inlineTools, err := validateMessagesToolChanges(&envelope)
	if err != nil {
		return DecodeResult{}, err
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
	noteToolHints := func(tool anthropicmessages.Tool) error {
		if tool.CacheControl != nil {
			if err := noteCacheControl(); err != nil {
				return err
			}
		}
		if tool.DeferLoading != nil && *tool.DeferLoading && !deferNoted {
			deferNoted = true
			if err := result.Report.Note(
				FeatureAnthropicControls,
				"tools[].defer_loading",
				"deferred tool loading needs Anthropic server-side tool search; deferred tools are sent eagerly",
			); err != nil {
				return err
			}
		}
		return nil
	}
	for _, tool := range envelope.Tools {
		if err := noteToolHints(tool); err != nil {
			return DecodeResult{}, err
		}
	}
	for _, inline := range inlineTools {
		if err := noteToolHints(inline.tool); err != nil {
			return DecodeResult{}, err
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

	// A target has one global tool list. Resolve every definition against the
	// original source registry before rendering conversation content, retaining
	// one report decision per inline definition and its actual source path.
	if err := normalizeMessagesTools(envelope.Tools, inlineTools, policy, &result); err != nil {
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
		parts, err := anthropicContentToCanonical(*envelope.System, "system", policy, &result.Report, &result.Request.Artifacts)
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
		parts, err := anthropicContentToCanonical(message.Content, fmt.Sprintf("messages[%d].content", i), policy, &result.Report, &result.Request.Artifacts)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if len(parts) == 0 && len(message.Content.ContentBlocks) > 0 {
			// Availability controls and replayed MCP listings carry only provider
			// state. Their explicit decisions account for the omission; inventing
			// an empty text part would create conversation content. Other scrubbed
			// messages retain the established empty-part boundary encoding.
			if !carriesOnlyToolState(message.Content.ContentBlocks) {
				parts = []CanonicalPart{CanonicalText{Text: ""}}
			}
		} else if len(parts) == 0 && len(message.Content.ContentBlocks) == 0 &&
			message.Content.ContentStr == nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: content array is empty", i)
		}

		if len(parts) > 0 {
			result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
				Role:  role,
				Parts: parts,
			})
		}
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

// messagesInlineTool keeps the decoded definition beside its source path.
// No content slice is filtered: reporting, validation and replay all retain
// the coordinates the client actually sent.
type messagesInlineTool struct {
	tool anthropicmessages.Tool
	path string
}

// validateMessagesToolChanges checks source-only relationships before losses
// can erase them. A provider-defined tool dropped by policy still owns its
// original name and cannot become an unrelated custom function later.
func validateMessagesToolChanges(envelope *anthropicmessages.Request) ([]messagesInlineTool, error) {
	type sourceTool struct {
		kind         string
		typeName     string
		path         string
		declarations int
	}
	registry := make(map[string]sourceTool)
	register := func(tool anthropicmessages.Tool, path string, initial bool) error {
		// An MCP toolset has server identity rather than a function name. It is
		// validated by its own wire arm and remains an unsupported inline form.
		if tool.Name == "" {
			return nil
		}
		kind := messagesToolKind(tool)
		typeName := tool.Type
		if tool.IsClientFunction() {
			typeName = "custom"
		}
		previous, exists := registry[tool.Name]
		if exists && previous.kind != kind {
			return &wire.DecodeError{
				Kind: wire.DecodeContradictoryUnion,
				Path: path + ".name",
				Message: fmt.Sprintf("tool %q has type %q but was declared as %q at %s (tool_name_conflict)",
					tool.Name, typeName, previous.typeName, previous.path),
			}
		}
		if exists && !initial && previous.declarations > 1 {
			return &wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    path + ".name",
				Message: fmt.Sprintf("inline definition of tool %q is ambiguous because tools[] declares that name more than once", tool.Name),
			}
		}
		count := 1
		if initial && exists {
			// Ordinary duplicate tools remain the upstream's decision, as on
			// the other request decoders. Only an inline replacement needs an
			// unambiguous earlier definition to supersede.
			count += previous.declarations
		}
		registry[tool.Name] = sourceTool{kind: kind, typeName: typeName, path: path, declarations: count}
		return nil
	}
	for i, tool := range envelope.Tools {
		if err := register(tool, fmt.Sprintf("tools[%d]", i), true); err != nil {
			return nil, err
		}
	}

	var inline []messagesInlineTool
	var checkContent func(AnthropicContent, string, bool, bool) error
	checkContent = func(content AnthropicContent, path string, changesAllowed, listingAllowed bool) error {
		for i, block := range content.ContentBlocks {
			blockPath := fmt.Sprintf("%s[%d]", path, i)
			switch block.Type {
			case AnthropicContentBlockTypeToolAddition, AnthropicContentBlockTypeToolRemoval:
				if !changesAllowed {
					return &wire.DecodeError{
						Kind:    wire.DecodeContradictoryUnion,
						Path:    blockPath,
						Message: "tool changes are allowed only as top-level content of a system message in messages[]",
					}
				}
				if block.Tool.Type == anthropicmessages.ToolChangeTypeReference {
					// Availability may be removed and restored without destroying
					// a declaration. A reference must nevertheless name a tool
					// declared at this position, never a future definition.
					if _, exists := registry[block.Tool.Name]; !exists {
						return &wire.DecodeError{
							Kind:    wire.DecodeMalformed,
							Path:    blockPath + ".tool.name",
							Message: fmt.Sprintf("tool %q is not declared at this position (tool_reference_unresolved)", block.Tool.Name),
						}
					}
					continue
				}
				if block.Tool.Type != anthropicmessages.ToolChangeTypeDefinition {
					// MCP members require provider server/tool-list context the
					// canonical request cannot represent. Their structurally valid
					// references remain the explicit tool-change loss decision;
					// do not invent member resolution from a bare server name.
					continue
				}
				var tool anthropicmessages.Tool
				if err := wire.Decode(block.Tool.Definition, &tool); err != nil {
					return fmt.Errorf("%s.tool.definition: %w", blockPath, err)
				}
				if block.CacheControl != nil && tool.CacheControl != nil {
					return &wire.DecodeError{
						Kind:    wire.DecodeContradictoryUnion,
						Path:    blockPath + ".cache_control",
						Message: "cache_control may be set on the tool_addition block or its definition, never both",
					}
				}
				if tool.DeferLoading != nil && *tool.DeferLoading && tool.CacheControl != nil {
					return &wire.DecodeError{
						Kind:    wire.DecodeContradictoryUnion,
						Path:    blockPath + ".tool.definition.cache_control",
						Message: "a deferred tool definition cannot carry cache_control",
					}
				}
				if err := register(tool, blockPath+".tool.definition", false); err != nil {
					return err
				}
				inline = append(inline, messagesInlineTool{tool: tool, path: blockPath})

			case AnthropicContentBlockTypeMCPToolListing:
				if !listingAllowed {
					return &wire.DecodeError{
						Kind:    wire.DecodeContradictoryUnion,
						Path:    blockPath,
						Message: "an MCP tool listing may be replayed only as top-level assistant message content",
					}
				}
			}
			if block.Content != nil {
				if err := checkContent(*block.Content, blockPath+".content", false, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if envelope.System != nil {
		if err := envelope.System.Validate(); err != nil {
			return nil, fmt.Errorf("messages system: %w", err)
		}
		if err := checkContent(*envelope.System, "system", false, false); err != nil {
			return nil, err
		}
	}
	for i, message := range envelope.Messages {
		if err := message.Validate(); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if err := checkContent(message.Content, fmt.Sprintf("messages[%d].content", i),
			message.Role == AnthropicMessageRoleSystem, message.Role == AnthropicMessageRoleAssistant); err != nil {
			return nil, err
		}
	}
	if err := validateToolChangePlacement(envelope.Messages); err != nil {
		return nil, err
	}
	return inline, nil
}

// messagesToolKind distinguishes tool families before any approved drop. The
// official tool directory versions provider-defined tools with _YYYYMMDD; changing
// that date within a known family is a version update, not a different kind.
// Unknown discriminants remain exact, and no predefined kind aliases a function.
func messagesToolKind(tool anthropicmessages.Tool) string {
	if tool.IsClientFunction() {
		return "client:custom"
	}
	kind := tool.Type
	if split := strings.LastIndexByte(kind, '_'); split >= 0 && len(kind)-split-1 == 8 {
		version := kind[split+1:]
		dated := true
		for _, digit := range version {
			if digit < '0' || digit > '9' {
				dated = false
				break
			}
		}
		if dated {
			switch kind[:split] {
			case "advisor", "bash", "code_execution", "computer", "memory",
				"text_editor", "tool_search_tool_bm25", "tool_search_tool_regex", "web_fetch", "web_search":
				kind = kind[:split]
			}
		}
	}
	return "provider:" + kind
}

// validateToolChangePlacement applies the inline-tools boundary contract to a
// consecutive group of system messages containing a change. Text-only system
// groups keep the decoder's existing placement support.
func validateToolChangePlacement(messages []anthropicmessages.Message) error {
	for i := 0; i < len(messages); {
		if messages[i].Role != AnthropicMessageRoleSystem {
			i++
			continue
		}
		start := i
		changePath := ""
		for i < len(messages) && messages[i].Role == AnthropicMessageRoleSystem {
			for j, block := range messages[i].Content.ContentBlocks {
				if changePath == "" && (block.Type == AnthropicContentBlockTypeToolAddition || block.Type == AnthropicContentBlockTypeToolRemoval) {
					changePath = fmt.Sprintf("messages[%d].content[%d]", i, j)
				}
			}
			i++
		}
		if changePath == "" {
			continue
		}
		// The general system-message contract also permits a paused assistant
		// turn ending in a server-tool result, but the tool-change beta
		// explicitly excludes that boundary: only text can be appended there.
		if start == 0 || messages[start-1].Role != AnthropicMessageRoleUser {
			return &wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    changePath,
				Message: "a tool-change system group must follow a user message; only text system messages may follow a paused assistant server-tool result",
			}
		}
		if i < len(messages) && messages[i].Role != AnthropicMessageRoleAssistant {
			return &wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    changePath,
				Message: "a tool-change system group must precede an assistant message or end the conversation",
			}
		}
	}
	return nil
}

// normalizeMessagesTools uses one conversion path for declared and inline
// functions. A name index bounds replacement work linearly in the tool count.
func normalizeMessagesTools(declared []anthropicmessages.Tool, inline []messagesInlineTool, policy LossPolicy, result *DecodeResult) error {
	indices := make(map[string]int)
	for i, tool := range declared {
		if !tool.IsClientFunction() {
			if err := result.Report.Lose(policy, FeatureAnthropicServerTools, fmt.Sprintf("tools[%d]", i),
				fmt.Sprintf("the %q provider-defined tool has no portable function schema", tool.Type)); err != nil {
				return err
			}
			continue
		}
		canonical, err := canonicalMessagesFunctionTool(tool)
		if err != nil {
			return fmt.Errorf("messages tools[%d]: %w", i, err)
		}
		indices[tool.Name] = len(result.Request.Tools)
		result.Request.Tools = append(result.Request.Tools, canonical)
	}
	for _, definition := range inline {
		if !definition.tool.IsClientFunction() {
			return &UnrepresentableError{
				Protocol: "messages",
				Path:     definition.path + ".tool.definition",
				Detail:   fmt.Sprintf("the %q provider-defined tool definition has no portable function schema", definition.tool.Type),
			}
		}
		canonical, err := canonicalMessagesFunctionTool(definition.tool)
		if err != nil {
			return fmt.Errorf("%s.tool.definition: %w", definition.path, err)
		}
		detail := fmt.Sprintf("tool %q defined mid-conversation is offered for the whole request (the target has one global tool list, so the availability point cannot be expressed)", canonical.Name)
		previous, exists := indices[canonical.Name]
		if exists {
			same, err := sameMessagesFunctionDefinition(result.Request.Tools[previous], canonical)
			if err != nil {
				return fmt.Errorf("%s.tool.definition.input_schema: %w", definition.path, err)
			}
			if same {
				detail = fmt.Sprintf("tool %q repeats the earlier function definition; it remains offered for the whole request (the target cannot express an inline availability point)", canonical.Name)
			} else {
				detail = fmt.Sprintf("tool %q is redefined mid-conversation; the newest definition serves the whole request, including turns authored against the earlier definition (the target cannot express an inline availability point)", canonical.Name)
			}
		}
		// The runtime logger deduplicates feature/path pairs. One combined
		// decision must describe both hoisting and any actual supersession.
		if err := result.Report.Note(FeatureAnthropicToolChanges, definition.path, detail); err != nil {
			return err
		}
		if exists {
			result.Request.Tools[previous] = canonical
		} else {
			indices[canonical.Name] = len(result.Request.Tools)
			result.Request.Tools = append(result.Request.Tools, canonical)
		}
	}
	return nil
}

func canonicalMessagesFunctionTool(tool anthropicmessages.Tool) (CanonicalTool, error) {
	if err := tool.Validate(); err != nil {
		return CanonicalTool{}, err
	}
	if _, err := decodeJSONObject(string(tool.InputSchema)); err != nil {
		return CanonicalTool{}, fmt.Errorf("input_schema: %w", err)
	}
	description := ""
	if tool.Description != nil {
		description = *tool.Description
	}
	return CanonicalTool{Name: tool.Name, Description: description, JSONSchema: tool.InputSchema}, nil
}

// sameMessagesFunctionDefinition ignores JSON object ordering and whitespace
// when recognizing a repeated definition. UseNumber preserves numeric lexemes:
// adjacent large integer values must never collapse through float64 rounding.
func sameMessagesFunctionDefinition(a, b CanonicalTool) (bool, error) {
	if a.Name != b.Name || a.Description != b.Description {
		return false, nil
	}
	if bytes.Equal(a.JSONSchema, b.JSONSchema) {
		return true, nil
	}
	var values [2]any
	for i, schema := range []json.RawMessage{a.JSONSchema, b.JSONSchema} {
		decoder := json.NewDecoder(bytes.NewReader(schema))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			return false, err
		}
	}
	return reflect.DeepEqual(values[0], values[1]), nil
}

// A message containing only these blocks carries provider tool state rather
// than conversation content. Its hoisting or loss decisions account for the
// state; no empty text turn is manufactured when the state disappears.
func carriesOnlyToolState(blocks []anthropicmessages.ContentBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		switch block.Type {
		case AnthropicContentBlockTypeToolAddition, AnthropicContentBlockTypeToolRemoval, AnthropicContentBlockTypeMCPToolListing:
		default:
			return false
		}
	}
	return true
}

// anthropicContentToCanonical maps Anthropic message content (string or
// blocks) to canonical parts.
func anthropicContentToCanonical(
	content AnthropicContent,
	path string,
	policy LossPolicy,
	report *ConversionReport,
	artifacts *SourceArtifacts,
) ([]CanonicalPart, error) {
	if content.ContentStr != nil {
		return []CanonicalPart{CanonicalText{Text: *content.ContentStr}}, nil
	}
	var parts []CanonicalPart
	for i, block := range content.ContentBlocks {
		blockPath := fmt.Sprintf("%s[%d]", path, i)
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
				blockPath+".content",
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
				blockPath+".content",
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
			// anthropic_tool_changes decision. Definitions have already been
			// normalized without filtering this source slice; skip only their
			// content contribution so subsequent block paths stay unchanged.
			if block.Tool.Type == anthropicmessages.ToolChangeTypeDefinition {
				continue
			}
			if err := report.Lose(
				policy,
				FeatureAnthropicToolChanges,
				blockPath,
				fmt.Sprintf(
					"the %q mid-conversation tool change cannot keep its availability timing in a chat request",
					string(block.Type),
				),
			); err != nil {
				return nil, err
			}

		case AnthropicContentBlockTypeMCPToolListing:
			// Replayed listings are client-supplied provider state. Unlike an
			// unsolicited response listing, losing this request state is a
			// policy decision. A listing contributes no conversation text; a
			// listing-only assistant message is omitted with the state.
			if err := report.Lose(policy, FeatureAnthropicToolChanges, blockPath,
				"the replayed MCP tool listing pins provider tools; its provider-only state is dropped, and no conversation turn is invented when the message contains only listings"); err != nil {
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
				blockPath,
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
