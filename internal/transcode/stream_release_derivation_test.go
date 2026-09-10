package transcode

// The release-bound derivation is closed on
// every axis the round-3 gate falsified — the per-event framing overhead
// times the event budget, the 6x-escaped request echo (bounded at
// decode), and tool-call identity rendered into the terminal envelope
// (charged against the exchange accumulated total).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseReplayReader replays a fixed sequence of raw SSE frames without
// materializing the whole stream: each frame is served from the same backing
// slice, so a million-delta source costs O(frames) pointers, not O(bytes).
type sseReplayReader struct {
	frames   [][]byte
	frameIdx int
	offset   int
}

func (r *sseReplayReader) Read(p []byte) (int, error) {
	for r.frameIdx < len(r.frames) {
		frame := r.frames[r.frameIdx]
		if r.offset < len(frame) {
			n := copy(p, frame[r.offset:])
			r.offset += n
			return n, nil
		}
		r.frameIdx++
		r.offset = 0
	}
	return 0, io.EOF
}

// TestChatStreamFramingOverheadCompletesRelease pins the full-event-budget
// release:
// a legal stream of single-byte deltas is bounded by the EVENT budget, and
// its generated total is dominated by per-event framing overhead (~221 bytes
// per chat→responses text-delta frame, measured), not payload escaping. The
// pre-round-4 generated total (terminal batch + 6x semantics + 1 MiB) died
// at ~69% of the event budget with SSEBoundError; the derivation now carries
// maxStreamTotalEvents x maxStreamPerEventFramingBytes, so an exchange at
// the full event budget completes its [DONE] release.
func TestChatStreamFramingOverheadCompletesRelease(t *testing.T) {
	const deltas = int(maxStreamTotalEvents) - 2
	deltaFrame := []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
	finishFrame := []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	doneFrame := []byte("data: [DONE]\n\n")

	frames := make([][]byte, 0, deltas+2)
	for range deltas {
		frames = append(frames, deltaFrame)
	}
	frames = append(frames, finishFrame, doneFrame)

	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	source := NewSSEReaderWithLimits(&sseReplayReader{frames: frames}, 0, 0)
	reader := newConvertingReaderWithLimits(source, newChatToResponsesConverter(state), 0, 0, 0)

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("full-event-budget exchange must complete its release: %v", err)
	}
	if !reader.sawTerminal {
		t.Fatal("exchange did not reach its terminal")
	}
	if reader.sawErrorEvent {
		t.Fatal("exchange emitted an error event")
	}
}

