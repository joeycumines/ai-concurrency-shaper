package transcode

// Proxy-reconstructed history must be adjudicated like inline history. The
// continuity store retains canonical turns and never re-decodes them, so any
// decision written into a canonical part at decode time is stale for a later
// exchange. These tests pin the consequence that matters most: a retained
// namespaced call must resolve against the FOLLOW-UP's declared tools, so a
// follow-up that declares the qualifier renders the qualified flat name instead
// of the bare name, which would silently re-point the call at a different
// surviving tool that owns it.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestContinuityFollowUpDeclaringTheQualifierResolvesTheDeclaredName proves the
// retained qualifier is still LIVE at render time, which only holds when no
// earlier exchange destroyed it.
//
// The chain is built by a tool-less exchange — the compaction shape, where the
// qualifier cannot be mapped for a chat target — and the follow-up then
// declares the namespace. A decision baked into the retained part by the
// tool-less exchange would make the follow-up render the BARE name, and because
// the follow-up also declares a plain tool called spawn_agent, that bare name
// addresses the WRONG tool. Resolving per exchange renders the qualified flat
// name instead.
func TestContinuityFollowUpDeclaringTheQualifierResolvesTheDeclaredName(t *testing.T) {
	var upstreamBodies []string
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = compactionLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true}
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:       mapping,
			Upstream:      mustParseURL(t, "https://upstream.example"),
			BodyLimits:    BodyLimits{AcceptedRequestBytes: 1 << 20, SuccessfulResponseBytes: 1 << 20},
			Continuity:    store,
			ContinuityKey: "test-map",
		},
		func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			upstreamBodies = append(upstreamBodies, string(body))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m",` +
						`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant",` +
						`"content":"summary"}}]}`,
				)),
			}, nil
		},
		nil,
	)

	post := func(body string) (int, []byte) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	// Turn 1: a tool-less exchange whose history carries the namespaced call.
	// For a chat target the qualifier cannot be mapped, so the bare name is
	// rendered and the drop recorded. store is left at its default so the
	// chain is retained for the follow-up.
	toolLess := `{
		"model": "m",
		"tools": [],
		"tool_choice": "auto",
		"input": [
			{"type": "message", "role": "user",
			 "content": [{"type": "input_text", "text": "delegate the review"}]},
			{"type": "function_call", "call_id": "call-1", "name": "spawn_agent",
			 "namespace": "multi_agent_v1", "arguments": "{\"message\":\"review it\"}"},
			{"type": "function_call_output", "call_id": "call-1", "output": "agent-1"}
		]
	}`
	code, first := post(toolLess)
	if code != http.StatusOK {
		t.Fatalf("turn 1 status = %d: %s", code, first)
	}
	var firstEnvelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first, &firstEnvelope); err != nil {
		t.Fatalf("turn 1 body: %v\n%s", err, first)
	}
	if firstEnvelope.ID == "" {
		t.Fatal("turn 1 emitted no response id")
	}

	// The retained chain carries the qualifier the client sent, whatever the
	// tool-less exchange had to render.
	retained, _, ok := resolveContinuityChain(store, "test-map", firstEnvelope.ID)
	if !ok {
		t.Fatal("turn 1 chain was not retained")
	}
	var retainedNamespaces []string
	for _, turn := range retained {
		for _, part := range turn.Parts {
			if call, ok := part.(CanonicalFunctionCall); ok {
				retainedNamespaces = append(retainedNamespaces, call.Namespace)
			}
		}
	}
	if len(retainedNamespaces) != 1 || retainedNamespaces[0] != "multi_agent_v1" {
		t.Fatalf("retained call namespaces = %v, want the client-sent qualifier preserved by the "+
			"tool-less exchange that could not map it", retainedNamespaces)
	}

	// The follow-up declares both a plain spawn_agent and the namespace child,
	// so the child's flat name is qualified and the bare name belongs to the
	// plain tool.
	followUp := `{"model":"m","input":"continue","tools":[` +
		`{"type": "function", "name": "spawn_agent", "description": "plain", "strict": false,` +
		` "parameters": {"type": "object", "properties": {}}},` +
		`{"type": "namespace", "name": "multi_agent_v1", "description": "agents", "tools": [` +
		`{"type": "function", "name": "spawn_agent", "description": "child", "strict": false,` +
		` "parameters": {"type": "object", "properties": {}}}]}],` +
		`"previous_response_id":` + strconv.Quote(firstEnvelope.ID) + `}`
	code, second := post(followUp)
	if code != http.StatusOK {
		t.Fatalf("follow-up status = %d: %s", code, second)
	}
	if len(upstreamBodies) != 2 {
		t.Fatalf("upstream saw %d exchanges, want 2", len(upstreamBodies))
	}
	if name := chatHistoryName(t, upstreamBodies[1]); name != "multi_agent_v1__spawn_agent" {
		t.Fatalf("re-declared follow-up history call name = %q, want the qualified flat name "+
			"(the bare name addresses the plain tool instead)\n%s",
			name, upstreamBodies[1])
	}
}
