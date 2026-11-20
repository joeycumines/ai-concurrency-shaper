// Package anthropicmessages implements the pinned Anthropic Messages wire
// contract (source: anthropic-api 2023-06-01):
// distinct strict types for the request, the response, and the stream
// events.
//
// The package is deliberately self-contained: it imports only the shared
// wire package, never the transcode package, so the pinned contract types
// can never depend on transcode internals (no import cycle).
package anthropicmessages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Role is the role of an Anthropic message.
type Role string

// Role values. RoleSystem covers the modern inline system-role message
// (Claude Code 2.1.x emits system turns inside the messages array).
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// ContentBlockType is the type of an Anthropic content block.
type ContentBlockType string

// ContentBlockType values.
const (
	ContentBlockTypeText             ContentBlockType = "text"
	ContentBlockTypeImage            ContentBlockType = "image"
	ContentBlockTypeDocument         ContentBlockType = "document"
	ContentBlockTypeToolUse          ContentBlockType = "tool_use"
	ContentBlockTypeToolResult       ContentBlockType = "tool_result"
	ContentBlockTypeThinking         ContentBlockType = "thinking"
	ContentBlockTypeRedactedThinking ContentBlockType = "redacted_thinking"
	// Server-side content the transcoder admits on the wire but never
	// forwards: the convert layer drops or rejects these under the
	// anthropic_server_tools loss key (a chat upstream executes no server
	// tools). mcp_tool_use / mcp_tool_result are client-side tools that
	// map 1:1 onto tool_use / tool_result; the remaining spellings are
	// server-executed or server references with no chat expression.
	ContentBlockTypeServerToolUse       ContentBlockType = "server_tool_use"
	ContentBlockTypeWebSearchToolResult ContentBlockType = "web_search_tool_result"
	ContentBlockTypeMCPToolUse          ContentBlockType = "mcp_tool_use"
	ContentBlockTypeMCPToolResult       ContentBlockType = "mcp_tool_result"
	ContentBlockTypeCodeExecution       ContentBlockType = "code_execution"
	ContentBlockTypeCodeExecutionResult ContentBlockType = "code_execution_tool_result"
	ContentBlockTypeContainerUpload     ContentBlockType = "container_upload"

	// Mid-conversation tool-change blocks (inline-tools-2026-09-15 /
	// mid-conversation-tool-changes-2026-07-01). They are content blocks in
	// the content array of a role:"system" message and change which tools the
	// model is offered from that point in the conversation onward, without
	// editing the tools array (so the cached prefix survives).
	ContentBlockTypeToolAddition ContentBlockType = "tool_addition"
	ContentBlockTypeToolRemoval  ContentBlockType = "tool_removal"
	// ContentBlockTypeMCPToolListing is a RESPONSE block (mcp-client-2026-09-15)
	// recording a server's fetched tool list, which pins that list when the
	// client sends the message back unchanged.
	ContentBlockTypeMCPToolListing ContentBlockType = "mcp_tool_listing"
)

// ToolChangeType is the discriminant of a tool-change block's `tool` payload.
type ToolChangeType string

// ToolChangeType values: the reference arms name a tool declared in the
// request's tools array, and tool_definition (tool_addition only, under the
// inline-tools beta) carries a full tools entry instead.
const (
	ToolChangeTypeReference           ToolChangeType = "tool_reference"
	ToolChangeTypeMCPReference        ToolChangeType = "mcp_tool_reference"
	ToolChangeTypeMCPToolsetReference ToolChangeType = "mcp_toolset_reference"
	ToolChangeTypeDefinition          ToolChangeType = "tool_definition"
)

// ToolChange is the `tool` payload of a tool_addition or tool_removal block.
// The reference arms name a tool the request already declared, so the arm's
// own fields are strict: an unknown field, a missing name, or a member from
// another arm is a malformed block rather than a silently discarded field.
type ToolChange struct {
	Type ToolChangeType
	// Name names the tool for tool_reference, and the member tool for
	// mcp_tool_reference.
	Name string
	// ServerName names the MCP server for mcp_tool_reference and
	// mcp_toolset_reference.
	ServerName string
	// Definition is the raw tools entry carried by tool_definition: a custom
	// tool, an Anthropic-defined client/server tool, or an mcp_toolset. It is
	// preserved byte-exact so the caller can decode it against whichever
	// tool schema it needs, never a generic envelope.
	Definition json.RawMessage
}

// UnmarshalJSON decodes the arm the discriminant names, rejecting both an
// unknown arm and a member that belongs to a different one.
func (t *ToolChange) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type ToolChangeType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	var change ToolChange
	switch probe.Type {
	case ToolChangeTypeReference:
		var shadow struct {
			Type ToolChangeType `json:"type"`
			Name string         `json:"name"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("tool_reference: %w", err)
		}
		change.Type = shadow.Type
		change.Name = shadow.Name

	case ToolChangeTypeMCPReference:
		var shadow struct {
			Type       ToolChangeType `json:"type"`
			ServerName string         `json:"server_name"`
			Name       string         `json:"name"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("mcp_tool_reference: %w", err)
		}
		change.Type = shadow.Type
		change.ServerName = shadow.ServerName
		change.Name = shadow.Name

	case ToolChangeTypeMCPToolsetReference:
		var shadow struct {
			Type       ToolChangeType `json:"type"`
			ServerName string         `json:"server_name"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("mcp_toolset_reference: %w", err)
		}
		change.Type = shadow.Type
		change.ServerName = shadow.ServerName

	case ToolChangeTypeDefinition:
		var shadow struct {
			Type       ToolChangeType  `json:"type"`
			Definition json.RawMessage `json:"definition"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("tool_definition: %w", err)
		}
		change.Type = shadow.Type
		change.Definition = shadow.Definition

	default:
		return fmt.Errorf("unknown tool change type %q", probe.Type)
	}
	*t = change
	return t.Validate()
}

