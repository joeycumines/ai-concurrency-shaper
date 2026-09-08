package wire

import (
	"encoding/json"
)

// NullOmitString is a string field that tolerates an explicit JSON null:
// null decodes to the empty value (the field's contract decides whether
// that means absent), and omitempty omits the empty value from rendered
// output. It exists for provider-opaque optional pass-through fields on the
// CLIENT request contract where real flagship clients (codex-tui, observed
// 2026-09-08) send explicit nulls — null there means "absent", never a
// fabricated empty value, because the field is only ever relayed unchanged
// or omitted. It is NOT a general relaxation: modeled semantic fields keep
// the strict illegal-null rejection.
type NullOmitString string

// UnmarshalJSON tolerates an explicit null and otherwise decodes the string.
func (n *NullOmitString) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceJSON(data)
	if string(trimmed) == "null" {
		*n = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	*n = NullOmitString(s)
	return nil
}

// MarshalJSON renders the bare string value (omitempty governs key presence).
func (n NullOmitString) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(n))
}

func trimSpaceJSON(data []byte) []byte {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\t' || data[end-1] == '\n' || data[end-1] == '\r') {
		end--
	}
	return data[start:end]
}
