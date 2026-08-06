package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Source contracts:
//
// Request-level input is string OR input-item list:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L24183-L24204
//
// Input item union:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L13164-L13205
//
// Easy input message:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L1895-L1921
//
// Function call:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8964-L8990
//
// Function call output:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L14335-L14361
//
// Reasoning item:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L19277-L19302
//
// Item reference:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L15515-L15525

// ResponsesInputRole is the role of an easy input message.
type ResponsesInputRole string

const (
	ResponsesInputRoleUser      ResponsesInputRole = "user"
	ResponsesInputRoleAssistant ResponsesInputRole = "assistant"
	ResponsesInputRoleSystem    ResponsesInputRole = "system"
	ResponsesInputRoleDeveloper ResponsesInputRole = "developer"
)

// ResponsesItemStatus is the lifecycle status of an input or output item.
type ResponsesItemStatus string

const (
	ResponsesItemInProgress ResponsesItemStatus = "in_progress"
	ResponsesItemCompleted  ResponsesItemStatus = "completed"
	ResponsesItemIncomplete ResponsesItemStatus = "incomplete"
)

// validStatus reports whether s is a legal ResponsesItemStatus value.
func validStatus(s ResponsesItemStatus) bool {
	switch s {
	case ResponsesItemInProgress,
		ResponsesItemCompleted,
		ResponsesItemIncomplete:
		return true
	default:
		return false
	}
}

// -----------------------------------------------------------------------------
// Request-level input union
// -----------------------------------------------------------------------------

// ResponsesInput models the request-level input union: either a plain string
// or a list of input items. Exactly one arm is selected.
type ResponsesInput struct {
	Text  *string
	Items ResponsesInputItems // nil means the list arm is not selected
}

