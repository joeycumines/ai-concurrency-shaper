package transcode

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// eofErrorConverter is a test frameConverter whose FinalizeEOF returns a fixed
// error, to drive the reader's EOF path with each error class.
type eofErrorConverter struct {
	finalErr error
}

func (c *eofErrorConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	return convertedBatch{}, nil
}

func (c *eofErrorConverter) ConversionReport() *ConversionReport { return &ConversionReport{} }

func (c *eofErrorConverter) FinalizeEOF() (convertedBatch, error) {
	return convertedBatch{}, c.finalErr
}

func (c *eofErrorConverter) ErrorEvent(err error) (frameEvent, bool) {
	return frameEvent{Type: "error", Data: []byte(`{"type":"error"}`)}, true
}

// TestEOFErrorClassificationKeepsConcreteTypes proves the reader's EOF path
// does not relabel a FinalizeEOF failure: only a genuine truncation matches
// errStreamTruncated, and every other typed failure keeps its concrete type for
// the outcome classifier and the client-facing error event.
func TestEOFErrorClassificationKeepsConcreteTypes(t *testing.T) {
	unsupported := &UnsupportedFeatureError{Protocol: "responses", Path: "usage", Feature: string(FeatureUsageUnknown)}
	unrepresentable := &UnrepresentableError{Protocol: "transcode", Path: "output[]", Detail: "args not an object"}
	arithmetic := &UsageArithmeticError{Detail: "input tokens: overflow"}
	wire := upstreamWireError(UpstreamResponses, 200, errors.New("envelope mismatch"))
	truncation := errStreamTruncated
	bound := &SSEBoundError{Bound: 1}

	cases := []struct {
		name string
		err  error
		// wantTruncation reports whether the error must match
		// errStreamTruncated. wantUpstream reports whether the outcome
		// classifier must call it an upstream failure; every other case is
		// a local conversion failure, never an upstream failure.
		wantTruncation bool
		wantUpstream   bool
		// check asserts the concrete type survived the reader's EOF path.
		check func(t *testing.T, err error)
	}{
		{
			name:           "unsupported feature keeps its type",
			err:            unsupported,
			wantTruncation: false,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
					t.Fatalf("err = %T %v, want *UnsupportedFeatureError preserved", err, err)
				}
			},
		},
		{
			name:           "unrepresentable keeps its type",
			err:            unrepresentable,
			wantTruncation: false,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
					t.Fatalf("err = %T %v, want *UnrepresentableError preserved", err, err)
				}
			},
		},
		{
			name:           "usage arithmetic keeps its type",
			err:            arithmetic,
			wantTruncation: false,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*UsageArithmeticError](err); !ok {
					t.Fatalf("err = %T %v, want *UsageArithmeticError preserved", err, err)
				}
			},
		},
		{
			name:           "upstream wire error keeps its type",
			err:            wire,
			wantTruncation: false,
			// *UpstreamWireError is corrupt upstream wire: the classifier
			// must call it an upstream failure, and the reader sets
			// SawUpstreamErrorFrame for it when it emits the error frame.
			wantUpstream: true,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
					t.Fatalf("err = %T %v, want *UpstreamWireError preserved", err, err)
				}
			},
		},
		{
			name:           "genuine truncation is marked as truncation",
			err:            truncation,
			wantTruncation: true,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, errStreamTruncated) {
					t.Fatalf("err = %v, want errors.Is(err, errStreamTruncated)", err)
				}
			},
		},
		{
			// The real shape the converters' finalizeEOF returns: the
			// sentinel wrapped in *UpstreamWireError. errors.Is must survive
			// the wrapper (UpstreamWireError.Unwrap exposes the cause), and
			// the truncation must NOT set SawUpstreamErrorFrame — a local
			// truncation is not a frame of real upstream data, so a
			// concurrent client cancel must not classify as a definitive
			// upstream failure.
			name:           "wrapped truncation is a truncation, not an upstream frame",
			err:            upstreamWireError(UpstreamResponses, 200, errStreamTruncated),
			wantTruncation: true,
			wantUpstream:   false,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
					t.Fatalf("err = %T %v, want *UpstreamWireError preserved", err, err)
				}
				if !errors.Is(err, errStreamTruncated) {
					t.Fatalf("err = %v, want errors.Is(err, errStreamTruncated) through the UpstreamWireError wrap", err)
				}
			},
		},
		{
			name: "delivery bound at EOF keeps its type and stays an upstream failure",
			err:  bound,
			// A generated bound is an upstream body/protocol failure by
			// design (isSSEBoundError in the classifier), so it must NOT be
			// relabeled a truncation, but it still classifies upstream.
			wantTruncation: false,
			wantUpstream:   true,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*SSEBoundError](err); !ok {
					t.Fatalf("err = %T %v, want *SSEBoundError preserved", err, err)
				}
				if errors.Is(err, errStreamTruncated) {
					t.Fatalf("err = %v, a delivery bound is not a truncation", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newConvertingReaderWithLimits(
				NewSSEReaderWithLimits(strings.NewReader(""), 0, 0),
				&eofErrorConverter{finalErr: tc.err},
				0, 0, 0,
			)
			_, err := io.ReadAll(reader)
			if err == nil {
				t.Fatal("Read: nil error, want the FinalizeEOF failure")
			}
			tc.check(t, err)
			if got := errors.Is(err, errStreamTruncated); got != tc.wantTruncation {
				t.Fatalf("errors.Is(err, errStreamTruncated) = %v, want %v (err = %T %v)", got, tc.wantTruncation, err, err)
			}
			// The outcome classifier must agree: a truncation or an
			// upstream body error is an upstream failure; a local typed
			// failure is a local conversion failure.
			// Build the observation the way the reader reports it, so the
			// test pins the flag-driven classifier against the real flags:
			// a truncation must not set SawUpstreamErrorFrame, an
			// *UpstreamWireError must.
			outcome := classifyStreamObservation(streamObservation{
				ReaderErr:             err,
				SawErrorEvent:         reader.SawErrorEvent(),
				SawTerminal:           reader.SawTerminal(),
				SawUpstreamErrorFrame: reader.SawUpstreamErrorFrame(),
			})
			if tc.wantTruncation && reader.SawUpstreamErrorFrame() {
				t.Fatalf("SawUpstreamErrorFrame = true for a truncation; a truncation is not a frame of real upstream data")
			}
			if _, ok := errors.AsType[*UpstreamWireError](tc.err); ok && !tc.wantTruncation && !reader.SawUpstreamErrorFrame() {
				t.Fatalf("SawUpstreamErrorFrame = false for *UpstreamWireError; corrupt upstream wire must mark the frame upstream")
			}
			if want := tc.wantTruncation || tc.wantUpstream; (outcome == streamOutcomeUpstreamFailure) != want {
				t.Fatalf("outcome = %v, want upstream failure = %v for err = %T %v", outcome, want, err, err)
			}
		})
	}
}

