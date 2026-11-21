package wire

import (
	"errors"
	"strings"
	"testing"
)

// TestDecodeRejectsCaseVariantDuplicateKeys proves that two keys in one JSON
// object which differ only by case are duplicates: encoding/json v1 matches
// struct fields case-insensitively, so the later-spelled key would silently
// overwrite the modeled field. Both Decode and DecodeTolerant must reject
// them, while the same key spellings in different objects (different scopes)
// remain accepted.
func TestDecodeRejectsCaseVariantDuplicateKeys(t *testing.T) {
	type doc struct {
		StopReason *string `json:"stop_reason"`
		Nested     struct {
			Text *string `json:"text"`
		} `json:"nested"`
		Upper *string `json:"UPPER"`
	}
	reject := map[string]string{
		"uppercase alias":   `{"stop_reason":"refusal","STOP_REASON":"end_turn"}`,
		"mixed case alias":  `{"stop_reason":"refusal","Stop_Reason":"end_turn"}`,
		"field alias":       `{"nested":{"text":"a"},"NESTED":{"text":"b"}}`,
		"all lower vs caps": `{"UPPER":"a","upper":"b"}`,
	}
	for name, docJSON := range reject {
		t.Run("Decode/"+name, func(t *testing.T) {
			var out doc
			err := Decode([]byte(docJSON), &out)
			var de *DecodeError
			if !errors.As(err, &de) || de.Kind != DecodeDuplicateKey {
				t.Fatalf("Decode(%s): err = %v, want DecodeDuplicateKey", docJSON, err)
			}
		})
		t.Run("DecodeTolerant/"+name, func(t *testing.T) {
			var out doc
			err := DecodeTolerant([]byte(docJSON), &out)
			var de *DecodeError
			if !errors.As(err, &de) || de.Kind != DecodeDuplicateKey {
				t.Fatalf("DecodeTolerant(%s): err = %v, want DecodeDuplicateKey", docJSON, err)
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
