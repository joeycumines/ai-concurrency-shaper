package geminigeneratecontent

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Content is one turn of a Gemini conversation, or the system instruction.
type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts,omitempty"`
}

// Part is one element of Content.Parts. Gemini distinguishes payload members,
// of which a part carries at most one, from modifiers that may accompany any
// payload. A signature-only part is the one exception: it carries no payload.
//
// Members the transcoder never interprets stay raw so their presence is
// detectable without guessing their shape. An explicit JSON null is kept as
// the literal null in a raw member; read presence with RawMemberPresent.
type Part struct {
	Text             *string           `json:"text,omitempty"`
	Thought          *bool             `json:"thought,omitempty"`
	ThoughtSignature *string           `json:"thoughtSignature,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FileData         *FileData         `json:"fileData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`

	ExecutableCode      json.RawMessage `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage `json:"codeExecutionResult,omitempty"`
	ToolCall            json.RawMessage `json:"toolCall,omitempty"`
	ToolResponse        json.RawMessage `json:"toolResponse,omitempty"`
	AudioTranscription  json.RawMessage `json:"audioTranscription,omitempty"`
	VideoMetadata       json.RawMessage `json:"videoMetadata,omitempty"`
	SpeechMetadata      json.RawMessage `json:"speechMetadata,omitempty"`
	PartMetadata        json.RawMessage `json:"partMetadata,omitempty"`
	MediaResolution     json.RawMessage `json:"mediaResolution,omitempty"`
	MediaProcessing     *string         `json:"mediaProcessing,omitempty"`
}

// Blob is inline bytes carried by an inlineData part.
type Blob struct {
	MimeType    string `json:"mimeType,omitempty"`
	Data        string `json:"data,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// FileData references a file held by Google.
type FileData struct {
	MimeType    string `json:"mimeType,omitempty"`
	FileURI     string `json:"fileUri,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// FunctionCall is a model-issued tool call.
type FunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// FunctionResponse answers a FunctionCall.
type FunctionResponse struct {
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	Parts        json.RawMessage `json:"parts,omitempty"`
	Scheduling   *string         `json:"scheduling,omitempty"`
	WillContinue *bool           `json:"willContinue,omitempty"`
}

// UnmarshalJSON decodes a part strictly, so an unknown member is rejected on
// both the request and the response surface, then validates its union arm.
func (p *Part) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return &wire.DecodeError{
			Kind:    wire.DecodeIllegalNull,
			Path:    "part",
			Message: "null is not allowed for a part",
		}
	}
	type partAlias Part
	var decoded partAlias
	if err := wire.Decode(data, &decoded); err != nil {
		return err
	}
	part := Part(decoded)
	if err := part.Validate(); err != nil {
		return err
	}
	*p = part
	return nil
}

// Validate enforces the payload/modifier rule and the mediaProcessing enum.
// A function call or function response payload must carry a JSON object.
func (p Part) Validate() error {
	payloads := 0
	for _, present := range [...]bool{
		p.Text != nil,
		p.InlineData != nil,
		p.FileData != nil,
		p.FunctionCall != nil,
		p.FunctionResponse != nil,
		RawMemberPresent(p.ExecutableCode),
		RawMemberPresent(p.CodeExecutionResult),
		RawMemberPresent(p.ToolCall),
		RawMemberPresent(p.ToolResponse),
		RawMemberPresent(p.AudioTranscription),
	} {
		if present {
			payloads++
		}
	}
	if payloads > 1 {
		return &wire.DecodeError{
			Kind:    wire.DecodeContradictoryUnion,
			Path:    "part",
			Message: "part carries more than one payload member",
		}
	}
	if payloads == 0 && p.ThoughtSignature == nil {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "part",
			Message: "part requires a payload member or thoughtSignature",
		}
	}
	if p.MediaProcessing != nil {
		switch *p.MediaProcessing {
		case "MEDIA_PROCESSING_UNSPECIFIED", "STATIC", "AGENTIC":
		default:
			return &wire.DecodeError{
				Kind:    wire.DecodeMalformed,
				Path:    "part.mediaProcessing",
				Message: fmt.Sprintf("unknown mediaProcessing value %q", *p.MediaProcessing),
			}
		}
	}
	if p.FunctionCall != nil {
		if err := validateJSONObject("part.functionCall.args", p.FunctionCall.Args); err != nil {
			return err
		}
	}
	if p.FunctionResponse != nil {
		if err := validateJSONObject("part.functionResponse.response", p.FunctionResponse.Response); err != nil {
			return err
		}
	}
	return nil
}

// RawMemberPresent reports whether a raw member carries a value. An absent
// member and an explicit JSON null both report false, because RawMessage keeps
// the literal null bytes.
func RawMemberPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

// validateJSONObject rejects a present raw payload that is not a JSON object.
func validateJSONObject(path string, raw json.RawMessage) error {
	if !RawMemberPresent(raw) {
		return nil
	}
	if _, err := wire.JSONObject(string(raw)); err != nil {
		return &wire.DecodeError{
			Kind:    wire.DecodeMalformed,
			Path:    path,
			Message: err.Error(),
		}
	}
	return nil
}
