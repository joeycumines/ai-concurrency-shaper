package transcode

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// anthropicBlockEvents collects (event type, content block index) pairs in
// order from an Anthropic dialect SSE body.
func anthropicBlockEvents(t *testing.T, body string) []struct {
	Type  string
	Index int
} {
	t.Helper()
	var events []struct {
		Type  string
		Index int
	}
	scanner := bufio.NewScanner(strings.NewReader(body))
	eventType := ""
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = after
			continue
		}
		after, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if eventType != "content_block_start" && eventType != "content_block_stop" {
			continue
		}
		var frame struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(after), &frame); err != nil {
			t.Fatalf("unmarshal %s frame: %v", eventType, err)
		}
		events = append(events, struct {
			Type  string
			Index int
		}{Type: eventType, Index: frame.Index})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// positions returns the first position of an event in the collected order.
func eventPosition(events []struct {
	Type  string
	Index int
}, eventType string, index int) int {
	for i, event := range events {
		if event.Type == eventType && event.Index == index {
			return i
		}
	}
	return -1
}

func TestStreamTextThenToolClosesEachBlockBeforeTheNext(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	// The Messages contract requires usage on message_start, which the chat
	// upstream can only provide later: the CLI default approves that known
	// loss, so the ordering under test is reached rather than the rejection.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureUsageUnknown: {}}}
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":\"thinking out loud\"}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	events := anthropicBlockEvents(t, out)

	textStart := eventPosition(events, "content_block_start", 0)
	textStop := eventPosition(events, "content_block_stop", 0)
	toolStart := eventPosition(events, "content_block_start", 1)
	toolStop := eventPosition(events, "content_block_stop", 1)
	if textStart < 0 || textStop < 0 || toolStart < 0 || toolStop < 0 {
		t.Fatalf("missing block transitions: %+v\n%s", events, out)
	}
	if !(textStart < textStop && textStop < toolStart && toolStart < toolStop) {
		t.Fatalf("blocks must close before the next opens: text start %d, text stop %d, tool start %d, tool stop %d: %+v",
			textStart, textStop, toolStart, toolStop, events)
	}
	stops := 0
	for _, event := range events {
		if event.Type == "content_block_stop" {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("started blocks = 2, stops = %d: %+v", stops, events)
	}
}

func TestStreamTextOnlyStillStopsItsBlock(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	// The Messages contract requires usage on message_start, which the chat
	// upstream can only provide later: the CLI default approves that known
	// loss, so the ordering under test is reached rather than the rejection.
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureUsageUnknown: {}}}
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":\"just text\"}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	events := anthropicBlockEvents(t, out)
	start := eventPosition(events, "content_block_start", 0)
	stop := eventPosition(events, "content_block_stop", 0)
	if start < 0 || stop < 0 || start > stop {
		t.Fatalf("a text-only stream must open and close one block: %+v\n%s", events, out)
	}
	stops := 0
	for _, event := range events {
		if event.Type == "content_block_stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("text-only stream stops = %d, want 1: %+v", stops, events)
	}
}

// TestStreamContentAfterToolCallStillCompletes proves the lifecycle that
// closes a message item at a tool transition still completes when the model
// emits content after the tool call: the closed item is never appended to, a
// new message item opens, and every block still closes before the next opens.
func TestStreamContentAfterToolCallStillCompletes(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureUsageUnknown: {}}}
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":\"before\"}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"content\":\"after\"}}]}\n\n" +
					"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "event: error") {
		t.Fatalf("content after a tool call must still complete, got an error event: %s", out)
	}
	events := anthropicBlockEvents(t, out)
	if len(events) == 0 {
		t.Fatalf("no block events: %s", out)
	}
	// Every block must close before the next one opens, on every transition of
	// this exchange: text, tool, then the resumed text.
	open := 0
	for _, event := range events {
		switch event.Type {
		case "content_block_start":
			if open != 0 {
				t.Fatalf("a block started while another was open: %+v", events)
			}
			open = event.Index + 1
		case "content_block_stop":
			if open == 0 {
				t.Fatalf("a block stopped while none was open: %+v", events)
			}
			open = 0
		}
	}
	if open != 0 {
		t.Fatalf("a block was left open at the end: %+v", events)
	}
	if len(events) != 6 {
		t.Fatalf("want three sealed blocks (text, tool, resumed text): %+v", events)
	}
	if !strings.Contains(out, "after") {
		t.Fatalf("the content emitted after the tool call is missing: %s", out)
	}
}