// TestResponsesRequestEchoCapped pins the echo cap: the request
// echo is bounded at decode (maxStreamEchoBytes, fail-closed 413 resource
// limit) because it re-marshals into every generated envelope frame at up to
// 6x JSON escaping. A '<'-heavy echo above the bound is rejected at decode —
// accepted by the pre-round-4 code (raw body well under AcceptedRequestBytes),
// which then failed the first upstream frame.
func TestResponsesRequestEchoCapped(t *testing.T) {
	t.Run("escaping-heavy instructions above the bound rejected", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","instructions":%q}`,
			strings.Repeat("<", maxStreamEchoBytes+1),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("escaping-heavy metadata above the bound rejected", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","metadata":{"k":%q}}`,
			strings.Repeat("<", maxStreamEchoBytes),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("escaping-heavy tools above the bound rejected", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","tools":[{"type":"function","name":"f","strict":true,"description":%q}]}`,
			strings.Repeat("<", maxStreamEchoBytes),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("echo under the bound accepted", func(t *testing.T) {
		// The measurement is the SERIALIZED echo (json.Marshal with the
		// envelope's HTML escaping), so the under-bound payload must not
		// escape: 'x' renders 1x. The '<'-heavy cases above already pin the
		// escaping amplification.
		body := fmt.Sprintf(
			`{"model":"m","input":"x","instructions":%q}`,
			strings.Repeat("x", maxStreamEchoBytes-512),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if err != nil {
			t.Fatalf("echo under the bound must be accepted: %v", err)
		}
	})
	t.Run("escaping-heavy user above the bound rejected", func(t *testing.T) {
		// The rendered members beyond instructions are measured too.
		// round 5): a '<'-heavy user field renders at 6x into every envelope.
		body := fmt.Sprintf(
			`{"model":"m","input":"x","user":%q}`,
			strings.Repeat("<", maxStreamEchoBytes),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("ordinary request unaffected", func(t *testing.T) {
		_, _, err := DecodeResponsesRequest(
			[]byte(`{"model":"m","input":"x","instructions":"be concise"}`),
			StrictLossPolicy(),
		)
		if err != nil {
			t.Fatalf("ordinary request rejected: %v", err)
		}
	})
}

// TestChatStreamMaximalEchoReleases pins the release half of the echo bound:
// a maximal accepted echo (exactly maxStreamEchoBytes of '<', rendering at
// 6x = 24 MiB) rides every generated envelope frame — created, in_progress,
// and the terminal envelope — and the release must succeed within the frame
// and batch bounds derived from the echo term. The exchange is driven
// through the converting reader so the marshaled terminal envelope is
// checked against the real frame/batch bounds. The echo carries the pinned
// defaults (tool_choice auto) exactly as DecodeResponsesRequest builds it.
func TestChatStreamMaximalEchoReleases(t *testing.T) {
	auto := "auto"
	echo := &ResponsesRequestEcho{
		Instructions: &ResponsesInput{Text: new(strings.Repeat("<", maxStreamEchoBytes))},
		ToolChoice:   ResponsesToolChoice{Str: &auto},
	}
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		echo,
	)
	finishFrame := []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	source := NewSSEReaderWithLimits(&sseReplayReader{frames: [][]byte{finishFrame, []byte("data: [DONE]\n\n")}}, 0, 0)
	reader := newConvertingReaderWithLimits(source, newChatToResponsesConverter(state), 0, 0, 0)

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("maximal-echo terminal release failed: %v", err)
	}
	if !reader.sawTerminal {
		t.Fatal("exchange did not reach its terminal")
	}
	if reader.sawErrorEvent {
		t.Fatal("exchange emitted an error event")
	}
}

// TestResponsesAnthropicToolIdentityCharged pins the round-5 gate finding:
// the DIRECT Responses→Anthropic direction charges tool-call identity (call
// id, function name) against the exchange accumulated total, exactly like
// the chat direction — identity renders into the generated tool_use block
// start and the terminal reconciliation, so uncharged identity would defeat
// the generated-total derivation.
func TestResponsesAnthropicToolIdentityCharged(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{},
		"msg_1",
		"m",
		1,
	)
	converter := newResponsesToAnthropicConverter(state)
	feed := func(eventType, data string) error {
		_, err := converter.Convert(SSEEvent{Event: eventType, Data: []byte(data)})
		return err
	}
	if err := feed("response.created",
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`); err != nil {
		t.Fatal(err)
	}

	bigID := strings.Repeat("i", maxStreamAccumulatedBytes)
	bigName := strings.Repeat("n", maxStreamAccumulatedBytes)
	if err := feed("response.output_item.added", fmt.Sprintf(
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":%q,"name":%q,"arguments":""}}`,
		bigID, bigName,
	)); err != nil {
		t.Fatalf("identity at the per-item scale must be accepted: %v", err)
	}
	if state.totalAccumulated != int64(2*len(bigID)) {
		t.Fatalf("totalAccumulated = %d, want %d (identity charged)", state.totalAccumulated, 2*len(bigID))
	}

	// A third tool call's identity exceeds the exchange accumulated total:
	// two items charge 2x2 MiB = 4 MiB (exactly the bound, admitted by the
	// strict >), and the third pushes past it — rejected as corrupt upstream
	// wire, never silently accumulated.
	for i := 2; i <= 3; i++ {
		err := feed("response.output_item.added", fmt.Sprintf(
			`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":{"id":"fc_%d","type":"function_call","status":"in_progress","call_id":%q,"name":%q,"arguments":""}}`,
			i, i-1, i, bigID, bigName,
		))
		if i == 2 {
			if err != nil {
				t.Fatalf("second item at the exact exchange bound must be accepted: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("identity beyond the exchange total must be rejected (round 5)")
		}
		if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
			t.Fatalf("err = %T %v, want UpstreamWireError", err, err)
		}
	}
}

// TestChatStreamToolIdentityCharged pins the tool-identity charge: tool-call identity
// (call id, function name) renders into the terminal envelope and the done
// events, so it is charged against the exchange accumulated total. Identity
// bytes beyond the exchange total are corrupt upstream wire; a repeated id
// is charged once.
func TestChatStreamToolIdentityCharged(t *testing.T) {
	t.Run("identity beyond the exchange total rejected", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1710000000,
			nil,
		)
		bigID := strings.Repeat("i", maxStreamAccumulatedBytes)
		for i := range 4 {
			call := ChatToolCallDelta{ID: new(fmt.Sprintf("%s-%d", bigID[:maxStreamAccumulatedBytes-4], i))}
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{call}}, nil)); err != nil {
				t.Fatalf("fragment %d rejected before the exchange total: %v", i+1, err)
			}
		}
		fifth := ChatToolCallDelta{ID: new(bigID)}
		_, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{fifth}}, nil))
		if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
			t.Fatalf("err = %T %v, want UpstreamWireError for identity beyond the exchange total", err, err)
		}
	})
	t.Run("repeated identity charged once", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1710000000,
			nil,
		)
		bigID := strings.Repeat("i", maxStreamAccumulatedBytes)
		call := ChatToolCallDelta{ID: new(bigID)}
		for range 4 {
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{call}}, nil)); err != nil {
				t.Fatalf("repeated id fragment rejected: %v", err)
			}
		}
		if state.totalAccumulated != int64(len(bigID)) {
			t.Fatalf("totalAccumulated = %d, want %d (charged once)", state.totalAccumulated, len(bigID))
		}
	})
}

