// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseModelTableEntry_HappyBare parses a bare entry with no facts.
func TestParseModelTableEntry_HappyBare(t *testing.T) {
	entry, err := parseModelTableEntry("opus@anthropic=claude-opus-4-1")
	if err != nil {
		t.Fatal(err)
	}
	want := modelTableEntry{
		Surrogate: "opus",
		Provider:  "anthropic",
		Wire:      "claude-opus-4-1",
		Raw:       "opus@anthropic=claude-opus-4-1",
	}
	if !reflect.DeepEqual(entry, want) {
		t.Fatalf("entry = %+v, want %+v", entry, want)
	}
}

// TestParseModelTableEntry_HappyFullFacts parses every fact kind at once.
func TestParseModelTableEntry_HappyFullFacts(t *testing.T) {
	entry, err := parseModelTableEntry(
		"s@openai=w;context=128000;max_output=4096;efforts=minimal+low+medium+high;modalities=text+image+audio;default",
	)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Context == nil || *entry.Context != 128000 {
		t.Fatalf("Context = %v", entry.Context)
	}
	if entry.MaxOutput == nil || *entry.MaxOutput != 4096 {
		t.Fatalf("MaxOutput = %v", entry.MaxOutput)
	}
	if want := []string{"minimal", "low", "medium", "high"}; !reflect.DeepEqual(entry.Efforts, want) {
		t.Fatalf("Efforts = %v, want %v", entry.Efforts, want)
	}
	if want := []string{"text", "image", "audio"}; !reflect.DeepEqual(entry.Modalities, want) {
		t.Fatalf("Modalities = %v, want %v", entry.Modalities, want)
	}
	if !entry.Default {
		t.Fatal("Default = false, want true")
	}
	if entry.Deprecated {
		t.Fatal("Deprecated = true, want false")
	}
}

// TestParseModelTableEntry_HappyDeprecated parses the bare deprecated flag.
func TestParseModelTableEntry_HappyDeprecated(t *testing.T) {
	entry, err := parseModelTableEntry("old@openai=gpt-3.5-turbo;deprecated")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Deprecated {
		t.Fatal("Deprecated = false, want true")
	}
	if entry.Default {
		t.Fatal("Default = true, want false")
	}
}

// TestParseModelTableEntry_WireOpaqueChars accepts every character the wire-id
// charset allows: the wire id is opaque to the shaper.
func TestParseModelTableEntry_WireOpaqueChars(t *testing.T) {
	entry, err := parseModelTableEntry("a@b=openai/gpt-4o:plus=v2@x+y")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Wire != "openai/gpt-4o:plus=v2@x+y" {
		t.Fatalf("Wire = %q", entry.Wire)
	}
}

// TestParseModelTableEntry_MissingEquals rejects an entry without the
// surrogate@provider=wire split point.
func TestParseModelTableEntry_MissingEquals(t *testing.T) {
	assertModelTableParseError(t, "opus@anthropic",
		`invalid -model-table "opus@anthropic": want surrogate@provider=wire[;facts]`)
}

// TestParseModelTableEntry_MissingAt rejects an entry whose left side has no
// provider separator.
func TestParseModelTableEntry_MissingAt(t *testing.T) {
	assertModelTableParseError(t, "opus=wire",
		`invalid -model-table "opus=wire": want surrogate@provider=wire[;facts]`)
}

// TestParseModelTableEntry_EmptyParts names the empty part in every position.
func TestParseModelTableEntry_EmptyParts(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"@p=w", `invalid -model-table "@p=w": empty surrogate`},
		{"s@=w", `invalid -model-table "s@=w": empty provider`},
		{"s@p=", `invalid -model-table "s@p=": empty wire id`},
		{"s@p=;context=1", `invalid -model-table "s@p=;context=1": empty wire id`},
	}
	for _, tc := range cases {
		assertModelTableParseError(t, tc.raw, tc.want)
	}
}

