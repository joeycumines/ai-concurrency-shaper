package transcode

// Synthetic conformance cases for Anthropic thinking and inline tools. The
// fixtures follow the published contracts; they are not original CLI captures.
// https://platform.claude.com/docs/en/build-with-claude/thinking
// https://platform.claude.com/docs/en/build-with-claude/mid-conversation-system-messages

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// TestClaudeThinkingDisplayUpdatesDecodes checks the documented adaptive
// updates arm using a synthetic conformance fixture. The decoder carries the
// preference without loss; target renderers decide whether they can express it.
func TestClaudeThinkingDisplayUpdatesDecodes(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.ClaudeThinkingDisplayUpdatesJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("the conformance request was refused: %v", err)
	}
	thinking := result.Request.Thinking
	if thinking == nil || thinking.Type != "adaptive" {
		t.Fatalf("thinking = %+v, want the adaptive arm", thinking)
	}
	if thinking.Display != "updates" {
		t.Fatalf("thinking display = %q, want the beta member carried rather than destroyed", thinking.Display)
	}
	if thinking.BudgetTokens != nil {
		t.Fatalf("budget_tokens = %v, want absent on adaptive", thinking.BudgetTokens)
	}
	if len(result.Report.Losses) != 0 {
		t.Fatalf("the request carries no loss, got %+v", result.Report.Losses)
	}

	// A Messages target carries the whole union natively, so the upstream sees
	// exactly what the client asked for.
	rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	for _, want := range []string{
		`"type":"adaptive"`,
		`"display":"updates"`,
	} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("rendered thinking is missing %s: %s", want, rendered)
		}
	}

	// Every other target loses the display preference, and losing it is a
	// policy decision the operator can refuse — never a silent drop.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning: {},
	}}
	// The fixtures carry tools, and a Messages-sourced tool on a Responses
	// target is its own documented refusal (tool_schema_strictness). Drop them
	// so each sub-test decides the thinking arm and nothing else.
	withoutTools := result.Request
	withoutTools.Tools = nil
	withoutTools.ToolChoice = nil
	for name, render := range map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error){
		"chat": func(r CanonicalRequest, c *ExchangeContext) ([]byte, ConversionReport, error) {
			return RenderChatRequest(r, c, ChatCapabilities{})
		},
		"responses": RenderResponsesRequest,
		"gemini":    RenderGeminiRequest,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := render(withoutTools, testExchangeContext()); err == nil {
				t.Fatal("strict policy accepted losing the display preference; want a keyed refusal")
			} else if !strings.Contains(err.Error(), string(FeatureRequestReasoning)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureRequestReasoning)
			}
			_, report, err := render(withoutTools, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			if !reportHasAt(report, FeatureRequestReasoning, "thinking.display") {
				t.Fatalf("report = %+v, want the display decision at thinking.display", report.Losses)
			}
		})
	}
}

// TestClaudeThinkingBetweenToolsDecodes replays Claude Sonnet
// 5.5's lowest thinking setting: type between_tools, which takes no other
// field. The proxy must carry it rather than reject the arm, and no target may
// silently drop what it asserts.
func TestClaudeThinkingBetweenToolsDecodes(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.ClaudeThinkingBetweenToolsJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("the between_tools arm was refused: %v", err)
	}
	if thinking := result.Request.Thinking; thinking == nil ||
		thinking.Type != "between_tools" ||
		thinking.BudgetTokens != nil ||
		thinking.Display != "" {
		t.Fatalf("thinking = %+v, want a bare between_tools arm", result.Request.Thinking)
	}
	if len(result.Report.Losses) != 0 {
		t.Fatalf("the request carries no loss, got %+v", result.Report.Losses)
	}

	rendered, _, err := RenderMessagesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	if !strings.Contains(string(rendered), `"type":"between_tools"`) {
		t.Fatalf("between_tools is missing from the Messages wire: %s", rendered)
	}

	// The arm asserts no up-front thinking plus progress updates between tool
	// calls. No non-Messages dialect can express either half, so the loss is
	// policy-gated: strict refuses, an approving policy records it.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning: {},
	}}
	// The fixtures carry tools, and a Messages-sourced tool on a Responses
	// target is its own documented refusal (tool_schema_strictness). Drop them
	// so each sub-test decides the thinking arm and nothing else.
	withoutTools := result.Request
	withoutTools.Tools = nil
	withoutTools.ToolChoice = nil
	for name, render := range map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error){
		"chat": func(r CanonicalRequest, c *ExchangeContext) ([]byte, ConversionReport, error) {
			return RenderChatRequest(r, c, ChatCapabilities{})
		},
		"responses": RenderResponsesRequest,
		"gemini":    RenderGeminiRequest,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := render(withoutTools, testExchangeContext()); err == nil {
				t.Fatal("strict policy accepted losing between_tools; want a keyed refusal")
			} else if !strings.Contains(err.Error(), string(FeatureRequestReasoning)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureRequestReasoning)
			}
			_, report, err := render(withoutTools, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			if !reportHasFeature(report, FeatureRequestReasoning) {
				t.Fatalf("report = %+v, want a request_reasoning entry", report.Losses)
			}
		})
	}
}

