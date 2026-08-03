package transcode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/joeycumines/sesame/stream"
)

// Format identifies an API wire format supported by the transcode engine.
type Format string

// Supported wire formats.
const (
	// FormatResponses is the OpenAI Responses API.
	FormatResponses Format = "responses"
	// FormatChatCompletions is the OpenAI Chat Completions API.
	FormatChatCompletions Format = "chat-completions"
	// FormatMessages is the Anthropic Messages API.
	FormatMessages Format = "messages"
)

// SupportedFormatPair reports whether the client-to-upstream format pair has
// complete request, response, and streaming conversions. Unsupported pairs
// are rejected at configuration time.
func SupportedFormatPair(clientFormat, upstreamFormat Format) bool {
	switch clientFormat {
	case FormatResponses:
		return upstreamFormat == FormatChatCompletions
	case FormatMessages:
		return upstreamFormat == FormatResponses || upstreamFormat == FormatChatCompletions
	}
	return false
}

// HandlerConfig configures a TranscodeHandler.
type HandlerConfig struct {
	// ClientPath is the route path clients use, e.g. "/v1/responses".
	ClientPath string
	// UpstreamPath is the route path the upstream expects.
	UpstreamPath string
	// ClientFormat is the wire format of client payloads.
	ClientFormat Format
	// UpstreamFormat is the wire format of upstream payloads.
	UpstreamFormat Format
	// Upstream is the upstream base URL.
	Upstream *url.URL
	// MaxBodyBytes bounds the client request body read; zero means no limit.
	// The body must be buffered for conversion, so an unbounded read would be
	// an OOM vector for oversized payloads.
	MaxBodyBytes int64
}

// RoundTrip executes an outbound HTTP request through the proxy engine.
type RoundTrip func(*http.Request) (*http.Response, error)

// TranscodeHandler intercepts requests for a transcoded route, converts the
// payload to the upstream schema, forwards through the proxy engine, and
// converts the response back to the client schema. Streaming responses are
// translated incrementally through the stateful accumulators; the
// asymmetrical half-close lifecycle is managed by stream.Proxy.
type TranscodeHandler struct {
	cfg       HandlerConfig
	roundTrip RoundTrip
}

// NewTranscodeHandler returns a handler for the given configuration. The
// client-to-upstream format pair must be supported (see SupportedFormatPair).
func NewTranscodeHandler(cfg HandlerConfig, roundTrip RoundTrip) *TranscodeHandler {
	return &TranscodeHandler{cfg: cfg, roundTrip: roundTrip}
}

// ClientPath returns the client-facing route path of the handler.
func (h *TranscodeHandler) ClientPath() string { return h.cfg.ClientPath }