// TestParseModelTableEntry_BadSurrogateCharset rejects every character the
// surrogate charset forbids.
func TestParseModelTableEntry_BadSurrogateCharset(t *testing.T) {
	for _, raw := range []string{
		"my model@a=b",
		"a,b@c=d",
		"a/b@c=d",
		"a:b@c=d",
		"a?b@c=d",
		"a#b@c=d",
		strings.Repeat("s", 129) + "@a=b",
	} {
		err := modelTableParseErr(t, raw)
		if !strings.Contains(err, `invalid surrogate`) || !strings.Contains(err, `want 1-128 chars of [A-Za-z0-9._-]`) {
			t.Errorf("error for %q = %q, want invalid surrogate charset", raw, err)
		}
	}

	// A second '@' in the left side lands in the provider, whose charset
	// forbids it: the wire id (right of the first '=') may still contain '@'.
	err := modelTableParseErr(t, "a@b@c=d")
	if !strings.Contains(err, `invalid provider "b@c"`) {
		t.Errorf("second @ in left side error = %q, want invalid provider", err)
	}
}

// TestParseModelTableEntry_BadWireCharset rejects every character the wire
// charset forbids, and the length bound.
func TestParseModelTableEntry_BadWireCharset(t *testing.T) {
	for _, raw := range []string{
		"s@p=w w",
		"s@p=w,w",
		"s@p=w?w",
		"s@p=w#w",
		"s@p=" + strings.Repeat("w", 257),
	} {
		err := modelTableParseErr(t, raw)
		if !strings.Contains(err, `invalid wire id`) || !strings.Contains(err, `want 1-256 chars of [A-Za-z0-9._\-/:@=+]`) {
			t.Errorf("error for %q = %q, want invalid wire id charset", raw, err)
		}
	}
}

// TestParseModelTableEntry_EmptyFact rejects a trailing or empty fact segment.
func TestParseModelTableEntry_EmptyFact(t *testing.T) {
	assertModelTableParseError(t, "s@p=w;",
		`invalid -model-table "s@p=w;": empty fact`)
	assertModelTableParseError(t, "s@p=w;;context=1",
		`invalid -model-table "s@p=w;;context=1": empty fact`)
	assertModelTableParseError(t, "s@p=w; context=1",
		`invalid -model-table "s@p=w; context=1": empty fact`)
}

// TestParseModelTableEntry_UnknownFact rejects an unmodelled fact key.
func TestParseModelTableEntry_UnknownFact(t *testing.T) {
	assertModelTableParseError(t, "s@p=w;foo=1",
		`invalid -model-table "s@p=w;foo=1": unknown fact "foo" (want context, max_output, efforts, modalities, default, deprecated, cost_input, cost_output, tags, description, created)`)
}

func TestParseModelTableEntry_RichFacts(t *testing.T) {
	entry, err := parseModelTableEntry("s@p=w;cost_input=0.0015;cost_output=0.002;tags=chat+fast;description=Fast_model;created=1710000000")
	if err != nil {
		t.Fatal(err)
	}
	if entry.CostInput == nil || *entry.CostInput != 0.0015 {
		t.Errorf("CostInput = %v", entry.CostInput)
	}
	if entry.CostOutput == nil || *entry.CostOutput != 0.002 {
		t.Errorf("CostOutput = %v", entry.CostOutput)
	}
	if want := []string{"chat", "fast"}; !reflect.DeepEqual(entry.Tags, want) {
		t.Errorf("Tags = %v, want %v", entry.Tags, want)
	}
	if entry.Description != "Fast_model" {
		t.Errorf("Description = %q", entry.Description)
	}
	if entry.Created != 1710000000 {
		t.Errorf("Created = %d", entry.Created)
	}
}

