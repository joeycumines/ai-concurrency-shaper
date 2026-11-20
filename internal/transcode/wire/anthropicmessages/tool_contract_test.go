package anthropicmessages

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestClientToolSpellingsPreserveTheSameContract(t *testing.T) {
	const schema = `{"type":"object","properties":{"n":{"enum":[9007199254740993,1.5e100,0.0000001]}}}`
	for _, spelling := range []string{"", "custom", "function"} {
		name := spelling
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			prefix := ""
			if spelling != "" {
				prefix = fmt.Sprintf(`"type":%q,`, spelling)
			}
			body := []byte(`{` + prefix + `"name":"db","description":"Query the database",` +
				`"input_schema":` + schema + `,"cache_control":{"type":"ephemeral"},"defer_loading":false}`)
			var tool Tool
			if err := wire.Decode(body, &tool); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !tool.IsClientFunction() || len(tool.ServerParams) != 0 {
				t.Fatalf("client tool entered the server contract: %+v", tool)
			}
			if tool.Type != spelling || tool.Name != "db" || tool.Description == nil || *tool.Description != "Query the database" {
				t.Fatalf("identity or description changed: %+v", tool)
			}
			if string(tool.InputSchema) != schema {
				t.Fatalf("schema bytes changed: %s", tool.InputSchema)
			}
			if tool.CacheControl == nil || tool.DeferLoading == nil || *tool.DeferLoading {
				t.Fatalf("modeled controls lost presence or value: %+v", tool)
			}
			encoded, err := json.Marshal(tool)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var replay Tool
			if err := wire.Decode(encoded, &replay); err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if replay.Type != spelling || string(replay.InputSchema) != schema {
				t.Fatalf("type or numeric schema values changed: %s", encoded)
			}
		})
	}
}

func TestClientToolSpellingsRejectIncompleteOrUnsupportedFields(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		kind   wire.DecodeErrorKind
	}{
		{"missing schema", `"name":"db"`, wire.DecodeMissingRequired},
		{"null schema", `"name":"db","input_schema":null`, wire.DecodeIllegalNull},
		{"array schema", `"name":"db","input_schema":[]`, wire.DecodeMalformed},
		{"string schema", `"name":"db","input_schema":"object"`, wire.DecodeMalformed},
		{"boolean schema", `"name":"db","input_schema":true`, wire.DecodeMalformed},
		{"numeric schema", `"name":"db","input_schema":42`, wire.DecodeMalformed},
		{"server member", `"name":"db","input_schema":{},"max_uses":1`, wire.DecodeUnknownField},
		{"unknown member", `"name":"db","input_schema":{},"extra":1`, wire.DecodeUnknownField},
		{"unsupported examples", `"name":"db","input_schema":{},"input_examples":[{}]`, wire.DecodeUnknownField},
		{"unsupported strictness", `"name":"db","input_schema":{},"strict":true`, wire.DecodeUnknownField},
		{"unsupported callers", `"name":"db","input_schema":{},"allowed_callers":["direct"]`, wire.DecodeUnknownField},
		{"duplicate schema key", `"name":"db","input_schema":{"type":"object","type":"string"}`, wire.DecodeDuplicateKey},
	}
	for _, spelling := range []string{"", "custom", "function"} {
		prefix := ""
		if spelling != "" {
			prefix = fmt.Sprintf(`"type":%q,`, spelling)
		}
		for _, tc := range cases {
			t.Run(spelling+"/"+tc.name, func(t *testing.T) {
				var tool Tool
				err := wire.Decode([]byte(`{`+prefix+tc.fields+`}`), &tool)
				var typed *wire.DecodeError
				if !errors.As(err, &typed) || typed.Kind != tc.kind {
					t.Fatalf("error = %v, want wire.DecodeError/%s", err, tc.kind)
				}
			})
		}
	}
}

func TestClientToolNullTypeUsesTheStrictClientContract(t *testing.T) {
	var tool Tool
	if err := wire.Decode([]byte(`{"type":null,"name":"db","input_schema":{},"defer_loading":true}`), &tool); err != nil {
		t.Fatalf("the nullable custom-tool type was rejected: %v", err)
	}
	if !tool.IsClientFunction() || tool.Type != "" || tool.DeferLoading == nil || !*tool.DeferLoading || len(tool.ServerParams) != 0 {
		t.Fatalf("null type did not select the untagged client arm: %+v", tool)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["type"] != nil {
		t.Fatalf("null type should render as the equivalent absent type: %s", encoded)
	}
	var replay Tool
	if err := wire.Decode(encoded, &replay); err != nil || !replay.IsClientFunction() || replay.DeferLoading == nil || !*replay.DeferLoading {
		t.Fatalf("untagged round trip changed the client tool: %+v, %v", replay, err)
	}
	for _, tc := range []struct {
		body string
		kind wire.DecodeErrorKind
	}{
		{`{"type":null,"name":"db"}`, wire.DecodeMissingRequired},
		{`{"type":null,"name":"db","input_schema":null}`, wire.DecodeIllegalNull},
		{`{"type":null,"name":"db","input_schema":{},"max_uses":2}`, wire.DecodeUnknownField},
	} {
		err := wire.Decode([]byte(tc.body), &tool)
		var typed *wire.DecodeError
		if !errors.As(err, &typed) || typed.Kind != tc.kind {
			t.Fatalf("%s: error = %v, want wire.DecodeError/%s", tc.body, err, tc.kind)
		}
	}
}

func TestClientToolValidateRequiresAnObjectSchema(t *testing.T) {
	for _, spelling := range []string{"", "custom", "function"} {
		for _, schema := range []json.RawMessage{nil, {}, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`"object"`), json.RawMessage(`{} {}`)} {
			tool := Tool{Type: spelling, Name: "db", InputSchema: schema}
			if err := tool.Validate(); err == nil {
				t.Errorf("type %q accepted schema %q", spelling, schema)
			}
		}
	}
}