// TestChatStateSealsMessageBeforeThinking proves the producer closes an open
// message item before it opens a reasoning item: a thinking block must not
// start while the text block is still open.
func TestChatStateSealsMessageBeforeThinking(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		LossPolicy{},
		ChatCapabilities{ProviderReasoningThinking: true},
		"resp_1",
		"m",
		1,
		nil,
	)
	first, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: new("spoken")}, nil))
	if err != nil {
		t.Fatalf("content convert: %v", err)
	}
	second, err := state.Convert(chatChunk(t, ChatStreamDelta{ReasoningContent: new("thought")}, nil))
	if err != nil {
		t.Fatalf("reasoning convert: %v", err)
	}

	var names []string
	for _, event := range append(append([]ResponsesSSEEvent{}, first...), second...) {
		switch event.(type) {
		case ResponseContentPartDoneEvent:
			names = append(names, "content_part.done")
		case ResponseOutputItemDoneEvent:
			names = append(names, "output_item.done")
		case ResponseOutputItemAddedEvent:
			names = append(names, "output_item.added")
		}
	}
	// The second batch must open the reasoning item only after the message
	// item has been fully closed.
	indexOf := func(name string) int {
		for i, seen := range names {
			if seen == name {
				return i
			}
		}
		return -1
	}
	var addedAt []int
	for i, seen := range names {
		if seen == "output_item.added" {
			addedAt = append(addedAt, i)
		}
	}
	if len(addedAt) != 2 || indexOf("content_part.done") < 0 || indexOf("output_item.done") < 0 {
		t.Fatalf("unexpected event sequence: %v", names)
	}
	// The message opens first; the thinking item must open only after the
	// message's part and item are both done.
	partDone, itemDone, thinkingAdded := indexOf("content_part.done"), indexOf("output_item.done"), addedAt[1]
	if !(addedAt[0] < partDone && partDone < itemDone && itemDone < thinkingAdded) {
		t.Fatalf("the message must be sealed before the thinking item opens: %v", names)
	}
}

// streamChatUpstream drives one Messages->Chat stream through a handler whose
// chat capability set the test controls.
func streamChatUpstream(t *testing.T, capabilities ChatCapabilities, sse string) *httptest.ResponseRecorder {
	t.Helper()
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	mapping.ChatCapabilities = capabilities
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// assertBlocksSealedInOrder proves every content block closes before the next
// opens, count blocks by their stops, and no error event was emitted.
func assertBlocksSealedInOrder(t *testing.T, out string, wantStops int) []struct {
	Type  string
	Index int
} {
	t.Helper()
	if strings.Contains(out, "event: error") {
		t.Fatalf("stream must complete without an error event: %s", out)
	}
	events := anthropicBlockEvents(t, out)
	open := 0
	for _, event := range events {
		switch event.Type {
		case "content_block_start":
			if open != 0 {
				t.Fatalf("a block started while another was open: %+v", events)
			}
			open = event.Index + 1
		case "content_block_stop":
			if open == 0 {
				t.Fatalf("a block stopped while none was open: %+v", events)
			}
			open = 0
		}
	}
	if open != 0 {
		t.Fatalf("a block was left open at the end: %+v", events)
	}
	stops := 0
	for _, event := range events {
		if event.Type == "content_block_stop" {
			stops++
		}
	}
	if stops != wantStops {
		t.Fatalf("stopped blocks = %d, want %d: %+v", stops, wantStops, events)
	}
	return events
}

// TestStreamToolThenThinkingClosesEachBlockBeforeTheNext proves a started tool
// block is sealed when a reasoning item must open: the tool_use block cannot
// stay open while the thinking block opens.
func TestStreamToolThenThinkingClosesEachBlockBeforeTheNext(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{ProviderReasoningThinking: true},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"thinking\"}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	events := assertBlocksSealedInOrder(t, out, 2)
	toolStop := eventPosition(events, "content_block_stop", 0)
	thinkingStart := eventPosition(events, "content_block_start", 1)
	if toolStop < 0 || thinkingStart < 0 {
		t.Fatalf("expected a sealed tool block and an opened thinking block: %+v\n%s", events, out)
	}
	if toolStop > thinkingStart {
		t.Fatalf("the tool block must close before the thinking block opens: tool stop %d, thinking start %d: %+v",
			toolStop, thinkingStart, events)
	}
	if !strings.Contains(out, "thinking") {
		t.Fatalf("the reasoning content is missing: %s", out)
	}
}

// TestStreamReasoningThenRefusalClosesEachBlockBeforeTheNext proves a refusal
// arriving while a reasoning item is open seals the reasoning item first: the
// thinking block cannot stay open while the refusal part opens.
func TestStreamReasoningThenRefusalClosesEachBlockBeforeTheNext(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{ProviderReasoningThinking: true},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"thinking\"}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"refusal\":\"cannot comply\"}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	events := assertBlocksSealedInOrder(t, out, 2)
	thinkingStop := eventPosition(events, "content_block_stop", 0)
	refusalStart := eventPosition(events, "content_block_start", 1)
	if thinkingStop < 0 || refusalStart < 0 {
		t.Fatalf("expected a sealed thinking block and an opened refusal block: %+v\n%s", events, out)
	}
	if thinkingStop > refusalStart {
		t.Fatalf("the thinking block must close before the refusal part opens: thinking stop %d, refusal start %d: %+v",
			thinkingStop, refusalStart, events)
	}
	if !strings.Contains(out, "cannot comply") {
		t.Fatalf("the refusal content is missing: %s", out)
	}
}

// TestStreamLateToolFragmentAfterSealRejected proves a fragment reusing the id
// of an already-sealed call is corrupt upstream wire: it must fail the stream
// instead of forking a duplicate call id into the terminal envelope.
func TestStreamLateToolFragmentAfterSealRejected(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n"+
			// Content resumes, sealing the tool call inline.
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"content\":\"after\"}}]}\n\n"+
			// A late fragment for the sealed call arrives, carrying enough
			// identity that a forked call would complete and duplicate the
			// sealed call id in the terminal envelope.
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"more\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("a late fragment for a sealed call must fail the stream: %s", out)
	}
	if strings.Contains(out, "event: message_stop") {
		t.Fatalf("a failed stream must not report a successful terminal: %s", out)
	}
	if strings.Count(out, `"call-1"`) > 1 {
		t.Fatalf("the sealed call id must not be duplicated: %s", out)
	}
}

