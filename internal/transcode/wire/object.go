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
	return decode(data, dst, true)
}

// DecodeTolerant decodes exactly one JSON value into dst with the same
// malformed-wire rejections as Decode (duplicate keys, illegal nulls,
// trailing values, malformed syntax, missing-required, contradictory-union)
// but WITHOUT the unknown-field rejection: an unknown field is skipped,
// never a failure. Field names match their JSON tags exactly, so a case
// variant is also skipped rather than bound to a modeled field. It is the
// decode posture for the UPSTREAM provider
// response envelope — a "subject to change" contract the transcoder does not
// control — and for a NATIVE passthrough request, where nothing is converted
// so field coverage belongs to the upstream and only the always-reject
// structural set protects this proxy's own parsing. It is never used for a
// CLIENT request that will be converted (whose unknown fields must be
// rejected to preserve the lossless-transcoding invariant), and the
// content-block and tool unions keep their per-arm strictness where the
// decoder into a typed destination still runs them.
func DecodeTolerant(data []byte, dst any) error {
	return decode(data, dst, false)
}

// decode is the shared strict/tolerant decoder. When disallowUnknown is
// true, encoding/json rejects unknown fields; when false, an unknown field
// is skipped. The typed walk removes only names that encoding/json would
// otherwise bind through case folding; other unknown names are skipped by
// the decoder itself.
func decode(data []byte, dst any, disallowUnknown bool) error {
	if err := checkDuplicateKeys(data); err != nil {
		return &DecodeError{Kind: DecodeDuplicateKey, Message: err.Error()}
	}
	var err error
	data, err = prepareDecodeFields(data, dst, disallowUnknown)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		dec.DisallowUnknownFields()
	}
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
// A non-object value, empty input, trailing values, or malformed JSON is an error.
// The caller decides the classification (client input vs model-generated output).
//
// Unlike Decode (which validates wire-envelope structs and strictly rejects duplicate
// keys in protocol request/response documents), JSONObject validates payload-level
// JSON objects (such as function call arguments and schemas) where duplicate keys
// are resolved by standard Go JSON unmarshaling (last key wins) per RFC 8259.
func JSONObject(raw string) (map[string]json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("empty JSON object")
	}
	if !strings.HasPrefix(trimmed, "{") {
		return nil, errors.New("value is not a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	var value map[string]json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("value is not a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, &DecodeError{
				Kind:    DecodeTrailingValue,
				Message: "unexpected trailing JSON value",
			}
		}
		return nil, &DecodeError{
			Kind:    DecodeMalformed,
			Message: err.Error(),
		}
	}
	return value, nil
}

// checkDuplicateKeys walks the JSON document and rejects any object that
// contains a duplicated key, at any depth. encoding/json accepts duplicates
// silently (last value wins); the wire layer rejects the document. JSON keys
// are case-sensitive, including schema property names, function arguments and
// provider extensions. Case aliases of modeled struct fields are checked
// separately by the typed walk, where the destination contract is known.
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	// This pass checks structure, not numeric ranges. A valid raw JSON
	// number outside float64 must not end the walk before later keys.
	dec.UseNumber()
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
var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// prepareDecodeFields checks modeled field names and nullability against the
// destination type. Strict decodes reject noncanonical field names. Tolerant
// decodes discard them as unknown fields before encoding/json can bind them
// through its implicit case-insensitive matching. Every retained field and
// subtree keeps its original bytes; only discarded object members are removed.
// The walk rejects null for non-nullable fields: fields whose type cannot hold null
// (plain values) silently keep their zero value under encoding/json, which
// would fabricate a legal empty value out of an explicit null. Nullable
// fields — pointers, slices, maps, interfaces, json.RawMessage, Field[T],
// and any custom Unmarshaler — record or reject the null themselves.
func prepareDecodeFields(data []byte, dst any, disallowUnknown bool) ([]byte, error) {
	typ := reflect.TypeOf(dst)
	if typ == nil {
		return data, nil // encoding/json reports an invalid destination.
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Raw schemas and skipped provider fields may contain numbers outside
	// float64. Preserve their lexemes while checking the surrounding shape.
	dec.UseNumber()
	walk := decodeFieldWalk{dec: dec, data: data, disallowUnknown: disallowUnknown}
	if err := walk.value(typ, ""); err != nil {
		return nil, err
	}
	if len(walk.omitted) == 0 {
		return data, nil
	}
	// Ranges are recorded in source order and never overlap: an omitted
	// member is skipped as a whole, so its children cannot add more ranges.
	// Splicing once avoids remarshal changes to raw schemas and artifacts.
	out := make([]byte, 0, len(data))
	previous := 0
	for _, span := range walk.omitted {
		out = append(out, data[previous:span[0]]...)
		previous = span[1]
	}
	out = append(out, data[previous:]...)
	return out, nil
}

type decodeFieldWalk struct {
	dec             *json.Decoder
	data            []byte
	disallowUnknown bool
	omitted         [][2]int
}

// value validates the next JSON value in dec against typ. The
// first token is read here so the null check sees the ORIGINAL field type
// (a null into a pointer field is legal — it sets nil — while a null into
// a plain value field silently keeps the zero value and is rejected).
// Syntax errors are deferred to the decode pass, which reports them as
// DecodeMalformed; this walk reports field-name and illegal-null findings.
func (w *decodeFieldWalk) value(typ reflect.Type, path string) error {
	tok, err := w.dec.Token()
	if err != nil {
		// Malformed input; the decode pass reports it.
		return nil
	}
	return w.token(typ, tok, path)
}

// token processes a token already read as the start of a value.
func (w *decodeFieldWalk) token(typ reflect.Type, tok json.Token, path string) error {
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
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	// Interfaces hold any value; consume the rest of the value unchecked.
	if typ.Kind() == reflect.Interface {
		if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
			return skipJSONRest(w.dec)
		}
		return nil
	}

	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			// A custom unmarshaler (e.g. Field[T], json.RawMessage, or a
			// union type) owns its own subtree, including any nulls inside
			// it.
			if isCustomUnmarshaler(typ) {
				return skipJSONRest(w.dec)
			}
			if typ.Kind() == reflect.Map {
				return w.mapObject(typ.Elem(), path)
			}
			if typ.Kind() != reflect.Struct {
				return skipJSONRest(w.dec)
			}
			return w.object(typ, path)
		case '[':
			// A custom unmarshaler owns its own subtree; plain slices and
			// arrays are walked element-wise with the element type.
			if (typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array) ||
				isCustomUnmarshaler(typ) {
				return skipJSONRest(w.dec)
			}
			elem := typ.Elem()
			for {
				tok, err := walkToken(w.dec)
				if err != nil {
					// Malformed input; the decode pass reports it.
					return nil
				}
				if d, ok := tok.(json.Delim); ok && d == ']' {
					return nil
				}
				if err := w.token(elem, tok, path+"[]"); err != nil {
					return err
				}
			}
		}
	}
	// Scalars: no null to check.
	return nil
}

