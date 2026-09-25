package transcode

// Tests for the tool_result_images chat capability: multimodal tool-result
// content renders as multipart chat content blocks (text + image_url, order
// preserved) when the target upstream accepts image parts inside a tool
// message, and falls back to the deterministic JSON text envelope when the
// capability is off.

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolResultImageCapabilities is the capability set under test: image parts
// inside a tool message, with the user-image vocabulary they require.
func toolResultImageCapabilities() ChatCapabilities {
	return ChatCapabilities{ImageInput: true, ToolResultImages: true}
}

// TestRenderChatToolResultMultipart proves the capability renders text and
// image parts as ordered content blocks — never the JSON envelope — so a
// vision model receives the image bytes.
func TestRenderChatToolResultMultipart(t *testing.T) {
	result := CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalText{Text: "analysis complete"},
			CanonicalImage{MediaType: "image/png", Base64: "aGk="},
		},
	}
	var report ConversionReport
	message, err := renderChatToolResult(result, toolResultImageCapabilities(), StrictLossPolicy(), &report)
	if err != nil {
		t.Fatalf("multipart render under strict policy: %v", err)
	}
	if message.Role != ChatMessageRoleTool {
		t.Fatalf("role = %q, want tool", message.Role)
	}
	if message.ChatToolMessage == nil || message.ChatToolMessage.ToolCallID == nil ||
		*message.ChatToolMessage.ToolCallID != "call_1" {
		t.Fatalf("tool_call_id = %+v, want call_1", message.ChatToolMessage)
	}
	if message.Content == nil || message.Content.ContentStr != nil {
		t.Fatalf("content = %+v, want the block arm (never the string arm)", message.Content)
	}
	blocks := message.Content.ContentBlocks
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if blocks[0].Type != ChatContentBlockTypeText || blocks[0].Text == nil || *blocks[0].Text != "analysis complete" {
		t.Fatalf("block 0 = %+v, want the text part first", blocks[0])
	}
	if blocks[1].Type != ChatContentBlockTypeImage || blocks[1].ImageURL == nil ||
		!strings.HasPrefix(blocks[1].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("block 1 = %+v, want the image part second", blocks[1])
	}
	if blocks[1].ImageURL.Detail == nil || *blocks[1].ImageURL.Detail != "auto" {
		t.Fatalf("image detail = %v, want auto", blocks[1].ImageURL.Detail)
	}
	// The payload must be wire-legal: the block union marshals as an array.
	marshaled, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal tool message: %v", err)
	}
	if !strings.HasPrefix(string(marshaled), `{"role":"tool"`) {
		t.Fatalf("marshaled message = %s", marshaled)
	}
	if !strings.Contains(string(marshaled), `"content":[{"type":"text"`) {
		t.Fatalf("content is not a block array: %s", marshaled)
	}
	// The truthful lossless encoding records NO envelope loss.
	if reportHasFeature(report, FeatureToolResultMultimodalContent) ||
		reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("multipart render recorded an envelope loss: %+v", report)
	}
}

// TestRenderChatToolResultMultipartOrderPreserved proves the client's part
// order survives: an image part BEFORE the text part stays first.
func TestRenderChatToolResultMultipartOrderPreserved(t *testing.T) {
	result := CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalImage{MediaType: "image/webp", URL: "https://example.test/x.webp"},
			CanonicalText{Text: "after"},
		},
	}
	var report ConversionReport
	message, err := renderChatToolResult(result, toolResultImageCapabilities(), StrictLossPolicy(), &report)
	if err != nil {
		t.Fatal(err)
	}
	blocks := message.Content.ContentBlocks
	if len(blocks) != 2 || blocks[0].Type != ChatContentBlockTypeImage || blocks[1].Type != ChatContentBlockTypeText {
		t.Fatalf("blocks = %+v, want image then text", blocks)
	}
	if blocks[0].ImageURL.URL != "https://example.test/x.webp" {
		t.Fatalf("image URL = %q", blocks[0].ImageURL.URL)
	}
}

// TestRenderChatToolResultMultipartFallback proves the capability does NOT
// hijack shapes it cannot carry: a document still takes the JSON envelope,
// while an image whose media type is outside the Chat vocabulary is a real
// encoding error on both paths (the envelope cannot data-URL it either), so
// the render fails rather than inventing content.
func TestRenderChatToolResultMultipartFallback(t *testing.T) {
	document := CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalText{Text: "see attached"},
			CanonicalDocument{MediaType: "application/pdf", URL: "https://example.test/d.pdf"},
		},
	}
	var report ConversionReport
	message, err := renderChatToolResult(document, toolResultImageCapabilities(), toolResultPermissivePolicy(), &report)
	if err != nil {
		t.Fatalf("document fallback: %v", err)
	}
	if message.Content.ContentStr == nil {
		t.Fatalf("document did not fall back to the envelope: %+v", message.Content)
	}
	if !strings.Contains(*message.Content.ContentStr, `"type":"document"`) {
		t.Fatalf("envelope = %s", *message.Content.ContentStr)
	}
	if !reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("document fallback recorded no envelope loss: %+v", report)
	}

	// An image media type outside the Chat vocabulary is an encoding error on
	// BOTH paths: the multipart path declines, and the JSON envelope cannot
	// data-URL it either — so the render fails rather than inventing content.
	badMedia := CanonicalFunctionResult{
		CallID: "call_2",
		Parts: []CanonicalPart{
			CanonicalImage{MediaType: "image/svg+xml", Base64: "aGk="},
		},
	}
	var report2 ConversionReport
	if _, err := renderChatToolResult(badMedia, toolResultImageCapabilities(), toolResultPermissivePolicy(), &report2); err == nil {
		t.Fatal("unsupported image media type accepted")
	}
}

// TestRenderChatToolResultMultipartRequiresImageInput proves the capability
// is not a bypass: with ToolResultImages on but ImageInput off, the multipart
// path declines and the envelope fallback stands (ImageInput is the
// vocabulary that authorizes rendering image parts at all).
func TestRenderChatToolResultMultipartRequiresImageInput(t *testing.T) {
	result := CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalText{Text: "x"},
			CanonicalImage{MediaType: "image/png", Base64: "aGk="},
		},
	}
	var report ConversionReport
	message, err := renderChatToolResult(
		result,
		ChatCapabilities{ToolResultImages: true},
		toolResultPermissivePolicy(),
		&report,
	)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content.ContentStr == nil {
		t.Fatalf("without ImageInput the multipart path must decline: %+v", message.Content)
	}
	if !reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("expected the envelope fallback loss: %+v", report)
	}
}

// TestRenderChatToolResultMultipartInertForText proves the capability changes
// nothing for text-only results: the string arm is used unconditionally.
func TestRenderChatToolResultMultipartInertForText(t *testing.T) {
	result := CanonicalFunctionResult{
		CallID: "call_1",
		Parts:  []CanonicalPart{CanonicalText{Text: "plain"}},
	}
	var report ConversionReport
	message, err := renderChatToolResult(result, toolResultImageCapabilities(), StrictLossPolicy(), &report)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content.ContentStr == nil || *message.Content.ContentStr != "plain" {
		t.Fatalf("text result rendered as %+v, want the string arm", message.Content)
	}
	if len(report.Losses) != 0 {
		t.Fatalf("text result recorded report entries: %+v", report)
	}
}