// TestGeminiToAnthropicEOFReleaseRefusalKeepsItsType drives a real composed
// converter to EOF: the Gemini finish chunk holds the terminal, EOF releases it
// through the Anthropic state, and the strict usage refusal must surface as
// *UnsupportedFeatureError, not as a truncation.
func TestGeminiToAnthropicEOFReleaseRefusalKeepsItsType(t *testing.T) {
	ctx := geminiStreamTestContext()
	gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil)
	// Strict on usage_unknown: the terminal envelope carries no usage, so the
	// Anthropic render's Lose(FeatureUsageUnknown) refuses.
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1710000000,
	)
	c := newGeminiToAnthropicConverter(gemini, anthropic)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(
			"data: "+`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`+"\n\n"+
				"data: "+geminiFinishChunk+"\n\n",
		), 0, 0),
		c, 0, 0, 0,
	)
	_, err := io.ReadAll(reader)
	if err == nil {
		t.Fatal("Read: nil error, want the usage refusal from the EOF terminal release")
	}
	if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
		t.Fatalf("Read: err = %T %v, want *UnsupportedFeatureError preserved at EOF", err, err)
	}
	if errors.Is(err, errStreamTruncated) {
		t.Fatalf("Read: err = %v, a policy refusal is not a truncation", err)
	}
	if outcome := classifyStreamObservation(streamObservation{ReaderErr: err, SawErrorEvent: true}); outcome == streamOutcomeUpstreamFailure {
		t.Fatalf("outcome = streamOutcomeUpstreamFailure, want a local conversion failure for the usage refusal")
	}
}
