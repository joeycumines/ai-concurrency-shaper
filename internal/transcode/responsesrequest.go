package transcode

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// Direct source-to-IR-to-target conversion. There must not be an
// implementation that chains wire serializers:
//
//	Messages JSON -> Responses JSON -> Chat JSON
//
// because every intermediate serializer loses source semantics and invents
// fields that were never supplied. Each source decodes exactly once into the
// canonical IR and each target renders directly from that IR.

// probeUnsupportedResponsesFields reports the first recognized-but-unsupported
// Responses request field, or "" when none is present. These fields are
// background and conversation-state controls the transcoder deliberately
// does not implement; their presence is a typed unsupported-feature error,
// never a silent drop. (include and prompt_cache_key are NOT probed here:
// include is a best-effort response-format preference and prompt_cache_key
// is the responses_controls loss decision, both handled in
// DecodeResponsesRequest.)
func probeUnsupportedResponsesFields(data []byte) (string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	for _, name := range []string{
		"prompt",
		"background",
		"max_tool_calls",
		"safety_identifier",
		"status",
	} {
		if _, ok := probe[name]; ok {
			return name, nil
		}
	}
	return "", nil
}

// DecodeResponsesRequest decodes a Responses request body into the canonical
// IR and captures the request echo required to reconstruct the client
// response envelope. The body is decoded through the pinned wire contract
// (wire/openairesponses.Request): the stream field is presence-aware so the
// handler can apply the documented precedence over the Accept header, and
// tool entries must carry an explicit strict (a missing or null strict is a
// malformed request, rejected client-dialect before any upstream request).
func DecodeResponsesRequest(
	body []byte,
	policy LossPolicy,
) (DecodeResult, *ResponsesRequestEcho, error) {
	var request openairesponses.Request
	if err := wire.Decode(body, &request); err != nil {
		// A valid-but-unsupported feature reported by a wire union dispatcher
		// (e.g. an input_audio part) surfaces as UnsupportedFeatureError —
		// client-dialect local — never as a malformed-request error. All six
		// malformed-JSON categories surface as typed wire.DecodeError.
		return DecodeResult{}, nil, fmt.Errorf(
			"responses request: %w",
			wireUnsupportedToFeature(err),
		)
	}
	// Recognized-but-unsupported request fields (background and
	// conversation-state controls) are a typed unsupported-feature error,
	// never a silent drop. The probe runs after the strict decode so a
	// malformed document is reported as malformed, not as an unsupported
	// field.
	if name, err := probeUnsupportedResponsesFields(body); err != nil {
		return DecodeResult{}, nil, err
	} else if name != "" {
		return DecodeResult{}, nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     name,
			Feature:  name,
		}
	}

	var result DecodeResult

	// Client-controlled request fields that a chat upstream cannot honor
	// are recorded, never silently dropped:
	//
	//  - include requests extra response fields (e.g.
	//    reasoning.encrypted_content). The chat upstream cannot produce
	//    them; the rendered response carries what the source actually
	//    provided (the reasoning content mapping under the
	//    provider_reasoning_text capability). Best-effort by nature.
	//  - client_metadata is pure client telemetry with no upstream
	//    semantics.
	//  - prompt_cache_key is a conversation-cache control; the
	//    responses_controls loss decision applies (approved by the CLI
	//    defaults, rejectable by policy).
	if len(request.Include) > 0 {
		if err := result.Report.Note(
			FeatureResponsesControls,
			"include",
			"the include response-format preference cannot be honored by a chat upstream; the rendered response carries what the source provides",
		); err != nil {
			return DecodeResult{}, nil, err
		}
	}
	if len(request.ClientMetadata) > 0 {
		if err := result.Report.Note(
			FeatureResponsesControls,
			"client_metadata",
			"client_metadata is client telemetry with no upstream semantics; it is not forwarded",
		); err != nil {
			return DecodeResult{}, nil, err
		}
	}
	if request.PromptCacheKey != "" {
		if err := result.Report.Lose(
			policy,
			FeatureResponsesControls,
			"prompt_cache_key",
			"the prompt cache key cannot be reproduced by a chat upstream",
		); err != nil {
			return DecodeResult{}, nil, err
		}
	}

	// Tool strictness is required on the wire for function tools: reject
	// before any value is consumed so a strict-less tool can never reach the
	// converters.
	for i, tool := range request.Tools {
		if err := tool.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses tools[%d]: %w", i, err)
		}
	}

	if request.Model == "" {
		return DecodeResult{}, nil, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "model",
			Message: "responses request model is empty",
		}
	}
	if request.Truncation != nil {
		switch *request.Truncation {
		case "auto", "disabled":
		default:
			return DecodeResult{}, nil, fmt.Errorf(
				"invalid truncation %q",
				*request.Truncation,
			)
		}
	}
	if request.TopLogprobs != nil && *request.TopLogprobs < 0 {
		return DecodeResult{}, nil, errors.New("negative top_logprobs")
	}

	result.Request.ClientModel = request.Model
	result.Request.Stream = request.Stream.Value
	// An explicitly present body stream field (true or false, never null) is
	// recorded as present so the handler can apply the documented precedence
	// over the Accept header. An absent or null field
	// is treated as absent, consistent with the envelope's other pointer
	// fields.
	if request.Stream.Present && !request.Stream.Null {
		result.StreamSet = true
		result.Request.Stream = request.Stream.Value
	}
	result.Request.MaxOutputTokens = request.MaxOutputTokens
	result.Request.ParallelTools = request.ParallelToolCalls
	result.Request.Temperature = request.Temperature
	result.Request.TopP = request.TopP
	result.Request.Metadata = request.Metadata

	// Tools. Function tools are supported; namespace tools are flattened
	// into their nested function tools (the grouping is client-side
	// structure the chat target cannot express; the functions themselves
	// are fully portable); built-in tools (web_search, file_search,
	// code_interpreter, computer_use, ...) are the builtin_tools loss
	// decision — approved, they are dropped; rejected, the request fails.
	// The raw parameters schema is validated as exactly one JSON object at
	// this boundary; its bytes are preserved, never decoded and remarshaled
	// through a map, so large integers, decimals, and exponents survive
	// byte-exact.
	//
	// Flattening records every namespace child in a per-exchange map whose
	// flat name is the bare child name while it is unique; a child that
	// collides with a plain function name or an earlier namespace child is
	// qualified deterministically. The map is the ONLY reverse lookup: the
	// response renderer restores the namespace qualifier from it and never
	// parses a separator out of a name.
	reserved := plainToolNames(request.Tools)
	names := &ToolNames{FlatToRef: map[string]ToolNameRef{}, RefToFlat: map[ToolNameRef]string{}}
	for i, tool := range request.Tools {
		switch tool.Type {
		case "namespace":
			flattened, err := flattenNamespaceTool(tool, &result.Report, policy)
			if err != nil {
				return DecodeResult{}, nil, fmt.Errorf(
					"responses tools[%d]: %w",
					i,
					err,
				)
			}
			for j, child := range flattened {
				nested := child.Tool
				if len(nested.Parameters) > 0 {
					if _, err := decodeJSONObject(string(nested.Parameters)); err != nil {
						return DecodeResult{}, nil, fmt.Errorf(
							"responses tools[%d] namespace[%d] parameters: %w",
							i,
							j,
							err,
						)
					}
				}
				result.Request.Tools = append(result.Request.Tools, CanonicalTool{
					Name:        chooseToolFlatName(names, reserved, child.Namespace, nested.Name),
					Description: nested.Description,
					JSONSchema:  nested.Parameters,
					Strict:      fieldBoolPtr(nested.Strict),
				})
			}
		case "function":
			if len(tool.Parameters) > 0 {
				if _, err := decodeJSONObject(string(tool.Parameters)); err != nil {
					return DecodeResult{}, nil, fmt.Errorf(
						"responses tools[%d] parameters: %w",
						i,
						err,
					)
				}
			}
			result.Request.Tools = append(result.Request.Tools, CanonicalTool{
				Name:        tool.Name,
				Description: tool.Description,
				JSONSchema:  tool.Parameters,
				Strict:      fieldBoolPtr(tool.Strict),
			})
		default:
			// Built-in and unknown tool types: the builtin_tools loss
			// decision (approved by the CLI defaults, rejectable by
			// policy).
			if err := result.Report.Lose(
				policy,
				FeatureBuiltinTools,
				fmt.Sprintf("tools[%d]", i),
				fmt.Sprintf(
					"the %q built-in tool cannot be reproduced in a chat request",
					tool.Type,
				),
			); err != nil {
				return DecodeResult{}, nil, err
			}
		}
	}
	if len(names.RefToFlat) > 0 {
		result.ToolNames = names
	}

	// Tool choice, reconciled against the tools that survive conversion: an
	// approved built-in drop can leave "required" or a named function choice
	// pointing at tools that no longer exist, which would render an invalid
	// upstream request.
	if request.ToolChoice != nil {
		choice, err := canonicalizeResponsesToolChoice(*request.ToolChoice)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		choice, err = reconcileToolChoice(
			choice,
			len(request.Tools),
			&result.Request,
			&result.Report,
		)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.ToolChoice = choice
	}

	// Structured output from text.format.
	if request.Text != nil && request.Text.Format != nil {
		format := request.Text.Format
		switch format.Type {
		case "", "text":
			// No structured output requested.
		case "json_schema":
			if len(format.Schema) == 0 {
				return DecodeResult{}, nil, errors.New(
					"responses text.format json_schema has no schema",
				)
			}
			if _, err := decodeJSONObject(string(format.Schema)); err != nil {
				return DecodeResult{}, nil, fmt.Errorf(
					"responses text.format schema: %w",
					err,
				)
			}
			strict := format.Strict != nil && *format.Strict
			result.Request.StructuredOutput = &CanonicalStructuredOutput{
				Name:        format.Name,
				Description: format.Description,
				Schema:      format.Schema,
				Strict:      strict,
			}
		case "json_object":
			if err := result.Report.Lose(
				policy,
				FeatureStructuredOutput,
				"text.format.type",
				"json_object mode cannot be reproduced in every target",
			); err != nil {
				return DecodeResult{}, nil, err
			}
		default:
			return DecodeResult{}, nil, &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     "text.format.type",
				Feature:  format.Type,
			}
		}
	}

	// Turns from instructions and input.
	if request.Instructions != nil {
		result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
			Role:  CanonicalSystem,
			Parts: []CanonicalPart{CanonicalText{Text: *request.Instructions}},
		})
	}
	if request.Input != nil {
		if err := request.Input.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses input: %w", err)
		}
		turns, err := responsesInputToTurns(
			*request.Input,
			policy,
			&result.Report,
			&result.Request.Artifacts,
		)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.Turns = append(result.Request.Turns, turns...)
	}

	// Echo for the client envelope reconstruction. The client's
	// instructions is a plain string; the response envelope echo renders
	// the string arm of the pinned instructions union. The effective values
	// (parallel_tool_calls, temperature, top_p, tool_choice) carry the
	// pinned API defaults — parallel_tool_calls defaults to true,
	// temperature to 1.0, top_p to 1.0, tool_choice to "auto" — so the
	// renderer can emit a complete response envelope without guessing later.
	echo := &ResponsesRequestEcho{}
	if request.Instructions != nil {
		echo.Instructions = &ResponsesInput{Text: request.Instructions}
	}
	echo.MaxOutputTokens = request.MaxOutputTokens
	echo.ParallelToolCalls = defaultBoolValue(request.ParallelToolCalls, true)
	echo.PreviousResponseID = request.PreviousResponseID
	echo.Store = request.Store
	echo.Temperature = defaultFloatValue(request.Temperature, 1.0)
	echo.TopP = defaultFloatValue(request.TopP, 1.0)
	echo.Truncation = request.Truncation
	echo.User = request.User
	echo.Metadata = request.Metadata
	echo.Tools = request.Tools
	echo.ToolChoice = defaultToolChoice(request.ToolChoice)
	echo.Reasoning = request.Reasoning
	echo.Text = request.Text
	echo.ServiceTier = request.ServiceTier
	echo.TopLogprobs = request.TopLogprobs

	// The echo re-marshals into EVERY generated envelope frame (created,
	// in_progress, terminal) and the non-streaming response envelope, at up
	// to 6x JSON escaping. Bound its serialized size at decode as a
	// fail-closed resource limit: an unbounded echo would defeat the
	// generated-frame derivation (limits.go maxStreamEchoBytes).
	if err := checkEchoSize(echo); err != nil {
		return DecodeResult{}, nil, err
	}

	return result, echo, nil
}

