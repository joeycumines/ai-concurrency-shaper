package openairesponses

// Comprehensive validation-branch coverage for the openairesponses types:
// the transcode suite drives the happy paths, and these tests exercise the
// rejection branches of every Validate method on the request/response/event
// types.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestToolValidateBranches(t *testing.T) {
	good := Tool{Type: "function", Name: "f", Strict: fieldBool(true)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	// Non-function (built-in) tool types decode and validate at the wire
	// layer; the loss/reject decision lives at the transcode boundary.
	if err := (Tool{Type: "web_search", Name: "f", Strict: fieldBool(true)}).Validate(); err != nil {
		t.Fatalf("built-in tool validate: %v", err)
	}
	// Namespace tools validate their nested tools and need no strict.
	namespace := Tool{
		Type: "namespace", Name: "ns",
		Tools: []Tool{{Type: "function", Name: "inner", Strict: fieldBool(false)}},
	}
	if err := namespace.Validate(); err != nil {
		t.Fatalf("namespace tool validate: %v", err)
	}
	if err := (Tool{Type: "namespace", Name: "ns"}).Validate(); err == nil {
		t.Fatal("tool-less namespace accepted")
	}
	if err := (Tool{
		Type: "namespace", Name: "ns",
		Tools: []Tool{{Type: "namespace", Name: "inner"}},
	}).Validate(); err == nil {
		t.Fatal("function-less nested namespace accepted")
	}
	if err := (Tool{Type: "function", Strict: fieldBool(true)}).Validate(); err == nil {
		t.Fatal("name-less tool accepted")
	}
	// Strict missing or null is a typed missing-required rejection.
	err := (Tool{Type: "function", Name: "f"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "missing_required") {
		t.Fatalf("missing strict err = %v", err)
	}
	err = (Tool{Type: "function", Name: "f", Strict: fieldBoolNull()}).Validate()
	if err == nil || !strings.Contains(err.Error(), "missing_required") {
		t.Fatalf("null strict err = %v", err)
	}
	// Marshal emits the bare strict value (never the presence bookkeeping).
	wire, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"type":"function","name":"f","strict":true}` {
		t.Fatalf("marshal = %s", wire)
	}
}

// TestToolValidateMissingType reproduces review-12 finding 4: a tool with an
// empty/missing type decodes into the lenient built-in branch and Validate's
// default arm returns nil, so it can be silently dropped under an approved
// builtin_tools loss instead of rejected as malformed. Missing type is never
// a built-in — built-ins are known non-empty strings — so it must be a
// typed missing-required error under every policy.
func TestToolValidateMissingType(t *testing.T) {
	err := (Tool{}).Validate()
	if err == nil {
		t.Fatal("empty-type tool accepted")
	}
	var de *wire.DecodeError
	if !errors.As(err, &de) || de.Kind != wire.DecodeMissingRequired {
		t.Fatalf("err = %v, want DecodeError{missing_required}", err)
	}
	if !strings.Contains(de.Path, "type") {
		t.Fatalf("path = %q, want it to name type", de.Path)
	}
	// An explicitly null type field is the same defect: not a built-in.
	err = (Tool{Type: "", Name: "f"}).Validate()
	if err == nil {
		t.Fatal("empty-string type tool accepted")
	}
	if !errors.As(err, &de) || de.Kind != wire.DecodeMissingRequired {
		t.Fatalf("err = %v, want DecodeError{missing_required}", err)
	}
}

// TestToolValidateCrossTypeFields reproduces review-12 finding 4: cross-type
// fields (tools on a function tool; parameters/strict on a namespace tool)
// are accepted by the shared decode struct then silently ignored. The union
// must reject them with typed errors so a malformed tool definition never
// reaches the converter with half its fields discarded.
func TestToolValidateCrossTypeFields(t *testing.T) {
	// A function tool carrying nested tools is a shape violation: the
	// converter would ignore tool.Tools entirely.
	err := (Tool{
		Type:   "function",
		Name:   "f",
		Strict: fieldBool(true),
		Tools:  []Tool{{Type: "function", Name: "inner", Strict: fieldBool(true)}},
	}).Validate()
	if err == nil {
		t.Fatal("function tool with nested tools accepted")
	}
	if !strings.Contains(err.Error(), "tools") {
		t.Fatalf("err = %v, want it to name tools", err)
	}

	// A namespace tool carrying parameters is a shape violation.
	err = (Tool{
		Type:       "namespace",
		Name:       "ns",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Tools:      []Tool{{Type: "function", Name: "inner", Strict: fieldBool(true)}},
	}).Validate()
	if err == nil {
		t.Fatal("namespace tool with parameters accepted")
	}
	if !strings.Contains(err.Error(), "parameters") {
		t.Fatalf("err = %v, want it to name parameters", err)
	}

	// A namespace tool carrying strict is a shape violation.
	err = (Tool{
		Type:   "namespace",
		Name:   "ns",
		Strict: fieldBool(true),
		Tools:  []Tool{{Type: "function", Name: "inner", Strict: fieldBool(true)}},
	}).Validate()
	if err == nil {
		t.Fatal("namespace tool with strict accepted")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Fatalf("err = %v, want it to name strict", err)
	}
}

// TestToolUnmarshalMissingTypeIsMalformed proves a missing/null type is
// rejected at DECODE time so it can never reach the builtin_tools loss path,
// under every policy.
func TestToolUnmarshalMissingTypeIsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing type", `{"name":"f","strict":true}`},
		{"null type", `{"type":null,"name":"f","strict":true}`},
		{"empty type", `{"type":"","name":"f","strict":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tool Tool
			if err := json.Unmarshal([]byte(tc.body), &tool); err == nil {
				t.Fatalf("decoded without error; tool = %+v", tool)
			} else {
				var de *wire.DecodeError
				if !errors.As(err, &de) || de.Kind != wire.DecodeMissingRequired {
					t.Fatalf("err = %v, want DecodeError{missing_required}", err)
				}
			}
		})
	}

	// A genuine built-in (web_search) still decodes — the loss/reject path
	// for built-ins is unchanged.
	var tool Tool
	if err := json.Unmarshal([]byte(`{"type":"web_search","name":"ws"}`), &tool); err != nil {
		t.Fatalf("built-in tool decode: %v", err)
	}
	if tool.Type != "web_search" {
		t.Fatalf("tool = %+v", tool)
	}
}

