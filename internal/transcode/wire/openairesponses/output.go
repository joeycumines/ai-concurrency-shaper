package openairesponses

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Annotation is one annotation on an output_text part. Only the type tag is
// modeled; variant-specific fields are added only as supported, and unknown
// annotation types are rejected rather than represented by an "everything
// optional" struct.
type Annotation struct {
	Type string `json:"type"`
}

// TextLogprob is one token log-probability entry.
type TextLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// OutputContentPart is one content part of an output message.
type OutputContentPart interface {
	isOutputContentPart()
	Validate() error
}

// OutputText is an output_text content part. Annotations are required on the
// wire; use an empty array when there are none.
type OutputText struct {
	Type        string        `json:"type"`
	Text        string        `json:"text"`
	Annotations []Annotation  `json:"annotations"`
	Logprobs    []TextLogprob `json:"logprobs,omitempty"`
}

func (*OutputText) isOutputContentPart() {}

func (p *OutputText) Validate() error {
	if p.Type != "output_text" {
		return fmt.Errorf("output text type = %q", p.Type)
	}
	if p.Annotations == nil {
		return errors.New("output_text annotations must be present; use an empty array")
	}
	return nil
}

// OutputRefusal is a refusal content part of an output message. Refusal is
// modeled as message content, never as a standalone item type.
type OutputRefusal struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

func (*OutputRefusal) isOutputContentPart() {}

func (p *OutputRefusal) Validate() error {
	if p.Type != "refusal" {
		return fmt.Errorf("refusal part type = %q", p.Type)
	}
	return nil
}

// OutputContentParts is the content array of an output message. It must be
// present on the wire; use an empty array when there is no content.
type OutputContentParts []OutputContentPart

// Validate checks presence and every part.
func (parts OutputContentParts) Validate() error {
	if parts == nil {
		return errors.New("output message content must be present; use an empty array")
	}
	for i, part := range parts {
		if part == nil {
			return fmt.Errorf("output content part %d is nil", i)
		}
		if err := part.Validate(); err != nil {
			return fmt.Errorf("output content part %d: %w", i, err)
		}
	}
	return nil
}

// UnmarshalJSON decodes each part through its tagged variant.
func (parts *OutputContentParts) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := wire.Decode(data, &raw); err != nil {
		return err
	}

	out := make(OutputContentParts, 0, len(raw))
	for i, partData := range raw {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(partData, &probe); err != nil {
			return fmt.Errorf("output content part %d: %w", i, err)
		}

		if probe.Type == "" {
			return &wire.DecodeError{
				Kind:    wire.DecodeMissingRequired,
				Path:    fmt.Sprintf("output[].content[%d].type", i),
				Message: "output content part requires a type tag",
			}
		}
		var part OutputContentPart
		switch probe.Type {
		case "output_text":
			part = &OutputText{}
		case "refusal":
			part = &OutputRefusal{}
		default:
			return &wire.UnsupportedTypeError{
				Protocol: "responses",
				Path:     fmt.Sprintf("output[].content[%d].type", i),
				Type:     probe.Type,
			}
		}

		if err := wire.Decode(partData, part); err != nil {
			return fmt.Errorf("output content part %d: %w", i, err)
		}
		// Decode-side normalization (autopsy 01): real clients omit the
		// annotations key on output_text parts; a decoded absent array is
		// the same empty array. Validate itself stays strict for hand-built
		// values.
		if text, ok := part.(*OutputText); ok && text.Annotations == nil {
			text.Annotations = []Annotation{}
		}
		if err := part.Validate(); err != nil {
			return fmt.Errorf("output content part %d: %w", i, err)
		}
		out = append(out, part)
	}

	*parts = out
	return nil
}

// MarshalJSON validates and emits every part.
func (parts OutputContentParts) MarshalJSON() ([]byte, error) {
	if err := parts.Validate(); err != nil {
		return nil, err
	}

	raw := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		b, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		raw = append(raw, b)
	}
	return json.Marshal(raw)
}

// OutputItem is one tagged variant of the output item union.
type OutputItem interface {
	isOutputItem()
	Validate() error
}

// OutputMessage is an assistant output message.
type OutputMessage struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Status  ItemStatus         `json:"status"`
	Phase   string             `json:"phase,omitempty"`
	Content OutputContentParts `json:"content"`
}

func (*OutputMessage) isOutputItem() {}

