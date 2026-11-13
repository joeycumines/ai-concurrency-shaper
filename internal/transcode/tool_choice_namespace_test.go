package transcode

// A named tool_choice identifies a tool the CLIENT knows: the bare function
// name it declared. The flatten can move that name (a plain function owns it, or
// an earlier child claimed it), and the wire carries only the flat name — so the
// adjudication has four distinct outcomes and each must be the honest one:
//
//   - a plain function owns the name: bind as sent, note the possible
//     reinterpretation;
//   - exactly one namespace child shares it: bind that child's flat wire name;
//   - nothing shares it but the name IS a proxy-invented flat name: refuse as
//     dangling, because the client could not have known that spelling;
//   - several children share it and no plain function owns it: refuse as
//     ambiguous.

import (
	"strings"
	"testing"
)

// decodeChoiceTools decodes a Responses body and returns the canonical tool
// names plus the recorded choice name.
func decodeChoiceTools(t *testing.T, tools string, choice string) ([]string, string, ConversionReport) {
	t.Helper()
	body := []byte(`{"model":"m","input":"hi","tool_choice":` + choice + `,"tools":` + tools + `}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := make([]string, 0, len(result.Request.Tools))
	for _, tool := range result.Request.Tools {
		names = append(names, tool.Name)
	}
	chosen := ""
	if result.Request.ToolChoice != nil {
		chosen = result.Request.ToolChoice.Name
	}
	return names, chosen, result.Report
}

// TestNamedToolChoiceBindsThePlainOwnerDespiteSiblingChildren proves the plain
// function wins even when several namespace children share the bare name: both
// children were qualified, so the wire's only tool answering to the name is the
// plain function. Refusing here would be a new availability failure for a
// request that was previously served.
func TestNamedToolChoiceBindsThePlainOwnerDespiteSiblingChildren(t *testing.T) {
	tools := `[` +
		`{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}},` +
		`{"type":"namespace","name":"nsA","tools":[{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}}]},` +
		`{"type":"namespace","name":"nsB","tools":[{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}}]}` +
		`]`
	names, chosen, report := decodeChoiceTools(t, tools, `{"type":"function","name":"foo"}`)
	if chosen != "foo" {
		t.Fatalf("tool_choice = %q, want the plain owner %q (tools: %v)", chosen, "foo", names)
	}
	if !reportHasAt(report, FeatureToolChoiceNamespaceBind, "tool_choice") {
		t.Fatalf("report = %+v, want the bind Note", report.Losses)
	}
}

// TestNamedToolChoiceTranslatesADisplacedUniqueChild proves a unique child whose
// flat name was displaced is named by that flat name: leaving the client's bare
// spelling would bind the choice to a DIFFERENT surviving tool.
func TestNamedToolChoiceTranslatesADisplacedUniqueChild(t *testing.T) {
	// plain "foo" owns foo. ns1's child foo qualifies to "ns1__foo"; ns2's
	// child is literally named "ns1__foo", which is taken, so it qualifies to
	// "ns2__ns1__foo". A client naming "ns1__foo" means ns2's child.
	tools := `[` +
		`{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}},` +
		`{"type":"namespace","name":"ns1","tools":[{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}}]},` +
		`{"type":"namespace","name":"ns2","tools":[{"type":"function","name":"ns1__foo","strict":false,"parameters":{"type":"object"}}]}` +
		`]`
	names, chosen, _ := decodeChoiceTools(t, tools, `{"type":"function","name":"ns1__foo"}`)
	if chosen != "ns2__ns1__foo" {
		t.Fatalf("tool_choice = %q, want the child's flat wire name %q (tools: %v)",
			chosen, "ns2__ns1__foo", names)
	}
	// The chosen name must be one of the tools the request declares.
	found := false
	for _, name := range names {
		if name == chosen {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_choice %q names no declared tool (tools: %v)", chosen, names)
	}
}

// TestNamedToolChoiceRefusesAProxyInventedFlatName proves a client cannot name a
// spelling only the proxy invents: with a lone namespace child flattened to
// "ns1__foo", the client could never have known that name, so accepting it would
// silently reinterpret a dangling reference.
func TestNamedToolChoiceRefusesAProxyInventedFlatName(t *testing.T) {
	tools := `[` +
		`{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}},` +
		`{"type":"namespace","name":"ns1","tools":[{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}}]}` +
		`]`
	body := []byte(`{"model":"m","input":"hi","tool_choice":{"type":"function","name":"ns1__foo"},"tools":` + tools + `}`)
	_, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("a proxy-invented flat name was accepted as a tool choice")
	}
	if !strings.Contains(err.Error(), "ns1__foo") {
		t.Fatalf("err = %q, want it to name the refused spelling", err)
	}
}

// TestNamedToolChoiceStillRefusesAGenuinelyAmbiguousName proves the ambiguity
// refusal survives where it is real: several children share the bare name and NO
// plain function owns it, so no wire tool is the referent.
func TestNamedToolChoiceStillRefusesAGenuinelyAmbiguousName(t *testing.T) {
	tools := `[` +
		`{"type":"namespace","name":"nsA","tools":[{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}}]},` +
		`{"type":"namespace","name":"nsB","tools":[{"type":"function","name":"foo","strict":false,"parameters":{"type":"object"}}]}` +
		`]`
	body := []byte(`{"model":"m","input":"hi","tool_choice":{"type":"function","name":"foo"},"tools":` + tools + `}`)
	_, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("an ambiguous named choice was accepted")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %q, want the ambiguity refusal", err)
	}
}

// TestNamedToolChoiceServesAUniqueChildUnderStrictPolicy is the availability
// guard: the common unqualified case still binds without a policy decision.
func TestNamedToolChoiceServesAUniqueChildUnderStrictPolicy(t *testing.T) {
	tools := `[` +
		`{"type":"namespace","name":"ns","tools":[{"type":"function","name":"search","strict":false,"parameters":{"type":"object"}}]}` +
		`]`
	names, chosen, report := decodeChoiceTools(t, tools, `{"type":"function","name":"search"}`)
	if chosen != "search" {
		t.Fatalf("tool_choice = %q, want the bare child name (tools: %v)", chosen, names)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureToolChoiceNamespaceBind {
			t.Fatalf("an unambiguous child name was noted as a reinterpretation: %+v", loss)
		}
	}
}