// checkEchoSize rejects a request echo whose rendered size exceeds
// maxStreamEchoBytes. The echo is measured the way the envelope renders it:
// one json.Marshal of the ENTIRE ResponsesRequestEcho with the same
// escaping the envelope render applies: per-member raw lengths under-count
// escaping-heavy strings and omit rendered members such as user,
// previous_response_id, and service_tier — the measurement must be the
// serialized whole, not an inventory of selected members.
func checkEchoSize(echo *ResponsesRequestEcho) error {
	serialized, err := json.Marshal(echo)
	if err != nil {
		return fmt.Errorf("responses request echo: %w", err)
	}
	if len(serialized) > maxStreamEchoBytes {
		return fmt.Errorf("%w: the rendered responses request echo of %d bytes exceeds the %d byte echo bound; the request echo renders into every response envelope and is bounded as a resource limit",
			errEchoTooLarge, len(serialized), maxStreamEchoBytes)
	}
	return nil
}

// loseInputPhase applies the input-message phase decision at decode time.
// The canonical IR cannot carry a phase and no target dialect can reproduce
// it, so its presence is a loss/reject decision regardless of the target
func loseInputPhase(
	policy LossPolicy,
	report *ConversionReport,
	phase string,
	index int,
) error {
	if phase == "" {
		return nil
	}
	return report.Lose(
		policy,
		FeatureOutputPhase,
		fmt.Sprintf("input[%d].phase", index),
		"the input message phase cannot be reproduced in any target dialect",
	)
}