// TestHandlerServesClaudeThinkingDisplayUpdates exercises the production
// handler with the synthetic adaptive-updates fixture and a scripted upstream.
// It proves request conversion, not live-provider or CLI interoperability.
func TestHandlerServesClaudeThinkingDisplayUpdates(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	// The fixture carries a thinking preference, tools and system blocks; every
	// loss the conversion legitimately takes here is approved so a refusal can
	// only come from the thinking union itself.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		// The adaptive arm maps to the provider default; its display preference
		// is a Messages-only control, so approve that request_reasoning loss.
		FeatureRequestReasoning:       {},
		FeatureReasoningSummary:       {},
		FeatureToolSchemaStrictness:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	var upstreamBody []byte
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	// The fixture asks for streaming; this test pins the request path, so
	// the JSON response fixture is paired with a non-streaming request.
	body := bytes.Replace(
		testcorpus.ClaudeThinkingDisplayUpdatesJSON(),
		[]byte(`"stream":true,`), nil, 1,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Anthropic-Beta", "thinking-display-updates-2026-08-18")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the fixture request was answered %d: %s", rec.Code, rec.Body.String())
	}
	// The display preference was decided during conversion; it must not leak
	// into the Chat request.
	if len(upstreamBody) == 0 {
		t.Fatal("the upstream saw no request")
	}
	if bytes.Contains(upstreamBody, []byte("updates")) {
		t.Fatalf("a Messages-only display value leaked into the chat request: %s", upstreamBody)
	}
}

// TestHandlerServesClaudeThinkingBetweenTools exercises the between_tools
// arm through the handler with a scripted upstream: the approved loss must
// allow the request through without treating the arm as an unknown type.
func TestHandlerServesClaudeThinkingBetweenTools(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestReasoning:       {},
		FeatureReasoningSummary:       {},
		FeatureToolSchemaStrictness:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	var upstreamBody []byte
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	body := bytes.Replace(
		testcorpus.ClaudeThinkingBetweenToolsJSON(),
		[]byte(`"stream":true,`), nil, 1,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the between_tools request was answered %d: %s", rec.Code, rec.Body.String())
	}
	if len(upstreamBody) == 0 {
		t.Fatal("the upstream saw no request")
	}
}

// TestClaudeToolAdditionDefinitionDecodes exercises a synthetic request
// ending in a system message that mixes text and an inline tool definition.
// The tool must become available
// (hoisted into the request's tools) with the availability-timing change
// recorded as exactly one Note — never an unknown-type rejection, and never a
// silent drop.
func TestClaudeToolAdditionDefinitionDecodes(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.ClaudeToolAdditionDefinitionJSON(),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("the tool_addition fixture was refused: %v", err)
	}
	var names []string
	for _, tool := range result.Request.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "Read,db_query" {
		t.Fatalf("tools = %v, want the declared Read plus the hoisted db_query", names)
	}
	// The function description and schema survive normalization.
	hoisted := result.Request.Tools[len(result.Request.Tools)-1]
	if hoisted.Description != "Run a read-only SQL query against the analytics database." {
		t.Fatalf("hoisted description = %q", hoisted.Description)
	}
	if !bytes.Contains(hoisted.JSONSchema, []byte(`"sql"`)) {
		t.Fatalf("hoisted schema lost its property: %s", hoisted.JSONSchema)
	}

	// Exactly one Note, under the tool-change key, naming the timing move.
	var notes []ConversionLoss
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicToolChanges {
			notes = append(notes, loss)
		}
		if loss.Kind != NoteRecord {
			t.Fatalf("hoisting a definition is a sanctioned encoding, got %v: %+v", loss.Kind, loss)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("tool-change notes = %d, want exactly one: %+v", len(notes), result.Report.Losses)
	}
	if !strings.Contains(notes[0].Detail, "db_query") {
		t.Fatalf("note does not name the hoisted tool: %q", notes[0].Detail)
	}

	// The fixture's tool_addition carries a block-level cache_control, which the
	// contract allows on the block or in the definition. It is a prompt-cache
	// performance hint with no chat representation, so it takes the existing
	// deduped cache_control note — it must not vanish unrecorded just because
	// the block type is new.
	cacheControlNotes := 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicControls && loss.Path == "cache_control" {
			cacheControlNotes++
		}
	}
	if cacheControlNotes != 1 {
		t.Fatalf("cache_control notes = %d, want exactly one for the block-level hint", cacheControlNotes)
	}

	// The decoder's own output must pass the central validator, and the Chat
	// target must render the hoisted tool. This fixture's system message sits
	// after a user turn (that is what mid-conversation means), so the render
	// needs the position loss approved — the same approval real traffic needs.
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("validator refused the decoder output: %v", err)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem: {},
		FeatureMultipleSystemTurns:   {},
	}}
	rendered, _, err := RenderChatRequest(result.Request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if !bytes.Contains(rendered, []byte(`"db_query"`)) {
		t.Fatalf("the hoisted tool is missing from the chat request: %s", rendered)
	}
}