// TestStreamLateSealedCallIDRejectedBeforeAdoption proves the tombstone also
// covers the adoption path: a sealed call's id arriving again must be rejected
// even when an unstarted pending call would otherwise adopt it through its
// fragment index, because either resolution forks a duplicate call id.
func TestStreamLateSealedCallIDRejectedBeforeAdoption(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"content\":\"after\"}}]}\n\n"+
			// A new index-0 call arrives without an id (unstarted).
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"other\",\"arguments\":\"\"}}]}}]}\n\n"+
			// The sealed call's id arrives; index resolution would adopt it.
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"arguments\":\"x\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	out := rec.Body.String()
	if !strings.Contains(out, "reuses the id") {
		t.Fatalf("a sealed call id must be rejected before adoption: %s", out)
	}
	if strings.Contains(out, "event: message_stop") {
		t.Fatalf("a failed stream must not report a successful terminal: %s", out)
	}
	if got := strings.Count(out, `"call-1"`); got != 1 {
		t.Fatalf("the sealed call id must appear exactly once (its original block), got %d: %s", got, out)
	}
	if strings.Contains(out, `"name":"other"`) {
		t.Fatalf("the forked call block must never open: %s", out)
	}
}

// anthropicEvent captures one content-block event's type, block index, and
// (for input_json_delta) its partial JSON payload.
type anthropicEvent struct {
	Type  string
	Index int
	Data  string
}

