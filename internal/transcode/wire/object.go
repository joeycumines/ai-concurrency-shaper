// Package wire implements the pinned wire-contract layer of the transcoder:
// the shared presence type Field[T], the strict object decoder, and the
// typed decode errors that every pinned contract type reports.
//
// The package is deliberately self-contained: it imports nothing from the
// transcode package, so the pinned contract types can never depend on
// transcode internals (no import cycle). The transcode layer maps the typed
// errors here into its classification taxonomy at its boundaries.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Field[T] is a presence-aware JSON field: it records whether the field was
// present in the JSON document, whether it was an explicit null, and its
// value. The object contract decides whether absence or null is legal for
// each field; Field[T] makes the distinction observable.
type Field[T any] struct {
	Value   T
	Present bool
	Null    bool
}

// UnmarshalJSON marks the field present, records an explicit null, and
// otherwise decodes the value.
func (f *Field[T]) UnmarshalJSON(data []byte) error {
	f.Present = true
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		f.Null = true
		return nil
	}
	return json.Unmarshal(data, &f.Value)
}

// MarshalJSON emits the field as its bare value. The JSON document never
// carries the presence bookkeeping; the object contract decides whether the
// key is required on the wire (a required Field is therefore always emitted,
// with the value the contract supplies).
func (f Field[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.Value)
}

// IsZero reports whether the field is absent, so json:"...,omitempty"
// omits an absent field entirely (structs are otherwise never omitted by
// omitempty). A present field — including an explicit false — is emitted.
func (f Field[T]) IsZero() bool {
	return !f.Present
}

// DecodeErrorKind is the category of a rejected decode.
type DecodeErrorKind uint8

// DecodeErrorKind values — the six malformed-JSON categories every pinned
// contract rejects, plus malformed syntax.
const (
	DecodeMalformed DecodeErrorKind = iota
	DecodeDuplicateKey
	DecodeUnknownField
	DecodeMissingRequired
	DecodeIllegalNull
	DecodeContradictoryUnion
	DecodeTrailingValue
)

// String returns the stable category name.
func (k DecodeErrorKind) String() string {
	switch k {
	case DecodeMalformed:
		return "malformed"
	case DecodeDuplicateKey:
		return "duplicate_key"
	case DecodeUnknownField:
		return "unknown_field"
	case DecodeMissingRequired:
		return "missing_required"
	case DecodeIllegalNull:
		return "illegal_null"
	case DecodeContradictoryUnion:
		return "contradictory_union"
	case DecodeTrailingValue:
		return "trailing_value"
	default:
		return "unknown"
	}
}

// DecodeError is the typed decode error of the wire layer. Every rejection
// of the six malformed-JSON categories is a *DecodeError, so the transcode
// layer can classify without string matching.
type DecodeError struct {
	Kind    DecodeErrorKind
	Path    string
	Message string
}

// Error implements error.
func (e *DecodeError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Kind.String()
	}
	if e.Path == "" {
		return fmt.Sprintf("wire: %s: %s", e.Kind, msg)
	}
	return fmt.Sprintf("wire: %s: %s at %s", e.Kind, msg, e.Path)
}

// UnsupportedTypeError reports a valid union arm the transcoder does not
// support — the wire equivalent of the transcode layer's
// UnsupportedFeatureError. It is never a malformed-wire rejection: the wire
// is valid, the feature is outside the supported subset.
type UnsupportedTypeError struct {
	Protocol string
	Path     string
	Type     string
}

// Error implements error.
func (e *UnsupportedTypeError) Error() string {
	return fmt.Sprintf(
		"unsupported %s type %q at %s",
		e.Protocol,
		e.Type,
		e.Path,
	)
}

// Decode strictly decodes exactly one JSON value into dst, rejecting:
//
//   - duplicate JSON keys at any depth (encoding/json silently keeps the
//     last value; the wire layer rejects the document instead);
//   - unknown fields (DisallowUnknownFields);
//   - null for non-nullable fields (a plain value type silently keeps its
//     zero value under encoding/json; the wire layer rejects the null);
//   - trailing JSON values;
//   - malformed syntax.
//
// Missing-required and contradictory-union rejections are contract
// decisions made by the per-type Validate methods (Field[T] records the
// presence the contract needs); both report *DecodeError.
func Decode(data []byte, dst any) error {
	if err := checkDuplicateKeys(data); err != nil {
		return &DecodeError{Kind: DecodeDuplicateKey, Message: err.Error()}
	}
	if err := checkIllegalNulls(data, dst); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// A typed rejection from a nested decode or a custom unmarshaler
		// passes through untouched: it is more specific than "malformed".
		// This keeps the classification chain intact — a
		// wire.UnsupportedTypeError reported by a union dispatcher must
		// survive the enclosing decode so the transcode boundary can
		// translate it into its own unsupported-feature error.
		var decodeErr *DecodeError
		var unsupported *UnsupportedTypeError
		if errors.As(err, &decodeErr) || errors.As(err, &unsupported) {
			return err
		}
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return &DecodeError{
				Kind:    DecodeUnknownField,
				Message: err.Error(),
			}
		}
		return &DecodeError{
			Kind:    DecodeMalformed,
			Message: err.Error(),
		}
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return &DecodeError{
				Kind:    DecodeTrailingValue,
				Message: "unexpected trailing JSON value",
			}
		}
		return &DecodeError{
			Kind:    DecodeMalformed,
			Message: err.Error(),
		}
	}
	return nil
}