// TestClaudeToolRemovalIsPolicyGated replays the withdrawal
// counterpart: a tool_removal naming a declared tool. A chat target carries one
// global tool list, so the timing change cannot be expressed — the strict policy
// must REFUSE it with the registered key and an approving policy must record the
// loss. It is never silently dropped.
func TestClaudeToolRemovalIsPolicyGated(t *testing.T) {
	body := testcorpus.ClaudeToolRemovalReferenceJSON()
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("strict policy accepted a tool_removal; want a keyed refusal")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("err = %T: %v, want UnsupportedFeatureError", err, err)
	}
	if target.Feature != string(FeatureAnthropicToolChanges) {
		t.Fatalf("feature = %q, want %q", target.Feature, FeatureAnthropicToolChanges)
	}

	result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicToolChanges: {},
	}})
	if err != nil {
		t.Fatalf("approved removal refused: %v", err)
	}
	var losses int
	for _, loss := range result.Report.Losses {
		if loss.Feature != FeatureAnthropicToolChanges {
			continue
		}
		losses++
		if loss.Kind != LossRecord {
			t.Fatalf("a withdrawal changes what the model can call, so it is a policy-gated loss: %+v", loss)
		}
	}
	if losses != 1 {
		t.Fatalf("tool-change losses = %d, want exactly one: %+v", losses, result.Report.Losses)
	}
	// The declared tool is still offered for the whole request: that is the
	// sanctioned encoding the loss records.
	if len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "get_weather" {
		t.Fatalf("declared tools = %+v, want get_weather still offered", result.Request.Tools)
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("validator refused the decoder output: %v", err)
	}
}

// TestClaudeToolChangeStrictnessHolds proves the widened union did
// not become blanket tolerance: an UNDOCUMENTED block type is still a typed
// rejection, and the tool-change arm still rejects a member from another arm, a
// nameless reference, and a tool_definition on a removal.
func TestClaudeToolChangeStrictnessHolds(t *testing.T) {
	envelope := func(block string) []byte {
		return []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"system","content":[` + block + `]}]}`)
	}
	cases := map[string]string{
		"undocumented block type": `{"type":"quantum_compute"}`,
		"unknown arm":             `{"type":"tool_addition","tool":{"type":"teleport","name":"x"}}`,
		"cross-arm member":        `{"type":"tool_addition","tool":{"type":"tool_reference","name":"x","server_name":"s"}}`,
		"nameless reference":      `{"type":"tool_removal","tool":{"type":"tool_reference"}}`,
		"definition on removal": `{"type":"tool_removal","tool":{"type":"tool_definition",` +
			`"definition":{"name":"x","input_schema":{"type":"object"}}}}`,
		"non-object definition": `{"type":"tool_addition","tool":{"type":"tool_definition","definition":[]}}`,
	}
	approve := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMessagesRequest(envelope(block), approve); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// TestHandlerServesClaudeToolAddition sends the synthetic inline-tool fixture
// through a real TranscodeHandler with a scripted upstream and checks its body.
func TestHandlerServesClaudeToolAddition(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	// Approve what this fixture legitimately takes: a mid-conversation
	// system message (Claude Code appends one per tool-change batch) and the
	// usual usage/reasoning defaults. The tool-change decision itself is a Note
	// here, so it needs no approval.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem:  {},
		FeatureMultipleSystemTurns:    {},
		FeatureRequestReasoning:       {},
		FeatureReasoningSummary:       {},
		FeatureToolSchemaStrictness:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	var upstreamBody []byte
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		upstreamBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	body := bytes.Replace(
		testcorpus.ClaudeToolAdditionDefinitionJSON(),
		[]byte(`"stream":true,`), nil, 1,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Anthropic-Beta", "inline-tools-2026-09-15")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the tool_addition request was answered %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"db_query"`, `"Read"`} {
		if !bytes.Contains(upstreamBody, []byte(want)) {
			t.Fatalf("upstream request is missing %s: %s", want, upstreamBody)
		}
	}
	if bytes.Contains(upstreamBody, []byte("tool_addition")) {
		t.Fatalf("a Messages-only block leaked into the chat request: %s", upstreamBody)
	}
}

