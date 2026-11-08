package transcode

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestRefusedBatchLeavesNoEffect proves a batch the stream reader refuses at
// delivery leaves no effect: the thought signature the call accepted is never
// written to the store, and the call's report entries and Responses sequence
// are restored, exactly as for any other failed call.
func TestRefusedBatchLeavesNoEffect(t *testing.T) {
	ctx, store := geminiSignatureStoreContext()
	c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil))
	// The id-less function call records a call-id note and accepts its
	// signature. The generated batch bound is far below the call's frames, so
	// the reader refuses the batch after the conversion returned.
	body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"sig"}]},"index":0}]}`
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader("data: "+body+"\n\n"), 0, 0),
		c, 0, 256, 0,
	)
	_, err := io.ReadAll(reader)
	if _, ok := errors.AsType[*SSEBoundError](err); !ok {
		t.Fatalf("Read: err = %T %v, want the generated batch bound", err, err)
	}
	if n := store.Len(); n != 0 {
		t.Fatalf("signature store holds %d entries for a refused batch, want 0", n)
	}
	if report := c.ConversionReport(); len(report.Losses) != 0 || report.Dropped != 0 {
		t.Fatalf("report after a refused batch = %+v, want no entries", report)
	}
	frame, ok := c.ErrorEvent(err)
	if !ok {
		t.Fatal("ErrorEvent: no frame")
	}
	if seq := errorFrameSequence(t, frame); seq != 0 {
		t.Fatalf("error event sequence_number = %d, want 0: no event of the refused batch was delivered", seq)
	}
}

// TestRefusedTerminalAtEOFLeavesNoEffect proves a terminal batch refused at
// delivery rolls back the EOF release with its call: the missing-sentinel note
// and the Responses sequence the release advanced are restored, and the failure
// latches so no later call reports a clean terminal.
func TestRefusedTerminalAtEOFLeavesNoEffect(t *testing.T) {
	c := newChatToResponsesConverter(newChatResponsesStreamState(
		testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
	))
	refusal := &SSEBoundError{Bound: 1}
	c.installDelivery(func(batch convertedBatch) ([]byte, error) {
		if batch.Terminal {
			return nil, refusal
		}
		return nil, nil
	})
	delivered := 0
	batch, err := c.Convert(chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil))
	if err != nil {
		t.Fatalf("text delta: %v", err)
	}
	delivered += len(batch.Events)
	batch, err = c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop")))
	if err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	delivered += len(batch.Events)
	if _, err := c.FinalizeEOF(); !errors.Is(err, refusal) {
		t.Fatalf("FinalizeEOF: err = %v, want the refused terminal", err)
	}
	if reportHasFeature(*c.ConversionReport(), FeatureMissingStreamSentinel) {
		t.Fatal("the note of a refused EOF release remains in the report")
	}
	frame, ok := c.ErrorEvent(refusal)
	if !ok {
		t.Fatal("ErrorEvent: no frame")
	}
	if seq := errorFrameSequence(t, frame); seq != int64(delivered) {
		t.Fatalf("error event sequence_number = %d, want %d: the refused release advanced no delivered sequence", seq, delivered)
	}
	if _, err := c.FinalizeEOF(); !errors.Is(err, refusal) {
		t.Fatalf("FinalizeEOF after the refusal: err = %v, want the latched refusal", err)
	}
}

// TestDeliveryBoundAtEOFKeepsItsType proves a generated bound refused while the
// reader delivers the EOF batch surfaces as the bound itself, not as a
// truncation, so the outcome classifier and the client see the real cause.
func TestDeliveryBoundAtEOFKeepsItsType(t *testing.T) {
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(""), 0, 0),
		&fixedConverter{err: &SSEBoundError{Bound: 1}},
		0, 0, 0,
	)
	_, err := io.ReadAll(reader)
	if _, ok := errors.AsType[*SSEBoundError](err); !ok {
		t.Fatalf("Read: err = %T %v, want the generated bound at EOF", err, err)
	}
	if errors.Is(err, errStreamTruncated) {
		t.Fatalf("Read: err = %v, a delivery bound is not a truncation", err)
	}
}