// ServeHTTP implements http.Handler.
func (h *TranscodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The request body must be buffered for conversion; bound the read so an
	// oversized payload cannot exhaust memory (the journal's capture limit
	// only bounds the journal slice, not the stream).
	bodyReader := io.Reader(r.Body)
	if h.cfg.MaxBodyBytes > 0 {
		bodyReader = io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone: abort rather than write an error the proxy
			// machinery would classify as a failure.
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if h.cfg.MaxBodyBytes > 0 && int64(len(body)) > h.cfg.MaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	upstreamBody, err := convertRequest(h.cfg.ClientFormat, h.cfg.UpstreamFormat, body)
	if err != nil {
		http.Error(w, "convert request: "+err.Error(), http.StatusBadRequest)
		return
	}
	outReq, err := h.upstreamRequest(r, upstreamBody)
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := h.roundTrip(outReq)
	if err != nil {
		// A RoundTripper may return a non-nil response alongside an error;
		// release its body so the connection is not leaked.
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		// A cancelled client context means the exchange is already over: abort
		// without a response. Aborting (http.ErrAbortHandler) also lets the
		// proxy classify this as a client cancel instead of an upstream
		// failure — a 502 here would trip the breaker and phantom penalty
		// for a failure that never happened (the passthrough path suppresses
		// the same case via its transport-error guards).
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "upstream request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Non-success responses pass through unchanged: provider error payloads
	// do not follow the API response schemas and must not be transcoded.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		copyResponse(w, resp)
		return
	}

	switch {
	case isEventStream(resp):
		h.streamResponse(w, r, resp)
	case isJSON(resp):
		h.jsonResponse(w, r, resp)
	default:
		// Non-JSON responses (e.g. provider error pages) pass through
		// unchanged.
		copyResponse(w, resp)
	}
}

// upstreamRequest builds the outbound request: the client headers, the
// rewritten upstream path, the converted body, and a recomputed
// Content-Length. The client context is carried so downstream cancellation
// aborts the upstream request.
func (h *TranscodeHandler) upstreamRequest(r *http.Request, body []byte) (*http.Request, error) {
	u := *r.URL
	u.Scheme = h.cfg.Upstream.Scheme
	u.Host = h.cfg.Upstream.Host
	// Join the configured upstream base path (e.g. "/api") with the mapped
	// route path, mirroring the reverse proxy's path concatenation. The error
	// is unreachable: the upstream URL was parsed at configuration time, so
	// its path cannot contain invalid escapes.
	u.Path, _ = url.JoinPath(h.cfg.Upstream.Path, h.cfg.UpstreamPath)
	u.RawPath = ""
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	outReq.Header = r.Header.Clone()
	outReq.Header.Set("Content-Type", "application/json")
	outReq.ContentLength = int64(len(body))
	// Sanitize the cloned client headers: hop-by-hop and connection-level
	// headers must not reach the upstream (mirroring the reverse proxy's
	// Director), and a client-requested Accept-Encoding would make the
	// upstream compress the response — the transport does not transparently
	// decode it when the request explicitly asks for it, which would break
	// JSON/SSE conversion. Content-Length is recomputed below, and forwarded
	// headers are stripped like the passthrough path.
	for _, h := range []string{
		"Accept-Encoding",
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	} {
		outReq.Header.Del(h)
	}
	return outReq, nil
}

// jsonResponse converts a non-streaming upstream response back to the client
// schema and writes it downstream.
func (h *TranscodeHandler) jsonResponse(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone: abort rather than write a 502 the proxy
			// machinery would classify as an upstream failure.
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "read upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	converted, err := convertResponse(h.cfg.ClientFormat, h.cfg.UpstreamFormat, body)
	if err != nil {
		http.Error(w, "convert response: "+err.Error(), http.StatusBadGateway)
		return
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(converted)))
	w.WriteHeader(resp.StatusCode)
	// A failed write is surfaced by the writer's flush/abort tracking; the
	// client is already gone in that case.
	_, _ = w.Write(converted)
}

// streamResponse translates an upstream SSE stream into client SSE events,
// delegating stream copying and asymmetrical half-close propagation to
// stream.Proxy, and flushing every event chunk immediately.
//
// stream.Proxy blocks until the response copy completes (the client request
// side reaches EOF instantly here, soft-closing the upstream write side while
// the response keeps flowing) or the context is cancelled, so when it returns
// the response delivery is finished; the upstream body is then closed to
// release the connection and unblock any in-flight read.
func (h *TranscodeHandler) streamResponse(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	converter := newFrameConverter(h.cfg.UpstreamFormat, h.cfg.ClientFormat)
	if converter == nil {
		http.Error(w, "unsupported stream conversion", http.StatusInternalServerError)
		return
	}
	// Preserve upstream entity headers (CORS, tracking metadata); the SSE
	// headers below intentionally override the copied content-type.
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	reader := &convertingReader{br: bufio.NewReader(resp.Body), conv: converter}
	local := &clientStream{body: r.Body, w: w}
	remote := &upstreamStream{read: reader}

	_ = stream.Proxy(r.Context(), local, remote)
	// Release the upstream connection; this also unblocks the converter's
	// in-flight read after a client-side cancellation.
	resp.Body.Close()
}

// convertingReader reads the upstream SSE stream, converts each data frame to
// client-format SSE frames, and serves them as a byte stream. It is read by a
// single goroutine (the stream proxy's response copy).
type convertingReader struct {
	br   *bufio.Reader
	conv frameConverter

	mu  sync.Mutex
	buf bytes.Buffer
}

// Read returns the next converted bytes.
func (r *convertingReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buf.Len() == 0 {
		err := r.fill()
		if r.buf.Len() == 0 && err != nil {
			return 0, err
		}
	}
	return r.buf.Read(p)
}

// fill converts upstream frames into the buffer until it holds data or the
// upstream stream ends. Malformed frames are skipped without terminating the
// stream. Every frame is written with an SSE event line so clients that
// dispatch on the event name (both the responses and anthropic SDKs do) can
// route frames correctly.
func (r *convertingReader) fill() error {
	for r.buf.Len() == 0 {
		frame, err := readSSEFrame(r.br)
		if err != nil {
			if err == io.EOF {
				// Flush any terminal frames the converter holds back until EOF.
				if terminal := r.conv.Terminal(); terminal != nil {
					for _, f := range terminal {
						writeSSEFrame(&r.buf, f)
					}
				}
			}
			return err
		}
		frames, err := r.conv.Convert(frame)
		if err != nil {
			// Conversion failures are treated as malformed frames: skip them
			// without dropping the stream.
			continue
		}
		for _, f := range frames {
			writeSSEFrame(&r.buf, f)
		}
	}
	return nil
}

