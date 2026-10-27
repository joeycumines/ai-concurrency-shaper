package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
)

// vacuousBoundKeys are the JSON Schema numeric bound keywords a vacuous bound
// can appear under.
var vacuousBoundKeys = map[string]struct{}{
	"minimum":          {},
	"maximum":          {},
	"exclusiveMinimum": {},
	"exclusiveMaximum": {},
}

// dropVacuousNumericBounds removes every numeric bound keyword whose value is
// ±math.MaxFloat64 (or beyond) from a JSON Schema. Such a bound admits every
// finite number, so removing it does not change what the schema accepts. MCP
// servers built on zod emit it for every plain number, and some chat providers
// (Amazon Bedrock's OpenAI-compatible endpoint) reject the whole request
// because of it.
//
// A schema without such a bound is returned byte-identical with changed=false.
// A rewritten schema keeps every key in its original order and every number in
// its original spelling.
func dropVacuousNumericBounds(raw json.RawMessage) (json.RawMessage, bool, error) {
	if len(raw) == 0 || !bytes.Contains(raw, []byte("imum")) {
		return raw, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out bytes.Buffer
	changed := false
	if err := copyWithoutVacuousBounds(decoder, &out, &changed); err != nil {
		return nil, false, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false, fmt.Errorf("tool schema has trailing data")
	}
	if !changed {
		return raw, false, nil
	}
	return json.RawMessage(out.Bytes()), true, nil
}

// copyWithoutVacuousBounds copies one JSON value from decoder to out, skipping
// object members that are vacuous numeric bounds.
func copyWithoutVacuousBounds(decoder *json.Decoder, out *bytes.Buffer, changed *bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			out.WriteByte('{')
			first := true
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("tool schema object key is not a string")
				}
				if _, bound := vacuousBoundKeys[key]; bound {
					var member json.RawMessage
					if err := decoder.Decode(&member); err != nil {
						return err
					}
					if isVacuousBound(member) {
						*changed = true
						continue
					}
					if !first {
						out.WriteByte(',')
					}
					first = false
					writeJSONString(out, key)
					out.WriteByte(':')
					out.Write(bytes.TrimSpace(member))
					continue
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				writeJSONString(out, key)
				out.WriteByte(':')
				if err := copyWithoutVacuousBounds(decoder, out, changed); err != nil {
					return err
				}
			}
			if _, err := decoder.Token(); err != nil {
				return err
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for decoder.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if err := copyWithoutVacuousBounds(decoder, out, changed); err != nil {
					return err
				}
			}
			if _, err := decoder.Token(); err != nil {
				return err
			}
			out.WriteByte(']')
		default:
			return fmt.Errorf("tool schema has an unexpected delimiter %q", value)
		}
	case string:
		writeJSONString(out, value)
	case json.Number:
		out.WriteString(value.String())
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case nil:
		out.WriteString("null")
	default:
		return fmt.Errorf("tool schema has an unexpected token %T", token)
	}
	return nil
}

// isVacuousBound reports whether a bound keyword's raw value is a number at or
// beyond ±math.MaxFloat64.
func isVacuousBound(member json.RawMessage) bool {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(member))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return false
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		// Beyond float64 range parses as ±Inf with a range error: still vacuous.
		return math.IsInf(parsed, 0)
	}
	return math.Abs(parsed) >= math.MaxFloat64
}

func writeJSONString(out *bytes.Buffer, value string) {
	encoded, _ := json.Marshal(value)
	out.Write(encoded)
}
