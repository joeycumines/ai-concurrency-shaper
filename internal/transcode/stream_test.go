package transcode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// fixedConverter is a test frameConverter that emits a fixed frame and
// reports a configurable terminal.
type fixedConverter struct {
	terminal bool
	err      error
}

func (c *fixedConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	return convertedBatch{
		Events:   []frameEvent{{Type: "response.completed", Data: []byte(`{"type":"response.completed"}`)}},
		Terminal: c.terminal,
	}, c.err
}

func (c *fixedConverter) ConversionReport() *ConversionReport { return &ConversionReport{} }

func (c *fixedConverter) FinalizeEOF() (convertedBatch, error) {
	return convertedBatch{}, c.err
}

func (c *fixedConverter) ErrorEvent(err error) (frameEvent, bool) {
	return frameEvent{Type: "error", Data: []byte(`{"type":"error","message":"conversion failed"}`)}, true
}

func TestConvertingReaderDrainsTerminal(t *testing.T) {
	source := NewSSEReaderWithLimits(strings.NewReader("data: {\"x\":1}\n\n"), 0, 0)
	converter := &fixedConverter{terminal: true}
	reader := newConvertingReaderWithLimits(source, converter, 0, 0, 0)

	// The first Read returns the frame bytes.
	buf := make([]byte, 4096)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "response.completed") {
		t.Fatalf("data = %q", buf[:n])
	}
	// The next Read returns io.EOF because stopAfterDrain is set.
	_, err = reader.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if !reader.SawTerminal() {
		t.Fatal("SawTerminal not set")
	}
}

func TestConvertingReaderStopAfterDrainEvenWithOpenUpstream(t *testing.T) {
	// After the terminal batch drains, the reader returns EOF even when the
	// upstream keeps the connection open (an endless stream of bytes).
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(&thenHangReader{data: []byte("data: {}\n\n")}, 0, 0),
		&fixedConverter{terminal: true}, 0, 0, 0,
	)
	buf := make([]byte, 4096)
	if _, err := reader.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF despite open upstream", err)
	}
}

// thenHangReader yields data once, then blocks forever like an upstream that
// keeps a connection open.
type thenHangReader struct {
	data []byte
	sent bool
}

func (r *thenHangReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	time.Sleep(time.Hour)
	return 0, nil
}

func TestConvertingReaderConversionError(t *testing.T) {
	source := NewSSEReaderWithLimits(strings.NewReader("data: {}\n\n"), 0, 0)
	converter := &fixedConverter{err: errors.New("convert failed")}
	reader := newConvertingReaderWithLimits(source, converter, 0, 0, 0)
	var output bytes.Buffer
	buf := make([]byte, 64)
	var readErr error
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			readErr = err
			break
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
	if readErr == nil || !strings.Contains(readErr.Error(), "convert failed") {
		t.Fatalf("err = %v", readErr)
	}
}

func TestConvertingReaderMalformedFrame(t *testing.T) {
	// Malformed JSON in a chat stream frame is a conversion error that the
	// reader surfaces (it cannot silently become success). The error event
	// frame is drained before the error.
	converter := newChatToResponsesConverter(newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	))
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader("data: not-json\n\n"), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	var readErr error
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			readErr = err
			break
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
	if readErr == nil {
		t.Fatal("expected malformed frame error")
	}
	if !strings.Contains(output.String(), "event: error") {
		t.Fatalf("missing error event: %q", output.String())
	}
}

func TestConvertingReaderEOFWithoutTerminal(t *testing.T) {
	// EOF before any terminal is a truncation error, never success. The
	// client-dialect error event frame is drained before the error surfaces.
	// The fixture is a pin-conformant empty chunk (no finish_reason) so the
	// stream really exercises the EOF-truncation path, not a decode
	// rejection.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(
			"data: {\"id\":\"x\", \"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[]}\n\n"), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	var readErr error
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			readErr = err
			break
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		t.Fatalf("err = %v, want truncation error (not clean EOF)", readErr)
	}
	if !strings.Contains(output.String(), "event: error") {
		t.Fatalf("missing error event: %q", output.String())
	}
	if !reader.SawErrorEvent() {
		t.Fatal("SawErrorEvent not set")
	}
	if reader.SawTerminal() {
		t.Fatal("truncated stream must not report a success terminal")
	}
}

