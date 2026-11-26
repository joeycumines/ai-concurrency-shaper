package openairesponses

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestOutputUnionNullDiscriminatorClassification(t *testing.T) {
	decoders := map[string]func([]byte) error{
		"output item": func(raw []byte) error {
			_, err := DecodeOutputItem(raw)
			return err
		},
		"stream content part": func(raw []byte) error {
			_, err := decodeStreamContentPart(raw)
			return err
		},
		"output content part": func(raw []byte) error {
			var parts OutputContentParts
			return json.Unmarshal([]byte("["+string(raw)+"]"), &parts)
		},
	}
	for name, decode := range decoders {
		for _, tc := range []struct {
			raw  string
			kind wire.DecodeErrorKind
		}{
			{"null", wire.DecodeMissingRequired},
			{" \n null\t", wire.DecodeMissingRequired},
			{`{}`, wire.DecodeMissingRequired},
			{`{"TYPE":"output_text"}`, wire.DecodeMissingRequired},
			{`{"type":null}`, wire.DecodeIllegalNull},
			{`{"TYPE":"output_text","type":null}`, wire.DecodeIllegalNull},
		} {
			t.Run(name+"/"+tc.raw, func(t *testing.T) {
				var malformed *wire.DecodeError
				if err := decode([]byte(tc.raw)); !errors.As(err, &malformed) || malformed.Kind != tc.kind {
					t.Fatalf("error = %v, want %s", err, tc.kind)
				}
			})
		}
	}
}
