package geminigeneratecontent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func requireDecodeErrorKind(t *testing.T, err error, kind wire.DecodeErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s decode error, got nil", kind)
	}
	var decodeErr *wire.DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("error %v is not a *wire.DecodeError", err)
	}
	if decodeErr.Kind != kind {
		t.Fatalf("error kind = %s, want %s (error: %v)", decodeErr.Kind, kind, err)
	}
}

func TestPartValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantKind wire.DecodeErrorKind
		wantErr  bool
	}{
		{
			name: "text with thought and signature",
			body: `{"text":"hello","thought":true,"thoughtSignature":"c2ln"}`,
		},
		{
			name: "signature only",
			body: `{"thoughtSignature":"c2ln"}`,
		},
		{
			name: "inline data with modifiers",
			body: `{"inlineData":{"mimeType":"image/png","data":"aGk="},"videoMetadata":{"startOffset":"1s"},"mediaResolution":"MEDIA_RESOLUTION_HIGH","mediaProcessing":"STATIC"}`,
		},
		{
			name: "function call with object args",
			body: `{"functionCall":{"id":"call-1","name":"lookup","args":{"q":"x"}}}`,
		},
		{
			name: "function response with object response",
			body: `{"functionResponse":{"id":"call-1","name":"lookup","response":{"output":"ok"}}}`,
		},
		{
			name:     "empty part",
			body:     `{}`,
			wantErr:  true,
			wantKind: wire.DecodeMissingRequired,
		},
		{
			name:     "modifier without payload",
			body:     `{"thought":true}`,
			wantErr:  true,
			wantKind: wire.DecodeMissingRequired,
		},
		{
			name:     "two payload members",
			body:     `{"text":"hello","functionCall":{"name":"lookup"}}`,
			wantErr:  true,
			wantKind: wire.DecodeContradictoryUnion,
		},
		{
			name:     "unknown member",
			body:     `{"text":"hello","bogus":true}`,
			wantErr:  true,
			wantKind: wire.DecodeUnknownField,
		},
		{
			name:     "unknown media processing value",
			body:     `{"text":"hello","mediaProcessing":"DYNAMIC"}`,
			wantErr:  true,
			wantKind: wire.DecodeMalformed,
		},
		{
			name:     "function call args array",
			body:     `{"functionCall":{"name":"lookup","args":[]}}`,
			wantErr:  true,
			wantKind: wire.DecodeMalformed,
		},
		{
			name:     "function response response array",
			body:     `{"functionResponse":{"name":"lookup","response":[]}}`,
			wantErr:  true,
			wantKind: wire.DecodeMalformed,
		},
		{
			name:     "duplicate member",
			body:     `{"text":"a","text":"b"}`,
			wantErr:  true,
			wantKind: wire.DecodeDuplicateKey,
		},
		{
			name:     "trailing value",
			body:     `{"text":"a"}{"text":"b"}`,
			wantErr:  true,
			wantKind: wire.DecodeTrailingValue,
		},
		{
			name:     "explicit null raw payload leaves the part without a payload",
			body:     `{"executableCode":null}`,
			wantErr:  true,
			wantKind: wire.DecodeMissingRequired,
		},
		{
			name: "explicit null raw payload beside a text payload",
			body: `{"executableCode":null,"text":"x"}`,
		},
		{
			name: "explicit null raw payload beside a signature",
			body: `{"toolCall":null,"thoughtSignature":"c2ln"}`,
		},
		{
			name: "explicit null pointer payload beside a signature",
			body: `{"text":null,"thoughtSignature":"c2ln"}`,
		},
		{
			name:     "explicit null function call is absent",
			body:     `{"functionCall":null}`,
			wantErr:  true,
			wantKind: wire.DecodeMissingRequired,
		},
		{
			name: "explicit null function call args is absent",
			body: `{"functionCall":{"name":"lookup","args":null}}`,
		},
		{
			name: "explicit null function response is absent",
			body: `{"functionResponse":{"name":"lookup","response":null}}`,
		},
		{
			name:     "null part",
			body:     `null`,
			wantErr:  true,
			wantKind: wire.DecodeIllegalNull,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var part Part
			err := wire.Decode([]byte(test.body), &part)
			if !test.wantErr {
				if err != nil {
					t.Fatalf("decode failed: %v", err)
				}
				return
			}
			requireDecodeErrorKind(t, err, test.wantKind)
		})
	}
}

func TestRawMemberPresent(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "absent", raw: nil, want: false},
		{name: "explicit null", raw: json.RawMessage("null"), want: false},
		{name: "padded null", raw: json.RawMessage(" null "), want: false},
		{name: "empty object", raw: json.RawMessage("{}"), want: true},
		{name: "false", raw: json.RawMessage("false"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RawMemberPresent(test.raw); got != test.want {
				t.Fatalf("RawMemberPresent(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestPartValidateConstructed(t *testing.T) {
	text := "hello"
	signature := "c2ln"
	media := "AGENTIC"
	if err := (Part{Text: &text, ThoughtSignature: &signature, MediaProcessing: &media}).Validate(); err != nil {
		t.Fatalf("valid part rejected: %v", err)
	}
	if err := (Part{}).Validate(); err == nil {
		t.Fatal("empty part accepted")
	}
}
