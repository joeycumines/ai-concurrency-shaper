package anthropicmessages

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// The official discriminator is custom; function remains a compatibility
// spelling. Neither spelling may bypass the strict client-function contract.
func TestClientToolDiscriminatorsAreStrict(t *testing.T) {
	for _, kind := range []string{"", "custom", "function"} {
		t.Run("type="+kind, func(t *testing.T) {
			typeField := ""
			if kind != "" {
				typeField = `"type":"` + kind + `",`
			}
			schema := `{"type":"object", "properties":{"n":{"const":9007199254740993},"e":{"const":1e-1000}}}`
			body := `{` + typeField + `"name":"f","description":"run f","input_schema":` + schema + `,"defer_loading":true}`
			var tool Tool
			if err := json.Unmarshal([]byte(body), &tool); err != nil {
				t.Fatal(err)
			}
			if !tool.IsClientFunction() || tool.Type != kind || tool.Name != "f" ||
				tool.Description == nil || *tool.Description != "run f" ||
				tool.DeferLoading == nil || !*tool.DeferLoading {
				t.Fatalf("client tool = %+v", tool)
			}
			if string(tool.InputSchema) != schema {
				t.Fatalf("schema bytes = %s, want %s", tool.InputSchema, schema)
			}
			encoded, err := json.Marshal(tool)
			if err != nil {
				t.Fatal(err)
			}
			var restored Tool
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatalf("round trip: %v: %s", err, encoded)
			}
			if !bytes.Contains(restored.InputSchema, []byte("9007199254740993")) ||
				!bytes.Contains(restored.InputSchema, []byte("1e-1000")) {
				t.Fatalf("numeric schema lexemes changed: %s", restored.InputSchema)
			}

			for _, field := range []string{`"unknown":1`, `"strict":true`, `"input_examples":[]`, `"allowed_callers":["direct"]`, `"mcp_server_name":"s"`} {
				var invalid Tool
				err := json.Unmarshal([]byte(`{`+typeField+`"name":"f","input_schema":{},`+field+`}`), &invalid)
				var typed *wire.DecodeError
				if !errors.As(err, &typed) || typed.Kind != wire.DecodeUnknownField {
					t.Fatalf("%s: error = %v, want unknown-field rejection", field, err)
				}
			}
			for _, shape := range []string{
				`"name":"f"`,
				`"name":"f","input_schema":null`,
				`"name":"f","input_schema":[]`,
				`"name":"f","input_schema":true`,
				`"name":"f","input_schema":"{}"`,
				`"input_schema":{}`,
			} {
				var invalid Tool
				if err := json.Unmarshal([]byte(`{`+typeField+shape+`}`), &invalid); err == nil {
					t.Fatalf("invalid client tool accepted: %s", shape)
				}
			}
		})
	}
}

func TestMCPToolsetUsesServerIdentity(t *testing.T) {
	const body = `{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}}`
	var tool Tool
	if err := json.Unmarshal([]byte(body), &tool); err != nil {
		t.Fatal(err)
	}
	if tool.IsClientFunction() || tool.MCPServerName != "calendar" || tool.Name != "" || string(tool.ServerParams) != body {
		t.Fatalf("MCP toolset identity or preserved parameters = %+v", tool)
	}
	for _, body := range []string{
		`{"type":"mcp_toolset"}`,
		`{"type":"mcp_toolset","mcp_server_name":null}`,
		`{"type":"mcp_toolset","mcp_server_name":""}`,
		`{"type":"mcp_toolset","mcp_server_name":"calendar","name":"wrong"}`,
	} {
		var invalid Tool
		if err := json.Unmarshal([]byte(body), &invalid); err == nil {
			t.Fatalf("invalid MCP toolset accepted: %s", body)
		}
	}
	// Existing provider-defined tools still enter their observable
	// unsupported-feature path without requiring a custom input schema.
	var server Tool
	if err := json.Unmarshal([]byte(`{"type":"web_search_20250305","name":"web_search","max_uses":2}`), &server); err != nil {
		t.Fatal(err)
	}
	if server.IsClientFunction() || len(server.ServerParams) == 0 {
		t.Fatalf("server tool misclassified: %+v", server)
	}
}

func TestToolRequiredFieldsHaveTypedErrors(t *testing.T) {
	for _, tool := range []Tool{
		{Type: "custom", InputSchema: json.RawMessage(`{}`)},
		{Type: "function", Name: "f"},
		{Name: "f"},
		{Type: "mcp_toolset"},
	} {
		var typed *wire.DecodeError
		if err := tool.Validate(); !errors.As(err, &typed) || typed.Kind != wire.DecodeMissingRequired {
			t.Fatalf("%+v: error = %v, want missing-required classification", tool, err)
		}
	}
}