// MarshalJSON emits the arm's own fields. Validate first so a hand-built
// value cannot silently discard fields belonging to another arm.
func (t ToolChange) MarshalJSON() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	switch t.Type {
	case ToolChangeTypeReference:
		return json.Marshal(struct {
			Type ToolChangeType `json:"type"`
			Name string         `json:"name"`
		}{t.Type, t.Name})
	case ToolChangeTypeMCPReference:
		return json.Marshal(struct {
			Type       ToolChangeType `json:"type"`
			ServerName string         `json:"server_name"`
			Name       string         `json:"name"`
		}{t.Type, t.ServerName, t.Name})
	case ToolChangeTypeMCPToolsetReference:
		return json.Marshal(struct {
			Type       ToolChangeType `json:"type"`
			ServerName string         `json:"server_name"`
		}{t.Type, t.ServerName})
	case ToolChangeTypeDefinition:
		return json.Marshal(struct {
			Type       ToolChangeType  `json:"type"`
			Definition json.RawMessage `json:"definition"`
		}{t.Type, t.Definition})
	default:
		return nil, fmt.Errorf("unknown tool change type %q", t.Type)
	}
}

// Validate checks the arm's required members and that a definition is exactly
// one JSON object. A reference arm requires the name it references, because a
// nameless reference can never resolve.
func (t ToolChange) Validate() error {
	switch t.Type {
	case ToolChangeTypeReference:
		if t.ServerName != "" || t.Definition != nil {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "tool_reference cannot carry server_name or definition"}
		}
		if t.Name == "" {
			return errors.New("tool_reference has no name")
		}
	case ToolChangeTypeMCPReference:
		if t.Definition != nil {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "mcp_tool_reference cannot carry definition"}
		}
		if t.ServerName == "" || t.Name == "" {
			return errors.New("mcp_tool_reference requires server_name and name")
		}
	case ToolChangeTypeMCPToolsetReference:
		if t.Name != "" || t.Definition != nil {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "mcp_toolset_reference cannot carry name or definition"}
		}
		if t.ServerName == "" {
			return errors.New("mcp_toolset_reference has no server_name")
		}
	case ToolChangeTypeDefinition:
		if t.Name != "" || t.ServerName != "" {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "tool_definition cannot carry name or server_name outside definition"}
		}
		if err := validateJSONObject(t.Definition); err != nil {
			return fmt.Errorf("tool_definition: %w", err)
		}
	default:
		return fmt.Errorf("unknown tool change type %q", t.Type)
	}
	return nil
}

// validateJSONObject requires raw to be exactly one JSON object.
func validateJSONObject(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("value is empty")
	}
	if trimmed[0] != '{' {
		return errors.New("value is not a JSON object")
	}
	var probe map[string]json.RawMessage
	if err := wire.Decode(trimmed, &probe); err != nil {
		return err
	}
	return nil
}

// validateMCPToolListing checks the listing's content without replacing its
// raw bytes. The response and replay request share the same three tool fields;
// description may be null in the replay request.
func validateMCPToolListing(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("tools is empty")
	}
	if trimmed[0] != '[' {
		return errors.New("tools is not a JSON array")
	}
	var tools []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
		Description *string         `json:"description,omitempty"`
	}
	// A tool listing is provider state, not client-authored conversation
	// content: the convert layer drops it on both the response and replay
	// paths. The modeled fields are checked below (name, input_schema), and
	// DecodeTolerant still rejects duplicate keys, illegal nulls, malformed
	// syntax and type-corrupt modeled fields. Only unmodeled provider
	// extensions are skipped, so they cannot fail an exchange whose listing
	// carries nothing client-visible.
	if err := wire.DecodeTolerant(trimmed, &tools); err != nil {
		return err
	}
	for i, tool := range tools {
		if tool.Name == "" {
			return fmt.Errorf("tools[%d] has no name", i)
		}
		if err := validateJSONObject(tool.InputSchema); err != nil {
			return fmt.Errorf("tools[%d].input_schema: %w", i, err)
		}
	}
	return nil
}

// SourceType is the type of an image or document source.
type SourceType string

// SourceType values.
const (
	SourceTypeBase64 SourceType = "base64"
	SourceTypeURL    SourceType = "url"
)

// Source is the source of an image or document block.
type Source struct {
	Type      SourceType `json:"type"`
	MediaType string     `json:"media_type"`
	Data      string     `json:"data,omitempty"`
	URL       string     `json:"url,omitempty"`
}

// Validate checks the source shape. media_type is required for base64
// sources; URL sources may omit it (the API derives it, e.g. application/pdf
// for documents), matching the official SDKs' URL source params.
func (s Source) Validate() error {
	switch s.Type {
	case SourceTypeBase64:
		if s.Data == "" {
			return errors.New("base64 source has no data")
		}
		if s.MediaType == "" {
			return errors.New("base64 source has no media_type")
		}
		// The source union is exclusive: a base64 source must not also
		// carry a url, and vice versa — an ambiguous source is rejected
		// instead of silently preferring one arm.
		if s.URL != "" {
			return errors.New("base64 source must not carry a url")
		}
	case SourceTypeURL:
		if s.URL == "" {
			return errors.New("url source has no url")
		}
		if s.Data != "" {
			return errors.New("url source must not carry base64 data")
		}
	default:
		return fmt.Errorf("unknown anthropic source type %q", s.Type)
	}
	return nil
}

// TextCitationType is the type of an Anthropic text citation.
type TextCitationType string

// TextCitationType values.
const (
	CitationTypeCharLocation            TextCitationType = "char_location"
	CitationTypePageLocation            TextCitationType = "page_location"
	CitationTypeContentBlockLocation    TextCitationType = "content_block_location"
	CitationTypeWebSearchResultLocation TextCitationType = "web_search_result_location"
	CitationTypeSearchResultLocation    TextCitationType = "search_result_location"
)

// TextCitation represents an Anthropic text citation.
type TextCitation struct {
	Type TextCitationType `json:"type"`

	// Common field for all citations:
	CitedText string `json:"cited_text"`

	// char_location, page_location, content_block_location:
	DocumentIndex *int    `json:"document_index,omitempty"`
	DocumentTitle *string `json:"document_title,omitempty"`
	FileID        *string `json:"file_id,omitempty"`

	// char_location:
	StartCharIndex *int `json:"start_char_index,omitempty"`
	EndCharIndex   *int `json:"end_char_index,omitempty"`

	// page_location:
	StartPageNumber *int `json:"start_page_number,omitempty"`
	EndPageNumber   *int `json:"end_page_number,omitempty"`

	// content_block_location:
	StartBlockIndex *int `json:"start_block_index,omitempty"`
	EndBlockIndex   *int `json:"end_block_index,omitempty"`

	// web_search_result_location:
	URL            *string `json:"url,omitempty"`
	Title          *string `json:"title,omitempty"`
	EncryptedIndex *string `json:"encrypted_index,omitempty"`

	// search_result_location:
	SearchResultIndex *int    `json:"search_result_index,omitempty"`
	Source            *string `json:"source,omitempty"`
}

