package transcode

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestConvertGeminiSchema(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "scalar type maps to JSON Schema name",
			input: `{"type":"STRING"}`,
			want:  `{"type":"string"}`,
		},
		{
			name:  "integer and boolean types map",
			input: `{"type":"INTEGER"}`,
			want:  `{"type":"integer"}`,
		},
		{
			name:  "NULL type maps to null",
			input: `{"type":"NULL"}`,
			want:  `{"type":"null"}`,
		},
		{
			name:  "TYPE_UNSPECIFIED is omitted",
			input: `{"type":"TYPE_UNSPECIFIED","description":"any"}`,
			want:  `{"description":"any"}`,
		},
		{
			name:  "nullable widens the type with null",
			input: `{"type":"STRING","nullable":true}`,
			want:  `{"type":["string","null"]}`,
		},
		{
			name:    "nullable without a type is rejected",
			input:   `{"nullable":true}`,
			wantErr: true,
		},
		{
			name:  "nullable false keeps the plain type",
			input: `{"type":"NUMBER","nullable":false}`,
			want:  `{"type":"number"}`,
		},
		{
			name:  "verbatim members carry over",
			input: `{"type":"STRING","format":"date","title":"T","description":"d","pattern":"^a","default":"x","enum":["a","b"],"required":["a"],"minimum":1.5,"maximum":9}`,
			want:  `{"type":"string","format":"date","title":"T","description":"d","pattern":"^a","default":"x","enum":["a","b"],"required":["a"],"minimum":1.5,"maximum":9}`,
		},
		{
			name:  "int64 members accept a number",
			input: `{"type":"ARRAY","minItems":2,"maxItems":5}`,
			want:  `{"type":"array","minItems":2,"maxItems":5}`,
		},
		{
			name:  "int64 members accept a decimal string",
			input: `{"type":"STRING","minLength":"3","maxLength":"12"}`,
			want:  `{"type":"string","minLength":3,"maxLength":12}`,
		},
		{
			name:    "int64 member rejects a non-integer",
			input:   `{"type":"STRING","minLength":1.5}`,
			wantErr: true,
		},
		{
			name:    "int64 member rejects a non-numeric string",
			input:   `{"type":"OBJECT","maxProperties":"many"}`,
			wantErr: true,
		},
		{
			name:  "items converts recursively",
			input: `{"type":"ARRAY","items":{"type":"INTEGER","nullable":true}}`,
			want:  `{"type":"array","items":{"type":["integer","null"]}}`,
		},
		{
			name:  "properties convert recursively",
			input: `{"type":"OBJECT","properties":{"city":{"type":"STRING"},"age":{"type":"INTEGER"}}}`,
			want:  `{"type":"object","properties":{"age":{"type":"integer"},"city":{"type":"string"}}}`,
		},
		{
			name:  "anyOf arms convert recursively",
			input: `{"anyOf":[{"type":"STRING"},{"type":"NUMBER"}]}`,
			want:  `{"anyOf":[{"type":"string"},{"type":"number"}]}`,
		},
		{
			name:  "example becomes a one-element examples array",
			input: `{"type":"STRING","example":"hi"}`,
			want:  `{"type":"string","examples":["hi"]}`,
		},
		{
			name:  "propertyOrdering has no JSON Schema counterpart and is dropped",
			input: `{"type":"OBJECT","properties":{"a":{"type":"STRING"}},"propertyOrdering":["a"]}`,
			want:  `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name:    "unknown member is rejected rather than dropped",
			input:   `{"type":"STRING","x-hint":"y"}`,
			wantErr: true,
		},
		{
			name:    "unsupported type name is rejected",
			input:   `{"type":"DATE"}`,
			wantErr: true,
		},
		{
			name:    "non-object input is rejected",
			input:   `"STRING"`,
			wantErr: true,
		},
		{
			name:    "unknown member inside a nested property is rejected",
			input:   `{"type":"OBJECT","properties":{"a":{"type":"STRING","bogus":1}}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertGeminiSchema(json.RawMessage(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ConvertGeminiSchema(%s) = %s, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConvertGeminiSchema(%s) error: %v", tt.input, err)
			}
			var gotValue, wantValue any
			if err := json.Unmarshal(got, &gotValue); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantValue); err != nil {
				t.Fatalf("fixture is not JSON: %v", err)
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Fatalf("ConvertGeminiSchema(%s)\n got  %s\n want %s", tt.input, got, tt.want)
			}
		})
	}
}
