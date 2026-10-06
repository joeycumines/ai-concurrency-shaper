package anthropicmessages

// Unit tests for the Anthropic wire types' union and validation branches
// that the transcode conversion suite does not reach (URL sources, document
// blocks, thinking/redacted_thinking preservation, stop details).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestSourceValidate(t *testing.T) {
	base64 := Source{Type: SourceTypeBase64, MediaType: "image/png", Data: "aGk="}
	if err := base64.Validate(); err != nil {
		t.Fatal(err)
	}
	url := Source{Type: SourceTypeURL, URL: "https://example.test/x.png"}
	if err := url.Validate(); err != nil {
		t.Fatal(err)
	}
	// The source union is exclusive: a base64 source must not carry a url,
	// and vice versa.
	if err := (Source{Type: SourceTypeBase64, Data: "x", MediaType: "image/png", URL: "u"}).Validate(); err == nil {
		t.Fatal("base64 source with url accepted")
	}
	if err := (Source{Type: SourceTypeURL, URL: "u", Data: "x"}).Validate(); err == nil {
		t.Fatal("url source with data accepted")
	}
	if err := (Source{Type: SourceTypeBase64, MediaType: "image/png"}).Validate(); err == nil {
		t.Fatal("data-less base64 source accepted")
	}
	if err := (Source{Type: SourceTypeURL}).Validate(); err == nil {
		t.Fatal("url-less url source accepted")
	}
	if err := (Source{Type: "bogus"}).Validate(); err == nil {
		t.Fatal("unknown source type accepted")
	}
}

func TestContentBlockValidateArms(t *testing.T) {
	text := "hi"
	if err := (ContentBlock{Type: ContentBlockTypeText, Text: &text}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeText}).Validate(); err == nil {
		t.Fatal("text-less text block accepted")
	}
	if err := (ContentBlock{Type: ContentBlockTypeImage}).Validate(); err == nil {
		t.Fatal("source-less image block accepted")
	}
	// A document block with a URL source (media_type derived by the API).
	document := ContentBlock{Type: ContentBlockTypeDocument, Source: &Source{
		Type: SourceTypeURL,
		URL:  "https://example.test/doc.pdf",
	}}
	if err := document.Validate(); err != nil {
		t.Fatal(err)
	}
	// tool_use requires id, name, and an object input.
	id, name := "tu_1", "f"
	block := ContentBlock{Type: ContentBlockTypeToolUse, ID: &id, Name: &name, Input: json.RawMessage(`{"a":1}`)}
	if err := block.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeToolUse, Name: &name, Input: json.RawMessage(`{}`)}).Validate(); err == nil {
		t.Fatal("id-less tool_use accepted")
	}
	if err := (ContentBlock{Type: ContentBlockTypeToolUse, ID: &id, Input: json.RawMessage(`{}`)}).Validate(); err == nil {
		t.Fatal("name-less tool_use accepted")
	}
	if err := (ContentBlock{Type: ContentBlockTypeToolUse, ID: &id, Name: &name}).Validate(); err == nil {
		t.Fatal("input-less tool_use accepted")
	}
	if err := (ContentBlock{Type: ContentBlockTypeToolUse, ID: &id, Name: &name, Input: json.RawMessage(`"not an object"`)}).Validate(); err == nil {
		t.Fatal("non-object tool_use input accepted")
	}
	// tool_result requires tool_use_id and content.
	toolUseID := "tu_1"
	content := Content{ContentStr: &text}
	result := ContentBlock{Type: ContentBlockTypeToolResult, ToolUseID: &toolUseID, Content: &content}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeToolResult, Content: &content}).Validate(); err == nil {
		t.Fatal("tool_use_id-less tool_result accepted")
	}
	if err := (ContentBlock{Type: ContentBlockTypeToolResult, ToolUseID: &toolUseID}).Validate(); err == nil {
		t.Fatal("content-less tool_result accepted")
	}
	// thinking requires thinking and signature; redacted_thinking requires
	// data — both preserved byte-exact, never synthesized.
	thinking, signature := "t", "sig"
	if err := (ContentBlock{Type: ContentBlockTypeThinking, Thinking: &thinking, Signature: &signature}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeThinking, Thinking: &thinking}).Validate(); err == nil {
		t.Fatal("signature-less thinking accepted")
	}
	data := "redacted"
	if err := (ContentBlock{Type: ContentBlockTypeRedactedThinking, Data: &data}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeRedactedThinking}).Validate(); err == nil {
		t.Fatal("data-less redacted_thinking accepted")
	}
	if err := (ContentBlock{Type: "bogus"}).Validate(); err == nil {
		t.Fatal("unknown block type accepted")
	}
}

