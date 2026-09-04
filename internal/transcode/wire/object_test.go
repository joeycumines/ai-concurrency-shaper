package wire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Field[T]
// ---------------------------------------------------------------------------

func TestFieldUnmarshal(t *testing.T) {
	var f Field[string]
	if err := json.Unmarshal([]byte(`"hello"`), &f); err != nil {
		t.Fatal(err)
	}
	if !f.Present || f.Null || f.Value != "hello" {
		t.Fatalf("value decode = %+v", f)
	}

	var n Field[string]
	if err := json.Unmarshal([]byte(`null`), &n); err != nil {
		t.Fatal(err)
	}
	if !n.Present || !n.Null || n.Value != "" {
		t.Fatalf("null decode = %+v", n)
	}

	// A zero value and an empty value are present but not null.
	var z Field[int]
	if err := json.Unmarshal([]byte(`0`), &z); err != nil {
		t.Fatal(err)
	}
	if !z.Present || z.Null || z.Value != 0 {
		t.Fatalf("zero decode = %+v", z)
	}

	var e Field[string]
	if err := json.Unmarshal([]byte(`""`), &e); err != nil {
		t.Fatal(err)
	}
	if !e.Present || e.Null || e.Value != "" {
		t.Fatalf("empty decode = %+v", e)
	}

	// Absent: never unmarshaled.
	var a Field[string]
	if a.Present || a.Null {
		t.Fatalf("absent field = %+v", a)
	}
}

// ---------------------------------------------------------------------------
// Duplicate keys
// ---------------------------------------------------------------------------