// TestEchoCapLiveHandler413 pins the round-5 gate finding: the echo cap
// rejects through the LIVE handler as a 413 for both a streaming and a
// non-streaming Responses request (the decode hook runs before stream
// negotiation, so the classification must not depend on the stream intent).
func TestEchoCapLiveHandler413(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","stream":%t,"instructions":%q}`,
			stream, strings.Repeat("<", maxStreamEchoBytes+1),
		)
		mapping := responsesMapping(t)
		mapping.ModelMap = ModelMap{AllowIdentity: true}
		handler := NewTranscodeHandler(
			HandlerConfig{Mapping: mapping, Upstream: mustParseURL(t, "https://upstream.example")},
			func(req *http.Request) (*http.Response, error) {
				t.Fatal("round trip must not be reached for an oversized echo")
				return nil, nil
			},
			nil,
		)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("stream=%t: status = %d body=%q, want 413", stream, rec.Code, rec.Body.String())
		}
	}
}

// TestPerEventFramingChargeCoversAllShapes pins the 256-byte per-event
// framing charge BY CONSTRUCTION:
// every generated non-terminal frame shape from both stream converters is
// re-marshaled with the worst-case large sequence number and long identity
// strings, and its FIXED overhead (frame bytes minus payload bytes) must fit
// maxStreamPerEventFramingBytes. An under-count would let the exchange
// generated total fire before the event budget — the
// regression — for streams of legal tiny deltas with long identities.
func TestPerEventFramingChargeCoversAllShapes(t *testing.T) {
	// maxStreamPerEventFramingBytes is the fixed overhead each generated
	// frame may contribute to the exchange generated total. The
	// measurement: (full frame bytes incl. SSE framing) minus (payload
	// bytes), re-marshaled with maximal-width numbers and identities.
	const charge = int(maxStreamPerEventFramingBytes)

	type shape struct {
		name    string
		typ     string
		payload string // the JSON data of the frame, as generated
	}
	var shapes []shape

	measure := func(name string, events []ResponsesSSEEvent) {
		t.Helper()
		builder := &ResponsesEventBuilder{}
		for i := range events {
			// Re-stamp worst-case widths: a maximal int64 sequence number
			// and long item/identity strings are legal wire values, so the
			// fixed overhead must cover them.
			events[i] = restampForFramingProbe(t, events[i], builder)
			data, err := MarshalResponsesEvent(events[i])
			if err != nil {
				t.Fatalf("%s: marshal: %v", name, err)
			}
			shapes = append(shapes, shape{name: name + ":" + events[i].EventType(), typ: events[i].EventType(), payload: string(data)})
		}
	}

	// --- chat -> responses: every delta shape ---
	chatState := newChatResponsesStreamState(
		testStreamContext(), StrictLossPolicy(), ChatCapabilities{},
		"resp_1", "gpt-4.1", 1710000000, nil,
	)
	roleChunk := chatChunk(t, ChatStreamDelta{Role: new("assistant"), Content: new("H")}, nil)
	events, err := chatState.Convert(roleChunk)
	if err != nil {
		t.Fatal(err)
	}
	measure("chat→responses", events)
	// Plain text delta (the dominant shape of a legal tiny-delta stream).
	events, err = chatState.Convert(chatChunk(t, ChatStreamDelta{Content: new("x")}, nil))
	if err != nil {
		t.Fatal(err)
	}
	measure("chat→responses", events)
	// Tool-call delta (identity rendered per event).
	events, err = chatState.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{{
		Index:    new(0),
		ID:       new("call_worst_case_identity_string"),
		Type:     new("function"),
		Function: ChatToolCallFunction{Name: new("fn_worst_case_name_string"), Arguments: "{}"},
	}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	measure("chat→responses", events)
	// Finish reason chunk.
	events, err = chatState.Convert(chatChunk(t, ChatStreamDelta{}, new("stop")))
	if err != nil {
		t.Fatal(err)
	}
	measure("chat→responses", events)
	// Terminal event (usage envelope) is measured for information but
	// excluded from the charge assertion: the terminal batch carries its
	// own bound (GeneratedSSEBatchBytes) and the derivation adds it
	// separately.
	terminalEvents, err := chatState.Convert(ChatStreamResponse{
		ID: "chatcmpl-1", Object: "chat.completion.chunk", Created: 1710000000, Model: "gpt-4.1",
		Choices: []ChatChoice{},
		Usage: &ChatLLMUsage{
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
			PromptTokensDetails:     &ChatPromptTokensDetails{CachedTokens: 0},
			CompletionTokensDetails: &ChatCompletionTokensDetails{ReasoningTokens: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	measure("chat→responses-terminal", terminalEvents)

	// --- responses -> anthropic: every render shape ---
	anthropicState := newAnthropicResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1", "claude-x", 1710000000,
	)
	created := ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}
	convEvents, err := anthropicState.Convert(created)
	if err != nil {
		t.Fatal(err)
	}
	var anthropicFrames []AnthropicStreamEvent
	anthropicFrames = append(anthropicFrames, convEvents...)
	part := &ResponsesSummaryTextPart{Type: "summary_text", Text: ""}
	for _, ev := range []ResponsesSSEEvent{
		ResponseOutputItemAddedEvent{Type: "response.output_item.added", SequenceNumber: 1, OutputIndex: 0, Item: &ResponsesOutputMessage{ID: "msg_worst_case_identity_string", Type: "message", Role: "assistant", Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{}}},
		ResponseContentPartAddedEvent{Type: "response.content_part.added", SequenceNumber: 2, ItemID: "msg_worst_case_identity_string", OutputIndex: 0, ContentIndex: 0, Part: &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}},
		ResponseTextDeltaEvent{Type: "response.output_text.delta", SequenceNumber: 3, ItemID: "msg_worst_case_identity_string", OutputIndex: 0, ContentIndex: 0, Delta: "x", Logprobs: []ResponsesTextLogprob{}},
		ResponseReasoningSummaryPartAddedEvent{Type: "response.reasoning_summary_part.added", SequenceNumber: 4, ItemID: "rs_worst_case_identity_string", OutputIndex: 1, SummaryIndex: 0, Part: *part},
		ResponseReasoningSummaryTextDeltaEvent{Type: "response.reasoning_summary_text.delta", SequenceNumber: 5, ItemID: "rs_worst_case_identity_string", OutputIndex: 1, SummaryIndex: 0, Delta: "x"},
		ResponseReasoningSummaryTextDoneEvent{Type: "response.reasoning_summary_text.done", SequenceNumber: 6, ItemID: "rs_worst_case_identity_string", OutputIndex: 1, SummaryIndex: 0, Text: "x"},
		ResponseReasoningSummaryPartDoneEvent{Type: "response.reasoning_summary_part.done", SequenceNumber: 7, ItemID: "rs_worst_case_identity_string", OutputIndex: 1, SummaryIndex: 0, Part: *part},
	} {
		out, err := anthropicState.Convert(ev)
		if err != nil {
			t.Fatalf("%T: %v", ev, err)
		}
		anthropicFrames = append(anthropicFrames, out...)
	}
	for _, ev := range anthropicFrames {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		shapes = append(shapes, shape{name: "responses→anthropic:" + string(ev.Type), typ: string(ev.Type), payload: string(data)})
	}

	// Measure every non-terminal shape: fixed overhead = full SSE frame
	// minus payload, with worst-case widths.
	worst := 0
	worstName := ""
	for _, s := range shapes {
		full := len("event: ") + len(s.typ) + len("\ndata: ") + len(s.payload) + len("\n\n")
		overhead := full - len(s.payload)
		// writeFrameBytesBounded reserves len(Type)+16 on top of data; the
		// charge covers the same fixed cost.
		if s.typ == "response.completed" || s.typ == "response.failed" || s.typ == "response.incomplete" || s.typ == "message_stop" {
			continue
		}
		if overhead > worst {
			worst = overhead
			worstName = s.name
		}
	}
	t.Logf("worst non-terminal fixed overhead = %d bytes (%s), charge = %d", worst, worstName, charge)
	if worst > charge {
		t.Fatalf("frame shape %s has fixed overhead %d > charge %d: the generated total can fire before the event budget", worstName, worst, charge)
	}
}

// restampForFramingProbe replaces an event's sequence number and item
// identity strings with worst-case widths so the measured overhead covers
// legal maximal frames.
func restampForFramingProbe(t *testing.T, event ResponsesSSEEvent, builder *ResponsesEventBuilder) ResponsesSSEEvent {
	t.Helper()
	switch e := event.(type) {
	case ResponseCreatedEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseInProgressEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseOutputItemAddedEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseContentPartAddedEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseTextDeltaEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseTextDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseContentPartDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseOutputItemDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseFunctionCallArgumentsDeltaEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseFunctionCallArgumentsDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseRefusalDeltaEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseRefusalDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseReasoningSummaryPartAddedEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseReasoningSummaryTextDeltaEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseReasoningSummaryTextDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseReasoningSummaryPartDoneEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	case ResponseCompletedEvent:
		e.SequenceNumber = int64(math.MaxInt64)
		return e
	default:
		return event
	}
}
