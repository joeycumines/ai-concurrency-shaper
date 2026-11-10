package transcode

import (
	"fmt"
)

// openResponsesItem tracks one open Responses output item during Chat→
// Responses streaming.
type openResponsesItem struct {
	outputIndex int64
	item        ResponsesOutputItem

	// openPartIndex is the content index of the currently open content part,
	// or -1 when no part is open.
	openPartIndex int64

	// sealedParts records the content indexes already closed inline at a
	// part-type transition (closeOpenPart), so finish() never emits a
	// duplicate value-done/part-done for them.
	sealedParts map[int64]struct{}
}

// isMessage reports whether the open item is a message item.
func (o *openResponsesItem) isMessage() bool {
	_, ok := o.item.(*ResponsesOutputMessage)
	return ok
}

// responsesOutputItemID returns the item id of a typed output item, or ""
// for item types that never appear on the stream.
func responsesOutputItemID(item ResponsesOutputItem) string {
	switch value := item.(type) {
	case *ResponsesOutputMessage:
		return value.ID
	case *ResponsesFunctionCallOutputItem:
		return value.ID
	case *ResponsesReasoningOutputItem:
		return value.ID
	default:
		return ""
	}
}

// itemTypeName returns a stable type name for an output item.
func itemTypeName(item ResponsesOutputItem) string {
	switch item.(type) {
	case *ResponsesOutputMessage:
		return "message"
	case *ResponsesFunctionCallOutputItem:
		return "function_call"
	case *ResponsesReasoningOutputItem:
		return "reasoning"
	default:
		return fmt.Sprintf("%T", item)
	}
}

// outputPartTypeName returns a stable type name for a completed message
// content part (the envelope and output_item.done shapes).
func outputPartTypeName(part ResponsesOutputContentPart) string {
	switch part.(type) {
	case *ResponsesOutputText:
		return "output_text"
	case *ResponsesOutputRefusal:
		return "refusal"
	default:
		return fmt.Sprintf("%T", part)
	}
}

// partTypeName returns a stable type name for a stream content part.
func partTypeName(part ResponsesStreamContentPart) string {
	switch part.(type) {
	case *ResponsesStreamOutputTextPart:
		return "output_text"
	case *ResponsesStreamRefusalPart:
		return "refusal"
	default:
		return fmt.Sprintf("%T", part)
	}
}