// TestToolDefinitionUsesTheStrictClientContract proves a hoisted definition is
// decoded through the SAME strict tools[] path as a declared one, not a private
// probe. A private probe silently discarded every modelled tools[] member it did
// not happen to list — the client-contract hole the strict decode exists to
// close — and let a type-tagged server tool through as a plain chat function.
func TestToolDefinitionUsesTheStrictClientContract(t *testing.T) {
	envelope := func(definition string) []byte {
		return []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[{"type":"tool_addition","tool":` +
			`{"type":"tool_definition","definition":` + definition + `}}]}]}`)
	}
	// An unknown definition field is a client-dialect error, never a silent drop.
	_, err := DecodeMessagesRequest(envelope(
		`{"name":"db","input_schema":{"type":"object"},"totally_bogus":1}`,
	), StrictLossPolicy())
	if err == nil {
		t.Fatal("an unknown definition field was silently accepted")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %q, want an unknown-field rejection", err)
	}

	// A type-tagged server tool must not be smuggled past the server-tools gate.
	_, err = DecodeMessagesRequest(envelope(
		`{"type":"web_search_20250305","name":"ws"}`,
	), StrictLossPolicy())
	if err == nil {
		t.Fatal("a server-tool definition was hoisted past the server-tools gate")
	}
	var unsupported *UnrepresentableError
	if !errors.As(err, &unsupported) || unsupported.Protocol != "messages" ||
		unsupported.Path != "messages[1].content[0].tool.definition" {
		t.Fatalf("err = %T: %v, want a typed inline-tool refusal at its source definition", err, err)
	}
}

// TestToolDefinitionEquivalentNumbersAreNotSuperseded proves numerically equal
// schema numbers are the same definition even when their JSON lexemes differ.
// A false "redefined mid-conversation" report would misreport loss
// observability. Equal repeats are Note-only and therefore succeed even under
// StrictLossPolicy; only a genuine change is a policy-gated loss. The exponent
// branch of the comparator is exercised here (1 vs 1e0, 1000 vs 1e3, 0.001 vs
// 1e-3, 1.5 vs 15e-1, -1 vs -1E+0, 0 vs 0e5, 100 vs 1e2, 1 vs 10e-1), not
// just the plain decimal path (1 vs 1.0).
func TestToolDefinitionEquivalentNumbersAreNotSuperseded(t *testing.T) {
	def := func(minimum string) string {
		return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":` +
			`{"name":"db_query","input_schema":{"type":"object","minimum":` + minimum + `}}}}`
	}
	// The approved policy permits the supersession loss, so genuine changes
	// decode while equal repeats stay Note-only.
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
	equivalent := []string{"1.0", "1e0", "10e-1", "1E+0", "1.000"}
	for _, minimum := range equivalent {
		body := `{"model":"m","max_tokens":16,` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def("1") + `]},` +
			`{"role":"system","content":[` + def(minimum) + `]}]}`
		result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
		if err != nil {
			t.Fatalf("minimum %s (equal repeat) under strict policy: %v", minimum, err)
		}
		for _, loss := range result.Report.Losses {
			if strings.Contains(loss.Detail, "redefined") {
				t.Fatalf("1 vs %s reported as a supersession: %+v", minimum, loss)
			}
		}
	}
	// Distinct values that merely spell the same magnitude differently must
	// still differ: 0e5 is zero, but 1e-3 is not 1. Under the approved policy
	// the genuine change decodes and is reported; under strict it is refused.
	for _, pair := range [][2]string{{"0", "1e-3"}, {"1", "1e1"}, {"1000", "1e2"}, {"0.001", "1e-2"}} {
		body := `{"model":"m","max_tokens":16,` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def(pair[0]) + `]},` +
			`{"role":"system","content":[` + def(pair[1]) + `]}]}`
		result, err := DecodeMessagesRequest([]byte(body), policy)
		if err != nil {
			t.Fatalf("%s vs %s: %v", pair[0], pair[1], err)
		}
		// The combined Note plus the policy-gated Loss both carry "redefined".
		redefined := 0
		for _, loss := range result.Report.Losses {
			if strings.Contains(loss.Detail, "redefined") {
				redefined++
			}
		}
		if redefined != 2 {
			t.Fatalf("%s vs %s: got %d supersession reports, want 2 (Note + Loss): %+v", pair[0], pair[1], redefined, result.Report.Losses)
		}
		if _, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy()); err == nil {
			t.Fatalf("%s vs %s accepted under strict policy", pair[0], pair[1])
		}
	}
	// Mixed sign/exponent spellings of one value.
	body := `{"model":"m","max_tokens":16,` +
		`"messages":[{"role":"user","content":"x"},` +
		`{"role":"system","content":[` + def("-1") + `]},` +
		`{"role":"system","content":[` + def("-1E+0") + `]}]}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, loss := range result.Report.Losses {
		if strings.Contains(loss.Detail, "redefined") {
			t.Fatalf("-1 vs -1E+0 reported as a supersession: %+v", loss)
		}
	}
	// 0.001 vs 1e-3 and 1.5 vs 15e-1.
	for _, pair := range [][2]string{{"0.001", "1e-3"}, {"1.5", "15e-1"}, {"100", "1e2"}, {"1000", "1e3"}, {"0", "0e5"}} {
		body := `{"model":"m","max_tokens":16,` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def(pair[0]) + `]},` +
			`{"role":"system","content":[` + def(pair[1]) + `]}]}`
		result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
		if err != nil {
			t.Fatalf("%s vs %s: %v", pair[0], pair[1], err)
		}
		for _, loss := range result.Report.Losses {
			if strings.Contains(loss.Detail, "redefined") {
				t.Fatalf("%s vs %s reported as a supersession: %+v", pair[0], pair[1], loss)
			}
		}
	}
}

// TestToolDefinitionHugeExponentsStayBounded pins the availability invariant:
// client-supplied exponents are never expanded. Exact exponent comparison
// ensures a different huge value is still reported as a
// supersession rather than silently treated as an unchanged repeat. The
// supersession is a policy-gated loss, so the check runs under the approved
// policy and also asserts the strict refusal.
func TestToolDefinitionHugeExponentsStayBounded(t *testing.T) {
	def := func(minimum string) string {
		return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":` +
			`{"name":"db_query","input_schema":{"type":"object","minimum":` + minimum + `}}}}`
	}
	body := `{"model":"m","max_tokens":16,` +
		`"messages":[{"role":"user","content":"x"},` +
		`{"role":"system","content":[` + def(`1e2000000000`) + `]},` +
		`{"role":"system","content":[` + def(`1e2000000001`) + `]}]}`
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
	result, err := DecodeMessagesRequest([]byte(body), policy)
	if err != nil {
		t.Fatal(err)
	}
	redefined := 0
	for _, loss := range result.Report.Losses {
		if strings.Contains(loss.Detail, "redefined") {
			redefined++
		}
	}
	if redefined != 2 {
		t.Fatalf("distinct huge-exponent schemas: got %d supersession reports, want 2 (Note + Loss): %+v", redefined, result.Report.Losses)
	}
	if _, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy()); err == nil {
		t.Fatal("distinct huge-exponent schemas accepted under strict policy")
	}
}

