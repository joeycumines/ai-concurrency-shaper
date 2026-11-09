package transcode

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// pendingToolCall buffers Chat tool-call fragments until call ID and function
// name are both known: do not emit an incomplete
// function_call output_item.added and hope a later delta supplies identity.
type pendingToolCall struct {
	itemID      string
	outputIndex int64
	callID      string
	name        string

	// pending holds argument fragments not yet emitted as deltas.
	pending strings.Builder
	// complete accumulates every fragment for the final .done arguments.
	complete strings.Builder
	started  bool
}

// convertToolCall buffers or emits one Chat tool-call fragment. Fragments are
// keyed by their Chat index; when the id arrives, an id alias is registered
// so later id-addressed fragments resolve to the same pending call.
func (s *chatResponsesStreamState) convertToolCall(
	call ChatToolCallDelta,
) ([]ResponsesSSEEvent, error) {
	var events []ResponsesSSEEvent

	// Legacy function_call fragments carry a synthesized id derived from
	// the chunk id: record the synthesis once per stream as an ungated
	// note naming the source.
	if call.ID != nil && strings.Contains(*call.ID, ":legacy-function-call-") && !s.legacyFunctionCallNoted {
		if err := s.report.Note(
			FeatureLegacyFunctionCall,
			"choices[].delta.function_call",
			"legacy function_call mapped to one tool call with id synthesized from the chunk id ("+*call.ID+")",
		); err != nil {
			return nil, err
		}
		s.legacyFunctionCallNoted = true
	}

	// A fragment carrying the id of a call that was already sealed inline
	// cannot merge back: the sealed call's block closed on the wire, so any
	// resolution would fork a duplicate call id into the terminal envelope
	// (either by creating a new pending entry or by an unstarted pending
	// call adopting the id through its fragment index). Reject it as corrupt
	// upstream wire.
	if call.ID != nil && *call.ID != "" {
		for _, sealed := range s.closedCalls {
			if sealed.callID == *call.ID {
				return nil, s.wireError(fmt.Errorf(
					"chat tool call fragment reuses the id %q of a sealed call",
					*call.ID,
				))
			}
		}
	}

	// Resolve the pending call: by id alias first (an id makes the fragment
	// attributable even without an index), then by fragment index.
	var pending *pendingToolCall
	ok := false
	if call.ID != nil && *call.ID != "" {
		for _, existing := range s.pendingCalls {
			if existing.callID == *call.ID {
				pending = existing
				ok = true
				break
			}
		}
	}
	if !ok && call.Index != nil {
		pending, ok = s.pendingCalls[*call.Index]
		if ok && call.ID != nil && *call.ID != "" &&
			pending.callID != "" && pending.callID != *call.ID {
			return nil, s.wireError(fmt.Errorf(
				"chat tool call fragment index %d carries id %q but the pending call is %q",
				*call.Index,
				*call.ID,
				pending.callID,
			))
		}
	}
	if !ok && call.Index == nil && call.ID == nil {
		// An index-less, id-less continuation fragment is attributable only
		// when exactly one pending call exists; with several, the fragment
		// is ambiguous and must not merge into an unrelated call.
		switch len(s.pendingCalls) {
		case 0:
			// First fragment: create below.
		case 1:
			for _, existing := range s.pendingCalls {
				pending = existing
			}
			ok = true
		default:
			return nil, s.wireError(errors.New(
				"chat tool call fragment without index or id is ambiguous",
			))
		}
	}
	if !ok {
		// New pending call. An index-less fragment that arrives alongside
		// existing calls gets a synthetic negative key so it cannot collide
		// with a real fragment index.
		if len(s.pendingCalls) >= maxStreamToolCalls {
			return nil, s.wireError(fmt.Errorf(
				"chat stream tool calls exceed the exchange bound of %d",
				maxStreamToolCalls,
			))
		}
		index := 0
		if call.Index != nil {
			index = *call.Index
		} else {
			index = -1
			for {
				if _, taken := s.pendingCalls[index]; !taken {
					break
				}
				index--
			}
		}
		pending = &pendingToolCall{
			outputIndex: s.itemIndex,
		}
		s.itemIndex++
		s.pendingCalls[index] = pending
	} else if call.Index != nil {
		// The fragment resolved by id (or the single-pending fallback) but
		// also carries an index: the index must resolve to the same pending
		// call, never silently select another.
		if other, exists := s.pendingCalls[*call.Index]; exists && other != pending {
			return nil, s.wireError(fmt.Errorf(
				"chat tool call fragment index %d resolves to call %q but the fragment id %q resolves to call %q",
				*call.Index,
				other.callID,
				derefStr(call.ID),
				pending.callID,
			))
		}
	}

	// Identity immutability: once the output_item.added
	// event was announced, a later fragment changing the call NAME is corrupt
	// upstream wire — the client already saw the announced identity and the
	// done events must never drift from it. A conflicting id cannot reach
	// this point: id resolution matches exactly, and an index resolution
	// whose stored id differs is rejected above.
	if pending.started && call.Function.Name != nil && *call.Function.Name != "" &&
		pending.name != *call.Function.Name {
		return nil, s.wireError(fmt.Errorf(
			"chat tool call name changed from %q to %q after the added event",
			pending.name,
			*call.Function.Name,
		))
	}

	// Tool-call identity (call id, function name) renders into the terminal
	// envelope and the done events, so it is semantic state: charge new
	// identity bytes against the exchange accumulated total exactly like
	// text/refusal/arguments (an unbounded
	// identity would defeat the release-bound derivation; repeated
	// fragments carrying the same id are charged once).
	if call.ID != nil && *call.ID != "" && pending.callID != *call.ID {
		if err := s.chargeIdentity(len(*call.ID)); err != nil {
			return nil, err
		}
		pending.callID = *call.ID
	}
	if call.Function.Name != nil && *call.Function.Name != "" && pending.name != *call.Function.Name {
		if err := s.chargeIdentity(len(*call.Function.Name)); err != nil {
			return nil, err
		}
		pending.name = *call.Function.Name
	}
	if call.Function.Arguments != "" {
		// The complete buffer is emitted as one generated downstream frame
		// at finish: bound its cumulative size BEFORE the write, so a
		// rejected fragment leaves the accumulated state untouched.
		if err := s.checkAccumulated(&pending.complete, len(call.Function.Arguments), "tool arguments"); err != nil {
			return nil, err
		}
		pending.pending.WriteString(call.Function.Arguments)
		pending.complete.WriteString(call.Function.Arguments)
	}

	// Emit the output_item.added only once identity (call ID + name) is
	// complete, then replay buffered argument fragments. The added event
	// carries an EMPTY argument string — never an invented complete object
	// the arguments arrive through deltas and the done
	// event.
	//
	// The emitted item is a fresh value constructed here, never the pending
	// call: later fragments mutate only the pendingToolCall (callID, name,
	// builders), so the added event is already a detached snapshot and needs
	// no copy.
	if !pending.started && pending.callID != "" && pending.name != "" {
		// The tool block must not open before the preceding message's content
		// block is sealed, so the open message item is closed at the
		// transition exactly as an open reasoning item is.
		closed, err := s.closeOpenMessageItem()
		if err != nil {
			return nil, err
		}
		events = append(events, closed...)
		pending.itemID = s.ctx.IDs.New("fc_")
		pending.started = true
		callName, callNamespace := s.ctx.ToolNames.clientCallName(pending.name)
		events = append(events, s.builder.OutputItemAdded(
			pending.outputIndex,
			&ResponsesFunctionCallOutputItem{
				ID:        pending.itemID,
				Type:      "function_call",
				Status:    ResponsesItemInProgress,
				CallID:    pending.callID,
				Name:      callName,
				Arguments: "",
				Namespace: callNamespace,
			},
		))
	}

	if pending.started && pending.pending.Len() > 0 {
		events = append(events, s.builder.FunctionArgumentsDelta(
			pending.itemID,
			pending.outputIndex,
			pending.pending.String(),
		))
		pending.pending.Reset()
	}

	return events, nil
}

