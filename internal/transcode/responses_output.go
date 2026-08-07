package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Source contracts:
//
// Output item union:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L7860-L7910
//
// Output message:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8398-L8425
//
// Output text requires annotations and text:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8681-L8719
//
// Refusal is a message content variant:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8630-L8661

// ResponsesAnnotation is one annotation on an output_text part. Only the type
// tag is modeled; variant-specific fields are added only as supported, and
// unknown annotation types are rejected rather than represented by an
// "everything optional" struct.
type ResponsesAnnotation struct {
	Type string `json:"type"`
}

// ResponsesTextLogprob is one token log-probability entry.
type ResponsesTextLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// ResponsesOutputContentPart is one content part of an output message.
type ResponsesOutputContentPart interface {
	isResponsesOutputContentPart()
	Validate() error
}

// ResponsesOutputText is an output_text content part. Annotations are
// required on the wire; use an empty array when there are none.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8681-L8719
type ResponsesOutputText struct {
	Type        string                 `json:"type"`
	Text        string                 `json:"text"`
	Annotations []ResponsesAnnotation  `json:"annotations"`
	Logprobs    []ResponsesTextLogprob `json:"logprobs,omitempty"`
}

func (*ResponsesOutputText) isResponsesOutputContentPart() {}

func (p *ResponsesOutputText) Validate() error {
	if p.Type != "output_text" {
		return fmt.Errorf("output text type = %q", p.Type)
	}
	if p.Annotations == nil {
		return errors.New("output_text annotations must be present; use an empty array")
	}
	return nil
}

// ResponsesOutputRefusal is a refusal content part of an output message.
// Refusal is modeled as message content, never as a standalone item type.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8630-L8661
type ResponsesOutputRefusal struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

func (*ResponsesOutputRefusal) isResponsesOutputContentPart() {}

func (p *ResponsesOutputRefusal) Validate() error {
	if p.Type != "refusal" {
		return fmt.Errorf("refusal part type = %q", p.Type)
	}
	return nil
}

// ResponsesOutputContentParts is the content array of an output message. It
// must be present on the wire; use an empty array when there is no content.
type ResponsesOutputContentParts []ResponsesOutputContentPart

