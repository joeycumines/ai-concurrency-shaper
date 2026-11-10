package transcode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// convertPart converts one Gemini stream part into Responses events.
func (s *geminiResponsesStreamState) convertPart(
	part geminigeneratecontent.Part, partIndex int,
) ([]ResponsesSSEEvent, error) {
	path := fmt.Sprintf("candidates[].content.parts[%d]", partIndex)

	// Unsupported payloads are unrepresentable in the Responses stream. Raw
	// member payloads are read with RawMemberPresent, matching Part.Validate
	// and the non-streaming decode: an explicit JSON null is not a payload.
	switch {
	case part.InlineData != nil, part.FileData != nil,
		geminigeneratecontent.RawMemberPresent(part.ExecutableCode),
		geminigeneratecontent.RawMemberPresent(part.CodeExecutionResult),
		geminigeneratecontent.RawMemberPresent(part.ToolCall),
		geminigeneratecontent.RawMemberPresent(part.ToolResponse),
		geminigeneratecontent.RawMemberPresent(part.AudioTranscription):
		return nil, &UnrepresentableError{
			Protocol: string(UpstreamGemini),
			Path:     path,
			Detail:   "the part payload cannot be represented in the target dialect",
		}
	case part.FunctionResponse != nil:
		return nil, &UnrepresentableError{
			Protocol: string(UpstreamGemini),
			Path:     path,
			Detail:   "a functionResponse part cannot appear in a model response stream",
		}
	}

	// Part modifiers, a signature on a non-call part, and a dropped thought
	// are observable losses, once per stream. Every policy gate the part
	// needs is decided BEFORE any of its payload commits, so a part refused
	// for one loss leaves no other entry, no stored signature, and no events
	// behind. Each loss is recorded only AFTER the payload committed, so a
	// part whose payload is refused (bounds, an invalid function call) leaves
	// none. Signature-only and empty-thought parts commit no payload, yet the
	// metadata and signature they carry are still losses of that exact part:
	// the non-streaming decode records both on every part shape.
	metadataPath, partMetadata := partMetadataPath(part, path)
	thought := part.Thought != nil && *part.Thought
	dropsThought := thought && part.Text != nil && *part.Text != "" &&
		!s.capabilities.ProviderReasoningThinking && !s.capabilities.ProviderReasoningText
	if err := s.decidePartLosses(part, path, metadataPath, partMetadata, dropsThought); err != nil {
		return nil, err
	}

	if part.FunctionCall != nil {
		events, err := s.convertFunctionCall(part, path, metadataPath, partMetadata)
		if err != nil {
			return nil, err
		}
		if err := s.losePartMetadata(partMetadata, metadataPath); err != nil {
			return nil, err
		}
		return events, nil
	}

	if part.Text == nil || (thought && *part.Text == "") {
		// A signature-only part, or an empty thought (the empty-delta short
		// circuit in emitThoughtText applies), carries no payload and emits
		// nothing, and records no reasoning note or loss for an encoding that
		// produced nothing.
		return nil, s.losePartModifiers(part, path, metadataPath, partMetadata)
	}
	if thought {
		// Same capability rules as the non-streaming decode: thought text is
		// provider reasoning; mapped under the capability or an approved
		// loss, each recorded once per stream. The encoding commits first, so
		// a rejected delta leaves no note behind.
		var events []ResponsesSSEEvent
		switch {
		case s.capabilities.ProviderReasoningThinking:
			var err error
			events, err = s.emitThoughtText(*part.Text, path)
			if err != nil {
				return nil, err
			}
		case s.capabilities.ProviderReasoningText:
			var err error
			events, err = s.emitTextDelta(*part.Text)
			if err != nil {
				return nil, err
			}
		default:
			// The dropped thought commits as an approved loss, and its
			// modifiers and signature remain observable losses of the part.
			if err := s.loseThought(path); err != nil {
				return nil, err
			}
			return nil, s.losePartModifiers(part, path, metadataPath, partMetadata)
		}
		// The thought note follows the committed encoding and precedes the
		// part's modifier losses, the order the non-streaming decode records
		// them in.
		if s.capabilities.ProviderReasoningThinking {
			if !s.thoughtThinkingNoted {
				if err := s.report.Note(
					FeatureProviderReasoningText,
					path,
					"provider reasoning mapped to a native thinking block (provider_reasoning_thinking encoding)",
				); err != nil {
					return nil, err
				}
				s.thoughtThinkingNoted = true
			}
		} else if !s.thoughtTextNoted {
			s.thoughtTextNoted = true
			if err := s.report.Note(
				FeatureProviderReasoningText,
				path,
				"provider reasoning text mapped to ordinary text (provider_reasoning_text capability)",
			); err != nil {
				return nil, err
			}
		}
		if err := s.losePartModifiers(part, path, metadataPath, partMetadata); err != nil {
			return nil, err
		}
		return events, nil
	}

	events, err := s.emitTextDelta(*part.Text)
	if err != nil {
		return nil, err
	}
	if err := s.losePartModifiers(part, path, metadataPath, partMetadata); err != nil {
		return nil, err
	}
	return events, nil
}