// Validate checks citation shape and bounds.
func (c TextCitation) Validate() error {
	switch c.Type {
	case CitationTypeCharLocation:
		if c.DocumentIndex == nil {
			return errors.New("char_location citation has no document_index")
		}
		if c.StartCharIndex == nil {
			return errors.New("char_location citation has no start_char_index")
		}
		if c.EndCharIndex == nil {
			return errors.New("char_location citation has no end_char_index")
		}
		if *c.StartCharIndex < 0 || *c.EndCharIndex < *c.StartCharIndex {
			return errors.New("char_location citation has invalid char indices")
		}
	case CitationTypePageLocation:
		if c.DocumentIndex == nil {
			return errors.New("page_location citation has no document_index")
		}
		if c.StartPageNumber == nil {
			return errors.New("page_location citation has no start_page_number")
		}
		if c.EndPageNumber == nil {
			return errors.New("page_location citation has no end_page_number")
		}
		if *c.StartPageNumber < 0 || *c.EndPageNumber < *c.StartPageNumber {
			return errors.New("page_location citation has invalid page numbers")
		}
	case CitationTypeContentBlockLocation:
		if c.DocumentIndex == nil {
			return errors.New("content_block_location citation has no document_index")
		}
		if c.StartBlockIndex == nil {
			return errors.New("content_block_location citation has no start_block_index")
		}
		if c.EndBlockIndex == nil {
			return errors.New("content_block_location citation has no end_block_index")
		}
		if *c.StartBlockIndex < 0 || *c.EndBlockIndex < *c.StartBlockIndex {
			return errors.New("content_block_location citation has invalid block indices")
		}
	case CitationTypeWebSearchResultLocation:
		if c.URL == nil || *c.URL == "" {
			return errors.New("web_search_result_location citation has no url")
		}
	case CitationTypeSearchResultLocation:
		if c.SearchResultIndex == nil {
			return errors.New("search_result_location citation has no search_result_index")
		}
		if c.Source == nil || *c.Source == "" {
			return errors.New("search_result_location citation has no source")
		}
		if c.StartBlockIndex == nil {
			return errors.New("search_result_location citation has no start_block_index")
		}
		if c.EndBlockIndex == nil {
			return errors.New("search_result_location citation has no end_block_index")
		}
		if *c.StartBlockIndex < 0 || *c.EndBlockIndex < *c.StartBlockIndex {
			return errors.New("search_result_location citation has invalid block indices")
		}
	default:
		return fmt.Errorf("unknown anthropic citation type %q", c.Type)
	}
	return nil
}

