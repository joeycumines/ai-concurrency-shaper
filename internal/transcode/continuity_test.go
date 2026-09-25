package transcode

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// TestContinuityStoreDisabledByDefault proves the statefulness decision at
// the store level: a nil store resolves nothing and records nothing, so a
// handler built without Continuity keeps the existing observable loss.
func TestContinuityStoreDisabledByDefault(t *testing.T) {
	var store *ContinuityStore
	if _, _, ok := resolveContinuityChain(store, "m", "resp_1"); ok {
		t.Fatal("nil store resolved an id")
	}
	// Record on a nil store must not panic.
	store.Record("m", "resp_1", []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}}}, 0, false)
	if store.Len() != 0 {
		t.Fatal("nil store retained a chain")
	}
}

// TestContinuityStoreRecordResolve proves the basic retain/recall cycle:
// the recorded turns come back with depth 0, and a second-generation
// record built on them resolves at depth 1.
func TestContinuityStoreRecordResolve(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	first := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "first"}}}}
	store.Record("map-a", "resp_1", first, 0, false)
	got, depth, ok := store.Resolve("map-a", "resp_1")
	if !ok {
		t.Fatal("recorded id did not resolve")
	}
	if depth != 0 {
		t.Fatalf("depth = %d, want 0", depth)
	}
	if len(got) != 1 || got[0].Parts[0].(CanonicalText).Text != "first" {
		t.Fatalf("turns = %+v, want the recorded turn", got)
	}
	// Second generation: request turns (prior + own) recorded under resp_2.
	second := append(got, CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "second"}}})
	store.Record("map-a", "resp_2", second, 1, false)
	got2, depth2, ok := store.Resolve("map-a", "resp_2")
	if !ok || depth2 != 1 || len(got2) != 2 {
		t.Fatalf("second generation = %+v depth %d ok %v, want 2 turns at depth 1", got2, depth2, ok)
	}
}

// TestContinuityStoreMissDegrades proves every miss shape is a miss, never
// an error: unknown ids, foreign mapping keys, and empty ids all resolve
// to ok=false so the caller falls back to the existing observable loss.
func TestContinuityStoreMissDegrades(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	store.Record("map-a", "resp_1", []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "x"}}}}, 0, false)
	for _, tc := range []struct {
		name string
		key  string
		id   string
	}{
		{"unknown id", "map-a", "resp_404"},
		{"foreign map", "map-b", "resp_1"},
		{"empty id", "map-a", ""},
		{"empty mapping", "", "resp_1"},
	} {
		if _, _, ok := store.Resolve(tc.key, tc.id); ok {
			t.Fatalf("%s resolved, want a miss", tc.name)
		}
	}
}

// TestContinuityStoreStoreFalseRetainsNothing proves store:false disables
// retention: the Record call with storeFalse set leaves the store empty.
func TestContinuityStoreStoreFalseRetainsNothing(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	turns := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "x"}}}}
	store.Record("m", "resp_1", turns, 0, true)
	if store.Len() != 0 {
		t.Fatal("store:false retained a chain")
	}
}

// TestContinuityStoreCapacityEvictsOldest proves the capacity bound: with
// capacity 2, recording three ids evicts the oldest, and the survivors
// still resolve.
func TestContinuityStoreCapacityEvictsOldest(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 2, TTL: time.Hour})
	mk := func(text string) []CanonicalTurn {
		return []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: text}}}}
	}
	store.Record("m", "resp_1", mk("one"), 0, false)
	store.Record("m", "resp_2", mk("two"), 0, false)
	store.Record("m", "resp_3", mk("three"), 0, false)
	if store.Len() != 2 {
		t.Fatalf("Len = %d, want 2", store.Len())
	}
	if _, _, ok := store.Resolve("m", "resp_1"); ok {
		t.Fatal("evicted resp_1 still resolves")
	}
	for _, id := range []string{"resp_2", "resp_3"} {
		if _, _, ok := store.Resolve("m", id); !ok {
			t.Fatalf("%s does not resolve, want survivor", id)
		}
	}
}

