package transcode

// J1 regression tests (review-k finding 1, blocker):
//
//   - response.output_item.added is a DETACHED snapshot of the message as it
//     exists at creation (item.content == [] on the first ordinary text
//     delta), never the live item that subsequent events mutate;
//   - a premature [DONE] — before any finish_reason — terminates the
//     exchange IMMEDIATELY at the sentinel with the typed upstream
//     truncation error, so the reader never hangs on an upstream that keeps
//     the HTTP response open after [DONE] and the exchange classifies as an
//     upstream body failure.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// drainReader reads the converting reader to its terminal error or io.EOF
// and returns the output bytes and the final error.
func drainReader(t *testing.T, reader *convertingReader) (string, error) {
	t.Helper()
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			return output.String(), err
		}
		if n == 0 {
			t.Fatal("reader made no progress")
		}
	}
}

// TestChatStreamOutputItemAddedIsCreationSnapshot proves the first ordinary
// text delta emits the full trace — output_item.added with item.content == []
// (a snapshot of creation-time state), then content_part.added, then
// output_text.delta — and the terminal envelope later carries the FULL
// accumulated text (review-k finding 1).
func TestChatStreamOutputItemAddedIsCreationSnapshot(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)

	// First text delta: created, in_progress, output_item.added,
	// content_part.added, output_text.delta.
	batch, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 5 {
		t.Fatalf("first batch = %d frames, want 5", len(batch.Events))
	}
	for i, wantType := range []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
	} {
		if batch.Events[i].Type != wantType {
			t.Fatalf("frame %d type = %q, want %q", i, batch.Events[i].Type, wantType)
		}
	}

	addedEvent, ok := mustDecodeResponsesEvent(t, batch.Events[2].Data).(ResponseOutputItemAddedEvent)
	if !ok {
		t.Fatalf("added frame = %T", mustDecodeResponsesEvent(t, batch.Events[2].Data))
	}
	message, ok := addedEvent.Item.(*ResponsesOutputMessage)
	if !ok {
		t.Fatalf("added item = %T", addedEvent.Item)
	}
	if len(message.Content) != 0 {
		t.Fatalf("output_item.added content = %d parts, want 0 (creation snapshot): %+v",
			len(message.Content), message.Content)
	}
	if message.Status != ResponsesItemInProgress || message.Role != "assistant" {
		t.Fatalf("added message = %+v", message)
	}

	partEvent := mustDecodeResponsesEvent(t, batch.Events[3].Data)
	partAddedEvent, ok := partEvent.(ResponseContentPartAddedEvent)
	if !ok || partAddedEvent.Part == nil {
		t.Fatalf("content_part.added = %T %+v", partEvent, partEvent)
	}

	deltaEvent := mustDecodeResponsesEvent(t, batch.Events[4].Data)
	textDelta, ok := deltaEvent.(ResponseTextDeltaEvent)
	if !ok || textDelta.Delta != "Hello" {
		t.Fatalf("delta = %T %+v", deltaEvent, deltaEvent)
	}

	// The second delta continues the same part.
	if _, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
	)}); err != nil {
		t.Fatal(err)
	}

	// Finish, then [DONE]: the terminal envelope carries the FULL text.
	if _, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)}); err != nil {
		t.Fatal(err)
	}
	terminalBatch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatal(err)
	}
	if !terminalBatch.Terminal {
		t.Fatal("[DONE] batch is not terminal")
	}
	var completed *ResponseCompletedEvent
	for _, frame := range terminalBatch.Events {
		if candidate, ok := mustDecodeResponsesEvent(t, frame.Data).(ResponseCompletedEvent); ok {
			completed = &candidate
		}
	}
	if completed == nil {
		t.Fatal("no response.completed in the terminal batch")
	}
	if len(completed.Response.Output) != 1 {
		t.Fatalf("terminal output = %d items, want 1", len(completed.Response.Output))
	}
	terminalMessage, ok := completed.Response.Output[0].(*ResponsesOutputMessage)
	if !ok {
		t.Fatalf("terminal output[0] = %T", completed.Response.Output[0])
	}
	if len(terminalMessage.Content) != 1 {
		t.Fatalf("terminal content = %d parts, want 1", len(terminalMessage.Content))
	}
	text, ok := terminalMessage.Content[0].(*ResponsesOutputText)
	if !ok || text.Text != "Hello world" {
		t.Fatalf("terminal text = %+v, want the full accumulation", terminalMessage.Content[0])
	}
}

// TestChatToResponsesConverterPropagatesStateError proves a chunk the state
// machine rejects (choice index != 0) propagates through the adapter
// untouched, never swallowed or converted into an empty batch.
func TestChatToResponsesConverterPropagatesStateError(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	_, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":1,"delta":{"content":"x"},"finish_reason":null}]}`,
	)})
	if err == nil {
		t.Fatal("choice index 1 accepted")
	}
	if !strings.Contains(err.Error(), "choice index = 1") {
		t.Fatalf("err = %v", err)
	}
}

// mustDecodeResponsesEvent decodes one frame payload into the typed event
// union.
func mustDecodeResponsesEvent(t *testing.T, data []byte) ResponsesSSEEvent {
	t.Helper()
	event, err := decodeResponsesSSEEvent(data)
	if err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return event
}