// partMetadataPath names the first part modifier the non-streaming decode
// reports for the part, in the decode's order, and reports whether any modifier
// is present. The field path makes a refusal or a logged loss name the same
// field on both paths.
func partMetadataPath(part geminigeneratecontent.Part, path string) (string, bool) {
	modifiers := []struct {
		name    string
		present bool
	}{
		{"mediaResolution", geminigeneratecontent.RawMemberPresent(part.MediaResolution)},
		{"partMetadata", geminigeneratecontent.RawMemberPresent(part.PartMetadata)},
		{"videoMetadata", geminigeneratecontent.RawMemberPresent(part.VideoMetadata)},
		{"speechMetadata", geminigeneratecontent.RawMemberPresent(part.SpeechMetadata)},
		{"mediaProcessing", part.MediaProcessing != nil},
	}
	for _, modifier := range modifiers {
		if modifier.present {
			return path + "." + modifier.name, true
		}
	}
	return path, false
}

// decidePartLosses peeks every policy gate the part's losses need — a dropped
// thought, a non-call signature, part modifier metadata — before any of the
// part's payload commits. The losses themselves are recorded afterwards by
// loseThought and losePartModifiers, which then cannot be refused. A function
// call's metadata gate is decided by convertFunctionCall, after the call's wire
// checks, so an upstream fault takes precedence over the local refusal.
func (s *geminiResponsesStreamState) decidePartLosses(
	part geminigeneratecontent.Part, path, metadataPath string, partMetadata, dropsThought bool,
) error {
	if dropsThought {
		if err := s.policy.refusal(FeatureProviderReasoningText, path); err != nil {
			return err
		}
	}
	// The order matches the non-streaming decode, so both name the same
	// feature when more than one is refused.
	if hasNonCallSignature(part) {
		if err := s.policy.refusal(FeatureGeminiResponseThoughtSignature, path+".thoughtSignature"); err != nil {
			return err
		}
	}
	if partMetadata && part.FunctionCall == nil {
		if err := s.policy.refusal(FeatureGeminiPartMetadata, metadataPath); err != nil {
			return err
		}
	}
	return nil
}

// losePartModifiers records the part's non-call signature and then its metadata
// losses, the order the non-streaming decode records them in. Both are already
// decided by decidePartLosses.
func (s *geminiResponsesStreamState) losePartModifiers(
	part geminigeneratecontent.Part, path, metadataPath string, partMetadata bool,
) error {
	if err := s.loseNonCallSignature(part, path+".thoughtSignature"); err != nil {
		return err
	}
	return s.losePartMetadata(partMetadata, metadataPath)
}

// losePartMetadata records the approved loss of part modifier metadata, once
// per stream, at the field path of the first present modifier. Its policy gate
// was decided before the part's payload committed.
func (s *geminiResponsesStreamState) losePartMetadata(present bool, path string) error {
	if !present || s.partMetadataLossRecorded {
		return nil
	}
	s.partMetadataLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureGeminiPartMetadata,
		path,
		"part modifier metadata cannot be reproduced in the target response",
	)
}