// anthropicEventsDetailed collects the content-block events of an Anthropic
// dialect SSE body, including delta payloads.
func anthropicEventsDetailed(t *testing.T, body string) []anthropicEvent {
	t.Helper()
	var events []anthropicEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	eventType := ""
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = after
			continue
		}
		after, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		switch eventType {
		case "content_block_start", "content_block_stop":
			var frame struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(after), &frame); err != nil {
				t.Fatalf("unmarshal %s frame: %v", eventType, err)
			}
			events = append(events, anthropicEvent{Type: eventType, Index: frame.Index})
		case "content_block_delta":
			var frame struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(after), &frame); err != nil {
				t.Fatalf("unmarshal delta frame: %v", err)
			}
			if frame.Delta.Type == "input_json_delta" {
				events = append(events, anthropicEvent{Type: "input_json_delta", Index: frame.Index, Data: frame.Delta.PartialJSON})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// toolCallInputs assembles each block's input_json_delta payloads in order.
func toolCallInputs(events []anthropicEvent) map[int]string {
	inputs := map[int]string{}
	for _, event := range events {
		if event.Type == "input_json_delta" {
			inputs[event.Index] += event.Data
		}
	}
	return inputs
}

// assertBlocksSequential proves one block opens at a time and closes before
// the next opens, returning the ordered block indexes.
func assertBlocksSequential(t *testing.T, out string) []int {
	t.Helper()
	if strings.Contains(out, "event: error") {
		t.Fatalf("stream must complete without an error event: %s", out)
	}
	events := anthropicBlockEvents(t, out)
	var order []int
	open := -1
	for _, event := range events {
		switch event.Type {
		case "content_block_start":
			if open >= 0 {
				t.Fatalf("block %d opened while block %d was open: %+v", event.Index, open, events)
			}
			open = event.Index
			order = append(order, event.Index)
		case "content_block_stop":
			if open != event.Index {
				t.Fatalf("block %d stopped while block %d was open: %+v", event.Index, open, events)
			}
			open = -1
		}
	}
	if open >= 0 {
		t.Fatalf("block %d was left open at the end: %+v", open, events)
	}
	return order
}

// TestStreamParallelToolCallsSealedInReservationOrder proves two parallel
// calls with interleaved argument fragments render as two sequential tool_use
// blocks, each closed before the next opens and each carrying its own complete
// arguments.
func TestStreamParallelToolCallsSealedInReservationOrder(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"fa\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"fb\",\"arguments\":\"{\\\"y\\\":\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"2}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	order := assertBlocksSequential(t, out)
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Fatalf("blocks must be sequential in reservation order, got %v: %s", order, out)
	}
	inputs := toolCallInputs(anthropicEventsDetailed(t, out))
	if inputs[0] != `{"x":1}` {
		t.Errorf("block 0 input = %q, want {\\\"x\\\":1}", inputs[0])
	}
	if inputs[1] != `{"y":2}` {
		t.Errorf("block 1 input = %q, want {\\\"y\\\":2}", inputs[1])
	}
	if !strings.Contains(out, "call-a") || !strings.Contains(out, "call-b") {
		t.Errorf("both call ids must appear: %s", out)
	}
}

// TestStreamThreeParallelToolCallsStaySequential proves three parallel calls
// render as three sequential blocks in reservation order.
func TestStreamThreeParallelToolCallsStaySequential(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"fa\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"fb\",\"arguments\":\"{\\\"b\\\":2}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":2,\"id\":\"call-c\",\"type\":\"function\",\"function\":{\"name\":\"fc\",\"arguments\":\"{\\\"c\\\":3}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	order := assertBlocksSequential(t, out)
	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Fatalf("three calls must render sequentially in reservation order, got %v: %s", order, out)
	}
	inputs := toolCallInputs(anthropicEventsDetailed(t, out))
	wants := map[int]string{0: `{"a":1}`, 1: `{"b":2}`, 2: `{"c":3}`}
	for index, want := range wants {
		if inputs[index] != want {
			t.Errorf("block %d input = %q, want %q", index, inputs[index], want)
		}
	}
}

// TestStreamDeferredToolFragmentsReplayedInOrder proves a deferred call's
// fragments are buffered and replayed byte-identically in arrival order.
func TestStreamDeferredToolFragmentsReplayedInOrder(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"fa\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n"+
			// All of B's fragments arrive while A's block is still open.
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"fb\",\"arguments\":\"{\\\"b\\\":\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"\\\"two\\\"\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: [DONE]\n\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	order := assertBlocksSequential(t, out)
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Fatalf("blocks = %v: %s", order, out)
	}
	// Block 1's deltas must preserve the arrival order of B's fragments.
	var deltas []string
	for _, event := range anthropicEventsDetailed(t, out) {
		if event.Type == "input_json_delta" && event.Index == 1 {
			deltas = append(deltas, event.Data)
		}
	}
	wantDeltas := []string{`{"b":`, `"two"`, `}`}
	if len(deltas) != len(wantDeltas) {
		t.Fatalf("deferred deltas = %q, want %q", deltas, wantDeltas)
	}
	for i, want := range wantDeltas {
		if deltas[i] != want {
			t.Fatalf("deferred delta %d = %q, want %q (full %q)", i, deltas[i], want, deltas)
		}
	}
	if joined := strings.Join(deltas, ""); joined != `{"b":"two"}` {
		t.Fatalf("deferred input = %q", joined)
	}
}

