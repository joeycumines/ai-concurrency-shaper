package transcode

import (
	"encoding/json"
	"testing"
)

func TestDropVacuousNumericBounds(t *testing.T) {
	clean := json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","minimum":0,"maximum":1e3}}}`)
	got, changed, err := dropVacuousNumericBounds(clean)
	if err != nil || changed || string(got) != string(clean) {
		t.Fatalf("clean schema: got %s changed=%v err=%v, want byte-identical", got, changed, err)
	}

	zod := json.RawMessage(`{"type":"object","properties":{"z":{"type":"string","description":"—"},"n":{"anyOf":[{"type":"number","minimum":-1.7976931348623157e+308,"maximum":1.7976931348623157e+308,"multipleOf":0.50},{"type":"string"}]},"m":{"type":"integer","exclusiveMaximum":1e309,"minimum":2}},"required":[]}`)
	got, changed, err = dropVacuousNumericBounds(zod)
	if err != nil || !changed {
		t.Fatalf("zod schema: changed=%v err=%v", changed, err)
	}
	want := `{"type":"object","properties":{"z":{"type":"string","description":"—"},"n":{"anyOf":[{"type":"number","multipleOf":0.50},{"type":"string"}]},"m":{"type":"integer","minimum":2}},"required":[]}`
	if string(got) != want {
		t.Fatalf("zod schema:\n got %s\nwant %s", got, want)
	}

	if _, _, err := dropVacuousNumericBounds(json.RawMessage(`{"maximum":1e400} {}`)); err == nil {
		t.Fatalf("trailing data must be an error")
	}
}