func TestConvertingReaderDoneReleasesTerminal(t *testing.T) {
	// A chat stream: content chunk, finish chunk, then [DONE]. The [DONE]
	// releases the held terminal and stops the reader.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	input := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	reader := newConvertingReaderWithLimits(NewSSEReaderWithLimits(strings.NewReader(input), 0, 0), converter, 0, 0, 0)

	var output bytes.Buffer
	// The upstream never EOFs (simulated by the [DONE] frame terminating the
	// reader).
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !strings.Contains(output.String(), "response.completed") {
		t.Fatalf("missing terminal in output: %q", output.String())
	}
	if !reader.SawTerminal() {
		t.Fatal("SawTerminal not set")
	}
}

// guardResponseWriter counts ResponseWriter operations after a return marker,
// mirroring the returnGuardWriter used in the fuzz harness.
type guardResponseWriter struct {
	mu sync.Mutex

	header   http.Header
	status   int
	body     bytes.Buffer
	returned bool
	lateOps  int
	flushes  int
}

func (w *guardResponseWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *guardResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.returned {
		w.lateOps++
		return
	}
	if w.status == 0 {
		w.status = status
	}
}

func (w *guardResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.returned {
		w.lateOps++
		return 0, io.ErrClosedPipe
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *guardResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.returned {
		w.lateOps++
		return
	}
	w.flushes++
}

func TestSealedSSEWriterWritesCompleteEvents(t *testing.T) {
	var w guardResponseWriter
	writer := newSealedSSEWriter(&w)

	// Two complete frames in one Write.
	_, err := writer.Write([]byte("data: {\"a\":1}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = writer.Write([]byte("data: {\"b\":2}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A partial frame stays pending.
	if _, err := writer.Write([]byte("data: {\"c\":")); err != nil {
		t.Fatal(err)
	}
	// Seal with pending bytes returns errTrailingSSEBytes.
	if err := writer.Seal(); !errors.Is(err, errTrailingSSEBytes) {
		t.Fatalf("seal err = %v", err)
	}
	// Complete the pending frame and seal clean.
	_, _ = writer.Write([]byte("3}\n\n"))
	if err := writer.Seal(); err != nil {
		t.Fatalf("seal = %v", err)
	}
}

func TestSealedSSEWriterRejectsLateWrites(t *testing.T) {
	var w guardResponseWriter
	writer := newSealedSSEWriter(&w)
	writer.StopAccepting()
	if _, err := writer.Write([]byte("data: x\n\n")); !errors.Is(err, errStreamWriterSealed) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunTranslatedStreamNoLateOps(t *testing.T) {
	// The full stream path: a chat stream terminating in [DONE], translated
	// through runTranslatedStream with a guard writer. After the handler
	// returns, no ResponseWriter operation may occur.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	input := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	// The upstream body keeps the connection open after [DONE]; the reader
	// terminates on the terminal batch and the handler closes the body.
	upstream := &heldOpenBody{reader: strings.NewReader(input)}
	reader := newConvertingReaderWithLimits(NewSSEReaderWithLimits(upstream, 0, 0), converter, 0, 0, 0)

	var w guardResponseWriter
	observation := runTranslatedStream(context.Background(), &w, upstream, reader)

	// The stream translated successfully.
	if observation.SawTerminal != true {
		t.Fatalf("SawTerminal = %v", observation.SawTerminal)
	}
	if !strings.Contains(w.bodyString(), "response.completed") {
		t.Fatalf("body = %q", w.bodyString())
	}
	if upstream.closed != true {
		t.Fatal("upstream body not closed")
	}

	// No late operations after return.
	time.Sleep(10 * time.Millisecond)
	if w.lateOpCount() != 0 {
		t.Fatalf("late operations = %d", w.lateOpCount())
	}
}

// heldOpenBody simulates an upstream response body that would remain open
// after [DONE]; the handler must close it.
type heldOpenBody struct {
	reader *strings.Reader
	closed bool
	mu     sync.Mutex
}

func (b *heldOpenBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *heldOpenBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func TestClassifyStreamObservation(t *testing.T) {
	tests := []struct {
		name string
		obs  streamObservation
		want streamOutcome
	}{
		{
			name: "success with terminal",
			obs:  streamObservation{SawTerminal: true},
			want: streamOutcomeSuccess,
		},
		{
			name: "no terminal is local conversion failure",
			obs:  streamObservation{},
			want: streamOutcomeLocalConversionFailure,
		},
		{
			name: "error event from truncation is upstream failure",
			obs:  streamObservation{SawErrorEvent: true, ReaderErr: fmt.Errorf("%w: boom", errStreamTruncated), SawTerminal: true},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "error event from oversized line is upstream failure",
			obs:  streamObservation{SawErrorEvent: true, ReaderErr: &SSEBoundError{Line: true, Bound: 1024}, SawTerminal: true},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "error event from upstream body read failure is upstream failure",
			obs:  streamObservation{SawErrorEvent: true, UpstreamBodyError: true, ReaderErr: io.ErrUnexpectedEOF, SawTerminal: true},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "error event from typed upstream conversion error is upstream failure",
			obs: streamObservation{
				SawErrorEvent:         true,
				SawUpstreamErrorFrame: true,
				ReaderErr: &StreamConversionError{
					Cause:      errors.New("chat stream chunk: boom"),
					Provenance: ProvenanceUpstreamBodyError,
					Status:     200,
				},
				SawTerminal: true,
			},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "error event from oversized frame is upstream failure",
			obs:  streamObservation{SawErrorEvent: true, ReaderErr: &SSEBoundError{Line: false, Bound: 1024}, SawTerminal: true},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "error event from local conversion is local failure",
			obs:  streamObservation{SawErrorEvent: true, ReaderErr: errors.New("convert: boom"), SawTerminal: true},
			want: streamOutcomeLocalConversionFailure,
		},
		{
			name: "reader error is upstream failure",
			obs:  streamObservation{ReaderErr: errors.New("truncated"), SawTerminal: true},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "copy error with client abort",
			obs:  streamObservation{CopyErr: context.Canceled, ClientContextErr: context.Canceled, SawTerminal: true},
			want: streamOutcomeClientAbort,
		},
		{
			name: "copy error without client abort is upstream failure",
			obs:  streamObservation{CopyErr: errors.New("io"), SawTerminal: true},
			want: streamOutcomeUpstreamFailure,
		},
		{
			name: "writer error with client abort",
			obs:  streamObservation{WriterErr: errors.New("closed"), ClientContextErr: context.Canceled, SawTerminal: true},
			want: streamOutcomeClientAbort,
		},
		{
			name: "writer error without client abort is downstream failure",
			obs:  streamObservation{WriterErr: errors.New("closed"), SawTerminal: true},
			want: streamOutcomeDownstreamFailure,
		},
		{
			name: "seal error is downstream failure",
			obs:  streamObservation{SealErr: errTrailingSSEBytes, SawTerminal: true},
			want: streamOutcomeDownstreamFailure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStreamObservation(tt.obs)
			if got != tt.want {
				t.Fatalf("classify = %v (%s), want %v", got, got, tt.want)
			}
		})
	}
}

func TestRunTranslatedStreamClientAbortReleases(t *testing.T) {
	// A client that cancels mid-stream must release the upstream body and
	// produce a client-abort outcome with no late writer operations.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	upstream := &heldOpenBody{reader: strings.NewReader(
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
	)}
	reader := newConvertingReaderWithLimits(NewSSEReaderWithLimits(upstream, 0, 0), converter, 0, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	var w guardResponseWriter
	done := make(chan struct{})
	var observation streamObservation
	go func() {
		defer close(done)
		observation = runTranslatedStream(ctx, &w, upstream, reader)
	}()

	// Wait for the first flush, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for w.flushCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runTranslatedStream did not return after cancellation")
	}

	if observation.SawTerminal {
		t.Fatal("aborted stream must not report a terminal")
	}
	if upstream.closed != true {
		t.Fatal("upstream body not closed after abort")
	}
	time.Sleep(10 * time.Millisecond)
	if w.lateOpCount() != 0 {
		t.Fatalf("late ops = %d", w.lateOpCount())
	}
}

// flushCount returns the guarded flush count.
func (w *guardResponseWriter) flushCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushes
}

// lateOpCount returns the guarded late-operation count.
func (w *guardResponseWriter) lateOpCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lateOps
}

// bodyString returns the guarded body contents.
func (w *guardResponseWriter) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func TestFixtureChatStreamToResponsesFrames(t *testing.T) {
	// End-to-end through the adapter: the official-shaped chat stream fixture
	// translates to Responses frames.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE()), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !reader.SawTerminal() {
		t.Fatal("no terminal seen")
	}
	if !strings.Contains(output.String(), "response.completed") {
		t.Fatalf("missing terminal: %q", output.String())
	}
	if !strings.Contains(output.String(), "sequence_number") {
		t.Fatalf("missing sequence numbers: %q", output.String())
	}
}