// TestParseModelTableEntry_Costs verifies that non-negative decimal costs (including 0 and 0.0)
// are accepted, while negative values, NaN, and Inf are rejected.
func TestParseModelTableEntry_Costs(t *testing.T) {
	entry, err := parseModelTableEntry("s@p=w;cost_input=0;cost_output=0.0")
	if err != nil {
		t.Fatalf("expected cost_input=0;cost_output=0.0 to succeed, got: %v", err)
	}
	if entry.CostInput == nil || *entry.CostInput != 0.0 {
		t.Errorf("CostInput = %v, want 0.0", entry.CostInput)
	}
	if entry.CostOutput == nil || *entry.CostOutput != 0.0 {
		t.Errorf("CostOutput = %v, want 0.0", entry.CostOutput)
	}

	badCases := []struct {
		raw  string
		want string
	}{
		{"s@p=w;cost_input=-1", `invalid -model-table "s@p=w;cost_input=-1": invalid cost_input "-1": want non-negative decimal`},
		{"s@p=w;cost_input=-0", `invalid -model-table "s@p=w;cost_input=-0": invalid cost_input "-0": want non-negative decimal`},
		{"s@p=w;cost_input=-0.0", `invalid -model-table "s@p=w;cost_input=-0.0": invalid cost_input "-0.0": want non-negative decimal`},
		{"s@p=w;cost_input=-0.01", `invalid -model-table "s@p=w;cost_input=-0.01": invalid cost_input "-0.01": want non-negative decimal`},
		{"s@p=w;cost_input=NaN", `invalid -model-table "s@p=w;cost_input=NaN": invalid cost_input "NaN": want non-negative decimal`},
		{"s@p=w;cost_input=+Inf", `invalid -model-table "s@p=w;cost_input=+Inf": invalid cost_input "+Inf": want non-negative decimal`},
		{"s@p=w;cost_input=-Inf", `invalid -model-table "s@p=w;cost_input=-Inf": invalid cost_input "-Inf": want non-negative decimal`},
		{"s@p=w;cost_input=Inf", `invalid -model-table "s@p=w;cost_input=Inf": invalid cost_input "Inf": want non-negative decimal`},
		{"s@p=w;cost_output=-1", `invalid -model-table "s@p=w;cost_output=-1": invalid cost_output "-1": want non-negative decimal`},
		{"s@p=w;cost_output=NaN", `invalid -model-table "s@p=w;cost_output=NaN": invalid cost_output "NaN": want non-negative decimal`},
		{"s@p=w;cost_output=+Inf", `invalid -model-table "s@p=w;cost_output=+Inf": invalid cost_output "+Inf": want non-negative decimal`},
		{"s@p=w;cost_input=abc", `invalid -model-table "s@p=w;cost_input=abc": invalid cost_input "abc": want non-negative decimal`},
		{"s@p=w;cost_input=", `invalid -model-table "s@p=w;cost_input=": invalid cost_input "": want non-negative decimal`},
	}
	for _, tc := range badCases {
		assertModelTableParseError(t, tc.raw, tc.want)
	}
}

// TestParseModelTableEntry_DuplicateFact rejects a repeated fact key.
func TestParseModelTableEntry_DuplicateFact(t *testing.T) {
	assertModelTableParseError(t, "s@p=w;context=1;context=2",
		`invalid -model-table "s@p=w;context=1;context=2": duplicate fact "context"`)
}

// TestParseModelTableEntry_BadContext rejects every malformed positive-integer
// fact value.
func TestParseModelTableEntry_BadContext(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"s@p=w;context=0", `invalid -model-table "s@p=w;context=0": invalid context "0": want 1-999999999`},
		{"s@p=w;context=-1", `invalid -model-table "s@p=w;context=-1": invalid context "-1": want 1-999999999`},
		{"s@p=w;context=abc", `invalid -model-table "s@p=w;context=abc": invalid context "abc": want 1-999999999`},
		{"s@p=w;context=1.5", `invalid -model-table "s@p=w;context=1.5": invalid context "1.5": want 1-999999999`},
		{"s@p=w;context=", `invalid -model-table "s@p=w;context=": invalid context "": want 1-999999999`},
		{"s@p=w;max_output=1000000000", `invalid -model-table "s@p=w;max_output=1000000000": invalid max_output "1000000000": want 1-999999999`},
	}
	for _, tc := range cases {
		assertModelTableParseError(t, tc.raw, tc.want)
	}
}