// toolCallClosure builds the arguments-done and output_item.done events for
// one started tool call: the arguments are materialized from the accumulated
// fragments, an empty accumulation becoming the empty object.
func (s *chatResponsesStreamState) toolCallClosure(pending *pendingToolCall) []ResponsesSSEEvent {
	arguments := pending.complete.String()
	if arguments == "" {
		arguments = "{}"
	}
	callName, callNamespace := s.ctx.ToolNames.clientCallName(pending.name)
	return []ResponsesSSEEvent{
		s.builder.FunctionArgumentsDone(
			pending.itemID,
			pending.outputIndex,
			arguments,
		),
		s.builder.OutputItemDone(
			pending.outputIndex,
			&ResponsesFunctionCallOutputItem{
				ID:        pending.itemID,
				Type:      "function_call",
				Status:    ResponsesItemCompleted,
				CallID:    pending.callID,
				Name:      callName,
				Arguments: arguments,
				Namespace: callNamespace,
			},
		),
	}
}

// pendingIndexes returns the fragment indexes of the pending tool calls in
// ascending order so any emission over them is deterministic.
func (s *chatResponsesStreamState) pendingIndexes() []int {
	indexes := make([]int, 0, len(s.pendingCalls))
	for index := range s.pendingCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

// pendingSlice returns the pending tool calls in fragment-index order.
func (s *chatResponsesStreamState) pendingSlice() []*pendingToolCall {
	indexes := s.pendingIndexes()
	calls := make([]*pendingToolCall, 0, len(indexes))
	for _, index := range indexes {
		calls = append(calls, s.pendingCalls[index])
	}
	return calls
}

// closePendingToolCalls seals every started pending tool call inline at a
// transition where the next content item must open, in fragment-index order
// so the emitted order is deterministic. A call that never received an
// identity is left for finish, which reports it as corrupt upstream data. The
// sealed calls move to closedCalls: the terminal envelope still carries them,
// and finish never closes them twice.
func (s *chatResponsesStreamState) closePendingToolCalls() []ResponsesSSEEvent {
	if len(s.pendingCalls) == 0 {
		return nil
	}
	var events []ResponsesSSEEvent
	for _, index := range s.pendingIndexes() {
		pending := s.pendingCalls[index]
		if !pending.started {
			continue
		}
		events = append(events, s.toolCallClosure(pending)...)
		s.closedCalls = append(s.closedCalls, pending)
		delete(s.pendingCalls, index)
	}
	return events
}