// loseThought records the approved loss of thought text under no reasoning
// capability, once per stream. Its policy gate was decided before the part's
// payload committed.
func (s *geminiResponsesStreamState) loseThought(path string) error {
	if s.thoughtLossNoted {
		return nil
	}
	if err := s.report.Lose(
		s.policy, FeatureProviderReasoningText, path,
		"provider reasoning text cannot be reproduced in the target response",
	); err != nil {
		return err
	}
	s.thoughtLossNoted = true
	return nil
}

// hasNonCallSignature reports whether the part carries a thought signature
// that no function call retains. An explicit empty signature carries none.
func hasNonCallSignature(part geminigeneratecontent.Part) bool {
	return part.ThoughtSignature != nil && *part.ThoughtSignature != "" && part.FunctionCall == nil
}

// loseNonCallSignature records the approved loss of a thought signature on a
// non-function-call part, once per stream. The path names the signature field.
// Its policy gate was decided before the part's payload committed.
func (s *geminiResponsesStreamState) loseNonCallSignature(part geminigeneratecontent.Part, path string) error {
	if !hasNonCallSignature(part) || s.partSignatureLossRecorded {
		return nil
	}
	s.partSignatureLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureGeminiResponseThoughtSignature,
		path,
		"a thought signature on a non-function-call stream part cannot be reproduced in the target response",
	)
}

// convertFunctionCall emits one complete function call: close any open item,
// then output_item.added, one arguments delta with the whole arguments string,
// arguments done, output_item.done. The call's wire faults are checked first;
// its metadata gate is decided after them and before any output commits, so an
// upstream fault is never reported as a local refusal. The thoughtSignature is
// queued for the signature store and written when the whole call succeeds.
func (s *geminiResponsesStreamState) convertFunctionCall(
	part geminigeneratecontent.Part, path, metadataPath string, partMetadata bool,
) ([]ResponsesSSEEvent, error) {
	call := part.FunctionCall
	if call.Name == "" {
		return nil, s.wireError(fmt.Errorf("gemini stream functionCall at %s has no name", path))
	}
	args := ""
	if len(call.Args) > 0 {
		trimmed := strings.TrimSpace(string(call.Args))
		if trimmed != "" && trimmed != "null" {
			if !json.Valid(call.Args) {
				return nil, s.wireError(fmt.Errorf(
					"gemini stream functionCall at %s carries malformed argument JSON", path,
				))
			}
			args = trimmed
		}
	}
	if args == "" {
		args = "{}"
	}
	synthesizedCallID := false
	callID := call.ID
	if len(args) > maxStreamAccumulatedBytes {
		return nil, s.wireError(fmt.Errorf(
			"gemini stream functionCall arguments at %s exceed %d bytes",
			path,
			maxStreamAccumulatedBytes,
		))
	}
	if err := s.chargeTotal(len(args), "tool arguments"); err != nil {
		return nil, err
	}
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	if err := s.budget.addToolCall(); err != nil {
		return nil, s.wireError(err)
	}
	outputIndex := int64(len(s.items))
	itemID := s.ctx.IDs.New("fc_")
	if callID == "" {
		callID = s.ctx.IDs.New("gcall_")
		synthesizedCallID = true
	}
	// Map the flattened wire name back to the client-visible tool name AND
	// its namespace qualifier, exactly as the chat paths do
	// (clientCallName is nil-safe and identity-maps an unmapped name).
	name, namespace := s.ctx.ToolNames.clientCallName(call.Name)
	// The call identity renders into the done events and the terminal
	// envelope, so it is retained semantic state like the arguments; the
	// namespace renders alongside the name, so its bytes are charged too.
	if err := s.chargeTotal(len(callID)+len(name)+len(namespace), "tool-call identity"); err != nil {
		return nil, err
	}
	// The part's metadata gate follows the call's wire checks and precedes every
	// record and commit below.
	if partMetadata {
		if err := s.policy.refusal(FeatureGeminiPartMetadata, metadataPath); err != nil {
			return nil, err
		}
	}
	if synthesizedCallID && !s.callIDSynthesizedNoted {
		// The note is recorded only once per stream, after the synthesized
		// identity passed the exchange-total charge; the flag is set after
		// the note commits.
		if err := s.report.Note(
			FeatureGeminiCallIDSynthesized, path+".functionCall",
			"the upstream function call carried no id; a proxy-minted call id was emitted",
		); err != nil {
			return nil, err
		}
		s.callIDSynthesizedNoted = true
	}
	events := s.closeOpenMessage()
	events = append(events, s.closeOpenReasoning()...)
	item := &ResponsesFunctionCallOutputItem{
		ID:        itemID,
		Type:      "function_call",
		Status:    ResponsesItemCompleted,
		CallID:    callID,
		Name:      name,
		Arguments: args,
		Namespace: namespace,
	}
	s.items = append(s.items, openResponsesItem{outputIndex: outputIndex, item: item})
	// Queue the thought signature for the next turn, keyed by the
	// client-visible call id; commitCall writes it once the call succeeds.
	if part.ThoughtSignature != nil && *part.ThoughtSignature != "" &&
		s.ctx.GeminiSignatures != nil {
		s.pendingSignatures = append(s.pendingSignatures, pendingSignature{
			callID:    callID,
			signature: *part.ThoughtSignature,
		})
	}
	// The added event announces the call in progress with empty arguments,
	// which then arrive through the delta, as on every other stream path.
	added := *item
	added.Status = ResponsesItemInProgress
	added.Arguments = ""
	return append(events,
		s.builder.OutputItemAdded(outputIndex, &added),
		s.builder.FunctionArgumentsDelta(itemID, outputIndex, args),
		s.builder.FunctionArgumentsDone(itemID, outputIndex, args),
		s.builder.OutputItemDone(outputIndex, item),
	), nil
}