// UnmarshalJSON decodes the citation tagged union per-arm with wire.Decode.
func (c *TextCitation) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type TextCitationType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}

	var cit TextCitation
	switch probe.Type {
	case CitationTypeCharLocation:
		var shadow struct {
			Type           TextCitationType `json:"type"`
			CitedText      *string          `json:"cited_text"`
			DocumentIndex  *int             `json:"document_index"`
			DocumentTitle  *string          `json:"document_title,omitempty"`
			StartCharIndex *int             `json:"start_char_index"`
			EndCharIndex   *int             `json:"end_char_index"`
			FileID         *string          `json:"file_id,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("char_location citation: %w", err)
		}
		if shadow.CitedText == nil {
			return errors.New("char_location citation has no cited_text")
		}
		cit = TextCitation{
			Type:           shadow.Type,
			CitedText:      *shadow.CitedText,
			DocumentIndex:  shadow.DocumentIndex,
			DocumentTitle:  shadow.DocumentTitle,
			StartCharIndex: shadow.StartCharIndex,
			EndCharIndex:   shadow.EndCharIndex,
			FileID:         shadow.FileID,
		}

	case CitationTypePageLocation:
		var shadow struct {
			Type            TextCitationType `json:"type"`
			CitedText       *string          `json:"cited_text"`
			DocumentIndex   *int             `json:"document_index"`
			DocumentTitle   *string          `json:"document_title,omitempty"`
			StartPageNumber *int             `json:"start_page_number"`
			EndPageNumber   *int             `json:"end_page_number"`
			FileID          *string          `json:"file_id,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("page_location citation: %w", err)
		}
		if shadow.CitedText == nil {
			return errors.New("page_location citation has no cited_text")
		}
		cit = TextCitation{
			Type:            shadow.Type,
			CitedText:       *shadow.CitedText,
			DocumentIndex:   shadow.DocumentIndex,
			DocumentTitle:   shadow.DocumentTitle,
			StartPageNumber: shadow.StartPageNumber,
			EndPageNumber:   shadow.EndPageNumber,
			FileID:          shadow.FileID,
		}

	case CitationTypeContentBlockLocation:
		var shadow struct {
			Type            TextCitationType `json:"type"`
			CitedText       *string          `json:"cited_text"`
			DocumentIndex   *int             `json:"document_index"`
			DocumentTitle   *string          `json:"document_title,omitempty"`
			StartBlockIndex *int             `json:"start_block_index"`
			EndBlockIndex   *int             `json:"end_block_index"`
			FileID          *string          `json:"file_id,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("content_block_location citation: %w", err)
		}
		if shadow.CitedText == nil {
			return errors.New("content_block_location citation has no cited_text")
		}
		cit = TextCitation{
			Type:            shadow.Type,
			CitedText:       *shadow.CitedText,
			DocumentIndex:   shadow.DocumentIndex,
			DocumentTitle:   shadow.DocumentTitle,
			StartBlockIndex: shadow.StartBlockIndex,
			EndBlockIndex:   shadow.EndBlockIndex,
			FileID:          shadow.FileID,
		}

	case CitationTypeWebSearchResultLocation:
		var shadow struct {
			Type           TextCitationType `json:"type"`
			CitedText      *string          `json:"cited_text"`
			URL            *string          `json:"url"`
			Title          *string          `json:"title,omitempty"`
			EncryptedIndex *string          `json:"encrypted_index,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("web_search_result_location citation: %w", err)
		}
		if shadow.CitedText == nil {
			return errors.New("web_search_result_location citation has no cited_text")
		}
		cit = TextCitation{
			Type:           shadow.Type,
			CitedText:      *shadow.CitedText,
			URL:            shadow.URL,
			Title:          shadow.Title,
			EncryptedIndex: shadow.EncryptedIndex,
		}

	case CitationTypeSearchResultLocation:
		var shadow struct {
			Type              TextCitationType `json:"type"`
			CitedText         *string          `json:"cited_text"`
			SearchResultIndex *int             `json:"search_result_index"`
			Title             *string          `json:"title,omitempty"`
			Source            *string          `json:"source"`
			StartBlockIndex   *int             `json:"start_block_index"`
			EndBlockIndex     *int             `json:"end_block_index"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("search_result_location citation: %w", err)
		}
		if shadow.CitedText == nil {
			return errors.New("search_result_location citation has no cited_text")
		}
		cit = TextCitation{
			Type:              shadow.Type,
			CitedText:         *shadow.CitedText,
			SearchResultIndex: shadow.SearchResultIndex,
			Title:             shadow.Title,
			Source:            shadow.Source,
			StartBlockIndex:   shadow.StartBlockIndex,
			EndBlockIndex:     shadow.EndBlockIndex,
		}

	default:
		return fmt.Errorf("unknown anthropic citation type %q", probe.Type)
	}

	*c = cit
	return c.Validate()
}

// ContentBlock is one content block of an Anthropic message.
type ContentBlock struct {
	Type      ContentBlockType `json:"type"`
	Text      *string          `json:"text,omitempty"`
	Thinking  *string          `json:"thinking,omitempty"`
	Signature *string          `json:"signature,omitempty"`
	Data      *string          `json:"data,omitempty"` // redacted_thinking payload

	ToolUseID *string         `json:"tool_use_id,omitempty"`
	ID        *string         `json:"id,omitempty"`
	Name      *string         `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   *Content        `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
	Source    *Source         `json:"source,omitempty"`

	// Citations on text blocks:
	Citations []TextCitation `json:"citations,omitempty"`

	// CacheControl is the Anthropic prompt-cache marker (e.g.
	// {"type":"ephemeral"}) real clients (Claude Code) attach to text,
	// image, tool_use, and tool_result blocks. It is a caching performance
	// hint with no semantic content; chat upstreams cache automatically,
	// so it is accepted on the wire and not forwarded — the decode path
	// reports the drop observably (one deduped anthropic_controls note per
	// exchange).
	CacheControl any `json:"cache_control,omitempty"`

	// ServerContent carries the raw JSON of an admitted-but-unforwardable
	// server-side block (server_tool_use, web_search_tool_result,
	// code_execution results, container_upload). The convert layer decides
	// its fate under the anthropic_server_tools loss key; the wire layer
	// never interprets it.
	ServerContent json.RawMessage `json:"-"`

	// ServerName is the MCP server that owns a mcp_tool_use block. The
	// contract requires it on every such block; it is decoded so a real
	// replayed conversation decodes instead of failing as unknown field.
	ServerName *string `json:"server_name,omitempty"`
	// MCPServerName is the server identified by an mcp_tool_listing. Its wire
	// spelling differs from an mcp_tool_use's server_name and must remain
	// distinct when a listing is marshaled for replay.
	MCPServerName *string `json:"mcp_server_name,omitempty"`

	// Tool is the `tool` payload of a tool_addition / tool_removal block, and
	// ToolChange is the discriminator of that payload (nil on every other
	// block type). ServerName is not reused for it: an MCP server named by a
	// tool-change reference is a different semantic from the server that owns
	// an mcp_tool_use, and conflating them would let one overwrite the other.
	Tool *ToolChange `json:"tool,omitempty"`

	// Tools is the fetched tool list of a response-side mcp_tool_listing
	// block, preserved as its raw JSON array so the client can be handed back
	// exactly what the server recorded.
	Tools json.RawMessage `json:"tools,omitempty"`
}

// UnmarshalJSON decodes the tagged union per-arm: each type admits exactly
// its own fields with DisallowUnknownFields, so a block carrying another
// arm's fields is rejected at decode instead of having those fields silently
// discarded. The public struct keeps its broad shape for rendering; a
// decode-accepted block always validates. The type probe is a lenient
// unmarshal — the arm decode applies the strictness.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type ContentBlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}

	var block ContentBlock
	switch probe.Type {
	case ContentBlockTypeText:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			Text         *string          `json:"text"`
			Citations    []TextCitation   `json:"citations,omitempty"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("text block: %w", err)
		}
		block.Type = shadow.Type
		block.Text = shadow.Text
		block.Citations = shadow.Citations
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeImage, ContentBlockTypeDocument:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			Source       *Source          `json:"source"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("%s block: %w", probe.Type, err)
		}
		block.Type = shadow.Type
		block.Source = shadow.Source
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeToolUse:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			ID           *string          `json:"id"`
			Name         *string          `json:"name"`
			Input        json.RawMessage  `json:"input"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("tool_use block: %w", err)
		}
		block.Type = shadow.Type
		block.ID = shadow.ID
		block.Name = shadow.Name
		block.Input = shadow.Input
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeToolResult:
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			ToolUseID    *string          `json:"tool_use_id"`
			Content      *Content         `json:"content"`
			IsError      *bool            `json:"is_error"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("tool_result block: %w", err)
		}
		block.Type = shadow.Type
		block.ToolUseID = shadow.ToolUseID
		block.Content = shadow.Content
		block.IsError = shadow.IsError
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeThinking:
		// cache_control is intentionally NOT admitted on this arm (and the
		// redacted_thinking arm below), unlike text/image/document/tool_use/
		// tool_result: the official prompt-caching contract states "Thinking
		// blocks cannot be cached directly with cache_control" (Anthropic,
		// Build with Claude → Prompt caching, "What cannot be cached"), so a
		// block carrying it is a contract violation on the client side and
		// the typed unknown-field rejection below is the correct strict-side
		// behavior — pinned by TestThinkingBlockCacheControlRejected and
		// verified against the vendor's prompt-caching documentation.
		var shadow struct {
			Type      ContentBlockType `json:"type"`
			Thinking  *string          `json:"thinking"`
			Signature *string          `json:"signature"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("thinking block: %w", err)
		}
		block.Type = shadow.Type
		block.Thinking = shadow.Thinking
		block.Signature = shadow.Signature

	case ContentBlockTypeRedactedThinking:
		// Same contract exclusion as the thinking arm above:
		// thinking-family blocks are never directly cacheable, so
		// cache_control is rejected here too.
		var shadow struct {
			Type ContentBlockType `json:"type"`
			Data *string          `json:"data"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("redacted_thinking block: %w", err)
		}
		block.Type = shadow.Type
		block.Data = shadow.Data

	case ContentBlockTypeServerToolUse,
		ContentBlockTypeWebSearchToolResult,
		ContentBlockTypeCodeExecution,
		ContentBlockTypeCodeExecutionResult,
		ContentBlockTypeContainerUpload:
		// Server-executed content (or a server-side reference, for
		// container_upload) a chat upstream cannot express. Admit the
		// block with its raw bytes preserved; the convert layer drops or
		// rejects it under the anthropic_server_tools loss key. The
		// envelope shape is deliberately minimal (type only): server
		// spellings vary by tool (id/name/input on server_tool_use,
		// tool_use_id/content on results, file references on uploads),
		// and any per-arm field modeling would only widen the surface
		// that must stay unforwarded. wire.DecodeTolerant skips the
		// unmodeled fields instead of rejecting them.
		var shadow struct {
			Type ContentBlockType `json:"type"`
		}
		if err := wire.DecodeTolerant(data, &shadow); err != nil {
			return fmt.Errorf("%s block: %w", probe.Type, err)
		}
		block.Type = shadow.Type
		block.ServerContent = append(block.ServerContent[:0], data...)

	case ContentBlockTypeMCPToolUse:
		// MCP tools are client-side tools under a server spelling: decode
		// exactly the tool_use fields so the convert layer can map 1:1.
		//
		// server_name is REQUIRED by the contract on every mcp_tool_use and
		// names the MCP server that owns the tool. It is decoded and kept on
		// the block: omitting it from this STRICT shadow made the only shape
		// that occurs on the wire an unknown-field error, failing any
		// replayed conversation that contained a real mcp_tool_use.
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			ID           *string          `json:"id"`
			ServerName   *string          `json:"server_name"`
			Name         *string          `json:"name"`
			Input        json.RawMessage  `json:"input"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("mcp_tool_use block: %w", err)
		}
		block.Type = shadow.Type
		block.ID = shadow.ID
		block.ServerName = shadow.ServerName
		block.Name = shadow.Name
		block.Input = shadow.Input
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeMCPToolResult:
		// Same as above, mirroring the tool_result fields.
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			ToolUseID    *string          `json:"tool_use_id"`
			Content      *Content         `json:"content"`
			IsError      *bool            `json:"is_error"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("mcp_tool_result block: %w", err)
		}
		block.Type = shadow.Type
		block.ToolUseID = shadow.ToolUseID
		block.Content = shadow.Content
		block.IsError = shadow.IsError
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeToolAddition, ContentBlockTypeToolRemoval:
		// The mid-conversation tool-change blocks. `tool` is a discriminated
		// payload whose arms the ToolChange decoder validates strictly, so an
		// unknown arm or a member from another arm is malformed rather than a
		// silently dropped field. cache_control is admitted on the BLOCK (the
		// contract allows it on the block or in the definition, never both);
		// the convert layer decides whether it can be honoured.
		var shadow struct {
			Type         ContentBlockType `json:"type"`
			Tool         *ToolChange      `json:"tool"`
			CacheControl any              `json:"cache_control,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("%s block: %w", probe.Type, err)
		}
		block.Type = shadow.Type
		block.Tool = shadow.Tool
		block.CacheControl = shadow.CacheControl

	case ContentBlockTypeMCPToolListing:
		// Response-side: the server's fetched tool list, pinned by sending the
		// message back unchanged. mcp_server_name is required so the listing
		// can be attributed to its server.
		var shadow struct {
			Type          ContentBlockType `json:"type"`
			MCPServerName *string          `json:"mcp_server_name"`
			Tools         json.RawMessage  `json:"tools"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("mcp_tool_listing block: %w", err)
		}
		block.Type = shadow.Type
		block.MCPServerName = shadow.MCPServerName
		block.Tools = shadow.Tools

	default:
		return fmt.Errorf("unknown anthropic content block type %q", probe.Type)
	}

	*b = block
	return b.Validate()
}

// Validate checks the block shape.
func (b ContentBlock) Validate() error {
	switch b.Type {
	case ContentBlockTypeText:
		if b.Text == nil {
			return errors.New("text block has no text")
		}
		for i, cit := range b.Citations {
			if err := cit.Validate(); err != nil {
				return fmt.Errorf("citation %d: %w", i, err)
			}
		}
	case ContentBlockTypeImage:
		if b.Source == nil {
			return errors.New("image block has no source")
		}
		return b.Source.Validate()
	case ContentBlockTypeDocument:
		if b.Source == nil {
			return errors.New("document block has no source")
		}
		return b.Source.Validate()
	case ContentBlockTypeToolUse:
		if b.ID == nil || *b.ID == "" {
			return errors.New("tool_use block has no id")
		}
		if b.Name == nil || *b.Name == "" {
			return errors.New("tool_use block has no name")
		}
		if len(b.Input) == 0 {
			return errors.New("tool_use block has no input")
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b.Input, &probe); err != nil {
			return errors.New("tool_use input is not a JSON object")
		}
	case ContentBlockTypeToolResult:
		if b.ToolUseID == nil || *b.ToolUseID == "" {
			return errors.New("tool_result block has no tool_use_id")
		}
		if b.Content == nil {
			return errors.New("tool_result block has no content")
		}
	case ContentBlockTypeThinking:
		if b.Thinking == nil || b.Signature == nil {
			return errors.New("thinking block requires thinking and signature")
		}
	case ContentBlockTypeRedactedThinking:
		if b.Data == nil {
			return errors.New("redacted_thinking block has no data")
		}
	case ContentBlockTypeServerToolUse,
		ContentBlockTypeWebSearchToolResult,
		ContentBlockTypeCodeExecution,
		ContentBlockTypeCodeExecutionResult,
		ContentBlockTypeContainerUpload:
		if len(b.ServerContent) == 0 {
			return fmt.Errorf("%s block has no preserved content", b.Type)
		}
	case ContentBlockTypeMCPToolUse:
		if b.ID == nil || *b.ID == "" {
			return errors.New("mcp_tool_use block has no id")
		}
		if b.Name == nil || *b.Name == "" {
			return errors.New("mcp_tool_use block has no name")
		}
		if len(b.Input) == 0 {
			return errors.New("mcp_tool_use block has no input")
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b.Input, &probe); err != nil {
			return errors.New("mcp_tool_use input is not a JSON object")
		}
	case ContentBlockTypeMCPToolResult:
		if b.ToolUseID == nil || *b.ToolUseID == "" {
			return errors.New("mcp_tool_result block has no tool_use_id")
		}
		if b.Content == nil {
			return errors.New("mcp_tool_result block has no content")
		}
	case ContentBlockTypeToolAddition, ContentBlockTypeToolRemoval:
		if b.Tool == nil {
			return fmt.Errorf("%s block has no tool", b.Type)
		}
		// tool_definition is the inline-tools-2026-09-15 arm and is only legal
		// on an addition: a removal always names a tool by reference.
		if b.Type == ContentBlockTypeToolRemoval &&
			b.Tool.Type == ToolChangeTypeDefinition {
			return errors.New(
				"tool_removal cannot carry tool_definition (a removal names a tool by reference)",
			)
		}
		return b.Tool.Validate()
	case ContentBlockTypeMCPToolListing:
		if b.MCPServerName == nil || *b.MCPServerName == "" {
			return errors.New("mcp_tool_listing block has no mcp_server_name")
		}
		if b.ServerName != nil {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "mcp_tool_listing cannot carry server_name"}
		}
		if err := validateMCPToolListing(b.Tools); err != nil {
			return fmt.Errorf("mcp_tool_listing: %w", err)
		}
	default:
		return fmt.Errorf("unknown anthropic content block type %q", b.Type)
	}
	return nil
}

// Content is the content of an Anthropic message: either a plain string or
// an array of content blocks. Exactly one form is set.
type Content struct {
	ContentStr    *string
	ContentBlocks []ContentBlock
}

// Validate checks the union invariants and each block.
func (c Content) Validate() error {
	if c.ContentStr != nil && c.ContentBlocks != nil {
		return errors.New("anthropic content has both string and block variants")
	}
	if c.ContentStr == nil && c.ContentBlocks == nil {
		return errors.New("anthropic content has no selected variant")
	}
	for i, block := range c.ContentBlocks {
		if err := block.Validate(); err != nil {
			return fmt.Errorf("anthropic content block %d: %w", i, err)
		}
	}
	return nil
}

// MarshalJSON emits the active form, without a wrapper object.
func (c Content) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.ContentStr != nil {
		return json.Marshal(*c.ContentStr)
	}
	return json.Marshal(c.ContentBlocks)
}

// UnmarshalJSON decodes a string or an array of content blocks, rejecting
// invalid blocks at decode time so a decode-accepted value always validates.
func (c *Content) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return errors.New("anthropic content is null")
	}
	if data[0] == '"' {
		var str string
		if err := wire.Decode(data, &str); err != nil {
			return err
		}
		c.ContentStr = &str
		c.ContentBlocks = nil
		return c.Validate()
	}
	var blocks []ContentBlock
	if err := wire.Decode(data, &blocks); err != nil {
		return err
	}
	c.ContentBlocks = blocks
	c.ContentStr = nil
	return c.Validate()
}

// Message is a message in an Anthropic conversation.
type Message struct {
	Role    Role    `json:"role"`
	Content Content `json:"content"`
}

// Validate checks the message shape.
func (m Message) Validate() error {
	switch m.Role {
	case RoleUser, RoleAssistant, RoleSystem:
	default:
		return fmt.Errorf("unknown anthropic message role %q", m.Role)
	}
	return m.Content.Validate()
}

// Tool is a tool definition in an Anthropic request. InputSchema is the raw
// schema JSON: it is validated as exactly one JSON object and retained as raw
// bytes, so numbers are never decoded and remarshaled through a floating-point
// map. JSON rendering may normalize insignificant whitespace and escapes.
//
// Server-side tools (type-discriminated definitions such as
// web_search_20250305) are admitted on the wire with their raw bytes
// preserved; the convert layer drops or rejects them under the
// anthropic_server_tools loss key. A chat upstream executes no server
// tools, so they are never forwarded.
type Tool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description *string         `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// An mcp_toolset is identified by its server rather than a tool name.
	MCPServerName string `json:"mcp_server_name,omitempty"`

	// ServerParams carries the raw JSON of a server-side tool definition
	// (everything besides type/name/description/input_schema, e.g.
	// max_uses). Preserved for the loss report; never forwarded.
	ServerParams json.RawMessage `json:"-"`

	// CacheControl is the Anthropic prompt-cache marker real clients
	// (Claude Code) attach to tool definitions; a caching performance hint
	// accepted on the wire and not forwarded — the decode path reports the
	// drop observably (one deduped anthropic_controls note per exchange).
	CacheControl any `json:"cache_control,omitempty"`

	// DeferLoading is the Anthropic tool-search hint (Claude Code sets it on
	// MCP tools it would load on demand). Deferral needs the Anthropic
	// server-side tool search, which no transcoded target has, so the tool is
	// rendered eagerly and the decode path records one deduped note.
	DeferLoading *bool `json:"defer_loading,omitempty"`
}

