package wire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeTolerantDiscardsAliasesInEveryMemberPosition(t *testing.T) {
	type document struct {
		Model string `json:"model"`
	}
	parts := []string{
		`"MODEL":null`,
		`"Model":{"ignored":1e10000}`,
		`"mOdEl":[null,{"x":"\\\""}]`,
		`"model":"canonical"`,
	}
	var permute func(int)
	permute = func(at int) {
		if at == len(parts) {
			body := "{\n\t" + strings.Join(parts, " , \n\t") + "\n}"
			var out document
			if err := DecodeTolerant([]byte(body), &out); err != nil || out.Model != "canonical" {
				t.Fatalf("member order changed exact-name decoding: %s; got %+v, error %v", body, out, err)
			}
			return
		}
		for i := at; i < len(parts); i++ {
			parts[at], parts[i] = parts[i], parts[at]
			permute(at + 1)
			parts[at], parts[i] = parts[i], parts[at]
		}
	}
	permute(0)
	for _, body := range []string{
		`{"MODEL":null}`,
		`{"MODEL":null,"Model":{},"mOdEl":[]}`,
		`{"MODEL":null,"unknown":true,"Model":{}}`,
		`{"unknown":true,"MODEL":null,"Model":{}}`,
	} {
		var out document
		if err := DecodeTolerant([]byte(body), &out); err != nil || out.Model != "" {
			t.Fatalf("only unknown fields must leave model absent: %s; got %+v, error %v", body, out, err)
		}
	}
}

func TestDecodeTolerantAliasFilteringPreservesRawSubtrees(t *testing.T) {
	type child struct {
		ID  string          `json:"id"`
		Raw json.RawMessage `json:"raw"`
	}
	type document struct {
		Model    string                     `json:"model"`
		Children []child                    `json:"children"`
		Raw      json.RawMessage            `json:"raw"`
		Map      map[string]json.RawMessage `json:"map"`
	}
	const raw = `{ "ID" : 1e10000, "id" : "\u003c&>" }`
	body := `{"MODEL":null,"children":[{"ID":null,"raw":` + raw + `,"id":"one","Id":{}},{"ID":null,"id":"two","raw":` + raw + `}],"Raw":null,"model":"m","raw":` + raw + `,"map":{"ID":1,"id":2,"Model":3,"model":4},"mOdEl":null}`
	var out document
	if err := DecodeTolerant([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "m" || len(out.Children) != 2 || out.Children[0].ID != "one" || out.Children[1].ID != "two" {
		t.Fatalf("nested exact fields changed: %+v", out)
	}
	for _, got := range []json.RawMessage{out.Raw, out.Children[0].Raw, out.Children[1].Raw} {
		if string(got) != raw {
			t.Fatalf("retained raw bytes changed: %s; want %s", got, raw)
		}
	}
	if len(out.Map) != 4 || string(out.Map["ID"]) != "1" || string(out.Map["id"]) != "2" || string(out.Map["Model"]) != "3" || string(out.Map["model"]) != "4" {
		t.Fatalf("map keys were treated as modeled names: %v", out.Map)
	}
}

func TestDecodeTolerantAliasFilteringRetainsMalformedChecks(t *testing.T) {
	type document struct {
		Model string `json:"model"`
	}
	for _, body := range []string{
		`{"MODEL":null,"model":null}`,
		`{"model":null,"MODEL":"ignored"}`,
		`{"MODEL":null,"model":17}`,
		`{"MODEL":null,"model":"a","model":"b"}`,
		`{"MODEL":{"x":1,"x":2},"model":"m"}`,
		`{"MODEL":null,"MODEL":null,"model":"m"}`,
		`{"MODEL":null,}`,
		`{"MODEL":null,,"model":"m"}`,
		`{"model":"m","MODEL":null,}`,
		`{"model":"m","MODEL":[1,]}`,
		`{"MODEL":null,"model":"m"} {"model":"n"}`,
	} {
		var out document
		if err := DecodeTolerant([]byte(body), &out); err == nil {
			t.Fatalf("discarding an alias concealed malformed source: %s", body)
		}
	}
	var out document
	err := DecodeTolerant([]byte(`{"MODEL":null,"model":null}`), &out)
	var malformed *DecodeError
	if !errors.As(err, &malformed) || malformed.Kind != DecodeIllegalNull {
		t.Fatalf("canonical illegal null lost its classification: %v", err)
	}
}

func TestDecodeTolerantMapValuesUseTheirDeclaredType(t *testing.T) {
	type value struct {
		Type string          `json:"type"`
		Raw  json.RawMessage `json:"raw"`
	}
	const raw = `{ "Type" : 1, "type" : 2 }`
	body := `{"One":{"TYPE":null,"type":"one","raw":` + raw + `},"one":{"TYPE":"ignored","raw":` + raw + `}}`
	var out map[string]value
	if err := DecodeTolerant([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out["One"].Type != "one" || out["one"].Type != "" {
		t.Fatalf("map keys or modeled values changed: %+v", out)
	}
	if string(out["One"].Raw) != raw || string(out["one"].Raw) != raw {
		t.Fatalf("raw map-value subtree changed: %+v", out)
	}
	if err := Decode([]byte(body), &out); err == nil {
		t.Fatal("strict decode accepted an unknown field inside a typed map value")
	}
}
