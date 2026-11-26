package transcode

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Numerically equal schema values are unchanged definitions even when their
// exponents cannot fit an int. Repeating one needs only the timing Note;
// genuinely different values still require schema-replacement approval.
func TestMessagesToolDefinitionExactExponents(t *testing.T) {
	nines := strings.Repeat("9", 4096)
	power := "1" + strings.Repeat("0", 4096)
	for _, tc := range []struct {
		name, previous, latest string
		equal                  bool
	}{
		{"huge positive exponent", "1e2000000000", "10e1999999999", true},
		{"huge negative exponent", "1e-2000000000", "10e-2000000001", true},
		{"old positive boundary", "1e1073741824", "10e1073741823", true},
		{"beyond old positive boundary", "1e1073741825", "10e1073741824", true},
		{"beyond old negative boundary", "1e-1073741825", "0.1e-1073741824", true},
		{"positive exponent carry", "10e" + nines, "1e" + power, true},
		{"positive exponent borrow", "0.1e" + power, "1e" + nines, true},
		{"negative exponent carry", "0.1e-" + nines, "1e-" + power, true},
		{"negative exponent borrow", "10e-" + power, "1e-" + nines, true},
		{"huge padded exponent", "1e+000" + nines, "1e" + nines, true},
		{"negative exponent becomes zero", "10e-1", "1e+000", true},
		{"positive exponent becomes zero", "0.1e1", "1e-000", true},
		{"negative exponent becomes positive", "100e-1", "1e1", true},
		{"positive exponent becomes negative", "0.001e1", "1e-2", true},
		{"fraction and trailing zeros", "0.00100e2000000000", "1e1999999997", true},
		{"fraction with internal zeros", "1000.0001e2000000000", "10000001e1999999996", true},
		{"zero ignores sign and exponent", "-0.000e" + nines, "0e-" + power, true},
		{"distinct huge positive values", "1e2000000000", "1e2000000001", false},
		{"distinct huge negative values", "1e-2000000000", "1e-2000000001", false},
		{"distinct thousand-digit exponents", "1e" + nines, "1e" + power, false},
		{"distinct significant digits", "1e" + nines, "2e" + nines, false},
		{"distinct signs", "1e" + nines, "-1e" + nines, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := func(number string) string {
				return `{"type":"object","properties":{"n":{"const":` + number + `}}}`
			}
			latestSchema := schema(tc.latest)
			body := []byte(`{"model":"m","max_tokens":16,"tools":[{"name":"db","input_schema":` + schema(tc.previous) + `}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":` + latestSchema + `}}}]}]}`)
			for _, approved := range []bool{false, true} {
				name, policy := "strict", StrictLossPolicy()
				if approved {
					name = "approved"
					policy = LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
				}
				t.Run(name, func(t *testing.T) {
					result, err := DecodeMessagesRequest(body, policy)
					if !tc.equal && !approved {
						var unsupported *UnsupportedFeatureError
						if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureAnthropicToolChanges) ||
							unsupported.Path != "messages[1].content[0].tool.definition" {
							t.Fatalf("different schema must require its loss approval: %T: %v", err, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("schema conversion (equal=%v, approved=%v): %v", tc.equal, approved, err)
					}
					if len(result.Request.Tools) != 1 || !bytes.Equal(result.Request.Tools[0].JSONSchema, []byte(latestSchema)) {
						t.Fatal("comparison changed the latest schema's numeric spelling")
					}
					losses, notes := 0, 0
					for _, entry := range result.Report.Losses {
						if entry.Feature != FeatureAnthropicToolChanges {
							t.Fatalf("unexpected conversion report entry: %+v", entry)
						}
						switch entry.Kind {
						case LossRecord:
							if tc.equal || entry.Path != "messages[1].content[0].tool.definition" {
								t.Fatalf("incorrect schema-replacement loss: %+v", entry)
							}
							losses++
						case NoteRecord:
							word := "redefined"
							if tc.equal {
								word = "repeats"
							}
							if entry.Path != "messages[1].content[0]" || !strings.Contains(entry.Detail, word) {
								t.Fatalf("incorrect definition note: %+v", entry)
							}
							notes++
						}
					}
					wantLosses := 1
					if tc.equal {
						wantLosses = 0
					}
					if losses != wantLosses || notes != 1 {
						t.Fatalf("report = %d losses + %d notes, want %d + 1", losses, notes, wantLosses)
					}
					rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
					if err != nil {
						t.Fatalf("render accepted schema: %v", err)
					}
					if !bytes.Contains(rendered, []byte(`"const":`+tc.latest)) {
						t.Fatal("rendering changed the latest schema's numeric spelling")
					}
				})
			}
		})
	}
}