// appendRefusal adds a refusal content part to the open message item, opening
// one when none is open (the same placement as the non-streaming decode). An
// open text part is closed first, so at most one part is ever open. The
// refusal text is streamed as one delta so the done event reconciles with it;
// the part stays open until closeAllAndHold.
func (s *geminiResponsesStreamState) appendRefusal(text string) ([]ResponsesSSEEvent, error) {
	// The refusal text comes from the upstream (finishMessage), so it has the
	// same per-part bound as streamed text. The bounds are checked before any
	// item or part is opened.
	if err := s.checkDeltaBounds(len(text), "refusal"); err != nil {
		return nil, err
	}
	events, err := s.ensureMessageOpen()
	if err != nil {
		return nil, err
	}
	if err := s.budget.addPart(); err != nil {
		return nil, s.wireError(err)
	}
	if err := s.chargeTotal(len(text), "refusal"); err != nil {
		return nil, err
	}
	item := &s.items[len(s.items)-1]
	message := item.item.(*ResponsesOutputMessage)
	events = append(events, s.closeOpenPart(item)...)
	contentIndex := int64(len(message.Content))
	message.Content = append(message.Content, &ResponsesOutputRefusal{Type: "refusal", Refusal: text})
	item.openPartIndex = contentIndex
	events = append(events,
		s.builder.ContentPartAdded(message.ID, item.outputIndex, contentIndex,
			&ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""}),
		s.builder.RefusalDelta(message.ID, item.outputIndex, contentIndex, text),
	)
	return events, nil
}

// rejectContinuationToken refuses a candidate carrying an unrepresentable
// continuation token. It is pure, so callers run it before committing output.
func rejectContinuationToken(candidate *geminigeneratecontent.Candidate) error {
	if candidate.ContinuationToken == nil || *candidate.ContinuationToken == "" {
		return nil
	}
	return &UnrepresentableError{
		Protocol: string(UpstreamGemini),
		Path:     "candidates[].continuationToken",
		Detail:   "a continuation token cannot be represented in the target dialect",
	}
}