// fieldBoolPtr converts a presence-aware wire bool to a plain pointer:
// absent or explicit null becomes nil, everything else becomes the value.
func fieldBoolPtr(field wire.Field[bool]) *bool {
	if !field.Present || field.Null {
		return nil
	}
	value := field.Value
	return &value
}

// defaultBoolValue applies the pinned API default when the field is absent.
func defaultBoolValue(value *bool, def bool) bool {
	if value == nil {
		return def
	}
	return *value
}

// defaultFloatValue applies the pinned API default when the field is absent.
func defaultFloatValue(value *float64, def float64) float64 {
	if value == nil {
		return def
	}
	return *value
}

// defaultToolChoice applies the pinned tool_choice default ("auto") when the
// request carried no explicit choice, so the response envelope echo always
// carries a valid value.
func defaultToolChoice(choice *ResponsesToolChoice) ResponsesToolChoice {
	if choice != nil {
		return *choice
	}
	auto := "auto"
	return ResponsesToolChoice{Str: &auto}
}

// rawMessage marshals a map into a raw JSON object. The map comes from
// decodeJSONObject, so every value is valid JSON by construction and
// marshaling cannot fail — but the conversion runs on the stream-copy
// goroutine in the composed directions, where an escaping panic kills the
// process, so the impossible failure is surfaced as a typed error instead of
// a panic.
func rawMessage(value map[string]json.RawMessage) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("tool arguments object: %w", err)
	}
	return raw, nil
}
