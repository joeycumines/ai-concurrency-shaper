package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func messagesInlineToolRequest(role, definition, blockSuffix string) []byte {
	return []byte(fmt.Sprintf(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":%q,"content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":%s}%s}]}]}`, role, definition, blockSuffix))
}

func TestMessagesInlineToolHintsAreNoted(t *testing.T) {
	for _, tc := range []struct {
		name, field, notePath string
	}{
		{"cache", `"cache_control":{"type":"ephemeral"}`, "cache_control"},
		{"defer", `"defer_loading":true`, "tools[].defer_loading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := `{"name":"db","input_schema":{"type":"object"},` + tc.field + `}`
			declaredBody := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[` + definition + `]}`)
			declared, err := DecodeMessagesRequest(declaredBody, StrictLossPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if !reportHasAt(declared.Report, FeatureAnthropicControls, tc.notePath) {
				t.Fatalf("declared definition did not produce expected baseline note: %+v", declared.Report)
			}
			inline, err := DecodeMessagesRequest(messagesInlineToolRequest("system", definition, ""), StrictLossPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if len(inline.Request.Tools) != 1 || inline.Request.Tools[0].Name != "db" {
				t.Fatal("tool lost")
			}
			if !reportHasAt(inline.Report, FeatureAnthropicControls, tc.notePath) {
				t.Fatalf("inline definition silently discarded %s; report=%+v", tc.field, inline.Report.Losses)
			}
		})
	}
}

func TestMessagesToolChangesRequireSystemRole(t *testing.T) {
	for _, role := range []string{"user", "assistant"} {
		t.Run(role, func(t *testing.T) {
			result, err := DecodeMessagesRequest(messagesInlineToolRequest(role, `{"name":"db","input_schema":{"type":"object"}}`, ""), StrictLossPolicy())
			if err != nil {
				requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
				return
			}
			rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
			if err != nil {
				t.Fatalf("accepted malformed role but failed later: %v", err)
			}
			t.Fatalf("a tool change in a %s message reached the upstream tool list and erased its turn: %s", role, rendered)
		})
	}
}

func TestMessagesInlineFunctionRequiresInputSchema(t *testing.T) {
	definition := `{"type":"function","name":"db"}`
	declaredBody := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[` + definition + `]}`)
	if _, err := DecodeMessagesRequest(declaredBody, StrictLossPolicy()); err == nil {
		t.Fatal("declared baseline unexpectedly accepts missing schema")
	}
	result, err := DecodeMessagesRequest(messagesInlineToolRequest("system", definition, ""), StrictLossPolicy())
	if err != nil {
		if _, ok := errors.AsType[*wire.DecodeError](err); !ok {
			t.Fatalf("schema error is not typed: %T: %v", err, err)
		}
		return
	}
	t.Fatalf("the same invalid tool is accepted inline and receives an invented input schema: %+v", result.Request.Tools)
}

func TestMessagesInlineCustomTypeDecodes(t *testing.T) {
	definition := `{"type":"custom","name":"db","input_schema":{"type":"object"}}`
	result, err := DecodeMessagesRequest(messagesInlineToolRequest("system", definition, ""), StrictLossPolicy())
	if err != nil {
		t.Fatalf("official custom-tool type is rejected as a server tool: %v", err)
	}
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "db" {
		t.Fatal("custom tool was lost")
	}
}

func TestMessagesInlineCacheControlExclusions(t *testing.T) {
	for _, tc := range []struct{ name, definition, suffix string }{
		{"both locations", `{"name":"db","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}`, `,"cache_control":{"type":"ephemeral"}`},
		{"deferred definition", `{"name":"db","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"},"defer_loading":true}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := DecodeMessagesRequest(messagesInlineToolRequest("system", tc.definition, tc.suffix), StrictLossPolicy())
			if err != nil {
				requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
			}
			if err == nil {
				t.Fatalf("accepted prohibited inline-cache combination %q: %+v", tc.name, result.Report.Losses)
			}
		})
	}
}

func TestMessagesToolRedefinitionReachesRuntimeLog(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"tools":[{"name":"db","input_schema":{"type":"object","properties":{"old":{"type":"string"}}}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":{"type":"object","properties":{"new":{"type":"string"}}}}}}]}]}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	var handler TranscodeHandler
	handler.logConversionReport(result.Report, httptest.NewRequest("POST", "/v1/messages", nil), "request")
	if !strings.Contains(buf.String(), "redefined") {
		t.Fatalf("new schema took effect but the runtime log contains no supersession: %s; raw report=%+v", buf.String(), result.Report.Losses)
	}
}

func requireMessagesToolContractError(t *testing.T, err error, kind wire.DecodeErrorKind) {
	t.Helper()
	var malformed *wire.DecodeError
	if !errors.As(err, &malformed) || malformed.Kind != kind {
		t.Fatalf("error = %T: %v, want a %s client-contract error", err, err, kind)
	}
}

func messagesToolStateApproval() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicToolChanges: {},
		FeatureAnthropicServerTools: {},
		FeatureAnthropicControls:    {},
	}}
}

func TestMessagesToolChangesPreserveSourceCoordinates(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"y","input_schema":{"type":"object"}}}},{"type":"tool_removal","tool":{"type":"tool_reference","name":"x"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"continue"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"y"}}]}]}`)
	result, err := DecodeMessagesRequest(body, messagesToolStateApproval())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range result.Report.Losses {
		if entry.Feature == FeatureAnthropicToolChanges && entry.Kind == LossRecord {
			paths = append(paths, entry.Path)
		}
	}
	if fmt.Sprint(paths) != "[messages[1].content[1] messages[4].content[0]]" {
		t.Fatalf("withdrawal decisions lost source coordinates: %v", paths)
	}
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	var handler TranscodeHandler
	handler.logConversionReport(result.Report, httptest.NewRequest("POST", "/v1/messages", nil), "request")
	for _, path := range paths {
		if !strings.Contains(buf.String(), string(FeatureAnthropicToolChanges)+" at "+path) {
			t.Fatalf("runtime log lost a distinct source decision at %s: %s", path, buf.String())
		}
	}
}

func TestMessagesInlineToolRejectsSourceKindConflictBeforeLoss(t *testing.T) {
	for _, declarations := range []string{
		`"tools":[{"type":"web_search_20250305","name":"web_search"}],`,
		``,
	} {
		messages := `{"role":"user","content":"hi"},`
		if declarations == "" {
			messages += `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"web_search_20250305","name":"web_search"}}}]},`
		}
		messages += `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"web_search","input_schema":{"type":"object"}}}}]}`
		body := []byte(`{"model":"m","max_tokens":16,` + declarations + `"messages":[` + messages + `]}`)
		for _, policy := range []LossPolicy{StrictLossPolicy(), messagesToolStateApproval()} {
			_, err := DecodeMessagesRequest(body, policy)
			requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
			if !strings.Contains(err.Error(), "tool_name_conflict") {
				t.Fatalf("wrong source error: %v", err)
			}
		}
	}
}

func TestMessagesDuplicateDeclarationCitesOriginalPath(t *testing.T) {
	// tools[] declares "db" twice; a later inline provider-type definition
	// conflicts. The diagnostic must point at the first declaration.
	def := func(kind string) string {
		return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":` + kind + `}}`
	}
	server := `{"type":"web_search_20250305","name":"db"}`
	body := []byte(`{"model":"m","max_tokens":16,` +
		`"tools":[{"name":"db","input_schema":{"type":"object"}},{"name":"db","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[` + def(server) + `]}]}`)
	_, err := DecodeMessagesRequest(body, messagesToolStateApproval())
	requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
	if !strings.Contains(err.Error(), `but was declared as "custom" at tools[0] (tool_name_conflict)`) {
		t.Fatalf("duplicate definition cited the wrong origin: %v", err)
	}

	// Two versions of the same provider tool family share a normalized kind;
	// the conflict still cites the FIRST declaration's type and path together.
	computer := `{"type":"computer_20260101","name":"db"}`
	body = []byte(`{"model":"m","max_tokens":16,` +
		`"tools":[{"type":"web_search_20250305","name":"db"},{"type":"web_search_20260101","name":"db"}],` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[` + def(computer) + `]}]}`)
	_, err = DecodeMessagesRequest(body, messagesToolStateApproval())
	requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
	if !strings.Contains(err.Error(), `but was declared as "web_search_20250305" at tools[0] (tool_name_conflict)`) {
		t.Fatalf("same-kind duplicates cited a mixed origin: %v", err)
	}
}

func TestMessagesToolChangePlacement(t *testing.T) {
	change := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":{"type":"object"}}}}]}`
	user := `{"role":"user","content":"hi"}`
	assistant := `{"role":"assistant","content":"ok"}`
	textSystem := `{"role":"system","content":"instructions"}`
	for _, tc := range []struct {
		name, messages string
		valid          bool
	}{
		{"final group after user", user + `,` + change, true},
		{"group followed by assistant", user + `,` + change + `,` + assistant, true},
		{"adjacent system messages form one group", user + `,` + textSystem + `,` + change + `,` + textSystem + `,` + assistant, true},
		{"first message", change, false},
		{"after assistant text", user + `,` + assistant + `,` + change, false},
		{"before user", user + `,` + change + `,` + user, false},
		{"text system cannot hide missing predecessor", textSystem + `,` + change, false},
		{"text system cannot hide invalid successor", user + `,` + change + `,` + textSystem + `,` + user, false},
		{"paused web search result", user + `,{"role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]},` + change, false},
		{"paused code execution result", user + `,{"role":"assistant","content":[{"type":"code_execution_tool_result","tool_use_id":"s","content":[]}]},` + change, false},
		{"paused MCP result", user + `,{"role":"assistant","content":[{"type":"mcp_tool_result","tool_use_id":"s","content":"done"}]},` + change, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","max_tokens":16,"messages":[` + tc.messages + `]}`)
			result, err := DecodeMessagesRequest(body, messagesToolStateApproval())
			if !tc.valid {
				requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateCanonicalRequest(result.Request); err != nil {
				t.Fatal(err)
			}
			if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "db" {
				t.Fatalf("tool missing: %+v", result.Request.Tools)
			}
		})
	}
}

func TestMessagesToolChangesRejectNonMessagePlacement(t *testing.T) {
	change := `{"type":"tool_removal","tool":{"type":"tool_reference","name":"db"}}`
	for _, body := range []string{
		`{"model":"m","max_tokens":16,"system":[` + change + `],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":[` + change + `]}]}]}`,
	} {
		_, err := DecodeMessagesRequest([]byte(body), messagesToolStateApproval())
		requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
	}
}

func TestMessagesInlineUnsupportedDefinitionsStayTyped(t *testing.T) {
	for _, definition := range []string{
		`{"type":"web_search_20250305","name":"web_search"}`,
		`{"type":"mcp_toolset","mcp_server_name":"calendar"}`,
	} {
		for _, policy := range []LossPolicy{StrictLossPolicy(), messagesToolStateApproval()} {
			_, err := DecodeMessagesRequest(messagesInlineToolRequest("system", definition, ""), policy)
			var unsupported *UnrepresentableError
			if !errors.As(err, &unsupported) || unsupported.Path != "messages[1].content[0].tool.definition" {
				t.Fatalf("unsupported definition error = %T: %v", err, err)
			}
		}
	}
}

func TestMessagesServerToolVersionChangesKeepTheirSourceKind(t *testing.T) {
	for _, tc := range []struct {
		name, initial, latest string
		sameKind              bool
	}{
		{"web search version", "web_search_20250305", "web_search_20260209", true},
		{"text editor version", "text_editor_20250124", "text_editor_20250728", true},
		{"tool search variants", "tool_search_tool_regex_20251119", "tool_search_tool_bm25_20251119", false},
		{"unknown discriminants", "unmodeled_20250101", "unmodeled_20260101", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"m","max_tokens":16,"tools":[{"type":%q,"name":"tool"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":%q,"name":"tool"}}}]}]}`, tc.initial, tc.latest))
			_, err := DecodeMessagesRequest(body, messagesToolStateApproval())
			if !tc.sameKind {
				requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
				return
			}
			// A legal server-version upgrade still lacks a portable target
			// representation. It must fail for that reason, not as corrupt input.
			if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
				t.Fatalf("valid version update was misclassified: %T: %v", err, err)
			}
		})
	}
}

func TestMessagesToolReferencesResolveAtTheirSourcePosition(t *testing.T) {
	definition := `{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":{"type":"object"}}}}`
	remove := `{"type":"tool_removal","tool":{"type":"tool_reference","name":"db"}}`
	add := `{"type":"tool_addition","tool":{"type":"tool_reference","name":"db"}}`
	for _, content := range []string{remove, add, remove + `,` + definition, add + `,` + definition} {
		body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[` + content + `]}]}`)
		for _, policy := range []LossPolicy{StrictLossPolicy(), messagesToolStateApproval()} {
			_, err := DecodeMessagesRequest(body, policy)
			requireMessagesToolContractError(t, err, wire.DecodeMalformed)
			if !strings.Contains(err.Error(), "tool_reference_unresolved") {
				t.Fatalf("wrong reference rejection: %v", err)
			}
		}
	}
	// Removing availability does not erase the declaration: a later reference
	// may restore it, including one introduced by an earlier inline definition.
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[` + definition + `,` + remove + `,` + add + `]}]}`)
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	var denied *UnsupportedFeatureError
	if !errors.As(err, &denied) || denied.Feature != string(FeatureAnthropicToolChanges) || denied.Path != "messages[1].content[1]" {
		t.Fatalf("valid withdrawal should require its own approval: %v", err)
	}
	result, err := DecodeMessagesRequest(body, messagesToolStateApproval())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "db" {
		t.Fatalf("declaration lost: %+v", result.Request.Tools)
	}
	var paths []string
	for _, entry := range result.Report.Losses {
		if entry.Feature == FeatureAnthropicToolChanges {
			paths = append(paths, entry.Path)
		}
	}
	if fmt.Sprint(paths) != "[messages[1].content[0] messages[1].content[1] messages[1].content[2]]" {
		t.Fatalf("wrong definition/reference paths: %v", paths)
	}
}