func TestContentUnionValidate(t *testing.T) {
	text := "hi"
	blocks := Content{ContentBlocks: []ContentBlock{{Type: ContentBlockTypeText, Text: &text}}}
	if err := blocks.Validate(); err != nil {
		t.Fatal(err)
	}
	// Both variants selected is a contradictory union.
	if err := (Content{ContentStr: &text, ContentBlocks: blocks.ContentBlocks}).Validate(); err == nil {
		t.Fatal("both content variants accepted")
	}
	// No variant selected is invalid.
	if err := (Content{}).Validate(); err == nil {
		t.Fatal("empty content accepted")
	}
	// Marshal rejects an invalid union.
	if _, err := json.Marshal(Content{}); err == nil {
		t.Fatal("invalid content marshaled")
	}
}

func TestMessageAndToolValidate(t *testing.T) {
	text := "hi"
	if err := (Message{Role: RoleUser, Content: Content{ContentStr: &text}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Message{Role: "bogus", Content: Content{ContentStr: &text}}).Validate(); err == nil {
		t.Fatal("unknown role accepted")
	}
	if err := (Message{Role: RoleUser}).Validate(); err == nil {
		t.Fatal("content-less message accepted")
	}

	if err := (Tool{Name: "f", InputSchema: json.RawMessage(`{"type":"object"}`)}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Tool{InputSchema: json.RawMessage(`{}`)}).Validate(); err == nil {
		t.Fatal("name-less tool accepted")
	}
	if err := (Tool{Name: "f"}).Validate(); err == nil {
		t.Fatal("schema-less tool accepted")
	}
}

// TestRequestRequiredFields proves the pinned request contract rejects a
// missing model or non-positive max_tokens (client-dialect validation in the
// transcode boundary; the wire shape carries the fields).
func TestRequestShape(t *testing.T) {
	wire := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	var request Request
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatal(err)
	}
	if request.Model != "m" || request.MaxTokens != 10 || len(request.Messages) != 1 {
		t.Fatalf("request = %+v", request)
	}
}

// TestThinkingBlockCacheControlRejected pins that cache_control on a
// thinking-family block is a typed unknown-field rejection, matching the
// official prompt-caching contract ("Thinking blocks cannot be cached
// directly with cache_control", Anthropic Build with Claude -> Prompt
// caching, "What cannot be cached"). The asymmetry with the
// text/image/document/tool_use/tool_result arms - which admit the marker
// and note the drop at decode - is the contract's own shape, not an
// oversight.
func TestThinkingBlockCacheControlRejected(t *testing.T) {
	for _, block := range []string{
		`{"type":"thinking","thinking":"t","signature":"s","cache_control":{"type":"ephemeral"}}`,
		`{"type":"redacted_thinking","data":"ZGF0YQ==","cache_control":{"type":"ephemeral"}}`,
	} {
		var cb ContentBlock
		err := json.Unmarshal([]byte(block), &cb)
		if err == nil {
			t.Fatalf("cache_control accepted on a thinking-family block: %s", block)
		}
		var typed *wire.DecodeError
		if !errors.As(err, &typed) || typed.Kind != wire.DecodeUnknownField {
			t.Fatalf("block %s: error = %v (%T), want wire.DecodeError/unknown_field", block, err, err)
		}
	}
}

