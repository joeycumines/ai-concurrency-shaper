package transcode

// A stream converter call is all-or-nothing. A call that fails leaves no
// observable effect: the report it recorded into is rolled back, so no note
// describes an encoding that was never delivered. The one external write, a
// thought signature for the signature store, is queued during the call and
// written by commitCall only after the whole call succeeds. The sequence of a
// Responses error event is the count of frames actually delivered (see
// deliveryGate), so terminal events that were numbered but held, and never
// delivered, leave no gap.
//
// Delivery is part of the call. The stream reader installs its admission of
// generated frames and batches on every converter (deliveryGate), and each call
// admits its batch before it commits. A batch the reader would refuse therefore
// fails the call and rolls it back exactly like any other failure, so no
// signature is written and no note or sequence number survives for a batch that
// was never delivered.

// streamCallMark is the state a failed converter call restores.
type streamCallMark struct {
	report reportMark
}

// pendingSignature is a thought signature a call accepted for the signature
// store.
type pendingSignature struct {
	callID    string
	signature string
}

// beginCall marks the chat state before one converter call.
func (s *chatResponsesStreamState) beginCall() streamCallMark {
	return streamCallMark{report: s.report.mark()}
}

// rollbackCall restores the chat state to a mark taken before a failed call.
func (s *chatResponsesStreamState) rollbackCall(m streamCallMark) {
	s.report.restore(m.report)
}

// beginCall marks the Responses-to-Anthropic state before one converter call.
func (s *anthropicResponsesStreamState) beginCall() streamCallMark {
	return streamCallMark{report: s.report.mark()}
}

// rollbackCall restores the Responses-to-Anthropic state to a mark taken before
// a failed call.
func (s *anthropicResponsesStreamState) rollbackCall(m streamCallMark) {
	s.report.restore(m.report)
}

// beginCall marks the Gemini stream state before one converter call.
func (s *geminiResponsesStreamState) beginCall() streamCallMark {
	return streamCallMark{report: s.report.mark()}
}

// rollbackCall restores the Gemini stream state to a mark taken before a failed
// call and drops the signatures that call queued.
func (s *geminiResponsesStreamState) rollbackCall(m streamCallMark) {
	s.report.restore(m.report)
	s.pendingSignatures = nil
}

// commitCall writes the signatures the successful call queued to the store.
func (s *geminiResponsesStreamState) commitCall() {
	for _, pending := range s.pendingSignatures {
		s.ctx.GeminiSignatures.Put(s.ctx.GeminiSignatureScope, pending.callID, pending.signature)
	}
	s.pendingSignatures = nil
}

// beginCall marks the Gemini client state before one converter call.
func (s *geminiStreamClientState) beginCall() streamCallMark {
	return streamCallMark{report: s.report.mark()}
}

// rollbackCall restores the Gemini client state to a mark taken before a failed
// call.
func (s *geminiStreamClientState) rollbackCall(m streamCallMark) {
	s.report.restore(m.report)
}

// beginCall marks the upstream Messages state before one converter call.
func (s *anthropicUpstreamResponsesStreamState) beginCall() streamCallMark {
	return streamCallMark{report: s.report.mark()}
}

// rollbackCall restores the upstream Messages state to a mark taken before a
// failed call.
func (s *anthropicUpstreamResponsesStreamState) rollbackCall(m streamCallMark) {
	s.report.restore(m.report)
}

// deliveryAware is implemented by every converter. The stream reader installs
// its batch admission through it when it wraps the converter.
type deliveryAware interface {
	installDelivery(admit func(convertedBatch) ([]byte, error))
}

// deliveryGate is embedded in every converter. Its admission runs inside the
// call's transaction, after the batch is converted and before any external write
// commits; a refusal is a failure of the call. It also counts the frames a
// successful call delivers, which numbers the sequence of a later error event.
type deliveryGate struct {
	admit     func(convertedBatch) ([]byte, error)
	delivered int64
}

// installDelivery installs the admission of the reader that wraps the converter.
func (g *deliveryGate) installDelivery(admit func(convertedBatch) ([]byte, error)) {
	g.admit = admit
}

// gate admits one converted batch and counts its frames as delivered. A
// converter without an installed admission passes the batch through unadmitted,
// and the reader admits it on delivery.
func (g *deliveryGate) gate(batch convertedBatch) (convertedBatch, error) {
	if g.admit != nil {
		staged, err := g.admit(batch)
		if err != nil {
			return convertedBatch{}, err
		}
		batch.staged = staged
		batch.admitted = true
	}
	g.delivered += int64(len(batch.Events))
	return batch, nil
}

// nextDeliveredSequence returns the sequence number an error event takes: the
// number of frames delivered so far, since delivered Responses events are
// numbered contiguously from zero.
func (g *deliveryGate) nextDeliveredSequence() int64 {
	return g.delivered
}

// Every stream converter admits its batches inside its call. These assertions
// keep a converter from being added without the gate, which would let its
// refused batches commit.
var (
	_ deliveryAware = (*chatToResponsesConverter)(nil)
	_ deliveryAware = (*responsesToAnthropicConverter)(nil)
	_ deliveryAware = (*chatToAnthropicConverter)(nil)
	_ deliveryAware = (*geminiToResponsesConverter)(nil)
	_ deliveryAware = (*geminiToAnthropicConverter)(nil)
	_ deliveryAware = (*chatToGeminiConverter)(nil)
	_ deliveryAware = (*responsesToGeminiConverter)(nil)
	_ deliveryAware = (*messagesToGeminiConverter)(nil)
)