func TestDecodeRejectsDuplicateKeys(t *testing.T) {
	type nested struct {
		B int `json:"b"`
	}
	type doc struct {
		A int    `json:"a"`
		N nested `json:"n"`
		L []int  `json:"l"`
	}
	cases := map[string]string{
		"top level": `{"a":1,"a":2}`,
		"nested":    `{"n":{"b":1,"b":2}}`,
		"in array":  `{"l":[{"x":1,"x":2}]}`,
		"deep":      `{"n":{"b":1},"n":{"b":2}}`,
	}
	for name, docJSON := range cases {
		t.Run(name, func(t *testing.T) {
			var out doc
			err := Decode([]byte(docJSON), &out)
			var de *DecodeError
			if !errors.As(err, &de) || de.Kind != DecodeDuplicateKey {
				t.Fatalf("err = %v, want DecodeDuplicateKey", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Unknown fields, trailing values, malformed syntax
// ---------------------------------------------------------------------------

func TestDecodeRejectsUnknownFields(t *testing.T) {
	type doc struct {
		A int `json:"a"`
	}
	var out doc
	err := Decode([]byte(`{"a":1,"b":2}`), &out)
	var de *DecodeError
	if !errors.As(err, &de) || de.Kind != DecodeUnknownField {
		t.Fatalf("err = %v, want DecodeUnknownField", err)
	}
}

func TestDecodeRejectsTrailingValues(t *testing.T) {
	type doc struct {
		A int `json:"a"`
	}
	var out doc
	err := Decode([]byte(`{"a":1} {"b":2}`), &out)
	var de *DecodeError
	if !errors.As(err, &de) || de.Kind != DecodeTrailingValue {
		t.Fatalf("err = %v, want DecodeTrailingValue", err)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	type doc struct {
		A int `json:"a"`
	}
	for _, bad := range []string{``, `{`, `{"a":}`, `{"a":1`, `not json`} {
		var out doc
		err := Decode([]byte(bad), &out)
		var de *DecodeError
		if !errors.As(err, &de) || de.Kind != DecodeMalformed {
			t.Fatalf("decode %q: err = %v, want DecodeMalformed", bad, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Illegal nulls
// ---------------------------------------------------------------------------

type nullTestOuter struct {
	ID        string            `json:"id"`
	Ptr       *string           `json:"ptr"`
	Slice     []string          `json:"slice"`
	Map       map[string]string `json:"map"`
	Raw       json.RawMessage   `json:"raw"`
	Field     Field[string]     `json:"field"`
	Any       any               `json:"any"`
	Embedded  nullTestInner     `json:"embedded"`
	PtrStruct *nullTestInner    `json:"ptr_struct"`
	Arr       [2]string         `json:"arr"`
}

type nullTestInner struct {
	Text string `json:"text"`
}

func TestDecodeRejectsIllegalNulls(t *testing.T) {
	cases := map[string]struct {
		wire string
		path string
	}{
		"plain string":  {`{"id":null}`, ".id"},
		"nested struct": {`{"embedded":{"text":null}}`, ".embedded.text"},
		"array element": {`{"arr":[null,null]}`, ".arr[]"},
		"struct value":  {`{"embedded":null}`, ".embedded"},
		"slice element": {`{"slice":[null]}`, ".slice[]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out nullTestOuter
			err := Decode([]byte(c.wire), &out)
			var de *DecodeError
			if !errors.As(err, &de) || de.Kind != DecodeIllegalNull {
				t.Fatalf("err = %v, want DecodeIllegalNull", err)
			}
			if c.path != "" && de.Path != c.path {
				t.Fatalf("path = %q, want %q", de.Path, c.path)
			}
		})
	}
}

func TestDecodeAcceptsLegalNulls(t *testing.T) {
	wire := `{"id":"x","ptr":null,"slice":null,"map":null,"raw":null,"field":null,"any":null,"embedded":{"text":"t"},"ptr_struct":null,"arr":["a","b"]}`
	var out nullTestOuter
	if err := Decode([]byte(wire), &out); err != nil {
		t.Fatalf("legal nulls rejected: %v", err)
	}
	if out.ID != "x" || out.Ptr != nil || out.Slice != nil || out.Map != nil ||
		string(out.Raw) != "null" || !out.Field.Present || !out.Field.Null ||
		out.Any != nil || out.PtrStruct != nil || out.Arr != [2]string{"a", "b"} {
		t.Fatalf("decode = %+v", out)
	}
}

// TestDecodeNullPathIntoNestedPointer verifies a null inside a pointer
// target is still checked: the walk descends allocated pointer values.
func TestDecodeNullPathIntoNestedPointer(t *testing.T) {
	type doc struct {
		Inner *nullTestInner `json:"inner"`
	}
	var out doc
	err := Decode([]byte(`{"inner":{"text":null}}`), &out)
	var de *DecodeError
	if !errors.As(err, &de) || de.Kind != DecodeIllegalNull {
		t.Fatalf("err = %v, want DecodeIllegalNull", err)
	}
}

// customUnmarshalerUnion is a union type that owns its own decoding.
type customUnmarshalerUnion struct {
	Raw json.RawMessage `json:"-"`
}

func (u *customUnmarshalerUnion) UnmarshalJSON(data []byte) error {
	u.Raw = append(u.Raw[:0], data...)
	return nil
}

// TestDecodeNestedCustomUnmarshalerSkipsNulls verifies a custom unmarshaler
// (a union type) owns its own subtree: nulls inside it are the contract's
// decision, never the decoder's.
func TestDecodeNestedCustomUnmarshalerSkipsNulls(t *testing.T) {
	type doc struct {
		U customUnmarshalerUnion `json:"u"`
	}
	var out doc
	if err := Decode([]byte(`{"u":{"x":null}}`), &out); err != nil {
		t.Fatalf("custom unmarshaler subtree rejected: %v", err)
	}
	if !strings.Contains(string(out.U.Raw), "null") {
		t.Fatalf("raw = %s", out.U.Raw)
	}
}

// ---------------------------------------------------------------------------
// Success paths
// ---------------------------------------------------------------------------

func TestDecodeSuccess(t *testing.T) {
	type doc struct {
		ID    string   `json:"id"`
		Count int      `json:"count"`
		Score float64  `json:"score"`
		Tags  []string `json:"tags"`
	}
	wire := `{"id":"x","count":0,"score":1.5,"tags":[]}`
	var out doc
	if err := Decode([]byte(wire), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "x" || out.Count != 0 || out.Score != 1.5 || out.Tags == nil {
		t.Fatalf("decode = %+v", out)
	}
}

// ---------------------------------------------------------------------------
// JSONObject
// ---------------------------------------------------------------------------

func TestJSONObject(t *testing.T) {
	obj, err := JSONObject(`{"a":1,"b":{"c":[1,2]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(obj) != 2 || string(obj["b"]) != `{"c":[1,2]}` {
		t.Fatalf("obj = %v", obj)
	}
	for _, bad := range []string{``, `null`, `[]`, `"x"`, `1`, `{`, `{"a":1} x`} {
		if _, err := JSONObject(bad); err == nil {
			t.Fatalf("JSONObject(%q) accepted", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Error shapes
// ---------------------------------------------------------------------------

func TestDecodeErrorString(t *testing.T) {
	e := &DecodeError{Kind: DecodeIllegalNull, Path: ".a", Message: "null is not allowed for string"}
	if !strings.Contains(e.Error(), "illegal_null") || !strings.Contains(e.Error(), ".a") {
		t.Fatalf("Error() = %q", e.Error())
	}
	if DecodeMalformed.String() != "malformed" || DecodeUnknownField.String() != "unknown_field" {
		t.Fatal("kind strings drifted")
	}
}

func TestUnsupportedTypeError(t *testing.T) {
	e := &UnsupportedTypeError{Protocol: "responses", Path: "stream[].type", Type: "bogus"}
	if !strings.Contains(e.Error(), "bogus") || !strings.Contains(e.Error(), "responses") {
		t.Fatalf("Error() = %q", e.Error())
	}
}
