package openairesponses

// Unit tests for the event types' accessor and validation surface: the
// transcode suite exercises the full stream lifecycles, while these cover
// the per-event accessors (EventType, Sequence) and Validate branches that
// the lifecycle paths do not reach.

import (
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestEventAccessorsAndValidation(t *testing.T) {
	envelope := Response{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "completed",
		Model:     "m",
		Output:    []OutputItem{},
	}
	part := &StreamOutputTextPart{Type: "output_text", Text: "x", Annotations: []Annotation{}}
	builder := &EventBuilder{}
	cases := []struct {
		name    string
		event   Event
		want    string // expected EventType
		valid   bool   // whether Validate passes
		invalid bool   // whether a mutated copy fails Validate
	}{
		{"created", builder.Created(envelope), "response.created", true, false},
		{"in_progress", builder.InProgress(envelope), "response.in_progress", true, false},
		{"output_item.added", builder.OutputItemAdded(0, &OutputMessage{
			ID: "msg_1", Type: "message", Role: "assistant", Status: ItemInProgress,
			Content: OutputContentParts{},
		}), "response.output_item.added", true, false},
		{"output_item.done", builder.OutputItemDone(0, &OutputMessage{
			ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted,
			Content: OutputContentParts{},
		}), "response.output_item.done", true, false},
		{"reasoning_summary_part.added", builder.ReasoningSummaryPartAdded(
			"rs_1", 0, 0, SummaryTextPart{Type: "summary_text", Text: "s"},
		), "response.reasoning_summary_part.added", true, true},
		{"reasoning_summary_text.delta", builder.ReasoningSummaryTextDelta(
			"rs_1", 0, 0, "d",
		), "response.reasoning_summary_text.delta", true, true},
		{"reasoning_summary_text.done", builder.ReasoningSummaryTextDone(
			"rs_1", 0, 0, "t",
		), "response.reasoning_summary_text.done", true, true},
		{"reasoning_summary_part.done", builder.ReasoningSummaryPartDone(
			"rs_1", 0, 0, SummaryTextPart{Type: "summary_text", Text: "s"},
		), "response.reasoning_summary_part.done", true, true},
		{"content_part.added", builder.ContentPartAdded("msg_1", 0, 0, part),
			"response.content_part.added", true, true},
		{"output_text.delta", builder.TextDelta("msg_1", 0, 0, "d"),
			"response.output_text.delta", true, false},
		{"output_text.done", builder.TextDone("msg_1", 0, 0, "t"),
			"response.output_text.done", true, false},
		{"content_part.done", builder.ContentPartDone("msg_1", 0, 0, part),
			"response.content_part.done", true, true},
		{"function_call_arguments.delta", builder.FunctionArgumentsDelta("fc_1", 0, "{"),
			"response.function_call_arguments.delta", true, true},
		{"function_call_arguments.done", builder.FunctionArgumentsDone("fc_1", 0, "{}"),
			"response.function_call_arguments.done", true, true},
		{"refusal.delta", builder.RefusalDelta("msg_1", 0, 0, "d"),
			"response.refusal.delta", true, true},
		{"refusal.done", builder.RefusalDone("msg_1", 0, 0, "r"),
			"response.refusal.done", true, true},
		{"completed", builder.Completed(envelope), "response.completed", true, false},
		{"incomplete", builder.Incomplete(incompleteResponse(envelope)),
			"response.incomplete", true, false},
		{"failed", builder.Failed(failedResponse(envelope)), "response.failed", true, false},
		{"error", builder.Error("code", "message", "param"), "error", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.event.EventType(); got != c.want {
				t.Fatalf("EventType() = %q, want %q", got, c.want)
			}
			if err := c.event.Validate(); err != nil && c.valid {
				t.Fatalf("Validate() = %v", err)
			}
			if got := c.event.Sequence(); got < 0 {
				t.Fatalf("Sequence() = %d", got)
			}
		})
	}
}

// incompleteResponse returns the envelope with the status the incomplete
// terminal event requires.
func incompleteResponse(envelope Response) Response {
	envelope.Status = "incomplete"
	return envelope
}

// failedResponse returns the envelope with the status the failed terminal
// event requires.
func failedResponse(envelope Response) Response {
	envelope.Status = "failed"
	return envelope
}