func TestMessagesReferencesKeepDeclarationsBeforeApprovedServerDrop(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"web_search"}}]}]}`)
	result, err := DecodeMessagesRequest(body, messagesToolStateApproval())
	if err != nil {
		t.Fatalf("an approved drop erased source declaration evidence: %v", err)
	}
	if len(result.Request.Tools) != 0 || !reportHasAt(result.Report, FeatureAnthropicServerTools, "tools[0]") || !reportHasAt(result.Report, FeatureAnthropicToolChanges, "messages[1].content[0]") {
		t.Fatalf("wrong server/reference decisions: tools=%+v report=%+v", result.Request.Tools, result.Report.Losses)
	}
}

func TestMessagesMCPReferencesRequireObservableLoss(t *testing.T) {
	for _, reference := range []string{
		`{"type":"mcp_tool_reference","server_name":"calendar","name":"list_events"}`,
		`{"type":"mcp_toolset_reference","server_name":"calendar"}`,
	} {
		body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":` + reference + `}]}]}`)
		_, err := DecodeMessagesRequest(body, StrictLossPolicy())
		var denied *UnsupportedFeatureError
		if !errors.As(err, &denied) || denied.Feature != string(FeatureAnthropicToolChanges) {
			t.Fatalf("MCP reference bypassed policy: %v", err)
		}
		result, err := DecodeMessagesRequest(body, messagesToolStateApproval())
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Request.Tools) != 0 || len(result.Request.Turns) != 1 {
			t.Fatalf("MCP reference invented tool/content: %+v", result.Request)
		}
		if !reportHasAt(result.Report, FeatureAnthropicToolChanges, "messages[1].content[0]") {
			t.Fatalf("MCP reference loss missing: %+v", result.Report.Losses)
		}
	}
}