// IsClientFunction recognizes the official optional "custom" discriminator
// and the "function" compatibility spelling. All three spellings take the
// same strict decode and validation path.
func (t Tool) IsClientFunction() bool {
	return t.Type == "" || t.Type == "custom" || t.Type == "function"
}

// UnmarshalJSON admits client functions strictly and other tool types with
// their raw bytes preserved for the convert layer's unsupported-feature
// decision. A type discriminator alone does not identify a server tool:
// "custom" is the official client-function spelling.
func (t *Tool) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	// The SDK's optional "custom" discriminator is nullable; explicit null is
	// the untagged client arm. Presence-aware probing keeps a null type from
	// falling into the tolerant server-tool branch.
	typeName := ""
	if probe.Type != nil {
		typeName = *probe.Type
	}
	if (Tool{Type: typeName}).IsClientFunction() {
		var function struct {
			Type         *string         `json:"type"`
			Name         string          `json:"name"`
			Description  *string         `json:"description,omitempty"`
			InputSchema  json.RawMessage `json:"input_schema,omitempty"`
			CacheControl any             `json:"cache_control,omitempty"`
			DeferLoading *bool           `json:"defer_loading,omitempty"`
		}
		if err := wire.Decode(data, &function); err != nil {
			return err
		}
		*t = Tool{
			Name:         function.Name,
			Description:  function.Description,
			InputSchema:  function.InputSchema,
			CacheControl: function.CacheControl,
			DeferLoading: function.DeferLoading,
		}
		if function.Type != nil {
			t.Type = *function.Type
		}
		return t.Validate()
	}
	var server struct {
		Type          string          `json:"type"`
		Name          string          `json:"name"`
		MCPServerName string          `json:"mcp_server_name,omitempty"`
		Description   *string         `json:"description,omitempty"`
		InputSchema   json.RawMessage `json:"input_schema,omitempty"`
		CacheControl  any             `json:"cache_control,omitempty"`
		// MaxUses and every other server-side param ride inside
		// ServerParams below; only the envelope fields the convert layer
		// needs for the loss report are modeled. wire.Decode would reject
		// the unmodeled params, so the params are captured with a
		// tolerant decode instead.
		MaxUses *int `json:"max_uses,omitempty"`
	}
	if err := wire.DecodeTolerant(data, &server); err != nil {
		return fmt.Errorf("server tool definition: %w", err)
	}
	*t = Tool{
		Type:          server.Type,
		Name:          server.Name,
		MCPServerName: server.MCPServerName,
		Description:   server.Description,
		InputSchema:   server.InputSchema,
		ServerParams:  append(t.ServerParams[:0], data...),
		CacheControl:  server.CacheControl,
	}
	return t.Validate()
}