// TestEventValidateNegativeCases covers the validation branches the
// lifecycle tests do not reach.
func TestEventValidateNegativeCases(t *testing.T) {
	builder := &EventBuilder{}
	if err := builder.ReasoningSummaryPartAdded(
		"", 0, 0, SummaryTextPart{Type: "summary_text", Text: "s"},
	).Validate(); err == nil || !strings.Contains(err.Error(), "item_id") {
		t.Fatalf("missing item_id accepted: %v", err)
	}
	if err := builder.ReasoningSummaryTextDelta(
		"", 0, 0, "d",
	).Validate(); err == nil || !strings.Contains(err.Error(), "item_id") {
		t.Fatalf("missing delta item_id accepted: %v", err)
	}
	if err := builder.ReasoningSummaryTextDone(
		"", 0, 0, "t",
	).Validate(); err == nil || !strings.Contains(err.Error(), "item_id") {
		t.Fatalf("missing done item_id accepted: %v", err)
	}
	if err := builder.ReasoningSummaryPartDone(
		"", 0, 0, SummaryTextPart{Type: "summary_text", Text: "s"},
	).Validate(); err == nil || !strings.Contains(err.Error(), "item_id") {
		t.Fatalf("missing part-done item_id accepted: %v", err)
	}
	if err := builder.FunctionArgumentsDelta(
		"", 0, "{",
	).Validate(); err == nil || !strings.Contains(err.Error(), "item_id") {
		t.Fatalf("missing arguments delta item_id accepted: %v", err)
	}
	// A done event carries model-generated arguments preserved byte-exact:
	// any string is legal on the wire (review-z commit 2).
	if err := builder.FunctionArgumentsDone(
		"fc_1", 0, "not json",
	).Validate(); err != nil {
		t.Fatalf("invalid final arguments rejected: %v", err)
	}
	// Negative indexes.
	if err := builder.ContentPartAdded(
		"msg_1", -1, 0, &StreamOutputTextPart{Type: "output_text", Text: "x", Annotations: []Annotation{}},
	).Validate(); err == nil {
		t.Fatal("negative output index accepted")
	}
}

// TestEventValidateBranchCoverage exercises the remaining rejection branches
// of the event Validate methods: negative indexes, nil payloads, and
// required logprobs arrays.
func TestEventValidateBranchCoverage(t *testing.T) {
	builder := &EventBuilder{}

	// Terminal events with mismatched envelope status.
	okEnvelope := Response{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "completed",
		Model: "m", Output: []OutputItem{},
	}
	if err := builder.Completed(failedResponse(okEnvelope)).Validate(); err == nil {
		t.Fatal("completed terminal accepted a failed envelope")
	}
	if err := builder.Failed(okEnvelope).Validate(); err == nil {
		t.Fatal("failed terminal accepted a completed envelope")
	}
	if err := builder.Incomplete(okEnvelope).Validate(); err == nil {
		t.Fatal("incomplete terminal accepted a completed envelope")
	}

	// output_item events: negative output index and nil item.
	if err := builder.OutputItemAdded(-1, &OutputMessage{
		ID: "m", Type: "message", Role: "assistant", Status: ItemInProgress, Content: OutputContentParts{},
	}).Validate(); err == nil {
		t.Fatal("negative output index accepted")
	}
	added := builder.OutputItemAdded(0, &OutputMessage{
		ID: "m", Type: "message", Role: "assistant", Status: ItemInProgress, Content: OutputContentParts{},
	})
	added.Item = nil
	if err := added.Validate(); err == nil {
		t.Fatal("nil item accepted")
	}

	// text/refusal deltas and done events: missing item_id or nil logprobs.
	if err := builder.TextDelta("", 0, 0, "d").Validate(); err == nil {
		t.Fatal("item_id-less text delta accepted")
	}
	if err := builder.TextDone("", 0, 0, "t").Validate(); err == nil {
		t.Fatal("item_id-less text done accepted")
	}
	delta := builder.TextDelta("m", 0, 0, "d")
	delta.Logprobs = nil
	if err := delta.Validate(); err == nil {
		t.Fatal("logprobs-less text delta accepted")
	}
	done := builder.TextDone("m", 0, 0, "t")
	done.Logprobs = nil
	if err := done.Validate(); err == nil {
		t.Fatal("logprobs-less text done accepted")
	}
	if err := builder.RefusalDelta("", 0, 0, "d").Validate(); err == nil {
		t.Fatal("item_id-less refusal delta accepted")
	}
	if err := builder.RefusalDone("", 0, 0, "r").Validate(); err == nil {
		t.Fatal("item_id-less refusal done accepted")
	}

	// content_part events: nil part, negative indexes, bad part type.
	if err := builder.ContentPartAdded("m", 0, -1, &StreamOutputTextPart{
		Type: "output_text", Text: "x", Annotations: []Annotation{},
	}).Validate(); err == nil {
		t.Fatal("negative content index accepted")
	}
	partAdded := builder.ContentPartAdded("m", 0, 0, &StreamOutputTextPart{
		Type: "output_text", Text: "x", Annotations: []Annotation{},
	})
	partAdded.Part = nil
	if err := partAdded.Validate(); err == nil {
		t.Fatal("nil part accepted")
	}
	if err := (&StreamOutputTextPart{Type: "bogus", Text: "x", Annotations: []Annotation{}}).Validate(); err == nil {
		t.Fatal("bad stream part type accepted")
	}
	if err := (&StreamOutputTextPart{Type: "output_text", Text: "x"}).Validate(); err == nil {
		t.Fatal("annotations-less stream part accepted")
	}
	if err := (&StreamRefusalPart{Type: "bogus", Refusal: "r"}).Validate(); err == nil {
		t.Fatal("bad stream refusal type accepted")
	}
	if err := builder.ContentPartDone("m", 0, 0, &StreamRefusalPart{Type: "refusal", Refusal: "r"}).Validate(); err != nil {
		t.Fatal(err)
	}

	// Error event requires both code and message.
	if err := builder.Error("", "m", "p").Validate(); err == nil {
		t.Fatal("code-less error event accepted")
	}
	if err := builder.Error("c", "", "p").Validate(); err == nil {
		t.Fatal("message-less error event accepted")
	}
}

