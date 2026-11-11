package transcode

// Proxy-reconstructed history must be adjudicated exactly like inline
// history. The continuity store retains canonical turns and never re-decodes
// them, so any decision written into a canonical part at decode time is either
// absent or stale by the time a follow-up renders. These tests pin the
// consequence that matters most: a retained namespaced call must resolve
// against the FOLLOW-UP's declared tools, so a follow-up that re-declares the
// qualifier renders the flat child name instead of being misattributed to a
// different surviving tool that happens to share the bare name.

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

// collisionToolRequest declares a plain tool AND a namespace child with the
// same bare name, so the child's flat wire name is qualified
// (multi_agent_v1__spawn_agent) and is distinguishable from the plain tool.
const collisionToolRequest = `{
	"model": "m",
	"input": "delegate",
	"tools": [
		{"type": "function", "name": "spawn_agent", "description": "plain", "strict": false,
		 "parameters": {"type": "object", "properties": {}}},
		{"type": "namespace", "name": "multi_agent_v1", "description": "agents", "tools": [
			{"type": "function", "name": "spawn_agent", "description": "child", "strict": false,
			 "parameters": {"type": "object", "properties": {"message": {"type": "string"}}}}]}
	]
}`

// TestContinuityFollowUpRedefiningTheQualifierResolvesTheDeclaredName proves
// the retained qualifier is still live at render time: the follow-up declares
// the namespace again, so the retained call must render the qualified flat
// name. Rendering the bare name there would silently re-point a namespaced
// call at the plain tool that owns it.
func TestContinuityFollowUpRedefiningTheQualifierResolvesTheDeclaredName(t *testing.T) {
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
						`"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant",` +
						`"content":null,"tool_calls":[{"id":"call-1","type":"function",` +
						`"function":{"name":"multi_agent_v1__spawn_agent","arguments":"{}"}}]}}]}`,
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

	code, first := post(collisionToolRequest)
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
	// The upstream call was the qualified flat name; the client saw the bare
	// child name plus its qualifier, and that is what the store retained.
	if !strings.Contains(string(first), `"namespace":"multi_agent_v1"`) {
		t.Fatalf("turn 1 did not restore the qualifier to the client: %s", first)
	}

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
			"(the bare name would re-point the call at the plain tool)\n%s",
			name, upstreamBodies[1])
	}
}