func TestMessagesInlineDefinitionEqualityPreservesSchemaNumbers(t *testing.T) {
	initial := `{"type":"object","properties":{"n":{"const":9007199254740992,"type":"integer"}}}`
	same := `{ "properties": {"n":{"type":"integer","const":9007199254740992}}, "type":"object" }`
	changed := `{"type":"object","properties":{"n":{"const":9007199254740993,"type":"integer"}}}`
	for _, tc := range []struct{ name, schema, note, number string }{
		{"reordered object", same, "repeats", "9007199254740992"},
		{"adjacent large integer", changed, "redefined", "9007199254740993"},
		{"large exponent", `{"type":"object","properties":{"n":{"const":1e10000}}}`, "redefined", "1e10000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","max_tokens":16,"tools":[{"type":"custom","name":"db","input_schema":` + initial + `}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"function","name":"db","input_schema":` + tc.schema + `}}}]}]}`)
			result, err := DecodeMessagesRequest(body, StrictLossPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Request.Tools) != 1 || !bytes.Equal(result.Request.Tools[0].JSONSchema, []byte(tc.schema)) {
				t.Fatalf("definition/schema bytes changed: %+v", result.Request.Tools)
			}
			count := 0
			for _, entry := range result.Report.Losses {
				if entry.Feature == FeatureAnthropicToolChanges {
					count++
					if !strings.Contains(entry.Detail, tc.note) {
						t.Fatalf("note = %q, want %q", entry.Detail, tc.note)
					}
				}
			}
			if count != 1 {
				t.Fatalf("got %d decisions for one definition", count)
			}
			rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(rendered, []byte(tc.number)) {
				t.Fatalf("numeric literal changed on target wire: %s", rendered)
			}
		})
	}
}

func TestMessagesDuplicateDeclaredToolsKeepExistingDisposition(t *testing.T) {
	tools := `"tools":[{"name":"db","input_schema":{"type":"object"}},{"name":"db","input_schema":{"type":"object"}}],`
	base := `{"model":"m","max_tokens":16,` + tools + `"messages":[{"role":"user","content":"hi"}`
	result, err := DecodeMessagesRequest([]byte(base+`]}`), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 2 {
		t.Fatalf("unrelated duplicate-name behavior changed: %+v", result.Request.Tools)
	}
	_, err = DecodeMessagesRequest([]byte(base+`,{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":{"type":"object"}}}}]}]}`), StrictLossPolicy())
	requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("wrong duplicate replacement error: %v", err)
	}
}