// Validate checks the union invariants and each selected item.
func (v ResponsesInput) Validate() error {
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
func (v *ResponsesInput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty responses input")
	}

	switch data[0] {
	case '"':
		var text string
		if err := strictDecode(data, &text); err != nil {
			return err
		}
		v.Text = &text
		v.Items = nil
		return nil

	case '[':
		var items ResponsesInputItems
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
func (v ResponsesInput) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	if v.Text != nil {
		return json.Marshal(*v.Text)
	}
	return json.Marshal(v.Items)
}

// -----------------------------------------------------------------------------
// Input message content
// -----------------------------------------------------------------------------

// ResponsesInputContentPart is one content part of an input message.
type ResponsesInputContentPart interface {
	isResponsesInputContentPart()
	Validate() error
}

// ResponsesInputText is an input_text content part.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L6919-L6950
type ResponsesInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (ResponsesInputText) isResponsesInputContentPart() {}

func (p ResponsesInputText) Validate() error {
	if p.Type != "input_text" {
		return fmt.Errorf("input text type = %q, want input_text", p.Type)
	}
	return nil
}

// ResponsesInputImage is an input_image content part. The official shape uses
// image_url (including base64 data URLs) or file_id — never a private
// image_data field.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L4670-L4710
type ResponsesInputImage struct {
	Type     string `json:"type"`
	Detail   string `json:"detail"`
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

func (ResponsesInputImage) isResponsesInputContentPart() {}

func (p ResponsesInputImage) Validate() error {
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

// ResponsesInputFile is an input_file content part.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L4603-L4643
type ResponsesInputFile struct {
	Type     string `json:"type"`
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	Filename string `json:"filename,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (ResponsesInputFile) isResponsesInputContentPart() {}

func (p ResponsesInputFile) Validate() error {
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

// ResponsesInputContentParts is the array arm of message content.
type ResponsesInputContentParts []ResponsesInputContentPart

// UnmarshalJSON decodes each part through its tagged variant.
func (p *ResponsesInputContentParts) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := strictDecode(data, &raw); err != nil {
		return err
	}

	out := make(ResponsesInputContentParts, 0, len(raw))
	for i, item := range raw {
		part, err := decodeResponsesInputContentPart(item)
		if err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
		out = append(out, part)
	}
	*p = out
	return nil
}

// MarshalJSON validates and emits every part.
func (p ResponsesInputContentParts) MarshalJSON() ([]byte, error) {
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

// decodeResponsesInputContentPart dispatches on the type tag.
func decodeResponsesInputContentPart(
	data []byte,
) (ResponsesInputContentPart, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}

	var part ResponsesInputContentPart
	switch probe.Type {
	case "input_text":
		part = &ResponsesInputText{}
	case "input_image":
		part = &ResponsesInputImage{}
	case "input_file":
		part = &ResponsesInputFile{}
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "input[].content[].type",
			Feature:  probe.Type,
		}
	}

	if err := strictDecode(data, part); err != nil {
		return nil, err
	}
	if err := part.Validate(); err != nil {
		return nil, err
	}
	return part, nil
}

// ResponsesInputMessageContent is a string-or-parts union for message content.
type ResponsesInputMessageContent struct {
	Text  *string
	Parts ResponsesInputContentParts
}

// Validate checks the union invariants.
func (c ResponsesInputMessageContent) Validate() error {
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
func (c *ResponsesInputMessageContent) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty message content")
	}
	if data[0] == '"' {
		var text string
		if err := strictDecode(data, &text); err != nil {
			return err
		}
		c.Text = &text
		c.Parts = nil
		return nil
	}
	if data[0] == '[' {
		var parts ResponsesInputContentParts
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
func (c ResponsesInputMessageContent) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Text != nil {
		return json.Marshal(*c.Text)
	}
	return json.Marshal(c.Parts)
}

// -----------------------------------------------------------------------------
// Input item variants
// -----------------------------------------------------------------------------

// ResponsesInputItem is one tagged variant of the request input item union.
type ResponsesInputItem interface {
	isResponsesInputItem()
	Validate() error
}

// ResponsesEasyInputMessage is an easy input message without an ID.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L228-L272
type ResponsesEasyInputMessage struct {
	Type    string                       `json:"type,omitempty"`
	Role    ResponsesInputRole           `json:"role"`
	Content ResponsesInputMessageContent `json:"content"`
	Phase   string                       `json:"phase,omitempty"`
}

func (*ResponsesEasyInputMessage) isResponsesInputItem() {}

func (m *ResponsesEasyInputMessage) Validate() error {
	if m.Type != "" && m.Type != "message" {
		return fmt.Errorf("message type = %q", m.Type)
	}
	switch m.Role {
	case ResponsesInputRoleUser,
		ResponsesInputRoleAssistant,
		ResponsesInputRoleSystem,
		ResponsesInputRoleDeveloper:
	default:
		return fmt.Errorf("invalid easy-message role %q", m.Role)
	}
	if m.Phase != "" && m.Phase != "commentary" && m.Phase != "final_answer" {
		return fmt.Errorf("invalid assistant phase %q", m.Phase)
	}
	return m.Content.Validate()
}

// ResponsesPreviousOutputMessage is a previous model output message reused as
// input. It is not the same wire variant as an easy input message: it carries
// an ID, status, assistant role, and output content.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8398-L8425
type ResponsesPreviousOutputMessage struct {
	ID      string                      `json:"id"`
	Type    string                      `json:"type"`
	Role    string                      `json:"role"`
	Status  ResponsesItemStatus         `json:"status"`
	Phase   string                      `json:"phase,omitempty"`
	Content ResponsesOutputContentParts `json:"content"`
}

func (*ResponsesPreviousOutputMessage) isResponsesInputItem() {}

func (m *ResponsesPreviousOutputMessage) Validate() error {
	if m.ID == "" {
		return errors.New("previous output message id is empty")
	}
	if m.Type != "message" {
		return fmt.Errorf("previous output message type = %q", m.Type)
	}
	if m.Role != "assistant" {
		return fmt.Errorf("previous output message role = %q", m.Role)
	}
	if !validStatus(m.Status) {
		return fmt.Errorf("invalid previous output status %q", m.Status)
	}
	return m.Content.Validate()
}

// ResponsesFunctionCallInput is a function_call input item.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L3670-L3716
type ResponsesFunctionCallInput struct {
	ID        string              `json:"id,omitempty"`
	Type      string              `json:"type"`
	Status    ResponsesItemStatus `json:"status,omitempty"`
	CallID    string              `json:"call_id"`
	Name      string              `json:"name"`
	Arguments string              `json:"arguments"`
}

func (*ResponsesFunctionCallInput) isResponsesInputItem() {}

func (c *ResponsesFunctionCallInput) Validate() error {
	if c.Type != "function_call" {
		return fmt.Errorf("function call type = %q", c.Type)
	}
	if c.CallID == "" {
		return errors.New("function call call_id is empty")
	}
	if c.Name == "" {
		return errors.New("function call name is empty")
	}
	if c.Status != "" && !validStatus(c.Status) {
		return fmt.Errorf("invalid function call status %q", c.Status)
	}
	if !json.Valid([]byte(c.Arguments)) {
		return errors.New("function call arguments are not valid JSON")
	}
	return nil
}

// ResponsesFunctionOutput is the output arm of a function_call_output item:
// either a plain string or an array of input content parts.
type ResponsesFunctionOutput struct {
	Text  *string
	Parts ResponsesInputContentParts
}

// Validate checks the union invariants.
func (o ResponsesFunctionOutput) Validate() error {
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
func (o *ResponsesFunctionOutput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty function output")
	}
	if data[0] == '"' {
		var text string
		if err := strictDecode(data, &text); err != nil {
			return err
		}
		o.Text = &text
		o.Parts = nil
		return nil
	}
	if data[0] == '[' {
		var parts ResponsesInputContentParts
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
func (o ResponsesFunctionOutput) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.Text != nil {
		return json.Marshal(*o.Text)
	}
	return json.Marshal(o.Parts)
}

// ResponsesFunctionCallOutputInput is a function_call_output input item.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L5275-L5308
type ResponsesFunctionCallOutputInput struct {
	ID     string                  `json:"id,omitempty"`
	Type   string                  `json:"type"`
	Status ResponsesItemStatus     `json:"status,omitempty"`
	CallID string                  `json:"call_id"`
	Name   string                  `json:"name,omitempty"`
	Output ResponsesFunctionOutput `json:"output"`
}

func (*ResponsesFunctionCallOutputInput) isResponsesInputItem() {}

func (o *ResponsesFunctionCallOutputInput) Validate() error {
	if o.Type != "function_call_output" {
		return fmt.Errorf("function call output type = %q", o.Type)
	}
	if o.CallID == "" {
		return errors.New("function call output call_id is empty")
	}
	if o.Status != "" && !validStatus(o.Status) {
		return fmt.Errorf("invalid function call output status %q", o.Status)
	}
	return o.Output.Validate()
}

// ResponsesReasoningSummary is one summary entry of a reasoning item.
type ResponsesReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesReasoningText is one reasoning_text content entry.
type ResponsesReasoningText struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

// ResponsesReasoningInput is a reasoning input item. Summary must be present
// (use an empty array); content and encrypted_content are provider-opaque and
// only ever passed through unchanged.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L9562-L9639
type ResponsesReasoningInput struct {
	ID               string                      `json:"id"`
	Type             string                      `json:"type"`
	Status           ResponsesItemStatus         `json:"status,omitempty"`
	Summary          []ResponsesReasoningSummary `json:"summary"`
	Content          []ResponsesReasoningText    `json:"content,omitempty"`
	EncryptedContent string                      `json:"encrypted_content,omitempty"`
}

func (*ResponsesReasoningInput) isResponsesInputItem() {}

func (r *ResponsesReasoningInput) Validate() error {
	if r.ID == "" {
		return errors.New("reasoning item id is empty")
	}
	if r.Type != "reasoning" {
		return fmt.Errorf("reasoning item type = %q", r.Type)
	}
	if r.Summary == nil {
		return errors.New("reasoning summary must be present; use an empty array")
	}
	if r.Status != "" && !validStatus(r.Status) {
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

// ResponsesItemReferenceInput is an item_reference input item.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L5585-L5620
type ResponsesItemReferenceInput struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (*ResponsesItemReferenceInput) isResponsesInputItem() {}

func (r *ResponsesItemReferenceInput) Validate() error {
	if r.Type != "item_reference" {
		return fmt.Errorf("item reference type = %q", r.Type)
	}
	if r.ID == "" {
		return errors.New("item reference id is empty")
	}
	return nil
}

// ResponsesInputItems is the item-list arm of the request input union.
type ResponsesInputItems []ResponsesInputItem

// UnmarshalJSON decodes each item through its tagged variant.
func (items *ResponsesInputItems) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := strictDecode(data, &raw); err != nil {
		return err
	}

	out := make(ResponsesInputItems, 0, len(raw))
	for i, itemData := range raw {
		item, err := decodeResponsesInputItem(itemData)
		if err != nil {
			return fmt.Errorf("input item %d: %w", i, err)
		}
		out = append(out, item)
	}
	*items = out
	return nil
}

// MarshalJSON validates and emits every item.
func (items ResponsesInputItems) MarshalJSON() ([]byte, error) {
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

// decodeResponsesInputItem dispatches on the type tag. A message whose role is
// assistant and carries an ID or status is a previous output message; any
// other message is an easy input message.
func decodeResponsesInputItem(data []byte) (ResponsesInputItem, error) {
	var probe struct {
		Type   string              `json:"type"`
		Role   ResponsesInputRole  `json:"role"`
		ID     string              `json:"id"`
		Status ResponsesItemStatus `json:"status"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}

	var item ResponsesInputItem
	switch probe.Type {
	case "", "message":
		if probe.Role == ResponsesInputRoleAssistant &&
			(probe.ID != "" || probe.Status != "") {
			item = &ResponsesPreviousOutputMessage{}
		} else {
			item = &ResponsesEasyInputMessage{}
		}

	case "function_call":
		item = &ResponsesFunctionCallInput{}

	case "function_call_output":
		item = &ResponsesFunctionCallOutputInput{}

	case "reasoning":
		item = &ResponsesReasoningInput{}

	case "item_reference":
		item = &ResponsesItemReferenceInput{}

	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "input[].type",
			Feature:  probe.Type,
		}
	}

	if err := strictDecode(data, item); err != nil {
		return nil, err
	}
	if err := item.Validate(); err != nil {
		return nil, err
	}
	return item, nil
}