// TestParseModelTableEntry_EffortsVocabulary pins the closed effort
// vocabulary: xhigh and max are accepted (real Responses clients dispatch
// them), while values outside the set or in the wrong case are rejected.
func TestParseModelTableEntry_EffortsVocabulary(t *testing.T) {
	entry, err := parseModelTableEntry("s@p=w;efforts=minimal+low+medium+high+xhigh+max")
	if err != nil {
		t.Fatalf("parseModelTableEntry returned error: %v", err)
	}
	if want := []string{"minimal", "low", "medium", "high", "xhigh", "max"}; !reflect.DeepEqual(entry.Efforts, want) {
		t.Fatalf("Efforts = %v, want %v", entry.Efforts, want)
	}

	assertModelTableParseError(t, "s@p=w;efforts=ultra",
		`invalid -model-table "s@p=w;efforts=ultra": unknown effort "ultra" (want minimal, low, medium, high, xhigh, max)`)
	assertModelTableParseError(t, "s@p=w;efforts=Low",
		`invalid -model-table "s@p=w;efforts=Low": unknown effort "Low" (want minimal, low, medium, high, xhigh, max)`)
	assertModelTableParseError(t, "s@p=w;modalities=video",
		`invalid -model-table "s@p=w;modalities=video": unknown modality "video" (want text, image, audio)`)
}

// TestParseModelTableEntry_BareFlagWithValue rejects a value attached to a
// bare flag fact.
func TestParseModelTableEntry_BareFlagWithValue(t *testing.T) {
	assertModelTableParseError(t, "s@p=w;default=true",
		`invalid -model-table "s@p=w;default=true": fact "default" takes no value`)
	assertModelTableParseError(t, "s@p=w;deprecated=1",
		`invalid -model-table "s@p=w;deprecated=1": fact "deprecated" takes no value`)
}

// TestParseModelTableEntry_DefaultDeprecatedConflict rejects the
// self-contradictory combination.
func TestParseModelTableEntry_DefaultDeprecatedConflict(t *testing.T) {
	assertModelTableParseError(t, "s@p=w;default;deprecated",
		`invalid -model-table "s@p=w;default;deprecated": default and deprecated cannot combine`)
}

// TestParseModelTableEntry_FactsDoNotAffectProjection proves facts are
// presentation-only: they never reach the projected model map.
func TestParseModelTableEntry_FactsDoNotAffectProjection(t *testing.T) {
	bare, err := parseModelTableEntry("a@p=w")
	if err != nil {
		t.Fatal(err)
	}
	full, err := parseModelTableEntry("a@p=w;context=1;default")
	if err != nil {
		t.Fatal(err)
	}
	bareMap := modelMapFromTable([]modelTableEntry{bare})
	fullMap := modelMapFromTable([]modelTableEntry{full})
	if !reflect.DeepEqual(bareMap.Exact, fullMap.Exact) {
		t.Fatalf("projection differs: %+v vs %+v", bareMap.Exact, fullMap.Exact)
	}
	want := map[string]struct{}{"a": {}}
	if _, ok := bareMap.Exact["a"]; !ok || len(bareMap.Exact) != len(want) {
		t.Fatalf("Exact = %+v", bareMap.Exact)
	}
}

// modelTableParseErr returns the parse error text for raw.
func modelTableParseErr(t *testing.T, raw string) string {
	t.Helper()
	_, err := parseModelTableEntry(raw)
	if err == nil {
		t.Fatalf("parseModelTableEntry(%q): want error, got nil", raw)
	}
	return err.Error()
}

// assertModelTableParseError asserts the exact error text for raw.
func assertModelTableParseError(t *testing.T, raw, want string) {
	t.Helper()
	if got := modelTableParseErr(t, raw); got != want {
		t.Errorf("parseModelTableEntry(%q) error = %q, want %q", raw, got, want)
	}
}