// TestContinuityStoreTTLExpires proves the TTL bound with a controllable
// clock: a chain older than the TTL is a miss, and re-recording revives it.
func TestContinuityStoreTTLExpires(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Minute})
	now := time.Now()
	store.now = func() time.Time { return now }
	turns := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "x"}}}}
	store.Record("m", "resp_1", turns, 0, false)
	now = now.Add(2 * time.Minute)
	if _, _, ok := store.Resolve("m", "resp_1"); ok {
		t.Fatal("expired chain resolved, want a miss")
	}
	store.Record("m", "resp_1", turns, 0, false)
	if _, _, ok := store.Resolve("m", "resp_1"); !ok {
		t.Fatal("re-recorded chain did not resolve")
	}
}

// TestContinuityChainDepthCapped proves resolution depth is bounded: a
// chain recorded past MaxContinuityDepth resolves at exactly the cap.
func TestContinuityChainDepthCapped(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	turns := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "x"}}}}
	store.Record("m", "resp_deep", turns, MaxContinuityDepth, false)
	_, depth, ok := resolveContinuityChain(store, "m", "resp_deep")
	if !ok {
		t.Fatal("deep chain did not resolve")
	}
	if depth != MaxContinuityDepth {
		t.Fatalf("depth = %d, want cap %d", depth, MaxContinuityDepth)
	}
}

// TestContinuityConcurrentIsolation proves thread safety and chain
// isolation: parallel writers and readers on distinct ids never observe
// each other's turns, under -race.
func TestContinuityConcurrentIsolation(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 64, TTL: time.Hour})
	done := make(chan bool, 16)
	for i := range 8 {
		id := strings.Repeat(string(rune('a'+i)), 4)
		go func(id string, seed int) {
			for j := range 25 {
				text := strings.Repeat(id, j+1)
				store.Record("m", id, []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: text}}}}, 0, false)
				got, _, ok := store.Resolve("m", id)
				if !ok || len(got) != 1 {
					t.Errorf("id %q: resolve failed", id)
					done <- false
					return
				}
				for _, turn := range got {
					for _, part := range turn.Parts {
						if text, ok := part.(CanonicalText); ok {
							for _, r := range text.Text {
								if r != rune(id[0]) {
									t.Errorf("id %q observed foreign content %q", id, text.Text)
									done <- false
									return
								}
							}
						}
					}
				}
				_ = seed
			}
			done <- true
		}(id, i)
	}
	for range 8 {
		if !<-done {
			t.Fatal("concurrent isolation failed")
		}
	}
}

// TestContinuityFollowUpReconstructs proves the end-to-end behaviour at the
// conversion level: a follow-up request carrying a previous_response_id the
// store knows resolves the prior turns ahead of its own input and records
// the hit Note; an unknown id degrades to the existing observable loss.
func TestContinuityFollowUpReconstructs(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	prior := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "prior question"}}}}
	store.Record("map-a", "resp_1", prior, 0, false)

	// Hit path: resolve + prepend + note.
	resolved, depth, ok := resolveContinuityChain(store, "map-a", "resp_1")
	if !ok || depth != 1 {
		t.Fatalf("hit = ok %v depth %d, want ok true depth 1", ok, depth)
	}
	own := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "follow-up"}}}}
	merged := append(append([]CanonicalTurn{}, resolved...), own...)
	if len(merged) != 2 {
		t.Fatalf("merged = %d turns, want prior + own", len(merged))
	}
	var report ConversionReport
	if err := continuityNote(&report, depth, "resp_1"); err != nil {
		t.Fatal(err)
	}
	if len(report.Losses) != 1 || report.Losses[0].Kind != NoteRecord {
		t.Fatalf("report = %+v, want one hit Note", report.Losses)
	}

	// Miss path: unknown id resolves to ok=false; the caller then applies
	// the existing observable loss (a Lose under the approved policy).
	if _, _, ok := resolveContinuityChain(store, "map-a", "resp_404"); ok {
		t.Fatal("unknown id resolved, want a miss")
	}
	var missReport ConversionReport
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeaturePreviousResponseID: {}}}
	if err := missReport.Lose(policy, FeaturePreviousResponseID, "request.previous_response_id", "the Responses previous_response_id field is not portable to a chat upstream"); err != nil {
		t.Fatal(err)
	}
	if len(missReport.Losses) != 1 || missReport.Losses[0].Kind != LossRecord {
		t.Fatalf("miss report = %+v, want the existing observable loss", missReport.Losses)
	}
}