func TestToolChangeRoundTripsEveryArm(t *testing.T) {
	for _, body := range []string{
		`{"type":"tool_reference","name":"f"}`,
		`{"type":"mcp_tool_reference","server_name":"s","name":"f"}`,
		`{"type":"mcp_toolset_reference","server_name":"s"}`,
		`{"type":"tool_definition","definition":{"type":"custom","name":"f","input_schema":{"type":"object"}}}`,
	} {
		var change ToolChange
		if err := json.Unmarshal([]byte(body), &change); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		var restored ToolChange
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(change, restored) {
			t.Fatalf("round trip changed arm: %+v -> %+v", change, restored)
		}
	}
}

func TestToolChangeMarshalRejectsContradictoryArms(t *testing.T) {
	definition := json.RawMessage(`{"name":"f","input_schema":{}}`)
	for _, change := range []ToolChange{
		{Type: ToolChangeTypeReference, Name: "f", ServerName: "s"},
		{Type: ToolChangeTypeReference, Name: "f", Definition: definition},
		{Type: ToolChangeTypeMCPReference, Name: "f", ServerName: "s", Definition: definition},
		{Type: ToolChangeTypeMCPToolsetReference, ServerName: "s", Name: "f"},
		{Type: ToolChangeTypeMCPToolsetReference, ServerName: "s", Definition: definition},
		{Type: ToolChangeTypeDefinition, Definition: definition, Name: "f"},
		{Type: ToolChangeTypeDefinition, Definition: definition, ServerName: "s"},
	} {
		var typed *wire.DecodeError
		if err := change.Validate(); !errors.As(err, &typed) || typed.Kind != wire.DecodeContradictoryUnion {
			t.Fatalf("%+v: Validate error = %v, want contradictory union", change, err)
		}
		if _, err := json.Marshal(change); err == nil {
			t.Fatalf("Marshal silently discarded a cross-arm field: %+v", change)
		}
	}
	for _, change := range []ToolChange{
		{Type: ToolChangeTypeReference},
		{Type: ToolChangeTypeMCPReference, Name: "f"},
		{Type: ToolChangeTypeMCPToolsetReference},
		{Type: ToolChangeTypeDefinition, Definition: json.RawMessage(`[]`)},
		{Type: ToolChangeTypeDefinition, Definition: json.RawMessage(`{"a":1,"a":2}`)},
	} {
		if _, err := json.Marshal(change); err == nil {
			t.Fatalf("Marshal accepted malformed arm: %+v", change)
		}
	}
}

func TestMCPToolListingRoundTripUsesMCPServerName(t *testing.T) {
	const rawTools = `[{"name":"read","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}},"description":null}]`
	const body = `{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":` + rawTools + `}`
	var listing ContentBlock
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.MCPServerName == nil || *listing.MCPServerName != "calendar" || listing.ServerName != nil || string(listing.Tools) != rawTools {
		t.Fatalf("listing lost identity or raw tools: %+v", listing)
	}
	encoded, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"server_name"`) || !strings.Contains(string(encoded), `"mcp_server_name":"calendar"`) {
		t.Fatalf("listing uses the wrong server field: %s", encoded)
	}
	var replay ContentBlock
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatalf("marshaled listing cannot replay: %v: %s", err, encoded)
	}
	if !reflect.DeepEqual(listing, replay) {
		t.Fatalf("listing changed on replay: %+v -> %+v", listing, replay)
	}

	const useJSON = `{"type":"mcp_tool_use","server_name":"calendar","id":"call_1","name":"read","input":{}}`
	var use ContentBlock
	if err := json.Unmarshal([]byte(useJSON), &use); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(use)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"mcp_server_name"`) || !strings.Contains(string(encoded), `"server_name":"calendar"`) {
		t.Fatalf("MCP invocation's distinct spelling changed: %s", encoded)
	}
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatalf("MCP invocation no longer round trips: %v", err)
	}
}

func TestMCPToolListingRejectsMalformedTools(t *testing.T) {
	for _, tools := range []string{
		`null`, `{}`, `[null]`, `[1]`, `[{}]`,
		`[{"name":"f"}]`,
		`[{"name":"","input_schema":{}}]`,
		`[{"name":"f","input_schema":null}]`,
		`[{"name":"f","input_schema":[]}]`,
		`[{"name":"f","input_schema":{},"description":42}]`,
		`[{"name":"f","input_schema":{},"unexpected":true}]`,
	} {
		var listing ContentBlock
		body := `{"type":"mcp_tool_listing","mcp_server_name":"s","tools":` + tools + `}`
		if err := json.Unmarshal([]byte(body), &listing); err == nil {
			t.Fatalf("malformed listing accepted: %s", body)
		}
	}
	var empty ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"mcp_tool_listing","mcp_server_name":"s","tools":[]}`), &empty); err != nil {
		t.Fatalf("an empty server tool list is valid: %v", err)
	}
}
