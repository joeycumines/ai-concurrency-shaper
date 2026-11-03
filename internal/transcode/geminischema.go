package transcode

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// geminiSchemaTypes maps the Gemini Schema.type enum to JSON Schema types.
// TYPE_UNSPECIFIED is handled separately (it is omitted).
var geminiSchemaTypes = map[string]string{
	"STRING":  "string",
	"NUMBER":  "number",
	"INTEGER": "integer",
	"BOOLEAN": "boolean",
	"ARRAY":   "array",
	"OBJECT":  "object",
	"NULL":    "null",
}

// geminiSchemaVerbatim are the Schema members whose value carries over
// unchanged under the same name.
var geminiSchemaVerbatim = map[string]struct{}{
	"format": {}, "title": {}, "description": {}, "pattern": {}, "default": {},
	"enum": {}, "required": {}, "minimum": {}, "maximum": {},
}

// geminiSchemaIntegers are the Schema members that carry a proto int64, which
// the JSON form may spell as a number or as a decimal string.
var geminiSchemaIntegers = map[string]struct{}{
	"minItems": {}, "maxItems": {}, "minLength": {}, "maxLength": {},
	"minProperties": {}, "maxProperties": {},
}

// ConvertGeminiSchema converts a Gemini OpenAPI-style Schema into a JSON
// Schema object, following the field table of the Gemini conversion
// contract. Recursion covers items, properties, and anyOf. The output is a
// new object; the input is never forwarded, and an unknown member is an
// error rather than a silent drop.
func ConvertGeminiSchema(raw json.RawMessage) (json.RawMessage, error) {
	return convertGeminiSchemaAt(raw, "schema")
}

func convertGeminiSchemaAt(raw json.RawMessage, path string) (json.RawMessage, error) {
	members, err := wire.JSONObject(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", path, err)
	}
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make(map[string]json.RawMessage, len(members)+1)
	var typeName string
	nullable := false
	for _, key := range keys {
		value := members[key]
		memberPath := path + "." + key
		switch key {
		case "type":
			var name string
			if err := json.Unmarshal(value, &name); err != nil {
				return nil, fmt.Errorf("%s must be a string", memberPath)
			}
			if name == "TYPE_UNSPECIFIED" {
				continue
			}
			mapped, ok := geminiSchemaTypes[name]
			if !ok {
				return nil, fmt.Errorf("%s has unsupported type %q", memberPath, name)
			}
			typeName = mapped
		case "nullable":
			if err := json.Unmarshal(value, &nullable); err != nil {
				return nil, fmt.Errorf("%s must be a boolean", memberPath)
			}
		case "items":
			converted, err := convertGeminiSchemaAt(value, memberPath)
			if err != nil {
				return nil, err
			}
			out["items"] = converted
		case "properties":
			props, err := wire.JSONObject(string(value))
			if err != nil {
				return nil, fmt.Errorf("%s must be a JSON object: %w", memberPath, err)
			}
			converted := make(map[string]json.RawMessage, len(props))
			for name, prop := range props {
				c, err := convertGeminiSchemaAt(prop, memberPath+"."+name)
				if err != nil {
					return nil, err
				}
				converted[name] = c
			}
			encoded, err := json.Marshal(converted)
			if err != nil {
				return nil, err
			}
			out["properties"] = encoded
		case "anyOf":
			var arms []json.RawMessage
			if err := json.Unmarshal(value, &arms); err != nil {
				return nil, fmt.Errorf("%s must be an array", memberPath)
			}
			converted := make([]json.RawMessage, 0, len(arms))
			for i, arm := range arms {
				c, err := convertGeminiSchemaAt(arm, fmt.Sprintf("%s[%d]", memberPath, i))
				if err != nil {
					return nil, err
				}
				converted = append(converted, c)
			}
			encoded, err := json.Marshal(converted)
			if err != nil {
				return nil, err
			}
			out["anyOf"] = encoded
		case "example":
			encoded, err := json.Marshal([]json.RawMessage{value})
			if err != nil {
				return nil, err
			}
			out["examples"] = encoded
		case "propertyOrdering":
			// Ordering has no JSON Schema counterpart; the conversion note
			// names the schema conversion as a whole.
			continue
		default:
			if _, ok := geminiSchemaVerbatim[key]; ok {
				out[key] = value
				continue
			}
			if _, ok := geminiSchemaIntegers[key]; ok {
				n, err := geminiSchemaInteger(value)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", memberPath, err)
				}
				encoded, err := json.Marshal(n)
				if err != nil {
					return nil, err
				}
				out[key] = encoded
				continue
			}
			return nil, fmt.Errorf("%s is not a supported schema member", memberPath)
		}
	}
	if nullable {
		if typeName == "" {
			return nil, fmt.Errorf("%s is nullable but declares no type", path)
		}
		encoded, err := json.Marshal([]string{typeName, "null"})
		if err != nil {
			return nil, err
		}
		out["type"] = encoded
	} else if typeName != "" {
		encoded, err := json.Marshal(typeName)
		if err != nil {
			return nil, err
		}
		out["type"] = encoded
	}
	return json.Marshal(out)
}

// geminiSchemaInteger reads a proto int64 spelled as a JSON number or as a
// decimal string. Anything that is not a whole number is an error.
func geminiSchemaInteger(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, `"`) {
		var quoted string
		if err := json.Unmarshal(raw, &quoted); err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		text = quoted
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be an integer")
	}
	return n, nil
}