// TestChatStreamPrematureDoneImmediateEOF proves a [DONE] before any
// finish_reason, followed by upstream EOF, terminates the exchange at the
// sentinel: the typed upstream truncation error is returned, the client
// receives the dialect error event, and no success terminal is emitted
// (review-k finding 1).
func TestChatStreamPrematureDoneImmediateEOF(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(
			"data: {\"id\":\"c\", \"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"+
				"data: [DONE]\n\n"), 0, 0),
		converter, 0, 0, 0,
	)

	output, readErr := drainReader(t, reader)
	assertPrematureDoneResult(t, reader, output, readErr)
}

// assertPrematureDoneResult asserts the common premature-[DONE] invariants:
// the typed upstream truncation error, an emitted client error event, no
// success terminal, and upstream-error-frame provenance.
func assertPrematureDoneResult(
	t *testing.T,
	reader *convertingReader,
	output string,
	readErr error,
) {
	t.Helper()
	if readErr == nil || errors.Is(readErr, io.EOF) {
		t.Fatalf("read err = %v, want the premature-[DONE] typed error", readErr)
	}
	var wireErr *UpstreamWireError
	if !errors.As(readErr, &wireErr) {
		t.Fatalf("read err = %T %v, want *UpstreamWireError", readErr, readErr)
	}
	if wireErr.Protocol != UpstreamChatCompletions {
		t.Fatalf("protocol = %v, want chat completions", wireErr.Protocol)
	}
	if wireErr.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", wireErr.Status)
	}
	if !strings.Contains(wireErr.Error(), "chat stream [DONE] before a terminal condition") {
		t.Fatalf("error = %q", wireErr.Error())
	}
	if !reader.SawUpstreamErrorFrame() {
		t.Fatal("premature [DONE] not marked as an upstream error frame")
	}
	if reader.SawTerminal() {
		t.Fatal("premature [DONE] must not report a success terminal")
	}
	if !strings.Contains(output, "event: error") || !strings.Contains(output, `"type":"error"`) {
		t.Fatalf("client error event missing: %q", output)
	}
	if !strings.Contains(output, "chat stream [DONE] before a terminal condition") {
		t.Fatalf("error event must carry the sentinel message: %q", output)
	}
	if strings.Contains(output, "response.completed") {
		t.Fatalf("premature [DONE] must not emit a success terminal: %q", output)
	}
}

// TestChatStreamPrematureDoneHeldOpen proves the reader stops at the [DONE]
// sentinel even when the upstream keeps the connection open after it: no hang
// (bounded by the test deadline), same typed upstream error and error event
// (review-k finding 1).
func TestChatStreamPrematureDoneHeldOpen(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(&thenHangReader{data: []byte(
			"data: {\"id\":\"c\", \"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
				"data: [DONE]\n\n")}, 0, 0),
		converter, 0, 0, 0,
	)

	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := drainReader(t, reader)
		done <- result{output: output, err: err}
	}()
	select {
	case res := <-done:
		assertPrematureDoneResult(t, reader, res.output, res.err)
	case <-time.After(3 * time.Second):
		t.Fatal("reader hung after [DONE] with the upstream connection held open")
	}
}

// TestChatStreamComposedPrematureDone proves the composed Chat→Anthropic
// direction: a [DONE] before any finish_reason returns the typed upstream
// truncation error and the reader emits the Anthropic error frame and stops —
// never an empty non-terminal batch (review-k finding 1).
func TestChatStreamComposedPrematureDone(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1710000000,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)

	// One content delta, then [DONE] before any finish_reason.
	batch, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) == 0 {
		t.Fatal("no frames from the content chunk")
	}
	if _, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")}); err == nil {
		t.Fatal("[DONE] before a terminal condition accepted")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		if wireErr.Protocol != UpstreamChatCompletions {
			t.Fatalf("protocol = %v, want chat completions", wireErr.Protocol)
		}
	}

	// Through the reader: the client receives the Anthropic error frame and
	// the exchange classifies as an upstream failure.
	chat2 := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	anthropic2 := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1710000000,
	)
	converter2 := newChatToAnthropicConverter(chat2, anthropic2)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(
			"data: {\"id\":\"c\", \"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"+
				"data: [DONE]\n\n"), 0, 0),
		converter2, 0, 0, 0,
	)

	output, readErr := drainReader(t, reader)
	if readErr == nil || errors.Is(readErr, io.EOF) {
		t.Fatalf("read err = %v, want the premature-[DONE] typed error", readErr)
	}
	var wireErr *UpstreamWireError
	if !errors.As(readErr, &wireErr) || wireErr.Protocol != UpstreamChatCompletions {
		t.Fatalf("read err = %T %v, want the typed upstream wire error", readErr, readErr)
	}
	if !reader.SawUpstreamErrorFrame() {
		t.Fatal("premature [DONE] not marked as an upstream error frame")
	}
	if reader.SawTerminal() {
		t.Fatal("premature [DONE] must not report a success terminal")
	}
	if !strings.Contains(output, `"type":"error"`) {
		t.Fatalf("Anthropic error frame missing: %q", output)
	}
	if strings.Contains(output, "message_stop") {
		t.Fatalf("premature [DONE] must not emit a success terminal: %q", output)
	}
}
