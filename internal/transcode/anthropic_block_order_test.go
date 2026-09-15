package transcode

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
