package transcode

// Tests for truthful image-detail handling on the chat target: the Chat
// vocabulary is auto|low|high, so the Responses-only "original" maps to "high"
// with an ungated note, a source dialect with no detail field (Anthropic)
// gets the documented "auto" default with the invention noted, and any other
// value is rejected rather than forwarded unvalidated.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// detailOf renders one user image request and returns the rendered upstream
// detail value plus the report.
func detailOf(t *testing.T, detail string, caps ChatCapabilities) (string, ConversionReport) {
	t.Helper()
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalText{Text: "see"},
				CanonicalImage{
					MediaType: "image/png",
					URL:       "https://example.test/x.png",
					Detail:    detail,
				},
			},
		}},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureImageInput: {},
	}}
	rendered, report, err := RenderChatRequest(request, context, caps)
	if err != nil {
		t.Fatalf("render detail %q: %v", detail, err)
	}
	var body struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL *struct {
					Detail *string `json:"detail"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rendered, &body); err != nil {
		t.Fatalf("rendered request is not JSON: %v", err)
	}
	for _, m := range body.Messages {
		for _, block := range m.Content {
			if block.Type == "image_url" && block.ImageURL != nil && block.ImageURL.Detail != nil {
				return *block.ImageURL.Detail, report
			}
		}
	}
	t.Fatal("no image_url block with a detail in the rendered request")
	return "", report
}

// TestChatImageDetailVocabularyPassThrough proves auto/low/high pass through
// unchanged and record nothing.
func TestChatImageDetailVocabularyPassThrough(t *testing.T) {
	caps := ChatCapabilities{ImageInput: true}
	for _, detail := range []string{"auto", "low", "high"} {
		got, report := detailOf(t, detail, caps)
		if got != detail {
			t.Fatalf("detail %q rendered as %q", detail, got)
		}
		if reportHasFeature(report, FeatureImageDetailInvented) ||
			reportHasFeature(report, FeatureImageDetailOriginal) {
			t.Fatalf("detail %q recorded a detail note: %+v", detail, report)
		}
	}
}

// TestChatImageDetailOriginalMappedToHigh proves the Responses-only value
// "original" maps to "high" and is recorded as an ungated note.
func TestChatImageDetailOriginalMappedToHigh(t *testing.T) {
	got, report := detailOf(t, "original", ChatCapabilities{ImageInput: true})
	if got != "high" {
		t.Fatalf("original rendered as %q, want high", got)
	}
	if !reportHasFeature(report, FeatureImageDetailOriginal) {
		t.Fatalf("report lacks image_detail_original: %+v", report)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureImageDetailOriginal && loss.Kind != NoteRecord {
			t.Fatalf("image_detail_original kind = %v, want NoteRecord", loss.Kind)
		}
	}
}

// TestChatImageDetailInventedAutoNoted proves an empty detail (the Anthropic
// image block carries none) renders the documented auto default and records
// the invention as a note.
func TestChatImageDetailInventedAutoNoted(t *testing.T) {
	got, report := detailOf(t, "", ChatCapabilities{ImageInput: true})
	if got != "auto" {
		t.Fatalf("empty detail rendered as %q, want auto", got)
	}
	if !reportHasFeature(report, FeatureImageDetailInvented) {
		t.Fatalf("report lacks image_detail_invented: %+v", report)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureImageDetailInvented && loss.Kind != NoteRecord {
			t.Fatalf("image_detail_invented kind = %v, want NoteRecord", loss.Kind)
		}
	}
}

// TestChatImageDetailUnknownRejected proves a value outside both vocabularies
// is rejected rather than forwarded unvalidated.
func TestChatImageDetailUnknownRejected(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalImage{
					MediaType: "image/png",
					URL:       "https://example.test/x.png",
					Detail:    "ultra",
				},
			},
		}},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureImageInput: {},
	}}
	_, _, err := RenderChatRequest(request, context, ChatCapabilities{ImageInput: true})
	if err == nil {
		t.Fatal("unknown detail accepted")
	}
	if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
		t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
	}
	if !strings.Contains(err.Error(), "image detail ultra") {
		t.Fatalf("error = %q, want the offending detail named", err.Error())
	}
}