// Validate checks the tool shape.
func (t Tool) Validate() error {
	if t.Type == "mcp_toolset" {
		if t.MCPServerName == "" {
			return &wire.DecodeError{Kind: wire.DecodeMissingRequired, Path: "mcp_server_name", Message: "mcp_toolset has no mcp_server_name"}
		}
		if t.Name != "" {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "mcp_toolset cannot carry name"}
		}
		return nil
	}
	if t.Name == "" {
		return &wire.DecodeError{Kind: wire.DecodeMissingRequired, Path: "name", Message: "anthropic tool name is empty"}
	}
	if !t.IsClientFunction() {
		// A server-side definition needs only its identity: the params
		// are never forwarded, so presence-checking them would reject
		// shapes the convert layer is about to drop-or-reject anyway.
		return nil
	}
	if len(bytes.TrimSpace(t.InputSchema)) == 0 {
		return &wire.DecodeError{Kind: wire.DecodeMissingRequired, Path: "input_schema", Message: "anthropic tool has no input_schema"}
	}
	if bytes.Equal(bytes.TrimSpace(t.InputSchema), []byte("null")) {
		return &wire.DecodeError{Kind: wire.DecodeIllegalNull, Path: "input_schema", Message: "anthropic tool input_schema is null"}
	}
	if t.MCPServerName != "" {
		return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "a client function cannot carry mcp_server_name"}
	}
	if err := validateJSONObject(t.InputSchema); err != nil {
		return fmt.Errorf("input_schema: %w", err)
	}
	return nil
}

