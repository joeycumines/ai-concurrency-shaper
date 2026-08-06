package transcode

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/joeycumines/sesame/stream"
)

// Required copy primitive and lifecycle contract:
// https://github.com/joeycumines/sesame/blob/v0.1.1/stream/proxy.go
//
// SSE contracts:
// https://platform.claude.com/docs/en/build-with-claude/streaming
// https://platform.openai.com/docs/api-reference/responses-streaming

// streamLocal is the local side of stream.Proxy: an explicit EOF request
// source (the converted request body was already submitted by RoundTrip) and
// the sealed downstream writer.
type streamLocal struct {
	// The converted request body was already sent by RoundTrip. Use an
	// explicit EOF source rather than reusing an exhausted client body.
	requestEOF io.Reader
	response   io.Writer
}

func (s *streamLocal) Read(p []byte) (int, error) {
	return s.requestEOF.Read(p)
}

func (s *streamLocal) Write(p []byte) (int, error) {
	return s.response.Write(p)
}

// streamRemote is the remote side of stream.Proxy: the translated upstream
// response reader. No request bytes remain to be sent during response
// translation.
type streamRemote struct {
	response io.Reader
}

func (s *streamRemote) Read(p []byte) (int, error) {
	return s.response.Read(p)
}

// No request bytes remain to be sent during response translation.
func (s *streamRemote) Write(p []byte) (int, error) {
	return len(p), nil
}

// This is the request-side soft close required by the stream.Proxy adapter.
// The actual HTTP response body is closed explicitly by the handler after the
// copy boundary returns.
func (s *streamRemote) Close() error {
	return nil
}

// streamObservation is the complete set of signals used to classify the
// stream outcome. It is gathered after the copy boundary returns and the
// writer is sealed.
type streamObservation struct {
	ReaderErr error
	WriterErr error
	CopyErr   error
	SealErr   error
	CloseErr  error

	ClientContextErr error
	SawTerminal      bool
	SawErrorEvent    bool

	// SawUpstreamErrorFrame reports whether the error event originated from
	// real upstream data (a converted upstream error frame) rather than a
	// local truncation or conversion error.
	SawUpstreamErrorFrame bool
}

// runTranslatedStream runs the mandated stream.Proxy copy boundary exactly
// once and then performs the handler-owned shutdown sequence: atomically stop
// new downstream writes, cancel the child context, close the upstream body,
// and seal the writer so no further ResponseWriter operation is possible
// after return.
func runTranslatedStream(
	parent context.Context,
	dst http.ResponseWriter,
	upstreamBody io.ReadCloser,
	convertedReader *convertingReader,
) streamObservation {
	copyCtx, cancel := context.WithCancel(parent)
	defer cancel()

	writer := newSealedSSEWriter(dst)

	local := &streamLocal{
		requestEOF: http.NoBody,
		response:   writer,
	}
	remote := &streamRemote{
		response: convertedReader,
	}

	// stream.Proxy remains the required copy/cancellation primitive.
	copyErr := stream.Proxy(copyCtx, local, remote)

	// First prevent any new downstream operation, then terminate upstream work,
	// then wait for an operation already inside Write/Flush to leave.
	writer.StopAccepting()
	cancel()
	closeErr := upstreamBody.Close()
	sealErr := writer.Seal()

	return streamObservation{
		ReaderErr:             convertedReader.Err(),
		WriterErr:             writer.firstErr,
		CopyErr:               copyErr,
		SealErr:               sealErr,
		CloseErr:              closeErr,
		ClientContextErr:      parent.Err(),
		SawTerminal:           convertedReader.SawTerminal(),
		SawErrorEvent:         convertedReader.SawErrorEvent(),
		SawUpstreamErrorFrame: convertedReader.SawUpstreamErrorFrame(),
	}
}

// streamOutcome classifies the stream into one of the outcome buckets the
// breaker and metrics rely on.
type streamOutcome uint8

const (
	streamOutcomeSuccess streamOutcome = iota
	streamOutcomeClientAbort
	streamOutcomeUpstreamFailure
	streamOutcomeLocalConversionFailure
	streamOutcomeDownstreamFailure
)

func (o streamOutcome) String() string {
	switch o {
	case streamOutcomeSuccess:
		return "success"
	case streamOutcomeClientAbort:
		return "client_abort"
	case streamOutcomeUpstreamFailure:
		return "upstream_failure"
	case streamOutcomeLocalConversionFailure:
		return "local_conversion_failure"
	case streamOutcomeDownstreamFailure:
		return "downstream_failure"
	default:
		return "unknown"
	}
}

// classifyStreamObservation maps the observation to an outcome using explicit
// provenance, never status-code guessing. A client abort is neither success
// nor failure; a local conversion failure is not an upstream failure.
//
// A client cancellation that interrupts an incomplete exchange (upstream
// body cut off, local truncation error event, downstream write failure, or
// copy abort) is a client abort, not an upstream failure: the client's
// disconnect caused the interruption. Only a genuine upstream error frame —
// real upstream data — is a definitive upstream failure regardless of a
// concurrent cancellation.
func classifyStreamObservation(o streamObservation) streamOutcome {
	// A genuine upstream error event (converted upstream data) is a
	// definitive upstream outcome, independent of any concurrent client
	// cancellation.
	if o.SawUpstreamErrorFrame {
		return streamOutcomeUpstreamFailure
	}
	// A locally generated error event (truncation or conversion error) is
	// an upstream failure only when the client did not cancel; an aborted
	// exchange must not trip or reset breaker health.
	if o.SawErrorEvent {
		if o.ClientContextErr != nil {
			return streamOutcomeClientAbort
		}
		return streamOutcomeUpstreamFailure
	}
	if o.WriterErr != nil || o.SealErr != nil {
		if o.ClientContextErr != nil {
			return streamOutcomeClientAbort
		}
		return streamOutcomeDownstreamFailure
	}
	if o.ReaderErr != nil && !errors.Is(o.ReaderErr, io.EOF) {
		if o.ClientContextErr != nil {
			return streamOutcomeClientAbort
		}
		return streamOutcomeUpstreamFailure
	}
	if o.CopyErr != nil {
		if o.ClientContextErr != nil {
			return streamOutcomeClientAbort
		}
		return streamOutcomeUpstreamFailure
	}
	if !o.SawTerminal {
		return streamOutcomeLocalConversionFailure
	}
	return streamOutcomeSuccess
}