// TestToolUnmarshalCrossTypeFieldsRejected proves cross-type fields are
// rejected at DECODE time, not silently swallowed into the shared struct.
func TestToolUnmarshalCrossTypeFieldsRejected(t *testing.T) {
	// function tool with tools.
	var fn Tool
	if err := json.Unmarshal([]byte(`{"type":"function","name":"f","strict":true,"tools":[{"type":"function","name":"inner","strict":true}]}`), &fn); err == nil {
		t.Fatalf("function tool with tools decoded; tool = %+v", fn)
	} else if !strings.Contains(err.Error(), "tools") {
		t.Fatalf("err = %v, want it to name tools", err)
	}
	// namespace tool with parameters.
	var ns Tool
	if err := json.Unmarshal([]byte(`{"type":"namespace","name":"ns","parameters":{"type":"object"},"tools":[{"type":"function","name":"inner","strict":true}]}`), &ns); err == nil {
		t.Fatalf("namespace tool with parameters decoded; tool = %+v", ns)
	} else if !strings.Contains(err.Error(), "parameters") {
		t.Fatalf("err = %v, want it to name parameters", err)
	}
	// namespace tool with strict.
	if err := json.Unmarshal([]byte(`{"type":"namespace","name":"ns","strict":true,"tools":[{"type":"function","name":"inner","strict":true}]}`), &ns); err == nil {
		t.Fatalf("namespace tool with strict decoded; tool = %+v", ns)
	} else if !strings.Contains(err.Error(), "strict") {
		t.Fatalf("err = %v, want it to name strict", err)
	}
}