// Validate checks presence and every part.
func (parts ResponsesOutputContentParts) Validate() error {
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
func (parts *ResponsesOutputContentParts) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := strictDecode(data, &raw); err != nil {
		return err
	}

	out := make(ResponsesOutputContentParts, 0, len(raw))
	for i, partData := range raw {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(partData, &probe); err != nil {
			return fmt.Errorf("output content part %d: %w", i, err)
		}

		var part ResponsesOutputContentPart
		switch probe.Type {
		case "output_text":
			part = &ResponsesOutputText{}
		case "refusal":
			part = &ResponsesOutputRefusal{}
		default:
			return &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     fmt.Sprintf("output[].content[%d].type", i),
				Feature:  probe.Type,
			}
		}

		if err := strictDecode(partData, part); err != nil {
			return fmt.Errorf("output content part %d: %w", i, err)
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
func (parts ResponsesOutputContentParts) MarshalJSON() ([]byte, error) {
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

// ResponsesOutputItem is one tagged variant of the output item union.
type ResponsesOutputItem interface {
	isResponsesOutputItem()
	Validate() error
}

// ResponsesOutputMessage is an assistant output message.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L8398-L8425
type ResponsesOutputMessage struct {
	ID      string                      `json:"id"`
	Type    string                      `json:"type"`
	Role    string                      `json:"role"`
	Status  ResponsesItemStatus         `json:"status"`
	Phase   string                      `json:"phase,omitempty"`
	Content ResponsesOutputContentParts `json:"content"`
}

func (*ResponsesOutputMessage) isResponsesOutputItem() {}

func (m *ResponsesOutputMessage) Validate() error {
	if m.ID == "" {
		return errors.New("output message id is empty")
	}
	if m.Type != "message" {
		return fmt.Errorf("output message type = %q", m.Type)
	}
	if m.Role != "assistant" {
		return fmt.Errorf("output message role = %q", m.Role)
	}
	if !validStatus(m.Status) {
		return fmt.Errorf("invalid output message status %q", m.Status)
	}
	if m.Phase != "" && m.Phase != "commentary" && m.Phase != "final_answer" {
		return fmt.Errorf("invalid output message phase %q", m.Phase)
	}
	return m.Content.Validate()
}

// ResponsesFunctionCallOutputItem is an output function_call item.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L3762-L3813
type ResponsesFunctionCallOutputItem struct {
	ID        string              `json:"id"`
	Type      string              `json:"type"`
	Status    ResponsesItemStatus `json:"status"`
	CallID    string              `json:"call_id"`
	Name      string              `json:"name"`
	Arguments string              `json:"arguments"`
}

func (*ResponsesFunctionCallOutputItem) isResponsesOutputItem() {}

func (c *ResponsesFunctionCallOutputItem) Validate() error {
	if c.ID == "" {
		return errors.New("output function call id is empty")
	}
	if c.Type != "function_call" {
		return fmt.Errorf("output function call type = %q", c.Type)
	}
	// status is optional on the wire: the official function-call output
	// events omit it while the call is in progress.
	if c.Status != "" && !validStatus(c.Status) {
		return fmt.Errorf("invalid output function call status %q", c.Status)
	}
	if c.CallID == "" || c.Name == "" {
		return errors.New("output function call requires call_id and name")
	}
	// arguments may be empty on the wire: the official added event carries
	// an empty arguments string, with the payload arriving via deltas.
	if c.Arguments != "" && !json.Valid([]byte(c.Arguments)) {
		return errors.New("output function call arguments are invalid JSON")
	}
	return nil
}

// ResponsesFunctionCallOutputResultItem is an output function_call_output
// item. It appears in the output array when a function result is included in
// the response.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L3780-L3813
type ResponsesFunctionCallOutputResultItem struct {
	ID     string                  `json:"id"`
	Type   string                  `json:"type"`
	Status ResponsesItemStatus     `json:"status"`
	CallID string                  `json:"call_id"`
	Output ResponsesFunctionOutput `json:"output"`
}

func (*ResponsesFunctionCallOutputResultItem) isResponsesOutputItem() {}

func (c *ResponsesFunctionCallOutputResultItem) Validate() error {
	if c.ID == "" {
		return errors.New("output function call output id is empty")
	}
	if c.Type != "function_call_output" {
		return fmt.Errorf("output function call output type = %q", c.Type)
	}
	if c.CallID == "" {
		return errors.New("output function call output requires call_id")
	}
	if c.Status != "" && !validStatus(c.Status) {
		return fmt.Errorf("invalid output function call output status %q", c.Status)
	}
	return c.Output.Validate()
}

// ResponsesReasoningOutputItem is an output reasoning item.
//
// https://github.com/openai/openai-go/blob/main/responses/response.go#L9562-L9639
type ResponsesReasoningOutputItem struct {
	ID               string                      `json:"id"`
	Type             string                      `json:"type"`
	Status           ResponsesItemStatus         `json:"status"`
	Summary          []ResponsesReasoningSummary `json:"summary"`
	Content          []ResponsesReasoningText    `json:"content,omitempty"`
	EncryptedContent string                      `json:"encrypted_content,omitempty"`
}

func (*ResponsesReasoningOutputItem) isResponsesOutputItem() {}

func (r *ResponsesReasoningOutputItem) Validate() error {
	if r.ID == "" {
		return errors.New("output reasoning id is empty")
	}
	if r.Type != "reasoning" {
		return fmt.Errorf("output reasoning type = %q", r.Type)
	}
	if !validStatus(r.Status) {
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

// DecodeResponsesOutputItem decodes one output item through its tagged
// variant. Unknown item types produce an UnsupportedFeatureError identifying
// the exact type, never a silent drop.
func DecodeResponsesOutputItem(data []byte) (ResponsesOutputItem, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}

	var item ResponsesOutputItem
	switch probe.Type {
	case "message":
		item = &ResponsesOutputMessage{}
	case "function_call":
		item = &ResponsesFunctionCallOutputItem{}
	case "function_call_output":
		item = &ResponsesFunctionCallOutputResultItem{}
	case "reasoning":
		item = &ResponsesReasoningOutputItem{}
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "output[].type",
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
