package openairesponses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Input models the request-level input union: either a plain string or a
// list of input items. Exactly one arm is selected.
type Input struct {
	Text  *string
	Items InputItems // nil means the list arm is not selected
}

// Validate checks the union invariants and each selected item.
func (v Input) Validate() error {
	if v.Text != nil && v.Items != nil {
		return errors.New("responses input has both string and item-list variants")
	}
	if v.Text == nil && v.Items == nil {
		return errors.New("responses input has no selected variant")
	}
	for i, item := range v.Items {
		if item == nil {
			return fmt.Errorf("responses input item %d is nil", i)
		}
		if err := item.Validate(); err != nil {
			return fmt.Errorf("responses input item %d: %w", i, err)
		}
	}
	return nil
}

// UnmarshalJSON decodes the string or item-list arm.
func (v *Input) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty responses input")
	}

	switch data[0] {
	case '"':
		var text string
		if err := wire.Decode(data, &text); err != nil {
			return err
		}
		v.Text = &text
		v.Items = nil
		return nil

	case '[':
		var items InputItems
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		v.Text = nil
		v.Items = items
		return nil

	default:
		return errors.New("responses input must be a string or item array")
	}
}

// MarshalJSON emits the selected arm.
func (v Input) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	if v.Text != nil {
		return json.Marshal(*v.Text)
	}
	return json.Marshal(v.Items)
}

// InputContentPart is one content part of an input message.
type InputContentPart interface {
	isInputContentPart()
	Validate() error
}

// InputText is an input_text content part.
type InputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (InputText) isInputContentPart() {}

func (p InputText) Validate() error {
	if p.Type != "input_text" {
		return fmt.Errorf("input text type = %q, want input_text", p.Type)
	}
	return nil
}