func TestServerToolClassificationRetainsOpaqueParameters(t *testing.T) {
	const body = `{"type":"web_search_20250305","name":"web_search","max_uses":3,"provider_extension":{"future":true}}`
	var tool Tool
	if err := wire.Decode([]byte(body), &tool); err != nil {
		t.Fatal(err)
	}
	if tool.IsClientFunction() || string(tool.ServerParams) != body {
		t.Fatalf("server definition was reclassified or lost its parameters: %+v", tool)
	}
	if err := tool.Validate(); err != nil {
		t.Fatalf("server tools do not require client input_schema: %v", err)
	}
}

func TestMCPServerNamesRoundTripOnTheirOwnArms(t *testing.T) {
	cases := []struct {
		name, body, requiredKey, forbiddenKey string
	}{
		{
			"listing", `{"type":"mcp_tool_listing","mcp_server_name":"s","tools":[{"name":"echo","input_schema":{"type":"object","maximum":9007199254740993},"description":"Echo"}]}`,
			"mcp_server_name", "server_name",
		},
		{
			"empty listing", `{"type":"mcp_tool_listing","mcp_server_name":"s","tools":[]}`,
			"mcp_server_name", "server_name",
		},
		{
			"listing with nullable description", `{"type":"mcp_tool_listing","mcp_server_name":"s","tools":[{"name":"echo","input_schema":{},"description":null}]}`,
			"mcp_server_name", "server_name",
		},
		{
			"tool use", `{"type":"mcp_tool_use","server_name":"s","id":"call_1","name":"echo","input":{"n":9007199254740993}}`,
			"server_name", "mcp_server_name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var block ContentBlock
			if err := wire.Decode([]byte(tc.body), &block); err != nil {
				t.Fatalf("decode: %v", err)
			}
			encoded, err := json.Marshal(block)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields[tc.requiredKey]) != `"s"` || fields[tc.forbiddenKey] != nil {
				t.Fatalf("wrong server field spelling: %s", encoded)
			}
			var replay ContentBlock
			if err := wire.Decode(encoded, &replay); err != nil {
				t.Fatalf("marshal did not produce a decodable block: %v", err)
			}
			if string(replay.Tools) != string(block.Tools) || string(replay.Input) != string(block.Input) {
				t.Fatalf("raw tool definitions or input changed: %s", encoded)
			}
		})
	}
}

func TestMCPToolListingRejectsCorruptModeledFields(t *testing.T) {
	for _, tools := range []string{
		`null`, `{}`, `[null]`, `[1]`, `[{}]`,
		`[{"name":null,"input_schema":{}}]`,
		`[{"name":7,"input_schema":{}}]`,
		`[{"name":"","input_schema":{}}]`,
		`[{"name":"echo"}]`,
		`[{"name":"echo","input_schema":null}]`,
		`[{"name":"echo","input_schema":[]}]`,
		`[{"name":"echo","input_schema":"object"}]`,
		`[{"name":"echo","input_schema":{},"description":false}]`,
		`[{"name":"echo","input_schema":{},"unknown":true}]`,
		`[{"name":"echo","name":"other","input_schema":{}}]`,
	} {
		t.Run(tools, func(t *testing.T) {
			body := `{"type":"mcp_tool_listing","mcp_server_name":"s","tools":` + tools + `}`
			var block ContentBlock
			if err := wire.Decode([]byte(body), &block); err == nil {
				t.Fatalf("malformed listing accepted: %s", body)
			}
		})
	}
	for _, body := range []string{
		`{"type":"mcp_tool_listing","tools":[]}`,
		`{"type":"mcp_tool_listing","mcp_server_name":null,"tools":[]}`,
		`{"type":"mcp_tool_listing","mcp_server_name":"","tools":[]}`,
		`{"type":"mcp_tool_listing","mcp_server_name":"s"}`,
		`{"type":"mcp_tool_listing","server_name":"s","tools":[]}`,
		`{"type":"mcp_tool_use","mcp_server_name":"s","id":"call_1","name":"echo","input":{}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var block ContentBlock
			if err := wire.Decode([]byte(body), &block); err == nil {
				t.Fatalf("missing or cross-arm member accepted: %s", body)
			}
		})
	}
}

func TestMCPToolListingRetainsRawSchemaNumbers(t *testing.T) {
	const tools = `[ {"name":"db","input_schema": { "enum":[9007199254740993,1.5e100,0.0000001] } } ]`
	var block ContentBlock
	if err := wire.Decode([]byte(`{"type":"mcp_tool_listing","mcp_server_name":"s","tools":`+tools+`}`), &block); err != nil {
		t.Fatal(err)
	}
	if string(block.Tools) != tools {
		t.Fatalf("validation rewrote listing bytes: %s", block.Tools)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"9007199254740993", "1.5e100", "0.0000001"} {
		if !strings.Contains(string(encoded), number) {
			t.Errorf("numeric spelling %q changed in %s", number, encoded)
		}
	}
}