// TestResponseItemsToTurns proves the record-side rebuild: message items
// become assistant turns, function calls join as call parts with identity,
// and result/reasoning items are skipped as non-model-output state.
func TestResponseItemsToTurns(t *testing.T) {
	response := CanonicalResponse{
		Items: []CanonicalResponseItem{
			&CanonicalMessageItem{Role: CanonicalAssistant, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
			&CanonicalFunctionCallItem{CallID: "call_1", Name: "get_weather", Arguments: ParseToolArguments(`{"city":"x"}`)},
			&CanonicalFunctionResultItem{CallID: "call_1", Parts: []CanonicalPart{CanonicalText{Text: "sunny"}}},
			&CanonicalReasoningItem{Raw: []byte(`{}`)},
			&CanonicalMessageItem{Role: CanonicalAssistant, Parts: []CanonicalPart{CanonicalText{Text: "done"}}},
		},
	}
	turns, omitted := responseItemsToTurns(response)
	if omitted != 0 {
		t.Fatalf("omitted = %d, want 0 for object-shaped arguments", omitted)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want message(+call folded) + message", len(turns))
	}
	if len(turns[0].Parts) != 2 {
		t.Fatalf("turn 0 parts = %d, want text + folded call", len(turns[0].Parts))
	}
	if _, ok := turns[0].Parts[1].(CanonicalFunctionCall); !ok {
		t.Fatalf("turn 0 part 1 = %T, want CanonicalFunctionCall", turns[0].Parts[1])
	}
	if call := turns[0].Parts[1].(CanonicalFunctionCall); call.CallID != "call_1" || call.Name != "get_weather" {
		t.Fatalf("call identity = %+v, want call_1/get_weather", call)
	}
	if empty, _ := responseItemsToTurns(CanonicalResponse{}); len(empty) != 0 {
		t.Fatal("empty response yielded turns")
	}
}

// TestContinuityTwoTurnReplay proves the full handler-level exchange: turn 1
// records its conversation under the emitted id; turn 2 carrying that id as
// previous_response_id reaches the upstream WITH the prior turns prepended
// (the upstream sees two user messages), while the default handler without
// a store sends only the follow-up input.
func TestContinuityTwoTurnReplay(t *testing.T) {
	chatReply := func(text string) func(*http.Request) (*http.Response, error) {
		return func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":` + strconv.Quote(text) + `}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":2}}}`,
				)),
			}, nil
		}
	}
	build := func(store *ContinuityStore) (*TranscodeHandler, *[][]string) {
		var seen [][]string
		mapping := responsesMapping(t)
		mapping.ModelMap = ModelMap{AllowIdentity: true}
		mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeaturePreviousResponseID: {}, FeatureUsageCacheReadUnknown: {}, FeatureUsageCacheWriteUnknown: {}, FeatureUsageReasoningUnknown: {}}}
		mapping.Auth = AuthPolicy{Mode: AuthNone}
		mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
		mapping.AllowedClientQuery = map[string]struct{}{}
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
				var chat ChatRequest
				if err := json.Unmarshal(body, &chat); err != nil {
					t.Fatalf("upstream request: %v", err)
				}
				var texts []string
				for _, msg := range chat.Messages {
					if msg.Content != nil && msg.Content.ContentStr != nil {
						texts = append(texts, *msg.Content.ContentStr)
					} else if msg.Content != nil {
						for _, block := range msg.Content.ContentBlocks {
							if block.Text != nil {
								texts = append(texts, *block.Text)
							}
						}
					}
				}
				seen = append(seen, texts)
				return chatReply("answer")(req)
			},
			nil,
		)
		return handler, &seen
	}

	// With the store: turn 1, capture the emitted id, then follow up.
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	handler, seen := build(store)
	post := func(h *TranscodeHandler, body string) (int, []byte) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	code, first := post(handler, `{"model":"m","input":"first question"}`)
	if code != http.StatusOK {
		t.Fatalf("turn 1 status = %d: %s", code, first)
	}
	var firstEnvelope struct {
		ID string `json:"id"`
	}
	t.Logf("turn 1 body: %s", string(first))
	if err := json.Unmarshal(first, &firstEnvelope); err != nil {
		t.Fatal(err)
	}
	if firstEnvelope.ID == "" {
		t.Fatal("turn 1 emitted no response id")
	}
	followUp := `{"model":"m","input":"second question","previous_response_id":` + strconv.Quote(firstEnvelope.ID) + `}`
	code, second := post(handler, followUp)
	if code != http.StatusOK {
		t.Fatalf("turn 2 status = %d: %s", code, second)
	}
	if len(*seen) != 2 {
		t.Fatalf("upstream saw %d exchanges, want 2", len(*seen))
	}
	turn2 := (*seen)[1]
	if len(turn2) < 3 {
		t.Fatalf("turn 2 upstream texts = %q, want prior + answer + follow-up", turn2)
	}
	if turn2[0] != "first question" || turn2[1] != "answer" || turn2[2] != "second question" {
		t.Fatalf("turn 2 upstream texts = %q, want [first question answer second question]", turn2)
	}

	// Without the store (the default): the same follow-up succeeds but the
	// upstream sees only the follow-up input (existing observable loss).
	plain, seenPlain := build(nil)
	code, _ = post(plain, `{"model":"m","input":"first question"}`)
	if code != http.StatusOK {
		t.Fatalf("plain turn 1 status = %d", code)
	}
	code, _ = post(plain, `{"model":"m","input":"second question","previous_response_id":"resp_unknown"}`)
	if code != http.StatusOK {
		t.Fatalf("plain turn 2 status = %d, want success via the existing loss", code)
	}
	if len(*seenPlain) != 2 || len((*seenPlain)[1]) != 1 || (*seenPlain)[1][0] != "second question" {
		t.Fatalf("plain turn 2 upstream = %q, want only the follow-up", *seenPlain)
	}
}

// TestContinuityHitSkipsRenderLoss proves the consumed-not-lost rule: when
// the store resolves the id, RenderChatRequest does NOT record the
// previous_response_id loss (the decode-time hit Note carries the fact);
// on a miss the loss is still recorded exactly as before.
func TestContinuityHitSkipsRenderLoss(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeaturePreviousResponseID: {}}}
	mkCtx := func(depth int) *ExchangeContext {
		echo := &ResponsesRequestEcho{PreviousResponseID: new("resp_1")}
		return &ExchangeContext{
			IDs:                      NewExchangeIDs(),
			LossPolicy:               policy,
			OriginalResponsesRequest: echo,
			RequestDepth:             depth,
			RequestTurns:             []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "q"}}}},
		}
	}
	req := CanonicalRequest{
		ClientModel: "m",
		Turns:       []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "q"}}}},
	}
	// Hit (depth 1): no loss recorded.
	_, hitReport, err := RenderChatRequest(req, mkCtx(1), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	for _, loss := range hitReport.Losses {
		if loss.Feature == FeaturePreviousResponseID {
			t.Fatalf("hit recorded previous_response_id loss: %+v", loss)
		}
	}
	// Miss (depth 0): the existing observable loss stands.
	_, missReport, err := RenderChatRequest(req, mkCtx(0), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, loss := range missReport.Losses {
		if loss.Feature == FeaturePreviousResponseID && loss.Kind == LossRecord {
			found = true
		}
	}
	if !found {
		t.Fatalf("miss report = %+v, want the existing observable loss", missReport.Losses)
	}
}

// TestContinuityStreamRetains proves the streaming record path: a completed
// Responses->Chat stream retains its conversation under the emitted id, and
// a follow-up resolving that id reconstructs prior + answer + follow-up.
func TestContinuityStreamRetains(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeaturePreviousResponseID: {}}}
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	ctx := &ExchangeContext{
		IDs:          NewExchangeIDs(),
		LossPolicy:   policy,
		RequestTurns: []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "stream q"}}}},
	}
	// Drive the state machine directly: one text delta, finish, release.
	state := newChatResponsesStreamState(ctx, policy, ChatCapabilities{}, "resp_stream_1", "m", 1710000000, nil)
	text := "stream answer"
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: &text}, nil)); err != nil {
		t.Fatal(err)
	}
	stop := "stop"
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, &stop)); err != nil {
		t.Fatal(err)
	}
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	if len(held) == 0 {
		t.Fatal("empty terminal batch")
	}
	if id, turns, depth, _, ok := state.continuityTurns(ctx.RequestTurns, 0); !ok {
		t.Fatal("continuityTurns not ready after release")
	} else {
		if id != "resp_stream_1" {
			t.Fatalf("id = %q, want resp_stream_1", id)
		}
		store.Record("k", id, turns, depth, false)
	}
	// Follow-up resolves: prior + answer + new question.
	prior, depth, ok := resolveContinuityChain(store, "k", "resp_stream_1")
	if !ok || depth != 1 {
		t.Fatalf("resolve = ok %v depth %d, want ok true depth 1", ok, depth)
	}
	texts := []string{}
	follow := append(append([]CanonicalTurn{}, prior...), CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "next"}}})
	for _, turn := range follow {
		for _, part := range turn.Parts {
			if text, ok := part.(CanonicalText); ok {
				texts = append(texts, text.Text)
			}
		}
	}
	if len(texts) != 3 || texts[0] != "stream q" || texts[1] != "stream answer" || texts[2] != "next" {
		t.Fatalf("texts = %q, want [stream q stream answer next]", texts)
	}
	// Pre-release retains nothing.
	state2 := newChatResponsesStreamState(ctx, policy, ChatCapabilities{}, "resp_x", "m", 1710000000, nil)
	if _, _, _, _, ok := state2.continuityTurns(ctx.RequestTurns, 0); ok {
		t.Fatal("pre-release continuityTurns ready, want not-ready")
	}
}

// TestContinuityForeignIdRefusal proves that a request for one chain that
// sends a previous_response_id from a different chain (same provider, different
// id) misses and degrades to the existing observable loss.
func TestContinuityForeignIdRefusal(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 10, TTL: time.Minute})

	// Record chain A.
	chainA := []CanonicalTurn{
		{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "chain A turn"}}},
	}
	store.Record("provider1", "resp-a", chainA, 0, false)

	// Request for chain B (different id, same provider) should miss.
	_, _, ok := store.Resolve("provider1", "resp-b")
	if ok {
		t.Error("foreign id within same provider should not resolve")
	}

	// Request for chain A with wrong provider should miss.
	_, _, ok = store.Resolve("provider2", "resp-a")
	if ok {
		t.Error("foreign provider should not resolve")
	}
}

func TestContinuityStreamingEvictionLogging(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 1, TTL: time.Hour})
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.AllowedClientQuery = map[string]struct{}{}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:       mapping,
			Upstream:      mustParseURL(t, "https://upstream.example"),
			BodyLimits:    BodyLimits{AcceptedRequestBytes: 1 << 20, SuccessfulResponseBytes: 1 << 20},
			Continuity:    store,
			ContinuityKey: "test-map",
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE())),
			}, nil
		},
		nil,
	)

	// Stream 1: records first chain
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"q1","stream":true}`))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("stream 1 status = %d: %s", rec1.Code, rec1.Body.String())
	}
	if store.Len() != 1 {
		t.Fatalf("store len after stream 1 = %d, want 1", store.Len())
	}

	// Stream 2: causes eviction, verify log output
	var logBuf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origWriter)

	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"q2","stream":true}`))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("stream 2 status = %d: %s", rec2.Code, rec2.Body.String())
	}

	logOutput := logBuf.String()
	wantLog := "continuity store evicted 1 chain(s) at capacity/TTL bound"
	if !strings.Contains(logOutput, wantLog) {
		t.Errorf("expected log output to contain %q, got: %q", wantLog, logOutput)
	}
	if !strings.Contains(logOutput, "[local_response_conversion_error]") {
		t.Errorf("expected log output to contain [local_response_conversion_error], got: %q", logOutput)
	}
}

// TestContinuityReplaysEmptyArgumentToolCall proves a retained tool call whose
// arguments are the empty string - the shape upstream models commonly emit for
// a no-argument call - does not make the FOLLOW-UP request fail. The response
// path preserves that output byte-exact (arguments: ""), but the request-side
// renderer requires object-shaped arguments, so carrying the raw text minted a
// part the renderer rejected and turned a valid follow-up into a 400 over
// content the client never sent. The normalized "{}" must be carried instead.
func TestContinuityReplaysEmptyArgumentToolCall(t *testing.T) {
	var upstreamBodies []string
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})

	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeaturePreviousResponseID: {}, FeatureUsageCacheReadUnknown: {},
		FeatureUsageCacheWriteUnknown: {}, FeatureUsageReasoningUnknown: {},
	}}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

	call := 0
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
			call++
			reply := `{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`
			if call == 1 {
				// No-argument tool call: arguments is the empty string, which the
				// response path must preserve byte-exact.
				reply = `{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"do_thing","arguments":""}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(reply)),
			}, nil
		},
		nil,
	)

	// Turn 1: the model emits the no-argument tool call.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"call the tool"}`))
	req1.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, want 200, body: %s", rec1.Code, rec1.Body.String())
	}
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal turn 1 envelope: %v", err)
	}
	if env.ID == "" {
		t.Fatal("turn 1 emitted no response id to follow up on")
	}

	// Turn 2: the follow-up must succeed and must carry the reconstructed call.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"thanks","previous_response_id":"`+env.ID+`"}`))
	req2.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200: a retained no-argument tool call must not "+
			"fail the follow-up. body: %s", rec2.Code, rec2.Body.String())
	}
	if len(upstreamBodies) != 2 {
		t.Fatalf("upstream exchanges = %d, want 2 (the follow-up must reach the upstream)", len(upstreamBodies))
	}
	if !strings.Contains(upstreamBodies[1], "do_thing") {
		t.Errorf("follow-up upstream body lost the reconstructed tool call: %s", upstreamBodies[1])
	}
	if !strings.Contains(upstreamBodies[1], `"arguments":"{}"`) {
		t.Errorf("follow-up upstream body must carry normalized object arguments, got: %s", upstreamBodies[1])
	}
}