// TestStreamTruncatedWhileToolBlockDeferredRejected proves the terminal gate
// still rejects a stream that ends while a tool block is deferred: an early
// seal (or a never-started deferred block) never hides a missing terminal.
func TestStreamTruncatedWhileToolBlockDeferredRejected(t *testing.T) {
	rec := streamChatUpstream(t, ChatCapabilities{},
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"fa\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n"+
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"fb\",\"arguments\":\"{\\\"b\\\":2}\"}}]}}]}\n\n")
	// The upstream ends without a finish chunk and without [DONE].
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("a stream ending with a deferred block must fail: %s", out)
	}
	if strings.Contains(out, "event: message_stop") {
		t.Fatalf("a truncated stream must not report a successful terminal: %s", out)
	}
}

// TestAnthropicDeferredToolCallsDrainOutOfOrder feeds the Anthropic adapter
// three parallel function-call items whose done events arrive in reverse order
// and proves the deferral cascades: every block opens and closes exactly once
// in reservation order, and no deferred or pending block is left behind.
func TestAnthropicDeferredToolCallsDrainOutOfOrder(t *testing.T) {
	state := anthropicLifecycleState(t)
	feedAnthropicCreated(t, state, 0)

	added := func(seq, index int64, itemID, callID, name string) {
		t.Helper()
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: seq,
			OutputIndex: index,
			Item: &ResponsesFunctionCallOutputItem{
				ID: itemID, Type: "function_call", Status: ResponsesItemInProgress,
				CallID: callID, Name: name, Arguments: "",
			},
		}); err != nil {
			t.Fatalf("added %s: %v", itemID, err)
		}
	}
	args := func(seq, index int64, itemID, delta string) {
		t.Helper()
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: seq,
			ItemID: itemID, OutputIndex: index, Delta: delta,
		}); err != nil {
			t.Fatalf("delta %s: %v", itemID, err)
		}
	}
	doneSeq := int64(6)
	done := func(index int64, itemID, callID, name, arguments string) []AnthropicStreamEvent {
		t.Helper()
		// The FSM requires the call's arguments to be done before the item's
		// done, exactly as the producer emits them.
		doneSeq++
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: doneSeq,
			ItemID: itemID, OutputIndex: index, Arguments: arguments,
		}); err != nil {
			t.Fatalf("arguments done %s: %v", itemID, err)
		}
		doneSeq++
		events, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: doneSeq,
			OutputIndex: index,
			Item: &ResponsesFunctionCallOutputItem{
				ID: itemID, Type: "function_call", Status: ResponsesItemCompleted,
				CallID: callID, Name: name, Arguments: arguments,
			},
		})
		if err != nil {
			t.Fatalf("done %s: %v", itemID, err)
		}
		return events
	}

	added(1, 0, "fc_a", "call_a", "fa") // A starts (no block open)
	added(2, 1, "fc_b", "call_b", "fb") // B defers behind A
	added(3, 2, "fc_c", "call_c", "fc") // C defers behind A
	args(4, 0, "fc_a", `{"a":1}`)
	args(5, 1, "fc_b", `{"b":2}`)
	args(6, 2, "fc_c", `{"c":3}`)
	// The done events arrive in reverse order: C, B, then A.
	done(2, "fc_c", "call_c", "fc", `{"c":3}`)
	done(1, "fc_b", "call_b", "fb", `{"b":2}`)
	cascade := done(0, "fc_a", "call_a", "fa", `{"a":1}`)

	var starts, stops []int
	inputs := map[int]string{}
	for _, event := range cascade {
		switch event.Type {
		case AnthropicStreamEventTypeContentBlockStart:
			starts = append(starts, *event.Index)
		case AnthropicStreamEventTypeContentBlockStop:
			stops = append(stops, *event.Index)
		case AnthropicStreamEventTypeContentBlockDelta:
			if event.Delta != nil && event.Delta.Type == AnthropicStreamDeltaTypeInputJSONDelta {
				inputs[*event.Index] += *event.Delta.PartialJSON
			}
		}
	}
	// A's stop plus B's and C's start/stop (both already done) drain together.
	if want := []int{1, 2}; !slices.Equal(starts, want) {
		t.Fatalf("cascade starts = %v, want %v", starts, want)
	}
	if want := []int{0, 1, 2}; !slices.Equal(stops, want) {
		t.Fatalf("cascade stops = %v, want %v", stops, want)
	}
	if inputs[1] != `{"b":2}` || inputs[2] != `{"c":3}` {
		t.Fatalf("cascade inputs = %v, want buffered arguments for B and C", inputs)
	}
	if len(state.deferredTools) != 0 || len(state.pendingToolStart) != 0 {
		t.Fatalf("deferred=%d pending=%d, want both empty after the cascade",
			len(state.deferredTools), len(state.pendingToolStart))
	}
}
