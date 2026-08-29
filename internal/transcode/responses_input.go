package transcode

// The OpenAI Responses wire definitions live in the pinned wire package
// wire/openairesponses (see contracts.lock.json); this file re-exports them
// under the package's historical names so consumers compile unchanged. New
// code should prefer the wire package names.

import (
	"errors"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// ResponsesInputRole is the role of an easy input message.
type ResponsesInputRole = openairesponses.InputRole

// ResponsesInputRole values.
const (
	ResponsesInputRoleUser      = openairesponses.InputRoleUser
	ResponsesInputRoleAssistant = openairesponses.InputRoleAssistant
	ResponsesInputRoleSystem    = openairesponses.InputRoleSystem
	ResponsesInputRoleDeveloper = openairesponses.InputRoleDeveloper
)

// ResponsesItemStatus is the lifecycle status of an input or output item.
type ResponsesItemStatus = openairesponses.ItemStatus

// ResponsesItemStatus values.
const (
	ResponsesItemInProgress = openairesponses.ItemInProgress
	ResponsesItemCompleted  = openairesponses.ItemCompleted
	ResponsesItemIncomplete = openairesponses.ItemIncomplete
)

// ResponsesInput models the request-level input union: either a plain string
// or a list of input items. Exactly one arm is selected.
type ResponsesInput = openairesponses.Input

// ResponsesInputContentPart is one content part of an input message.
type ResponsesInputContentPart = openairesponses.InputContentPart

// ResponsesInputText is an input_text content part.
type ResponsesInputText = openairesponses.InputText

// ResponsesInputImage is an input_image content part.
type ResponsesInputImage = openairesponses.InputImage

// ResponsesInputFile is an input_file content part.
type ResponsesInputFile = openairesponses.InputFile

// ResponsesInputContentParts is the array arm of message content.
type ResponsesInputContentParts = openairesponses.InputContentParts

// ResponsesInputMessageContent is a string-or-parts union for message
// content.
type ResponsesInputMessageContent = openairesponses.InputMessageContent

// ResponsesInputItem is one tagged variant of the request input item union.
type ResponsesInputItem = openairesponses.InputItem

// ResponsesEasyInputMessage is an easy input message without an ID.
type ResponsesEasyInputMessage = openairesponses.EasyInputMessage

// ResponsesPreviousOutputMessage is a previous model output message reused
// as input.
type ResponsesPreviousOutputMessage = openairesponses.PreviousOutputMessage

// ResponsesFunctionCallInput is a function_call input item.
type ResponsesFunctionCallInput = openairesponses.FunctionCallInput

// ResponsesFunctionOutput is the output arm of a function_call_output item.
type ResponsesFunctionOutput = openairesponses.FunctionOutput

// ResponsesFunctionCallOutputInput is a function_call_output input item.
type ResponsesFunctionCallOutputInput = openairesponses.FunctionCallOutputInput

// ResponsesReasoningSummary is one summary entry of a reasoning item.
type ResponsesReasoningSummary = openairesponses.ReasoningSummary

// ResponsesReasoningText is one reasoning_text content entry.
type ResponsesReasoningText = openairesponses.ReasoningText

// ResponsesReasoningInput is a reasoning input item.
type ResponsesReasoningInput = openairesponses.ReasoningInput

// ResponsesItemReferenceInput is an item_reference input item.
type ResponsesItemReferenceInput = openairesponses.ItemReferenceInput

// ResponsesInputItems is the item-list arm of the request input union.
type ResponsesInputItems = openairesponses.InputItems

// wireUnsupportedToFeature translates a wire-layer unsupported-type report
// into the transcode unsupported-feature error. The wire layer is valid;
// the feature is outside the supported subset — never corrupt wire.
func wireUnsupportedToFeature(err error) error {
	var unsupported *wire.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		return err
	}
	return &UnsupportedFeatureError{
		Protocol: unsupported.Protocol,
		Path:     unsupported.Path,
		Feature:  unsupported.Type,
	}
}