// TrimSpace returns data with surrounding whitespace removed.
func TrimSpace(data []byte) []byte {
	return bytes.TrimSpace(data)
}

// JSONObject decodes raw as exactly one JSON object, returning its keys.
// A non-object value or malformed JSON is an error. The caller decides the
// classification (client input vs model-generated output).
func JSONObject(raw string) (map[string]json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("empty JSON object")
	}
	var value map[string]json.RawMessage
	if err := Decode([]byte(raw), &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("value is not a JSON object")
	}
	return value, nil
}

// checkDuplicateKeys walks the JSON document and rejects any object that
// contains a duplicated key, at any depth. encoding/json accepts duplicates
// silently (last value wins); the wire layer rejects the document.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	// stack of seen-key sets, one per open object. Each frame records
	// whether the next string token is a key (after '{' or after a value in
	// an object) or a value (after a key). Array frames are inert: strings
	// inside arrays are values.
	type frame struct {
		isObject  bool
		keys      map[string]struct{}
		expectKey bool
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Malformed syntax; the decode pass reports it.
			return nil
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{
					isObject:  true,
					keys:      map[string]struct{}{},
					expectKey: true,
				})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				// A value just completed; the next string in the enclosing
				// object is a key.
				if len(stack) > 0 && stack[len(stack)-1].isObject {
					stack[len(stack)-1].expectKey = true
				}
			}
		case string:
			if len(stack) == 0 {
				continue
			}
			top := &stack[len(stack)-1]
			if !top.isObject || !top.expectKey {
				// A string value (or an array element): the next string in
				// the enclosing object is a key.
				if top.isObject {
					top.expectKey = true
				}
				continue
			}
			if _, dup := top.keys[t]; dup {
				return fmt.Errorf("duplicate JSON key %q", t)
			}
			top.keys[t] = struct{}{}
			top.expectKey = false
		default:
			// Numbers, booleans, and null: a completed value.
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				stack[len(stack)-1].expectKey = true
			}
		}
	}
	return nil
}

// unmarshalerType is the json.Unmarshaler interface type.
var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// checkIllegalNulls walks the JSON document against the destination type and
// rejects null for non-nullable fields: fields whose type cannot hold null
// (plain values) silently keep their zero value under encoding/json, which
// would fabricate a legal empty value out of an explicit null. Nullable
// fields — pointers, slices, maps, interfaces, json.RawMessage, Field[T],
// and any custom Unmarshaler — record or reject the null themselves.
func checkIllegalNulls(data []byte, dst any) error {
	typ := reflect.TypeOf(dst)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	return checkNullValue(dec, typ, "")
}

// checkNullValue validates the next JSON value in dec against typ. The
// first token is read here so the null check sees the ORIGINAL field type
// (a null into a pointer field is legal — it sets nil — while a null into
// a plain value field silently keeps the zero value and is rejected).
// Syntax errors are deferred to the decode pass, which reports them as
// DecodeMalformed; this walk reports only illegal-null findings.
func checkNullValue(dec *json.Decoder, typ reflect.Type, path string) error {
	tok, err := dec.Token()
	if err != nil {
		// Malformed input; the decode pass reports it.
		return nil
	}
	return checkNullToken(dec, typ, tok, path)
}