// ToolChoice configures tool selection.
type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// StopReason is the reason the model stopped generating.
type StopReason string

// StopReason values.
const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonStopSequence StopReason = "stop_sequence"
	StopReasonToolUse      StopReason = "tool_use"
	StopReasonPauseTurn    StopReason = "pause_turn"
	StopReasonRefusal      StopReason = "refusal"
)

// StopDetails carries the refusal stop details of a response.
type StopDetails struct {
	Type             string  `json:"type"`
	Category         *string `json:"category,omitempty"`
	Explanation      *string `json:"explanation,omitempty"`
	RecommendedModel *string `json:"recommended_model,omitempty"`
}

// OutputTokensDetails breaks down output tokens.
type OutputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

// Usage is the token usage of an Anthropic response. The cache token fields
// are always present on the wire (zero when unused), matching the real API.
type Usage struct {
	InputTokens              int                  `json:"input_tokens"`
	CacheCreationInputTokens int                  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                  `json:"cache_read_input_tokens"`
	OutputTokens             int                  `json:"output_tokens"`
	OutputTokensDetails      *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// Response is an Anthropic messages API response.
//
// StopReason and StopSequence are nullable fields that are ALWAYS present on
// the wire: they serialize as explicit null before generation completes (the
// message_start payload) and carry their real values in message_delta or the
// completed non-stream response. They are pointers WITHOUT omitempty for
// exactly this reason.
type Response struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *StopReason    `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        *Usage         `json:"usage,omitempty"`
	StopDetails  *StopDetails   `json:"stop_details,omitempty"`
}

// Request is an Anthropic messages API request (the pinned subset the
// transcoder maps; unsupported fields are rejected by strict decoding).
type Request struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []Message       `json:"messages"`
	System        *Content        `json:"system,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        *bool           `json:"stream,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`

	// ContextManagement and OutputConfig are the newest Anthropic Messages
	// request controls (sent by Claude Code 2.1.x: context_management with
	// clear_thinking_20251015 edits, output_config with the output budget).
	// They are client-side controls with no target equivalent: accepted on
	// the wire and gated as an observable loss/reject decision
	// (anthropic_controls) by the transcode layer — never silently dropped.
	ContextManagement any `json:"context_management,omitempty"`
	OutputConfig      any `json:"output_config,omitempty"`
}

// ThinkingConfig is the Anthropic Messages thinking request configuration.
// The official contract is:
//
//	{"type": "enabled", "budget_tokens": N, "display": "summarized"|"omitted"|"updates"|null}
//	{"type": "disabled"}
//	{"type": "between_tools"}
//	{"type": "adaptive", "display": "summarized"|"omitted"|"updates"|null}
//
// "adaptive" (used by Claude Code 2.1.x effort picker) delegates the
// thinking decision to the model/server, and "between_tools" (Claude Sonnet
// 5.5) turns off up-front thinking while keeping the progress updates models
// write between tool calls. display works on BOTH the enabled and the
// adaptive arm; its "updates" member is beta-gated by the
// thinking-display-updates-2026-08-18 Anthropic-Beta header (claude-cli 2.x
// sends it), and it is invalid on disabled, which has nothing to display, and
// on between_tools, which accepts no other field. Each arm is decoded through
// its own strict shape, so an illegal member is rejected even when its value
// is null. The transcode layer also validates the canonical representation.
type ThinkingConfig struct {
	Type         string  `json:"type"`
	BudgetTokens *int    `json:"budget_tokens,omitempty"`
	Display      *string `json:"display,omitempty"`
}

// UnmarshalJSON applies the selected arm's field set before decoding into the
// common representation. A broad shadow would turn display:null into absence
// even on disabled and between_tools, where the field itself is illegal.
func (t *ThinkingConfig) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	var config ThinkingConfig
	switch probe.Type {
	case "enabled":
		var shadow struct {
			Type         string  `json:"type"`
			BudgetTokens *int    `json:"budget_tokens"`
			Display      *string `json:"display,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("thinking enabled: %w", err)
		}
		config = ThinkingConfig{Type: shadow.Type, BudgetTokens: shadow.BudgetTokens, Display: shadow.Display}
	case "adaptive":
		var shadow struct {
			Type    string  `json:"type"`
			Display *string `json:"display,omitempty"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("thinking adaptive: %w", err)
		}
		config = ThinkingConfig{Type: shadow.Type, Display: shadow.Display}
	case "disabled", "between_tools":
		var shadow struct {
			Type string `json:"type"`
		}
		if err := wire.Decode(data, &shadow); err != nil {
			return fmt.Errorf("thinking %s: %w", probe.Type, err)
		}
		config.Type = shadow.Type
	default:
		return fmt.Errorf("thinking type %q is invalid (want enabled, disabled, between_tools, or adaptive)", probe.Type)
	}
	if err := config.Validate(); err != nil {
		return err
	}
	*t = config
	return nil
}

// Validate checks the wire union. Unlike the canonical representation, an
// enabled wire arm requires budget_tokens because it is emitted to the API.
func (t ThinkingConfig) Validate() error {
	switch t.Type {
	case "enabled":
		if t.BudgetTokens == nil {
			return errors.New("thinking type enabled requires budget_tokens")
		}
		if *t.BudgetTokens <= 0 {
			return errors.New("thinking budget_tokens must be positive")
		}
	case "adaptive":
		if t.BudgetTokens != nil {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: "thinking adaptive cannot carry budget_tokens"}
		}
	case "disabled", "between_tools":
		if t.BudgetTokens != nil || t.Display != nil {
			return &wire.DecodeError{Kind: wire.DecodeContradictoryUnion, Message: fmt.Sprintf("thinking %s accepts no other field", t.Type)}
		}
	default:
		return fmt.Errorf("thinking type %q is invalid (want enabled, disabled, between_tools, or adaptive)", t.Type)
	}
	if t.Display != nil {
		switch *t.Display {
		case "summarized", "omitted", "updates":
		default:
			return fmt.Errorf("thinking display %q is invalid (want summarized, omitted, or updates)", *t.Display)
		}
	}
	return nil
}

func (t ThinkingConfig) MarshalJSON() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	type shadow ThinkingConfig
	return json.Marshal(shadow(t))
}

// StreamEventType is the type of an Anthropic SSE event.
type StreamEventType string

// StreamEventType values.
const (
	StreamEventTypeMessageStart      StreamEventType = "message_start"
	StreamEventTypeMessageStop       StreamEventType = "message_stop"
	StreamEventTypeContentBlockStart StreamEventType = "content_block_start"
	StreamEventTypeContentBlockDelta StreamEventType = "content_block_delta"
	StreamEventTypeContentBlockStop  StreamEventType = "content_block_stop"
	StreamEventTypeMessageDelta      StreamEventType = "message_delta"
	StreamEventTypePing              StreamEventType = "ping"
	StreamEventTypeError             StreamEventType = "error"
)

// StreamDeltaType is the type of an Anthropic stream delta.
type StreamDeltaType string

// StreamDeltaType values.
const (
	StreamDeltaTypeTextDelta      StreamDeltaType = "text_delta"
	StreamDeltaTypeInputJSONDelta StreamDeltaType = "input_json_delta"
	StreamDeltaTypeThinkingDelta  StreamDeltaType = "thinking_delta"
	StreamDeltaTypeSignatureDelta StreamDeltaType = "signature_delta"
	StreamDeltaTypeCitationsDelta StreamDeltaType = "citations_delta"
)

// StreamDelta is the delta payload of an Anthropic SSE event.
type StreamDelta struct {
	Type         StreamDeltaType `json:"type,omitempty"`
	Text         *string         `json:"text,omitempty"`
	PartialJSON  *string         `json:"partial_json,omitempty"`
	Thinking     *string         `json:"thinking,omitempty"`
	Signature    *string         `json:"signature,omitempty"`
	Citation     *TextCitation   `json:"citation,omitempty"`
	StopReason   *StopReason     `json:"stop_reason,omitempty"`
	StopSequence *string         `json:"stop_sequence,omitempty"`
}

// StreamError is the nested error object of an Anthropic error event. The
// official stream error contract requires it.
type StreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// StreamEvent is one SSE event of an Anthropic messages stream. The tagged
// event shapes share one struct because every field is optional on the wire;
// per-event requiredness is enforced by the consuming stream state machine.
type StreamEvent struct {
	Type         StreamEventType `json:"type"`
	Message      *Response       `json:"message,omitempty"`
	Index        *int            `json:"index,omitempty"`
	ContentBlock *ContentBlock   `json:"content_block,omitempty"`
	Delta        *StreamDelta    `json:"delta,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
	Error        *StreamError    `json:"error,omitempty"`
}