func fieldBool(value bool) wire.Field[bool] {
	return wire.Field[bool]{Value: value, Present: true}
}

func fieldBoolNull() wire.Field[bool] {
	return wire.Field[bool]{Present: true, Null: true}
}

func TestToolChoiceValidateBranches(t *testing.T) {
	named := &ToolChoiceNamed{Type: "function", Name: "f"}
	if err := (ToolChoice{Named: named}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ToolChoice{Named: &ToolChoiceNamed{Type: "auto", Name: "f"}}).Validate(); err == nil {
		t.Fatal("non-function named choice accepted")
	}
	if err := (ToolChoice{Named: &ToolChoiceNamed{Type: "function"}}).Validate(); err == nil {
		t.Fatal("name-less named choice accepted")
	}
	bad := "always"
	if err := (ToolChoice{Str: &bad}).Validate(); err == nil {
		t.Fatal("invalid choice string accepted")
	}
	if err := (ToolChoice{}).Validate(); err == nil {
		t.Fatal("empty choice accepted")
	}
	both := "auto"
	if err := (ToolChoice{Str: &both, Named: named}).Validate(); err == nil {
		t.Fatal("both variants accepted")
	}
	// Unmarshal rejects an unknown union shape.
	var decoded ToolChoice
	if err := json.Unmarshal([]byte(`{"type":"function","name":"f"} {"x":1}`), &decoded); err == nil {
		t.Fatal("trailing value accepted")
	}
}

