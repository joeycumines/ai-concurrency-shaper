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
//
// The setting is a BOOLEAN, not a flag: an explicit false asserts that parallel
// calls ARE allowed, so both values must be carried. Dropping the false case left
// ParallelTools nil, which then bypassed every target's parallel-tools gate.
// The union arm is validated BEFORE the request is mutated, so a rejected type
// leaves no half-applied setting (tests reuse one request across cases).
func canonicalizeAnthropicToolChoice(
	choice AnthropicToolChoice,
	request *CanonicalRequest,
) (*CanonicalToolChoice, error) {
	var parallel *bool
	if choice.DisableParallelToolUse != nil {
		enabled := !*choice.DisableParallelToolUse
		parallel = &enabled
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
		request.ParallelTools = parallel
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
		request.ParallelTools = parallel
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
// malformed no matter how many tools the client sent. A MODE choice is
// reconciled when the client DID send tools and the converter dropped them all
// (built-in-tool or server-tool loss): every mode choice is then dropped with
// the tools, because OpenAI Chat, OpenAI Responses and Anthropic Messages all
// reject tool_choice when no tool is declared ("tool_choice is only allowed
// when tools are specified"). "required" is refused instead: it asserts that a
// call must happen, and no remaining tool can satisfy that. When the client sent
// no tools at all the choice passes through untouched — the upstream judges that
// incoherence, not the converter (TestDecodeResponsesRequestToolChoiceRequired
// pins it). droppedFeature names the key of the loss that removed the tools, so
// the note is filed under the decision that actually happened rather than a
// fixed guess.
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
	if choice.Mode == "required" {
		return nil, errors.New(
			"tool_choice required but no portable tools remain after the tool loss",
		)
	}
	if err := report.Note(
		droppedFeature,
		"tool_choice",
		fmt.Sprintf(
			"tool_choice %s dropped with the last tool (every tool dialect rejects a tool choice when no tool is declared, and no tools remain to choose among)",
			choice.Mode,
		),
	); err != nil {
		return nil, err
	}
	return nil, nil
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