// TestToolDefinitionSchemaChangeIsLastDefinitionWins proves a mid-conversation
// schema change is served rather than refused when the policy approves the
// anthropic_tool_changes loss (the default). The contract makes a later
// definition REPLACE the earlier one from its position onward, and it is the
// normal way a client ships a tool whose schema changed, so refusing it would
// 400 the very traffic this path exists to serve. A chat request carries one
// schema per name, so the only honest encoding is the newest definition serving
// the whole request — recorded as BOTH a policy-gated Loss (at
// .tool.definition) and a Note, never a silent rewrite, and never two entries
// under one name. Under StrictLossPolicy the same change is refused with a
// named UnsupportedFeatureError: the supersession loses the historical meaning
// of the earlier definition, so strict policy cannot approve it (see
// TestToolRedefinitionIsPolicyGated).
func TestToolDefinitionSchemaChangeIsLastDefinitionWins(t *testing.T) {
	def := func(prop string) string {
		return `{"type":"tool_addition","tool":{"type":"tool_definition","definition":` +
			`{"name":"db_query","input_schema":{"type":"object","properties":{"` + prop + `":{"type":"string"}}}}}}`
	}
	cases := map[string]string{
		"two definitions, one name": `{"model":"m","max_tokens":16,` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def("a") + `]},` +
			`{"role":"system","content":[` + def("b") + `]}]}`,
		"shadows a declared tool": `{"model":"m","max_tokens":16,` +
			`"tools":[{"name":"db_query","input_schema":{"type":"object"}}],` +
			`"messages":[{"role":"user","content":"x"},` +
			`{"role":"system","content":[` + def("a") + `]}]}`,
	}
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicToolChanges: {}}}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			// A strict policy refuses the genuine redefinition.
			if _, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy()); err == nil {
				t.Fatal("strict policy accepted a mid-conversation schema change")
			} else {
				var unsupported *UnsupportedFeatureError
				if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureAnthropicToolChanges) {
					t.Fatalf("strict refusal = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureAnthropicToolChanges)
				}
			}
			result, err := DecodeMessagesRequest([]byte(body), policy)
			if err != nil {
				t.Fatalf("an approved schema change was refused: %v", err)
			}
			// Exactly one entry per name, carrying the NEWEST schema.
			count := 0
			for _, tool := range result.Request.Tools {
				if tool.Name != "db_query" {
					continue
				}
				count++
				if !bytes.Contains(tool.JSONSchema, []byte(`"`+lastProp(name)+`"`)) {
					t.Fatalf("tool schema = %s, want the newest definition", tool.JSONSchema)
				}
			}
			if count != 1 {
				t.Fatalf("%q appears %d times in the tool list, want exactly one", "db_query", count)
			}
			// The supersession is observable, not silent: one policy-gated
			// Loss at .tool.definition plus one combined Note at the path.
			losses, notes := 0, 0
			for _, loss := range result.Report.Losses {
				if loss.Feature != FeatureAnthropicToolChanges || !strings.Contains(loss.Detail, "redefined") {
					continue
				}
				switch loss.Kind {
				case LossRecord:
					if !strings.Contains(loss.Path, ".tool.definition") {
						t.Fatalf("supersession Loss at %q, want .tool.definition: %+v", loss.Path, loss)
					}
					losses++
				case NoteRecord:
					notes++
				default:
					t.Fatalf("unexpected kind %v: %+v", loss.Kind, loss)
				}
			}
			if losses != 1 || notes != 1 {
				t.Fatalf("supersession records = %d Loss + %d Note, want 1+1: %+v", losses, notes, result.Report.Losses)
			}
			if err := ValidateCanonicalRequest(result.Request); err != nil {
				t.Fatalf("validator refused the decoder output: %v", err)
			}
		})
	}
}

