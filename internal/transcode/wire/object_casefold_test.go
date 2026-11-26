package wire

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestDecodeRejectsCaseVariantDuplicateKeys proves that a case alias of an
// already-present modeled field cannot overwrite it through encoding/json's
// case-insensitive struct matching. Raw object keys retain their distinct JSON
// identity. Strict decode rejects aliases; tolerant decode skips them as
// unknown fields, preserving the value written under the exact modeled name.
func TestDecodeRejectsCaseVariantDuplicateKeys(t *testing.T) {
	type doc struct {
		StopReason *string `json:"stop_reason"`
		Nested     struct {
			Text *string `json:"text"`
		} `json:"nested"`
		Upper *string `json:"UPPER"`
	}
	reject := map[string]struct{ body, canonical string }{
		"uppercase alias":   {`{"stop_reason":"refusal","STOP_REASON":"end_turn"}`, `{"stop_reason":"refusal"}`},
		"mixed case alias":  {`{"stop_reason":"refusal","Stop_Reason":"end_turn"}`, `{"stop_reason":"refusal"}`},
		"field alias":       {`{"nested":{"text":"a"},"NESTED":{"text":"b"}}`, `{"nested":{"text":"a"}}`},
		"all lower vs caps": {`{"UPPER":"a","upper":"b"}`, `{"UPPER":"a"}`},
	}
	for name, test := range reject {
		docJSON := test.body
		t.Run("Decode/"+name, func(t *testing.T) {
			var out doc
			err := Decode([]byte(docJSON), &out)
			var de *DecodeError
			if !errors.As(err, &de) || de.Kind != DecodeDuplicateKey {
				t.Fatalf("Decode(%s): err = %v, want DecodeDuplicateKey", docJSON, err)
			}
		})
		t.Run("DecodeTolerant/"+name, func(t *testing.T) {
			var out, canonical doc
			if err := Decode([]byte(test.canonical), &canonical); err != nil {
				t.Fatal(err)
			}
			if err := DecodeTolerant([]byte(docJSON), &out); err != nil {
				t.Fatalf("DecodeTolerant(%s): %v", docJSON, err)
			}
			if !reflect.DeepEqual(out, canonical) {
				t.Fatalf("unknown alias changed the canonical value: got %+v, want %+v", out, canonical)
			}
		})
	}

	// Distinct keys and case-variant keys in DIFFERENT objects are fine.
	accept := map[string]string{
		"distinct keys": `{"stop_reason":"refusal","nested":{"text":"a"},"UPPER":"x"}`,
	}
	for name, docJSON := range accept {
		t.Run("accept/"+name, func(t *testing.T) {
			var out doc
			if err := Decode([]byte(docJSON), &out); err != nil {
				t.Fatalf("Decode(%s): err = %v, want nil", docJSON, err)
			}
		})
	}

	// A case-variant key inside a NESTED object is a duplicate of that
	// object's own key, still rejected; but the same spelling in a different
	// scope is not a duplicate of the outer key. DecodeTolerant accepts the
	// unknown nested key, so the scope distinction is checked through it.
	t.Run("case variant in a different scope is tolerated not a duplicate", func(t *testing.T) {
		var out map[string]any
		if err := DecodeTolerant([]byte(`{"stop_reason":"refusal","nested":{"STOP_REASON":"ignored"}}`), &out); err != nil {
			t.Fatalf("DecodeTolerant: err = %v, want nil (nested scope is separate)", err)
		}
	})
	t.Run("case variant within one nested object is a duplicate", func(t *testing.T) {
		var out doc
		err := Decode([]byte(`{"nested":{"text":"a","TEXT":"b"}}`), &out)
		var de *DecodeError
		if !errors.As(err, &de) || de.Kind != DecodeDuplicateKey {
			t.Fatalf("nested case-variant: err = %v, want DecodeDuplicateKey", err)
		}
	})

	// The error names the original key spelling.
	var out doc
	err := Decode([]byte(`{"stop_reason":"refusal","STOP_REASON":"end_turn"}`), &out)
	if err == nil || !strings.Contains(err.Error(), `"STOP_REASON"`) {
		t.Fatalf("error should quote the duplicate spelling: %v", err)
	}
}