func TestMessagesMCPListingReplayIsPolicyGatedAndPreservesConversation(t *testing.T) {
	listing := `{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[]}`
	for _, tc := range []struct {
		name, content string
		roles         []CanonicalRole
		texts         string
	}{
		{"listing only", listing, []CanonicalRole{CanonicalUser, CanonicalUser}, "before,after"},
		{"listing within text", `{"type":"text","text":"alpha"},` + listing + `,{"type":"text","text":"omega"}`, []CanonicalRole{CanonicalUser, CanonicalAssistant, CanonicalUser}, "before,alpha,omega,after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"before"},{"role":"assistant","content":[` + tc.content + `]},{"role":"user","content":"after"}]}`)
			_, err := DecodeMessagesRequest(body, StrictLossPolicy())
			var denied *UnsupportedFeatureError
			if !errors.As(err, &denied) || denied.Feature != string(FeatureAnthropicToolChanges) {
				t.Fatalf("strict replay should require tool-state approval: %v", err)
			}
			result, err := DecodeMessagesRequest(body, messagesToolStateApproval())
			if err != nil {
				t.Fatal(err)
			}
			requireTurnSequence(t, result.Request.Turns, tc.roles...)
			var texts []string
			for _, turn := range result.Request.Turns {
				for _, part := range turn.Parts {
					if text, ok := part.(CanonicalText); ok {
						texts = append(texts, text.Text)
					}
				}
			}
			if strings.Join(texts, ",") != tc.texts {
				t.Fatalf("conversation changed: %v", texts)
			}
			count := 0
			for _, entry := range result.Report.Losses {
				if entry.Feature == FeatureAnthropicToolChanges {
					count++
					if entry.Kind != LossRecord || !strings.HasPrefix(entry.Path, "messages[1].content[") {
						t.Fatalf("wrong replay decision: %+v", entry)
					}
				}
			}
			if count != 1 {
				t.Fatalf("listing decisions=%d", count)
			}
			rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(rendered, []byte("mcp_tool_listing")) {
				t.Fatalf("provider state leaked: %s", rendered)
			}
		})
	}
}