// TestContinuityRetainedSystemTurnIsNotChargedToClient proves that a system
// turn reconstructed from the store does not let a request-side gate reject a
// request the client authored cleanly. Turn 1 sends instructions; the retained
// chain carries that system turn. The follow-up ALSO sends instructions, so the
// retained system turn now sits after dialog turns and trips the
// mid_conversation_system position gate. Under a policy that does not approve
// that key, rejecting would blame messages[] - a field the client never sent in
// that request - and turn a working follow-up into a 400. The consolidation is
// the same encoding either way, so a fully-retained mid-dialog system turn is
// recorded as a Note instead of a policy-gated loss.
func TestContinuityRetainedSystemTurnIsNotChargedToClient(t *testing.T) {
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	var upstreamSeen int

	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	// Deliberately NOT approving mid_conversation_system: under this policy a
	// client-authored mid-dialog system turn must still be rejected, so the
	// test also proves the exemption is scoped to retained turns only.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown: {}, FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown: {},
	}}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}

	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:       mapping,
			Upstream:      mustParseURL(t, "https://upstream.example"),
			BodyLimits:    BodyLimits{AcceptedRequestBytes: 1 << 20, SuccessfulResponseBytes: 1 << 20},
			Continuity:    store,
			ContinuityKey: "test-map",
		},
		func(req *http.Request) (*http.Response, error) {
			upstreamSeen++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
				)),
			}, nil
		},
		nil,
	)

	// Turn 1 carries instructions, so the retained chain holds a system turn.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","instructions":"be brief","input":"hello"}`))
	req1.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, want 200, body: %s", rec1.Code, rec1.Body.String())
	}
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal turn 1 envelope: %v", err)
	}

	// The follow-up also carries instructions.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","instructions":"be brief","input":"again","previous_response_id":"`+env.ID+`"}`))
	req2.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200: retained history must not be charged to the "+
			"client. body: %s", rec2.Code, rec2.Body.String())
	}
	if upstreamSeen != 2 {
		t.Errorf("upstream exchanges = %d, want 2 (the follow-up must reach the upstream)", upstreamSeen)
	}
}

