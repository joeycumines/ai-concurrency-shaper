package transcode

import (
	"errors"
	"fmt"
)

// canonicalizeResponsesToolChoice maps the Responses tool_choice union to the
// canonical form. Unknown string values are rejected rather than silently
// defaulted to auto.
func canonicalizeResponsesToolChoice(
	choice ResponsesToolChoice,
) (*CanonicalToolChoice, error) {
	if err := choice.Validate(); err != nil {
		return nil, fmt.Errorf("responses tool_choice: %w", err)
	}
	if choice.Named != nil {
		return &CanonicalToolChoice{
			Mode: "named",
			Name: choice.Named.Name,
		}, nil
	}
	// "required" is part of the official Responses contract (and the Chat
	// dialect renders it natively), so it is portable as-is.
	return &CanonicalToolChoice{Mode: *choice.Str}, nil
}

// canonicalizeAnthropicToolChoice maps the Anthropic tool_choice union to the
// canonical form. disable_parallel_tool_use is the Anthropic spelling of the
// canonical parallel-tools setting, so it is CARRIED (not decided here): the
// Messages target renders it natively, and a target that cannot reproduce it
// takes the parallel_tool_calls loss at its own render stage. Deciding it at
// decode would destroy a value another target carries and, because
// parallel_tool_calls is not a default loss, would refuse a valid native
// request by default.
func canonicalizeAnthropicToolChoice(
	choice AnthropicToolChoice,
	request *CanonicalRequest,
) (*CanonicalToolChoice, error) {
	if choice.DisableParallelToolUse != nil && *choice.DisableParallelToolUse {
		disabled := false
		request.ParallelTools = &disabled
	}
	switch choice.Type {
	case "auto", "none", "any":
		// A name is only meaningful with type "tool": carrying one on
		// another mode is a contradictory union arm, rejected instead of
		// silently discarded.
		if choice.Name != "" {
			return nil, fmt.Errorf(
				"messages tool_choice type %s must not carry a name",
				choice.Type,
			)
		}
		mode := "auto"
		switch choice.Type {
		case "none":
			mode = "none"
		case "any":
			mode = "required"
		}
		return &CanonicalToolChoice{Mode: mode}, nil
	case "tool":
		if choice.Name == "" {
			return nil, errors.New("messages tool_choice type tool requires name")
		}
		return &CanonicalToolChoice{Mode: "named", Name: choice.Name}, nil
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "messages",
			Path:     "tool_choice.type",
			Feature:  choice.Type,
		}
	}
}

// reconcileToolChoice checks a canonicalized tool choice against the tools
// that actually survive conversion. A named choice must
// reference a surviving tool under every policy — a dangling reference is
// malformed no matter how many tools the client sent. A mode choice is only
// reconciled when the client DID send tools and the converter dropped them
// all (built-in-tool or server-tool loss): "required" against zero tools is
// a client-dialect error (the converter would otherwise render an invalid
// upstream request), and "auto" is dropped with an observable note because
// an empty tool list leaves it meaningless. "none" is PRESERVED: it names no
// tool, so it cannot dangle, and "call no tool" is exactly what an empty tool
// list already means — dropping it would change nothing the upstream reads.
// When the client sent no tools at all the choice passes through untouched —
// the upstream judges that incoherence, not the converter
// (TestDecodeResponsesRequestToolChoiceRequired pins it). droppedFeature names
// the key of the loss that removed the tools, so the note is filed under the
// decision that actually happened rather than a fixed guess.
func reconcileToolChoice(
	choice *CanonicalToolChoice,
	requestToolCount int,
	request *CanonicalRequest,
	report *ConversionReport,
	droppedFeature Feature,
) (*CanonicalToolChoice, error) {
	if choice == nil {
		return nil, nil
	}
	if choice.Mode == "named" {
		for _, tool := range request.Tools {
			if tool.Name == choice.Name {
				return choice, nil
			}
		}
		return nil, fmt.Errorf(
			"tool_choice names tool %q which is not among the tools that survive conversion",
			choice.Name,
		)
	}
	if requestToolCount == 0 || len(request.Tools) > 0 {
		return choice, nil
	}
	switch choice.Mode {
	case "required":
		return nil, errors.New(
			"tool_choice required but no portable tools remain after the tool loss",
		)
	case "auto":
		if err := report.Note(
			droppedFeature,
			"tool_choice",
			"tool_choice auto dropped with the last tool (no tools remain to choose among)",
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return choice, nil
}

// canonicalToolChoiceToResponses renders the canonical tool choice into the
// Responses union.
func canonicalToolChoiceToResponses(
	choice CanonicalToolChoice,
) (ResponsesToolChoice, error) {
	switch choice.Mode {
	case "none", "auto", "required":
		mode := choice.Mode
		return ResponsesToolChoice{Str: &mode}, nil
	case "named":
		if choice.Name == "" {
			return ResponsesToolChoice{}, errors.New("named tool choice has no name")
		}
		return ResponsesToolChoice{Named: &ResponsesToolChoiceNamed{
			Type: "function",
			Name: choice.Name,
		}}, nil
	default:
		return ResponsesToolChoice{}, fmt.Errorf(
			"unknown tool choice mode %q",
			choice.Mode,
		)
	}
}

func canonicalToolChoiceToChat(
	choice CanonicalToolChoice,
) (ChatToolChoice, error) {
	switch choice.Mode {
	case "none", "auto", "required":
		mode := choice.Mode
		return ChatToolChoice{Str: &mode}, nil
	case "named":
		if choice.Name == "" {
			return ChatToolChoice{}, errors.New("named tool choice has no name")
		}
		return ChatToolChoice{Struct: &ChatToolChoiceStruct{
			Type: "function",
			Function: &ChatToolChoiceFunction{
				Name: choice.Name,
			},
		}}, nil
	default:
		return ChatToolChoice{}, fmt.Errorf(
			"unknown tool choice mode %q",
			choice.Mode,
		)
	}
}