// TestDecodeStreamContentPartUnsupported proves the stream content part
// dispatcher reports unknown part types with the typed unsupported error.
func TestDecodeStreamContentPartUnsupported(t *testing.T) {
	_, err := decodeStreamContentPart([]byte(`{"type":"output_audio"}`))
	if err == nil {
		t.Fatal("unknown part type accepted")
	}
	var unsupported *wire.UnsupportedTypeError
	if !errors.As(err, &unsupported) || unsupported.Type != "output_audio" {
		t.Fatalf("err = %v, want unsupported output_audio", err)
	}
}

// TestEventValidateEnvelopeAndItemBranches covers the remaining rejection
// branches: invalid envelopes and items nested inside events.
func TestEventValidateEnvelopeAndItemBranches(t *testing.T) {
	builder := &EventBuilder{}

	// An envelope missing its id fails the created/in_progress terminals.
	bad := Response{Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m", Output: []OutputItem{}}
	if err := builder.Created(bad).Validate(); err == nil {
		t.Fatal("id-less envelope accepted in created event")
	}
	if err := builder.InProgress(bad).Validate(); err == nil {
		t.Fatal("id-less envelope accepted in in_progress event")
	}

	// An invalid item inside output_item.added fails the event.
	itemAdded := builder.OutputItemAdded(0, &OutputMessage{
		ID: "m", Type: "message", Role: "assistant", Status: ItemCompleted, Content: nil,
	})
	if err := itemAdded.Validate(); err == nil {
		t.Fatal("invalid item accepted in output_item.added")
	}

	// Reasoning summary parts with a wrong part type.
	if err := builder.ReasoningSummaryPartAdded(
		"rs_1", 0, 0, SummaryTextPart{Type: "bogus", Text: "s"},
	).Validate(); err == nil {
		t.Fatal("bad summary part type accepted")
	}
	if err := builder.ReasoningSummaryPartDone(
		"rs_1", 0, 0, SummaryTextPart{Type: "bogus", Text: "s"},
	).Validate(); err == nil {
		t.Fatal("bad summary done part type accepted")
	}
	// A part-done with an invalid status.
	if err := builder.ReasoningSummaryPartDone(
		"rs_1", 0, 0, SummaryTextPart{Type: "summary_text", Text: "s"},
	).Validate(); err != nil {
		t.Fatal(err)
	}
	partDone := builder.ReasoningSummaryPartDone(
		"rs_1", 0, 0, SummaryTextPart{Type: "summary_text", Text: "s"},
	)
	partDone.Status = "bogus"
	if err := partDone.Validate(); err == nil {
		t.Fatal("bad summary status accepted")
	}
	// A part-done with a negative summary index.
	neg := builder.ReasoningSummaryPartDone(
		"rs_1", 0, -1, SummaryTextPart{Type: "summary_text", Text: "s"},
	)
	if err := neg.Validate(); err == nil {
		t.Fatal("negative summary index accepted")
	}

	// Refusal done with a negative content index.
	refusal := builder.RefusalDone("m", 0, -1, "r")
	if err := refusal.Validate(); err == nil {
		t.Fatal("negative refusal content index accepted")
	}
}

// TestEventBaseValidationBranches covers the shared base validation error
// branches (wrong type tag, negative sequence number) on representative
// events of each family.
func TestEventBaseValidationBranches(t *testing.T) {
	builder := &EventBuilder{}
	envelope := Response{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "completed",
		Model: "m", Output: []OutputItem{},
	}

	// Wrong type tag on the base fails every event family.
	created := builder.Created(envelope)
	created.Type = "response.bogus"
	if err := created.Validate(); err == nil {
		t.Fatal("wrong type tag accepted")
	}
	added := builder.OutputItemAdded(0, &OutputMessage{
		ID: "m", Type: "message", Role: "assistant", Status: ItemInProgress, Content: OutputContentParts{},
	})
	added.Type = "response.bogus"
	if err := added.Validate(); err == nil {
		t.Fatal("wrong type tag accepted on output_item.added")
	}
	delta := builder.TextDelta("m", 0, 0, "d")
	delta.Type = "response.bogus"
	if err := delta.Validate(); err == nil {
		t.Fatal("wrong type tag accepted on text delta")
	}
	errorEvent := builder.Error("c", "m", "p")
	errorEvent.Type = "response.bogus"
	if err := errorEvent.Validate(); err == nil {
		t.Fatal("wrong type tag accepted on error event")
	}

	// A negative sequence number fails every event family.
	neg := EventBase{Type: "response.created", SequenceNumber: -1}
	createdNeg := CreatedEvent{EventBase: neg, Response: envelope}
	if err := createdNeg.Validate(); err == nil {
		t.Fatal("negative sequence accepted")
	}
}

// TestEventValidateRemainingBranches covers the last uncovered rejection
// branches: the done-family events' payload checks, the in_progress base
// error, and the stream content part dispatcher's refusal arm.
func TestEventValidateRemainingBranches(t *testing.T) {
	builder := &EventBuilder{}

	envelope := Response{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress",
		Model: "m", Output: []OutputItem{},
	}
	progress := builder.InProgress(envelope)
	progress.Type = "response.bogus"
	if err := progress.Validate(); err == nil {
		t.Fatal("wrong type tag accepted on in_progress")
	}

	// output_item.done payload checks.
	done := builder.OutputItemDone(0, &OutputMessage{
		ID: "m", Type: "message", Role: "assistant", Status: ItemCompleted, Content: OutputContentParts{},
	})
	done.OutputIndex = -1
	if err := done.Validate(); err == nil {
		t.Fatal("negative output index accepted on done")
	}
	done.OutputIndex = 0
	done.Item = nil
	if err := done.Validate(); err == nil {
		t.Fatal("nil item accepted on done")
	}
	done.Item = &OutputMessage{
		ID: "m", Type: "message", Role: "assistant", Status: ItemCompleted, Content: nil,
	}
	if err := done.Validate(); err == nil {
		t.Fatal("invalid item accepted on done")
	}

	// content_part.done payload checks.
	partDoneEvent := builder.ContentPartDone("m", 0, 0, &StreamRefusalPart{Type: "refusal", Refusal: "r"})
	partDoneEvent.ContentIndex = -1
	if err := partDoneEvent.Validate(); err == nil {
		t.Fatal("negative content index accepted on part done")
	}
	partDoneEvent.ContentIndex = 0
	partDoneEvent.Part = nil
	if err := partDoneEvent.Validate(); err == nil {
		t.Fatal("nil part accepted on part done")
	}

	// function_call_arguments.done payload checks.
	argsDone := builder.FunctionArgumentsDone("fc_1", 0, "{}")
	argsDone.OutputIndex = -1
	if err := argsDone.Validate(); err == nil {
		t.Fatal("negative index accepted on arguments done")
	}

	// The stream content part dispatcher's refusal arm.
	part, err := decodeStreamContentPart([]byte(`{"type":"refusal","refusal":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := part.Validate(); err != nil {
		t.Fatal(err)
	}
}