// InputImage is an input_image content part. The official shape uses
// image_url (including base64 data URLs) or file_id — never a private
// image_data field.
type InputImage struct {
	Type     string `json:"type"`
	Detail   string `json:"detail"`
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

func (InputImage) isInputContentPart() {}

func (p InputImage) Validate() error {
	if p.Type != "input_image" {
		return fmt.Errorf("input image type = %q, want input_image", p.Type)
	}
	// detail is optional on the wire; the official SDKs omit it and the API
	// defaults to auto. Empty is accepted and defaulted at render time.
	if p.Detail != "" {
		switch p.Detail {
		case "auto", "low", "high", "original":
		default:
			return fmt.Errorf("invalid input image detail %q", p.Detail)
		}
	}
	if (p.ImageURL == "") == (p.FileID == "") {
		return errors.New("input image requires exactly one of image_url or file_id")
	}
	return nil
}

// InputFile is an input_file content part.
type InputFile struct {
	Type     string `json:"type"`
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	Filename string `json:"filename,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (InputFile) isInputContentPart() {}

func (p InputFile) Validate() error {
	if p.Type != "input_file" {
		return fmt.Errorf("input file type = %q, want input_file", p.Type)
	}

	selected := 0
	if p.FileID != "" {
		selected++
	}
	if p.FileData != "" {
		selected++
	}
	if p.FileURL != "" {
		selected++
	}
	if selected != 1 {
		return errors.New(
			"input file requires exactly one of file_id, file_data, or file_url",
		)
	}

	if p.Detail != "" {
		switch p.Detail {
		case "auto", "low", "high":
		default:
			return fmt.Errorf("invalid input file detail %q", p.Detail)
		}
	}
	return nil
}

// InputContentParts is the array arm of message content.
type InputContentParts []InputContentPart

// UnmarshalJSON decodes each part through its tagged variant.
func (p *InputContentParts) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := wire.Decode(data, &raw); err != nil {
		return err
	}

	out := make(InputContentParts, 0, len(raw))
	for i, item := range raw {
		part, err := DecodeInputContentPart(item)
		if err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
		out = append(out, part)
	}
	*p = out
	return nil
}

// MarshalJSON validates and emits every part.
func (p InputContentParts) MarshalJSON() ([]byte, error) {
	raw := make([]json.RawMessage, 0, len(p))
	for i, part := range p {
		if part == nil {
			return nil, fmt.Errorf("content part %d is nil", i)
		}
		if err := part.Validate(); err != nil {
			return nil, fmt.Errorf("content part %d: %w", i, err)
		}
		b, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		raw = append(raw, b)
	}
	return json.Marshal(raw)
}

// DecodeInputContentPart dispatches on the type tag. Unknown part types
// produce a wire.UnsupportedTypeError identifying the exact type.
func DecodeInputContentPart(data []byte) (InputContentPart, error) {
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
			Message: "input content part requires a type tag",
		}
	}

	var part InputContentPart
	switch probe.Type {
	case "input_text":
		part = &InputText{}
	case "input_image":
		part = &InputImage{}
	case "input_file":
		part = &InputFile{}
	default:
		return nil, &wire.UnsupportedTypeError{
			Protocol: "responses",
			Path:     "input[].content[].type",
			Type:     probe.Type,
		}
	}

	if err := wire.Decode(data, part); err != nil {
		return nil, err
	}
	if err := part.Validate(); err != nil {
		return nil, err
	}
	return part, nil
}

// InputMessageContent is a string-or-parts union for message content.
type InputMessageContent struct {
	Text  *string
	Parts InputContentParts
}

// Validate checks the union invariants.
func (c InputMessageContent) Validate() error {
	if c.Text != nil && c.Parts != nil {
		return errors.New("message content has both string and part-list variants")
	}
	if c.Text == nil && c.Parts == nil {
		return errors.New("message content has no selected variant")
	}
	for i, part := range c.Parts {
		if err := part.Validate(); err != nil {
			return fmt.Errorf("message content part %d: %w", i, err)
		}
	}
	return nil
}

// UnmarshalJSON decodes the string or part-list arm.
func (c *InputMessageContent) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty message content")
	}
	if data[0] == '"' {
		var text string
		if err := wire.Decode(data, &text); err != nil {
			return err
		}
		c.Text = &text
		c.Parts = nil
		return nil
	}
	if data[0] == '[' {
		var parts InputContentParts
		if err := json.Unmarshal(data, &parts); err != nil {
			return err
		}
		c.Text = nil
		c.Parts = parts
		return nil
	}
	return errors.New("message content must be a string or content-part array")
}

// MarshalJSON emits the selected arm.
func (c InputMessageContent) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Text != nil {
		return json.Marshal(*c.Text)
	}
	return json.Marshal(c.Parts)
}

// InputItem is one tagged variant of the request input item union.
type InputItem interface {
	isInputItem()
	Validate() error
}

// EasyInputMessage is an easy input message without an ID.
type EasyInputMessage struct {
	Type    string              `json:"type,omitempty"`
	Role    InputRole           `json:"role"`
	Content InputMessageContent `json:"content"`
	Phase   string              `json:"phase,omitempty"`
}

func (*EasyInputMessage) isInputItem() {}

func (m *EasyInputMessage) Validate() error {
	if m.Type != "" && m.Type != "message" {
		return fmt.Errorf("message type = %q", m.Type)
	}
	switch m.Role {
	case InputRoleUser,
		InputRoleAssistant,
		InputRoleSystem,
		InputRoleDeveloper:
	default:
		return fmt.Errorf("invalid easy-message role %q", m.Role)
	}
	if m.Phase != "" && m.Phase != "commentary" && m.Phase != "final_answer" {
		return fmt.Errorf("invalid assistant phase %q", m.Phase)
	}
	return m.Content.Validate()
}

// PreviousOutputMessage is a previous model output message reused as input.
// It is not the same wire variant as an easy input message: it carries an
// ID, status, assistant role, and output content.
type PreviousOutputMessage struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Status  ItemStatus         `json:"status"`
	Phase   string             `json:"phase,omitempty"`
	Content OutputContentParts `json:"content"`
}

func (*PreviousOutputMessage) isInputItem() {}

func (m *PreviousOutputMessage) Validate() error {
	if m.ID == "" {
		return errors.New("previous output message id is empty")
	}
	if m.Type != "message" {
		return fmt.Errorf("previous output message type = %q", m.Type)
	}
	if m.Role != "assistant" {
		return fmt.Errorf("previous output message role = %q", m.Role)
	}
	if !ValidStatus(m.Status) {
		return fmt.Errorf("invalid previous output status %q", m.Status)
	}
	return m.Content.Validate()
}

// FunctionCallInput is a function_call input item.
type FunctionCallInput struct {
	ID        string     `json:"id,omitempty"`
	Type      string     `json:"type"`
	Status    ItemStatus `json:"status,omitempty"`
	CallID    string     `json:"call_id"`
	Name      string     `json:"name"`
	Arguments string     `json:"arguments"`
}

func (*FunctionCallInput) isInputItem() {}

func (c *FunctionCallInput) Validate() error {
	if c.Type != "function_call" {
		return fmt.Errorf("function call type = %q", c.Type)
	}
	if c.CallID == "" {
		return errors.New("function call call_id is empty")
	}
	if c.Name == "" {
		return errors.New("function call name is empty")
	}
	if c.Status != "" && !ValidStatus(c.Status) {
		return fmt.Errorf("invalid function call status %q", c.Status)
	}
	if !json.Valid([]byte(c.Arguments)) {
		return &wire.DecodeError{
			Kind:    wire.DecodeContradictoryUnion,
			Path:    "arguments",
			Message: "function call arguments are not valid JSON",
		}
	}
	return nil
}

// FunctionOutput is the output arm of a function_call_output item: either a
// plain string or an array of input content parts.
type FunctionOutput struct {
	Text  *string
	Parts InputContentParts
}

// Validate checks the union invariants.
func (o FunctionOutput) Validate() error {
	if o.Text != nil && o.Parts != nil {
		return errors.New("function output has both string and part-list variants")
	}
	if o.Text == nil && o.Parts == nil {
		return errors.New("function output has no selected variant")
	}
	for i, part := range o.Parts {
		if err := part.Validate(); err != nil {
			return fmt.Errorf("function output part %d: %w", i, err)
		}
	}
	return nil
}

// UnmarshalJSON decodes the string or part-list arm.
func (o *FunctionOutput) UnmarshalJSON(data []byte) error {
	data = wire.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty function output")
	}
	if data[0] == '"' {
		var text string
		if err := wire.Decode(data, &text); err != nil {
			return err
		}
		o.Text = &text
		o.Parts = nil
		return nil
	}
	if data[0] == '[' {
		var parts InputContentParts
		if err := json.Unmarshal(data, &parts); err != nil {
			return err
		}
		o.Text = nil
		o.Parts = parts
		return nil
	}
	return errors.New("function output must be a string or input-part array")
}

// MarshalJSON emits the selected arm.
func (o FunctionOutput) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.Text != nil {
		return json.Marshal(*o.Text)
	}
	return json.Marshal(o.Parts)
}

// FunctionCallOutputInput is a function_call_output input item.
type FunctionCallOutputInput struct {
	ID     string         `json:"id,omitempty"`
	Type   string         `json:"type"`
	Status ItemStatus     `json:"status,omitempty"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name,omitempty"`
	Output FunctionOutput `json:"output"`
}

func (*FunctionCallOutputInput) isInputItem() {}

func (o *FunctionCallOutputInput) Validate() error {
	if o.Type != "function_call_output" {
		return fmt.Errorf("function call output type = %q", o.Type)
	}
	if o.CallID == "" {
		return errors.New("function call output call_id is empty")
	}
	if o.Status != "" && !ValidStatus(o.Status) {
		return fmt.Errorf("invalid function call output status %q", o.Status)
	}
	return o.Output.Validate()
}

// ReasoningSummary is one summary entry of a reasoning item.
type ReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ReasoningText is one reasoning_text content entry.
type ReasoningText struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

// ReasoningInput is a reasoning input item. Summary must be present (use an
// empty array); content and encrypted_content are provider-opaque and only
// ever passed through unchanged.
type ReasoningInput struct {
	ID               string             `json:"id"`
	Type             string             `json:"type"`
	Status           ItemStatus         `json:"status,omitempty"`
	Summary          []ReasoningSummary `json:"summary"`
	Content          []ReasoningText    `json:"content,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
}

func (*ReasoningInput) isInputItem() {}

func (r *ReasoningInput) Validate() error {
	if r.ID == "" {
		return errors.New("reasoning item id is empty")
	}
	if r.Type != "reasoning" {
		return fmt.Errorf("reasoning item type = %q", r.Type)
	}
	if r.Summary == nil {
		return errors.New("reasoning summary must be present; use an empty array")
	}
	if r.Status != "" && !ValidStatus(r.Status) {
		return fmt.Errorf("invalid reasoning status %q", r.Status)
	}
	for i, summary := range r.Summary {
		if summary.Type != "summary_text" {
			return fmt.Errorf("reasoning summary %d type = %q", i, summary.Type)
		}
	}
	for i, content := range r.Content {
		if content.Type != "reasoning_text" {
			return fmt.Errorf("reasoning content %d type = %q", i, content.Type)
		}
	}
	return nil
}

// ItemReferenceInput is an item_reference input item.
type ItemReferenceInput struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (*ItemReferenceInput) isInputItem() {}

func (r *ItemReferenceInput) Validate() error {
	if r.Type != "item_reference" {
		return fmt.Errorf("item reference type = %q", r.Type)
	}
	if r.ID == "" {
		return errors.New("item reference id is empty")
	}
	return nil
}

// InputItems is the item-list arm of the request input union.
type InputItems []InputItem

// UnmarshalJSON decodes each item through its tagged variant.
func (items *InputItems) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := wire.Decode(data, &raw); err != nil {
		return err
	}

	out := make(InputItems, 0, len(raw))
	for i, itemData := range raw {
		item, err := DecodeInputItem(itemData)
		if err != nil {
			return fmt.Errorf("input item %d: %w", i, err)
		}
		out = append(out, item)
	}
	*items = out
	return nil
}

// MarshalJSON validates and emits every item.
func (items InputItems) MarshalJSON() ([]byte, error) {
	raw := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		if item == nil {
			return nil, fmt.Errorf("input item %d is nil", i)
		}
		if err := item.Validate(); err != nil {
			return nil, fmt.Errorf("input item %d: %w", i, err)
		}
		b, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		raw = append(raw, b)
	}
	return json.Marshal(raw)
}

// DecodeInputItem dispatches on the type tag. A message whose role is
// assistant and carries an ID or status is a previous output message; any
// other message is an easy input message. Unknown item types produce a
// wire.UnsupportedTypeError identifying the exact type.
func DecodeInputItem(data []byte) (InputItem, error) {
	// The type tag is OPTIONAL for input items (the easy input message
	// omits it and defaults to "message"), so a type-less OBJECT is legal —
	// only a null or empty payload is corrupt.
	if len(wire.TrimSpace(data)) == 0 || bytes.Equal(wire.TrimSpace(data), []byte("null")) {
		return nil, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "type",
			Message: "input item is null or empty",
		}
	}
	var probe struct {
		Type   string     `json:"type"`
		Role   InputRole  `json:"role"`
		ID     string     `json:"id"`
		Status ItemStatus `json:"status"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}

	var item InputItem
	switch probe.Type {
	case "", "message":
		if probe.Role == InputRoleAssistant &&
			(probe.ID != "" || probe.Status != "") {
			item = &PreviousOutputMessage{}
		} else {
			item = &EasyInputMessage{}
		}

	case "function_call":
		item = &FunctionCallInput{}

	case "function_call_output":
		item = &FunctionCallOutputInput{}

	case "reasoning":
		item = &ReasoningInput{}

	case "item_reference":
		item = &ItemReferenceInput{}

	default:
		return nil, &wire.UnsupportedTypeError{
			Protocol: "responses",
			Path:     "input[].type",
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