func TestMessagesMCPListingReplayRequiresTopLevelAssistant(t *testing.T) {
	listing := `{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[]}`
	for _, role := range []string{"user", "system"} {
		body := []byte(fmt.Sprintf(`{"model":"m","max_tokens":16,"messages":[{"role":%q,"content":[%s]}]}`, role, listing))
		_, err := DecodeMessagesRequest(body, messagesToolStateApproval())
		requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
	}
	for _, body := range []string{
		`{"model":"m","max_tokens":16,"system":[` + listing + `],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[` + listing + `]}]}]}`,
	} {
		_, err := DecodeMessagesRequest([]byte(body), messagesToolStateApproval())
		requireMessagesToolContractError(t, err, wire.DecodeContradictoryUnion)
	}
}

func TestHandlerRejectsMalformedToolChangesBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"wrong role", messagesInlineToolRequest("assistant", `{"name":"db","input_schema":{"type":"object"}}`, "")},
		{"missing schema", messagesInlineToolRequest("system", `{"type":"function","name":"db"}`, "")},
		{"both cache locations", messagesInlineToolRequest("system", `{"name":"db","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}`, `,"cache_control":{"type":"ephemeral"}`)},
		{"deferred cache", messagesInlineToolRequest("system", `{"name":"db","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"},"defer_loading":true}`, "")},
		{"cross kind", []byte(`{"model":"m","max_tokens":16,"tools":[{"type":"web_search_20250305","name":"db"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":{"type":"object"}}}}]}]}`)},
		{"before user", []byte(`{"model":"m","max_tokens":16,"tools":[{"name":"db","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"db"}}]},{"role":"user","content":"go"}]}`)},
		{"unresolved reference", []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"missing"}}]}]}`)},
		{"forward reference", []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"db"}},{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"db","input_schema":{"type":"object"}}}}]}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapping := messagesMapping(t, UpstreamChatCompletions)
			mapping.LossPolicy = messagesToolStateApproval()
			called := false
			handler := testHandler(t, mapping, func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("unexpected upstream request")
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(tc.body)))
			if called {
				t.Fatal("malformed client request reached the upstream")
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Type != "error" || response.Error.Type != "invalid_request_error" {
				t.Fatalf("wrong client error envelope: %s", recorder.Body.String())
			}
		})
	}
}
