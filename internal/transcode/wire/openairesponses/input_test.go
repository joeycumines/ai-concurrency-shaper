package openairesponses

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// TestDecodeInputItemCodexHistoryOutputTextNoAnnotations reproduces the
// field-observed Codex turn-2 failure (autopsy 01): real clients replay
// assistant history as output_text parts WITHOUT an annotations key, and the
// union decode must normalize the absent array to [] instead of rejecting
// the item. Validate itself stays strict — only the decode path normalizes.
func TestDecodeInputItemCodexHistoryOutputTextNoAnnotations(t *testing.T) {
	data := `{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]}`
	item, err := DecodeInputItem([]byte(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	message, ok := item.(*PreviousOutputMessage)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	text, ok := message.Content[0].(*OutputText)
	if !ok {
		t.Fatalf("content part = %T", message.Content[0])
	}
	if text.Annotations == nil || len(text.Annotations) != 0 {
		t.Fatalf("annotations = %#v, want decoded empty array", text.Annotations)
	}

	// Strictness pin: an unknown field inside an output_text part is still
	// a typed DecodeUnknownField rejection — normalization never widens the
	// accepted wire shape.
	bad := `{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","bogus":1}]}`
	if _, err := DecodeInputItem([]byte(bad)); err == nil {
		t.Fatal("expected unknown-field rejection inside output_text")
	} else {
		var decodeErr *wire.DecodeError
		if !errors.As(err, &decodeErr) || decodeErr.Kind != wire.DecodeUnknownField {
			t.Fatalf("err = %v, want DecodeUnknownField", err)
		}
	}

	// Strictness pin: a re-marshaled decoded part emits "annotations":[]
	// (never null and never an absent key) — identical to the response-side
	// emission contract.
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Content []struct {
			Annotations *json.RawMessage `json:"annotations"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Content) != 1 || probe.Content[0].Annotations == nil ||
		string(*probe.Content[0].Annotations) != "[]" {
		t.Fatalf("re-marshaled item = %s, want content[0].annotations == []", encoded)
	}

	// Validate stays strict for hand-built values.
	if err := (&OutputText{Type: "output_text", Text: "x"}).Validate(); err == nil {
		t.Fatal("hand-built nil-annotation OutputText accepted")
	}
}

// TestDecodeStreamContentPartOutputTextNoAnnotations pins the same
// decode-side normalization for the stream content-part union (autopsy 01):
// upstream content_part events may omit annotations.
func TestDecodeStreamContentPartOutputTextNoAnnotations(t *testing.T) {
	data := `{"type":"response.content_part.added","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hi"}}`
	var event ContentPartAddedEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatalf("decode: %v", err)
	}
	part, ok := event.Part.(*StreamOutputTextPart)
	if !ok {
		t.Fatalf("part = %T", event.Part)
	}
	if part.Annotations == nil || len(part.Annotations) != 0 {
		t.Fatalf("annotations = %#v, want decoded empty array", part.Annotations)
	}

	doneData := `{"type":"response.content_part.done","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hi"}}`
	var doneEvent ContentPartDoneEvent
	if err := json.Unmarshal([]byte(doneData), &doneEvent); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	donePart, ok := doneEvent.Part.(*StreamOutputTextPart)
	if !ok {
		t.Fatalf("done part = %T", doneEvent.Part)
	}
	if donePart.Annotations == nil || len(donePart.Annotations) != 0 {
		t.Fatalf("done annotations = %#v, want decoded empty array", donePart.Annotations)
	}

	// Validate stays strict for hand-built values.
	if err := (&StreamOutputTextPart{Type: "output_text", Text: "x"}).Validate(); err == nil {
		t.Fatal("hand-built nil-annotation StreamOutputTextPart accepted")
	}
}

// TestDecodeInputItemAssistantEasyMessageOutputParts covers the second
// autopsy-01 defect: an assistant easy input message (no id/status) carries
// output-type content parts in the field, and DecodeInputContentPart must
// decode output_text/refusal arms — legal only for the assistant role.
func TestDecodeInputItemAssistantEasyMessageOutputParts(t *testing.T) {
	item, err := DecodeInputItem([]byte(`{"role":"assistant","content":[{"type":"output_text","text":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode assistant output_text: %v", err)
	}
	easy, ok := item.(*EasyInputMessage)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	text, ok := easy.Content.Parts[0].(*OutputText)
	if !ok {
		t.Fatalf("content part = %T", easy.Content.Parts[0])
	}
	if text.Text != "hi" || text.Annotations == nil || len(text.Annotations) != 0 {
		t.Fatalf("part = %+v", text)
	}

	item, err = DecodeInputItem([]byte(`{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`))
	if err != nil {
		t.Fatalf("decode assistant refusal: %v", err)
	}
	easy, ok = item.(*EasyInputMessage)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	if _, ok := easy.Content.Parts[0].(*OutputRefusal); !ok {
		t.Fatalf("content part = %T", easy.Content.Parts[0])
	}

	// Non-assistant easy messages keep the typed rejection.
	for _, role := range []InputRole{InputRoleUser, InputRoleSystem, InputRoleDeveloper} {
		body := fmt.Sprintf(`{"role":%q,"content":[{"type":"output_text","text":"hi"}]}`, role)
		_, err := DecodeInputItem([]byte(body))
		if err == nil {
			t.Fatalf("%s role accepted output_text", role)
		}
		var unsupported *wire.UnsupportedTypeError
		if !errors.As(err, &unsupported) || unsupported.Type != "output_text" {
			t.Fatalf("%s role err = %v, want UnsupportedTypeError(output_text)", role, err)
		}
	}

	// Hand-built validation is role-conditional too.
	assistant := &EasyInputMessage{
		Role: InputRoleAssistant,
		Content: InputMessageContent{
			Parts: InputContentParts{
				&OutputText{Type: "output_text", Text: "x", Annotations: []Annotation{}},
			},
		},
	}
	if err := assistant.Validate(); err != nil {
		t.Fatal(err)
	}
	user := &EasyInputMessage{
		Role: InputRoleUser,
		Content: InputMessageContent{
			Parts: InputContentParts{
				&OutputText{Type: "output_text", Text: "x", Annotations: []Annotation{}},
			},
		},
	}
	if err := user.Validate(); err == nil {
		t.Fatal("user easy message accepted output parts")
	}
	// String-content assistant easy messages are unchanged.
	if err := (&EasyInputMessage{Role: InputRoleAssistant, Content: InputMessageContent{Text: new("hi")}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestDecodeInputItemFunctionOutputRejectsOutputParts pins review F1: the
// output_text/refusal input-part arms exist for assistant message history
// only — a function_call_output payload keeps the typed rejection it had
// before those arms were added.
func TestDecodeInputItemFunctionOutputRejectsOutputParts(t *testing.T) {
	for _, part := range []string{
		`{"type":"output_text","text":"hi"}`,
		`{"type":"refusal","refusal":"no"}`,
	} {
		body := fmt.Sprintf(
			`{"type":"function_call_output","call_id":"c","output":[%s]}`,
			part,
		)
		_, err := DecodeInputItem([]byte(body))
		if err == nil {
			t.Fatalf("function output accepted %s", part)
		}
		var unsupported *wire.UnsupportedTypeError
		if !errors.As(err, &unsupported) {
			t.Fatalf("err = %T: %v, want UnsupportedTypeError", err, err)
		}
		if unsupported.Type != "output_text" && unsupported.Type != "refusal" {
			t.Fatalf("type = %q, want output_text or refusal", unsupported.Type)
		}
	}

	// Input-type parts in a function output keep decoding.
	item, err := DecodeInputItem([]byte(
		`{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"ok"}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	output, ok := item.(*FunctionCallOutputInput)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	if _, ok := output.Output.Parts[0].(*InputText); !ok {
		t.Fatalf("part = %T", output.Output.Parts[0])
	}
}
