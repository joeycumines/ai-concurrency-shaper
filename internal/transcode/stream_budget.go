package transcode

// The total exchange budget for one stream (review-z commit 3): every state
// allocation and every event increments the budget BEFORE the mutation, so a
// corrupt upstream cannot grow memory without limit across many empty items,
// parts, tool calls, state-map entries, or generated frames. A violation
// terminates the exchange and releases the limiter slot and the upstream
// connection. Per-item/part bounds (limits.go) are not enough: the
// empty-part attack (millions of zero-byte parts) consumes unbounded
// map/object memory while the semantic-byte counter stays low — the
// stateEntries and parts dimensions close it.

import "fmt"

// streamBudget is the seven-dimension total budget of one stream exchange.
type streamBudget struct {
	// events bounds the total SSE events processed.
	events int
	// items bounds the total output items opened.
	items int
	// parts bounds the total content parts opened across the whole exchange.
	parts int
	// toolCalls bounds the total tool calls opened.
	toolCalls int
	// stateEntries bounds the total state-map entries (item, part, tool
	// bookkeeping) allocated by the exchange.
	stateEntries int
	// semanticBytes bounds the total accumulated semantic bytes (text,
	// refusal, tool arguments) across every item, part, and tool call.
	semanticBytes int64

	eventsBudget        int
	itemsBudget         int
	partsBudget         int
	toolCallsBudget     int
	stateEntriesBudget  int
	semanticBytesBudget int64
}

// newStreamBudget returns a budget with the package-default caps.
func newStreamBudget() streamBudget {
	return streamBudget{
		eventsBudget:        maxStreamTotalEvents,
		itemsBudget:         maxStreamOutputItems,
		partsBudget:         maxStreamTotalParts,
		toolCallsBudget:     maxStreamToolCalls,
		stateEntriesBudget:  maxStreamStateEntries,
		semanticBytesBudget: maxStreamTotalAccumulatedBytes,
	}
}

// addEvent charges one processed event against the budget.
func (b *streamBudget) addEvent() error {
	if b.events >= b.eventsBudget {
		return fmt.Errorf(
			"stream events exceed the exchange bound of %d",
			b.eventsBudget,
		)
	}
	b.events++
	return nil
}

// addItem charges one opened output item against the budget.
func (b *streamBudget) addItem() error {
	if b.items >= b.itemsBudget {
		return fmt.Errorf(
			"stream output items exceed the exchange bound of %d",
			b.itemsBudget,
		)
	}
	b.items++
	return nil
}

// addPart charges one opened content part against the budget.
func (b *streamBudget) addPart() error {
	if b.parts >= b.partsBudget {
		return fmt.Errorf(
			"stream content parts exceed the exchange bound of %d",
			b.partsBudget,
		)
	}
	b.parts++
	return nil
}

// addToolCall charges one opened tool call against the budget.
func (b *streamBudget) addToolCall() error {
	if b.toolCalls >= b.toolCallsBudget {
		return fmt.Errorf(
			"stream tool calls exceed the exchange bound of %d",
			b.toolCallsBudget,
		)
	}
	b.toolCalls++
	return nil
}

// addStateEntries charges n state-map entries against the budget.
func (b *streamBudget) addStateEntries(n int) error {
	if b.stateEntries+n > b.stateEntriesBudget {
		return fmt.Errorf(
			"stream state entries exceed the exchange bound of %d",
			b.stateEntriesBudget,
		)
	}
	b.stateEntries += n
	return nil
}

// addSemanticBytes charges n accumulated semantic bytes against the budget.
func (b *streamBudget) addSemanticBytes(n int64) error {
	if b.semanticBytes+n > b.semanticBytesBudget {
		return fmt.Errorf(
			"stream accumulated semantic bytes exceed the exchange bound of %d",
			b.semanticBytesBudget,
		)
	}
	b.semanticBytes += n
	return nil
}

// per-accumulator and per-frame byte bounds reused by the budget wiring.
const (
	// maxStreamTotalEvents bounds the total events one exchange may process.
	maxStreamTotalEvents = 1 << 20
	// maxStreamTotalParts bounds the total content parts across the whole
	// exchange (the per-item bound alone permits the empty-part attack).
	maxStreamTotalParts = 1 << 16
	// maxStreamStateEntries bounds the total state-map entries (item, part,
	// tool bookkeeping) one exchange may allocate.
	maxStreamStateEntries = 1 << 16
	// maxStreamGeneratedBytes bounds the total generated downstream bytes of
	// one exchange.
	maxStreamGeneratedBytes = 64 << 20
)
