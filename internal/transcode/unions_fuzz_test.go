package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// strictWireUnion is a wire union that must round-trip: any value accepted by
// UnmarshalJSON validates, and re-encoding yields a semantically identical
// document that decodes again.
type strictWireUnion interface {
	json.Unmarshaler
	json.Marshaler
	Validate() error
}

func FuzzWireUnions(f *testing.F) {
	seeds := [][]byte{
		[]byte(`"hello"`),
		[]byte(`[]`),
		[]byte(`[{"role":"user","content":"hello"}]`),
		[]byte(`{"role":"user","content":[{"type":"input_text","text":"hello"}]}`),
		[]byte(`{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}`),
		[]byte(`{"type":"function_call_output","call_id":"call_1","output":"ok"}`),
		[]byte(`{"type":"reasoning","id":"rs_1","summary":[]}`),
		[]byte(`{"type":"item_reference","id":"item_1"}`),
		[]byte(`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[]}`),
		[]byte(`{"type":"output_text","text":"","annotations":[]}`),
		[]byte(`null`),
		[]byte(`123`),
		[]byte(`{"type":"unknown"}`),
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		const maxInput = 1 << 20
		if len(data) > maxInput {
			t.Skip()
		}

		cases := []struct {
			name string
			new  func() strictWireUnion
		}{
			{
				name: "responses request input",
				new: func() strictWireUnion {
					return &ResponsesInput{}
				},
			},
			{
				name: "responses input-item envelope",
				new: func() strictWireUnion {
					return &ResponsesInputItemEnvelope{}
				},
			},
			{
				name: "responses output-item envelope",
				new: func() strictWireUnion {
					return &ResponsesOutputItemEnvelope{}
				},
			},
			{
				name: "responses tool choice",
				new: func() strictWireUnion {
					return &ResponsesToolChoice{}
				},
			},
			{
				name: "chat message content",
				new: func() strictWireUnion {
					return &ChatMessageContent{}
				},
			},
			{
				name: "chat tool choice",
				new: func() strictWireUnion {
					return &ChatToolChoice{}
				},
			},
			{
				name: "anthropic content",
				new: func() strictWireUnion {
					return &AnthropicContent{}
				},
			},
		}

		for _, tc := range cases {
			first := tc.new()
			err := json.Unmarshal(data, first)
			if err != nil {
				continue
			}

			if err := first.Validate(); err != nil {
				t.Fatalf(
					"%s accepted invalid value: %v\ninput=%s",
					tc.name,
					err,
					data,
				)
			}

			encoded, err := json.Marshal(first)
			if err != nil {
				t.Fatalf("%s marshal after successful decode: %v", tc.name, err)
			}
			if !json.Valid(encoded) {
				t.Fatalf("%s emitted invalid JSON: %s", tc.name, encoded)
			}

			second := tc.new()
			if err := json.Unmarshal(encoded, second); err != nil {
				t.Fatalf(
					"%s cannot decode its own encoding: %v\nencoded=%s",
					tc.name,
					err,
					encoded,
				)
			}
			if err := second.Validate(); err != nil {
				t.Fatalf("%s re-decoded invalid value: %v", tc.name, err)
			}

			if !semanticJSONEqual(encoded, mustMarshalJSON(t, second)) {
				t.Fatalf(
					"%s unstable round trip:\nfirst=%s\nsecond=%s",
					tc.name,
					encoded,
					mustMarshalJSON(t, second),
				)
			}
		}
	})
}

func semanticJSONEqual(a, b []byte) bool {
	var av, bv any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&av) != nil || db.Decode(&bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return data
}

// The envelopes expose the interface unions to encoding/json.
type ResponsesInputItemEnvelope struct {
	Value ResponsesInputItem
}

func (e *ResponsesInputItemEnvelope) UnmarshalJSON(data []byte) error {
	item, err := decodeResponsesInputItem(data)
	if err != nil {
		return err
	}
	e.Value = item
	return nil
}

func (e ResponsesInputItemEnvelope) MarshalJSON() ([]byte, error) {
	if e.Value == nil {
		return nil, fmt.Errorf("nil input item")
	}
	return json.Marshal(e.Value)
}

func (e ResponsesInputItemEnvelope) Validate() error {
	if e.Value == nil {
		return fmt.Errorf("nil input item")
	}
	return e.Value.Validate()
}

type ResponsesOutputItemEnvelope struct {
	Value ResponsesOutputItem
}

func (e *ResponsesOutputItemEnvelope) UnmarshalJSON(data []byte) error {
	item, err := DecodeResponsesOutputItem(data)
	if err != nil {
		return err
	}
	e.Value = item
	return nil
}

func (e ResponsesOutputItemEnvelope) MarshalJSON() ([]byte, error) {
	if e.Value == nil {
		return nil, fmt.Errorf("nil output item")
	}
	return json.Marshal(e.Value)
}

func (e ResponsesOutputItemEnvelope) Validate() error {
	if e.Value == nil {
		return fmt.Errorf("nil output item")
	}
	return e.Value.Validate()
}