func (m *OutputMessage) Validate() error {
	if m.ID == "" {
		return errors.New("output message id is empty")
	}
	if m.Type != "message" {
		return fmt.Errorf("output message type = %q", m.Type)
	}
	if m.Role != "assistant" {
		return &wire.DecodeError{
			Kind:    wire.DecodeContradictoryUnion,
			Path:    "role",
			Message: fmt.Sprintf("output message role = %q, want assistant", m.Role),
		}
	}
	if !ValidStatus(m.Status) {
		return fmt.Errorf("invalid output message status %q", m.Status)
	}
	if m.Phase != "" && m.Phase != "commentary" && m.Phase != "final_answer" {
		return fmt.Errorf("invalid output message phase %q", m.Phase)
	}
	return m.Content.Validate()
}

// FunctionCallOutputItem is an output function_call item.
type FunctionCallOutputItem struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Status    ItemStatus `json:"status"`
	CallID    string     `json:"call_id"`
	Name      string     `json:"name"`
	Arguments string     `json:"arguments"`
}

func (*FunctionCallOutputItem) isOutputItem() {}

func (c *FunctionCallOutputItem) Validate() error {
	if c.ID == "" {
		return errors.New("output function call id is empty")
	}
	if c.Type != "function_call" {
		return fmt.Errorf("output function call type = %q", c.Type)
	}
	// status is optional on the wire: the official function-call output
	// events omit it while the call is in progress.
	if c.Status != "" && !ValidStatus(c.Status) {
		return fmt.Errorf("invalid output function call status %q", c.Status)
	}
	if c.CallID == "" || c.Name == "" {
		return errors.New("output function call requires call_id and name")
	}
	// arguments is a stringified payload: model-generated arguments are
	// preserved exactly and are NOT required to be valid JSON (review-z
	// commit 2) — invalid model output is never an upstream defect.
	return nil
}

// FunctionCallOutputResultItem is an output function_call_output item. It
// appears in the output array when a function result is included in the
// response.
type FunctionCallOutputResultItem struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Status ItemStatus     `json:"status"`
	CallID string         `json:"call_id"`
	Output FunctionOutput `json:"output"`
}

func (*FunctionCallOutputResultItem) isOutputItem() {}

func (c *FunctionCallOutputResultItem) Validate() error {
	if c.ID == "" {
		return errors.New("output function call output id is empty")
	}
	if c.Type != "function_call_output" {
		return fmt.Errorf("output function call output type = %q", c.Type)
	}
	if c.CallID == "" {
		return errors.New("output function call output requires call_id")
	}
	if c.Status != "" && !ValidStatus(c.Status) {
		return fmt.Errorf("invalid output function call output status %q", c.Status)
	}
	return c.Output.Validate()
}

// ReasoningOutputItem is an output reasoning item.
type ReasoningOutputItem struct {
	ID               string             `json:"id"`
	Type             string             `json:"type"`
	Status           ItemStatus         `json:"status"`
	Summary          []ReasoningSummary `json:"summary"`
	Content          []ReasoningText    `json:"content,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
}

func (*ReasoningOutputItem) isOutputItem() {}

func (r *ReasoningOutputItem) Validate() error {
	if r.ID == "" {
		return errors.New("output reasoning id is empty")
	}
	if r.Type != "reasoning" {
		return fmt.Errorf("output reasoning type = %q", r.Type)
	}
	if !ValidStatus(r.Status) {
		return fmt.Errorf("invalid output reasoning status %q", r.Status)
	}
	if r.Summary == nil {
		return errors.New("output reasoning summary must be present")
	}
	for i, summary := range r.Summary {
		if summary.Type != "summary_text" {
			return fmt.Errorf("output reasoning summary %d type = %q", i, summary.Type)
		}
	}
	for i, content := range r.Content {
		if content.Type != "reasoning_text" {
			return fmt.Errorf("output reasoning content %d type = %q", i, content.Type)
		}
	}
	return nil
}

// DecodeOutputItem decodes one output item through its tagged variant.
// Unknown item types produce a wire.UnsupportedTypeError identifying the
// exact type, never a silent drop.
func DecodeOutputItem(data []byte) (OutputItem, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	if probe.Type == "" {
		return nil, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "type",
			Message: "output item requires a type tag",
		}
	}

	var item OutputItem
	switch probe.Type {
	case "message":
		item = &OutputMessage{}
	case "function_call":
		item = &FunctionCallOutputItem{}
	case "function_call_output":
		item = &FunctionCallOutputResultItem{}
	case "reasoning":
		item = &ReasoningOutputItem{}
	default:
		return nil, &wire.UnsupportedTypeError{
			Protocol: "responses",
			Path:     "output[].type",
			Type:     probe.Type,
		}
	}

	if err := wire.Decode(data, item); err != nil {
		return nil, err
	}
	if err := item.Validate(); err != nil {
		return nil, err
	}
	return item, nil
}