// writeSSEFrame writes one SSE event: an event line carrying the payload's
// type (when present) followed by the data line and the blank-line terminator.
func writeSSEFrame(buf *bytes.Buffer, frame []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &envelope) == nil && envelope.Type != "" {
		buf.WriteString("event: ")
		buf.WriteString(envelope.Type)
		buf.WriteString("\n")
	}
	buf.WriteString("data: ")
	buf.Write(frame)
	buf.WriteString("\n\n")
}

// clientStream is the downstream side of the stream proxy: it reads the
// client request body (consumed by the conversion; immediately at EOF) and
// writes converted SSE frames to the response writer, flushing after every
// write.
type clientStream struct {
	body io.Reader
	w    http.ResponseWriter
}

func (s *clientStream) Read(p []byte) (int, error) { return s.body.Read(p) }

func (s *clientStream) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err == nil {
		if err := http.NewResponseController(s.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return n, err
		}
	}
	return n, err
}

// upstreamStream is the upstream side of the stream proxy: it serves the
// converted SSE frames and treats Close as the request-side half-close. The
// converted request body was already fully sent upstream, so closing the
// write side is a no-op that leaves the response stream open.
type upstreamStream struct {
	read io.Reader
}

func (s *upstreamStream) Read(p []byte) (int, error) { return s.read.Read(p) }

// Write is unused: the converted request body is fully sent before the
// response phase begins.
func (s *upstreamStream) Write(p []byte) (int, error) { return len(p), nil }

// Close implements the asymmetrical half-close: the request write side closes
// without interrupting the response read side.
func (s *upstreamStream) Close() error { return nil }

// frameConverter converts upstream data frames to client data frames.
type frameConverter interface {
	Convert(frame []byte) ([][]byte, error)
	// Terminal returns any final frames to emit when the upstream stream
	// ends without a further data frame. May return nil.
	Terminal() [][]byte
}

// newFrameConverter returns the converter for the upstream-to-client format
// direction, composing chained converters where no direct conversion exists.
func newFrameConverter(upstreamFormat, clientFormat Format) frameConverter {
	switch {
	case upstreamFormat == FormatChatCompletions && clientFormat == FormatResponses:
		return &chatResponsesFrameConverter{}
	case upstreamFormat == FormatResponses && clientFormat == FormatMessages:
		return &responsesAnthropicFrameConverter{}
	case upstreamFormat == FormatChatCompletions && clientFormat == FormatMessages:
		return &chatAnthropicFrameConverter{
			chat:      &chatResponsesFrameConverter{},
			anthropic: &responsesAnthropicFrameConverter{},
		}
	}
	return nil
}

// chatResponsesFrameConverter converts chat completions chunks into responses
// events.
type chatResponsesFrameConverter struct {
	state ChatResponsesStreamState
}

func (c *chatResponsesFrameConverter) Convert(frame []byte) ([][]byte, error) {
	if string(frame) == "[DONE]" {
		// [DONE] is the explicit terminal marker: release the held-back
		// terminal event now rather than waiting for the connection to close
		// (an upstream that keeps the connection open after [DONE] would
		// otherwise never deliver the final event).
		return c.Terminal(), nil
	}
	var chunk ChatStreamResponse
	if err := json.Unmarshal(frame, &chunk); err != nil {
		return nil, nil // malformed frames are skipped
	}
	return marshalResponsesEvents(c.state.ConvertChatResponseResponsesStreamResponse(&chunk)), nil
}

// Terminal emits the terminal event held back for a trailing usage chunk.
func (c *chatResponsesFrameConverter) Terminal() [][]byte {
	return marshalResponsesEvents(c.state.Terminal())
}

// responsesAnthropicFrameConverter converts responses events into anthropic
// events.
type responsesAnthropicFrameConverter struct {
	state AnthropicResponsesStreamState
}

func (c *responsesAnthropicFrameConverter) Convert(frame []byte) ([][]byte, error) {
	var event ResponsesStreamResponse
	if err := json.Unmarshal(frame, &event); err != nil {
		return nil, nil
	}
	return marshalAnthropicEvents(c.state.ConvertResponsesStreamResponseAnthropicStreamEvent(&event)), nil
}

func (c *responsesAnthropicFrameConverter) Terminal() [][]byte { return nil }

