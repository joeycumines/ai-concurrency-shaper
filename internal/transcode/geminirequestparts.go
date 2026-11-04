package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// part converts one Gemini part of a turn. content is the index of the
// containing contents entry (-1 for the system instruction) and idless counts
// the id-less functionResponse parts of the same content, by name. A part may
// convert to nothing when its only content is an approved loss; every such
// drop has already been recorded.
func (d *geminiDecoder) part(
	path string,
	role CanonicalRole,
	content int,
	part geminigeneratecontent.Part,
	idless map[string]int,
) ([]CanonicalPart, error) {
	if part.ThoughtSignature != nil {
		if err := d.report.Lose(
			d.policy, FeatureGeminiThoughtSignature, path+".thoughtSignature",
			"the thought signature is not replayed",
		); err != nil {
			return nil, err
		}
	}
	if part.Thought != nil && *part.Thought {
		if part.Text == nil {
			return nil, fmt.Errorf("%s: thought is only valid on a text part", path)
		}
		if err := d.report.Lose(
			d.policy, FeatureGeminiThoughtSignature, path+".thought",
			"thought text is not replayed",
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	for _, member := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"videoMetadata", part.VideoMetadata},
		{"speechMetadata", part.SpeechMetadata},
		{"audioTranscription", part.AudioTranscription},
	} {
		if geminigeneratecontent.RawMemberPresent(member.raw) {
			return nil, fmt.Errorf("%s.%s is not supported", path, member.name)
		}
	}
	metadataLost := false
	if geminigeneratecontent.RawMemberPresent(part.MediaResolution) ||
		part.MediaProcessing != nil ||
		geminigeneratecontent.RawMemberPresent(part.PartMetadata) {
		if err := d.report.Lose(
			d.policy, FeatureGeminiPartMetadata, path,
			"part media and metadata settings are not forwarded",
		); err != nil {
			return nil, err
		}
		metadataLost = true
	}

	switch {
	case part.Text != nil:
		return []CanonicalPart{CanonicalText{Text: *part.Text}}, nil
	case part.InlineData != nil:
		converted, err := geminiInlineData(path, *part.InlineData)
		if err != nil {
			return nil, err
		}
		return []CanonicalPart{converted}, nil
	case part.FileData != nil:
		return nil, fmt.Errorf("%s.fileData is not supported: send the bytes inline", path)
	case part.FunctionCall != nil:
		if role != CanonicalAssistant {
			return nil, fmt.Errorf("%s.functionCall is only valid in a model turn", path)
		}
		call, err := d.functionCall(path, content, *part.FunctionCall)
		if err != nil {
			return nil, err
		}
		return []CanonicalPart{call}, nil
	case part.FunctionResponse != nil:
		if role != CanonicalUser {
			return nil, fmt.Errorf("%s.functionResponse is only valid in a user turn", path)
		}
		result, err := d.functionResponse(path, content, *part.FunctionResponse, idless)
		if err != nil {
			return nil, err
		}
		return []CanonicalPart{result}, nil
	}

	for _, member := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"executableCode", part.ExecutableCode},
		{"codeExecutionResult", part.CodeExecutionResult},
		{"toolCall", part.ToolCall},
		{"toolResponse", part.ToolResponse},
	} {
		if !geminigeneratecontent.RawMemberPresent(member.raw) {
			continue
		}
		if err := d.report.Lose(
			d.policy, FeatureGeminiProviderTools, path+"."+member.name,
			"provider tool content is not forwarded",
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if metadataLost || part.ThoughtSignature != nil {
		return nil, nil
	}
	return nil, fmt.Errorf("%s has no content", path)
}

// geminiInlineData converts inline bytes. Images and PDF documents have a
// canonical form; every other media type is rejected rather than guessed.
func geminiInlineData(path string, blob geminigeneratecontent.Blob) (CanonicalPart, error) {
	if blob.Data == "" {
		return nil, fmt.Errorf("%s.inlineData.data is required", path)
	}
	switch {
	case strings.HasPrefix(blob.MimeType, "image/"):
		return CanonicalImage{MediaType: blob.MimeType, Base64: blob.Data}, nil
	case blob.MimeType == "application/pdf":
		return CanonicalDocument{MediaType: blob.MimeType, Base64: blob.Data, Filename: blob.DisplayName}, nil
	default:
		return nil, fmt.Errorf(
			"%s.inlineData.mimeType %q is not supported: inline images and application/pdf documents only",
			path, blob.MimeType,
		)
	}
}

// functionCall converts a model-issued call. Missing args mean an empty
// object; any other non-object is rejected. The id follows the call-identity
// rules: a duplicate id is rejected and a missing id is minted once with a
// note.
func (d *geminiDecoder) functionCall(
	path string,
	content int,
	call geminigeneratecontent.FunctionCall,
) (CanonicalPart, error) {
	if call.Name == "" {
		return nil, fmt.Errorf("%s.functionCall.name is required", path)
	}
	args := json.RawMessage("{}")
	if geminigeneratecontent.RawMemberPresent(call.Args) {
		if _, err := wire.JSONObject(string(call.Args)); err != nil {
			return nil, fmt.Errorf("%s.functionCall.args must be a JSON object", path)
		}
		args = call.Args
	}
	id, err := d.callID(path, call.ID, call.Name)
	if err != nil {
		return nil, err
	}
	d.open = append(d.open, geminiOpenCall{id: id, name: call.Name, content: content})
	return CanonicalFunctionCall{ItemID: id, CallID: id, Name: call.Name, Arguments: args}, nil
}

// callID returns the id a functionCall carries, or a proxy-minted id when it
// carries none. Minted ids skip every id already in use.
func (d *geminiDecoder) callID(path string, id string, name string) (string, error) {
	if id != "" {
		if _, dup := d.callIDs[id]; dup {
			return "", fmt.Errorf("%s.functionCall.id %q is used by more than one functionCall", path, id)
		}
		d.callIDs[id] = struct{}{}
		return id, nil
	}
	if !d.callIDNoted {
		if err := d.report.Note(
			FeatureGeminiCallIDSynthesized, path+".functionCall",
			"a functionCall without an id was given a proxy-minted id",
		); err != nil {
			return "", err
		}
		d.callIDNoted = true
	}
	for {
		d.minted++
		candidate := fmt.Sprintf("gcall_%d", d.minted)
		if _, taken := d.callIDs[candidate]; !taken {
			d.callIDs[candidate] = struct{}{}
			return candidate, nil
		}
	}
}

// functionResponse converts a functionResponse of a user turn. Its parts,
// scheduling, and willContinue members have no canonical form and are
// rejected.
func (d *geminiDecoder) functionResponse(
	path string,
	content int,
	resp geminigeneratecontent.FunctionResponse,
	idless map[string]int,
) (CanonicalPart, error) {
	if resp.Name == "" {
		return nil, fmt.Errorf("%s.functionResponse.name is required", path)
	}
	if geminigeneratecontent.RawMemberPresent(resp.Parts) {
		return nil, fmt.Errorf("%s.functionResponse.parts is not supported", path)
	}
	if resp.Scheduling != nil {
		return nil, fmt.Errorf("%s.functionResponse.scheduling is not supported", path)
	}
	if resp.WillContinue != nil {
		return nil, fmt.Errorf("%s.functionResponse.willContinue is not supported", path)
	}
	callID, err := d.matchCall(path, content, resp.ID, resp.Name, idless)
	if err != nil {
		return nil, err
	}
	text, isError, err := d.responseText(path, resp.Response)
	if err != nil {
		return nil, err
	}
	return CanonicalFunctionResult{
		CallID:  callID,
		IsError: isError,
		Parts:   []CanonicalPart{CanonicalText{Text: text}},
	}, nil
}

// matchCall pairs a functionResponse with the open functionCall it answers.
// An id pairs exactly. An id-less response pairs by name: one open call with
// that name pairs directly; several pair in request order only when every
// such call sits in the immediately preceding model turn and the response
// count matches, which is recorded as a note.
func (d *geminiDecoder) matchCall(
	path string,
	content int,
	id string,
	name string,
	idless map[string]int,
) (string, error) {
	if id != "" {
		for i, open := range d.open {
			if open.id != id {
				continue
			}
			if open.name != name {
				return "", fmt.Errorf(
					"%s.functionResponse %q names %q but its functionCall is %q",
					path, id, name, open.name,
				)
			}
			d.removeOpen(i)
			return open.id, nil
		}
		return "", fmt.Errorf("%s.functionResponse id %q matches no open functionCall", path, id)
	}

	var candidates []int
	for i, open := range d.open {
		if open.name == name {
			candidates = append(candidates, i)
		}
	}
	unmatched := fmt.Errorf(
		"functionResponse %q cannot be matched to a single functionCall; send ids", name,
	)
	if len(candidates) == 0 {
		return "", fmt.Errorf("%s: %w", path, unmatched)
	}
	if len(candidates) > 1 {
		if idless[name] != len(candidates) {
			return "", fmt.Errorf("%s: %w", path, unmatched)
		}
		for _, i := range candidates {
			if d.open[i].content != content-1 {
				return "", fmt.Errorf("%s: %w", path, unmatched)
			}
		}
		if !d.correlationNote {
			if err := d.report.Note(
				FeatureGeminiCallCorrelation, path+".functionResponse",
				"id-less functionResponse parts were paired with the preceding model turn's functionCalls by order",
			); err != nil {
				return "", err
			}
			d.correlationNote = true
		}
	}
	open := d.open[candidates[0]]
	d.removeOpen(candidates[0])
	return open.id, nil
}

func (d *geminiDecoder) removeOpen(i int) {
	d.open = append(d.open[:i], d.open[i+1:]...)
}

// responseText renders a functionResponse object as the tool-result text. The
// exact shapes {"output": string} and {"error": string} map to ordinary text
// and to the error flag; any other object is carried as compact JSON, which
// is recorded as a note. An absent response is the empty object.
func (d *geminiDecoder) responseText(path string, raw json.RawMessage) (string, bool, error) {
	if !geminigeneratecontent.RawMemberPresent(raw) {
		raw = json.RawMessage("{}")
	}
	members, err := wire.JSONObject(string(raw))
	if err != nil {
		return "", false, fmt.Errorf("%s.functionResponse.response must be a JSON object", path)
	}
	if len(members) == 1 {
		for _, key := range []string{"output", "error"} {
			value, ok := members[key]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				break
			}
			return text, key == "error", nil
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", false, fmt.Errorf("%s.functionResponse.response must be a JSON object", path)
	}
	if err := d.report.Note(
		FeatureGeminiFunctionResponseJSON, path+".functionResponse.response",
		"the response object is carried as compact JSON text",
	); err != nil {
		return "", false, err
	}
	return buf.String(), false, nil
}
