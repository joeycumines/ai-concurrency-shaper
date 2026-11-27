package transcode

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// toolChangeDef builds one tool_addition system block whose db_query
// definition declares the given property name.
func toolChangeDef(prop string) string {
	return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":` +
		`{"name":"db_query","input_schema":{"type":"object","properties":{"` + prop + `":{"type":"string"}}}}}}`
}

// TestToolRedefinitionIsPolicyGated proves a genuine mid-conversation
// redefinition loses the historical meaning of the earlier definition, so it
// is a policy-gated loss under FeatureAnthropicToolChanges: StrictLossPolicy
// refuses it with a named UnsupportedFeatureError, while the default approved
// policy applies last-definition-wins and records BOTH the Loss (at
// .tool.definition) and the combined Note (at the block path). A definition
// that is NEW (no earlier definition of that name exists) is only hoisted, so
// it stays Note-only and succeeds even under strict policy.
func TestToolRedefinitionIsPolicyGated(t *testing.T) {
	body := func(propA, propB string) []byte {
		// Two inline definitions of db_query: the second either repeats or
		// genuinely redefines the first.
		return []byte(`{"model":"m","max_tokens":16,` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + toolChangeDef(propA) + `]},` +
			`{"role":"system","content":[` + toolChangeDef(propB) + `]}]}`)
	}
	// hoistedOnly defines a tool that no declared tool shares a name with:
	// the inline definition is hoisted (Note-only) but never supersedes.
	hoistedOnly := []byte(`{"model":"m","max_tokens":16,` +
		`"messages":[{"role":"user","content":"x"},` +
		`{"role":"system","content":[` + toolChangeDef("a") + `]}]}`)
	// declaredShadow redefines a DECLARED tool mid-conversation: the declared
	// schema {"type":"object"} differs from the inline one, so this is a
	// genuine supersession of the declared definition.
	declaredShadow := []byte(`{"model":"m","max_tokens":16,` +
		`"tools":[{"name":"db_query","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"x"},` +
		`{"role":"system","content":[` + toolChangeDef("a") + `]}]}`)

	t.Run("strict policy refuses a genuine redefinition", func(t *testing.T) {
		_, err := DecodeMessagesRequest(body("a", "b"), StrictLossPolicy())
		var unsupported *UnsupportedFeatureError
		if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureAnthropicToolChanges) {
			t.Fatalf("err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureAnthropicToolChanges)
		}
		// Shadowing a declared tool is also a genuine supersession.
		if _, err := DecodeMessagesRequest(declaredShadow, StrictLossPolicy()); err == nil {
			t.Fatal("strict policy accepted a redefinition of a declared tool")
		}
	})

	t.Run("approved policy applies last-definition-wins with Loss and Note", func(t *testing.T) {
		policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
		result, err := DecodeMessagesRequest(body("a", "b"), policy)
		if err != nil {
			t.Fatalf("approved policy refused the redefinition: %v", err)
		}
		count := 0
		for _, tool := range result.Request.Tools {
			if tool.Name != "db_query" {
				continue
			}
			count++
			if !bytes.Contains(tool.JSONSchema, []byte(`"b"`)) {
				t.Fatalf("tool schema = %s, want the newest definition", tool.JSONSchema)
			}
		}
		if count != 1 {
			t.Fatalf("%q appears %d times, want exactly one", "db_query", count)
		}
		var sawLoss, sawNote bool
		for _, loss := range result.Report.Losses {
			if loss.Feature != FeatureAnthropicToolChanges {
				continue
			}
			switch loss.Kind {
			case LossRecord:
				if strings.Contains(loss.Path, ".tool.definition") && strings.Contains(loss.Detail, "redefined") {
					sawLoss = true
				}
			case NoteRecord:
				if strings.Contains(loss.Detail, "redefined") {
					sawNote = true
				}
			}
		}
		if !sawLoss {
			t.Fatalf("no Loss recorded for the supersession: %+v", result.Report.Losses)
		}
		if !sawNote {
			t.Fatalf("no Note recorded for the supersession: %+v", result.Report.Losses)
		}
		if err := ValidateCanonicalRequest(result.Request); err != nil {
			t.Fatalf("validator refused the decoder output: %v", err)
		}
	})

	t.Run("an equal repeat succeeds under strict policy with a Note only", func(t *testing.T) {
		result, err := DecodeMessagesRequest(body("a", "a"), StrictLossPolicy())
		if err != nil {
			t.Fatalf("an equal repeat was refused under strict policy: %v", err)
		}
		for _, loss := range result.Report.Losses {
			if loss.Feature == FeatureAnthropicToolChanges && loss.Kind == LossRecord {
				t.Fatalf("an equal repeat recorded a Loss, want Note only: %+v", loss)
			}
		}
	})

	t.Run("a newly hoisted tool succeeds under strict policy with a Note only", func(t *testing.T) {
		result, err := DecodeMessagesRequest(hoistedOnly, StrictLossPolicy())
		if err != nil {
			t.Fatalf("a new inline tool was refused under strict policy: %v", err)
		}
		if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "db_query" {
			t.Fatalf("hoisted tool missing: %+v", result.Request.Tools)
		}
		for _, loss := range result.Report.Losses {
			if loss.Feature == FeatureAnthropicToolChanges && loss.Kind == LossRecord {
				t.Fatalf("a hoist recorded a Loss, want Note only: %+v", loss)
			}
		}
	})

	t.Run("histories differing only in the original definition are distinguishable", func(t *testing.T) {
		// Under strict policy the changed history is refused rather than
		// silently decoded to the same bytes as the unchanged one.
		_, errChanged := DecodeMessagesRequest(body("a", "b"), StrictLossPolicy())
		if errChanged == nil {
			t.Fatal("changed history accepted under strict policy")
		}
		if _, errSame := DecodeMessagesRequest(body("a", "a"), StrictLossPolicy()); errSame != nil {
			t.Fatalf("unchanged history refused: %v", errSame)
		}
		if _, ok := errors.AsType[*UnsupportedFeatureError](errChanged); !ok {
			t.Fatalf("changed history refused with %T %v, want UnsupportedFeatureError", errChanged, errChanged)
		}
	})
}
