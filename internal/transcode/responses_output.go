package transcode

// The OpenAI Responses wire definitions live in the pinned wire package
// wire/openairesponses (see contracts.lock.json); this file re-exports them
// under the package's historical names so consumers compile unchanged. New
// code should prefer the wire package names.

import (
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// ResponsesAnnotation is one annotation on an output_text part.
type ResponsesAnnotation = openairesponses.Annotation

// ResponsesTextLogprob is one token log-probability entry.
type ResponsesTextLogprob = openairesponses.TextLogprob

// ResponsesOutputContentPart is one content part of an output message.
type ResponsesOutputContentPart = openairesponses.OutputContentPart

// ResponsesOutputText is an output_text content part.
type ResponsesOutputText = openairesponses.OutputText

// ResponsesOutputRefusal is a refusal content part of an output message.
type ResponsesOutputRefusal = openairesponses.OutputRefusal

// ResponsesOutputContentParts is the content array of an output message.
type ResponsesOutputContentParts = openairesponses.OutputContentParts

// ResponsesOutputItem is one tagged variant of the output item union.
type ResponsesOutputItem = openairesponses.OutputItem

// ResponsesOutputMessage is an assistant output message.
type ResponsesOutputMessage = openairesponses.OutputMessage

// ResponsesFunctionCallOutputItem is an output function_call item.
type ResponsesFunctionCallOutputItem = openairesponses.FunctionCallOutputItem

// ResponsesFunctionCallOutputResultItem is an output function_call_output
// item.
type ResponsesFunctionCallOutputResultItem = openairesponses.FunctionCallOutputResultItem

// ResponsesReasoningOutputItem is an output reasoning item.
type ResponsesReasoningOutputItem = openairesponses.ReasoningOutputItem