// mapObject keeps map keys as case-sensitive data. Its values still have
// their declared types: a map of structs contains modeled fields, whereas
// RawMessage and interface values remain opaque through token's own rules.
func (w *decodeFieldWalk) mapObject(elem reflect.Type, path string) error {
	for {
		tok, err := walkToken(w.dec)
		if err != nil {
			return nil // The decode pass reports malformed syntax.
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return &DecodeError{Kind: DecodeMalformed, Path: path, Message: "object key is not a string"}
		}
		if err := w.value(elem, path+"."+key); err != nil {
			return err
		}
	}
}

// walkToken reads the next token for the null walk. A JSON null literal is
// returned as a nil token with a nil error (json.Decoder.Token semantics);
// EOF is reported through the error so the two are never conflated.
// Truncated documents are reported by the decode pass, not by this
// supplementary check.
func walkToken(dec *json.Decoder) (json.Token, error) {
	return dec.Token()
}

// object validates an object whose '{' has already been consumed.
func (w *decodeFieldWalk) object(typ reflect.Type, path string) error {
	seen := make(map[string]struct{})
	start := int(w.dec.InputOffset())
	kept, omittedLeading := false, false
	for {
		memberStart := int(w.dec.InputOffset())
		tok, err := walkToken(w.dec)
		if err != nil {
			// Malformed input; the decode pass reports it.
			return nil
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			if omittedLeading && !kept {
				w.omitted = append(w.omitted, [2]int{start, int(w.dec.InputOffset()) - 1})
			}
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
		field, _, found := structFieldByJSONName(typ, key, false)
		if !found {
			// Only modeled fields have Go's case-insensitive aliasing. Raw
			// objects, map keys and unknown provider extensions keep their
			// case-sensitive JSON identity. EqualFold matches the Unicode
			// simple-fold equivalence encoding/json uses, including long s.
			if _, name, alias := structFieldByJSONName(typ, key, true); alias {
				if w.disallowUnknown {
					kind := DecodeUnknownField
					message := fmt.Sprintf("JSON field %q must use the modeled spelling %q", key, name)
					if _, duplicate := seen[name]; duplicate {
						kind = DecodeDuplicateKey
						message = fmt.Sprintf("duplicate modeled JSON field %q through alias %q", name, key)
					}
					return &DecodeError{Kind: kind, Path: path + "." + key, Message: message}
				}
				if err := skipJSONValue(w.dec); err != nil {
					return nil // The decode pass reports malformed syntax.
				}
				if kept {
					// memberStart includes the comma after the preceding
					// retained or omitted value. The next member's comma stays.
					w.omitted = append(w.omitted, [2]int{memberStart, int(w.dec.InputOffset())})
				} else {
					// Leading omissions must also consume the separator before
					// the first retained member; defer the range until then.
					omittedLeading = true
				}
				continue
			}
		}
		if omittedLeading && !kept {
			// Token has validated a key here. Between the previous value and
			// this key there can be only whitespace and one separator, so the
			// first quote is the retained member's exact source start.
			keyStart := memberStart + bytes.IndexByte(w.data[memberStart:int(w.dec.InputOffset())], '"')
			w.omitted = append(w.omitted, [2]int{start, keyStart})
		}
		kept = true
		if !found {
			// Unknown field: DisallowUnknownFields rejects it in the
			// decode pass; skip the value here.
			if err := skipJSONValue(w.dec); err != nil {
				// Malformed input; the decode pass reports it.
				return nil
			}
			continue
		}
		seen[key] = struct{}{}
		if err := w.value(field.Type, path+"."+key); err != nil {
			return err
		}
	}
}

// structFieldByJSONName resolves a JSON object key to a struct field type,
// honoring json tags, "-", and embedded (anonymous) structs. Folded lookup is
// used only after exact lookup fails, so two explicitly modeled names that
// differ by case remain distinct fields.
func structFieldByJSONName(typ reflect.Type, key string, folded bool) (reflect.StructField, string, bool) {
	for field := range typ.Fields() {
		name := field.Tag.Get("json")
		if name == "-" {
			continue
		}
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}
		if field.Anonymous && name == "" {
			ft := field.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !isCustomUnmarshaler(ft) {
				if inner, innerName, ok := structFieldByJSONName(ft, key, folded); ok {
					return inner, innerName, true
				}
			}
			continue
		}
		if field.PkgPath != "" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if name == key || (folded && strings.EqualFold(name, key)) {
			return field, name, true
		}
	}
	return reflect.StructField{}, "", false
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
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
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