func TestWriteDialectEventNameMatchesType(t *testing.T) {
	// Every frame emitted by the chat adapter must have event name == JSON
	// type (the manual conformance validator checks this).
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE()), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	eventReader := bufio.NewReader(bytes.NewReader(output.Bytes()))
	for {
		event, err := readSSEEvent(eventReader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := validateEventNameMatchesJSONType(event); err != nil {
			t.Fatalf("event %q: %v", event.Event, err)
		}
	}
}

func TestFixtureResponsesStreamToAnthropicFrames(t *testing.T) {
	// End-to-end through the adapter: the official-shaped Responses stream
	// fixture translates to Anthropic frames.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"gpt-4.1",
		1,
	)
	converter := newResponsesToAnthropicConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(bytes.NewReader(testcorpus.ResponsesStreamSSE()), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !reader.SawTerminal() {
		t.Fatal("no terminal seen")
	}
	body := output.String()
	if !strings.Contains(body, "message_start") {
		t.Fatalf("missing message_start: %q", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("missing message_stop: %q", body)
	}
	// Thinking BLOCKS must never be synthesized. The usage field
	// thinking_tokens is a legitimate Anthropic accounting field and is
	// allowed.
	if strings.Contains(body, `"type":"thinking"`) || strings.Contains(body, `"type":"redacted_thinking"`) {
		t.Fatalf("thinking blocks must never be synthesized: %q", body)
	}
	if strings.Contains(body, `"type":"thinking_delta"`) {
		t.Fatalf("thinking deltas must never be synthesized: %q", body)
	}
	// No terminal and error event both present is impossible.
	if strings.Contains(body, "\"type\":\"error\"") {
		t.Fatalf("unexpected error event: %q", body)
	}
}

// TestChatStreamChunkTypedErrorFrame proves an in-band Chat error frame is a
// typed upstream conversion error carrying provenance and the upstream
// status, and that it flows through the converting reader as a definitive
// upstream outcome (review-j finding 11).
func TestChatStreamChunkTypedErrorFrame(t *testing.T) {
	_, err := chatStreamChunkFromSSE(SSEEvent{
		Data: []byte(`{"error":{"message":"boom"}}`),
	})
	var convErr *StreamConversionError
	if !errors.As(err, &convErr) {
		t.Fatalf("error = %T %v, want *StreamConversionError", err, err)
	}
	if convErr.Provenance != ProvenanceUpstreamBodyError {
		t.Fatalf("provenance = %v, want upstream_body_error", convErr.Provenance)
	}
	if convErr.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", convErr.Status)
	}

	// An error frame without a message falls back to a stable description.
	_, err = chatStreamChunkFromSSE(SSEEvent{
		Data: []byte(`{"error":{}}`),
	})
	if !errors.As(err, &convErr) {
		t.Fatalf("error = %T %v, want *StreamConversionError", err, err)
	}
	if !strings.Contains(err.Error(), "chat stream error frame") {
		t.Fatalf("error = %q, want the fallback description", err.Error())
	}

	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(
			strings.NewReader("data: {\"error\":{\"message\":\"boom\"}}\n\n"), 0, 0),
		converter, 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			break
		}
	}
	if !reader.SawUpstreamErrorFrame() {
		t.Fatal("in-band error frame not marked as upstream")
	}
	if !strings.Contains(output.String(), `"type":"error"`) {
		t.Fatalf("client error event missing: %q", output.String())
	}
	if !strings.Contains(output.String(), "boom") {
		t.Fatalf("client error event must carry the upstream message: %q", output.String())
	}
}

// errReader returns a fixed error after n bytes.
type errReader struct {
	data  string
	pos   int
	errAt int
	err   error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestConvertingReaderUpstreamBodyError proves a raw non-EOF upstream body
// read failure is marked as an upstream body error (never a local conversion
// failure) and the client still receives the dialect error event (review-j
// finding 1: a stream that fails while its body is read).
func TestConvertingReaderUpstreamBodyError(t *testing.T) {
	source := &errReader{
		data:  "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		errAt: 100,
		err:   io.ErrUnexpectedEOF,
	}
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(source, 0, 0),
		newChatToResponsesConverter(state), 0, 0, 0,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !reader.UpstreamBodyError() {
		t.Fatal("upstream body error not marked")
	}
	if !strings.Contains(output.String(), `"type":"error"`) {
		t.Fatalf("client must receive an error event: %q", output.String())
	}
}