// chatAnthropicFrameConverter composes the chat-to-responses and
// responses-to-anthropic converters.
type chatAnthropicFrameConverter struct {
	chat      *chatResponsesFrameConverter
	anthropic *responsesAnthropicFrameConverter
}

func (c *chatAnthropicFrameConverter) Convert(frame []byte) ([][]byte, error) {
	responsesFrames, err := c.chat.Convert(frame)
	if err != nil || len(responsesFrames) == 0 {
		return nil, err
	}
	var out [][]byte
	for _, rf := range responsesFrames {
		anthropicFrames, err := c.anthropic.Convert(rf)
		if err != nil {
			return nil, err
		}
		out = append(out, anthropicFrames...)
	}
	return out, nil
}

// Terminal forwards the terminal responses events through the anthropic
// converter.
func (c *chatAnthropicFrameConverter) Terminal() [][]byte {
	responsesFrames := c.chat.Terminal()
	if len(responsesFrames) == 0 {
		return nil
	}
	var out [][]byte
	for _, rf := range responsesFrames {
		anthropicFrames, err := c.anthropic.Convert(rf)
		if err != nil {
			continue
		}
		out = append(out, anthropicFrames...)
	}
	return out
}

func marshalResponsesEvents(events []ResponsesStreamResponse) [][]byte {
	out := make([][]byte, 0, len(events))
	for i := range events {
		if b, err := json.Marshal(&events[i]); err == nil {
			out = append(out, b)
		}
	}
	return out
}

func marshalAnthropicEvents(events []AnthropicStreamEvent) [][]byte {
	out := make([][]byte, 0, len(events))
	for i := range events {
		if b, err := json.Marshal(&events[i]); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// convertRequest converts a client-format request body to the upstream
// format, composing conversions where no direct conversion exists.
func convertRequest(clientFormat, upstreamFormat Format, body []byte) ([]byte, error) {
	switch {
	case clientFormat == FormatResponses && upstreamFormat == FormatChatCompletions:
		var req ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return json.Marshal(ConvertResponsesRequestChatRequest(&req))
	case clientFormat == FormatMessages && upstreamFormat == FormatResponses:
		var req AnthropicMessageRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return json.Marshal(ConvertAnthropicRequestResponsesRequest(&req))
	case clientFormat == FormatMessages && upstreamFormat == FormatChatCompletions:
		responsesBody, err := convertRequest(FormatMessages, FormatResponses, body)
		if err != nil {
			return nil, err
		}
		return convertRequest(FormatResponses, FormatChatCompletions, responsesBody)
	}
	return nil, fmt.Errorf("unsupported request conversion %s -> %s", clientFormat, upstreamFormat)
}

// convertResponse converts an upstream-format response body to the client
// format, composing conversions where no direct conversion exists.
func convertResponse(clientFormat, upstreamFormat Format, body []byte) ([]byte, error) {
	switch {
	case clientFormat == FormatResponses && upstreamFormat == FormatChatCompletions:
		var resp ChatResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		return json.Marshal(ConvertChatResponseResponsesResponse(&resp))
	case clientFormat == FormatMessages && upstreamFormat == FormatResponses:
		var resp ResponsesResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		return json.Marshal(ConvertResponsesResponseAnthropicResponse(&resp))
	case clientFormat == FormatMessages && upstreamFormat == FormatChatCompletions:
		responsesBody, err := convertResponse(FormatResponses, FormatChatCompletions, body)
		if err != nil {
			return nil, err
		}
		return convertResponse(FormatMessages, FormatResponses, responsesBody)
	}
	return nil, fmt.Errorf("unsupported response conversion %s -> %s", clientFormat, upstreamFormat)
}

// isEventStream reports whether the response is an SSE stream.
func isEventStream(resp *http.Response) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type"))), "text/event-stream")
}

// isJSON reports whether the response body is JSON.
func isJSON(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return ct == "" || strings.Contains(ct, "json")
}

// copyResponseHeaders copies upstream response headers, dropping hop-by-hop
// and entity headers recomputed downstream. Content-Encoding and entity
// validators are dropped because the transcoded body is a new entity: the
// upstream encoding/validators describe the upstream body, not the converted
// one.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length", "Content-Type", "Content-Encoding", "Content-Md5", "Etag":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyResponse passes a non-JSON or non-success upstream response through
// unchanged, preserving entity headers such as Content-Type.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// A failed copy is surfaced by the writer's abort tracking; the client
	// is already gone in that case.
	_, _ = io.Copy(w, resp.Body)
}