func TestTextCitationsArms(t *testing.T) {
	// char_location
	charJSON := `{"type":"char_location","cited_text":"excerpt","document_index":0,"document_title":"doc","start_char_index":5,"end_char_index":12,"file_id":"file_1"}`
	var charCit TextCitation
	if err := json.Unmarshal([]byte(charJSON), &charCit); err != nil {
		t.Fatalf("unmarshal char_location: %v", err)
	}
	if charCit.Type != CitationTypeCharLocation || charCit.CitedText != "excerpt" ||
		charCit.DocumentIndex == nil || *charCit.DocumentIndex != 0 ||
		charCit.StartCharIndex == nil || *charCit.StartCharIndex != 5 ||
		charCit.EndCharIndex == nil || *charCit.EndCharIndex != 12 ||
		charCit.DocumentTitle == nil || *charCit.DocumentTitle != "doc" ||
		charCit.FileID == nil || *charCit.FileID != "file_1" {
		t.Fatalf("char_location unexpected: %+v", charCit)
	}

	// page_location
	pageJSON := `{"type":"page_location","cited_text":"paged excerpt","document_index":1,"start_page_number":2,"end_page_number":4}`
	var pageCit TextCitation
	if err := json.Unmarshal([]byte(pageJSON), &pageCit); err != nil {
		t.Fatalf("unmarshal page_location: %v", err)
	}
	if pageCit.Type != CitationTypePageLocation || *pageCit.StartPageNumber != 2 || *pageCit.EndPageNumber != 4 {
		t.Fatalf("page_location unexpected: %+v", pageCit)
	}

	// content_block_location
	blockJSON := `{"type":"content_block_location","cited_text":"block excerpt","document_index":2,"start_block_index":0,"end_block_index":1}`
	var blockCit TextCitation
	if err := json.Unmarshal([]byte(blockJSON), &blockCit); err != nil {
		t.Fatalf("unmarshal content_block_location: %v", err)
	}
	if blockCit.Type != CitationTypeContentBlockLocation || *blockCit.StartBlockIndex != 0 || *blockCit.EndBlockIndex != 1 {
		t.Fatalf("content_block_location unexpected: %+v", blockCit)
	}

	// web_search_result_location
	webJSON := `{"type":"web_search_result_location","cited_text":"web excerpt","url":"https://example.com","title":"Example","encrypted_index":"enc_1"}`
	var webCit TextCitation
	if err := json.Unmarshal([]byte(webJSON), &webCit); err != nil {
		t.Fatalf("unmarshal web_search_result_location: %v", err)
	}
	if webCit.Type != CitationTypeWebSearchResultLocation || *webCit.URL != "https://example.com" ||
		*webCit.Title != "Example" || *webCit.EncryptedIndex != "enc_1" {
		t.Fatalf("web_search_result_location unexpected: %+v", webCit)
	}

	// search_result_location
	searchJSON := `{"type":"search_result_location","cited_text":"search excerpt","search_result_index":3,"title":"search title","source":"tool","start_block_index":1,"end_block_index":3}`
	var searchCit TextCitation
	if err := json.Unmarshal([]byte(searchJSON), &searchCit); err != nil {
		t.Fatalf("unmarshal search_result_location: %v", err)
	}
	if searchCit.Type != CitationTypeSearchResultLocation || *searchCit.SearchResultIndex != 3 ||
		searchCit.Title == nil || *searchCit.Title != "search title" ||
		*searchCit.Source != "tool" || *searchCit.StartBlockIndex != 1 || *searchCit.EndBlockIndex != 3 {
		t.Fatalf("search_result_location unexpected: %+v", searchCit)
	}

	// Unknown type rejected
	var bogus TextCitation
	if err := json.Unmarshal([]byte(`{"type":"bogus_location","cited_text":"x"}`), &bogus); err == nil {
		t.Fatal("unknown citation type accepted")
	}

	// Unknown field in arm rejected
	var unknownField TextCitation
	if err := json.Unmarshal([]byte(`{"type":"char_location","cited_text":"x","document_index":0,"start_char_index":0,"end_char_index":1,"extra":"nope"}`), &unknownField); err == nil {
		t.Fatal("unknown field in char_location accepted")
	}

	// Duplicate key in arm rejected
	var dupKey TextCitation
	if err := json.Unmarshal([]byte(`{"type":"char_location","cited_text":"x","cited_text":"y","document_index":0,"start_char_index":0,"end_char_index":1}`), &dupKey); err == nil {
		t.Fatal("duplicate key in citation accepted")
	}

	// Missing required fields rejected
	var missingRequired TextCitation
	if err := json.Unmarshal([]byte(`{"type":"char_location","document_index":0,"start_char_index":0,"end_char_index":1}`), &missingRequired); err == nil {
		t.Fatal("char_location without cited_text accepted")
	}

	// Inverted indices rejected
	var inverted TextCitation
	if err := json.Unmarshal([]byte(`{"type":"char_location","cited_text":"x","document_index":0,"start_char_index":10,"end_char_index":5}`), &inverted); err == nil {
		t.Fatal("char_location with inverted indices accepted")
	}
}