// lastProp is the property the newest definition in each case declares.
func lastProp(caseName string) string {
	if caseName == "shadows a declared tool" {
		return "a"
	}
	return "b"
}

// TestToolChangeOnlySystemMessageIsNotATurn proves a system message that exists
// only to change tool availability does not become a conversation turn. Turning
// it into an empty-text system turn would (a) put proxy-invented prompt text on
// the wire, (b) charge the client a mid_conversation_system move for a message
// the client never wrote as content, and (c) refuse ordinary claude-cli traffic
// under a strict policy. The message contributes its tool change and nothing
// else, so the surrounding turns stay adjacent.
func TestToolChangeOnlySystemMessageIsNotATurn(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"x"}}]},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":"go"}]}`)
	result, err := DecodeMessagesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAnthropicToolChanges: {},
	}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	requireTurnSequence(t, result.Request.Turns, CanonicalUser, CanonicalAssistant, CanonicalUser)
	for i, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if text, ok := part.(CanonicalText); ok && text.Text == "" {
				t.Fatalf("turn %d carries proxy-invented empty text: %+v", i, turn.Parts)
			}
		}
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		t.Fatalf("validator refused the decoder output: %v", err)
	}
	// With the tool change gone the request is a plain three-turn conversation,
	// which a strict Chat render must accept with no position loss at all.
	if _, report, err := RenderChatRequest(
		result.Request, testExchangeContext(), ChatCapabilities{},
	); err != nil {
		t.Fatalf("strict chat render refused a tool-change-only system message: %v", err)
	} else if reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("a message that is not a turn was charged a position: %+v", report.Losses)
	}
}
