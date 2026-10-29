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

	// Properties and definitions named after bound keywords are
	// sub-schemas, not bounds: they are kept, and their own bounds dropped.
	named := json.RawMessage(`{"type":"object","properties":{"maximum":{"type":"number","maximum":1.7976931348623157e308,"minimum":1},"minimum":{"anyOf":[{"type":"number","minimum":-1.7976931348623157e308}]}},"$defs":{"exclusiveMinimum":{"type":"number","exclusiveMinimum":-1e400}}}`)
	got, changed, err = dropVacuousNumericBounds(named)
	if err != nil || !changed {
		t.Fatalf("bound-named properties: changed=%v err=%v", changed, err)
	}
	want = `{"type":"object","properties":{"maximum":{"type":"number","minimum":1},"minimum":{"anyOf":[{"type":"number"}]}},"$defs":{"exclusiveMinimum":{"type":"number"}}}`
	if string(got) != want {
		t.Fatalf("bound-named properties:\n got %s\nwant %s", got, want)
	}
	// A quoted number under a bound keyword's name is data (here inside
	// default and examples), not a bound: it is kept verbatim.
	quoted := json.RawMessage(`{"type":"object","default":{"maximum":"1.7976931348623157e+308"},"examples":[{"minimum":"-1e400"}]}`)
	if got, changed, err := dropVacuousNumericBounds(quoted); err != nil || changed || string(got) != string(quoted) {
		t.Fatalf("quoted numeric strings: got %s changed=%v err=%v, want byte-identical", got, changed, err)
	}
	cleanNamed := json.RawMessage(`{"properties":{"maximum":{"type":"number", "maximum":10}}}`)
	if got, changed, err := dropVacuousNumericBounds(cleanNamed); err != nil || changed || string(got) != string(cleanNamed) {
		t.Fatalf("clean bound-named property: got %s changed=%v err=%v, want byte-identical", got, changed, err)
	}

	if _, _, err := dropVacuousNumericBounds(json.RawMessage(`{"maximum":1e400} {}`)); err == nil {
		t.Fatalf("trailing data must be an error")
	}
}
