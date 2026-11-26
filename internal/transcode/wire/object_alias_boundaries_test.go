package wire

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeCaseDistinctDataKeys(t *testing.T) {
	for name, decode := range map[string]func([]byte, any) error{"strict": Decode, "tolerant": DecodeTolerant} {
		t.Run(name, func(t *testing.T) {
			type document struct {
				Schema   json.RawMessage            `json:"schema"`
				Input    map[string]json.RawMessage `json:"input"`
				Metadata any                        `json:"metadata"`
			}
			const body = `{"schema":{"type":"object","properties":{"ID":{"type":"string"},"id":{"type":"number"}}},"input":{"PATH":"/usr/bin","Path":"/tmp"},"metadata":{"customer":1,"Customer":2}}`
			var got document
			if err := decode([]byte(body), &got); err != nil {
				t.Fatalf("case-sensitive data keys are distinct: %v", err)
			}
			if string(got.Input["PATH"]) != `"/usr/bin"` || string(got.Input["Path"]) != `"/tmp"` {
				t.Fatalf("case-sensitive input keys changed: %v", got.Input)
			}
			var native map[string]json.RawMessage
			if err := decode([]byte(body), &native); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecodeTolerantCaseDistinctExtensionKeys(t *testing.T) {
	var got struct {
		Model string `json:"model"`
	}
	if err := DecodeTolerant([]byte(`{"model":"m","cost":1,"Cost":2,"extra":{"k":1,"K":2}}`), &got); err != nil {
		t.Fatalf("provider extensions have case-sensitive JSON names: %v", err)
	}
	if got.Model != "m" {
		t.Fatalf("model = %q", got.Model)
	}
}

func TestDecodeUnicodeAliasesCannotOverwriteModeledFields(t *testing.T) {
	for name, decode := range map[string]func([]byte, any) error{"strict": Decode, "tolerant": DecodeTolerant} {
		for _, raw := range []string{
			`{"stop_reason":"refusal","ſtop_reason":"end_turn"}`,
			`{"stop_reason":"refusal","ſtop_reaſon":"end_turn"}`,
			`{"ſtop_reason":"end_turn","stop_reason":"refusal"}`,
		} {
			t.Run(name+"/"+raw, func(t *testing.T) {
				var got struct {
					StopReason string `json:"stop_reason"`
				}
				err := decode([]byte(raw), &got)
				if name == "strict" {
					if err == nil {
						t.Fatal("strict decode accepted an unknown alias")
					}
				} else if err != nil || got.StopReason != "refusal" {
					t.Fatalf("tolerant decode must retain the exact stop_reason: got %q, error %v", got.StopReason, err)
				}
			})
		}
	}
}

func TestDecodeModeledFieldNamesAreExact(t *testing.T) {
	for name, decode := range map[string]func([]byte, any) error{"strict": Decode, "tolerant": DecodeTolerant} {
		for _, raw := range []string{`{"NAME":null}`, `{"Name":"alias"}`, `{"ſignature":"alias"}`} {
			t.Run(name+"/"+raw, func(t *testing.T) {
				var got struct {
					Name      string `json:"name"`
					Signature string `json:"signature"`
				}
				err := decode([]byte(raw), &got)
				if name == "strict" {
					var typed *DecodeError
					if !errors.As(err, &typed) || typed.Kind != DecodeUnknownField {
						t.Fatalf("noncanonical modeled name: %v", err)
					}
				} else if err != nil || got.Name != "" || got.Signature != "" {
					t.Fatalf("unknown alias must be skipped: got %+v, error %v", got, err)
				}
			})
		}
	}
}

func TestDecodeExactFieldsMayDifferByCase(t *testing.T) {
	var got struct {
		Lower string `json:"key"`
		Upper string `json:"KEY"`
	}
	if err := Decode([]byte(`{"key":"lower","KEY":"upper"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Lower != "lower" || got.Upper != "upper" {
		t.Fatalf("exact names changed: %+v", got)
	}
}