func TestInputValidateBranches(t *testing.T) {
	text := "hi"
	if err := (Input{Text: &text}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Input{Text: &text, Items: InputItems{}}).Validate(); err == nil {
		t.Fatal("both input variants accepted")
	}
	if err := (Input{}).Validate(); err == nil {
		t.Fatal("empty input accepted")
	}

	if err := (InputText{Type: "input_text", Text: "x"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (InputText{Type: "bogus", Text: "x"}).Validate(); err == nil {
		t.Fatal("bad input_text type accepted")
	}

	if err := (InputImage{Type: "input_image", ImageURL: "u"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (InputImage{Type: "input_image"}).Validate(); err == nil {
		t.Fatal("source-less input_image accepted")
	}
	if err := (InputImage{Type: "input_image", ImageURL: "u", FileID: "f"}).Validate(); err == nil {
		t.Fatal("dual-source input_image accepted")
	}
	if err := (InputImage{Type: "input_image", ImageURL: "u", Detail: "bogus"}).Validate(); err == nil {
		t.Fatal("bad detail accepted")
	}

	if err := (InputFile{Type: "input_file", FileID: "f"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (InputFile{Type: "input_file"}).Validate(); err == nil {
		t.Fatal("source-less input_file accepted")
	}
	if err := (InputFile{Type: "input_file", FileID: "f", FileURL: "u"}).Validate(); err == nil {
		t.Fatal("dual-source input_file accepted")
	}
	if err := (InputFile{Type: "input_file", FileID: "f", Detail: "bogus"}).Validate(); err == nil {
		t.Fatal("bad file detail accepted")
	}
}

func TestInputMessageContentBranches(t *testing.T) {
	text := "hi"
	if err := (InputMessageContent{Text: &text}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (InputMessageContent{Text: &text, Parts: InputContentParts{}}).Validate(); err == nil {
		t.Fatal("both content variants accepted")
	}
	if err := (InputMessageContent{}).Validate(); err == nil {
		t.Fatal("empty content accepted")
	}
}

func TestInputItemValidateBranches(t *testing.T) {
	if err := (&EasyInputMessage{Role: InputRoleUser, Content: InputMessageContent{Text: stringP("hi")}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&EasyInputMessage{Role: "bogus", Content: InputMessageContent{Text: stringP("hi")}}).Validate(); err == nil {
		t.Fatal("bad role accepted")
	}
	if err := (&EasyInputMessage{Role: InputRoleUser, Content: InputMessageContent{Text: stringP("hi")}, Phase: "bogus"}).Validate(); err == nil {
		t.Fatal("bad phase accepted")
	}
	if err := (&EasyInputMessage{Role: InputRoleUser, Content: InputMessageContent{Text: stringP("hi")}, Type: "bogus"}).Validate(); err == nil {
		t.Fatal("bad type accepted")
	}

	output := OutputContentParts{&OutputText{Type: "output_text", Text: "x", Annotations: []Annotation{}}}
	if err := (&PreviousOutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted, Content: output,
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&PreviousOutputMessage{
		Type: "message", Role: "assistant", Status: ItemCompleted, Content: output,
	}).Validate(); err == nil {
		t.Fatal("id-less previous output accepted")
	}
	if err := (&PreviousOutputMessage{
		ID: "msg_1", Type: "message", Role: "user", Status: ItemCompleted, Content: output,
	}).Validate(); err == nil {
		t.Fatal("non-assistant previous output accepted")
	}

	if err := (&FunctionCallInput{
		Type: "function_call", CallID: "c", Name: "f", Arguments: `{}`,
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&FunctionCallInput{
		Type: "function_call", Name: "f", Arguments: `{}`,
	}).Validate(); err == nil {
		t.Fatal("call_id-less input accepted")
	}
	if err := (&FunctionCallInput{
		Type: "function_call", CallID: "c", Arguments: `{}`,
	}).Validate(); err == nil {
		t.Fatal("name-less input accepted")
	}
	if err := (&FunctionCallInput{
		Type: "function_call", CallID: "c", Name: "f", Arguments: `not json`,
	}).Validate(); err == nil {
		t.Fatal("invalid arguments accepted")
	}

	if err := (&FunctionCallOutputInput{
		Type: "function_call_output", CallID: "c", Output: FunctionOutput{Text: stringP("ok")},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&FunctionCallOutputInput{
		Type: "function_call_output", Output: FunctionOutput{Text: stringP("ok")},
	}).Validate(); err == nil {
		t.Fatal("call_id-less output input accepted")
	}

	if err := (&ReasoningInput{
		ID: "rs_1", Type: "reasoning", Summary: []ReasoningSummary{},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&ReasoningInput{
		Type: "reasoning", Summary: []ReasoningSummary{},
	}).Validate(); err == nil {
		t.Fatal("id-less reasoning accepted")
	}
	if err := (&ReasoningInput{
		ID: "rs_1", Type: "reasoning",
	}).Validate(); err == nil {
		t.Fatal("summary-less reasoning accepted")
	}
	if err := (&ReasoningInput{
		ID: "rs_1", Type: "reasoning",
		Summary: []ReasoningSummary{{Type: "bogus", Text: "x"}},
	}).Validate(); err == nil {
		t.Fatal("bad summary type accepted")
	}

	if err := (&ItemReferenceInput{Type: "item_reference", ID: "i"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&ItemReferenceInput{Type: "item_reference"}).Validate(); err == nil {
		t.Fatal("id-less item reference accepted")
	}
}

func TestOutputItemValidateBranches(t *testing.T) {
	content := OutputContentParts{&OutputText{Type: "output_text", Text: "x", Annotations: []Annotation{}}}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted, Content: content,
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted, Content: nil,
	}).Validate(); err == nil {
		t.Fatal("nil content accepted")
	}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted,
		Content: OutputContentParts{&OutputText{Type: "output_text", Text: "x"}},
	}).Validate(); err == nil {
		t.Fatal("annotations-less output_text accepted")
	}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted,
		Content: OutputContentParts{&OutputText{Type: "bogus", Text: "x", Annotations: []Annotation{}}},
	}).Validate(); err == nil {
		t.Fatal("bad output_text type accepted")
	}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted,
		Content: OutputContentParts{&OutputRefusal{Type: "bogus", Refusal: "r"}},
	}).Validate(); err == nil {
		t.Fatal("bad refusal type accepted")
	}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: "bogus", Content: content,
	}).Validate(); err == nil {
		t.Fatal("bad status accepted")
	}
	if err := (&OutputMessage{
		ID: "msg_1", Type: "message", Role: "assistant", Status: ItemCompleted, Content: content, Phase: "bogus",
	}).Validate(); err == nil {
		t.Fatal("bad phase accepted")
	}

	if err := (&FunctionCallOutputItem{
		ID: "fc_1", Type: "function_call", Status: ItemCompleted, CallID: "c", Name: "f", Arguments: `{}`,
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&FunctionCallOutputItem{
		Type: "function_call", Status: ItemCompleted, CallID: "c", Name: "f", Arguments: `{}`,
	}).Validate(); err == nil {
		t.Fatal("id-less function call accepted")
	}
	if err := (&FunctionCallOutputItem{
		ID: "fc_1", Type: "function_call", Status: ItemCompleted, CallID: "c", Arguments: `{}`,
	}).Validate(); err == nil {
		t.Fatal("name-less function call accepted")
	}
	// Invalid model-generated arguments are preserved exactly: any string
	// is legal on the wire (review-z commit 2).
	if err := (&FunctionCallOutputItem{
		ID: "fc_1", Type: "function_call", Status: ItemCompleted, CallID: "c", Name: "f", Arguments: `{`,
	}).Validate(); err != nil {
		t.Fatalf("invalid model arguments rejected: %v", err)
	}

	if err := (&FunctionCallOutputResultItem{
		ID: "fc_2", Type: "function_call_output", Status: ItemCompleted, CallID: "c",
		Output: FunctionOutput{Text: stringP("ok")},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&FunctionCallOutputResultItem{
		Type: "function_call_output", Status: ItemCompleted, CallID: "c",
		Output: FunctionOutput{Text: stringP("ok")},
	}).Validate(); err == nil {
		t.Fatal("id-less result accepted")
	}

	reasoning := &ReasoningOutputItem{
		ID: "rs_1", Type: "reasoning", Status: ItemCompleted, Summary: []ReasoningSummary{},
	}
	if err := reasoning.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&ReasoningOutputItem{
		Type: "reasoning", Status: ItemCompleted, Summary: []ReasoningSummary{},
	}).Validate(); err == nil {
		t.Fatal("id-less reasoning output accepted")
	}
	if err := (&ReasoningOutputItem{
		ID: "rs_1", Type: "reasoning", Status: ItemCompleted, Summary: []ReasoningSummary{{Type: "bogus"}},
	}).Validate(); err == nil {
		t.Fatal("bad summary accepted")
	}
	if err := (&ReasoningOutputItem{
		ID: "rs_1", Type: "reasoning", Status: ItemCompleted, Summary: []ReasoningSummary{},
		Content: []ReasoningText{{Type: "bogus"}},
	}).Validate(); err == nil {
		t.Fatal("bad reasoning content accepted")
	}
}

func stringP(s string) *string { return &s }

// TestFunctionCallOutputResultBranches covers the result item's type and
// status rejection branches.
func TestFunctionCallOutputResultBranches(t *testing.T) {
	good := &FunctionCallOutputResultItem{
		ID: "fc_2", Type: "function_call_output", Status: ItemCompleted, CallID: "c",
		Output: FunctionOutput{Text: stringP("ok")},
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	badType := *good
	badType.Type = "bogus"
	if err := badType.Validate(); err == nil {
		t.Fatal("bad type accepted")
	}
	badStatus := *good
	badStatus.Status = "bogus"
	if err := badStatus.Validate(); err == nil {
		t.Fatal("bad status accepted")
	}
	badOutput := *good
	badOutput.Output = FunctionOutput{}
	if err := badOutput.Validate(); err == nil {
		t.Fatal("empty output accepted")
	}
}