// checkNullToken processes a token already read as the start of a value.
func checkNullToken(dec *json.Decoder, typ reflect.Type, tok json.Token, path string) error {
	if tok == nil {
		// Explicit null. The contract decides legality; the decoder
		// rejects null only where the type cannot hold it. The check runs
		// on the ORIGINAL type: pointers, slices, maps, interfaces,
		// RawMessage, and custom unmarshalers are null-capable.
		if !nullCapable(typ) {
			return &DecodeError{
				Kind:    DecodeIllegalNull,
				Path:    path,
				Message: fmt.Sprintf("null is not allowed for %s", typ),
			}
		}
		return nil
	}

	// Unwrap pointers for descent: the value is not null, so a pointer
	// field holds an allocated value of its element type.
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	// Interfaces hold any value; consume the rest of the value unchecked.
	if typ.Kind() == reflect.Interface {
		if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
			return skipJSONRest(dec)
		}
		return nil
	}

	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			// A custom unmarshaler (e.g. Field[T], json.RawMessage, or a
			// union type) owns its own subtree, including any nulls inside
			// it.
			if typ.Kind() != reflect.Struct || isCustomUnmarshaler(typ) {
				return skipJSONRest(dec)
			}
			return checkNullObject(dec, typ, path)
		case '[':
			// A custom unmarshaler owns its own subtree; plain slices and
			// arrays are walked element-wise with the element type.
			if (typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array) ||
				isCustomUnmarshaler(typ) {
				return skipJSONRest(dec)
			}
			elem := typ.Elem()
			for {
				tok, err := walkToken(dec)
				if err != nil {
					// Malformed input; the decode pass reports it.
					return nil
				}
				if d, ok := tok.(json.Delim); ok && d == ']' {
					return nil
				}
				if err := checkNullToken(dec, elem, tok, path+"[]"); err != nil {
					return err
				}
			}
		}
	}
	// Scalars: no null to check.
	return nil
}

// walkToken reads the next token for the null walk. A JSON null literal is
// returned as a nil token with a nil error (json.Decoder.Token semantics);
// EOF is reported through the error so the two are never conflated.
// Truncated documents are reported by the decode pass, not by this
// supplementary check.
func walkToken(dec *json.Decoder) (json.Token, error) {
	return dec.Token()
}

// checkNullObject validates an object whose '{' has already been consumed.
func checkNullObject(dec *json.Decoder, typ reflect.Type, path string) error {
	for {
		tok, err := walkToken(dec)
		if err != nil {
			// Malformed input; the decode pass reports it.
			return nil
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return &DecodeError{
				Kind:    DecodeMalformed,
				Path:    path,
				Message: "object key is not a string",
			}
		}
		field, found := structFieldByJSONName(typ, key)
		if !found {
			// Unknown field: DisallowUnknownFields rejects it in the
			// decode pass; skip the value here.
			if err := skipJSONValue(dec); err != nil {
				// Malformed input; the decode pass reports it.
				return nil
			}
			continue
		}
		if err := checkNullValue(dec, field.Type, path+"."+key); err != nil {
			return err
		}
	}
}

// structFieldByJSONName resolves a JSON object key to a struct field type,
// honoring json tags, "-", and embedded (anonymous) structs.
//
// NOTE: anonymous fields with their OWN json tag (e.g. `Inner json:"inner"`)
// are treated by encoding/json as named fields, but this walk would recurse
// into them and treat "inner" as unknown (skipping the value, so an illegal
// null inside could escape the null walk). No wire type uses a tagged
// anonymous field today; if one is ever introduced, this walk must be
// extended to honor the tag.
func structFieldByJSONName(typ reflect.Type, key string) (reflect.StructField, bool) {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous {
			ft := field.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !isCustomUnmarshaler(ft) {
				if inner, ok := structFieldByJSONName(ft, key); ok {
					return inner, true
				}
			}
			continue
		}
		name := field.Tag.Get("json")
		if name == "-" {
			continue
		}
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}
		if name == "" {
			name = field.Name
		}
		if name == key {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// isCustomUnmarshaler reports whether the type (or a pointer to it)
// implements json.Unmarshaler, meaning it owns its own null handling.
func isCustomUnmarshaler(typ reflect.Type) bool {
	return typ.Implements(unmarshalerType) ||
		reflect.PointerTo(typ).Implements(unmarshalerType)
}

// nullCapable reports whether a null value is representable by the type
// without fabricating a value: pointers, slices, maps, interfaces,
// json.RawMessage, custom unmarshalers, and Field[T] (which records the
// null). Everything else — plain scalars, structs, arrays — silently keeps
// its zero value under encoding/json, so a null there is an illegal null.
func nullCapable(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	return isCustomUnmarshaler(typ)
}

// skipJSONValue consumes one complete JSON value in dec, reading the first
// token itself (the caller has not consumed anything).
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		return skipJSONRest(dec)
	}
	return nil
}

// skipJSONRest consumes the remainder of a JSON container whose opening
// delimiter ('{' or '[') has already been consumed.
func skipJSONRest(dec *json.Decoder) error {
	depth := 1
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return nil
				}
			}
		}
	}
}