// TestClientAuthoredMidConversationSystemTurnStillGated is the scope guard for
// the retained-turn exemption above. Only history the PROXY prepended may be
// exempted; a mid-conversation system turn the client itself authored must
// still be a policy-gated mid_conversation_system loss. Without this, the
// exemption would silently weaken a real client-request gate.
func TestClientAuthoredMidConversationSystemTurnStillGated(t *testing.T) {
	text := func(s string) []CanonicalPart { return []CanonicalPart{CanonicalText{Text: s}} }
	req := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalSystem, Parts: text("leading")},
			{Role: CanonicalUser, Parts: text("hello")},
			{Role: CanonicalSystem, Parts: text("client's own mid-conversation turn")},
		},
		// RetainedTurns is 0: everything here is client-authored.
		RetainedTurns: 0,
	}
	policy := LossPolicy{Allowed: map[Feature]struct{}{}}
	ctx := &ExchangeContext{
		IDs:          NewExchangeIDs(),
		LossPolicy:   policy,
		Capabilities: ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
	}

	_, report, err := RenderChatRequest(req, ctx, ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true})
	if err == nil {
		t.Fatal("want a client-dialect rejection for a client-authored mid-conversation system " +
			"turn under a policy that does not approve mid_conversation_system")
	}
	if !strings.Contains(err.Error(), string(FeatureMidConversationSystem)) {
		t.Errorf("error = %q, want it to name %s", err.Error(), FeatureMidConversationSystem)
	}
	_ = report
	// The same client-authored shape must SUCCEED once the operator approves
	// the key: the gate is a policy decision, not a hard rejection.
	approved := LossPolicy{Allowed: map[Feature]struct{}{FeatureMidConversationSystem: {}}}
	ctxOK := &ExchangeContext{
		IDs:          NewExchangeIDs(),
		LossPolicy:   approved,
		Capabilities: ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
	}
	if _, _, err := RenderChatRequest(
		req, ctxOK, ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
	); err != nil {
		t.Errorf("with %s approved the same turns must convert, got: %v",
			FeatureMidConversationSystem, err)
	}
}