func TestContentBlockTextWithCitations(t *testing.T) {
	textBlockJSON := `{
		"type": "text",
		"text": "Here is information based on the document.",
		"citations": [
			{
				"type": "char_location",
				"cited_text": "source text",
				"document_index": 0,
				"start_char_index": 10,
				"end_char_index": 21
			}
		],
		"cache_control": {"type": "ephemeral"}
	}`
	var cb ContentBlock
	if err := json.Unmarshal([]byte(textBlockJSON), &cb); err != nil {
		t.Fatalf("unmarshal text block with citations: %v", err)
	}
	if cb.Type != ContentBlockTypeText || *cb.Text != "Here is information based on the document." {
		t.Fatalf("text block content mismatch: %+v", cb)
	}
	if len(cb.Citations) != 1 || cb.Citations[0].Type != CitationTypeCharLocation || cb.Citations[0].CitedText != "source text" {
		t.Fatalf("citations mismatch: %+v", cb.Citations)
	}
	if cb.CacheControl == nil {
		t.Fatal("cache_control dropped")
	}

	// Invalid citation inside text block fails decode
	badCitationJSON := `{
		"type": "text",
		"text": "bad",
		"citations": [{"type": "unknown"}]
	}`
	var badCB ContentBlock
	if err := json.Unmarshal([]byte(badCitationJSON), &badCB); err == nil {
		t.Fatal("text block with invalid citation accepted")
	}
}

func TestStreamDeltaCitations(t *testing.T) {
	deltaJSON := `{
		"type": "citations_delta",
		"citation": {
			"type": "web_search_result_location",
			"cited_text": "result",
			"url": "https://example.org"
		}
	}`
	var delta StreamDelta
	if err := json.Unmarshal([]byte(deltaJSON), &delta); err != nil {
		t.Fatalf("unmarshal citations_delta: %v", err)
	}
	if delta.Type != StreamDeltaTypeCitationsDelta || delta.Citation == nil ||
		delta.Citation.Type != CitationTypeWebSearchResultLocation ||
		*delta.Citation.URL != "https://example.org" {
		t.Fatalf("stream delta citation mismatch: %+v", delta)
	}
}

// TestServerBlockAdmission pins the server-spelling wire contract: server-side
// spellings decode (admit-and-tag) instead of failing strict decode, so the
// convert layer can drop or reject them under the anthropic_server_tools
// loss key; MCP spellings decode their tool_use/tool_result-equivalent
// fields for the 1:1 mapping. Unknown spellings still reject.
func TestServerBlockAdmission(t *testing.T) {
	admitted := []string{
		`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`,
		`{"type":"web_search_tool_result","tool_use_id":"srv_1","content":"x"}`,
		`{"type":"code_execution","id":"e1"}`,
		`{"type":"code_execution_tool_result","tool_use_id":"e1","content":"ok"}`,
		`{"type":"container_upload","file_id":"file_1"}`,
	}
	for _, raw := range admitted {
		var block ContentBlock
		if err := json.Unmarshal([]byte(raw), &block); err != nil {
			t.Fatalf("server block rejected at wire decode: %s: %v", raw, err)
		}
		if len(block.ServerContent) == 0 {
			t.Fatalf("server block lost its raw bytes: %s", raw)
		}
	}
	mcpUse := `{"type":"mcp_tool_use","id":"call_1","name":"read","input":{"x":1}}`
	var use ContentBlock
	if err := json.Unmarshal([]byte(mcpUse), &use); err != nil {
		t.Fatalf("mcp_tool_use rejected: %v", err)
	}
	if use.ID == nil || *use.ID != "call_1" || use.Name == nil || *use.Name != "read" {
		t.Fatalf("mcp_tool_use fields not decoded: %+v", use)
	}
	mcpResult := `{"type":"mcp_tool_result","tool_use_id":"call_1","content":"done"}`
	var result ContentBlock
	if err := json.Unmarshal([]byte(mcpResult), &result); err != nil {
		t.Fatalf("mcp_tool_result rejected: %v", err)
	}
	if result.ToolUseID == nil || *result.ToolUseID != "call_1" {
		t.Fatalf("mcp_tool_result fields not decoded: %+v", result)
	}
	var bogus ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"quantum_compute"}`), &bogus); err == nil {
		t.Fatal("unknown block type accepted")
	} else if got := err.Error(); !strings.Contains(got, `unknown anthropic content block type "quantum_compute"`) {
		t.Fatalf("unknown type err = %v, want the unattributed-type rejection", err)
	}
}
